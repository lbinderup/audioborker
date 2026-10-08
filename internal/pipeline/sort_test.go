package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"audioborker/internal/metadata"
	"audioborker/internal/store"
)

func TestSortFile(t *testing.T) {
	out := t.TempDir()
	write := func(rel string) string {
		t.Helper()
		p := filepath.Join(out, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(rel), 0o666); err != nil {
			t.Fatal(err)
		}
		return p
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	const tmpl = "{author}/{title}"
	book := metadata.Book{Title: "Small Gods", Authors: []string{"Terry Pratchett"}}

	// A forgotten file moves, its sidecar with it, and the emptied folder goes.
	src := write("Unsorted/new/sg.m4b")
	write("Unsorted/new/sg.chapters.txt")
	target, moved, err := SortFile(src, out, tmpl, book)
	want := filepath.Join(out, "Terry Pratchett", "Small Gods.m4b")
	if err != nil || !moved || target != want {
		t.Fatalf("SortFile = %q, %v, %v", target, moved, err)
	}
	if !exists(want) || !exists(sidecarFor(want)) || exists(src) || exists(filepath.Join(out, "Unsorted")) {
		t.Error("file, sidecar or empty folder not where they belong")
	}

	// Already in place: nothing to do.
	if _, inPlace, err := SortTarget(want, out, tmpl, book); err != nil || !inPlace {
		t.Errorf("in place = %v, %v", inPlace, err)
	}

	// Another file already at the target is never overwritten.
	other := write("Elsewhere/copy.m4b")
	if _, moved, err := SortFile(other, out, tmpl, book); err == nil || moved || !exists(other) {
		t.Errorf("collision moved = %v, err = %v", moved, err)
	}

	// No title, no way to place it.
	if _, _, err := SortTarget(other, out, tmpl, metadata.Book{Authors: []string{"A"}}); err == nil {
		t.Error("an untitled file was given a target")
	}
}

func TestRunSortUsesTheCheckedTags(t *testing.T) {
	out := t.TempDir()
	src := filepath.Join(out, "Unsorted", "sg.m4b")
	if err := os.MkdirAll(filepath.Dir(src), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	// No ffprobe exists here: the job must go by the tags it carries.
	rc := &RealConverter{FFprobe: filepath.Join(out, "no-ffprobe")}
	job := &store.Job{
		InputPath: "Unsorted/sg.m4b", SourceFiles: []string{src},
		Metadata: metadata.Book{Title: "Small Gods", Authors: []string{"Terry Pratchett"}},
		Options:  store.JobOptions{Kind: store.KindSort, OutputDir: out, PathTemplate: "{author}/{title}"},
	}
	res, err := rc.Run(context.Background(), job, func(string, float64) {}, func(string, ...any) {})
	want := filepath.Join(out, "Terry Pratchett", "Small Gods.m4b")
	if err != nil || res.OutputPath != want {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	// Again: now it is in place, and that's a success, not an error.
	job.SourceFiles = []string{want}
	if res, err := rc.Run(context.Background(), job, func(string, float64) {}, func(string, ...any) {}); err != nil || res.OutputPath != want {
		t.Fatalf("in place: %+v, %v", res, err)
	}
	// Without tags to go by it has to probe — and says so when it can't.
	job.Metadata = metadata.Book{}
	if _, err := rc.Run(context.Background(), job, func(string, float64) {}, func(string, ...any) {}); err == nil {
		t.Error("a job without tags ran without reading any")
	}
}
