package pipeline

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"audioborker/internal/metadata"
)

// ResolvedChapters is what actually gets embedded, plus provenance for the
// job record and UI.
type ResolvedChapters struct {
	Source   string             `json:"source"` // one of the Source* constants
	Chapters []metadata.Chapter `json:"chapters"`
	Shift    metadata.ShiftSpec `json:"shift,omitzero"` // as applied; zero when none was
	Warnings []string           `json:"-"`
}

// ResolvedChapters.Source values. ("provider" was called "audnexus" before
// the aggregation layer; the string is only interpreted live — verdict text
// and CSS classes — never re-parsed from stored jobs, so the rename is
// compatibility-free.)
const (
	SourceProvider       = "provider"        // the remote catalog's chapter list
	SourceExisting       = "existing"        // chapters already embedded in the input file
	SourceSidecar        = "sidecar"         // the chapters.txt next to the input file
	SourceFiles          = "files"           // one chapter per input file
	SourceSingle         = "single"          // one whole-book chapter
	SourceTitlesFiles    = "titles-files"    // provider titles on file boundaries
	SourceTitlesExisting = "titles-existing" // provider titles on embedded timings
)

// runtimeTolerance returns the allowed |provider - actual| runtime delta.
func runtimeTolerance(actualMs int64) int64 {
	onePct := actualMs / 100
	if onePct < 10_000 {
		return 10_000 // at least 10 seconds
	}
	return onePct
}

// Chapter source modes a job can request.
const (
	ChapterModeAuto     = "auto"     // provider if runtime matches → existing → boundaries
	ChapterModeExisting = "existing" // keep the input file's own chapters
	ChapterModeSidecar  = "sidecar"  // use the chapters.txt next to the input file
	ChapterModeProvider = "provider" // force provider chapters even on runtime mismatch
	// Mix modes: the provider's chapter TITLES on LOCAL timings — for rips
	// whose boundaries are right but whose names are junk ("Track 01").
	// Timings are local by construction, so the runtime gate doesn't apply;
	// a shift moves the file's own timings as it would without the titles.
	ChapterModeTitlesFiles    = "titles-files"    // titles onto file boundaries (multi-file)
	ChapterModeTitlesExisting = "titles-existing" // titles onto the file's own chapters (single-file)
)

// PlanChapters previews exactly what a conversion would embed, without
// converting: the match screen calls this so the "auto" decision is visible
// before queueing. totalMs is the summed source duration (a close stand-in
// for the merged duration the pipeline validates against).
func PlanChapters(mode string, provider *metadata.ChapterInfo, files []*FileInfo, totalMs int64, bookTitle string, shift metadata.ShiftSpec) ResolvedChapters {
	if mode == "" {
		mode = ChapterModeAuto
	}
	return resolveChapters(mode, provider, files, totalMs, bookTitle, shift)
}

// resolveChapters decides what chapters to embed, then applies the shift to
// them — after the choice, so it follows whichever list is embedded.
func resolveChapters(mode string, provider *metadata.ChapterInfo, files []*FileInfo, actualMs int64, bookTitle string, shift metadata.ShiftSpec) ResolvedChapters {
	return applyShift(chooseChapters(mode, provider, files, actualMs, bookTitle), shift, provider, actualMs)
}

// applyShift moves the chosen chapters' timings by the job's ShiftSpec: the
// catalog's chapters, or the file's own (also under the catalog's titles) —
// a file whose chapter names are right but whose timings are off. File
// boundaries and the whole-book chapter are exact by construction, so a
// shift there is ignored, with a warning rather than silently.
func applyShift(r ResolvedChapters, shift metadata.ShiftSpec, provider *metadata.ChapterInfo, actualMs int64) ResolvedChapters {
	if shift.IsZero() || len(r.Chapters) == 0 {
		return r
	}
	runtime := actualMs // chapters must not start past the audio
	switch r.Source {
	case SourceProvider:
		runtime = provider.RuntimeMs
	case SourceExisting, SourceSidecar, SourceTitlesExisting:
	case SourceFiles, SourceTitlesFiles:
		r.Warnings = append(r.Warnings, "Shift ignored: file boundaries are exact.")
		return r
	default:
		r.Warnings = append(r.Warnings, "Shift ignored: one whole-book chapter.")
		return r
	}
	shifted := (&metadata.ChapterInfo{RuntimeMs: runtime, Chapters: r.Chapters}).ShiftedBy(shift)
	r.Chapters, r.Shift = shifted.Chapters, shift
	return r
}

// chooseChapters decides what chapters to embed. In "auto" mode the
// priority is:
//  1. Provider chapters whose runtime matches the merged audio.
//  2. A single input file's own chapters (OwnChapters: those inside it, or
//     its chapters.txt when those are missing or cut short).
//  3. One chapter per input file (cumulative boundaries).
//  4. A single whole-book chapter (single-file input with no data at all).
//
// Multi-file input NEVER produces zero chapters (bragibooks/m4b-merge gap).
func chooseChapters(mode string, provider *metadata.ChapterInfo, files []*FileInfo, actualMs int64, bookTitle string) ResolvedChapters {
	var out ResolvedChapters

	// Mix modes run first: they combine two sources, so they can't sit in
	// the single-source priority chain below. On any alignment failure they
	// degrade to the automatic decision — never to zero chapters.
	switch mode {
	case ChapterModeTitlesFiles, ChapterModeTitlesExisting:
		if chs, src, warns := mixTitles(mode, provider, files); chs != nil {
			out.Source, out.Chapters, out.Warnings = src, chs, warns
			return out
		}
		out.Warnings = append(out.Warnings, mixFallbackWarning(mode, provider, files))
		mode = ChapterModeAuto
	}
	if mode == ChapterModeSidecar && (len(files) != 1 || len(files[0].Sidecar) == 0) {
		out.Warnings = append(out.Warnings, "No usable .chapters.txt next to the file — fell back to the automatic decision.")
		mode = ChapterModeAuto
	}

	local := mode == ChapterModeExisting || mode == ChapterModeSidecar
	useProvider := !local && provider != nil && len(provider.Chapters) > 0
	if useProvider {
		delta := provider.RuntimeMs - actualMs
		if delta < 0 {
			delta = -delta
		}
		mismatch := delta > runtimeTolerance(actualMs)
		if !mismatch || mode == ChapterModeProvider {
			out.Source = SourceProvider
			out.Chapters = provider.Chapters
			if mismatch {
				out.Warnings = append(out.Warnings, fmt.Sprintf(
					"Provider chapters were forced but expect a runtime of %s vs the actual %s — timings may be misaligned.",
					fmtDuration(provider.RuntimeMs), fmtDuration(actualMs)))
			}
			if !provider.IsAccurate {
				out.Warnings = append(out.Warnings,
					"The chapter source flags these timings as possibly inaccurate (isAccurate=false).")
			}
			return out
		}
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"Provider chapters expect a runtime of %s but the merged audio is %s — fell back to file-based chapters. (Different edition or incomplete rip?)",
			fmtDuration(provider.RuntimeMs), fmtDuration(actualMs)))
	}
	if mode == ChapterModeProvider && !useProvider {
		out.Warnings = append(out.Warnings,
			"Provider chapters were requested but none were available — used file-based chapters instead.")
	}

	// A single input file with chapters of its own: the kind asked for, else
	// whichever OwnChapters picks.
	if len(files) == 1 {
		f := files[0]
		chs, src := OwnChapters(f)
		switch {
		case mode == ChapterModeExisting && len(f.Chapters) > 0:
			chs, src = f.Chapters, SourceExisting
		case mode == ChapterModeSidecar:
			chs, src = f.Sidecar, SourceSidecar
		}
		if chs != nil {
			out.Source, out.Chapters = src, toChapters(chs)
			return out
		}
	}

	if len(files) > 1 {
		out.Source = SourceFiles
		out.Chapters = chaptersFromBoundaries(files)
		return out
	}

	// Single file, no chapters anywhere: one chapter spanning the book.
	title := bookTitle
	if title == "" {
		title = "Chapter 01"
	}
	out.Source = SourceSingle
	out.Chapters = []metadata.Chapter{{Title: title, StartMs: 0, LengthMs: actualMs}}
	return out
}

// mixTitles builds local-timed chapters carrying the provider's titles.
// Returns nil when the mode's local timing source is missing or the title
// counts can't be reconciled.
func mixTitles(mode string, provider *metadata.ChapterInfo, files []*FileInfo) ([]metadata.Chapter, string, []string) {
	if provider == nil || len(provider.Chapters) == 0 {
		return nil, "", nil
	}
	var local []metadata.Chapter
	var src string
	switch mode {
	case ChapterModeTitlesFiles:
		if len(files) < 2 {
			return nil, "", nil
		}
		local = chaptersFromBoundaries(files)
		src = SourceTitlesFiles
	case ChapterModeTitlesExisting:
		if len(files) != 1 {
			return nil, "", nil
		}
		own, _ := OwnChapters(files[0])
		if own == nil {
			return nil, "", nil
		}
		local = toChapters(own)
		src = SourceTitlesExisting
	default:
		return nil, "", nil
	}
	titles, note, ok := alignTitles(provider, len(local))
	if !ok {
		return nil, "", nil
	}
	for i := range local {
		local[i].Title = titles[i]
	}
	var warns []string
	if note != "" {
		warns = append(warns, note)
	}
	return local, src, warns
}

// alignTitles reconciles the provider's chapter titles with n local timing
// slots. An exact count maps 1:1. When the provider has one or two extras,
// a SHORT leading and/or trailing chapter is dropped — Audible editions
// carry "Opening/End Credits" (≈ the brand intro/outro stingers) that rips
// usually lack. Anything else fails: count matching is deliberately strict,
// because distributing titles fuzzily would silently misname every chapter
// after the first drift point.
func alignTitles(p *metadata.ChapterInfo, n int) (titles []string, note string, ok bool) {
	chs := p.Chapters
	extra := len(chs) - n
	if n == 0 || extra < 0 || extra > 2 {
		return nil, "", false
	}
	short := func(i int, brandMs int64) bool {
		limit := brandMs + 5_000
		if limit < 40_000 {
			limit = 40_000
		}
		return chs[i].LengthMs > 0 && chs[i].LengthMs <= limit
	}
	var dropped []string
	lo, hi := 0, len(chs)
	for range extra {
		switch {
		case short(lo, p.BrandIntroMs):
			dropped = append(dropped, chs[lo].Title)
			lo++
		case short(hi-1, p.BrandOutroMs):
			dropped = append(dropped, chs[hi-1].Title)
			hi--
		default:
			return nil, "", false
		}
	}
	for _, c := range chs[lo:hi] {
		titles = append(titles, c.Title)
	}
	if len(dropped) > 0 {
		note = fmt.Sprintf("Skipped the provider's short %q chapter(s) to line the titles up — rips usually omit those.",
			strings.Join(dropped, "”, “"))
	}
	return titles, note, true
}

// mixFallbackWarning explains why a requested mix mode couldn't be honored.
func mixFallbackWarning(mode string, provider *metadata.ChapterInfo, files []*FileInfo) string {
	if provider == nil || len(provider.Chapters) == 0 {
		return "Audible's chapter titles were requested but no chapter data is available — fell back to the automatic decision."
	}
	local := "your input"
	switch mode {
	case ChapterModeTitlesFiles:
		local = fmt.Sprintf("your %d file(s)", len(files))
	case ChapterModeTitlesExisting:
		n := 0
		if len(files) == 1 {
			own, _ := OwnChapters(files[0])
			n = len(own)
		}
		local = fmt.Sprintf("the file's %d embedded chapter(s)", n)
	}
	return fmt.Sprintf("Audible has %d chapter titles but they can't be lined up with %s — fell back to the automatic decision.",
		len(provider.Chapters), local)
}

func toChapters(chs []ProbedChapter) []metadata.Chapter {
	out := make([]metadata.Chapter, 0, len(chs))
	for _, c := range chs {
		out = append(out, metadata.Chapter{Title: c.Title, StartMs: c.StartMs, LengthMs: c.EndMs - c.StartMs})
	}
	return out
}

// chaptersFromBoundaries builds one chapter per input file from cumulative
// durations. Titles come from cleaned filenames when they carry signal,
// otherwise "Chapter NN".
func chaptersFromBoundaries(files []*FileInfo) []metadata.Chapter {
	titles := filenameTitles(files)
	var chapters []metadata.Chapter
	var offset int64
	for i, f := range files {
		chapters = append(chapters, metadata.Chapter{
			Title:    titles[i],
			StartMs:  offset,
			LengthMs: f.DurationMs,
		})
		offset += f.DurationMs
	}
	return chapters
}

var trackNumPrefix = regexp.MustCompile(`(?i)^\s*(?:track|chapter|part|ch|pt)?[\s._-]*\d+[\s._-]*`)

// filenameTitles derives chapter titles from filenames. If cleaning leaves
// nothing meaningful (pure track numbers) or every name collapses to the
// same string, fall back to "Chapter NN".
func filenameTitles(files []*FileInfo) []string {
	titles := make([]string, len(files))
	distinct := map[string]bool{}
	useful := true
	for i, f := range files {
		base := strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path))
		clean := trackNumPrefix.ReplaceAllString(base, "")
		clean = strings.Trim(strings.ReplaceAll(clean, "_", " "), " -–.")
		if len([]rune(clean)) < 3 || !containsLetter(clean) {
			useful = false
			break
		}
		titles[i] = clean
		distinct[clean] = true
	}
	if !useful || len(distinct) < len(files) {
		for i := range titles {
			titles[i] = fmt.Sprintf("Chapter %02d", i+1)
		}
	}
	return titles
}

func containsLetter(s string) bool {
	for _, r := range s {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r > 127 {
			return true
		}
	}
	return false
}

// chaptersTxt renders the classic m4b-tool/mp4chaps sidecar format:
// "HH:MM:SS.mmm Title" per line. tone imports the same format.
func chaptersTxt(chapters []metadata.Chapter) string {
	var b strings.Builder
	var end int64
	for _, c := range chapters {
		fmt.Fprintf(&b, "%s %s\n", msToTimestamp(c.StartMs), strings.TrimSpace(c.Title))
		if e := c.StartMs + c.LengthMs; e > end {
			end = e
		}
	}
	if end > 0 {
		fmt.Fprintf(&b, "## total-duration: %s\n", msToTimestamp(end))
	}
	return b.String()
}

func msToTimestamp(ms int64) string {
	h := ms / 3_600_000
	m := ms % 3_600_000 / 60_000
	s := ms % 60_000 / 1000
	frac := ms % 1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, frac)
}

func fmtDuration(ms int64) string {
	h := ms / 3_600_000
	m := ms % 3_600_000 / 60_000
	return fmt.Sprintf("%dh%02dm", h, m)
}

func (r ResolvedChapters) json() string {
	raw, _ := json.Marshal(r)
	return string(raw)
}
