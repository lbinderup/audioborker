// Package pipeline converts one queued job into a finished m4b. The
// Converter interface is the seam between the queue and the actual work:
// the real implementation drives ffmpeg + tone, the fake one simulates a
// conversion so the whole web UX can be exercised on a machine without
// those binaries.
package pipeline

import (
	"context"

	"audioborker/internal/store"
)

// Stages in execution order; the UI shows these as the job's current step.
var Stages = []string{"probe", "plan", "merge", "chapters", "tag", "verify", "move", "cleanup"}

// Result is what a successful conversion reports back.
type Result struct {
	OutputPath   string   // final m4b location
	ChaptersJSON string   // chapters actually embedded, incl. their source
	Warnings     []string // user-facing, e.g. "Audnexus chapter runtime off by 3%"
}

// ProgressFunc reports cosmetic progress; stage is one of Stages and
// progress is 0..1 across the whole job.
type ProgressFunc func(stage string, progress float64)

// LogFunc appends one line to the job log (also streamed to the UI).
type LogFunc func(format string, args ...any)

type Converter interface {
	// Run performs the conversion. Cancellation arrives via ctx; a cancelled
	// run returns ctx.Err(). Run must be safe to call from multiple
	// goroutines with different jobs.
	Run(ctx context.Context, job *store.Job, report ProgressFunc, logf LogFunc) (*Result, error)
}
