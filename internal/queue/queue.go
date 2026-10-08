// Package queue runs the persistent conversion queue: a pool of worker
// goroutines claiming pending jobs from the store, executing them through a
// pipeline.Converter, and broadcasting progress over the Broker.
package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"audioborker/internal/pipeline"
	"audioborker/internal/store"
)

// jobTimeout bounds a single conversion; even a 100-hour book transcodes
// well inside this on NAS hardware.
const jobTimeout = 6 * time.Hour

// progressThrottle limits SSE progress spam.
const progressThrottle = 500 * time.Millisecond

// Store is the subset of *store.Store the manager needs (interface for tests).
type Store interface {
	ClaimNextPending() (*store.Job, error)
	UpdateProgress(id, stage string, progress float64) error
	FinishJob(id, status, errMsg, outputPath, chaptersJSON string, warnings []string) error
	MarkInterrupted() (int64, error)
	CancelPending(id string) (bool, error)
	SetLogPath(id, path string) error
}

type Manager struct {
	store       Store
	conv        pipeline.Converter
	broker      *Broker
	logsDir     string
	concurrency int
	log         *slog.Logger

	wake    chan struct{}
	cancels sync.Map // job id -> context.CancelFunc
	wg      sync.WaitGroup
}

func NewManager(st Store, conv pipeline.Converter, broker *Broker, logsDir string, concurrency int, log *slog.Logger) *Manager {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Manager{
		store:       st,
		conv:        conv,
		broker:      broker,
		logsDir:     logsDir,
		concurrency: concurrency,
		log:         log,
		wake:        make(chan struct{}, 1),
	}
}

// Start reconciles interrupted jobs and launches the worker pool. Workers
// exit when ctx is cancelled; Wait blocks until they have drained.
func (m *Manager) Start(ctx context.Context) {
	if n, err := m.store.MarkInterrupted(); err != nil {
		m.log.Error("reconcile interrupted jobs", "err", err)
	} else if n > 0 {
		m.log.Warn("marked running jobs as interrupted after restart", "count", n)
	}
	for i := 0; i < m.concurrency; i++ {
		m.wg.Add(1)
		go m.worker(ctx, i)
	}
}

func (m *Manager) Wait() { m.wg.Wait() }

// Wake nudges the workers after new jobs are enqueued.
func (m *Manager) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
	m.broker.Publish(Event{Kind: EventStatus}) // queue-changed refresh
}

// Cancel stops a job: pending jobs are cancelled in the store, running jobs
// via their context. Returns an error if the job is already finished.
func (m *Manager) Cancel(id string) error {
	if ok, err := m.store.CancelPending(id); err != nil {
		return err
	} else if ok {
		m.broker.Publish(Event{Kind: EventStatus, JobID: id, Status: store.StatusCancelled})
		return nil
	}
	if c, ok := m.cancels.Load(id); ok {
		c.(context.CancelFunc)()
		return nil
	}
	return fmt.Errorf("job is not pending or running")
}

func (m *Manager) worker(ctx context.Context, n int) {
	defer m.wg.Done()
	tick := time.NewTicker(5 * time.Second) // safety net if a Wake is missed
	defer tick.Stop()
	for {
		job, err := m.store.ClaimNextPending()
		if err != nil {
			m.log.Error("claim job", "worker", n, "err", err)
		}
		if job != nil {
			m.run(ctx, job)
			continue // immediately look for the next one
		}
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-tick.C:
		}
	}
}

func (m *Manager) run(ctx context.Context, job *store.Job) {
	jobCtx, cancel := context.WithCancelCause(ctx)
	timeoutCtx, cancelTimeout := context.WithTimeout(jobCtx, jobTimeout)
	defer cancelTimeout()
	userCancelled := errors.New("cancelled by user")
	m.cancels.Store(job.ID, context.CancelFunc(func() { cancel(userCancelled) }))
	defer m.cancels.Delete(job.ID)

	logPath := filepath.Join(m.logsDir, job.ID+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o666)
	if err != nil {
		m.log.Error("open job log", "job", job.ID, "err", err)
	} else {
		defer logFile.Close()
		m.store.SetLogPath(job.ID, logPath)
		job.LogPath = logPath
	}

	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		if logFile != nil {
			fmt.Fprintf(logFile, "%s %s\n", time.Now().Format("15:04:05"), line)
		}
		m.broker.Publish(Event{Kind: EventLog, JobID: job.ID, Line: line})
	}

	var lastPub time.Time
	var lastStage string
	report := func(stage string, progress float64) {
		now := time.Now()
		if stage == lastStage && now.Sub(lastPub) < progressThrottle && progress < 1 {
			return
		}
		lastStage, lastPub = stage, now
		m.store.UpdateProgress(job.ID, stage, progress)
		m.broker.Publish(Event{Kind: EventProgress, JobID: job.ID, Stage: stage, Progress: progress})
	}

	m.log.Info("job started", "job", job.ID, "input", job.InputPath)
	m.broker.Publish(Event{Kind: EventStatus, JobID: job.ID, Status: store.StatusRunning})
	if job.ASIN != "" {
		logf("job %s started: %s (%s, region %s)", job.ID, job.InputPath, job.ASIN, job.Region)
	} else {
		logf("job %s started: %s", job.ID, job.InputPath)
	}

	result, err := m.conv.Run(timeoutCtx, job, report, logf)

	switch {
	case err == nil:
		m.store.FinishJob(job.ID, store.StatusDone, "", result.OutputPath, result.ChaptersJSON, result.Warnings)
		logf("job finished: %s", result.OutputPath)
		m.log.Info("job done", "job", job.ID, "output", result.OutputPath)
		m.broker.Publish(Event{Kind: EventStatus, JobID: job.ID, Status: store.StatusDone})
	case context.Cause(timeoutCtx) == userCancelled:
		m.store.FinishJob(job.ID, store.StatusCancelled, "cancelled by user", "", "", nil)
		logf("job cancelled")
		m.broker.Publish(Event{Kind: EventStatus, JobID: job.ID, Status: store.StatusCancelled})
	case errors.Is(timeoutCtx.Err(), context.DeadlineExceeded):
		m.store.FinishJob(job.ID, store.StatusError, fmt.Sprintf("timed out after %s", jobTimeout), "", "", nil)
		logf("job timed out")
		m.broker.Publish(Event{Kind: EventStatus, JobID: job.ID, Status: store.StatusError})
	case ctx.Err() != nil:
		// App is shutting down: put the job back the boot reconciler's way.
		m.store.FinishJob(job.ID, store.StatusError, "interrupted by shutdown", "", "", nil)
		m.broker.Publish(Event{Kind: EventStatus, JobID: job.ID, Status: store.StatusError})
	default:
		m.store.FinishJob(job.ID, store.StatusError, err.Error(), "", "", nil)
		logf("job failed: %v", err)
		m.log.Warn("job failed", "job", job.ID, "err", err)
		m.broker.Publish(Event{Kind: EventStatus, JobID: job.ID, Status: store.StatusError})
	}
}
