package web

import (
	"context"
	"net/http"
	"path"
	"time"

	"audioborker/internal/match"
	"audioborker/internal/metadata"
	"audioborker/internal/metadata/audnexus"
	"audioborker/internal/pipeline"
	"audioborker/internal/scan"
	"audioborker/internal/store"
)

type matchItem struct {
	Index    int
	RelPath  string
	Files    int
	Err      string
	Keywords string // pre-computed search guess from the folder name
	Title    string
	Author   string

	// Library retags only: what the file already claims, and the ASIN found in
	// its own atoms (empty when it carries none). Current is nil when the file
	// could not be probed.
	Current *metadata.Book
	ASIN    string

	// HasSidecar offers the chapters.txt next to a single file as a source.
	HasSidecar bool
}

type matchData struct {
	baseData
	Items          []matchItem
	Regions        []string
	Region         string
	CleanupDefault string
	// Root is "" for the import volume or "library" when retagging files
	// already in the library. It travels with every request the page makes so
	// each one resolves paths against the same directory.
	Root string
	// PathTemplate is shown next to the rename toggle (library only), which
	// starts ticked when RenameDefault is set.
	PathTemplate  string
	RenameDefault bool
}

// IsLibrary reports whether this is a retag batch, for template branching.
func (d matchData) IsLibrary() bool { return d.Root == RootLibrary }

// handleMatch receives the import selection and renders the match screen;
// each item then auto-loads its candidates via htmx.
func (s *Server) handleMatch(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	paths := scan.DedupeSelection(r.PostForm["paths"])
	if len(paths) == 0 {
		http.Redirect(w, r, "/import", http.StatusSeeOther)
		return
	}
	set := s.settings()
	data := matchData{
		baseData:       s.base("Match", "import"),
		Regions:        store.Regions,
		Region:         set.RegionDefault,
		CleanupDefault: set.CleanupMode,
		PathTemplate:   set.PathTemplate,
	}
	for i, p := range paths {
		item := matchItem{Index: i, RelPath: p}
		files, err := scan.CollectAudioFiles(set.InputDir, p)
		if err != nil {
			item.Err = err.Error()
		} else {
			item.Files = len(files)
			item.HasSidecar = len(files) == 1 && pipeline.HasSidecar(files[0])
		}
		normalized := match.Normalize(path.Base(p))
		if normalized == "" {
			normalized = path.Base(p) // all-digits names like "1984"
		}
		item.Keywords = normalized
		item.Author, item.Title = match.SplitAuthorTitle(normalized)
		data.Items = append(data.Items, item)
	}
	s.render.render(w, "match", data)
}

type candidatesData struct {
	RelPath string
	Region  string
	Results []metadata.SearchResult
	Err     string

	// LocalRuntimeMin is the measured length of the selected audio (0 when
	// unknown); AutoSelect marks the top result as safe to pre-tick.
	LocalRuntimeMin int
	AutoSelect      bool
}

// handleMatchCandidates searches the catalog and renders candidate cards.
func (s *Server) handleMatchCandidates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	set := s.settings()
	region := q.Get("region")
	if region == "" {
		region = set.RegionDefault
	}
	data := candidatesData{RelPath: q.Get("path"), Region: region}

	query := metadata.SearchQuery{
		Keywords: q.Get("q"),
		Title:    q.Get("title"),
		Author:   q.Get("author"),
		Region:   region,
	}
	if query.Keywords == "" && query.Title == "" && query.Author == "" {
		data.Err = "Enter search terms."
		s.render.partial(w, "match", "candidates", data)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// Measure the actual audio so runtime can inform the ranking. Unknown
	// (0) simply falls back to name-only matching.
	data.LocalRuntimeMin = s.LocalRuntimeMin(ctx, s.rootDir(q.Get), data.RelPath)

	results, err := s.provider().Search(ctx, query)
	if err != nil {
		data.Err = "Search failed: " + err.Error()
	} else {
		titleHint := query.Title
		if titleHint == "" {
			titleHint = query.Keywords
		}
		signals := match.Signals{
			Title:      titleHint,
			Author:     query.Author,
			RuntimeMin: data.LocalRuntimeMin,
			Language:   match.LanguageForRegion(region),
		}
		results = match.Score(results, signals)
		data.AutoSelect = match.AutoSelect(results, signals)
		if len(results) > 8 {
			results = results[:8]
		}
		data.Results = results
	}
	s.render.partial(w, "match", "candidates", data)
}

type bookCardData struct {
	RelPath string
	Book    *metadata.Book
	Err     string
}

// handleMatchLookup validates a manually entered ASIN against the provider
// and renders a confirmed book card.
func (s *Server) handleMatchLookup(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	set := s.settings()
	region := q.Get("region")
	if region == "" {
		region = set.RegionDefault
	}
	asin := q.Get("asin")
	data := bookCardData{RelPath: q.Get("path")}

	if !audnexus.ValidASIN(asin) {
		data.Err = "ASIN must be 10 letters/digits."
		s.render.partial(w, "match", "book_card", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	// The aggregator also resolves ASINs that Audnexus doesn't know but the
	// Audible catalog does — common for fresh releases.
	res, err := s.aggregator().GetBook(ctx, asin, region, nil)
	if err != nil {
		if metadata.IsNotFound(err) {
			data.Err = "No book with ASIN " + asin + " in region " + region + "."
		} else {
			data.Err = "Lookup failed: " + err.Error()
		}
	} else {
		data.Book = res.Book
	}
	s.render.partial(w, "match", "book_card", data)
}
