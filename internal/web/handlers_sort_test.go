package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"audioborker/internal/metadata"
	"audioborker/internal/store"
)

func TestSortApplyQueuesJobsWithTheCheckedTags(t *testing.T) {
	s := testServer(t)
	out := s.cfg.OutputDir
	write := func(rel string) string {
		p := filepath.Join(out, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(rel), 0o666); err != nil {
			t.Fatal(err)
		}
		return p
	}
	checked, unchecked := write("A/checked.m4b"), write("B/unchecked.m4b")
	info, _ := os.Stat(checked)
	s.sortChecks.put(checked, info, metadata.Book{Title: "Checked", Authors: []string{"Someone"}})

	form := url.Values{"paths": {"A/checked.m4b", "B/unchecked.m4b"}}
	req := httptest.NewRequest(http.MethodPost, "/library/sort/apply", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleLibrarySortApply(rec, req)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "/queue?queued=2") {
		t.Fatalf("redirect %q", loc)
	}

	jobs, _, err := s.store.ListJobs(0, 10)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs %v, %v", jobs, err)
	}
	byPath := map[string]*store.Job{}
	for _, j := range jobs {
		if !j.Options.IsSort() || j.Options.PathTemplate != s.settings().PathTemplate {
			t.Errorf("not a sort job snapshotting the template: %+v", j.Options)
		}
		byPath[j.InputPath] = j
	}
	if byPath["A/checked.m4b"].Metadata.Title != "Checked" {
		t.Error("the checked tags didn't travel with the job")
	}
	if byPath["B/unchecked.m4b"].Metadata.Title != "" || byPath["B/unchecked.m4b"].SourceFiles[0] != unchecked {
		t.Error("an unchecked file must leave the job to read its tags")
	}

	// A file changed since its check: the stale tags are not used.
	if err := os.WriteFile(checked, []byte("changed since"), 0o666); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(checked)
	if _, ok := s.sortChecks.get(checked, info); ok {
		t.Error("a changed file still matched its old check")
	}

	// Both are now queued; confirming again queues nothing twice.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/library/sort/apply", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleLibrarySortApply(rec, req)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "queued=0") {
		t.Errorf("re-queued: %q", loc)
	}
}
