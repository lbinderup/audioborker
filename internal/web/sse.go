package web

import (
	"bytes"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"audioborker/internal/queue"
)

// handleEvents is the single SSE stream. Queue events are rendered into
// HTML fragments named so the htmx SSE extension swaps them in place:
//
//	job-{id}-progress  → partials/progress.html   (row + detail progress bar)
//	job-{id}-status    → partials/job_row.html    (queue table row swap)
//	job-{id}-log       → one escaped log line     (detail page appends)
//	queue-changed      → empty; triggers a table refresh via hx-trigger
//	connected          → sent on subscribe; pages refresh on it to catch up
//	                     on whatever happened before their stream was open
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: don't buffer

	events, cancel := s.broker.Subscribe()
	defer cancel()
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	writeSSE(w, "connected", "ok")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case e := <-events:
			switch e.Kind {
			case queue.EventProgress:
				writeSSE(w, "job-"+e.JobID+"-progress", s.renderToString("queue", "progress", progressData{
					Stage: e.Stage, Percent: int(e.Progress * 100),
				}))
			case queue.EventLog:
				writeSSE(w, "job-"+e.JobID+"-log", html.EscapeString(e.Line)+"\n")
			case queue.EventStatus:
				if e.JobID != "" {
					if job, err := s.store.GetJob(e.JobID); err == nil {
						writeSSE(w, "job-"+e.JobID+"-status", s.renderToString("queue", "job_row", job))
					}
				}
				writeSSE(w, "queue-changed", "1")
			}
			flusher.Flush()
		}
	}
}

type progressData struct {
	Stage   string
	Percent int
}

// renderToString renders a partial to a string for SSE payloads.
func (s *Server) renderToString(page, block string, data any) string {
	t, err := s.render.page(page)
	if err != nil {
		return ""
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, block, data); err != nil {
		return ""
	}
	return buf.String()
}

// writeSSE emits one event; multi-line data is split into data: lines per
// the SSE spec.
func writeSSE(w http.ResponseWriter, event, data string) {
	fmt.Fprintf(w, "event: %s\n", event)
	for _, line := range strings.Split(strings.TrimRight(data, "\n"), "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
}
