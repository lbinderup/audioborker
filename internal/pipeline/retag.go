package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"audioborker/internal/metadata"
	"audioborker/internal/pathtmpl"
	"audioborker/internal/store"
)

// runRetag rewrites the tags and chapters of an m4b that is already in the
// library. It never edits the file in place: the audio is stream-copied into
// staging (which also clears every existing atom, since `tone tag` merges
// rather than replaces), the copy is tagged and verified, and only a verified
// copy is renamed over the original. A failure, a cancel or a crash therefore
// always leaves the library file exactly as it was.
func (rc *RealConverter) runRetag(ctx context.Context, job *store.Job, report ProgressFunc, logf LogFunc) (*Result, error) {
	opts := job.Options
	if len(job.SourceFiles) != 1 {
		return nil, fmt.Errorf("a retag job covers exactly one file, got %d", len(job.SourceFiles))
	}
	src := job.SourceFiles[0]

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

	// ---- probe: the file exactly as it stands today ---------------------
	// Everything downstream reads THIS probe. The staged copy has no chapters
	// and no atoms left, so "keep the file's own chapters" and the
	// titles-on-existing-timings mode would have nothing to work from.
	report("probe", 0)
	prober := prober{ffprobe: rc.FFprobe}
	orig, err := prober.probe(ctx, src)
	if err != nil {
		return nil, err
	}
	if !orig.IsAACInMP4() {
		return nil, fmt.Errorf("%s holds %s in %s — retagging only rewrites mp4 atoms; convert it from Import instead",
			filepath.Base(src), orig.Codec, orig.Container)
	}
	logf("current file: %s, %d chapters, %d tags", fmtDuration(orig.DurationMs), len(orig.Chapters), len(orig.Tags))

	// ---- plan: where it lands, and which cover it keeps ------------------
	report("plan", 0.04)
	target, renamed, err := retagTarget(src, opts, job.Metadata, osPathState)
	if err != nil {
		return nil, err
	}
	if renamed {
		logf("rename: %s -> %s", src, target)
	} else {
		logf("replacing in place: %s", target)
	}
	// The strip drops the art stream; rc.cover falls back to extracting it,
	// so a provider outage never silently removes artwork the file had.
	coverPath := rc.cover(ctx, job, src, workDir, logf)

	// ---- copy: stream copy that also clears every atom -------------------
	report("copy", 0.06)
	staged := filepath.Join(stagingDir, "book.m4b")
	runner := ffmpegRunner{ffmpeg: rc.FFmpeg}
	if err := runner.remuxStripped(ctx, src, staged, orig.DurationMs,
		func(frac float64) { report("copy", scaled(0.06, 0.66, frac)) }, logf); err != nil {
		return nil, err
	}

	// ---- chapters: the same single decision point as a conversion --------
	report("chapters", 0.74)
	chapterMode := opts.ChapterMode
	if chapterMode == "" {
		chapterMode = ChapterModeAuto
	}
	provided, chapterWarns := rc.providerChapters(ctx, job, chapterMode, logf)
	warnings = append(warnings, chapterWarns...)
	resolved := resolveChapters(chapterMode, provided, []*FileInfo{orig}, orig.DurationMs, job.Metadata.Title)
	warnings = append(warnings, resolved.Warnings...)
	logf("chapters: %d entries from %q", len(resolved.Chapters), resolved.Source)

	chaptersPath := filepath.Join(stagingDir, "book.chapters.txt")
	if err := os.WriteFile(chaptersPath, []byte(chaptersTxt(resolved.Chapters)), 0o666); err != nil {
		return nil, err
	}

	// ---- tag --------------------------------------------------------------
	report("tag", 0.80)
	tone := toneRunner{tone: rc.Tone}
	if err := tone.tag(ctx, staged, &job.Metadata, coverPath, chaptersPath, logf); err != nil {
		return nil, err
	}

	// ---- verify BEFORE replacing -----------------------------------------
	// The ordering is the whole safety guarantee: never swap a good file out
	// for one we have not checked.
	report("verify", 0.90)
	tagged, err := prober.probe(ctx, staged)
	if err != nil {
		return nil, fmt.Errorf("verify: %w", err)
	}
	if err := verifyRetag(orig, tagged, len(resolved.Chapters), job.Metadata.ASIN); err != nil {
		return nil, err // the library file has not been touched
	}
	logf("verified: %s, %d chapters", fmtDuration(tagged.DurationMs), len(tagged.Chapters))

	// ---- replace ----------------------------------------------------------
	report("replace", 0.95)
	if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
		return nil, err
	}
	if err := replaceFile(staged, target); err != nil {
		return nil, err
	}
	if opts.WriteChaptersTxt {
		if err := copyFile(chaptersPath, sidecarFor(target)); err != nil {
			warnings = append(warnings, "Could not write the chapters.txt sidecar: "+err.Error())
		}
	}
	if renamed {
		// The new file exists before the old one goes: a crash in between
		// leaves a visible duplicate, never a missing book.
		if err := os.Remove(src); err != nil && !os.IsNotExist(err) {
			warnings = append(warnings, "The retagged file is in place but the original could not be removed: "+err.Error())
		}
		os.Remove(sidecarFor(src)) // ours to clean up; absence is fine
		pruneEmptyParents(filepath.Dir(src), opts.OutputDir, logf)
	}

	report("cleanup", 1)
	return &Result{
		OutputPath:   target,
		ChaptersJSON: resolved.json(),
		Warnings:     warnings,
	}, nil
}

// pathState answers what retagTarget needs to know about a rename
// destination: whether something is already there, and whether that something
// is the source file itself. Injected so the collision rules are testable
// without a filesystem.
type pathState func(target, src string) (exists, isSource bool)

// osPathState is the real filesystem. os.SameFile is what recognizes a pure
// case change on NTFS and SMB, where the "existing" file at the new spelling
// is the very file being renamed.
func osPathState(target, src string) (exists, isSource bool) {
	ti, err := os.Stat(target)
	if err != nil {
		return false, false
	}
	si, err := os.Stat(src)
	return true, err == nil && os.SameFile(ti, si)
}

// RetagTarget reports where a retag would put a file, for the rename preview.
// It runs the same decision the retag itself makes, so the preview can never
// promise a path the job would not produce.
func RetagTarget(src string, opts store.JobOptions, book metadata.Book) (target string, renamed bool, err error) {
	return retagTarget(src, opts, book, osPathState)
}

// retagTarget decides where a retagged file goes.
//
// No rename: the file's own path. Rename: the path the snapshotted template
// renders. A target that resolves to the file itself — including a pure case
// change — is an in-place replace with no old copy to remove; a *different*
// file already there is refused rather than overwritten.
func retagTarget(src string, opts store.JobOptions, book metadata.Book,
	state pathState) (target string, renamed bool, err error) {
	if !opts.Rename {
		return src, false, nil
	}
	rel, err := pathtmpl.Render(opts.PathTemplate, PathVars(book))
	if err != nil {
		return "", false, fmt.Errorf("output path template: %w", err)
	}
	target = filepath.Join(opts.OutputDir, filepath.FromSlash(rel)+".m4b")
	if target == src {
		return src, false, nil
	}
	if exists, isSource := state(target, src); exists {
		if isSource {
			// Renaming fixes the spelling; there is no old copy afterwards.
			return target, false, nil
		}
		return "", false, fmt.Errorf("cannot rename to %s: a different file is already there — remove it or change the path template", target)
	}
	return target, true, nil
}

// verifyRetag asserts what a retag must never get wrong, while the original is
// still untouched: the audio is the same length (a stream copy cannot change
// it), every chapter asked for was written (tone silently ignoring the
// chapters file is exactly the failure this catches), and the identifying
// atoms actually landed.
func verifyRetag(before, after *FileInfo, wantChapters int, asin string) error {
	// A remux can shift the reported duration by a few ms (edit lists, gapless
	// padding), so this is a tolerance rather than an equality test.
	if delta := abs64(after.DurationMs - before.DurationMs); delta > runtimeTolerance(before.DurationMs) {
		return fmt.Errorf("verify: the retagged copy is %s but the original is %s — refusing to replace it",
			msToTimestamp(after.DurationMs), msToTimestamp(before.DurationMs))
	}
	if len(after.Chapters) != wantChapters {
		return fmt.Errorf("verify: wrote %d chapters but the copy has %d — refusing to replace the original",
			wantChapters, len(after.Chapters))
	}
	if strings.TrimSpace(after.Tags["title"]) == "" {
		return fmt.Errorf("verify: the retagged copy has no title tag — refusing to replace the original")
	}
	return nil
}

// replaceFile puts src at dst. Unlike moveFile it must NOT fall back to
// copy+delete: dst is a file the user already has, and a half-finished copy
// over it is the exact accident all the staging exists to prevent. Staging
// lives under the output volume, so a same-filesystem rename always holds —
// if it fails, something is wrong and stopping is the right answer.
func replaceFile(src, dst string) error {
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("could not replace %s (is it open in a player?): %w", dst, err)
	}
	return nil
}

// sidecarFor is the chapters.txt path that accompanies an m4b.
func sidecarFor(m4b string) string {
	return strings.TrimSuffix(m4b, ".m4b") + ".chapters.txt"
}

// pruneEmptyParents removes the directories a rename left behind, walking up
// from dir and stopping at (never touching) stopAt — the library root.
func pruneEmptyParents(dir, stopAt string, logf LogFunc) {
	stop := filepath.Clean(stopAt)
	for d := filepath.Clean(dir); d != stop; d = filepath.Dir(d) {
		rel, err := filepath.Rel(stop, d)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return // outside the library: not ours to touch
		}
		if !pruneEmptyDirs(d) {
			return // still holds something real
		}
		logf("removed the now-empty folder %s", d)
	}
}
