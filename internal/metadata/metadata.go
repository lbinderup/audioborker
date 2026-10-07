// Package metadata defines the provider abstraction for audiobook metadata.
// Audnexus is the shipped implementation; the interface exists so
// AudiobookDB (its announced successor) can slot in once its API launches.
package metadata

import (
	"context"
	"fmt"
)

// SearchQuery is a title/author search against the provider's catalog.
type SearchQuery struct {
	Keywords string // free-text; usually derived from the folder name
	Title    string
	Author   string
	Region   string
}

// SearchResult is one candidate match shown on the match screen.
type SearchResult struct {
	ASIN       string
	Region     string
	Title      string
	Subtitle   string
	Authors    []string
	Narrators  []string
	Year       string
	Language   string
	RuntimeMin int
	CoverURL   string
	Score      int // match confidence, higher is better (set by internal/match)

	// Comparison against the local audio's duration, filled in by
	// internal/match when a runtime is known. RuntimeMatch is one of
	// "" (unknown), "exact", "close", "near", "off".
	RuntimeDeltaMin int
	RuntimeMatch    string
}

// Book is the full metadata snapshot used for tagging.
type Book struct {
	ASIN           string   `json:"asin"`
	Region         string   `json:"region"`
	Title          string   `json:"title"`
	Subtitle       string   `json:"subtitle"`
	Authors        []string `json:"authors"`
	Narrators      []string `json:"narrators"`
	SeriesName     string   `json:"series_name"`
	SeriesPosition string   `json:"series_position"`
	Year           string   `json:"year"`
	ReleaseDate    string   `json:"release_date"` // YYYY-MM-DD when known
	Publisher      string   `json:"publisher"`
	Language       string   `json:"language"`
	Genres         []string `json:"genres"`
	// SubGenres are the more specific categories below Genres ("Fantasy",
	// "Epic"). Genres[0] is what gets written into the file; Plex shows all.
	SubGenres []string `json:"sub_genres,omitempty"`
	// GenrePaths are the catalog's category paths, top-level genre first,
	// when it has them (Audible's category ladders). They tie each sub-genre
	// to its genre, which is what lets CleanGenres drop both together.
	GenrePaths [][]string `json:"genre_paths,omitempty"`
	// LiteratureType is Audnexus' "fiction" / "nonfiction" label, the
	// strongest signal for spotting a miscategorized genre.
	LiteratureType string `json:"literature_type,omitempty"`
	// Description is the provider's short teaser (Audnexus truncates it to
	// ~250 chars ending in "..."); Summary is the full publisher blurb,
	// converted to plain text. Prefer Summary for tagging — see Book.Blurb.
	Description string `json:"description"`
	Summary     string `json:"summary"`
	RuntimeMin  int    `json:"runtime_min"`
	CoverURL    string `json:"cover_url"`

	// Sources records which catalog supplied each logical field, keyed by the
	// field names in internal/metadata/aggregate ("title", "series", …) with
	// source ids ("audnexus", "audible") as values. Nil on books fetched from
	// a single source and on job snapshots created before aggregation existed.
	Sources map[string]string `json:"sources,omitempty"`
}

// Chapter offsets are relative to the start of the complete audiobook.
type Chapter struct {
	Title    string `json:"title"`
	StartMs  int64  `json:"start_ms"`
	LengthMs int64  `json:"length_ms"`
}

// ChapterInfo is the provider's chapter listing for one edition.
type ChapterInfo struct {
	ASIN         string    `json:"asin"`
	RuntimeMs    int64     `json:"runtime_ms"`
	IsAccurate   bool      `json:"is_accurate"`
	BrandIntroMs int64     `json:"brand_intro_ms"`
	BrandOutroMs int64     `json:"brand_outro_ms"`
	Chapters     []Chapter `json:"chapters"`
}

// Blurb returns the fullest description text available: the complete
// publisher summary when the provider supplied one, otherwise the short
// teaser. This is what gets embedded in the file.
func (b Book) Blurb() string {
	if b.Summary != "" {
		return b.Summary
	}
	return b.Description
}

// ShiftSpec describes how provider chapter timings are offset to match a
// specific rip. Two modes:
//
//   - "fixed": every chapter moves by FixedMs.
//   - "interp": the shift is FromMs at anchor chapter FromIdx and ToMs at
//     anchor chapter ToIdx (both 1-based), linearly interpolated by the
//     chapters' original start times in between, and held constant outside
//     the anchors. This models rips whose drift accumulates over the book —
//     what sounds right at chapter 1 can be a second off by chapter 120.
type ShiftSpec struct {
	Mode    string `json:"mode,omitempty"` // "" (none) | "fixed" | "interp"
	FixedMs int64  `json:"fixed_ms,omitempty"`
	FromIdx int    `json:"from_idx,omitempty"`
	FromMs  int64  `json:"from_ms,omitempty"`
	ToIdx   int    `json:"to_idx,omitempty"`
	ToMs    int64  `json:"to_ms,omitempty"`
}

// IsZero reports whether applying the spec would change nothing.
func (s ShiftSpec) IsZero() bool {
	switch s.Mode {
	case "fixed":
		return s.FixedMs == 0
	case "interp":
		return s.FromMs == 0 && s.ToMs == 0
	}
	return true
}

func (s ShiftSpec) String() string {
	switch s.Mode {
	case "fixed":
		return fmt.Sprintf("fixed %+d ms", s.FixedMs)
	case "interp":
		return fmt.Sprintf("interpolated %+d ms @ chapter %d → %+d ms @ chapter %d",
			s.FromMs, s.FromIdx, s.ToMs, s.ToIdx)
	}
	return "none"
}

// Shifted returns a copy of the chapter list moved by a constant shiftMs
// (positive = later).
func (c *ChapterInfo) Shifted(shiftMs int64) *ChapterInfo {
	if shiftMs == 0 {
		return c
	}
	return c.shifted(func(int64) int64 { return shiftMs })
}

// ShiftedBy applies a ShiftSpec. Anchor indices are clamped into range and
// swapped if reversed; a degenerate anchor pair falls back to a fixed shift
// of FromMs.
func (c *ChapterInfo) ShiftedBy(spec ShiftSpec) *ChapterInfo {
	if c == nil || len(c.Chapters) == 0 || spec.IsZero() {
		return c
	}
	if spec.Mode == "fixed" {
		return c.Shifted(spec.FixedMs)
	}
	n := len(c.Chapters)
	clampIdx := func(i int) int {
		if i < 1 {
			return 1
		}
		if i > n {
			return n
		}
		return i
	}
	fi, ti := clampIdx(spec.FromIdx), clampIdx(spec.ToIdx)
	fMs, tMs := spec.FromMs, spec.ToMs
	if fi > ti {
		fi, ti = ti, fi
		fMs, tMs = tMs, fMs
	}
	t0, t1 := c.Chapters[fi-1].StartMs, c.Chapters[ti-1].StartMs
	if fi == ti || t1 == t0 {
		return c.Shifted(fMs)
	}
	return c.shifted(func(t int64) int64 {
		switch {
		case t <= t0:
			return fMs
		case t >= t1:
			return tMs
		default:
			return fMs + (tMs-fMs)*(t-t0)/(t1-t0)
		}
	})
}

// shifted rebuilds the chapter list with a per-start shift function. Starts
// are clamped into [0, runtime] and kept non-decreasing, and lengths are
// recomputed from the resulting boundaries, so the list stays consistent.
func (c *ChapterInfo) shifted(shiftAt func(startMs int64) int64) *ChapterInfo {
	if c == nil || len(c.Chapters) == 0 {
		return c
	}
	out := *c
	out.Chapters = make([]Chapter, 0, len(c.Chapters))
	var prev int64
	for _, ch := range c.Chapters {
		start := ch.StartMs + shiftAt(ch.StartMs)
		if start < 0 {
			start = 0
		}
		if c.RuntimeMs > 0 && start > c.RuntimeMs {
			start = c.RuntimeMs
		}
		if start < prev {
			start = prev
		}
		out.Chapters = append(out.Chapters, Chapter{Title: ch.Title, StartMs: start})
		prev = start
	}
	for i := range out.Chapters {
		if i+1 < len(out.Chapters) {
			out.Chapters[i].LengthMs = out.Chapters[i+1].StartMs - out.Chapters[i].StartMs
		} else if c.RuntimeMs > 0 {
			out.Chapters[i].LengthMs = c.RuntimeMs - out.Chapters[i].StartMs
		} else {
			out.Chapters[i].LengthMs = c.Chapters[i].LengthMs
		}
		if out.Chapters[i].LengthMs < 0 {
			out.Chapters[i].LengthMs = 0
		}
	}
	return &out
}

type Provider interface {
	// Search returns candidate matches. Implementations may consult a
	// different backend than book lookups (Audnexus has no search, so the
	// audnexus provider searches Audible's public catalog).
	Search(ctx context.Context, q SearchQuery) ([]SearchResult, error)
	// GetBook fetches the full metadata for an ASIN. Returns ErrNotFound if
	// the ASIN does not exist in the given region.
	GetBook(ctx context.Context, asin, region string) (*Book, error)
	// GetChapters fetches chapter data; may return ErrNotFound when the
	// provider has none for this edition.
	GetChapters(ctx context.Context, asin, region string) (*ChapterInfo, error)
}

// ValidASIN reports whether s looks like an ASIN (10 alphanumeric chars).
// A domain fact about ASINs rather than an Audnexus detail, so every source —
// the catalogs, and the atoms embedded in a local file — validates alike.
func ValidASIN(s string) bool {
	if len(s) != 10 {
		return false
	}
	for _, r := range s {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// ErrNotFound is returned for unknown ASINs (including wrong-region lookups).
type notFoundError struct{ msg string }

func (e notFoundError) Error() string { return e.msg }

func NotFound(msg string) error { return notFoundError{msg} }

func IsNotFound(err error) bool {
	_, ok := err.(notFoundError)
	return ok
}
