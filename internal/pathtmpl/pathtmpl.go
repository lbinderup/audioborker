// Package pathtmpl renders output-path templates like
// "{author}/{series_name}/{series_position} - {title}" into safe relative
// paths. Design goals, learned from bragibooks' path bugs:
//
//   - Tokens are delimited with braces and substituted in one pass — values
//     containing token-like text can never be re-substituted.
//   - Sanitization happens per path segment, never across separators.
//   - A segment whose tokens ALL resolve to empty is dropped entirely, and
//     leftover separator punctuation from partially-empty segments is
//     collapsed, so a book without a series never yields " - Title" or an
//     empty directory level.
//   - Likewise a bracketed group whose tokens all resolve empty is dropped
//     with the space before it, so a book without an ASIN (one tagged by
//     hand) is "Title.m4b", not "Title [].m4b".
package pathtmpl

import (
	"fmt"
	"regexp"
	"strings"
)

// Vars holds the values a template can reference. Empty strings are valid
// and mean "this book doesn't have that attribute".
type Vars struct {
	Author         string
	Narrator       string
	Title          string
	Subtitle       string
	SeriesName     string
	SeriesPosition string
	Year           string
	ASIN           string
}

func (v Vars) lookup(name string) (string, bool) {
	switch name {
	case "author":
		return v.Author, true
	case "narrator":
		return v.Narrator, true
	case "title":
		return v.Title, true
	case "subtitle":
		return v.Subtitle, true
	case "series_name":
		return v.SeriesName, true
	case "series_position":
		return v.SeriesPosition, true
	case "year":
		return v.Year, true
	case "asin":
		return v.ASIN, true
	}
	return "", false
}

var tokenRe = regexp.MustCompile(`\{([a-z_]+)\}`)

// Validate checks that every {token} in the template is known and that the
// template can produce a non-empty path for a fully-populated book.
func Validate(template string) error {
	if strings.TrimSpace(template) == "" {
		return fmt.Errorf("template is empty")
	}
	for _, m := range tokenRe.FindAllStringSubmatch(template, -1) {
		if _, ok := (Vars{}).lookup(m[1]); !ok {
			return fmt.Errorf("unknown variable {%s}", m[1])
		}
	}
	if !tokenRe.MatchString(template) {
		return fmt.Errorf("template contains no variables")
	}
	sample := Vars{
		Author: "Author", Narrator: "Narrator", Title: "Title", Subtitle: "Subtitle",
		SeriesName: "Series", SeriesPosition: "1", Year: "2020", ASIN: "B000000000",
	}
	out, err := Render(template, sample)
	if err != nil {
		return err
	}
	if out == "" {
		return fmt.Errorf("template renders to an empty path")
	}
	return nil
}

// Render substitutes vars into the template and returns a cleaned relative
// path (forward-slash separated, no extension). Segments whose tokens all
// resolve empty are dropped; if every segment drops, an error is returned so
// callers never write to the output root blindly.
func Render(template string, vars Vars) (string, error) {
	var kept []string
	for _, seg := range strings.Split(template, "/") {
		rendered, hadToken, hadValue := renderSegment(seg, vars)
		if hadToken && !hadValue {
			continue // e.g. "{series_name}" level for a standalone book
		}
		if rendered == "" {
			continue
		}
		kept = append(kept, rendered)
	}
	if len(kept) == 0 {
		return "", fmt.Errorf("path template %q resolved to an empty path", template)
	}
	return strings.Join(kept, "/"), nil
}

// renderSegment substitutes tokens in one segment and reports whether the
// segment referenced any token and whether at least one token had a value.
func renderSegment(seg string, vars Vars) (out string, hadToken, hadValue bool) {
	seg = dropEmptyGroups(seg, vars)
	out = tokenRe.ReplaceAllStringFunc(seg, func(m string) string {
		name := tokenRe.FindStringSubmatch(m)[1]
		hadToken = true
		val, ok := vars.lookup(name)
		if !ok {
			return "" // Validate rejects unknown tokens; be lenient at render time
		}
		if val != "" {
			hadValue = true
		}
		return val
	})
	return sanitizeSegment(out), hadToken, hadValue
}

// groupRe matches a bracketed part of a template segment together with the
// whitespace before it: " [{asin}]", " ({year})".
var groupRe = regexp.MustCompile(`\s*(\[[^\[\]]*\]|\([^()]*\))`)

// dropEmptyGroups removes bracketed groups whose tokens all resolve empty.
// It works on the template, before substitution, so a value that happens to
// contain brackets is never touched; groups without tokens ("[Unabridged]")
// are literal text and stay.
func dropEmptyGroups(seg string, vars Vars) string {
	return groupRe.ReplaceAllStringFunc(seg, func(g string) string {
		tokens := tokenRe.FindAllStringSubmatch(g, -1)
		if len(tokens) == 0 {
			return g
		}
		for _, t := range tokens {
			if v, _ := vars.lookup(t[1]); v != "" {
				return g
			}
		}
		return ""
	})
}

var (
	// Characters invalid on common filesystems (NTFS being the strictest),
	// plus control characters.
	invalidChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)
	multiSpace   = regexp.MustCompile(`\s{2,}`)
	// Runs of separator punctuation left behind by empty tokens,
	// e.g. "1 - " -> when series_position was empty: " - ".
	danglingSep = regexp.MustCompile(`^[\s\-–—ـ,.]+|[\s\-–—ـ,]+$`)
	repeatedSep = regexp.MustCompile(`(\s[-–—]\s?){2,}`)
)

func sanitizeSegment(seg string) string {
	seg = invalidChars.ReplaceAllString(seg, "")
	seg = multiSpace.ReplaceAllString(seg, " ")
	seg = repeatedSep.ReplaceAllString(seg, " - ")
	seg = danglingSep.ReplaceAllString(seg, "")
	seg = strings.TrimRight(seg, ". ") // Windows forbids trailing dots/spaces
	return strings.TrimSpace(seg)
}
