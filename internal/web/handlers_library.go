package web

import (
	"context"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"audioborker/internal/match"
	"audioborker/internal/metadata"
	"audioborker/internal/metadata/embedded"
	"audioborker/internal/pipeline"
	"audioborker/internal/scan"
	"audioborker/internal/store"
)

// searchRowLimit caps how many matches one filter shows. A NAS library can
// hold thousands of books; the cap keeps the response small and the count is
// always reported so a truncated list never reads as a complete one.
const searchRowLimit = 200

// searchWalkLimit bounds the directory walk itself, so a pathological tree
// cannot pin a request.
const searchWalkLimit = 200_000

type libraryData struct {
	baseData
	OutputDir string
	Tree      treeData
	Empty     bool
}

// handleLibrary renders the library browser: the same lazy folder tree Import
// uses, rooted at the output volume, plus a filter that searches the whole
// tree at once.
func (s *Server) handleLibrary(w http.ResponseWriter, r *http.Request) {
	set := s.settings()
	entries, err := libraryList(set.OutputDir, "")
	data := libraryData{
		baseData:  s.base("Library", "library"),
		OutputDir: set.OutputDir,
		Tree:      treeData{Entries: entries, TreeURL: "/library/tree"},
		Empty:     err == nil && len(entries) == 0,
	}
	if err != nil {
		data.Error = "Could not read the library directory (" + set.OutputDir + "): " + err.Error()
	}
	s.render.render(w, "library", data)
}

// handleLibraryTree returns one lazily-loaded level of the library tree.
func (s *Server) handleLibraryTree(w http.ResponseWriter, r *http.Request) {
	set := s.settings()
	rel := r.URL.Query().Get("path")
	entries, err := libraryList(set.OutputDir, rel)
	if err != nil {
		http.Error(w, "cannot list "+rel, http.StatusBadRequest)
		return
	}
	s.render.partial(w, "library", "tree", treeData{Entries: entries, TreeURL: "/library/tree"})
}

// libraryList lists one level, keeping only folders and .m4b files. Retagging
// rewrites atoms in an existing mp4; it cannot convert an mp3, so offering one
// would only produce a job that fails at probe time.
func libraryList(root, rel string) ([]scan.Entry, error) {
	entries, err := scan.List(root, rel)
	if err != nil {
		return nil, err
	}
	kept := entries[:0]
	for _, e := range entries {
		if e.IsDir || strings.EqualFold(filepath.Ext(e.Name), ".m4b") {
			kept = append(kept, e)
		}
	}
	return kept, nil
}

type searchData struct {
	Entries   []scan.Entry
	Query     string
	Total     int // matches found before the row cap
	Truncated bool
	Err       string
}

// handleLibrarySearch flattens the library to the files matching a query. The
// tree is lazy-loaded, so filtering has to happen server-side — the browser
// has never seen the folders it would need to filter.
func (s *Server) handleLibrarySearch(w http.ResponseWriter, r *http.Request) {
	set := s.settings()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		// An empty box restores browsing rather than listing everything.
		entries, _ := libraryList(set.OutputDir, "")
		s.render.partial(w, "library", "tree", treeData{Entries: entries, TreeURL: "/library/tree"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	entries, total, err := searchLibrary(ctx, set.OutputDir, q, searchRowLimit)
	data := searchData{Entries: entries, Query: q, Total: total, Truncated: total > len(entries)}
	if err != nil {
		data.Err = err.Error()
	}
	s.render.partial(w, "library", "library_rows", data)
}

// searchLibrary walks the library for .m4b files whose relative path contains
// every whitespace-separated term (case-insensitive). Matching the whole path
// rather than the filename is what makes "pratchett magic" find
// "Terry Pratchett/Discworld/The Colour of Magic [ASIN].m4b" — the author and
// series live in the directory names the path template created.
func searchLibrary(ctx context.Context, root, query string, limit int) ([]scan.Entry, int, error) {
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return nil, 0, nil
	}
	var out []scan.Entry
	total, visited := 0, 0

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable corners of the library shouldn't fail the search
		}
		if visited++; visited > searchWalkLimit {
			return fs.SkipAll
		}
		if visited%512 == 0 && ctx.Err() != nil {
			// The user kept typing and htmx aborted this request; stop walking
			// instead of finishing work nobody will see.
			return fs.SkipAll
		}
		if d.IsDir() {
			if p != root && scan.IsJunk(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(d.Name()), ".m4b") {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		hay := strings.ToLower(rel)
		for _, term := range terms {
			if !strings.Contains(hay, term) {
				return nil
			}
		}
		total++
		if len(out) >= limit {
			return nil
		}
		e := scan.Entry{Name: d.Name(), RelPath: rel, AudioFiles: 1}
		if info, statErr := d.Info(); statErr == nil {
			e.Size, e.ModTime = info.Size(), info.ModTime()
		}
		out = append(out, e)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return scan.NaturalLess(out[i].RelPath, out[j].RelPath) })
	return out, total, err
}

// handleLibraryMatch turns a library selection into the retag screen. Unlike
// an import, every item is one file: a selected folder contributes each m4b
// under it separately, because retagging is per-file.
func (s *Server) handleLibraryMatch(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	set := s.settings()
	files := libraryFilesFor(set.OutputDir, r.PostForm["paths"])
	if len(files) == 0 {
		http.Redirect(w, r, "/library", http.StatusSeeOther)
		return
	}

	data := matchData{
		baseData:     s.base("Retag", "library"),
		Regions:      store.Regions,
		Region:       set.RegionDefault,
		Root:         RootLibrary,
		PathTemplate: set.PathTemplate,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	for i, rel := range files {
		item := matchItem{Index: i, RelPath: rel, Files: 1}
		// Seed the search from what the file already claims. A file this app
		// produced carries its ASIN, so it re-matches in one click; a file from
		// another tool usually still carries title/author. Probing is best
		// effort — without ffprobe (or on a damaged file) the filename is
		// still a workable guess, so the Library stays usable.
		if abs, err := scan.Resolve(set.OutputDir, rel); err == nil {
			if info, perr := pipeline.ProbeFile(ctx, s.cfg.FFprobePath, abs); perr == nil {
				book := embedded.Book(info.Tags)
				item.Current = book
				item.ASIN = book.ASIN
				item.Title = book.Title
				if len(book.Authors) > 0 {
					item.Author = book.Authors[0]
				}
			}
		}
		if item.Title == "" {
			normalized := match.Normalize(strings.TrimSuffix(path.Base(rel), path.Ext(rel)))
			if normalized == "" {
				normalized = path.Base(rel)
			}
			item.Author, item.Title = match.SplitAuthorTitle(normalized)
		}
		item.Keywords = strings.TrimSpace(item.Author + " " + item.Title)
		data.Items = append(data.Items, item)
	}
	s.render.render(w, "match", data)
}

type renamePreviewData struct {
	Old, New string
	Same     bool // the template already reproduces the current path
	Exists   bool // a different file occupies the target
	Err      string
}

// handleLibraryRenamePreview shows where a retag would move a file, derived
// through the same pathtmpl call the retag itself makes, so the preview cannot
// drift from what actually happens. Per-field metadata overrides are included
// because picking a different title changes the path.
func (s *Server) handleLibraryRenamePreview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	set := s.settings()
	rel := q.Get("path")
	data := renamePreviewData{Old: rel}

	choice := q.Get("choice")
	asin, region, ok := strings.Cut(choice, "|")
	if choice != manualChoice && (!ok || asin == "") {
		s.render.partial(w, "match", "rename_preview", data) // nothing to preview yet
		return
	}
	abs, err := scan.Resolve(set.OutputDir, rel)
	if err != nil {
		data.Err = err.Error()
		s.render.partial(w, "match", "rename_preview", data)
		return
	}

	var book *metadata.Book
	if choice == manualChoice {
		var errs []string
		if book, _, errs = s.manualBook(manualFields(q.Get, rel)); len(errs) > 0 {
			s.render.partial(w, "match", "rename_preview", data) // not a whole book yet
			return
		}
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		res, err := s.aggregator().GetBook(ctx, asin, region, metadataOverridesFor(q.Get))
		if err != nil {
			data.Err = "Lookup failed: " + err.Error()
			s.render.partial(w, "match", "rename_preview", data)
			return
		}
		book = res.Book
	}

	opts := store.JobOptions{OutputDir: set.OutputDir, PathTemplate: set.PathTemplate, Rename: true}
	target, renamed, err := pipeline.RetagTarget(abs, opts, *book)
	switch {
	case err != nil:
		data.Err = err.Error()
		if strings.Contains(err.Error(), "already there") {
			data.Exists = true
		}
	case !renamed:
		data.Same = true
	default:
		if newRel, relErr := filepath.Rel(set.OutputDir, target); relErr == nil {
			data.New = filepath.ToSlash(newRel)
		} else {
			data.New = target
		}
	}
	s.render.partial(w, "match", "rename_preview", data)
}

// libraryFilesFor expands a checkbox selection into individual .m4b paths,
// deduped and in a stable order.
func libraryFilesFor(root string, paths []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range scan.DedupeSelection(paths) {
		abs, err := scan.Resolve(root, p)
		if err != nil {
			continue
		}
		info, err := os.Stat(abs)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			if strings.EqualFold(filepath.Ext(abs), ".m4b") && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
			continue
		}
		// A folder stands for every book inside it.
		filepath.WalkDir(abs, func(fp string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				if err == nil && d.IsDir() && fp != abs && scan.IsJunk(d.Name()) {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.EqualFold(filepath.Ext(d.Name()), ".m4b") {
				return nil
			}
			rel, relErr := filepath.Rel(root, fp)
			if relErr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if !seen[rel] {
				seen[rel] = true
				out = append(out, rel)
			}
			return nil
		})
	}
	sort.Slice(out, func(i, j int) bool { return scan.NaturalLess(out[i], out[j]) })
	return out
}
