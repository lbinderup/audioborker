package store

import (
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrateIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s1.Close()
	s2, err := Open(path) // second open re-runs migrate; must be a no-op
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
}

func TestSettingsRoundTrip(t *testing.T) {
	s := openTest(t)
	def := Defaults("/input", "/output")

	got, err := s.GetSettings(def)
	if err != nil {
		t.Fatal(err)
	}
	if got != def {
		t.Errorf("empty db should return defaults, got %+v", got)
	}

	want := def
	want.OutputDir = "/mnt/audiobooks"
	want.CleanupMode = "move"
	want.CompletedSubdir = "audiobooks/done"
	want.WorkerConcurrency = 2
	want.BitrateKbps = 128
	want.WriteChaptersTxt = false
	want.RetagMoveDefault = false // on by default, so off must survive a save
	if err := s.SaveSettings(want); err != nil {
		t.Fatal(err)
	}

	got, err = s.GetSettings(def)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestSaveSettingsRejectsInvalid(t *testing.T) {
	s := openTest(t)
	bad := Defaults("/input", "/output")
	bad.CleanupMode = "move"
	bad.CompletedSubdir = "/" // absolute: would target the container filesystem
	if err := s.SaveSettings(bad); err == nil {
		t.Error("absolute completed subfolder was accepted")
	}

	bad2 := Defaults("/input", "/output")
	bad2.PathTemplate = "{nope}"
	if err := s.SaveSettings(bad2); err == nil {
		t.Error("unknown template token was accepted")
	}
}

func TestJobLifecycle(t *testing.T) {
	s := openTest(t)

	job := &Job{InputPath: "Some Book", ASIN: "B000000001", Region: "us"}
	if err := s.CreateJob(job); err != nil {
		t.Fatal(err)
	}

	// Claim flips pending -> running.
	claimed, err := s.ClaimNextPending()
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if claimed.ID != job.ID || claimed.Status != StatusRunning || claimed.StartedAt.IsZero() {
		t.Errorf("claimed = %+v", claimed)
	}
	// Nothing else pending.
	if again, _ := s.ClaimNextPending(); again != nil {
		t.Errorf("claimed a second job: %+v", again)
	}

	// Boot reconciliation flips running -> error.
	n, err := s.MarkInterrupted()
	if err != nil || n != 1 {
		t.Fatalf("MarkInterrupted = %d, %v", n, err)
	}
	got, _ := s.GetJob(job.ID)
	if got.Status != StatusError || got.Error != "interrupted by restart" || got.FinishedAt.IsZero() {
		t.Errorf("after reconcile: %+v", got)
	}

	// Retry clones to a fresh pending job.
	clone, err := s.CloneForRetry(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if clone.ID == job.ID || clone.RetriedFrom != job.ID {
		t.Errorf("clone = %+v", clone)
	}
	if ok, _ := s.HasActiveJobForPath(KindConvert, "Some Book"); !ok {
		t.Error("clone should count as active for its path")
	}

	// Pending jobs cancel directly.
	if ok, err := s.CancelPending(clone.ID); err != nil || !ok {
		t.Fatalf("CancelPending = %v, %v", ok, err)
	}
	// Terminal jobs clear from history.
	logs, err := s.ClearHistory()
	if err != nil {
		t.Fatal(err)
	}
	_ = logs
	if _, total, _ := s.ListJobs(0, 10); total != 0 {
		t.Errorf("history not cleared, %d jobs left", total)
	}
}

func TestRetagKindSurvivesRetry(t *testing.T) {
	s := openTest(t)

	job := &Job{
		InputPath: "Author/Book/Book [B000000001].m4b",
		ASIN:      "B000000001", Region: "us",
		Options: JobOptions{Kind: KindRetag, Rename: true, InputDir: "/output"},
	}
	if err := s.CreateJob(job); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Options.IsRetag() || !got.Options.Rename {
		t.Fatalf("kind/rename did not round-trip: %+v", got.Options)
	}

	// A retried retag must stay a retag: running a conversion against a
	// library path would merge the file over itself.
	s.FinishJob(job.ID, StatusError, "boom", "", "", nil)
	clone, err := s.CloneForRetry(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !clone.Options.IsRetag() || !clone.Options.Rename {
		t.Errorf("retry lost the retag kind: %+v", clone.Options)
	}
}

func TestHasActiveJobForPathDiscriminatesByKind(t *testing.T) {
	s := openTest(t)

	// The same relative path under two roots is two different files.
	const shared = "Author/Book.m4b"
	if err := s.CreateJob(&Job{InputPath: shared}); err != nil { // convert
		t.Fatal(err)
	}
	if ok, err := s.HasActiveJobForPath(KindConvert, shared); err != nil || !ok {
		t.Errorf("convert job not found: %v %v", ok, err)
	}
	if ok, err := s.HasActiveJobForPath(KindRetag, shared); err != nil || ok {
		t.Errorf("a convert job must not block a retag of the same relative path: %v %v", ok, err)
	}

	if err := s.CreateJob(&Job{InputPath: shared, Options: JobOptions{Kind: KindRetag}}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.HasActiveJobForPath(KindRetag, shared); !ok {
		t.Error("retag job not found by its own kind")
	}
}

func TestValidateCompletedSubdir(t *testing.T) {
	valid := []string{"", "done", "audiobooks/done", "a/b/c", "with space"}
	for _, v := range valid {
		if err := ValidateCompletedSubdir(v); err != nil {
			t.Errorf("ValidateCompletedSubdir(%q) rejected: %v", v, err)
		}
	}
	// Absolute paths and escapes must be refused — these are the ones that
	// would write into the container's own filesystem.
	invalid := []string{"/", "/completed", "/etc/passwd", "..", "../../etc", `C:\temp`, `sub\dir`, "."}
	for _, v := range invalid {
		if err := ValidateCompletedSubdir(v); err == nil {
			t.Errorf("ValidateCompletedSubdir(%q) was accepted", v)
		}
	}
}

func TestCompletedPath(t *testing.T) {
	root := filepath.FromSlash("/completed")
	cases := map[string]string{
		"":           filepath.Clean(root),
		"done":       filepath.Join(root, "done"),
		"a/b":        filepath.Join(root, "a", "b"),
		"./nested/x": filepath.Join(root, "nested", "x"),
	}
	for sub, want := range cases {
		got := Settings{CompletedSubdir: sub}.CompletedPath(root)
		if got != want {
			t.Errorf("CompletedPath(%q) = %q, want %q", sub, got, want)
		}
	}
}

func TestValidateCollectsAllErrors(t *testing.T) {
	set := Settings{} // everything empty/invalid
	errs := set.Validate()
	if len(errs) < 5 {
		t.Errorf("expected many errors for zero-value settings, got %d: %v", len(errs), errs)
	}
}
