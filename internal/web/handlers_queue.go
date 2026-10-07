package web

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"audioborker/internal/metadata/aggregate"
	"audioborker/internal/scan"
	"audioborker/internal/store"
)

const jobsPerPage = 20

// metadataOverrides collects the match screen's per-field source choices for
// one item: msrc:{field}:{relpath}=source. Values equal to the automatic
// winner are submitted too (checked radios), which is harmless — the merge
// treats them as identical to no override.
func metadataOverrides(form url.Values, relPath string) map[string]string {
	return metadataOverridesFor(func(key string) string { return form.Get(key + ":" + relPath) })
}

// metadataOverridesFor collects the per-field source picks through an
// arbitrary key lookup, so the queue handler (form keys suffixed with the item
// path) and the rename preview (query keys, one item per request) share one
// parser rather than two that can drift.
func metadataOverridesFor(get func(string) string) map[string]string {
	var out map[string]string
	for _, f := range aggregate.Fields {
		if v := get("msrc:" + f); v != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[f] = v
		}
	}
	return out
}

// handleQueueCreate turns confirmed matches into queued jobs.
// Form shape: paths=<rel> (repeated) + match:<rel>=<ASIN>|<region>.
func (s *Server) handleQueueCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	set := s.settings()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// A library batch retags files in place instead of converting new ones.
	retag := isLibrary(r.PostForm.Get)
	root := s.rootDir(r.PostForm.Get)
	kind := store.KindConvert
	if retag {
		kind = store.KindRetag
	}

	// Resolve the completed subfolder once: it is stored relative to the
	// mapped volume so it can never point inside the container itself.
	completedPath := set.CompletedPath(s.cfg.CompletedDir)
	completedMounted := false
	if info, err := os.Stat(s.cfg.CompletedDir); err == nil && info.IsDir() {
		completedMounted = true
	}

	queued, skipped := 0, []string{}
	for _, p := range scan.DedupeSelection(r.PostForm["paths"]) {
		choice := r.PostForm.Get("match:" + p)
		if choice == "" {
			continue // user left this one unmatched
		}
		asin, region, ok := strings.Cut(choice, "|")
		if !ok || asin == "" {
			continue
		}

		if active, err := s.store.HasActiveJobForPath(kind, p); err == nil && active {
			skipped = append(skipped, p+" (already queued)")
			continue
		}
		files, err := scan.CollectAudioFiles(root, p)
		if err != nil {
			skipped = append(skipped, p+" ("+err.Error()+")")
			continue
		}
		if retag && len(files) != 1 {
			skipped = append(skipped, p+" (a retag covers one file at a time)")
			continue
		}
		res, err := s.aggregator().GetBook(ctx, asin, region, metadataOverrides(r.PostForm, p))
		if err != nil {
			skipped = append(skipped, p+" (metadata: "+err.Error()+")")
			continue
		}
		book := res.Book

		// Cleanup acts on the job's *source*. For a retag that source is the
		// library file itself, so "delete" or "move" would destroy the book
		// that was just rewritten — a retag never cleans up.
		cleanup := "leave"
		if !retag {
			cleanup = set.CleanupMode
			if o := r.PostForm.Get("cleanup:" + p); o != "" {
				cleanup = o
			}
			if cleanup == "move" && !completedMounted {
				skipped = append(skipped, p+" (cleanup \"move\" needs a volume mapped to "+s.cfg.CompletedDir+")")
				continue
			}
		}
		chapterMode := r.PostForm.Get("chapters:" + p) // "" = auto
		chapterShift := shiftSpecFrom(func(key string) string {
			return r.PostForm.Get(key + ":" + p)
		})

		job := &store.Job{
			InputPath:   p,
			SourceFiles: files,
			ASIN:        asin,
			Region:      region,
			Metadata:    *book,
			Options: store.JobOptions{
				Kind: kind,
				// For a retag this is the library root, so InputPath still
				// resolves the same way it does for a conversion.
				InputDir:         root,
				OutputDir:        set.OutputDir,
				CompletedDir:     completedPath,
				CleanupMode:      cleanup,
				PathTemplate:     set.PathTemplate,
				BitrateKbps:      set.BitrateKbps,
				Encoder:          set.Encoder,
				WriteChaptersTxt: set.WriteChaptersTxt,
				AudnexusURL:      set.AudnexusURL,
				ChapterMode:      chapterMode,
				ChapterShift:     chapterShift,
				Rename:           retag && r.PostForm.Get("rename") != "",
			},
		}
		if err := s.store.CreateJob(job); err != nil {
			skipped = append(skipped, p+" (queue: "+err.Error()+")")
			continue
		}
		queued++
	}
	s.queue.Wake()

	q := "queued=" + strconv.Itoa(queued)
	if len(skipped) > 0 {
		slog.Warn("queue: items skipped", "reasons", skipped)
		reasons := strings.Join(skipped, "; ")
		if len(reasons) > 500 {
			reasons = reasons[:500] + "…"
		}
		q += "&skipped=" + url.QueryEscape(reasons)
	}
	http.Redirect(w, r, "/queue?"+q, http.StatusSeeOther)
}

type queueData struct {
	baseData
	Jobs     []*store.Job
	Counts   map[string]int
	Page     int
	LastPage int
	Total    int
}

func (s *Server) queueData(r *http.Request) queueData {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	jobs, total, err := s.store.ListJobs((page-1)*jobsPerPage, jobsPerPage)
	counts, _ := s.store.CountByStatus()
	data := queueData{
		baseData: s.base("Queue", "queue"),
		Jobs:     jobs,
		Counts:   counts,
		Page:     page,
		LastPage: (total + jobsPerPage - 1) / jobsPerPage,
		Total:    total,
	}
	if err != nil {
		data.Error = err.Error()
	}
	return data
}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	data := s.queueData(r)
	if n := r.URL.Query().Get("queued"); n != "" && n != "0" {
		data.Flash = n + " conversion(s) queued."
	}
	if reasons := r.URL.Query().Get("skipped"); reasons != "" {
		data.Error = "Skipped: " + reasons
	}
	s.render.render(w, "queue", data)
}

func (s *Server) handleQueueTable(w http.ResponseWriter, r *http.Request) {
	s.render.partial(w, "queue", "queue_table", s.queueData(r))
}

func (s *Server) handleJobCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.queue.Cancel(id); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	redirectBack(w, r, "/queue")
}

func (s *Server) handleJobRetry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.CloneForRetry(id); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.queue.Wake()
	redirectBack(w, r, "/queue")
}

func (s *Server) handleClearHistory(w http.ResponseWriter, r *http.Request) {
	logs, err := s.store.ClearHistory()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, p := range logs {
		os.Remove(p)
	}
	s.pruneCovers(0) // covers chosen by hand for the jobs just cleared
	s.queue.Wake()   // repaint queue views
	http.Redirect(w, r, "/queue", http.StatusSeeOther)
}

type jobDetailData struct {
	baseData
	Job     *store.Job
	LogTail string
}

func (s *Server) handleJobDetail(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetJob(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data := jobDetailData{
		baseData: s.base("Job · "+job.Metadata.Title, "queue"),
		Job:      job,
		LogTail:  tailFile(job.LogPath, 200),
	}
	s.render.render(w, "job", data)
}

// handleJobStatusPartial re-renders the detail page's status block
// (htmx target for SSE job-status events).
func (s *Server) handleJobStatusPartial(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetJob(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.render.partial(w, "job", "job_status", job)
}

func (s *Server) handleJobLog(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetJob(r.PathValue("id"))
	if err != nil || job.LogPath == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	http.ServeFile(w, r, job.LogPath)
}

// tailFile returns the last n lines of a file (best effort).
func tailFile(path string, n int) string {
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// redirectBack sends the browser to the referer (same-origin paths only).
func redirectBack(w http.ResponseWriter, r *http.Request, fallback string) {
	ref := r.Header.Get("Referer")
	if ref != "" && strings.HasPrefix(ref, "http") {
		// Keep only the path to stay same-origin.
		if i := strings.Index(ref, "//"); i >= 0 {
			if j := strings.Index(ref[i+2:], "/"); j >= 0 {
				http.Redirect(w, r, ref[i+2+j:], http.StatusSeeOther)
				return
			}
		}
	}
	http.Redirect(w, r, fallback, http.StatusSeeOther)
}
