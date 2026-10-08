package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"audioborker/internal/config"
	"audioborker/internal/pathtmpl"
	"audioborker/internal/store"
)

type settingsData struct {
	baseData
	Settings store.Settings
	Errors   []string
	Regions  []string
	Modes    []string
	Bitrates []int
	Preview  pathPreview

	// CompletedRoot is the mapped volume the completed subfolder hangs off;
	// CompletedResolved is where sources would actually be moved.
	CompletedRoot     string
	CompletedResolved string
	CompletedMounted  bool

	// Unmapped lists volume mount points that don't exist — almost always a
	// forgotten -v in the compose file.
	Unmapped []volumeStatus
}

type volumeStatus struct {
	Name string // container path, e.g. /output
	Why  string // consequence of leaving it unmapped
}

func (s *Server) settingsData() settingsData {
	set := s.settings()
	d := settingsData{
		baseData: s.base("Settings", "settings"),
		Settings: set,
		Regions:  store.Regions,
		Modes:    store.CleanupModes,
		Bitrates: []int{64, 96, 128, 192, 256, 320},
	}
	d.fillCompleted(s.cfg.CompletedDir, set)
	d.fillVolumes(s.cfg)
	return d
}

func (d *settingsData) fillCompleted(root string, set store.Settings) {
	d.CompletedRoot = filepath.ToSlash(filepath.Clean(root))
	d.CompletedResolved = filepath.ToSlash(set.CompletedPath(root))
	d.CompletedMounted = isDir(root)
}

// fillVolumes reports mount points that are missing. The image declares no
// VOLUMEs, so a missing directory means the user did not map anything there.
func (d *settingsData) fillVolumes(cfg config.Config) {
	d.Unmapped = nil
	checks := []struct {
		path, why string
	}{
		{cfg.InputDir, "audioborker has nothing to import from."},
		{cfg.OutputDir, "converted files would be written inside the container and lost when it is recreated."},
		{cfg.CompletedDir, "the \"move\" cleanup mode cannot be used."},
	}
	for _, c := range checks {
		if !isDir(c.path) {
			d.Unmapped = append(d.Unmapped, volumeStatus{
				Name: filepath.ToSlash(filepath.Clean(c.path)),
				Why:  c.why,
			})
		}
	}
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	data := s.settingsData()
	if r.URL.Query().Get("saved") == "1" {
		data.Flash = "Settings saved."
	}
	data.Preview = previewPath(data.Settings.PathTemplate)
	s.render.render(w, "settings", data)
}

func (s *Server) handleSettingsPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	set := store.Settings{
		InputDir:  strings.TrimSpace(r.PostFormValue("input_dir")),
		OutputDir: strings.TrimSpace(r.PostFormValue("output_dir")),
		// Stored relative to the mapped completed volume; a leading slash is
		// trimmed rather than rejected since it is an easy thing to type.
		CompletedSubdir:  strings.Trim(strings.TrimSpace(r.PostFormValue("completed_subdir")), "/"),
		CleanupMode:      r.PostFormValue("cleanup_mode"),
		PathTemplate:     strings.TrimSpace(r.PostFormValue("path_template")),
		MetadataProvider: r.PostFormValue("metadata_provider"),
		AudnexusURL:      strings.TrimSpace(r.PostFormValue("audnexus_url")),
		RegionDefault:    r.PostFormValue("region_default"),
		Encoder:          r.PostFormValue("encoder"),
		WriteChaptersTxt: r.PostFormValue("write_chapters_txt") == "on",
		RetagMoveDefault: r.PostFormValue("retag_move_default") == "on",
	}
	set.WorkerConcurrency, _ = strconv.Atoi(r.PostFormValue("worker_concurrency"))
	set.BitrateKbps, _ = strconv.Atoi(r.PostFormValue("bitrate_kbps"))

	if errs := set.Validate(); len(errs) > 0 {
		data := s.settingsData()
		data.Settings = set // re-show what the user typed
		data.Errors = errs
		data.Preview = previewPath(set.PathTemplate)
		data.fillCompleted(s.cfg.CompletedDir, set)
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.render.render(w, "settings", data)
		return
	}
	if err := s.store.SaveSettings(set); err != nil {
		data := s.settingsData()
		data.Settings = set
		data.Errors = []string{err.Error()}
		w.WriteHeader(http.StatusInternalServerError)
		s.render.render(w, "settings", data)
		return
	}
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

// handlePathPreview renders the live path-template preview fragment as the
// user types (htmx keyup trigger).
func (s *Server) handlePathPreview(w http.ResponseWriter, r *http.Request) {
	s.render.partial(w, "settings", "path_preview", previewPath(r.URL.Query().Get("template")))
}

// Two realistic samples so the preview shows how the template behaves both
// when series variables are populated and when they're absent (segments
// containing only empty variables are dropped).
var (
	previewSeriesVars = pathtmpl.Vars{
		Author: "J.K. Rowling", Narrator: "Jim Dale",
		Title:      "Harry Potter and the Sorcerer's Stone",
		SeriesName: "Harry Potter", SeriesPosition: "1",
		Year: "2015", ASIN: "B017V4IM1G",
	}
	previewStandaloneVars = pathtmpl.Vars{
		Author: "Andy Weir", Narrator: "Ray Porter",
		Title: "Project Hail Mary", // standalone: no series, no subtitle
		Year:  "2021", ASIN: "B08G9PRS1K",
	}
)

type pathPreview struct {
	Err        string
	Series     string
	Standalone string
}

func previewPath(template string) pathPreview {
	if err := pathtmpl.Validate(template); err != nil {
		return pathPreview{Err: err.Error()}
	}
	series, err := pathtmpl.Render(template, previewSeriesVars)
	if err != nil {
		return pathPreview{Err: err.Error()}
	}
	standalone, err := pathtmpl.Render(template, previewStandaloneVars)
	if err != nil {
		return pathPreview{Err: err.Error()}
	}
	return pathPreview{Series: series + ".m4b", Standalone: standalone + ".m4b"}
}
