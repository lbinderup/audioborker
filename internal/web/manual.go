package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"audioborker/internal/metadata"
)

// Manual tagging is the last resort for an edition no catalog has (an
// out-of-print narration, a recording Audible never sold). The user gets a
// prompt to paste into any LLM, pastes the JSON reply back, reviews the
// fields and picks a cover; nothing here talks to an LLM, so no API keys and
// no data leaves the machine without the user carrying it.
//
// These are the pure parts: the prompt, the reply parser and the form ↔ book
// mapping. handlers_manual.go wires them to the page.

// manualForm is the review form, every field as the text the user edits.
// Lists are "; "-separated: commas appear inside names ("Doe, John") but
// semicolons essentially never do.
type manualForm struct {
	Title          string
	Subtitle       string
	Authors        string
	Narrators      string
	SeriesName     string
	SeriesPosition string
	Release        string // YYYY-MM-DD or YYYY
	Publisher      string
	Language       string
	Genres         string // most fitting first; the first goes in the genre tag
	Description    string
	ASIN           string // optional: only if this exact recording has one
}

const listSep = "; "

func formFromBook(b *metadata.Book) manualForm {
	if b == nil {
		return manualForm{}
	}
	release := b.ReleaseDate
	if release == "" {
		release = b.Year
	}
	return manualForm{
		Title:          b.Title,
		Subtitle:       b.Subtitle,
		Authors:        strings.Join(b.Authors, listSep),
		Narrators:      strings.Join(b.Narrators, listSep),
		SeriesName:     b.SeriesName,
		SeriesPosition: b.SeriesPosition,
		Release:        release,
		Publisher:      b.Publisher,
		Language:       b.Language,
		Genres:         strings.Join(append(append([]string(nil), b.Genres...), b.SubGenres...), listSep),
		Description:    b.Blurb(),
		ASIN:           b.ASIN,
	}
}

var (
	dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	yearRe = regexp.MustCompile(`^\d{4}$`)
)

// book validates the form and builds the metadata snapshot a job carries.
// Every field's source is "manual", which the job page shows.
func (f manualForm) book(localID string) (*metadata.Book, []string) {
	var errs []string
	b := &metadata.Book{
		Title:          strings.TrimSpace(f.Title),
		Subtitle:       strings.TrimSpace(f.Subtitle),
		Authors:        splitField(f.Authors),
		Narrators:      splitField(f.Narrators),
		SeriesName:     strings.TrimSpace(f.SeriesName),
		SeriesPosition: strings.TrimSpace(f.SeriesPosition),
		Publisher:      strings.TrimSpace(f.Publisher),
		Language:       strings.ToLower(strings.TrimSpace(f.Language)),
		Genres:         splitField(f.Genres),
		Summary:        strings.TrimSpace(strings.ReplaceAll(f.Description, "\r\n", "\n")),
		LocalID:        localID,
	}
	if b.Title == "" {
		errs = append(errs, "Title is required.")
	}
	if len(b.Authors) == 0 {
		errs = append(errs, "At least one author is required.")
	}
	if b.SeriesPosition != "" && b.SeriesName == "" {
		errs = append(errs, "Book number needs a series.")
	}
	switch release := strings.TrimSpace(f.Release); {
	case release == "":
	case dateRe.MatchString(release):
		b.ReleaseDate, b.Year = release, release[:4]
	case yearRe.MatchString(release):
		b.Year = release
	default:
		errs = append(errs, fmt.Sprintf("Released %q: use YYYY-MM-DD or YYYY.", release))
	}
	if asin := strings.ToUpper(strings.TrimSpace(f.ASIN)); asin != "" {
		if !metadata.ValidASIN(asin) {
			errs = append(errs, fmt.Sprintf("ASIN %q must be 10 letters/digits.", asin))
		}
		b.ASIN = asin
	}
	b.Sources = map[string]string{}
	for _, k := range []string{"title", "subtitle", "authors", "narrators", "series", "release",
		"publisher", "language", "genres", "description"} {
		b.Sources[k] = "manual"
	}
	return b, errs
}

func splitField(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ";") {
		if p := strings.TrimSpace(part); p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// --- the LLM reply ------------------------------------------------------

var fenceRe = regexp.MustCompile("(?s)```(?:json|JSON)?\\s*(\\{.*?\\})\\s*```")

var errNoJSON = errors.New("no JSON object found in the reply")

// parseManualReply reads the JSON an LLM returned, tolerating what they
// wrap around it (prose, ```json fences) and their usual liberties: numbers
// where text was asked for, a string where a list was, "unknown" for null.
// Values that can't be used are dropped with a warning rather than failing
// the whole paste.
func parseManualReply(reply string) (manualForm, []string, error) {
	raw := ""
	if m := fenceRe.FindStringSubmatch(reply); m != nil {
		raw = m[1]
	} else if i, j := strings.Index(reply, "{"), strings.LastIndex(reply, "}"); i >= 0 && j > i {
		raw = reply[i : j+1]
	}
	if raw == "" {
		return manualForm{}, nil, errNoJSON
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return manualForm{}, nil, fmt.Errorf("invalid JSON: %v", err)
	}

	var warnings []string
	str := func(key string) string {
		switch v := obj[key].(type) {
		case nil:
			return ""
		case string:
			v = strings.TrimSpace(v)
			switch strings.ToLower(v) {
			case "null", "none", "unknown", "n/a":
				return ""
			}
			return v
		case json.Number:
			return v.String()
		default:
			warnings = append(warnings, fmt.Sprintf("Ignored %q: expected text.", key))
			return ""
		}
	}
	list := func(key string) []string {
		switch v := obj[key].(type) {
		case nil:
			return nil
		case string:
			return splitField(v)
		case []any:
			var out []string
			for _, item := range v {
				if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, strings.TrimSpace(s))
				}
			}
			return out
		default:
			warnings = append(warnings, fmt.Sprintf("Ignored %q: expected a list.", key))
			return nil
		}
	}

	f := manualForm{
		Title:          str("title"),
		Subtitle:       str("subtitle"),
		Authors:        strings.Join(list("authors"), listSep),
		Narrators:      strings.Join(list("narrators"), listSep),
		SeriesName:     str("series_name"),
		SeriesPosition: str("series_position"),
		Publisher:      str("publisher"),
		Language:       strings.ToLower(str("language")),
		Genres:         strings.Join(splitField(strings.Join(append(list("genres"), list("sub_genres")...), ";")), listSep),
		Description:    strings.ReplaceAll(str("description"), "\r\n", "\n"),
	}
	release := str("release_date")
	if release == "" {
		release = str("year")
	}
	switch {
	case release == "", dateRe.MatchString(release), yearRe.MatchString(release):
		f.Release = release
	case len(release) >= 4 && yearRe.MatchString(release[:4]):
		f.Release = release[:4]
		warnings = append(warnings, fmt.Sprintf("Release %q kept as the year %s only.", release, release[:4]))
	default:
		warnings = append(warnings, fmt.Sprintf("Ignored release date %q: not YYYY-MM-DD.", release))
	}
	if asin := strings.ToUpper(str("asin")); asin != "" {
		if metadata.ValidASIN(asin) {
			f.ASIN = asin
		} else {
			warnings = append(warnings, fmt.Sprintf("Ignored ASIN %q: not 10 letters/digits.", asin))
		}
	}
	if f == (manualForm{}) {
		return f, warnings, errors.New("the JSON has none of the expected fields")
	}
	return f, warnings, nil
}

// --- the prompt ----------------------------------------------------------

// promptInput is what the page knows about the book.
type promptInput struct {
	Path         string   // relative to the import volume or library
	Files        []string // base names, in merge order
	RuntimeMin   int      // 0 = unknown
	Current      *metadata.Book
	ChapterCount int
	Chapters     []string // the first few titles
	Editions     []metadata.SearchResult
}

// manualPrompt is the text the user pastes into an LLM. It hands over
// everything the page knows, separates facts about the *book* (which other
// editions share) from facts about *this recording* (which they don't), and
// asks for null over a guess — the reply is reviewed, but a confident wrong
// narrator is easy to miss.
func manualPrompt(in promptInput) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	w("I'm tagging an audiobook file and need accurate metadata for it. Use what you know, and search the web if you can. ")
	w("Reply with only a JSON object in a ```json code block, no other text.\n\n")

	w("What I know about the file:\n")
	w("- Location: %s\n", in.Path)
	switch n := len(in.Files); {
	case n == 1:
		w("- File: %s\n", in.Files[0])
	case n > 1:
		shown := in.Files
		if n > 6 {
			shown = in.Files[:6]
		}
		w("- %d files: %s", n, strings.Join(shown, ", "))
		if n > len(shown) {
			w(", …")
		}
		w("\n")
	}
	if in.RuntimeMin > 0 {
		w("- Length: %dh %02dm (%d minutes) — this tells editions apart\n", in.RuntimeMin/60, in.RuntimeMin%60, in.RuntimeMin)
	}
	if c := in.Current; c != nil {
		var tags []string
		add := func(label, v string) {
			if v = strings.TrimSpace(v); v != "" {
				tags = append(tags, fmt.Sprintf("%s %q", label, v))
			}
		}
		add("title", c.Title)
		add("author", strings.Join(c.Authors, ", "))
		add("narrator", strings.Join(c.Narrators, ", "))
		if c.SeriesName != "" {
			add("series", strings.TrimSpace(c.SeriesName+" "+c.SeriesPosition))
		}
		add("publisher", c.Publisher)
		if c.ReleaseDate != "" {
			add("date", c.ReleaseDate)
		} else {
			add("year", c.Year)
		}
		add("genre", strings.Join(c.Genres, ", "))
		if len(tags) > 0 {
			w("- Its current tags (possibly wrong or incomplete): %s\n", strings.Join(tags, "; "))
		}
	}
	if in.ChapterCount > 0 {
		w("- %d chapters", in.ChapterCount)
		if len(in.Chapters) > 0 {
			w(", starting %s", strings.Join(quoteAll(in.Chapters), ", "))
		}
		w("\n")
	}

	if len(in.Editions) > 0 {
		w("\nAudible lists these editions, but none is this exact recording. Use them only for facts about the book itself ")
		w("(title, series, description, genres), never for this recording's narrator, publisher or release date:\n")
		for _, e := range in.Editions {
			w("- %q by %s", e.Title, strings.Join(e.Authors, ", "))
			if len(e.Narrators) > 0 {
				w(", read by %s", strings.Join(e.Narrators, ", "))
			}
			if e.RuntimeMin > 0 {
				w(", %dh %02dm", e.RuntimeMin/60, e.RuntimeMin%60)
			}
			if e.Year != "" {
				w(", %s", e.Year)
			}
			w(" (ASIN %s)\n", e.ASIN)
		}
	}

	w("\nReturn exactly these keys. Use null for anything you are not sure of: a wrong narrator or date is worse than none.\n")
	w("```json\n")
	w(`{
  "title": "the book's title, without series or subtitle",
  "subtitle": null,
  "authors": ["…"],
  "narrators": ["who reads THIS recording"],
  "series_name": null,
  "series_position": "as text, e.g. \"3\" or \"1.5\" (or null)",
  "release_date": "this recording's release, YYYY-MM-DD, or just YYYY",
  "publisher": "this recording's publisher",
  "language": "english",
  "genres": ["from the list below, most fitting first"],
  "sub_genres": ["more specific, e.g. \"Dystopian\", \"Epic\""],
  "description": "the publisher's summary as plain text, paragraphs separated by blank lines",
  "asin": "only if THIS recording has an Audible ASIN, else null"
}`)
	w("\n```\n\n")
	w("Genres to choose from (Audible's categories): %s.\n", strings.Join(metadata.AudibleGenreNames(), "; "))
	return b.String()
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

// realGenres drops the placeholder genre most taggers write ("Audiobook"),
// which would otherwise survive into the review form looking like a choice.
func realGenres(gs []string) []string {
	var out []string
	for _, g := range gs {
		switch strings.ToLower(strings.TrimSpace(g)) {
		case "audiobook", "audiobooks", "audio book", "audio books", "spoken word", "hörbuch", "books & spoken":
			continue
		}
		out = append(out, g)
	}
	return out
}
