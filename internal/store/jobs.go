package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"audioborker/internal/metadata"
)

// Job statuses.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusDone      = "done"
	StatusError     = "error"
	StatusCancelled = "cancelled"
)

// Job kinds. A convert job merges source files into a new m4b; a retag job
// rewrites the tags and chapters of an m4b already in the library; a sort job
// only moves a library file to the path its own tags render to. The kind
// lives in JobOptions rather than its own column because options_json is
// exactly the snapshot of "what the pipeline should do" — and because
// CloneForRetry copies Options verbatim, a retried retag stays a retag with no
// extra bookkeeping.
const (
	KindConvert = "" // zero value: every job queued before the Library existed
	KindRetag   = "retag"
	KindSort    = "sort"
)

// JobOptions is the per-job snapshot of every setting the pipeline consults.
// Snapshotting makes retries reproducible and isolates running jobs from
// settings edits.
type JobOptions struct {
	Kind string `json:"kind,omitempty"` // "" = convert
	// InputDir is the root Job.InputPath is relative to: the import volume for
	// a conversion, the library (output volume) for a retag.
	InputDir         string `json:"input_dir"`
	OutputDir        string `json:"output_dir"`
	CompletedDir     string `json:"completed_dir"`
	CleanupMode      string `json:"cleanup_mode"`
	PathTemplate     string `json:"path_template"`
	BitrateKbps      int    `json:"bitrate_kbps"`
	Encoder          string `json:"encoder"`
	WriteChaptersTxt bool   `json:"write_chapters_txt"`
	AudnexusURL      string `json:"audnexus_url"`
	ChapterMode      string `json:"chapter_mode"` // auto|existing|provider ("" = auto)
	// ChapterShift offsets the embedded chapters' timings to match this rip —
	// the catalog's chapters or the file's own, fixed or interpolated between
	// two anchor chapters. Ignored for file-boundary chapters.
	ChapterShift metadata.ShiftSpec `json:"chapter_shift,omitempty"`
	// ChapterShiftMs is the legacy fixed-only field, still read so old jobs
	// retry identically.
	ChapterShiftMs int64 `json:"chapter_shift_ms,omitempty"`
	// Rename lets a retag job also move the file to the path the snapshotted
	// template renders. Off means only the atoms change.
	Rename bool `json:"rename,omitempty"`
	// CoverFile is an image the user chose by hand (manual tagging), stored
	// under the config dir's covers/ folder. It wins over Metadata.CoverURL.
	CoverFile string `json:"cover_file,omitempty"`
}

// IsRetag reports whether this job rewrites a library file in place rather
// than producing a new one.
func (o JobOptions) IsRetag() bool { return o.Kind == KindRetag }

// IsSort reports whether this job only moves a library file.
func (o JobOptions) IsSort() bool { return o.Kind == KindSort }

// EffectiveChapterShift merges the current spec with the legacy field.
func (o JobOptions) EffectiveChapterShift() metadata.ShiftSpec {
	if o.ChapterShift.Mode != "" {
		return o.ChapterShift
	}
	if o.ChapterShiftMs != 0 {
		return metadata.ShiftSpec{Mode: "fixed", FixedMs: o.ChapterShiftMs}
	}
	return metadata.ShiftSpec{}
}

type Job struct {
	ID          string
	Status      string
	Stage       string
	Progress    float64
	InputPath   string // relative to the snapshotted input dir
	SourceFiles []string
	ASIN        string
	Region      string
	Metadata    metadata.Book
	Options     JobOptions
	ChaptersRaw string // JSON written by the pipeline
	OutputPath  string
	Error       string
	Warnings    []string
	RetriedFrom string
	LogPath     string
	CreatedAt   time.Time
	StartedAt   time.Time // zero if never started
	FinishedAt  time.Time // zero if not finished
}

func NewJobID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Store) CreateJob(j *Job) error {
	if j.ID == "" {
		j.ID = NewJobID()
	}
	if j.Status == "" {
		j.Status = StatusPending
	}
	if j.CreatedAt.IsZero() {
		j.CreatedAt = time.Now()
	}
	files, _ := json.Marshal(j.SourceFiles)
	meta, _ := json.Marshal(j.Metadata)
	opts, _ := json.Marshal(j.Options)
	warns, _ := json.Marshal(emptyIfNil(j.Warnings))
	_, err := s.db.Exec(`
		INSERT INTO jobs (id, status, stage, progress, input_path, source_files, asin, region,
		                  metadata_json, options_json, chapters_json, output_path, error, warnings,
		                  retried_from, log_path, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.ID, j.Status, j.Stage, j.Progress, j.InputPath, string(files), j.ASIN, j.Region,
		string(meta), string(opts), j.ChaptersRaw, j.OutputPath, j.Error, string(warns),
		j.RetriedFrom, j.LogPath, j.CreatedAt.UnixMilli())
	return err
}

const jobCols = `id, status, stage, progress, input_path, source_files, asin, region,
	metadata_json, options_json, chapters_json, output_path, error, warnings,
	retried_from, log_path, created_at, started_at, finished_at`

func scanJob(row interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	var files, meta, opts, warns string
	var created int64
	var started, finished sql.NullInt64
	err := row.Scan(&j.ID, &j.Status, &j.Stage, &j.Progress, &j.InputPath, &files, &j.ASIN, &j.Region,
		&meta, &opts, &j.ChaptersRaw, &j.OutputPath, &j.Error, &warns,
		&j.RetriedFrom, &j.LogPath, &created, &started, &finished)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(files), &j.SourceFiles)
	json.Unmarshal([]byte(meta), &j.Metadata)
	json.Unmarshal([]byte(opts), &j.Options)
	json.Unmarshal([]byte(warns), &j.Warnings)
	j.CreatedAt = time.UnixMilli(created)
	if started.Valid {
		j.StartedAt = time.UnixMilli(started.Int64)
	}
	if finished.Valid {
		j.FinishedAt = time.UnixMilli(finished.Int64)
	}
	return &j, nil
}

func (s *Store) GetJob(id string) (*Job, error) {
	return scanJob(s.db.QueryRow("SELECT "+jobCols+" FROM jobs WHERE id = ?", id))
}

// ClaimNextPending atomically flips the oldest pending job to running and
// returns it, or nil when the queue is empty.
func (s *Store) ClaimNextPending() (*Job, error) {
	var job *Job
	err := s.inTx(func(tx *sql.Tx) error {
		row := tx.QueryRow("SELECT " + jobCols + " FROM jobs WHERE status = 'pending' ORDER BY created_at, id LIMIT 1")
		j, err := scanJob(row)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		now := time.Now()
		if _, err := tx.Exec("UPDATE jobs SET status = ?, started_at = ? WHERE id = ?",
			StatusRunning, now.UnixMilli(), j.ID); err != nil {
			return err
		}
		j.Status = StatusRunning
		j.StartedAt = now
		job = j
		return nil
	})
	return job, err
}

// UpdateProgress records the pipeline's cosmetic progress state.
func (s *Store) UpdateProgress(id, stage string, progress float64) error {
	_, err := s.db.Exec("UPDATE jobs SET stage = ?, progress = ? WHERE id = ?", stage, progress, id)
	return err
}

// FinishJob records a terminal state.
func (s *Store) FinishJob(id, status, errMsg, outputPath, chaptersJSON string, warnings []string) error {
	warns, _ := json.Marshal(emptyIfNil(warnings))
	_, err := s.db.Exec(`
		UPDATE jobs SET status = ?, error = ?, output_path = ?, chapters_json = ?,
		                warnings = ?, finished_at = ?, progress = CASE WHEN ? = 'done' THEN 1.0 ELSE progress END
		WHERE id = ?`,
		status, errMsg, outputPath, chaptersJSON, string(warns), time.Now().UnixMilli(), status, id)
	return err
}

// MarkInterrupted flips every 'running' job to error at boot — nothing can
// still be running if the process just started. (bragibooks left these
// "Processing" forever, issues #191/#162.)
func (s *Store) MarkInterrupted() (int64, error) {
	res, err := s.db.Exec(`
		UPDATE jobs SET status = ?, error = 'interrupted by restart', finished_at = ?
		WHERE status = ?`, StatusError, time.Now().UnixMilli(), StatusRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CancelPending cancels a job that has not started; returns false if the job
// was not pending (running jobs are cancelled via context instead).
func (s *Store) CancelPending(id string) (bool, error) {
	res, err := s.db.Exec(`
		UPDATE jobs SET status = ?, finished_at = ? WHERE id = ? AND status = ?`,
		StatusCancelled, time.Now().UnixMilli(), id, StatusPending)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CloneForRetry inserts a fresh pending copy of a terminal job.
func (s *Store) CloneForRetry(id string) (*Job, error) {
	j, err := s.GetJob(id)
	if err != nil {
		return nil, err
	}
	if j.Status != StatusError && j.Status != StatusCancelled {
		return nil, fmt.Errorf("job is %s; only failed or cancelled jobs can be retried", j.Status)
	}
	clone := &Job{
		InputPath:   j.InputPath,
		SourceFiles: j.SourceFiles,
		ASIN:        j.ASIN,
		Region:      j.Region,
		Metadata:    j.Metadata,
		Options:     j.Options,
		RetriedFrom: j.ID,
	}
	if err := s.CreateJob(clone); err != nil {
		return nil, err
	}
	return clone, nil
}

// HasActiveJobForPath reports whether a pending or running job of the same
// kind already covers this input path — guards against double-queueing the
// same book. The kind matters because a conversion's path is relative to the
// import volume while a retag's is relative to the library, so the same string
// can legitimately name two different files.
func (s *Store) HasActiveJobForPath(kind, inputPath string) (bool, error) {
	var n int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM jobs
		WHERE input_path = ? AND status IN ('pending','running')
		  AND COALESCE(json_extract(options_json, '$.kind'), '') = ?`,
		inputPath, kind).Scan(&n)
	return n > 0, err
}

// ListJobs returns one page of jobs, newest first, plus the total count.
func (s *Store) ListJobs(offset, limit int) ([]*Job, int, error) {
	var total int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM jobs").Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query("SELECT "+jobCols+" FROM jobs ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?", limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var jobs []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, 0, err
		}
		jobs = append(jobs, j)
	}
	return jobs, total, rows.Err()
}

// CountByStatus powers the queue tab badges.
func (s *Store) CountByStatus() (map[string]int, error) {
	rows, err := s.db.Query("SELECT status, COUNT(*) FROM jobs GROUP BY status")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// ClearHistory deletes terminal jobs and returns their log paths so the
// caller can remove the files.
func (s *Store) ClearHistory() ([]string, error) {
	rows, err := s.db.Query(`SELECT log_path FROM jobs WHERE status IN ('done','error','cancelled') AND log_path != ''`)
	if err != nil {
		return nil, err
	}
	var logs []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err == nil && p != "" {
			logs = append(logs, p)
		}
	}
	rows.Close()
	if _, err := s.db.Exec(`DELETE FROM jobs WHERE status IN ('done','error','cancelled')`); err != nil {
		return nil, err
	}
	return logs, nil
}

// CoverFilesInUse returns the hand-chosen cover images (JobOptions.CoverFile)
// that jobs still reference, so unreferenced uploads can be removed.
func (s *Store) CoverFilesInUse() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT options_json FROM jobs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	used := map[string]bool{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var o JobOptions
		if json.Unmarshal([]byte(raw), &o) == nil && o.CoverFile != "" {
			used[o.CoverFile] = true
		}
	}
	return used, rows.Err()
}

// SetLogPath records where the job's log file lives.
func (s *Store) SetLogPath(id, path string) error {
	_, err := s.db.Exec("UPDATE jobs SET log_path = ? WHERE id = ?", path, id)
	return err
}

func emptyIfNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}
