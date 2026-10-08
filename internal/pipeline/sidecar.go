package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// A Book.chapters.txt next to an audiobook is a chapter source of its own,
// beside the chapters inside the file and Audible's. Taggers write one
// alongside the m4b (m4b-tool, mp4chaps, this app), and it can be the only
// intact copy: the tool behind Poseidon's Wake sized the embedded chapters
// to an overflowed length header, leaving 2 of 57, while its sidecar kept
// them all. Left to decide, the pipeline takes it over embedded chapters that
// are missing or were cut short — never over a complete set (OwnChapters).
//
// Only source files are read this way. The pipeline writes a chapters.txt
// next to its own staged copy, and verify has to count the chapters inside
// the file, or tone failing to write them would hide behind the sidecar.

// sidecarFor is the chapters.txt that accompanies an audio file.
func sidecarFor(path string) string {
	return strings.TrimSuffix(path, filepath.Ext(path)) + ".chapters.txt"
}

// writesSidecar decides whether a finished book gets a chapters.txt: when
// the setting asks for one, and when its single source file came with one —
// a book that had a sidecar leaves with one, rewritten to match the chapters
// baked in rather than left to contradict them.
func writesSidecar(setting bool, sources []string) bool {
	if setting {
		return true
	}
	if len(sources) != 1 {
		return false
	}
	_, err := os.Stat(sidecarFor(sources[0]))
	return err == nil
}

// HasSidecar reports whether a chapters.txt sits next to an audio file.
func HasSidecar(path string) bool { return fileExists(sidecarFor(path)) }

func removeWithSidecar(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(sidecarFor(path)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// readSidecar loads the chapters.txt next to a source file into Sidecar. One
// that can't belong to this audio is ignored and noted in SidecarErr.
func readSidecar(info *FileInfo) {
	path := sidecarFor(info.Path)
	raw, err := os.ReadFile(path)
	if err != nil {
		return // no sidecar: the common case
	}
	chs, err := parseChaptersTxt(string(raw), info.DurationMs)
	if err != nil {
		info.SidecarErr = fmt.Sprintf("Ignored %s: %v.", filepath.Base(path), err)
		return
	}
	info.Sidecar, info.SidecarName = chs, filepath.Base(path)
}

// OwnChapters is what counts as a single file's own chapters when nobody
// picked between the two kinds: those inside it, unless they are missing or
// were cut short by an overflowed header and a chapters.txt has them whole.
// source is SourceExisting or SourceSidecar; nil when the file has neither.
func OwnChapters(f *FileInfo) (chs []ProbedChapter, source string) {
	switch {
	case len(f.Chapters) > 0 && !f.ChaptersTruncated:
		return f.Chapters, SourceExisting
	case len(f.Sidecar) > 0:
		return f.Sidecar, SourceSidecar
	case len(f.Chapters) > 0:
		return f.Chapters, SourceExisting
	}
	return nil, ""
}

var (
	// "0:00:35.665 Chapter One" (m4b-tool writes 1-3 fraction digits and
	// unpadded hours; this app writes HH:MM:SS.mmm).
	plainChapterRe = regexp.MustCompile(`^(\d+):(\d{1,2}):(\d{1,2})(?:[.,](\d+))?(?:\s+(.*))?$`)
	// OGM style: CHAPTER01=00:00:00.000 then CHAPTER01NAME=Title.
	ogmTimeRe = regexp.MustCompile(`(?i)^CHAPTER(\d+)=(\d+):(\d{1,2}):(\d{1,2})(?:[.,](\d+))?$`)
	ogmNameRe = regexp.MustCompile(`(?i)^CHAPTER(\d+)NAME=(.*)$`)
)

// parseChaptersTxt reads a chapters.txt into chapters spanning durationMs.
// It returns an error for a list that can't describe this audio: out of
// order, or starting past its end (a sidecar from another edition).
func parseChaptersTxt(text string, durationMs int64) ([]ProbedChapter, error) {
	var chs []ProbedChapter
	ogm := map[string]int{} // OGM chapter number -> index in chs
	for _, line := range strings.Split(strings.TrimPrefix(text, "\xef\xbb\xbf"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := plainChapterRe.FindStringSubmatch(line); m != nil {
			chs = append(chs, ProbedChapter{Title: strings.TrimSpace(m[5]), StartMs: clockMs(m[1], m[2], m[3], m[4])})
			continue
		}
		if m := ogmTimeRe.FindStringSubmatch(line); m != nil {
			ogm[m[1]] = len(chs)
			chs = append(chs, ProbedChapter{StartMs: clockMs(m[2], m[3], m[4], m[5])})
			continue
		}
		if m := ogmNameRe.FindStringSubmatch(line); m != nil {
			if i, ok := ogm[m[1]]; ok {
				chs[i].Title = strings.TrimSpace(m[2])
			}
		}
	}
	for i := range chs {
		if i > 0 && chs[i].StartMs < chs[i-1].StartMs {
			return nil, fmt.Errorf("chapter %d starts before chapter %d", i+1, i)
		}
		if durationMs > 0 && chs[i].StartMs >= durationMs {
			return nil, fmt.Errorf("chapter %d starts after the audio ends", i+1)
		}
		if chs[i].Title == "" {
			chs[i].Title = fmt.Sprintf("Chapter %02d", i+1)
		}
		if i+1 < len(chs) {
			chs[i].EndMs = chs[i+1].StartMs
		} else {
			chs[i].EndMs = durationMs
		}
	}
	return chs, nil
}

// clockMs reads h:m:s plus a decimal fraction of a second ("14" in
// 25:36:45.14 is 140 ms).
func clockMs(h, m, s, frac string) int64 {
	hh, _ := strconv.ParseInt(h, 10, 64)
	mm, _ := strconv.ParseInt(m, 10, 64)
	ss, _ := strconv.ParseInt(s, 10, 64)
	var ms int64
	if frac != "" {
		f, _ := strconv.ParseFloat("0."+frac, 64)
		ms = int64(f*1000 + 0.5)
	}
	return ((hh*60+mm)*60+ss)*1000 + ms
}
