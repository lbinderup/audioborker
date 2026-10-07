// Package aggregate merges per-source metadata records into one Book, field
// by field, so the most complete data wins. Raw per-source records are the
// only thing ever cached — the merge is a pure function over them, which
// keeps per-field user overrides free of cache-invalidation concerns.
package aggregate

import (
	"context"
	"strconv"
	"strings"

	"audioborker/internal/metadata"
)

// Source ids. Recorded in Book.Sources, the metadata_cache source column, and
// the match screen's per-field override form values.
const (
	SourceAudnexus = "audnexus"
	SourceAudible  = "audible"
)

// Field keys. Composite keys move dependent values together, so a merged book
// can never pair e.g. a series name from one catalog with a position from the
// other.
const (
	FieldTitle       = "title"
	FieldSubtitle    = "subtitle"
	FieldAuthors     = "authors"
	FieldNarrators   = "narrators"
	FieldSeries      = "series"  // SeriesName + SeriesPosition
	FieldRelease     = "release" // Year + ReleaseDate
	FieldPublisher   = "publisher"
	FieldLanguage    = "language"
	FieldGenres      = "genres"      // Genres + SubGenres + GenrePaths
	FieldDescription = "description" // Description + Summary (the blurb pair)
	FieldRuntime     = "runtime_min"
	FieldCover       = "cover_url"
)

// Fields is the canonical ordered list; it drives the match screen's source
// table and the queue handler's override parsing.
var Fields = []string{
	FieldTitle, FieldSubtitle, FieldAuthors, FieldNarrators, FieldSeries,
	FieldRelease, FieldPublisher, FieldLanguage, FieldGenres,
	FieldDescription, FieldRuntime, FieldCover,
}

var fieldLabels = map[string]string{
	FieldTitle:       "Title",
	FieldSubtitle:    "Subtitle",
	FieldAuthors:     "Authors",
	FieldNarrators:   "Narrators",
	FieldSeries:      "Series",
	FieldRelease:     "Release date",
	FieldPublisher:   "Publisher",
	FieldLanguage:    "Language",
	FieldGenres:      "Genres",
	FieldDescription: "Description",
	FieldRuntime:     "Runtime",
	FieldCover:       "Cover",
}

// Label returns the human name of a field key.
func Label(key string) string {
	if l, ok := fieldLabels[key]; ok {
		return l
	}
	return key
}

// fieldSpec reads/writes one logical field of a Book.
type fieldSpec struct {
	key    string
	empty  func(b *metadata.Book) bool
	copyTo func(dst, src *metadata.Book)
}

var specs = []fieldSpec{
	{FieldTitle,
		func(b *metadata.Book) bool { return b.Title == "" },
		func(d, s *metadata.Book) { d.Title = s.Title }},
	{FieldSubtitle,
		func(b *metadata.Book) bool { return b.Subtitle == "" },
		func(d, s *metadata.Book) { d.Subtitle = s.Subtitle }},
	{FieldAuthors,
		func(b *metadata.Book) bool { return len(b.Authors) == 0 },
		func(d, s *metadata.Book) { d.Authors = s.Authors }},
	{FieldNarrators,
		func(b *metadata.Book) bool { return len(b.Narrators) == 0 },
		func(d, s *metadata.Book) { d.Narrators = s.Narrators }},
	{FieldSeries,
		func(b *metadata.Book) bool { return b.SeriesName == "" },
		func(d, s *metadata.Book) { d.SeriesName, d.SeriesPosition = s.SeriesName, s.SeriesPosition }},
	{FieldRelease,
		func(b *metadata.Book) bool { return b.Year == "" && b.ReleaseDate == "" },
		func(d, s *metadata.Book) { d.Year, d.ReleaseDate = s.Year, s.ReleaseDate }},
	{FieldPublisher,
		func(b *metadata.Book) bool { return b.Publisher == "" },
		func(d, s *metadata.Book) { d.Publisher = s.Publisher }},
	{FieldLanguage,
		func(b *metadata.Book) bool { return b.Language == "" },
		func(d, s *metadata.Book) { d.Language = s.Language }},
	{FieldGenres,
		func(b *metadata.Book) bool { return len(b.Genres) == 0 },
		func(d, s *metadata.Book) { d.Genres, d.SubGenres, d.GenrePaths = s.Genres, s.SubGenres, s.GenrePaths }},
	{FieldDescription,
		func(b *metadata.Book) bool { return b.Blurb() == "" },
		func(d, s *metadata.Book) { d.Description, d.Summary = s.Description, s.Summary }},
	{FieldRuntime,
		func(b *metadata.Book) bool { return b.RuntimeMin == 0 },
		func(d, s *metadata.Book) { d.RuntimeMin = s.RuntimeMin }},
	{FieldCover,
		func(b *metadata.Book) bool { return b.CoverURL == "" },
		func(d, s *metadata.Book) { d.CoverURL = s.CoverURL }},
	// Not a user-facing field (absent from Fields): only Audnexus has it, and
	// it exists to clean the genres.
	{fieldLiteratureType,
		func(b *metadata.Book) bool { return b.LiteratureType == "" },
		func(d, s *metadata.Book) { d.LiteratureType = s.LiteratureType }},
}

const fieldLiteratureType = "literature_type"

// DefaultPrecedence is the per-field source order used when the user has not
// overridden a field. Audnexus is primary for every field except genres (see
// init); Audible fills gaps. Kept per-field so a source that proves
// systematically better for one field can be promoted without touching the UI.
var DefaultPrecedence = map[string][]string{}

// defaultOrder is also the identity order: the first source with a record
// supplies ASIN/Region when the caller doesn't overwrite them.
var defaultOrder = []string{SourceAudnexus, SourceAudible}

func init() {
	for _, f := range Fields {
		DefaultPrecedence[f] = defaultOrder
	}
	DefaultPrecedence[fieldLiteratureType] = defaultOrder
	// Audible's category ladders carry the genre → sub-genre hierarchy that
	// Audnexus flattens away; without it a miscategorized genre's sub-genres
	// can't be told apart from the real ones.
	DefaultPrecedence[FieldGenres] = []string{SourceAudible, SourceAudnexus}
}

// Merge combines per-source books field by field. For each field an override
// wins when set AND non-empty at that source; otherwise the first source in
// DefaultPrecedence[key] with a non-empty value. Winning sources are recorded
// in the result's Sources map (fields nobody supplied get no entry).
// FieldDescription is completeness-aware: a source carrying the full Summary
// beats one with only the truncated teaser, regardless of precedence order.
// The merged genres are then cleaned (metadata.CleanGenres), so whatever the
// sources, a genre contradicting the book's fiction/nonfiction kind never
// reaches a file.
func Merge(books map[string]*metadata.Book, overrides map[string]string) *metadata.Book {
	out := &metadata.Book{Sources: map[string]string{}}
	for _, src := range defaultOrder {
		if b := books[src]; b != nil {
			out.ASIN, out.Region = b.ASIN, b.Region
			break
		}
	}
	for _, spec := range specs {
		src := pickSource(spec, books, overrides[spec.key])
		if src == "" {
			continue
		}
		spec.copyTo(out, books[src])
		if spec.key != fieldLiteratureType {
			out.Sources[spec.key] = src
		}
	}
	out.Genres, out.SubGenres = metadata.CleanGenres(out)
	return out
}

func pickSource(spec fieldSpec, books map[string]*metadata.Book, override string) string {
	if b := books[override]; b != nil && !spec.empty(b) {
		return override
	}
	if spec.key == FieldDescription {
		for _, src := range DefaultPrecedence[spec.key] {
			if b := books[src]; b != nil && b.Summary != "" {
				return src
			}
		}
	}
	for _, src := range DefaultPrecedence[spec.key] {
		if b := books[src]; b != nil && !spec.empty(b) {
			return src
		}
	}
	return ""
}

// Value renders one logical field of a book as its canonical display string
// ("" when empty or b is nil). Used both to compare sources for the per-field
// picker and, truncated, as the picker's option text.
func Value(b *metadata.Book, key string) string {
	if b == nil {
		return ""
	}
	switch key {
	case FieldTitle:
		return b.Title
	case FieldSubtitle:
		return b.Subtitle
	case FieldAuthors:
		return strings.Join(b.Authors, ", ")
	case FieldNarrators:
		return strings.Join(b.Narrators, ", ")
	case FieldSeries:
		if b.SeriesName == "" {
			return ""
		}
		if b.SeriesPosition == "" {
			return b.SeriesName
		}
		return b.SeriesName + " #" + b.SeriesPosition
	case FieldRelease:
		if b.ReleaseDate != "" {
			return b.ReleaseDate
		}
		return b.Year
	case FieldPublisher:
		return b.Publisher
	case FieldLanguage:
		return b.Language
	case FieldGenres:
		return strings.Join(b.Genres, ", ")
	case FieldDescription:
		return b.Blurb()
	case FieldRuntime:
		if b.RuntimeMin == 0 {
			return ""
		}
		return strconv.Itoa(b.RuntimeMin) + " min"
	case FieldCover:
		return b.CoverURL
	}
	return ""
}

// SourceValue is Value for one source's record as the merge would use it if
// that source were picked. Only genres differ from the raw record: they are
// cleaned, using the fiction/nonfiction label Audnexus carries even when
// Audible's ladders are picked, so the comparison panel never offers a genre
// that would not actually be tagged.
func SourceValue(books map[string]*metadata.Book, src, key string) string {
	b := books[src]
	if key != FieldGenres || b == nil || len(b.Genres) == 0 {
		return Value(b, key)
	}
	return Value(Merge(books, map[string]string{FieldGenres: src}), key)
}

// BookSource is the one-method slice of metadata.Provider the aggregator
// needs from each catalog client.
type BookSource interface {
	GetBook(ctx context.Context, asin, region string) (*metadata.Book, error)
}

// Cache stores raw per-source records; *store.Store implements it.
type Cache interface {
	CachedBook(source, asin, region string) (*metadata.Book, error)
	CacheBook(source, asin, region string, b *metadata.Book) error
}

// Aggregator fetches a book from every configured source (cache-first) and
// merges the records.
type Aggregator struct {
	Sources map[string]BookSource
	Order   []string // fetch order; Order[0] is the primary source
	Cache   Cache    // nil = no caching (tests)
}

// Result carries what the UI needs beyond the merged book itself.
type Result struct {
	Book      *metadata.Book            // merged; Sources populated
	PerSource map[string]*metadata.Book // raw records, for the provenance panel
	Notes     []string                  // user-facing degradation notes
}

// GetBook fetches each source cache-first, merges, and records provenance.
// Error semantics:
//   - primary source transient error → returned, so a job never silently
//     snapshots a degraded book the user believes is complete
//   - primary NotFound → continue; a secondary-only merge is allowed (manual
//     ASINs missing from Audnexus still resolve via Audible)
//   - secondary source ANY error → noted and skipped; the Audible catalog API
//     is unofficial and must never block the app
//   - nothing fetched anywhere → metadata.NotFound
func (a *Aggregator) GetBook(ctx context.Context, asin, region string, overrides map[string]string) (*Result, error) {
	res := &Result{PerSource: map[string]*metadata.Book{}}
	for i, name := range a.Order {
		src := a.Sources[name]
		if src == nil {
			continue
		}
		if a.Cache != nil {
			if b, err := a.Cache.CachedBook(name, asin, region); err == nil && b != nil {
				res.PerSource[name] = b
				continue
			}
		}
		b, err := src.GetBook(ctx, asin, region)
		switch {
		case err == nil:
			res.PerSource[name] = b
			if a.Cache != nil {
				a.Cache.CacheBook(name, asin, region, b)
			}
		case metadata.IsNotFound(err):
			// This catalog simply doesn't know the ASIN; others may.
		case i == 0:
			return nil, err
		default:
			res.Notes = append(res.Notes, name+" lookup failed ("+err.Error()+") — its fields are unavailable for this book.")
		}
	}
	if len(res.PerSource) == 0 {
		return nil, metadata.NotFound("no catalog has a book with ASIN " + asin + " in region " + region + " (check both)")
	}
	res.Book = Merge(res.PerSource, overrides)
	// Identity is the request, never merged — a source echoing a different
	// (e.g. marketplace-redirected) ASIN must not change what the job records.
	res.Book.ASIN, res.Book.Region = asin, region
	return res, nil
}
