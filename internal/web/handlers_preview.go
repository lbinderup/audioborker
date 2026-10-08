package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"audioborker/internal/match"
	"audioborker/internal/metadata"
	"audioborker/internal/metadata/aggregate"
	"audioborker/internal/mp4fix"
	"audioborker/internal/pipeline"
	"audioborker/internal/scan"
)

// audioContentTypes maps extensions the browser can typically play natively.
// wma is deliberately absent — no browser decodes it.
var audioContentTypes = map[string]string{
	".m4b": "audio/mp4", ".m4a": "audio/mp4", ".aac": "audio/aac",
	".mp3": "audio/mpeg", ".flac": "audio/flac",
	".ogg": "audio/ogg", ".opus": "audio/ogg", ".wav": "audio/wav",
}

// handlePreviewAudio streams one input file. http.ServeContent handles HTTP
// Range requests, so browser <audio> seeking works — including remotely
// through a reverse proxy.
func (s *Server) handlePreviewAudio(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	abs, err := scan.Resolve(s.rootDir(q.Get), q.Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() || !scan.IsAudioFile(abs) {
		http.NotFound(w, r)
		return
	}
	ct, ok := audioContentTypes[strings.ToLower(filepath.Ext(abs))]
	if !ok {
		http.Error(w, "this format cannot be played in a browser", http.StatusUnsupportedMediaType)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		http.Error(w, "cannot open file", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", ct)
	var content io.ReadSeeker = f
	modTime := info.ModTime()
	if fix := s.previewFixes.get(abs, f, info); fix != nil {
		content = fix.Reader(f)
		// These aren't the bytes on disk, so they get their own validator:
		// against the file's mtime, a browser that cached the stored headers
		// revalidated into a 304 and went on playing 2:29 of a 27-hour book.
		modTime = time.Time{}
		w.Header().Set("ETag", fmt.Sprintf(`"mp4fix1-%d-%d"`, info.Size(), info.ModTime().UnixNano()))
	}
	http.ServeContent(w, r, filepath.Base(abs), modTime, content)
}

// previewFixes remembers which MP4s need their overflowed length headers
// corrected for the browser (see mp4fix), so the player's many Range
// requests inspect a file once. A fix holds the rebuilt moov — about 17 MB
// for a 27-hour book — so the cache stays small.
type previewFixes struct {
	mu sync.Mutex
	m  map[string]*mp4fix.Fix // nil value: checked, nothing to fix
}

func (c *previewFixes) get(abs string, f *os.File, info os.FileInfo) *mp4fix.Fix {
	switch strings.ToLower(filepath.Ext(abs)) {
	case ".m4b", ".m4a", ".mp4":
	default:
		return nil
	}
	key := fmt.Sprintf("%s|%d|%d", abs, info.Size(), info.ModTime().UnixNano())
	c.mu.Lock()
	fix, ok := c.m[key]
	c.mu.Unlock()
	if ok {
		return fix
	}
	fix, err := mp4fix.Inspect(f, info.Size())
	if err != nil {
		slog.Warn("preview: can't inspect mp4 headers", "file", abs, "err", err)
		fix = nil
	}
	c.mu.Lock()
	if c.m == nil || len(c.m) >= 8 {
		c.m = map[string]*mp4fix.Fix{}
	}
	c.m[key] = fix
	c.mu.Unlock()
	return fix
}

type previewChapter struct {
	Title   string
	StartMs int64
	Stamp   string // HH:MM:SS
}

// previewFile is one source file in a multi-file selection: its start offset
// in the merged book is where a boundary chapter would begin.
type previewFile struct {
	Index     int
	Name      string
	Rel       string // input-relative, for the streaming URL
	StartMs   int64
	Stamp     string
	Duration  string
	EndSeekMs int64 // a few seconds before the file ends
	Playable  bool
}

type previewData struct {
	RelPath    string
	Root       string // "" = import volume, "library" = output volume
	Multi      bool
	Files      []previewFile
	FilesJSON  string // [{"rel","start","end"}] for the seek-across-files JS
	Playable   bool   // first file is browser-playable
	DurationMs int64
	Duration   string
	Chapters   []previewChapter // single-file only: the file's own chapters
	// ChaptersFrom names the chapters.txt they came from ("" = embedded).
	ChaptersFrom string
	Err          string
}

// handleMatchPreview renders the chapter-preview panel for one match item:
// an audio player plus seekable chapter/file lists. Single-file selections
// list the file's embedded chapters; multi-file selections list each file's
// start (the would-be boundary chapters), so the user can hear whether files
// begin at natural breaks or were split arbitrarily mid-sentence.
func (s *Server) handleMatchPreview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	root := s.rootDir(q.Get)
	rel := q.Get("path")
	data := previewData{RelPath: rel, Root: q.Get("root")}

	fail := func(msg string) {
		data.Err = msg
		s.render.partial(w, "match", "chapter_preview", data)
	}

	files, err := scan.CollectAudioFiles(root, rel)
	if err != nil {
		fail(err.Error())
		return
	}
	if len(files) > 200 {
		fail(fmt.Sprintf("This selection has %d files — too many to preview individually.", len(files)))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	type boundary struct {
		Rel   string `json:"rel"`
		Start int64  `json:"start"`
		End   int64  `json:"end"`
	}
	inputRoot := filepath.Clean(root)
	var bounds []boundary
	var offset int64
	for i, f := range files {
		info, err := pipeline.ProbeSource(ctx, s.cfg.FFprobePath, f)
		if err != nil {
			fail("ffprobe failed: " + err.Error())
			return
		}
		fileRel, err := filepath.Rel(inputRoot, f)
		if err != nil {
			fail("cannot resolve file path")
			return
		}
		frel := filepath.ToSlash(fileRel)
		_, playable := audioContentTypes[strings.ToLower(filepath.Ext(f))]

		endSeek := offset + info.DurationMs - 6000
		if endSeek < offset {
			endSeek = offset
		}
		data.Files = append(data.Files, previewFile{
			Index:     i + 1,
			Name:      filepath.Base(f),
			Rel:       frel,
			StartMs:   offset,
			Stamp:     msClock(offset),
			Duration:  msClock(info.DurationMs),
			EndSeekMs: endSeek,
			Playable:  playable,
		})
		bounds = append(bounds, boundary{Rel: frel, Start: offset, End: offset + info.DurationMs})

		if len(files) == 1 {
			data.ChaptersFrom = info.ChaptersFrom
			for _, c := range info.Chapters {
				data.Chapters = append(data.Chapters, previewChapter{
					Title: c.Title, StartMs: c.StartMs, Stamp: msClock(c.StartMs),
				})
			}
		}
		offset += info.DurationMs
	}

	data.Multi = len(files) > 1
	data.DurationMs = offset
	data.Duration = msClock(offset)
	data.Playable = data.Files[0].Playable
	if data.Multi {
		raw, _ := json.Marshal(bounds)
		data.FilesJSON = string(raw)
	}
	s.render.partial(w, "match", "chapter_preview", data)
}

type providerChaptersData struct {
	Path      string // input-relative selection path, echoed for the retry button
	Chapters  []previewChapter
	RuntimeMs int64
	Runtime   string
	Accurate  bool
	Err       string
}

// handleMatchProviderChapters fetches the selected candidate's chapter
// timings so the user can seek the local audio to them for comparison.
func (s *Server) handleMatchProviderChapters(w http.ResponseWriter, r *http.Request) {
	data := providerChaptersData{Path: r.URL.Query().Get("path")}
	asin, region, ok := s.choiceASIN(r.URL.Query().Get)
	if !ok {
		data.Err = "Select a match first."
		if r.URL.Query().Get("choice") == manualChoice {
			data.Err = "No ASIN, so no Audible chapters."
		}
		s.render.partial(w, "match", "provider_chapters", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	info, err := s.provider().GetChapters(ctx, asin, region)
	if err != nil {
		if metadata.IsNotFound(err) {
			data.Err = "No chapter data available for " + asin + " (" + region + ")."
		} else {
			data.Err = "Chapter lookup failed: " + err.Error()
		}
		s.render.partial(w, "match", "provider_chapters", data)
		return
	}
	s.store.CacheChapters(aggregate.SourceAudnexus, asin, region, info)
	data.RuntimeMs = info.RuntimeMs
	data.Runtime = msClock(info.RuntimeMs)
	data.Accurate = info.IsAccurate
	for _, c := range info.Chapters {
		data.Chapters = append(data.Chapters, previewChapter{
			Title: c.Title, StartMs: c.StartMs, Stamp: msClock(c.StartMs),
		})
	}
	s.render.partial(w, "match", "provider_chapters", data)
}

type rowSummaryData struct {
	Path          string
	Files         int
	Book          *metadata.Book
	AuthorLine    string
	SeriesLine    string
	OfficialClock string
	LocalClock    string
	BadgeClass    string
	BadgeText     string
	Notes         []string // aggregation degradation notes (secondary source down)
	Err           string
	Manual        bool // tagged by hand: no catalog runtime to compare
}

// handleMatchRowSummary renders the compact row description of the assigned
// match: title, author · ASIN, series · file count, and the official-vs-local
// runtime comparison.
func (s *Server) handleMatchRowSummary(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	root := s.rootDir(q.Get)
	data := rowSummaryData{Path: q.Get("path")}

	if files, err := scan.CollectAudioFiles(root, data.Path); err == nil {
		data.Files = len(files)
	}

	asin, region, ok := strings.Cut(q.Get("choice"), "|")
	if !ok || asin == "" {
		data.Err = "No match assigned"
		s.render.partial(w, "match", "row_summary", data)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	res, err := s.aggregator().GetBook(ctx, asin, region, nil)
	if err != nil {
		data.Err = "Lookup failed: " + err.Error()
		s.render.partial(w, "match", "row_summary", data)
		return
	}
	book := res.Book
	data.Book = book
	data.Notes = res.Notes
	data.AuthorLine = strings.Join(book.Authors, ", ")
	data.SeriesLine = seriesLine(book)

	// Official runtime: the chapter data carries millisecond precision; the
	// book record only minutes. Chapters may be cached already (the verdict
	// endpoint fetches them); don't force a fetch just for this line.
	officialMs := int64(book.RuntimeMin) * 60_000
	if ch, _ := s.store.CachedChapters(aggregate.SourceAudnexus, asin, region); ch != nil && ch.RuntimeMs > 0 {
		officialMs = ch.RuntimeMs
	}
	localMs := s.LocalRuntimeMs(ctx, root, data.Path)
	if officialMs > 0 && localMs > 0 {
		data.OfficialClock = msClock(officialMs)
		data.LocalClock = msClock(localMs)
		delta, class := match.RuntimeBadge(int((officialMs+30_000)/60_000), int((localMs+30_000)/60_000))
		data.BadgeClass = class
		switch class {
		case "exact":
			data.BadgeText = "runtime matches your audio"
		case "off":
			data.BadgeText = deltaMinutes(delta) + " — likely a different edition"
		default:
			data.BadgeText = deltaMinutes(delta) + " vs your audio"
		}
	}
	s.render.partial(w, "match", "row_summary", data)
}

func seriesLine(book *metadata.Book) string {
	if book.SeriesName == "" {
		return ""
	}
	if book.SeriesPosition != "" {
		return book.SeriesName + ", Book " + book.SeriesPosition
	}
	return book.SeriesName
}

// choiceASIN resolves an item's match choice to the catalog record whose
// chapters apply. A catalog pick names it as "ASIN|region"; a book tagged by
// hand has one only when its form names an ASIN, which its job looks up in
// the default region — so the verdict asks the same question the job will.
func (s *Server) choiceASIN(get func(string) string) (asin, region string, ok bool) {
	choice := get("choice")
	if choice == manualChoice {
		asin = strings.ToUpper(strings.TrimSpace(manualFields(get, get("path"))("asin")))
		if !metadata.ValidASIN(asin) {
			return "", "", false
		}
		return asin, s.settings().RegionDefault, true
	}
	asin, region, ok = strings.Cut(choice, "|")
	return asin, region, ok && asin != ""
}

type chapterPlanData struct {
	Source   string // pipeline.Source* value; also the verdict's CSS class suffix
	Icon     string
	Verdict  string
	Reason   string
	Shift    string // "" when no shift applies to the chosen chapters
	Warnings []string
	Err      string

	// The two sources the row's chips pick between: the file's own chapters
	// (or, for several files, one per file) and Audible's.
	Multi         bool
	FileCount     int
	FileFrom      string // the chapters.txt the file's chapters come from, if any
	ProviderCount int
	Overflow      string // set when a file's length header overflowed
}

// FileUsed and ProviderUsed light the chip of the list being embedded; a
// title mix uses both, so neither is lit and the verdict says which.
func (d chapterPlanData) FileUsed() bool {
	return d.Source == pipeline.SourceExisting || d.Source == pipeline.SourceFiles
}
func (d chapterPlanData) ProviderUsed() bool { return d.Source == pipeline.SourceProvider }

// overflowNote reports files whose 32-bit length header overflowed (see
// pipeline.unwrapDuration), so a batch hit by it stands out on the match
// screen. Players read those headers, so such a file shows a few minutes.
func overflowNote(infos []*pipeline.FileInfo) string {
	var hit []*pipeline.FileInfo
	for _, info := range infos {
		if info.HeaderMs > 0 {
			hit = append(hit, info)
		}
	}
	switch {
	case len(hit) == 0:
		return ""
	case len(infos) == 1:
		return "Length header overflowed: the file claims " + msClock(hit[0].HeaderMs) + " (fixed in the output)."
	}
	return fmt.Sprintf("Length headers overflowed in %d of %d files (fixed in the output).", len(hit), len(infos))
}

// handleMatchChapterPlan runs the pipeline's actual chapter decision against
// the selected match and reports the verdict, so "auto" is never opaque.
func (s *Server) handleMatchChapterPlan(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mode := q.Get("mode")
	if mode == "" {
		mode = pipeline.ChapterModeAuto
	}
	data := chapterPlanData{}

	files, err := scan.CollectAudioFiles(s.rootDir(q.Get), q.Get("path"))
	if err != nil {
		data.Err = err.Error()
		s.render.partial(w, "match", "chapter_plan", data)
		return
	}
	if len(files) > 300 {
		data.Err = "Too many files to preview; decided during conversion."
		s.render.partial(w, "match", "chapter_plan", data)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	infos := make([]*pipeline.FileInfo, 0, len(files))
	var totalMs int64
	for _, f := range files {
		info, err := pipeline.ProbeSource(ctx, s.cfg.FFprobePath, f)
		if err != nil {
			data.Err = "ffprobe failed: " + err.Error()
			s.render.partial(w, "match", "chapter_plan", data)
			return
		}
		infos = append(infos, info)
		totalMs += info.DurationMs
	}
	data.Overflow = overflowNote(infos)
	data.Multi = len(infos) > 1
	if data.Multi {
		data.FileCount = len(infos)
	} else if len(infos) == 1 {
		data.FileCount = len(infos[0].Chapters)
		data.FileFrom = infos[0].ChaptersFrom
		if e := infos[0].SidecarErr; e != "" {
			data.Warnings = append(data.Warnings, e)
		}
	}

	var provider *metadata.ChapterInfo
	if asin, region, ok := s.choiceASIN(q.Get); ok {
		if provider, _ = s.store.CachedChapters(aggregate.SourceAudnexus, asin, region); provider == nil {
			if ch, err := s.provider().GetChapters(ctx, asin, region); err == nil {
				provider = ch
				s.store.CacheChapters(aggregate.SourceAudnexus, asin, region, ch)
			} else if !metadata.IsNotFound(err) {
				data.Warnings = append(data.Warnings, "Chapter lookup failed ("+err.Error()+") — decision shown without provider data.")
			}
		}
	}
	if provider != nil {
		data.ProviderCount = len(provider.Chapters)
	}
	// The same shift the conversion will apply, to whichever list it picks.
	plan := pipeline.PlanChapters(mode, provider, infos, totalMs, "", shiftSpecFrom(q.Get))
	data.Source = plan.Source
	data.Warnings = append(data.Warnings, plan.Warnings...)
	if !plan.Shift.IsZero() {
		data.Shift = "Shifted " + plan.Shift.String() + "."
	}

	n := len(plan.Chapters)
	switch plan.Source {
	case pipeline.SourceProvider:
		// No reason line: the row summary right above already carries the
		// official-vs-local runtime comparison.
		data.Icon, data.Verdict = "🌐", fmt.Sprintf("Will embed Audible's %d chapters.", n)
	case pipeline.SourceExisting:
		data.Icon, data.Verdict = "📼", fmt.Sprintf("Will keep the file's own %d chapters.", n)
		if data.FileFrom != "" {
			data.Icon, data.Verdict = "📄", fmt.Sprintf("Will embed the %d chapters from %s.", n, data.FileFrom)
		}
		if mode == pipeline.ChapterModeExisting {
			data.Reason = "You chose to keep them."
		} else if provider == nil {
			data.Reason = "No usable Audible chapter data" + choiceHint(q.Get("choice")) + "."
		}
	case pipeline.SourceFiles:
		data.Icon, data.Verdict = "🧩", fmt.Sprintf("Will generate %d chapters from the file boundaries.", n)
		if provider == nil && mode != pipeline.ChapterModeExisting {
			data.Reason = "No usable Audible chapter data" + choiceHint(q.Get("choice")) + "."
		}
	case pipeline.SourceSingle:
		data.Icon, data.Verdict = "▭", "Will embed one whole-book chapter."
		data.Reason = "No chapter data from Audible or the file."
	case pipeline.SourceTitlesFiles:
		data.Icon, data.Verdict = "🔀", fmt.Sprintf("Will embed %d chapters: Audible's titles on your file boundaries.", n)
	case pipeline.SourceTitlesExisting:
		data.Icon, data.Verdict = "🔀", fmt.Sprintf("Will embed %d chapters: Audible's titles on the file's own timings.", n)
	}
	s.render.partial(w, "match", "chapter_plan", data)
}

func choiceHint(choice string) string {
	if choice == "" {
		return " (no match selected yet)"
	}
	return ""
}

// shiftSpecFrom builds a ShiftSpec from request values (query or form). The
// get function abstracts the key prefix, so both "shift_mode" and
// "shift_mode:Some Book" callers share this.
func shiftSpecFrom(get func(string) string) metadata.ShiftSpec {
	switch get("shift_mode") {
	case "interp":
		return metadata.ShiftSpec{
			Mode:    "interp",
			FromIdx: atoiOr(get("shift_from_idx"), 0),
			FromMs:  parseShiftMs(get("shift_from_ms")),
			ToIdx:   atoiOr(get("shift_to_idx"), 0),
			ToMs:    parseShiftMs(get("shift_to_ms")),
		}
	default: // "", "fixed"
		if ms := parseShiftMs(get("shift")); ms != 0 {
			return metadata.ShiftSpec{Mode: "fixed", FixedMs: ms}
		}
		return metadata.ShiftSpec{}
	}
}

func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

// parseShiftMs reads a user-supplied chapter offset, ignoring junk and
// capping at ±1 hour — a larger offset means the wrong edition, not a rip
// that drifted.
func parseShiftMs(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	const limit = 3_600_000
	if n > limit {
		return limit
	}
	if n < -limit {
		return -limit
	}
	return n
}

func msClock(ms int64) string {
	h := ms / 3_600_000
	m := ms % 3_600_000 / 60_000
	sec := ms % 60_000 / 1000
	if h > 0 {
		return fmtClock(h, m, sec)
	}
	return fmtClock(0, m, sec)[3:]
}

func fmtClock(h, m, s int64) string {
	const digits = "0123456789"
	b := []byte{0, 0, ':', 0, 0, ':', 0, 0}
	b[0], b[1] = digits[h/10%10], digits[h%10]
	b[3], b[4] = digits[m/10], digits[m%10]
	b[6], b[7] = digits[s/10], digits[s%10]
	return string(b)
}
