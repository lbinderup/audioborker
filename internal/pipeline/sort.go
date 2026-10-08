package pipeline

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"audioborker/internal/metadata"
	"audioborker/internal/store"
)

// SortTarget is where sorting would put a library file: the path the template
// renders from book, the file's own tags. It is retagTarget's decision — the
// one a retag that moves makes — so a file already in place stays (inPlace)
// and a different file at the target is an error, never overwritten.
func SortTarget(src, outputDir, pathTemplate string, book metadata.Book) (target string, inPlace bool, err error) {
	if strings.TrimSpace(book.Title) == "" {
		return "", false, errors.New("no title tag to sort by")
	}
	opts := store.JobOptions{OutputDir: outputDir, PathTemplate: pathTemplate, Rename: true}
	target, _, err = retagTarget(src, opts, book, osPathState)
	if err != nil {
		return "", false, err
	}
	return target, target == src, nil
}

// SortFile moves a library file to its SortTarget, with its chapters.txt — a
// retag's rename without the retag, for files that were never moved. It only
// renames: the library is one volume, and a copy+delete fallback could leave
// half a book at either end. Folders the move empties are pruned.
func SortFile(src, outputDir, pathTemplate string, book metadata.Book) (target string, moved bool, err error) {
	target, inPlace, err := SortTarget(src, outputDir, pathTemplate, book)
	if err != nil || inPlace {
		return target, false, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
		return "", false, err
	}
	if err := os.Rename(src, target); err != nil {
		return "", false, fmt.Errorf("could not move it (is it open in a player?): %w", err)
	}
	// A case-only rename finds "its" sidecar already at the target spelling.
	if side := sidecarFor(src); fileExists(side) && !fileExists(sidecarFor(target)) {
		if err := os.Rename(side, sidecarFor(target)); err != nil {
			return target, true, fmt.Errorf("moved, but its chapters.txt stayed behind: %w", err)
		}
	}
	pruneEmptyParents(filepath.Dir(src), outputDir, func(string, ...any) {})
	return target, true, nil
}
