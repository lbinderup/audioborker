package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"audioborker/internal/metadata"
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
