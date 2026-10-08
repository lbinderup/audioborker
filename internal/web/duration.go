package web

import (
	"context"
	"fmt"
	"os"
	"sync"

	"audioborker/internal/pipeline"
	"audioborker/internal/scan"
)

// durationCache memoizes the measured length of a selection. Probing costs
// one ffprobe per file, and the match screen asks repeatedly (every search,
// every re-render), so results are kept until the files change.
type durationCache struct {
	mu sync.Mutex
	m  map[string]durationEntry
}

type durationEntry struct {
	sig string // changes when the underlying files do
	localAudio
}

// localAudio is what the match screen measures of a selection's files.
type localAudio struct {
	totalMs    int64
	files      int
	overflowed int   // files whose 32-bit length header overflowed
	headerMs   int64 // what the first of those claims
}

// overflowNote reports files whose length header overflowed (see
// pipeline.unwrapDuration), so a batch hit by it stands out on the match
// screen. Players read those headers, so such a file shows a few minutes.
func (a localAudio) overflowNote() string {
	switch {
	case a.overflowed == 0:
		return ""
	case a.files == 1:
		return "Length header overflowed: the file claims " + msClock(a.headerMs) + " (fixed in the output)"
	}
	return fmt.Sprintf("Length headers overflowed in %d of %d files (fixed in the output)", a.overflowed, a.files)
}

func newDurationCache() *durationCache {
	return &durationCache{m: map[string]durationEntry{}}
}

// LocalRuntimeMs returns the combined duration of everything that would be
// merged for this selection under root. It returns 0 when the duration can't
// be determined — callers treat that as "unknown" rather than failing.
func (s *Server) LocalRuntimeMs(ctx context.Context, root, rel string) int64 {
	return s.localAudio(ctx, root, rel).totalMs
}

// localAudio measures a selection's files, memoized until they change. The
// zero value means unknown.
func (s *Server) localAudio(ctx context.Context, root, rel string) localAudio {
	files, err := scan.CollectAudioFiles(root, rel)
	if err != nil || len(files) == 0 {
		return localAudio{}
	}

	// The key carries the root: the same relative path can name a different
	// file under the import volume and under the library.
	key := root + "\x00" + rel
	sig := filesSignature(files)
	s.durations.mu.Lock()
	entry, ok := s.durations.m[key]
	s.durations.mu.Unlock()
	if ok && entry.sig == sig {
		return entry.localAudio
	}

	a := localAudio{files: len(files)}
	for _, f := range files {
		info, err := pipeline.ProbeFile(ctx, s.cfg.FFprobePath, f)
		if err != nil {
			return localAudio{} // partial totals would mis-rank candidates; better to skip
		}
		a.totalMs += info.DurationMs
		if info.HeaderMs > 0 {
			if a.overflowed == 0 {
				a.headerMs = info.HeaderMs
			}
			a.overflowed++
		}
	}

	s.durations.mu.Lock()
	s.durations.m[key] = durationEntry{sig: sig, localAudio: a}
	s.durations.mu.Unlock()
	return a
}

// LocalRuntimeMin is LocalRuntimeMs rounded to the nearest minute, for
// candidate scoring.
func (s *Server) LocalRuntimeMin(ctx context.Context, root, rel string) int {
	return int((s.LocalRuntimeMs(ctx, root, rel) + 30_000) / 60_000)
}

// filesSignature cheaply identifies a set of files by count, size and mtime,
// so edits or replacements invalidate the cached duration without re-probing.
func filesSignature(files []string) string {
	var sig string
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			return "stat-error"
		}
		sig += fmt.Sprintf("%s:%d:%d|", f, info.Size(), info.ModTime().UnixNano())
	}
	return sig
}
