// Package embedded interprets the metadata a media file already carries as a
// metadata.Book. A file on disk is just another metadata source, which is why
// this sits beside audible/ and audnexus/: the Library screen shows what a
// file claims today next to what the catalogs offer, and an ASIN found in a
// file's own atoms is what lets an existing m4b re-match in one click —
// including files another tool wrote.
//
// Nothing here shells out. It consumes the tag map ffprobe already returns
// (pipeline.FileInfo.Tags), so every rule stays pure and table-testable.
package embedded

import (
	"sort"
	"strings"

	"audioborker/internal/metadata"
)

// Book interprets container tags as a Book. Absent fields stay empty — a tag
// nobody wrote is unknown, not blank.
func Book(tags map[string]string) *metadata.Book {
	n := normalize(tags)
	b := &metadata.Book{
		ASIN:      ASIN(tags),
		Title:     n.first("title", "album"),
		Subtitle:  n.first("subtitle"),
		Publisher: n.first("publisher", "label"),
		Language:  n.first("language"),
		// Series lives in a freeform SERIES atom (what tone writes) or the
		// movement name; the position is PART for our own files and
		// SERIES-PART for several other taggers.
		SeriesName:     n.first("series", "movement_name", "show"),
		SeriesPosition: n.first("part", "series_part", "movement", "episode_id"),
	}
	b.Authors = splitPeople(n.first("artist", "album_artist", "author"))
	// ffprobe does not surface tone's ©nrt narrator atom, but by audiobook
	// convention (and in our own tagger) the narrator is also written to
	// composer — so that is the reliable place to read it back from.
	b.Narrators = splitPeople(n.first("narrator", "composer"))
	// The genre atom holds one value, the one players show. audioborker also
	// writes every genre and sub-genre into a GENRES atom, "; "-separated, so
	// a book tagged without any catalog (the manual flow) keeps them all.
	if gs := splitList(n.first("genres")); len(gs) > 0 {
		b.Genres = gs
	} else if g := n.first("genre"); g != "" {
		b.Genres = []string{g}
	}
	// Prefer the longest blurb rather than the first: the same
	// completeness-over-precedence rule aggregate applies to descriptions.
	b.Summary = n.longest("synopsis", "long_description", "description", "comment")
	if date := n.first("date", "releasetime", "originaldate", "recording_date", "year"); date != "" {
		if len(date) >= 10 && date[4] == '-' && date[7] == '-' {
			b.ReleaseDate = date[:10]
		}
		if y := leadingYear(date); y != "" {
			b.Year = y
		}
	}
	return b
}

// ManualID returns the AUDIOBORKER_ID of a book that was tagged by hand
// (AUDIOBORKER_SOURCE=manual), or "". Tagging the same book by hand again
// reuses it, so the Plex agent keeps matching it to the same item.
func ManualID(tags map[string]string) string {
	n := normalize(tags)
	if !strings.EqualFold(n["audioborker_source"], "manual") {
		return ""
	}
	return n["audioborker_id"]
}

// ASIN returns the Audible ASIN a file claims, or "". Spellings seen in the
// wild: AUDIBLE_ASIN (audioborker/tone), a bare ASIN (m4b-tool and friends),
// and namespaced freeform variants like ----:com.pilabor.tone:AUDIBLE_ASIN.
// The value is validated, so a stray free-text atom can never send the match
// screen off looking up garbage.
func ASIN(tags map[string]string) string {
	n := normalize(tags)
	for _, k := range []string{"audible_asin", "asin"} {
		if v := n[k]; metadata.ValidASIN(v) {
			return strings.ToUpper(v)
		}
	}
	// A tagger we have not seen: accept any key ending in "asin" whose value
	// has an ASIN's shape. Keys are sorted because Go map order is random and
	// the same file must always resolve to the same ASIN.
	keys := make([]string, 0, len(n))
	for k := range n {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.HasSuffix(k, "asin") && metadata.ValidASIN(n[k]) {
			return strings.ToUpper(n[k])
		}
	}
	return ""
}

// tagMap is a tag set with its key spellings folded together.
type tagMap map[string]string

// normalize folds key spellings apart from each other: lowercase, drop any
// freeform namespace prefix (everything up to the last ':'), and treat '-' and
// '_' alike — so SERIES-PART, series_part and ----:com.x:Series-Part are one
// key. Blank values are dropped so callers can treat absence and emptiness the
// same way.
func normalize(tags map[string]string) tagMap {
	out := make(tagMap, len(tags))
	for k, v := range tags {
		k = strings.ToLower(strings.TrimSpace(k))
		if i := strings.LastIndexByte(k, ':'); i >= 0 {
			k = k[i+1:]
		}
		k = strings.ReplaceAll(k, "-", "_")
		if v = strings.TrimSpace(v); v != "" && out[k] == "" {
			out[k] = v
		}
	}
	return out
}

// first returns the value of the earliest present key.
func (t tagMap) first(keys ...string) string {
	for _, k := range keys {
		if v := t[k]; v != "" {
			return v
		}
	}
	return ""
}

// longest returns the longest value among the keys.
func (t tagMap) longest(keys ...string) string {
	best := ""
	for _, k := range keys {
		if v := t[k]; len(v) > len(best) {
			best = v
		}
	}
	return best
}

// splitPeople splits a contributor list. "; " and " & " are unambiguous; ", "
// is accepted too even though it mis-splits "Doe, John"-style single names —
// these values are only ever search hints and display, never what gets written
// back to a file, so over-splitting costs nothing and under-splitting would
// lose the second author of most books.
func splitPeople(s string) []string {
	if s == "" {
		return nil
	}
	for _, sep := range []string{";", " & ", ","} {
		if strings.Contains(s, sep) {
			var out []string
			for _, part := range strings.Split(s, sep) {
				if p := strings.TrimSpace(part); p != "" {
					out = append(out, p)
				}
			}
			return out
		}
	}
	return []string{strings.TrimSpace(s)}
}

// splitList splits a "; "-separated list, dropping blanks.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ";") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// leadingYear pulls a 4-digit year off the front of a date-ish value.
func leadingYear(s string) string {
	if len(s) < 4 {
		return ""
	}
	for i := 0; i < 4; i++ {
		if s[i] < '0' || s[i] > '9' {
			return ""
		}
	}
	return s[:4]
}
