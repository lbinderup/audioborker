package web

import (
	"io/fs"
	"net/http"
	"path/filepath"
)

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/import", http.StatusFound)
	})

	mux.HandleFunc("GET /library", s.handleLibrary)
	mux.HandleFunc("GET /library/tree", s.handleLibraryTree)
	mux.HandleFunc("GET /library/search", s.handleLibrarySearch)
	mux.HandleFunc("POST /library/match", s.handleLibraryMatch)
	mux.HandleFunc("GET /library/rename-preview", s.handleLibraryRenamePreview)
	mux.HandleFunc("POST /library/sort", s.handleLibrarySort)
	mux.HandleFunc("POST /library/sort/apply", s.handleLibrarySortApply)

	mux.HandleFunc("GET /import", s.handleImport)
	mux.HandleFunc("GET /import/tree", s.handleImportTree)
	mux.HandleFunc("POST /match", s.handleMatch)
	mux.HandleFunc("GET /match/candidates", s.handleMatchCandidates)
	mux.HandleFunc("GET /match/lookup", s.handleMatchLookup)
	mux.HandleFunc("GET /match/preview", s.handleMatchPreview)
	mux.HandleFunc("GET /match/provider-chapters", s.handleMatchProviderChapters)
	mux.HandleFunc("GET /match/chapter-plan", s.handleMatchChapterPlan)
	mux.HandleFunc("GET /match/row-summary", s.handleMatchRowSummary)
	mux.HandleFunc("GET /match/metadata-sources", s.handleMatchMetadataSources)
	mux.HandleFunc("GET /preview/audio", s.handlePreviewAudio)

	// Tagging by hand: the match dialog's last resort for editions no catalog
	// has. It assigns a match like a catalog pick; POST /queue queues it.
	mux.HandleFunc("GET /match/manual", s.handleMatchManual)
	mux.HandleFunc("POST /match/manual/parse", s.handleManualParse)
	mux.HandleFunc("POST /match/manual/check", s.handleMatchManualCheck)
	mux.HandleFunc("POST /match/manual/cover", s.handleManualCover)
	mux.HandleFunc("GET /manual/covers/{name}", s.handleManualCoverFile)

	mux.HandleFunc("POST /queue", s.handleQueueCreate)
	mux.HandleFunc("GET /queue", s.handleQueue)
	mux.HandleFunc("GET /queue/table", s.handleQueueTable)
	mux.HandleFunc("POST /queue/clear-history", s.handleClearHistory)
	mux.HandleFunc("GET /jobs/{id}", s.handleJobDetail)
	mux.HandleFunc("GET /jobs/{id}/status", s.handleJobStatusPartial)
	mux.HandleFunc("GET /jobs/{id}/log", s.handleJobLog)
	mux.HandleFunc("POST /jobs/{id}/cancel", s.handleJobCancel)
	mux.HandleFunc("POST /jobs/{id}/retry", s.handleJobRetry)
	mux.HandleFunc("GET /events", s.handleEvents)

	mux.HandleFunc("GET /settings", s.handleSettingsGet)
	mux.HandleFunc("POST /settings", s.handleSettingsPost)
	mux.HandleFunc("GET /settings/path-preview", s.handlePathPreview)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Dev serves static assets from disk so CSS/JS edits apply on refresh,
	// matching the template reloading; production uses the embedded copies.
	var staticHandler http.Handler
	if s.cfg.Dev {
		staticHandler = http.FileServer(http.Dir(filepath.Join("internal", "web", "static")))
	} else {
		staticRoot, _ := fs.Sub(staticFS, "static")
		staticHandler = http.FileServerFS(staticRoot)
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", s.cacheStatic(staticHandler)))

	return mux
}

// cacheStatic sets caching for static assets. The layout appends ?v=<version>
// to asset URLs, so releases bust the cache; dev mode disables caching
// entirely because assets change constantly and the version string does not.
func (s *Server) cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Dev {
			w.Header().Set("Cache-Control", "no-store")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=86400")
		}
		next.ServeHTTP(w, r)
	})
}
