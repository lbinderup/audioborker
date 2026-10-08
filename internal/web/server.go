// Package web is the HTTP layer: routes, handlers, templates and static
// assets. Pages are server-rendered html/template; htmx swaps partial
// fragments for interactivity.
package web

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"audioborker/internal/config"
	"audioborker/internal/metadata"
	"audioborker/internal/metadata/aggregate"
	"audioborker/internal/metadata/audible"
	"audioborker/internal/metadata/audnexus"
	"audioborker/internal/queue"
	"audioborker/internal/store"
)

type Server struct {
	cfg    config.Config
	store  *store.Store
	render *renderer
	queue  *queue.Manager
	broker *queue.Broker

	provMu  sync.Mutex
	prov    metadata.Provider
	provURL string
	agg     *aggregate.Aggregator
	audible *audible.Client

	durations    *durationCache
	previewFixes previewFixes
}

func New(cfg config.Config, st *store.Store, mgr *queue.Manager, broker *queue.Broker) *http.Server {
	s := &Server{
		cfg:       cfg,
		store:     st,
		render:    newRenderer(cfg.Dev),
		queue:     mgr,
		broker:    broker,
		durations: newDurationCache(),
	}
	return &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// defaults returns the settings defaults seeded from process config.
func (s *Server) defaults() store.Settings {
	return store.Defaults(s.cfg.InputDir, s.cfg.OutputDir)
}

// settings loads current settings or falls back to defaults on DB error.
func (s *Server) settings() store.Settings {
	set, err := s.store.GetSettings(s.defaults())
	if err != nil {
		return s.defaults()
	}
	return set
}

// provider returns the metadata provider for the current settings, rebuilding
// it only when the configured base URL changes.
func (s *Server) provider() metadata.Provider {
	s.provMu.Lock()
	defer s.provMu.Unlock()
	return s.providerLocked(s.settings())
}

func (s *Server) providerLocked(set store.Settings) metadata.Provider {
	if s.prov == nil || s.provURL != set.AudnexusURL {
		s.prov = audnexus.New(set.AudnexusURL, "audioborker/"+s.cfg.Version)
		s.provURL = set.AudnexusURL
		s.agg = nil // carries the old provider; rebuilt on next use
	}
	return s.prov
}

// aggregator returns the multi-source book fetcher (Audnexus primary, Audible
// catalog filling gaps). The audible client is URL-independent and built once.
func (s *Server) aggregator() *aggregate.Aggregator {
	set := s.settings()
	s.provMu.Lock()
	defer s.provMu.Unlock()
	prov := s.providerLocked(set)
	if s.agg == nil {
		if s.audible == nil {
			s.audible = audible.New("audioborker/" + s.cfg.Version)
		}
		s.agg = &aggregate.Aggregator{
			Sources: map[string]aggregate.BookSource{
				aggregate.SourceAudnexus: prov,
				aggregate.SourceAudible:  s.audible,
			},
			Order: []string{aggregate.SourceAudnexus, aggregate.SourceAudible},
			Cache: s.store,
		}
	}
	return s.agg
}

// RootLibrary is the browse root for files already in the library (the output
// volume). Absent or anything else means the import volume, so every URL that
// predates the Library tab keeps working unchanged.
const RootLibrary = "library"

// rootDir resolves which configured directory a request is browsing. The get
// function abstracts query values from form values, the same way
// shiftSpecFrom does. The client selects a root by a fixed token and can never
// name a directory itself.
func (s *Server) rootDir(get func(string) string) string {
	set := s.settings()
	if get("root") == RootLibrary {
		return set.OutputDir
	}
	return set.InputDir
}

// isLibrary reports whether a request is operating on the library rather than
// the import volume.
func isLibrary(get func(string) string) bool { return get("root") == RootLibrary }

// baseData is embedded in every page's template data.
type baseData struct {
	Title   string
	Active  string // nav highlight: import|queue|settings
	Version string
	Flash   string
	Error   string
}

func (s *Server) base(title, active string) baseData {
	return baseData{Title: title, Active: active, Version: s.cfg.Version}
}
