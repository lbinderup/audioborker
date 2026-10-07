package web

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"audioborker/internal/config"
	"audioborker/internal/pipeline"
	"audioborker/internal/queue"
	"audioborker/internal/store"
)

// testServer is a Server over a temp config dir and an import volume holding
// one book, "Some Book/part1.mp3". The queue is never started: jobs only land
// in the store.
func testServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{
		ConfigDir: filepath.Join(dir, "config"),
		InputDir:  filepath.Join(dir, "input"),
		OutputDir: filepath.Join(dir, "output"),
	}
	for _, d := range []string{cfg.ConfigDir, filepath.Join(cfg.InputDir, "Some Book"), cfg.OutputDir} {
		if err := os.MkdirAll(d, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cfg.InputDir, "Some Book", "part1.mp3"), []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	broker := queue.NewBroker()
	return &Server{
		cfg:       cfg,
		store:     st,
		render:    newRenderer(false),
		queue:     queue.NewManager(st, &pipeline.FakeConverter{}, broker, cfg.LogsDir(), 1, slog.Default()),
		broker:    broker,
		durations: newDurationCache(),
	}
}

func postQueue(t *testing.T, s *Server, form url.Values) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/queue", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleQueueCreate(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d", rec.Code)
	}
	return rec.Header().Get("Location")
}

// manualItem posts one hand-tagged item the way the match form does.
func manualItem(fields map[string]string) url.Values {
	const p = "Some Book"
	form := url.Values{"paths": {p}, "match:" + p: {manualChoice}}
	for k, v := range fields {
		form.Set("manual:"+k+":"+p, v)
	}
	return form
}

func TestQueueManualChoice(t *testing.T) {
	s := testServer(t)
	const id = "6f1c2a7e-0b8d-4c55-9e0e-3d2b1a4c5f60"
	loc := postQueue(t, s, manualItem(map[string]string{
		"title": "Mockingjay", "authors": "Suzanne Collins", "narrators": "Carolyn McCormick",
		"series_name": "The Hunger Games", "series_position": "3", "release": "2010-08-24",
		"asin": "b003zxvc9i", "local_id": id,
		"cover": "edition:https://m.media-amazon.com/images/I/51abc.jpg",
	}))
	if !strings.Contains(loc, "queued=1") {
		t.Fatalf("redirect %q, want one queued", loc)
	}
	jobs, _, err := s.store.ListJobs(0, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs %v, %v", jobs, err)
	}
	j := jobs[0]
	m := j.Metadata
	if m.Title != "Mockingjay" || m.SeriesPosition != "3" || m.ReleaseDate != "2010-08-24" || m.LocalID != id {
		t.Errorf("snapshot = %+v", m)
	}
	if m.CoverURL != "https://m.media-amazon.com/images/I/51abc.jpg" || j.Options.CoverFile != "" {
		t.Errorf("cover = %q / %q", m.CoverURL, j.Options.CoverFile)
	}
	// An ASIN gets Audible's chapters, looked up in the default region.
	if j.ASIN != "B003ZXVC9I" || j.Region != s.settings().RegionDefault {
		t.Errorf("ASIN/region = %q/%q", j.ASIN, j.Region)
	}
}

func TestQueueManualChoiceInvalidIsSkipped(t *testing.T) {
	s := testServer(t)
	loc := postQueue(t, s, manualItem(map[string]string{"authors": "Someone", "cover": "file"}))
	if !strings.Contains(loc, "queued=0") || !strings.Contains(loc, url.QueryEscape("Title is required.")) {
		t.Fatalf("redirect %q, want the item skipped for its missing title", loc)
	}
}

func TestQueueManualUploadedCover(t *testing.T) {
	s := testServer(t)
	const name = "0123456789abcdef0123456789abcdef.jpg"
	postQueue(t, s, manualItem(map[string]string{
		"title": "T", "authors": "A", "cover": "upload", "cover_name": name,
	}))
	jobs, _, _ := s.store.ListJobs(0, 10)
	if len(jobs) != 1 || jobs[0].Options.CoverFile != filepath.Join(s.cfg.CoversDir(), name) {
		t.Fatalf("jobs = %+v", jobs)
	}
	if jobs[0].ASIN != "" || jobs[0].Region != "" {
		t.Errorf("no ASIN must mean no catalog lookup, got %q/%q", jobs[0].ASIN, jobs[0].Region)
	}
}

func TestChoiceASIN(t *testing.T) {
	s := testServer(t)
	region := s.settings().RegionDefault
	cases := []struct {
		name             string
		q                url.Values
		asin, wantRegion string
		ok               bool
	}{
		{"catalog pick", url.Values{"choice": {"B0ABCDEFGH|uk"}}, "B0ABCDEFGH", "uk", true},
		{"nothing chosen", url.Values{}, "", "", false},
		{"manual with ASIN", url.Values{"choice": {"manual"}, "path": {"A/B"}, "manual:asin:A/B": {" b0abcdefgh "}},
			"B0ABCDEFGH", region, true},
		{"manual without ASIN", url.Values{"choice": {"manual"}, "path": {"A/B"}}, "", "", false},
		{"another item's ASIN", url.Values{"choice": {"manual"}, "path": {"A/B"}, "manual:asin:C": {"B0ABCDEFGH"}},
			"", "", false},
	}
	for _, c := range cases {
		asin, reg, ok := s.choiceASIN(c.q.Get)
		if asin != c.asin || reg != c.wantRegion || ok != c.ok {
			t.Errorf("%s: got %q %q %v", c.name, asin, reg, ok)
		}
	}
}
