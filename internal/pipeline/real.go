package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"audioborker/internal/metadata"
	"audioborker/internal/pathtmpl"
	"audioborker/internal/store"
)

// ChapterSource fetches provider chapters (implemented by the audnexus
// provider; cached via the store wrapper in cmd wiring).
type ChapterSource interface {
	GetChapters(ctx context.Context, asin, region string) (*metadata.ChapterInfo, error)
}

// RealConverter drives ffmpeg + tone. One instance is shared by all workers;
// per-job state lives on the stack.
type RealConverter struct {
	FFmpeg   string
	FFprobe  string
	Tone     string
	Fdkaac   string // optional standalone encoder
	WorkRoot string // scratch: cover, concat list, chapters.txt
	Chapters func(audnexusURL string) ChapterSource
	HTTP     *http.Client // cover downloads
}

// stage weights: probe/plan 0-10%, merge 10-88%, chapters/tag 88-96%,
// verify/move/cleanup 96-100%.
func scaled(base, span, frac float64) float64 { return base + span*clamp01(frac) }

func (rc *RealConverter) http() *http.Client {
	if rc.HTTP != nil {
		return rc.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (rc *RealConverter) Run(ctx context.Context, job *store.Job, report ProgressFunc, logf LogFunc) (*Result, error) {
	if job.Options.IsRetag() {
		return rc.runRetag(ctx, job, report, logf)
	}
	opts := job.Options
	workDir := filepath.Join(rc.WorkRoot, job.ID)
	stagingDir := filepath.Join(opts.OutputDir, ".audioborker-work", job.ID)
	for _, d := range []string{workDir, stagingDir} {
		if err := os.MkdirAll(d, 0o777); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}
	defer os.RemoveAll(workDir)
	defer os.RemoveAll(stagingDir)

	var warnings []string

	// ---- probe ----------------------------------------------------------
	report("probe", 0)
	prober := prober{ffprobe: rc.FFprobe}
	files := make([]*FileInfo, 0, len(job.SourceFiles))
	for i, path := range job.SourceFiles {
		info, err := prober.probe(ctx, path)
		if err != nil {
			return nil, err
		}
		files = append(files, info)
		report("probe", scaled(0, 0.08, float64(i+1)/float64(len(job.SourceFiles))))
	}
	if len(files) == 0 {
		return nil, errors.New("no source files recorded on the job")
	}

	// ---- plan -----------------------------------------------------------
	report("plan", 0.08)
	plan := planMerge(files, opts.BitrateKbps)
	logf("plan: %d file(s), total %s, %s", len(files), fmtDuration(plan.TotalMs),
		planDescription(plan))

	rel, err := pathtmpl.Render(opts.PathTemplate, varsFor(job))
	if err != nil {
		return nil, fmt.Errorf("output path template: %w", err)
	}
	finalPath := filepath.Join(opts.OutputDir, filepath.FromSlash(rel)+".m4b")
	if _, err := os.Stat(finalPath); err == nil {
		return nil, fmt.Errorf("output already exists: %s — remove it or change the path template", finalPath)
	}
	logf("output: %s", finalPath)

	coverPath := rc.cover(ctx, job, job.SourceFiles[0], workDir, logf)
	report("plan", 0.10)

	// ---- merge ----------------------------------------------------------
	stagedM4B := filepath.Join(stagingDir, "book.m4b")
	runner := ffmpegRunner{ffmpeg: rc.FFmpeg}
	// Even a single already-AAC file goes through ffmpeg rather than a byte
	// copy: -map_metadata -1 -map_chapters -1 is what guarantees the staged file
	// starts with zero metadata, and tone MERGES rather than clears. A raw copy
	// left the source's own atoms (ISBN, RATING, a stale SERIES from whatever
	// tagged it) in the output, so single-file conversions were tagged
	// inconsistently with multi-file ones. Stream-copying one file is I/O-bound
	// like the copy was, and adds +faststart for free.
	listPath := filepath.Join(workDir, "concat.txt")
	if err := writeConcatList(listPath, files); err != nil {
		return nil, err
	}
	mergeProgress := func(frac float64) { report("merge", scaled(0.10, 0.78, frac)) }
	useFdkaac := opts.Encoder == "fdkaac" && !plan.StreamCopy
	if useFdkaac {
		if _, lookErr := exec.LookPath(rc.Fdkaac); lookErr != nil {
			warnings = append(warnings, "fdkaac is not available in this image — used ffmpeg's native AAC encoder instead.")
			useFdkaac = false
		}
	}
	var mergeErr error
	if useFdkaac {
		mergeErr = runner.mergeFdkaac(ctx, rc.Fdkaac, plan, listPath, stagedM4B, mergeProgress, logf)
	} else {
		mergeErr = runner.merge(ctx, plan, listPath, stagedM4B, mergeProgress, logf)
	}
	if mergeErr != nil {
		return nil, mergeErr
	}

	// ---- chapters -------------------------------------------------------
	report("chapters", 0.88)
	merged, err := prober.probe(ctx, stagedM4B)
	if err != nil {
		return nil, fmt.Errorf("probe merged file: %w", err)
	}
	chapterMode := opts.ChapterMode
	if chapterMode == "" {
		chapterMode = ChapterModeAuto
	}
	provided, chapterWarns := rc.providerChapters(ctx, job, chapterMode, logf)
	warnings = append(warnings, chapterWarns...)
	resolved := resolveChapters(chapterMode, provided, files, merged.DurationMs, job.Metadata.Title)
	warnings = append(warnings, resolved.Warnings...)
	logf("chapters: %d entries from %q", len(resolved.Chapters), resolved.Source)

	chaptersPath := filepath.Join(stagingDir, "book.chapters.txt")
	if err := os.WriteFile(chaptersPath, []byte(chaptersTxt(resolved.Chapters)), 0o666); err != nil {
		return nil, err
	}

	// ---- tag ------------------------------------------------------------
	report("tag", 0.92)
	tone := toneRunner{tone: rc.Tone}
	if err := tone.tag(ctx, stagedM4B, &job.Metadata, coverPath, chaptersPath, logf); err != nil {
		return nil, err
	}

	// ---- verify BEFORE moving ---------------------------------------------
	// As in a retag, the staged file is checked while it is still outside the
	// library. Verifying after the move left a failed job's book sitting at
	// its final path, and a retry then refused with "output already exists".
	report("verify", 0.96)
	tagged, err := prober.probe(ctx, stagedM4B)
	if err != nil {
		return nil, fmt.Errorf("verify: %w", err)
	}
	if err := verifyConversion(plan, tagged); err != nil {
		return nil, err // nothing has reached the library
	}
	logf("verified: %s, %d chapters", fmtDuration(tagged.DurationMs), len(tagged.Chapters))

	// ---- move -----------------------------------------------------------
	report("move", 0.98)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o777); err != nil {
		return nil, err
	}
	if err := moveFile(stagedM4B, finalPath); err != nil {
		return nil, err
	}
	if opts.WriteChaptersTxt {
		sidecar := strings.TrimSuffix(finalPath, ".m4b") + ".chapters.txt"
		if err := copyFile(chaptersPath, sidecar); err != nil {
			warnings = append(warnings, "Could not write the chapters.txt sidecar: "+err.Error())
		}
	}

	// ---- cleanup --------------------------------------------------------
	report("cleanup", 0.99)
	if err := rc.cleanupSource(job, logf); err != nil {
		warnings = append(warnings, "Source cleanup failed: "+err.Error())
	}

	report("cleanup", 1)
	return &Result{
		OutputPath:   finalPath,
		ChaptersJSON: resolved.json(),
		Warnings:     warnings,
	}, nil
}

// verifyConversion asserts what a finished book must get right before it
// goes into the library: the audio is as long as its sources together (a
// truncated merge or a decode that stopped early shows up here), and a merge
// of several files carries chapters — multi-file input must never produce
// zero.
func verifyConversion(plan mergePlan, out *FileInfo) error {
	if delta := abs64(out.DurationMs - plan.TotalMs); delta > runtimeTolerance(plan.TotalMs) {
		return fmt.Errorf("verify: output duration %s differs from source total %s",
			msToTimestamp(out.DurationMs), msToTimestamp(plan.TotalMs))
	}
	if len(plan.Files) > 1 && len(out.Chapters) == 0 {
		return errors.New("verify: merged file has no chapters")
	}
	return nil
}

func planDescription(p mergePlan) string {
	if p.StreamCopy {
		return "stream copy (no re-encode)"
	}
	return fmt.Sprintf("transcode to AAC %dk @ %d Hz", p.BitrateKbps, p.SampleRate)
}

// cover picks the artwork to embed: an image chosen by hand (the manual
// tagging flow), else the catalog's, else the one the source already carries.
// The last step matters for conversions too: the merge maps audio only, so
// without it a book with no catalog cover — or a failed download — would
// lose artwork its source files had.
func (rc *RealConverter) cover(ctx context.Context, job *store.Job, src, workDir string, logf LogFunc) string {
	if f := job.Options.CoverFile; f != "" {
		if fi, err := os.Stat(f); err == nil && fi.Size() > 0 {
			logf("cover: %s (chosen by hand)", f)
			return f
		}
		logf("cover: the chosen image %s is gone, falling back", f)
	}
	if p := rc.downloadCover(ctx, job.Metadata.CoverURL, workDir, logf); p != "" {
		return p
	}
	return extractCover(ctx, rc.FFmpeg, src, workDir, logf)
}

// downloadCover fetches the hi-res cover into the work dir; failures degrade
// to "" so the caller can fall back to the source's own artwork.
func (rc *RealConverter) downloadCover(ctx context.Context, url, workDir string, logf LogFunc) string {
	if url == "" {
		return ""
	}
	try := func(u string) string {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return ""
		}
		resp, err := rc.http().Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				resp.Body.Close()
			}
			return ""
		}
		defer resp.Body.Close()
		dst := filepath.Join(workDir, "cover.jpg")
		f, err := os.Create(dst)
		if err != nil {
			return ""
		}
		defer f.Close()
		if _, err := io.Copy(f, resp.Body); err != nil {
			return ""
		}
		return dst
	}
	// Prefer the 2000px variant Audible hosts under a size-suffixed name.
	if hi := hiResCoverURL(url); hi != url {
		if p := try(hi); p != "" {
			logf("cover: downloaded hi-res")
			return p
		}
	}
	if p := try(url); p != "" {
		logf("cover: downloaded")
		return p
	}
	logf("cover: download failed, continuing without")
	return ""
}

// amazonImageRe matches an Amazon image CDN URL, with or without a size
// modifier: ".../I/91F-2mp2T1L.jpg", "…L._SL500_.jpg", "…L._AC_SX342_.jpg".
var amazonImageRe = regexp.MustCompile(`^(https://[^/]*(?:media-amazon|images-amazon)\.com/images/I/[^./]+)(?:\._[A-Za-z0-9,_]+_)?\.jpg$`)

// hiResCoverURL asks Amazon's CDN for the cover at up to 2000 px. Catalog
// URLs often name a smaller rendition (product details ._SL1000_), which
// would otherwise be embedded as is; the CDN never upscales, so a smaller
// original simply comes back at its own size.
func hiResCoverURL(url string) string { return CoverURLAt(url, 2000) }

// CoverURLAt is an Amazon cover URL rendered at up to px on its longest
// side; any other URL is returned unchanged.
func CoverURLAt(url string, px int) string {
	if m := amazonImageRe.FindStringSubmatch(url); m != nil {
		return fmt.Sprintf("%s._SL%d_.jpg", m[1], px)
	}
	return url
}

// CoverDownloadURL is the URL a cover is fetched from first. Previews load
// it too, so the size they show is the size that gets embedded.
func CoverDownloadURL(url string) string { return hiResCoverURL(url) }

// cleanupSource applies the job's cleanup mode to the original input.
func (rc *RealConverter) cleanupSource(job *store.Job, logf LogFunc) error {
	opts := job.Options
	src := filepath.Join(opts.InputDir, filepath.FromSlash(job.InputPath))
	switch opts.CleanupMode {
	case "leave", "":
		return nil
	case "move":
		if opts.CompletedDir == "" {
			return errors.New("no completed directory configured")
		}
		if err := os.MkdirAll(opts.CompletedDir, 0o777); err != nil {
			return err
		}
		dst := filepath.Join(opts.CompletedDir, filepath.Base(src))
		if _, err := os.Stat(dst); err == nil {
			dst = dst + "-" + job.ID[:8]
		}
		logf("cleanup: moving source to %s", dst)
		return moveTree(src, dst)
	case "delete":
		logf("cleanup: deleting source files")
		info, err := os.Stat(src)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return os.Remove(src)
		}
		// Delete only the files we consumed, then prune empty dirs — a junk
		// file someone stashed in the folder should not be silently nuked.
		for _, f := range job.SourceFiles {
			if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		pruneEmptyDirs(src)
		return nil
	default:
		return fmt.Errorf("unknown cleanup mode %q", opts.CleanupMode)
	}
}

// providerChapters fetches the catalog's chapter list for a job and applies
// its snapshotted shift. A missing or failed lookup is not fatal — chapter
// resolution falls back to the file's own data — so this returns warnings
// rather than an error. Shared by conversions and retags.
func (rc *RealConverter) providerChapters(ctx context.Context, job *store.Job, chapterMode string, logf LogFunc) (*metadata.ChapterInfo, []string) {
	opts := job.Options
	var warnings []string
	var provided *metadata.ChapterInfo
	if chapterMode != ChapterModeExisting && rc.Chapters != nil && job.ASIN != "" {
		var err error
		provided, err = rc.Chapters(opts.AudnexusURL).GetChapters(ctx, job.ASIN, job.Region)
		if err != nil {
			provided = nil
			if metadata.IsNotFound(err) {
				logf("no provider chapters for %s (%s)", job.ASIN, job.Region)
			} else {
				logf("chapter lookup failed (%v) — using file-based chapters", err)
				warnings = append(warnings, "Chapter lookup failed; embedded file-based chapters instead.")
			}
		}
	}
	if shift := opts.EffectiveChapterShift(); !shift.IsZero() && provided != nil {
		provided = provided.ShiftedBy(shift)
		logf("applied chapter shift: %s", shift)
	}
	return provided, warnings
}

// pruneEmptyDirs removes dir if (recursively) empty, ignoring junk files.
func pruneEmptyDirs(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	empty := true
	for _, e := range entries {
		if e.IsDir() {
			if !pruneEmptyDirs(filepath.Join(dir, e.Name())) {
				empty = false
			}
		} else if isJunkFile(e.Name()) {
			os.Remove(filepath.Join(dir, e.Name()))
		} else {
			empty = false
		}
	}
	if empty {
		return os.Remove(dir) == nil
	}
	return false
}

func isJunkFile(name string) bool {
	switch name {
	case ".DS_Store", "Thumbs.db", "desktop.ini":
		return true
	}
	return strings.HasPrefix(name, "._")
}

// moveFile renames, falling back to copy+delete across filesystems (EXDEV).
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// moveTree moves a file or directory, falling back to per-file copy.
func moveTree(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return moveFile(src, dst)
	}
	err = filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o777)
		}
		return moveFile(p, target)
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(src)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
