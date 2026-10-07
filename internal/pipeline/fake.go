package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"audioborker/internal/metadata"
	"audioborker/internal/pathtmpl"
	"audioborker/internal/store"
)

// FakeConverter simulates a conversion for UI development: it walks the real
// stages with plausible timing, emits log lines and progress, honors
// cancellation, and fails deterministically when the input path contains
// "fail" (so error UX is testable).
type FakeConverter struct {
	// StageDelay scales the simulation; default ~8s total.
	StageDelay time.Duration
}

func (f *FakeConverter) delay() time.Duration {
	if f.StageDelay > 0 {
		return f.StageDelay
	}
	if ms, err := strconv.Atoi(os.Getenv("FAKE_STAGE_MS")); err == nil && ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return 700 * time.Millisecond
}

func (f *FakeConverter) Run(ctx context.Context, job *store.Job, report ProgressFunc, logf LogFunc) (*Result, error) {
	if job.Options.IsRetag() {
		return f.runRetag(ctx, job, report, logf)
	}
	logf("fake converter: starting %s (asin %s)", job.InputPath, job.ASIN)

	type step struct {
		stage string
		until float64
		ticks int
	}
	steps := []step{
		{"probe", 0.05, 1}, {"plan", 0.10, 1}, {"merge", 0.90, 10},
		{"chapters", 0.93, 1}, {"tag", 0.96, 1}, {"verify", 0.98, 1},
		{"move", 0.99, 1}, {"cleanup", 1.0, 1},
	}
	last := 0.0
	for _, st := range steps {
		logf("stage: %s", st.stage)
		for i := 1; i <= st.ticks; i++ {
			select {
			case <-ctx.Done():
				logf("cancelled during %s", st.stage)
				return nil, ctx.Err()
			case <-time.After(f.delay() / time.Duration(st.ticks)):
			}
			p := last + (st.until-last)*float64(i)/float64(st.ticks)
			report(st.stage, p)
			if st.stage == "merge" {
				logf("  time=00:%02d:00.00 bitrate=128.0kbits/s", i*7)
			}
		}
		last = st.until
		if st.stage == "merge" && strings.Contains(strings.ToLower(job.InputPath), "fail") {
			logf("simulated ffmpeg failure")
			return nil, fmt.Errorf("simulated merge failure (input path contains \"fail\")")
		}
	}

	rel, err := pathtmpl.Render(job.Options.PathTemplate, varsFor(job))
	if err != nil {
		return nil, err
	}
	out := path.Join(job.Options.OutputDir, rel+".m4b")
	chapters, _ := json.Marshal(map[string]any{
		"source": "fake",
		"chapters": []metadata.Chapter{
			{Title: "Opening Credits", StartMs: 0, LengthMs: 11264},
			{Title: "Chapter 1", StartMs: 11264, LengthMs: 2203838},
		},
	})
	logf("fake converter: wrote %s", out)
	return &Result{OutputPath: out, ChaptersJSON: string(chapters)}, nil
}

// runRetag simulates a library retag. Keeping a fake counterpart is what lets
// the whole Library UX be exercised on a machine without ffmpeg/tone, the same
// reason FakeConverter exists at all.
func (f *FakeConverter) runRetag(ctx context.Context, job *store.Job, report ProgressFunc, logf LogFunc) (*Result, error) {
	logf("fake retag: starting %s (asin %s)", job.InputPath, job.ASIN)

	target := job.InputPath
	if len(job.SourceFiles) == 1 {
		target = job.SourceFiles[0]
	}
	if job.Options.Rename {
		if rel, err := pathtmpl.Render(job.Options.PathTemplate, varsFor(job)); err == nil {
			target = path.Join(job.Options.OutputDir, rel+".m4b")
		}
	}

	type step struct {
		stage string
		until float64
	}
	// No merge: a retag copies, tags and swaps. Verify precedes replace here
	// exactly as it does in the real runner.
	steps := []step{
		{"probe", 0.10}, {"plan", 0.20}, {"copy", 0.60},
		{"chapters", 0.72}, {"tag", 0.85}, {"verify", 0.93}, {"replace", 1.0},
	}
	for _, st := range steps {
		logf("stage: %s", st.stage)
		select {
		case <-ctx.Done():
			logf("cancelled during %s — the library file is untouched", st.stage)
			return nil, ctx.Err()
		case <-time.After(f.delay()):
		}
		report(st.stage, st.until)
		if st.stage == "tag" && strings.Contains(strings.ToLower(job.InputPath), "fail") {
			logf("simulated tone failure")
			return nil, fmt.Errorf("simulated tag failure (path contains \"fail\")")
		}
	}

	chapters, _ := json.Marshal(map[string]any{
		"source": "fake",
		"chapters": []metadata.Chapter{
			{Title: "Chapter 1", StartMs: 0, LengthMs: 1_200_000},
			{Title: "Chapter 2", StartMs: 1_200_000, LengthMs: 1_200_000},
		},
	})
	logf("fake retag: rewrote %s", target)
	return &Result{OutputPath: target, ChaptersJSON: string(chapters)}, nil
}

// varsFor maps a job's metadata snapshot to path-template variables.
// Shared by real and fake converters.
func varsFor(job *store.Job) pathtmpl.Vars { return PathVars(job.Metadata) }

// PathVars maps a book to path-template variables. Exported so the Library's
// rename preview derives the new path through exactly the code the retag will
// run — the same single-source-of-truth rule PlanChapters follows for chapters.
func PathVars(book metadata.Book) pathtmpl.Vars {
	v := pathtmpl.Vars{
		Title:          book.Title,
		Subtitle:       book.Subtitle,
		SeriesName:     book.SeriesName,
		SeriesPosition: book.SeriesPosition,
		Year:           book.Year,
		ASIN:           book.ASIN,
	}
	if len(book.Authors) > 0 {
		v.Author = book.Authors[0]
	}
	if len(book.Narrators) > 0 {
		v.Narrator = book.Narrators[0]
	}
	return v
}
