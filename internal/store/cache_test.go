package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"testing"

	"audioborker/internal/metadata"
)

func TestCacheIsPerSource(t *testing.T) {
	s := openTest(t)

	audnexus := &metadata.Book{ASIN: "B000000001", Title: "From Audnexus"}
	audible := &metadata.Book{ASIN: "B000000001", Title: "From Audible"}
	if err := s.CacheBook("audnexus", "B000000001", "us", audnexus); err != nil {
		t.Fatal(err)
	}
	if err := s.CacheBook("audible", "B000000001", "us", audible); err != nil {
		t.Fatal(err)
	}

	got, err := s.CachedBook("audnexus", "B000000001", "us")
	if err != nil || got == nil || got.Title != "From Audnexus" {
		t.Errorf("audnexus read = %+v, %v", got, err)
	}
	got, err = s.CachedBook("audible", "B000000001", "us")
	if err != nil || got == nil || got.Title != "From Audible" {
		t.Errorf("audible read = %+v, %v", got, err)
	}
	// A source without a record is a miss even when another source has one.
	got, err = s.CachedBook("other", "B000000001", "us")
	if err != nil || got != nil {
		t.Errorf("unknown source should miss, got %+v, %v", got, err)
	}

	// Chapters live in the same row per source and must not clobber books.
	ch := &metadata.ChapterInfo{ASIN: "B000000001", RuntimeMs: 1000}
	if err := s.CacheChapters("audnexus", "B000000001", "us", ch); err != nil {
		t.Fatal(err)
	}
	gotCh, err := s.CachedChapters("audnexus", "B000000001", "us")
	if err != nil || gotCh == nil || gotCh.RuntimeMs != 1000 {
		t.Errorf("chapters read = %+v, %v", gotCh, err)
	}
	if gotB, _ := s.CachedBook("audnexus", "B000000001", "us"); gotB == nil || gotB.Title != "From Audnexus" {
		t.Errorf("caching chapters clobbered the book: %+v", gotB)
	}
}

// TestMigrateCacheToPerSource builds a database frozen at schema v1 (the
// pre-aggregation single-source cache) and verifies that opening it migrates
// existing rows to source='audnexus' without losing data (0002), and that the
// cached book — but not the chapters — is then dropped for re-fetching, since
// it predates the genre hierarchy (0003).
func TestMigrateCacheToPerSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	init1, err := migrationsFS.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	dsn := fmt.Sprintf("file:%s", url.PathEscape(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(init1)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		"INSERT INTO metadata_cache (asin, region, book_json, chapters_json, fetched_at) VALUES (?, ?, ?, ?, ?)",
		"B000000001", "us", `{"asin":"B000000001","title":"Old Row"}`,
		`{"asin":"B000000001","chapters":[{"title":"Opening","start_ms":0,"length_ms":1000}]}`, 42,
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ch, err := s.CachedChapters("audnexus", "B000000001", "us")
	if err != nil || ch == nil || len(ch.Chapters) != 1 || ch.Chapters[0].Title != "Opening" {
		t.Errorf("migrated chapters not readable as audnexus: %+v, %v", ch, err)
	}
	if got, err := s.CachedBook("audnexus", "B000000001", "us"); err != nil || got != nil {
		t.Errorf("pre-genre-hierarchy book should be a cache miss after 0003, got %+v, %v", got, err)
	}
	var fetched int64
	if err := s.db.QueryRow("SELECT fetched_at FROM metadata_cache WHERE source='audnexus' AND asin='B000000001'").Scan(&fetched); err != nil || fetched != 42 {
		t.Errorf("fetched_at not preserved: %d, %v", fetched, err)
	}
}
