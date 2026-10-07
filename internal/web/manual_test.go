package web

import (
	"reflect"
	"strings"
	"testing"

	"audioborker/internal/metadata"
)

// A real-shaped reply: chatty, fenced, with an LLM's usual liberties.
const sampleReply = "Sure! Here's the metadata for your recording:\n\n```json\n" + `{
  "title": "Mockingjay",
  "subtitle": null,
  "authors": ["Suzanne Collins"],
  "narrators": "Carolyn McCormick",
  "series_name": "The Hunger Games",
  "series_position": 3,
  "release_date": "2010-08-24",
  "publisher": "Scholastic Audio",
  "language": "English",
  "genres": ["Teen & Young Adult", "Science Fiction & Fantasy"],
  "sub_genres": ["Dystopian", "Teen & Young Adult"],
  "description": "My name is Katniss Everdeen.\r\n\r\nWhy am I not dead?",
  "asin": "unknown"
}` + "\n```\n\nLet me know if you need anything else!"

func TestParseManualReply(t *testing.T) {
	f, warnings, err := parseManualReply(sampleReply)
	if err != nil {
		t.Fatal(err)
	}
	want := manualForm{
		Title: "Mockingjay", Authors: "Suzanne Collins", Narrators: "Carolyn McCormick",
		SeriesName: "The Hunger Games", SeriesPosition: "3", Release: "2010-08-24",
		Publisher: "Scholastic Audio", Language: "english",
		Genres:      "Teen & Young Adult; Science Fiction & Fantasy; Dystopian",
		Description: "My name is Katniss Everdeen.\n\nWhy am I not dead?",
	}
	if f != want {
		t.Errorf("got  %+v\nwant %+v", f, want)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
}

func TestParseManualReplyLeniency(t *testing.T) {
	cases := []struct {
		name, reply string
		check       func(manualForm) bool
		warn        string
		err         string
	}{
		{name: "unfenced object in prose", reply: `Here: {"title": "X", "authors": ["A"]} hope it helps`,
			check: func(f manualForm) bool { return f.Title == "X" && f.Authors == "A" }},
		{name: "year only", reply: `{"title": "X", "release_date": 2010}`,
			check: func(f manualForm) bool { return f.Release == "2010" }},
		{name: "timestamp trimmed to the year", reply: `{"title": "X", "release_date": "2010-08-24T00:00:00Z"}`,
			check: func(f manualForm) bool { return f.Release == "2010" }, warn: "kept as the year"},
		{name: "nonsense date dropped", reply: `{"title": "X", "release_date": "summer"}`,
			check: func(f manualForm) bool { return f.Release == "" }, warn: "Ignored release date"},
		{name: "bad ASIN dropped", reply: `{"title": "X", "asin": "B0-NOPE"}`,
			check: func(f manualForm) bool { return f.ASIN == "" }, warn: "Ignored ASIN"},
		{name: "good ASIN uppercased", reply: `{"title": "X", "asin": "b004k50434"}`,
			check: func(f manualForm) bool { return f.ASIN == "B004K50434" }},
		{name: "wrong type ignored", reply: `{"title": "X", "publisher": {"name": "Y"}}`,
			check: func(f manualForm) bool { return f.Publisher == "" }, warn: `Ignored "publisher"`},
		{name: "semicolon list in a string", reply: `{"title": "X", "authors": "A; B;  ; A"}`,
			check: func(f manualForm) bool { return f.Authors == "A; B" }},
		{name: "no JSON", reply: "I couldn't find that audiobook.", err: "no JSON object"},
		{name: "broken JSON", reply: `{"title": "X",}`, err: "invalid JSON"},
		{name: "unrelated JSON", reply: `{"foo": 1}`, err: "none of the expected fields"},
	}
	for _, c := range cases {
		f, warnings, err := parseManualReply(c.reply)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: err = %v, want %q", c.name, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !c.check(f) {
			t.Errorf("%s: unexpected form %+v", c.name, f)
		}
		joined := strings.Join(warnings, " | ")
		if (c.warn == "") != (joined == "") || !strings.Contains(joined, c.warn) {
			t.Errorf("%s: warnings %q, want %q", c.name, joined, c.warn)
		}
	}
}

func TestManualFormBook(t *testing.T) {
	f, _, _ := parseManualReply(sampleReply)
	b, errs := f.book("id-1")
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if b.ReleaseDate != "2010-08-24" || b.Year != "2010" || b.LocalID != "id-1" || b.ASIN != "" {
		t.Errorf("got %+v", b)
	}
	if !reflect.DeepEqual(b.Genres, []string{"Teen & Young Adult", "Science Fiction & Fantasy", "Dystopian"}) {
		t.Errorf("genres %q", b.Genres)
	}
	if b.Blurb() != "My name is Katniss Everdeen.\n\nWhy am I not dead?" || b.Sources["narrators"] != "manual" {
		t.Errorf("got %+v", b)
	}

	// Round trip through the form keeps every value.
	if back := formFromBook(b); back != f {
		t.Errorf("round trip:\n got  %+v\n want %+v", back, f)
	}

	_, errs = manualForm{SeriesPosition: "2", Release: "soon", ASIN: "x"}.book("")
	for _, want := range []string{"Title is required", "author is required", "needs a series", "use YYYY-MM-DD", "must be 10 letters"} {
		if !strings.Contains(strings.Join(errs, " "), want) {
			t.Errorf("missing error %q in %q", want, errs)
		}
	}
}

func TestManualPrompt(t *testing.T) {
	p := manualPrompt(promptInput{
		Path:       "Suzanne Collins/Hunger Games/Mockingjay",
		Files:      []string{"Mockingjay - Hunger Games, Book 3.m4b"},
		RuntimeMin: 701,
		Current: &metadata.Book{Title: "Mockingjay", Authors: []string{"Suzanne Collins", "Carolyn McCormick"},
			Narrators: []string{"Carolyn McCormick"}, SeriesName: "Hunger Games", SeriesPosition: "3", Year: "2010",
			Genres: []string{"Audiobook"}},
		ChapterCount: 27,
		Chapters:     []string{"Chapter 1", "Chapter 2"},
		Editions: []metadata.SearchResult{{ASIN: "1338589040", Title: "Mockingjay", Authors: []string{"Suzanne Collins"},
			Narrators: []string{"Tatiana Maslany"}, RuntimeMin: 679, Year: "2019"}},
	})
	for _, want := range []string{
		"only a JSON object",
		"- Location: Suzanne Collins/Hunger Games/Mockingjay",
		"- File: Mockingjay - Hunger Games, Book 3.m4b",
		"11h 41m (701 minutes)",
		`narrator "Carolyn McCormick"`,
		`series "Hunger Games 3"`,
		`27 chapters, starting "Chapter 1", "Chapter 2"`,
		`"Mockingjay" by Suzanne Collins, read by Tatiana Maslany, 11h 19m, 2019 (ASIN 1338589040)`,
		"never for this recording's narrator",
		`"narrators": ["who reads THIS recording"]`,
		"Relationships, Parenting & Personal Development; Religion & Spirituality",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt is missing %q:\n%s", want, p)
		}
	}
	// The skeleton in the prompt must itself be the shape the parser reads.
	start := strings.Index(p, "```json\n")
	f, _, err := parseManualReply(p[start:])
	if err != nil || f.Title == "" || f.Authors == "" {
		t.Errorf("prompt skeleton doesn't parse as a reply: %v %+v", err, f)
	}
}

func TestRealGenres(t *testing.T) {
	got := realGenres([]string{"Audiobook", "Science Fiction & Fantasy", "audio books", "Dystopian"})
	if !reflect.DeepEqual(got, []string{"Science Fiction & Fantasy", "Dystopian"}) {
		t.Errorf("got %q", got)
	}
}
