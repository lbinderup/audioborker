package aggregate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"audioborker/internal/metadata"
)

func full() *metadata.Book {
	return &metadata.Book{
		ASIN: "B000000001", Region: "us",
		Title: "Nexus Title", Subtitle: "Nexus Sub",
		Authors: []string{"Nexus Author"}, Narrators: []string{"Nexus Narrator"},
		SeriesName: "Nexus Series", SeriesPosition: "2",
		Year: "2020", ReleaseDate: "2020-01-02",
		Publisher: "Nexus House", Language: "english",
		Genres:      []string{"Fantasy"},
		Description: "nexus teaser...", Summary: "nexus full summary",
		RuntimeMin: 600, CoverURL: "https://img/nexus.jpg",
	}
}

func TestMergeGapFill(t *testing.T) {
	nexus := full()
	nexus.SeriesName, nexus.SeriesPosition = "", "" // Audnexus missing series
	nexus.Subtitle = ""
	audible := &metadata.Book{
		ASIN: "B000000001", Region: "us",
		Title: "Audible Title", Subtitle: "Audible Sub",
		SeriesName: "Audible Series", SeriesPosition: "3",
	}

	got := Merge(map[string]*metadata.Book{SourceAudnexus: nexus, SourceAudible: audible}, nil)

	if got.Title != "Nexus Title" || got.Sources[FieldTitle] != SourceAudnexus {
		t.Errorf("precedence: title = %q from %q", got.Title, got.Sources[FieldTitle])
	}
	if got.Subtitle != "Audible Sub" || got.Sources[FieldSubtitle] != SourceAudible {
		t.Errorf("gap fill: subtitle = %q from %q", got.Subtitle, got.Sources[FieldSubtitle])
	}
	if got.SeriesName != "Audible Series" || got.SeriesPosition != "3" || got.Sources[FieldSeries] != SourceAudible {
		t.Errorf("series pair should move together: %q #%q from %q", got.SeriesName, got.SeriesPosition, got.Sources[FieldSeries])
	}
}

func TestMergeOverrideWins(t *testing.T) {
	nexus, audible := full(), full()
	audible.Title = "Audible Title"

	got := Merge(map[string]*metadata.Book{SourceAudnexus: nexus, SourceAudible: audible},
		map[string]string{FieldTitle: SourceAudible})
	if got.Title != "Audible Title" || got.Sources[FieldTitle] != SourceAudible {
		t.Errorf("override ignored: %q from %q", got.Title, got.Sources[FieldTitle])
	}
}

func TestMergeOverrideIgnoredWhenSourceEmpty(t *testing.T) {
	nexus := full()
	audible := &metadata.Book{ASIN: "B000000001", Region: "us", Title: "Audible Title"}

	got := Merge(map[string]*metadata.Book{SourceAudnexus: nexus, SourceAudible: audible},
		map[string]string{FieldNarrators: SourceAudible}) // audible has none
	if got.Narrators[0] != "Nexus Narrator" || got.Sources[FieldNarrators] != SourceAudnexus {
		t.Errorf("empty override should fall back: %v from %q", got.Narrators, got.Sources[FieldNarrators])
	}
}

func TestMergeDescriptionPrefersFullSummary(t *testing.T) {
	nexus := full()
	nexus.Summary = "" // Audnexus only has the truncated teaser
	audible := &metadata.Book{Description: "audible teaser...", Summary: "audible full summary"}

	got := Merge(map[string]*metadata.Book{SourceAudnexus: nexus, SourceAudible: audible}, nil)
	if got.Summary != "audible full summary" || got.Sources[FieldDescription] != SourceAudible {
		t.Errorf("completeness rule: summary = %q from %q", got.Summary, got.Sources[FieldDescription])
	}
	// The blurb pair moves together — the audible teaser rides along.
	if got.Description != "audible teaser..." {
		t.Errorf("description should come from the same source: %q", got.Description)
	}
}

func TestMergeSingleSource(t *testing.T) {
	nexus := full()
	got := Merge(map[string]*metadata.Book{SourceAudnexus: nexus}, nil)
	if got.ASIN != "B000000001" || got.Title != "Nexus Title" {
		t.Errorf("identity/title: %+v", got)
	}
	for _, f := range []string{FieldTitle, FieldAuthors, FieldSeries, FieldDescription} {
		if got.Sources[f] != SourceAudnexus {
			t.Errorf("Sources[%s] = %q", f, got.Sources[f])
		}
	}
	if _, ok := got.Sources[FieldSubtitle]; got.Subtitle == "" && !ok {
		// full() has a subtitle, so this branch shouldn't trigger; guard anyway
		t.Errorf("unexpected empty subtitle")
	}
}

func TestMergeUnsuppliedFieldHasNoProvenance(t *testing.T) {
	got := Merge(map[string]*metadata.Book{SourceAudnexus: {Title: "T"}}, nil)
	if _, ok := got.Sources[FieldSeries]; ok {
		t.Errorf("series provenance recorded for a field nobody supplied")
	}
}

// --- Aggregator ---

type fakeSource struct {
	book  *metadata.Book
	err   error
	calls int
}

func (f *fakeSource) GetBook(ctx context.Context, asin, region string) (*metadata.Book, error) {
	f.calls++
	return f.book, f.err
}

type memCache map[string]*metadata.Book

func (m memCache) CachedBook(source, asin, region string) (*metadata.Book, error) {
	return m[source+"|"+asin+"|"+region], nil
}
func (m memCache) CacheBook(source, asin, region string, b *metadata.Book) error {
	m[source+"|"+asin+"|"+region] = b
	return nil
}

func agg(nexus, audible *fakeSource, c Cache) *Aggregator {
	return &Aggregator{
		Sources: map[string]BookSource{SourceAudnexus: nexus, SourceAudible: audible},
		Order:   []string{SourceAudnexus, SourceAudible},
		Cache:   c,
	}
}

func TestAggregatorMergesAndCaches(t *testing.T) {
	nexus := &fakeSource{book: full()}
	audible := &fakeSource{book: &metadata.Book{Title: "A", Subtitle: "Audible Sub"}}
	cache := memCache{}

	res, err := agg(nexus, audible, cache).GetBook(context.Background(), "B000000001", "us", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Book.Title != "Nexus Title" || len(res.PerSource) != 2 {
		t.Errorf("merge: %+v", res.Book)
	}
	if cache["audnexus|B000000001|us"] == nil || cache["audible|B000000001|us"] == nil {
		t.Error("raw records not cached per source")
	}

	// Second call must be served from cache.
	if _, err := agg(nexus, audible, cache).GetBook(context.Background(), "B000000001", "us", nil); err != nil {
		t.Fatal(err)
	}
	if nexus.calls != 1 || audible.calls != 1 {
		t.Errorf("cache not consulted: nexus=%d audible=%d calls", nexus.calls, audible.calls)
	}
}

func TestAggregatorPrimaryNotFoundAllowsSecondaryOnly(t *testing.T) {
	nexus := &fakeSource{err: metadata.NotFound("nope")}
	audible := &fakeSource{book: &metadata.Book{ASIN: "B000000001", Region: "us", Title: "Audible Only"}}

	res, err := agg(nexus, audible, nil).GetBook(context.Background(), "B000000001", "us", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Book.Title != "Audible Only" || res.Book.Sources[FieldTitle] != SourceAudible {
		t.Errorf("secondary-only merge: %+v", res.Book)
	}
}

func TestAggregatorPrimaryTransientErrorFails(t *testing.T) {
	nexus := &fakeSource{err: errors.New("HTTP 502")}
	audible := &fakeSource{book: full()}

	if _, err := agg(nexus, audible, nil).GetBook(context.Background(), "B000000001", "us", nil); err == nil {
		t.Fatal("primary outage must fail, not silently degrade the snapshot")
	}
}

func TestAggregatorSecondaryErrorIsANote(t *testing.T) {
	nexus := &fakeSource{book: full()}
	audible := &fakeSource{err: errors.New("HTTP 503")}

	res, err := agg(nexus, audible, nil).GetBook(context.Background(), "B000000001", "us", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Notes) != 1 || res.Book.Title != "Nexus Title" {
		t.Errorf("expected note + audnexus-only merge, got notes=%v book=%+v", res.Notes, res.Book)
	}
}

func TestAggregatorNothingAnywhereIsNotFound(t *testing.T) {
	nexus := &fakeSource{err: metadata.NotFound("nope")}
	audible := &fakeSource{err: metadata.NotFound("nope")}

	_, err := agg(nexus, audible, nil).GetBook(context.Background(), "B000000001", "us", nil)
	if !metadata.IsNotFound(err) {
		t.Fatalf("want NotFound, got %v", err)
	}
}

func TestAggregatorIdentityIsTheRequest(t *testing.T) {
	echoed := full()
	echoed.ASIN, echoed.Region = "B0DIFFERENT", "de" // source echoing another marketplace
	nexus := &fakeSource{book: echoed}

	res, err := agg(nexus, &fakeSource{err: metadata.NotFound("x")}, nil).GetBook(context.Background(), "B000000001", "us", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Book.ASIN != "B000000001" || res.Book.Region != "us" {
		t.Errorf("identity drifted: %s %s", res.Book.ASIN, res.Book.Region)
	}
}

// lockeSources is the shape of both catalogs' records for The Lies of Locke
// Lamora: Audible's ladders put it under a non-fiction category too, Audnexus
// flattens that and labels the book fiction.
func lockeSources() map[string]*metadata.Book {
	return map[string]*metadata.Book{
		SourceAudnexus: {ASIN: "B004K50434", Title: "The Lies of Locke Lamora", LiteratureType: "fiction",
			Genres:    []string{"Relationships, Parenting & Personal Development", "Science Fiction & Fantasy"},
			SubGenres: []string{"Parenting & Families", "Fantasy", "Epic"}},
		SourceAudible: {ASIN: "B004K50434", Title: "The Lies of Locke Lamora",
			Genres:    []string{"Relationships, Parenting & Personal Development", "Science Fiction & Fantasy"},
			SubGenres: []string{"Parenting & Families", "Fantasy", "Epic"},
			GenrePaths: [][]string{
				{"Relationships, Parenting & Personal Development", "Parenting & Families"},
				{"Science Fiction & Fantasy", "Fantasy", "Epic"},
				{"Science Fiction & Fantasy", "Fantasy"},
			}},
	}
}

func TestMergeCleansGenres(t *testing.T) {
	b := Merge(lockeSources(), nil)
	if b.Sources[FieldGenres] != SourceAudible {
		t.Errorf("genres should come from Audible's ladders, got %q", b.Sources[FieldGenres])
	}
	if strings.Join(b.Genres, "|") != "Science Fiction & Fantasy" || strings.Join(b.SubGenres, "|") != "Fantasy|Epic" {
		t.Errorf("got %q / %q", b.Genres, b.SubGenres)
	}
	if b.LiteratureType != "fiction" {
		t.Errorf("literature type not merged: %q", b.LiteratureType)
	}
	if _, ok := b.Sources["literature_type"]; ok {
		t.Error("literature type is not a user-facing field and must not appear in Sources")
	}
	// Picking Audnexus' flat list still drops the miscategorized genre.
	b = Merge(lockeSources(), map[string]string{FieldGenres: SourceAudnexus})
	if strings.Join(b.Genres, "|") != "Science Fiction & Fantasy" || len(b.SubGenres) != 0 {
		t.Errorf("audnexus pick: got %q / %q", b.Genres, b.SubGenres)
	}
}

func TestSourceValueShowsWhatWouldBeTagged(t *testing.T) {
	books := lockeSources()
	for _, src := range []string{SourceAudnexus, SourceAudible} {
		if got := SourceValue(books, src, FieldGenres); got != "Science Fiction & Fantasy" {
			t.Errorf("%s genres = %q", src, got)
		}
	}
	if got := SourceValue(books, SourceAudnexus, FieldTitle); got != "The Lies of Locke Lamora" {
		t.Errorf("other fields are the raw value, got %q", got)
	}
	delete(books, SourceAudible)
	if got := SourceValue(books, SourceAudible, FieldGenres); got != "" {
		t.Errorf("a missing source has no value, got %q", got)
	}
}
