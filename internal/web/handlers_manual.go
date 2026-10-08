package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"audioborker/internal/match"
	"audioborker/internal/metadata"
	"audioborker/internal/metadata/embedded"
	"audioborker/internal/pipeline"
	"audioborker/internal/scan"
)

// manualChoice is the match:{path} value of an item tagged by hand. Its
// metadata travels in the match form as manual:{field}:{path} inputs, so the
// queue builds the job's snapshot from what was reviewed, like every other
// per-item control on the match screen.
const manualChoice = "manual"

// manualPanelData is the Tag by hand panel inside an item's match dialog.
type manualPanelData struct {
	Index      int
	Root       string
	Path       string
	Files      []string // base names
	RuntimeMin int
	LocalID    string
	Form       manualForm
	Prompt     string
	Errors     []string
	Notes      []string

	// Cover choices. FileCover is a data: URL of the art the source already
	// carries ("" hides that option); Editions are the closest catalog
	// editions, offered for their covers only.
	FileCover template.URL
	Editions  []metadata.SearchResult
	Cover     string // "file", "upload" or "edition:<cover URL>"
}

// handleMatchManual renders the panel, the first time an item's match dialog
// switches to Tag by hand. The fields start from the file's own tags.
func (s *Server) handleMatchManual(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d := s.manualPanel(r.Context(), q.Get("root"), q.Get("path"))
	d.Index = atoiOr(q.Get("index"), 0)
	s.render.partial(w, "match", "manual_panel", d)
}

func (s *Server) manualPanel(ctx context.Context, rootToken, rel string) manualPanelData {
	d := manualPanelData{Root: rootToken, Path: rel}
	root := s.rootDir(func(string) string { return rootToken })
	files, err := scan.CollectAudioFiles(root, rel)
	if err != nil || len(files) == 0 {
		d.Errors = append(d.Errors, "Can't read the book's files: "+errText(err))
		return d
	}
	for _, f := range files {
		d.Files = append(d.Files, filepath.Base(f))
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	d.RuntimeMin = s.LocalRuntimeMin(ctx, root, rel)

	var current *metadata.Book
	in := promptInput{Path: rel, Files: d.Files, RuntimeMin: d.RuntimeMin}
	if info, err := pipeline.ProbeSource(ctx, s.cfg.FFprobePath, files[0]); err == nil {
		tags := info.Tags
		if len(files) > 1 {
			// In a multi-file book each file's title is a part ("Part 1");
			// the album tag is what names the book.
			tags = withoutKeys(tags, "title")
		}
		current = embedded.Book(tags)
		in.Current = current
		d.LocalID = embedded.ManualID(info.Tags)
		in.ChapterCount = len(info.Chapters)
		for i, ch := range info.Chapters {
			if i == 5 {
				break
			}
			in.Chapters = append(in.Chapters, ch.Title)
		}
	}
	if d.LocalID == "" {
		// Re-tagging a hand-tagged book keeps its ID (above), so the Plex
		// agent goes on matching it to the same library item.
		d.LocalID = uuid.NewString()
	}
	if art := pipeline.CoverImage(ctx, s.cfg.FFmpegPath, files[0]); len(art) > 0 && len(art) < 8<<20 {
		d.FileCover = template.URL("data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(art))
	}
	d.Editions, d.Notes = s.nearestEditions(ctx, rel, current, d.RuntimeMin)
	in.Editions = d.Editions

	d.Prompt = manualPrompt(in)
	d.Form = formFromBook(current)
	d.Form.Genres = strings.Join(realGenres(splitField(d.Form.Genres)), listSep)
	switch {
	case d.FileCover != "":
		d.Cover = "file"
	case len(d.Editions) > 0 && d.Editions[0].CoverURL != "":
		d.Cover = "edition:" + d.Editions[0].CoverURL
	default:
		d.Cover = "upload"
	}
	return d
}

// nearestEditions searches the catalog the way the match screen does and
// returns the closest few — not to match (this page exists because none
// fits) but for the LLM's context and as cover choices.
func (s *Server) nearestEditions(ctx context.Context, rel string, current *metadata.Book, runtimeMin int) ([]metadata.SearchResult, []string) {
	title, author := "", ""
	if current != nil {
		title = current.Title
		if len(current.Authors) > 0 {
			author = current.Authors[0]
		}
	}
	if title == "" {
		normalized := match.Normalize(path.Base(rel))
		author, title = match.SplitAuthorTitle(normalized)
	}
	if title == "" {
		return nil, nil
	}
	region := s.settings().RegionDefault
	results, err := s.provider().Search(ctx, metadata.SearchQuery{Keywords: title, Author: author, Region: region})
	if err == nil && len(results) == 0 && author != "" {
		// Taggers often put the narrator in the artist tag (Absolution Gap
		// came as "John Lee"), and an author-scoped search then finds nothing.
		results, err = s.provider().Search(ctx, metadata.SearchQuery{Keywords: title, Region: region})
	}
	if err != nil {
		return nil, []string{"Catalog search failed: " + err.Error()}
	}
	results = match.Score(results, match.Signals{Title: title, Author: author, RuntimeMin: runtimeMin,
		Language: match.LanguageForRegion(region)})
	if len(results) > 6 {
		results = results[:6]
	}
	return results, nil
}

// handleManualParse turns a pasted LLM reply into form values (JSON), so
// the page can fill the fields without touching the cover choice.
func (s *Server) handleManualParse(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad form"})
		return
	}
	f, warnings, err := parseManualReply(r.PostForm.Get("reply"))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"error": capitalize(err.Error())})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fields": formFields(f), "warnings": warnings})
}

// formFields is the form as input name → value, the names the page uses.
func formFields(f manualForm) map[string]string {
	return map[string]string{
		"title": f.Title, "subtitle": f.Subtitle, "authors": f.Authors, "narrators": f.Narrators,
		"series_name": f.SeriesName, "series_position": f.SeriesPosition, "release": f.Release,
		"publisher": f.Publisher, "language": f.Language, "genres": f.Genres,
		"description": f.Description, "asin": f.ASIN,
	}
}

func formFromRequest(get func(string) string) manualForm {
	return manualForm{
		Title: get("title"), Subtitle: get("subtitle"), Authors: get("authors"), Narrators: get("narrators"),
		SeriesName: get("series_name"), SeriesPosition: get("series_position"), Release: get("release"),
		Publisher: get("publisher"), Language: get("language"), Genres: get("genres"),
		Description: get("description"), ASIN: get("asin"),
	}
}

// --- covers -----------------------------------------------------------------

const maxCoverBytes = 20 << 20

// coverNameRe is the only shape of name the covers dir ever holds or serves.
var coverNameRe = regexp.MustCompile(`^[0-9a-f]{32}\.(jpg|png)$`)

// handleManualCover stores a cover the user dropped, pasted or picked: an
// image file (multipart "image"), or the URL of one ("url") — what a browser
// hands over when an image is dragged in from another window.
func (s *Server) handleManualCover(w http.ResponseWriter, r *http.Request) {
	var data []byte
	var err error
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		if err = r.ParseMultipartForm(maxCoverBytes); err == nil {
			var f io.ReadCloser
			if f, _, err = r.FormFile("image"); err == nil {
				data, err = io.ReadAll(io.LimitReader(f, maxCoverBytes+1))
				f.Close()
			}
		}
	} else if err = r.ParseForm(); err == nil {
		data, err = s.fetchCover(r.Context(), r.PostForm.Get("url"))
	}
	if err == nil && len(data) > maxCoverBytes {
		err = errors.New("image over 20 MB")
	}
	var name string
	if err == nil {
		name, err = s.saveCover(data)
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"error": capitalize(err.Error())})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "url": "/manual/covers/" + name})
}

func (s *Server) fetchCover(ctx context.Context, raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return nil, errors.New("not an image or image link")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (audioborker cover fetch)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxCoverBytes+1))
}

// saveCover stores an image under the covers dir by a random name, after
// checking it is a JPEG or PNG: what tone embeds and every player shows.
func (s *Server) saveCover(data []byte) (string, error) {
	ext := ""
	switch ct := http.DetectContentType(data); ct {
	case "image/jpeg":
		ext = ".jpg"
	case "image/png":
		ext = ".png"
	case "image/webp", "image/gif", "image/bmp":
		return "", fmt.Errorf("%s isn't supported, use JPEG or PNG", strings.ToUpper(strings.TrimPrefix(ct, "image/")))
	default:
		return "", errors.New("not a JPEG or PNG image")
	}
	dir := s.cfg.CoversDir()
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	name := hex.EncodeToString(id) + ext
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o666); err != nil {
		return "", err
	}
	s.pruneCovers(24 * time.Hour)
	return name, nil
}

// handleManualCoverFile serves an uploaded cover for the page's preview.
func (s *Server) handleManualCoverFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !coverNameRe.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.cfg.CoversDir(), name))
}

// pruneCovers removes uploaded covers no job references, once they are
// older than minAge — younger ones may belong to a page still being filled in.
func (s *Server) pruneCovers(minAge time.Duration) {
	used, err := s.store.CoverFilesInUse()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(s.cfg.CoversDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		p := filepath.Join(s.cfg.CoversDir(), e.Name())
		info, err := e.Info()
		if err != nil || !coverNameRe.MatchString(e.Name()) || used[p] || time.Since(info.ModTime()) < minAge {
			continue
		}
		os.Remove(p)
	}
}

// --- assigning ---------------------------------------------------------------

// manualFields reads one item's Tag by hand inputs, manual:{field}:{path}:
// the match form posts them that way, and the per-item requests the page
// makes send them under the same names.
func manualFields(get func(string) string, rel string) func(string) string {
	return func(field string) string { return get("manual:" + field + ":" + rel) }
}

// manualBook builds a hand-tagged item's metadata snapshot and the cover it
// keeps (a file under the covers dir, or "" for the book's URL or the art the
// source already carries). The row summary, the rename preview and the queue
// all read the fields through here, so what was reviewed is what gets queued.
func (s *Server) manualBook(get func(string) string) (*metadata.Book, string, []string) {
	localID := get("local_id")
	if _, err := uuid.Parse(localID); err != nil {
		localID = uuid.NewString()
	}
	book, errs := formFromRequest(get).book(localID)
	coverFile := ""
	switch choice := get("cover"); {
	case strings.HasPrefix(choice, "edition:"):
		if u := strings.TrimPrefix(choice, "edition:"); strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
			book.CoverURL = u
		} else {
			errs = append(errs, "Invalid cover choice.")
		}
	case choice == "upload":
		name := get("cover_name")
		if !coverNameRe.MatchString(name) {
			errs = append(errs, "No cover image uploaded.")
		} else {
			coverFile = filepath.Join(s.cfg.CoversDir(), name)
		}
	default:
		// "file": no URL, so the pipeline keeps the art the source carries.
	}
	return book, coverFile, errs
}

// handleMatchManualCheck validates an item's reviewed fields when the user
// assigns them, and renders the row summary for the match screen.
func (s *Server) handleMatchManualCheck(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"errors": []string{"Bad form."}})
		return
	}
	get := r.PostForm.Get
	rel := get("path")
	book, _, errs := s.manualBook(manualFields(get, rel))
	if len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"errors": errs})
		return
	}
	data := rowSummaryData{Path: rel, Book: book, Manual: true, AuthorLine: strings.Join(book.Authors, ", ")}
	if files, err := scan.CollectAudioFiles(s.rootDir(get), rel); err == nil {
		data.Files = len(files)
	}
	data.SeriesLine = seriesLine(book)
	s.render.partial(w, "match", "row_summary", data)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func errText(err error) string {
	if err == nil {
		return "no audio files found"
	}
	return err.Error()
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// withoutKeys copies a tag map minus the given keys (any spelling case).
func withoutKeys(tags map[string]string, keys ...string) map[string]string {
	out := make(map[string]string, len(tags))
	for k, v := range tags {
		drop := false
		for _, d := range keys {
			if strings.EqualFold(k, d) {
				drop = true
			}
		}
		if !drop {
			out[k] = v
		}
	}
	return out
}
