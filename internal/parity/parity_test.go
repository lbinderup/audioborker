package parity

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"audioborker/internal/match"
	"audioborker/internal/metadata"
	"audioborker/internal/metadata/aggregate"
	"audioborker/internal/metadata/audible"
	"audioborker/internal/metadata/audnexus"
	"audioborker/internal/metadata/embedded"
)

var update = flag.Bool("update", false, "re-record testdata/golden.json from the Go implementation")

// golden is the whole fixture file. Every case carries its inputs and the Go
// implementation's outputs, so the Python side needs nothing else.
type golden struct {
	Normalize  []normalizeCase `json:"normalize"`
	Split      []splitCase     `json:"split_author_title"`
	Languages  []languageCase  `json:"language_for_region"`
	Score      []scoreCase     `json:"score"`
	Embedded   []embeddedCase  `json:"embedded"`
	HTMLToText []htmlCase      `json:"html_to_text"`
	Merge      []mergeCase     `json:"merge"`
	Genres     []genreCase     `json:"clean_genres"`
	Catalog    []catalogCase   `json:"catalog"`
}

type normalizeCase struct {
	In  string `json:"in"`
	Out string `json:"out"`
}

type splitCase struct {
	In     string `json:"in"`
	Author string `json:"author"`
	Title  string `json:"title"`
}

type languageCase struct {
	Region   string `json:"region"`
	Language string `json:"language"`
}

// result mirrors metadata.SearchResult with the snake_case keys the Python
// port uses (SearchResult itself has no JSON tags).
type result struct {
	ASIN       string   `json:"asin"`
	Region     string   `json:"region"`
	Title      string   `json:"title"`
	Subtitle   string   `json:"subtitle"`
	Authors    []string `json:"authors"`
	Narrators  []string `json:"narrators"`
	Year       string   `json:"year"`
	Language   string   `json:"language"`
	RuntimeMin int      `json:"runtime_min"`
	CoverURL   string   `json:"cover_url"`
}

type signals struct {
	Title      string `json:"title"`
	Author     string `json:"author"`
	RuntimeMin int    `json:"runtime_min"`
	Language   string `json:"language"`
}

type ranked struct {
	ASIN            string `json:"asin"`
	Score           int    `json:"score"`
	RuntimeMatch    string `json:"runtime_match"`
	RuntimeDeltaMin int    `json:"runtime_delta_min"`
}

type scoreCase struct {
	Name       string   `json:"name"`
	Signals    signals  `json:"signals"`
	Results    []result `json:"results"`
	Ranked     []ranked `json:"ranked"`
	AutoSelect bool     `json:"auto_select"`
}

type embeddedCase struct {
	Name string            `json:"name"`
	Tags map[string]string `json:"tags"`
	Book *metadata.Book    `json:"book"`
}

type htmlCase struct {
	In  string `json:"in"`
	Out string `json:"out"`
}

// genreCase is metadata.CleanGenres (and LiteratureKind) on one book.
type genreCase struct {
	Name      string         `json:"name"`
	Book      *metadata.Book `json:"book"`
	Kind      string         `json:"kind"`
	Genres    []string       `json:"genres"`
	SubGenres []string       `json:"sub_genres"`
}

type mergeCase struct {
	Name      string                    `json:"name"`
	Books     map[string]*metadata.Book `json:"books"`
	Overrides map[string]string         `json:"overrides"`
	Out       *metadata.Book            `json:"out"`
}

// catalogCase is one captured live response (testdata/api) and what the Go
// client makes of it.
type catalogCase struct {
	Kind     string         `json:"kind"` // audnexus_book | audible_product | audible_search
	Fixture  string         `json:"fixture"`
	ASIN     string         `json:"asin,omitempty"`
	Region   string         `json:"region"`
	NotFound bool           `json:"not_found,omitempty"`
	Book     *metadata.Book `json:"book,omitempty"`
	Results  []result       `json:"results,omitempty"`
}

func TestGolden(t *testing.T) {
	got := build(t)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(got); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "golden.json")
	if *update {
		if err := os.WriteFile(path, buf.Bytes(), 0o666); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v — record it with: go test ./internal/parity -update", err)
	}
	if !bytes.Equal(bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")), buf.Bytes()) {
		t.Fatal("behaviour mirrored by the Plex agent changed. If intended: " +
			"go test ./internal/parity -update, then port the change to " +
			"plex/Audioborker.bundle until `python -m unittest` in plex/ passes.")
	}
}

func build(t *testing.T) golden {
	var g golden

	for _, in := range []string{
		"Project Hail Mary [Unabridged] (2021).mp3",
		"01 - The Way of Kings",
		"andy_weir-the_martian_64k",
		"Dune (Complete Audiobook) {Frank Herbert}",
		"Émile Zola - Germinal",
		"The.Hobbit.m4b",
		"1984",
		"",
		"Scott Lynch - The Lies of Locke Lamora [B004K50434]",
		"The Colour of Magic [B09LYZPFLZ].m4b",
		"Terry Pratchett/Discworld/The Colour of Magic",
		`C:\Books\Brandon Sanderson - Mistborn 01 - The Final Empire (Retail MP3)`,
		"  The   Name of the Wind  [128k] .m4a",
		"Ærø – Ålborg Ñandú.flac",
		"Book.With.Many.Dots.audiobook",
		"Dune.ä",
		"trailing/slashes///",
		"Œuvres complètes - Volume 2_Disc 1",
	} {
		g.Normalize = append(g.Normalize, normalizeCase{in, match.Normalize(in)})
	}

	for _, in := range []string{
		"Andy Weir - Project Hail Mary",
		"Project Hail Mary",
		"The Long And Winding Road Of Many Words - Special Edition",
		"A - B - C",
		"Brandon Sanderson - Mistborn 01 - The Final Empire",
		" - Leading dash",
	} {
		a, ti := match.SplitAuthorTitle(in)
		g.Split = append(g.Split, splitCase{in, a, ti})
	}

	for _, r := range []string{"us", "uk", "au", "ca", "in", "de", "fr", "es", "it", "jp", "zz", "", "UK"} {
		g.Languages = append(g.Languages, languageCase{r, match.LanguageForRegion(r)})
	}

	g.Score = scoreCases(t)
	g.Embedded = embeddedCases()

	for _, in := range []string{
		"", "plain", "<p>one</p><p>two</p>", "a<br>b", "<i>x</i> &amp; <b>y</b>",
		"&quot;q&quot; &#39;s&#39;", "<p>trailing space </p>", "<ul><li>a</li><li>b</li></ul>",
		"a\u00a0b", "<p>A</p>\r\n<p>B</p>", "x&nbsp;y", "&lt;b&gt;", "line<br/>break<BR />two",
		"<div>a</div><div>b</div>", "a\n\n\n\nb", "&hellip;&mdash;&#8217;&#x2019;",
		"  spaced \t\t out  ", "<p>Para one.</p> <p>Para two.</p>",
	} {
		g.HTMLToText = append(g.HTMLToText, htmlCase{in, metadata.HTMLToText(in)})
	}

	g.Catalog = catalogCases(t)
	g.Merge = mergeCases(g.Catalog)
	g.Genres = genreCases(g.Catalog)
	return g
}

func toResults(rs []metadata.SearchResult) []result {
	out := make([]result, 0, len(rs))
	for _, r := range rs {
		out = append(out, result{r.ASIN, r.Region, r.Title, r.Subtitle, r.Authors, r.Narrators,
			r.Year, r.Language, r.RuntimeMin, r.CoverURL})
	}
	return out
}

func fromResults(rs []result) []metadata.SearchResult {
	out := make([]metadata.SearchResult, 0, len(rs))
	for _, r := range rs {
		out = append(out, metadata.SearchResult{ASIN: r.ASIN, Region: r.Region, Title: r.Title,
			Subtitle: r.Subtitle, Authors: r.Authors, Narrators: r.Narrators, Year: r.Year,
			Language: r.Language, RuntimeMin: r.RuntimeMin, CoverURL: r.CoverURL})
	}
	return out
}

func scored(name string, sig signals, in []result) scoreCase {
	s := match.Signals{Title: sig.Title, Author: sig.Author, RuntimeMin: sig.RuntimeMin, Language: sig.Language}
	rs := match.Score(fromResults(in), s)
	c := scoreCase{Name: name, Signals: sig, Results: in, AutoSelect: match.AutoSelect(rs, s)}
	for _, r := range rs {
		c.Ranked = append(c.Ranked, ranked{r.ASIN, r.Score, r.RuntimeMatch, r.RuntimeDeltaMin})
	}
	return c
}

func scoreCases(t *testing.T) []scoreCase {
	weir := []string{"Andy Weir"}
	phm := func(asin string, min int, lang string) result {
		return result{ASIN: asin, Title: "Project Hail Mary", Authors: weir, RuntimeMin: min, Language: lang}
	}
	sigPHM := signals{Title: "Project Hail Mary", Author: "Andy Weir", RuntimeMin: 970}
	cases := []scoreCase{
		scored("title ordering", signals{Title: "Project Hail Mary", Author: "Andy Weir"}, []result{
			{ASIN: "R1", Title: "Project Hail Mary: Deluxe Edition", Authors: weir},
			{ASIN: "R2", Title: "Project Hail Mary", Authors: weir},
			{ASIN: "R3", Title: "Artemis", Authors: weir},
		}),
		scored("runtime separates editions", sigPHM, []result{phm("R1", 420, ""), phm("R2", 969, ""), phm("R3", 1500, "")}),
		scored("runtime never outranks author", sigPHM, []result{
			{ASIN: "R1", Title: "Project Hail Mary", Authors: []string{"Someone Else"}, RuntimeMin: 970},
			{ASIN: "R2", Title: "Project Hail Mary", Authors: weir, RuntimeMin: 1000},
		}),
		scored("unknown local runtime", signals{Title: "A Book"}, []result{{ASIN: "R1", Title: "A Book", RuntimeMin: 100}}),
		scored("lone exact match", sigPHM, []result{phm("R1", 970, "")}),
		scored("two exact editions", sigPHM, []result{phm("R1", 970, ""), phm("R2", 970, "")}),
		scored("runtime off", sigPHM, []result{phm("R1", 500, "")}),
		scored("no runtime on either side", signals{Title: "Project Hail Mary"}, []result{phm("R1", 0, "")}),
		scored("coincidental runtime, wrong title", sigPHM, []result{
			{ASIN: "R1", Title: "Something Entirely Different", RuntimeMin: 970}}),
		scored("empty title hint", signals{RuntimeMin: 970}, []result{phm("R1", 970, "")}),
		scored("foreign edition", signals{Title: "Project Hail Mary", RuntimeMin: 970, Language: "english"},
			[]result{phm("R1", 970, "spanish")}),
		scored("language preference", signals{Title: "Harry Potter", RuntimeMin: 498, Language: "english"}, []result{
			{ASIN: "R1", Title: "Harry Potter", RuntimeMin: 498, Language: "spanish"},
			{ASIN: "R2", Title: "Harry Potter", RuntimeMin: 505, Language: "english"},
		}),
		scored("close, near and off bands", signals{Title: "Band", RuntimeMin: 1000}, []result{
			{ASIN: "R1", Title: "Band", RuntimeMin: 1015},
			{ASIN: "R2", Title: "Band", RuntimeMin: 1040},
			{ASIN: "R3", Title: "Band", RuntimeMin: 1100},
			{ASIN: "R4", Title: "Band", RuntimeMin: 1200},
			{ASIN: "R5", Title: "Band", RuntimeMin: 1005},
		}),
		scored("subtitle containment", signals{Title: "The Way of Kings", Author: "Brandon Sanderson"}, []result{
			{ASIN: "R1", Title: "The Way of Kings: Book One of the Stormlight Archive", Authors: []string{"Brandon Sanderson"}},
			{ASIN: "R2", Title: "The Way", Authors: []string{"Brandon Sanderson"}},
			{ASIN: "R3", Title: "Way of Kings", Authors: []string{"Kate Reading", "Brandon Sanderson"}},
		}),
		scored("diacritics and case folding", signals{Title: "Les Misérables", Author: "Victor Hugo"}, []result{
			{ASIN: "R1", Title: "LES MISERABLES", Authors: []string{"VICTOR HUGO"}},
			{ASIN: "R2", Title: "Les Misérables", Authors: []string{"Víctor Húgo"}},
			{ASIN: "R3", Title: "Ænid & Œdipus", Authors: []string{"Æsop"}},
		}),
		scored("final sigma and dotted I", signals{Title: "ΣΟΦΙΑΣ İstanbul"}, []result{
			{ASIN: "R1", Title: "σοφιασ istanbul"},
			{ASIN: "R2", Title: "Σοφίας Istanbul"},
		}),
	}

	// Real searches, with the signals the match screen would build.
	for _, c := range []struct {
		name, fixture string
		sig           signals
	}{
		{"live: Locke Lamora from file tags", "audible_search_locke_us.json",
			signals{Title: "The Lies of Locke Lamora", Author: "Scott Lynch", RuntimeMin: 1319, Language: "english"}},
		{"live: Locke Lamora, other edition's runtime", "audible_search_locke_us.json",
			signals{Title: "The Lies of Locke Lamora", Author: "Scott Lynch", RuntimeMin: 1356, Language: "english"}},
		{"live: Colour of Magic with runtime", "audible_search_colour_us.json",
			signals{Title: "The Colour of Magic", Author: "Terry Pratchett", RuntimeMin: 478, Language: "english"}},
		{"live: Colour of Magic keywords only", "audible_search_colour_us.json",
			signals{Title: "the colour of magic", Language: "english"}},
		{"live: Last Colony typed with the author (a podcast episode is filtered out)", "audible_search_lastcolony_us.json",
			signals{Title: "The Last Colony John Scalzi", Language: "english"}},
		{"live: Last Colony from tags with runtime", "audible_search_lastcolony_us.json",
			signals{Title: "The Last Colony", Author: "John Scalzi", RuntimeMin: 591, Language: "english"}},
	} {
		rs := searchFixture(t, c.fixture, "us")
		cases = append(cases, scored(c.name, c.sig, toResults(rs)))
	}
	return cases
}

func embeddedCases() []embeddedCase {
	cases := []struct {
		name string
		tags map[string]string
	}{
		{"audioborker output via ffprobe", map[string]string{
			"major_brand": "M4A ", "title": "The Colour of Magic", "artist": "Terry Pratchett",
			"composer": "Colin Morgan, Peter Serafinowicz, Bill Nighy", "comment": "Over 1 million Discworld audiobooks sold – discover…",
			"genre": "Science Fiction & Fantasy", "description": "Over 1 million Discworld audiobooks sold – discover…",
			"album_artist": "Terry Pratchett", "sort_album": "Discworld 1 - The Colour of Magic",
			"synopsis": "Over 1 million Discworld audiobooks sold – discover the extraordinary universe",
			"album":    "The Colour of Magic", "date": "2022-07-07", "AUDIBLE_ASIN": "B09LYZPFLZ", "media_type": "2",
			"PART": "1", "SUBTITLE": "Discworld, Book 1", "SERIES": "Discworld",
		}},
		{"audioborker output via the bundle's atom reader", map[string]string{
			"title": "The Colour of Magic", "artist": "Terry Pratchett", "album_artist": "Terry Pratchett",
			"composer": "Colin Morgan, Peter Serafinowicz, Bill Nighy", "narrator": "Colin Morgan, Peter Serafinowicz, Bill Nighy",
			"publisher": "Transworld Digital", "movement_name": "Discworld", "movement": "1",
			"AUDIBLE_ASIN": "B09LYZPFLZ", "PART": "1", "SERIES": "Discworld", "date": "2022-07-07",
		}},
		{"m4b-tool style", map[string]string{
			"title": "Chapter 1", "album": "The Book", "artist": "Ann Author; Bob Writer", "composer": "Nat Narrator",
			"ASIN": "b0abcdefgh", "date": "2019", "series": "The Series", "series-part": "2",
		}},
		{"namespaced freeform keys", map[string]string{
			"----:com.apple.iTunes:Series-Part": "1.5", "----:com.apple.iTunes:SERIES": "Cosmere",
			"©nam": "ignored spelling", "album": "Album Only",
		}},
		{"asin fallback key", map[string]string{"audible_asin": "bad", "custom_asin": "B012345678", "title": "T"}},
		{"asin wrong length", map[string]string{"asin": "B0123", "title": "T"}},
		{"timestamp date", map[string]string{"date": "2021-03-04T00:00:00Z", "artist": "A & B"}},
		{"padded year", map[string]string{"date": "  1999 ", "artist": "Doe, John & Jane"}},
		{"text date", map[string]string{"date": "March 2001", "composer": " ; "}},
		{"short date", map[string]string{"year": "20-01-01", "album_artist": "Solo Author"}},
		{"longest blurb by bytes", map[string]string{"description": "abcde", "synopsis": "ééé", "comment": "abcd"}},
		{"show and episode as series", map[string]string{"show": "Show Name", "episode_id": "3", "label": "Label Pub"}},
		{"all genres in GENRES", map[string]string{
			"genre": "Science Fiction & Fantasy", "----:com.pilabor.tone:GENRES": "Science Fiction & Fantasy; Fantasy;  ; Epic ",
			"----:com.pilabor.tone:AUDIOBORKER_SOURCE": "manual", "----:com.pilabor.tone:AUDIOBORKER_ID": "6f0c1a52-1b8e-4f0e-9a51-3c6c1f7d2e10",
		}},
		{"blank GENRES falls back to genre", map[string]string{"genre": "Romance", "GENRES": " ; "}},
		{"empty", map[string]string{}},
	}
	out := make([]embeddedCase, 0, len(cases))
	for _, c := range cases {
		out = append(out, embeddedCase{c.name, c.tags, embedded.Book(c.tags)})
	}
	return out
}

func mergeCases(catalog []catalogCase) []mergeCase {
	nexus := &metadata.Book{ASIN: "B1", Region: "us", Title: "Nexus Title", Authors: []string{"A"},
		Description: "Teaser...", Genres: []string{"Fantasy"}, RuntimeMin: 600}
	aud := &metadata.Book{ASIN: "B1", Region: "us", Title: "Audible Title", Subtitle: "Sub",
		Authors: []string{"A", "B"}, Narrators: []string{"N"}, SeriesName: "S", SeriesPosition: "2",
		Year: "2020", ReleaseDate: "2020-05-01", Publisher: "Pub", Language: "english",
		Genres: []string{"Sci-Fi", "Fantasy"}, Summary: "Full blurb.", CoverURL: "https://img/1000.jpg"}
	cases := []mergeCase{
		{Name: "audnexus primary, audible fills gaps", Books: map[string]*metadata.Book{"audnexus": nexus, "audible": aud}},
		{Name: "overrides", Books: map[string]*metadata.Book{"audnexus": nexus, "audible": aud},
			Overrides: map[string]string{"title": "audible", "genres": "audible", "runtime_min": "audible", "authors": "nobody"}},
		{Name: "audible only", Books: map[string]*metadata.Book{"audible": aud}},
		{Name: "audnexus only", Books: map[string]*metadata.Book{"audnexus": nexus}},
	}
	// The real per-source records for each ASIN captured from both catalogs.
	live := map[string]map[string]*metadata.Book{}
	for _, c := range catalog {
		if c.Book == nil || c.Region != "us" {
			continue
		}
		src := map[string]string{"audnexus_book": "audnexus", "audible_product": "audible"}[c.Kind]
		if live[c.ASIN] == nil {
			live[c.ASIN] = map[string]*metadata.Book{}
		}
		live[c.ASIN][src] = c.Book
	}
	for _, asin := range []string{"B004K50434", "B09LYZPFLZ"} {
		cases = append(cases, mergeCase{Name: "live " + asin, Books: live[asin]})
	}
	for i := range cases {
		cases[i].Out = aggregate.Merge(cases[i].Books, cases[i].Overrides)
	}
	return cases
}

func genreCases(catalog []catalogCase) []genreCase {
	locke := [][]string{
		{"Relationships, Parenting & Personal Development", "Parenting & Families"},
		{"Relationships, Parenting & Personal Development", "Relationships"},
		{"Science Fiction & Fantasy", "Fantasy", "Epic"},
		{"Science Fiction & Fantasy", "Fantasy", "Humorous"},
		{"Science Fiction & Fantasy", "Fantasy", "Paranormal & Urban", "Urban"},
	}
	books := []struct {
		name string
		book metadata.Book
	}{
		{"labelled fiction", metadata.Book{LiteratureType: "fiction", GenrePaths: locke}},
		{"label spelled oddly", metadata.Book{LiteratureType: " Fiction ", GenrePaths: locke}},
		{"unknown label falls back to the majority", metadata.Book{LiteratureType: "poetry", GenrePaths: locke}},
		{"majority", metadata.Book{GenrePaths: locke}},
		{"tie", metadata.Book{GenrePaths: [][]string{{"History", "Europe"}, {"Literature & Fiction", "Historical Fiction"}}}},
		{"non-fiction", metadata.Book{LiteratureType: "nonfiction", GenrePaths: [][]string{{"History", "Europe"}, {"Literature & Fiction", "Classics"}, {"History", "Europe", "Ancient"}}}},
		{"support order", metadata.Book{LiteratureType: "fiction", GenrePaths: [][]string{
			{"Comedy & Humor", "Literature & Fiction"}, {"Science Fiction & Fantasy", "Fantasy", "Epic"}, {"Science Fiction & Fantasy", "Humorous"}}}},
		{"label contradicts everything", metadata.Book{LiteratureType: "nonfiction", GenrePaths: [][]string{{"Science Fiction & Fantasy", "Fantasy"}}}},
		{"other storefront names", metadata.Book{LiteratureType: "fiction", GenrePaths: [][]string{{"Science-Fiction & Fantasy", "Fantasy"}, {"Ratgeber", "Familie"}}}},
		{"empty rungs and paths", metadata.Book{LiteratureType: "fiction", GenrePaths: [][]string{{}, {"", "x"}, {"Romance", "", "Historical"}}}},
		{"flat with a drop", metadata.Book{LiteratureType: "fiction",
			Genres: []string{"Relationships, Parenting & Personal Development", "Science Fiction & Fantasy"}, SubGenres: []string{"Parenting & Families", "Fantasy"}}},
		{"flat, nothing to drop", metadata.Book{LiteratureType: "fiction", Genres: []string{"Science Fiction & Fantasy"}, SubGenres: []string{"Fantasy", "Epic"}}},
		{"flat, unlabelled", metadata.Book{Genres: []string{"Relationships, Parenting & Personal Development", "Science Fiction & Fantasy"}, SubGenres: []string{"Fantasy"}}},
		{"flat, everything contradicts", metadata.Book{LiteratureType: "fiction", Genres: []string{"History"}, SubGenres: []string{"Europe"}}},
		{"nothing", metadata.Book{LiteratureType: "fiction"}},
	}
	var out []genreCase
	add := func(name string, b metadata.Book) {
		g, s := metadata.CleanGenres(&b)
		out = append(out, genreCase{name, &b, metadata.LiteratureKind(&b), g, s})
	}
	for _, c := range books {
		add(c.name, c.book)
	}
	// Each live record on its own, as a per-field override would leave it.
	for _, c := range catalog {
		if c.Book != nil {
			add("live "+c.Kind+" "+c.ASIN, *c.Book)
		}
	}
	return out
}

// fixtureTransport answers every request with one captured response body.
// The catalog clients use the default transport, and the Audible client
// builds its host from the region, so this is how they are pointed at a
// capture without touching their code.
type fixtureTransport struct{ body []byte }

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(bytes.NewReader(f.body)), Request: r}, nil
}

func withFixture(t *testing.T, name string, fn func()) {
	body, err := os.ReadFile(filepath.Join("testdata", "api", name))
	if err != nil {
		t.Fatal(err)
	}
	saved := http.DefaultTransport
	http.DefaultTransport = fixtureTransport{body}
	defer func() { http.DefaultTransport = saved }()
	fn()
}

func searchFixture(t *testing.T, name, region string) []metadata.SearchResult {
	var rs []metadata.SearchResult
	withFixture(t, name, func() {
		var err error
		rs, err = audible.New("parity").Search(context.Background(), metadata.SearchQuery{Keywords: "x", Region: region})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	})
	return rs
}

func catalogCases(t *testing.T) []catalogCase {
	var out []catalogCase
	for _, c := range []catalogCase{
		{Kind: "audnexus_book", Fixture: "audnexus_book_B004K50434_us.json", ASIN: "B004K50434", Region: "us"},
		{Kind: "audnexus_book", Fixture: "audnexus_book_B09LYZPFLZ_us.json", ASIN: "B09LYZPFLZ", Region: "us"},
		{Kind: "audible_product", Fixture: "audible_product_B004K50434_us.json", ASIN: "B004K50434", Region: "us"},
		{Kind: "audible_product", Fixture: "audible_product_B09LYZPFLZ_us.json", ASIN: "B09LYZPFLZ", Region: "us"},
		{Kind: "audible_product", Fixture: "audible_product_B09LYZPFLZ_uk.json", ASIN: "B09LYZPFLZ", Region: "uk"},
		{Kind: "audible_search", Fixture: "audible_search_locke_us.json", Region: "us"},
		{Kind: "audible_search", Fixture: "audible_search_colour_us.json", Region: "us"},
		{Kind: "audible_search", Fixture: "audible_search_lastcolony_us.json", Region: "us"},
	} {
		withFixture(t, c.Fixture, func() {
			ctx := context.Background()
			var err error
			switch c.Kind {
			case "audnexus_book":
				c.Book, err = audnexus.New("https://api.audnex.us", "parity").GetBook(ctx, c.ASIN, c.Region)
			case "audible_product":
				c.Book, err = audible.New("parity").GetBook(ctx, c.ASIN, c.Region)
			case "audible_search":
				var rs []metadata.SearchResult
				rs, err = audible.New("parity").Search(ctx, metadata.SearchQuery{Keywords: "x", Region: c.Region})
				c.Results = toResults(rs)
			}
			if metadata.IsNotFound(err) {
				c.NotFound, err = true, nil
			}
			if err != nil {
				t.Fatalf("%s: %v", c.Fixture, err)
			}
		})
		out = append(out, c)
	}
	return out
}
