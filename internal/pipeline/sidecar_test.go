package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"audioborker/internal/store"
)

func TestParseChaptersTxt(t *testing.T) {
	// m4b-tool's layout, as found next to Poseidon's Wake: a comment header,
	// unpadded hours, and fractions trimmed to two digits.
	m4bTool := "\xef\xbb\xbf# total-length 27:05:02.726\n0:00:00.000 Opening Credits\n0:00:35.665 Chapter One\n25:36:45.14 Chapter Fifty-Six\n"
	chs, err := parseChaptersTxt(m4bTool, 97_540_690)
	if err != nil || len(chs) != 3 {
		t.Fatalf("%v, %+v", err, chs)
	}
	if chs[1].StartMs != 35_665 || chs[1].EndMs != 92_205_140 || chs[2].StartMs != 92_205_140 || chs[2].EndMs != 97_540_690 {
		t.Errorf("%+v", chs)
	}
	if chs[2].Title != "Chapter Fifty-Six" {
		t.Errorf("title %q", chs[2].Title)
	}

	// This app's own sidecar reads back exactly.
	own := "00:00:00.000 Prologue\n01:02:03.004 One\n## total-duration: 02:00:00.000\n"
	chs, err = parseChaptersTxt(own, 7_200_000)
	if err != nil || len(chs) != 2 || chs[1].StartMs != 3_723_004 {
		t.Errorf("%v, %+v", err, chs)
	}

	// OGM style; a missing name gets a numbered one.
	ogm := "CHAPTER01=00:00:00.000\nCHAPTER01NAME=Intro\nCHAPTER02=00:10:00.500\n"
	chs, err = parseChaptersTxt(ogm, 3_600_000)
	if err != nil || len(chs) != 2 || chs[0].Title != "Intro" || chs[1].Title != "Chapter 02" || chs[1].StartMs != 600_500 {
		t.Errorf("%v, %+v", err, chs)
	}

	for name, text := range map[string]string{
		"out of order":   "00:10:00.000 B\n00:05:00.000 A\n",
		"past the audio": "00:00:00.000 A\n03:00:00.000 B\n",
	} {
		if _, err := parseChaptersTxt(text, 7_200_000); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestWithSidecarChapters(t *testing.T) {
	dir := t.TempDir()
	book := filepath.Join(dir, "Book.m4b")
	if err := os.WriteFile(sidecarFor(book), []byte("0:00:00.000 One\n0:30:00.000 Two\n1:00:00.000 Three\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if got := sidecarFor(filepath.Join(dir, "Book.m4a")); got != sidecarFor(book) {
		t.Errorf("sidecarFor ignores the extension: %s", got)
	}

	// No embedded chapters: the sidecar's are the book's own.
	none := &FileInfo{Path: book, DurationMs: 5_400_000}
	withSidecarChapters(none)
	if len(none.Chapters) != 3 || none.ChaptersFrom != "Book.chapters.txt" || none.Chapters[2].EndMs != 5_400_000 {
		t.Errorf("%+v", none)
	}

	// Embedded chapters cut short by an overflowed header give way too.
	cut := &FileInfo{Path: book, DurationMs: 5_400_000, ChaptersTruncated: true,
		Chapters: []ProbedChapter{{Title: "One", EndMs: 5_400_000}}}
	withSidecarChapters(cut)
	if len(cut.Chapters) != 3 || cut.ChaptersTruncated {
		t.Errorf("%+v", cut)
	}

	// A complete embedded set wins.
	full := &FileInfo{Path: book, DurationMs: 5_400_000, Chapters: []ProbedChapter{{Title: "Embedded", EndMs: 5_400_000}}}
	withSidecarChapters(full)
	if len(full.Chapters) != 1 || full.ChaptersFrom != "" {
		t.Errorf("%+v", full)
	}

	// A sidecar for a longer edition is ignored, and says why.
	short := &FileInfo{Path: book, DurationMs: 2_000_000}
	withSidecarChapters(short)
	if len(short.Chapters) != 0 || short.SidecarErr == "" {
		t.Errorf("%+v", short)
	}
}

func TestWritesSidecar(t *testing.T) {
	dir := t.TempDir()
	with, without := filepath.Join(dir, "With.m4b"), filepath.Join(dir, "Without.m4b")
	if err := os.WriteFile(sidecarFor(with), []byte("0:00:00.000 One\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		setting bool
		sources []string
		want    bool
	}{
		{"setting on", true, []string{without}, true},
		{"came with one", false, []string{with}, true},
		{"came without", false, []string{without}, false},
		{"several files: one per file isn't the book's", false, []string{with, without}, false},
	}
	for _, c := range cases {
		if got := writesSidecar(c.setting, c.sources); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

func TestCleanupTakesSidecars(t *testing.T) {
	write := func(p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	rc := &RealConverter{}
	job := func(in, rel, mode string, sources ...string) *store.Job {
		return &store.Job{ID: "0123456789abcdef", InputPath: rel, SourceFiles: sources,
			Options: store.JobOptions{InputDir: in, CleanupMode: mode, CompletedDir: filepath.Join(filepath.Dir(in), "done")}}
	}

	// A folder: the consumed file and its sidecar go, a stranger's file stays.
	in := filepath.Join(t.TempDir(), "in")
	book := filepath.Join(in, "Book", "Book.m4b")
	write(book)
	write(sidecarFor(book))
	write(filepath.Join(in, "Book", "notes.txt"))
	if err := rc.cleanupSource(job(in, "Book", "delete", book), func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if exists(book) || exists(sidecarFor(book)) || !exists(filepath.Join(in, "Book", "notes.txt")) {
		t.Error("delete: wrong files left behind")
	}

	// A single file selected on its own: moved with its sidecar.
	in = filepath.Join(t.TempDir(), "in")
	single := filepath.Join(in, "Single.m4b")
	write(single)
	write(sidecarFor(single))
	j := job(in, "Single.m4b", "move", single)
	if err := rc.cleanupSource(j, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(j.Options.CompletedDir, "Single.m4b")
	if exists(single) || exists(sidecarFor(single)) || !exists(moved) || !exists(sidecarFor(moved)) {
		t.Error("move: the sidecar didn't follow its file")
	}
}
