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
//   - Text inside a token's braces is conditional, as in Sonarr: in
//     "{Book series_position - }{title}" the "Book " and " - " appear only
//     when there is a position, so one template names series books and
//     standalones alike. A ":00" after the name zero-pads the number it
//     starts with to that many digits: {series_position:00} gives "01".
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

// tokenRe matches one token with its conditional text and padding:
// {prefix name:00 suffix}. Names are matched as whole lowercase words, so
// "Title" in conditional text is text, and "title" never matches inside
// "subtitle". Braces never span a "/": segments are split first.
var tokenRe = regexp.MustCompile(`\{([^{}/]*?)\b(author|narrator|title|subtitle|series_name|series_position|year|asin)\b(?::(0+))?([^{}/]*)\}`)

// braceRe finds every braced part, for Validate to name the unknown or
// unclosed ones; closedBraceRe is what Render substitutes, in one pass.
var (
	braceRe       = regexp.MustCompile(`\{[^{}]*\}?`)
	closedBraceRe = regexp.MustCompile(`\{[^{}]*\}`)
)

// Validate checks that every {token} in the template is known and that the
// template can produce a non-empty path for a fully-populated book.
func Validate(template string) error {
	if strings.TrimSpace(template) == "" {
		return fmt.Errorf("template is empty")
	}
	for _, b := range braceRe.FindAllString(template, -1) {
		switch {
		case !strings.HasSuffix(b, "}"):
			return fmt.Errorf("%s is missing its closing brace", b)
		case !tokenRe.MatchString(b) || tokenRe.FindString(b) != b:
			return fmt.Errorf("unknown variable %s", b)
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
	out = closedBraceRe.ReplaceAllStringFunc(seg, func(m string) string {
		hadToken = true
		sm := tokenRe.FindStringSubmatch(m)
		if sm == nil || sm[0] != m {
			return "" // unknown: Validate rejects it; never render the braces
		}
		prefix, name, pad, suffix := sm[1], sm[2], sm[3], sm[4]
		val, _ := vars.lookup(name)
		if val == "" {
			return "" // and its conditional text with it
		}
		hadValue = true
		return prefix + padNumber(val, len(pad)) + suffix
	})
	return sanitizeSegment(out), hadToken, hadValue
}

// padNumber zero-pads the number a value starts with: "1" → "01" and
// "1.5" → "01.5" at width 2. Values that don't start with a digit, or are
// already as wide, are left alone.
func padNumber(val string, width int) string {
	digits := 0
	for digits < len(val) && val[digits] >= '0' && val[digits] <= '9' {
		digits++
	}
	if digits == 0 || digits >= width {
		return val
	}
	return strings.Repeat("0", width-digits) + val
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
			if v, _ := vars.lookup(t[2]); v != "" {
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
