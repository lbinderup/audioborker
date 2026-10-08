package store

import (
	"database/sql"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"audioborker/internal/pathtmpl"
)

// Settings are the runtime-editable options, persisted as key/value rows.
// Zero rows is a valid state: Defaults() supplies every value until the user
// first saves.
type Settings struct {
	InputDir  string `key:"input_dir"`
	OutputDir string `key:"output_dir"`
	// CompletedSubdir is a path RELATIVE to the mapped /completed volume
	// (empty = the volume root). It is deliberately not an absolute path:
	// an absolute one could point inside the container's own filesystem,
	// where moved files would vanish when the container is recreated.
	CompletedSubdir   string `key:"completed_subdir"`
	CleanupMode       string `key:"cleanup_mode"` // move|delete|leave
	PathTemplate      string `key:"path_template"`
	MetadataProvider  string `key:"metadata_provider"`
	AudnexusURL       string `key:"audnexus_url"`
	RegionDefault     string `key:"region_default"`
	WorkerConcurrency int    `key:"worker_concurrency"`
	BitrateKbps       int    `key:"bitrate_kbps"` // 0 = auto-probe
	Encoder           string `key:"encoder"`      // aac|fdkaac
	WriteChaptersTxt  bool   `key:"write_chapters_txt"`
	// RetagMoveDefault pre-ticks the retag screen's "Move to the path
	// template" box. On unless turned off: a retag is usually the moment a
	// file gets the name its new tags call for.
	RetagMoveDefault bool `key:"retag_move_default"`
}

var Regions = []string{"us", "ca", "uk", "au", "fr", "de", "jp", "it", "in", "es"}

var CleanupModes = []string{"leave", "move", "delete"}

// Defaults returns the settings used before the user saves anything.
// inputDir/outputDir come from the process config (container volume mounts).
func Defaults(inputDir, outputDir string) Settings {
	return Settings{
		InputDir:          inputDir,
		OutputDir:         outputDir,
		CompletedSubdir:   "",
		CleanupMode:       "leave",
		PathTemplate:      "{author}/{series_name}/{title}/{title} [{asin}]",
		MetadataProvider:  "audnexus",
		AudnexusURL:       "https://api.audnex.us",
		RegionDefault:     "us",
		WorkerConcurrency: 1,
		BitrateKbps:       0,
		Encoder:           "aac",
		WriteChaptersTxt:  true,
		RetagMoveDefault:  true,
	}
}

// GetSettings loads settings, filling any missing keys from def.
func (s *Store) GetSettings(def Settings) (Settings, error) {
	rows, err := s.db.Query("SELECT key, value FROM settings")
	if err != nil {
		return def, err
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return def, err
		}
		got[k] = v
	}
	if err := rows.Err(); err != nil {
		return def, err
	}

	out := def
	if v, ok := got["input_dir"]; ok {
		out.InputDir = v
	}
	if v, ok := got["output_dir"]; ok {
		out.OutputDir = v
	}
	if v, ok := got["completed_subdir"]; ok {
		out.CompletedSubdir = v
	}
	if v, ok := got["cleanup_mode"]; ok {
		out.CleanupMode = v
	}
	if v, ok := got["path_template"]; ok {
		out.PathTemplate = v
	}
	if v, ok := got["metadata_provider"]; ok {
		out.MetadataProvider = v
	}
	if v, ok := got["audnexus_url"]; ok {
		out.AudnexusURL = v
	}
	if v, ok := got["region_default"]; ok {
		out.RegionDefault = v
	}
	if v, ok := got["worker_concurrency"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			out.WorkerConcurrency = n
		}
	}
	if v, ok := got["bitrate_kbps"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			out.BitrateKbps = n
		}
	}
	if v, ok := got["encoder"]; ok {
		out.Encoder = v
	}
	if v, ok := got["write_chapters_txt"]; ok {
		out.WriteChaptersTxt = v == "true"
	}
	if v, ok := got["retag_move_default"]; ok {
		out.RetagMoveDefault = v == "true"
	}
	return out, nil
}

// SaveSettings validates and persists all settings atomically.
func (s *Store) SaveSettings(set Settings) error {
	if errs := set.Validate(); len(errs) > 0 {
		return fmt.Errorf("invalid settings: %s", strings.Join(errs, "; "))
	}
	kv := map[string]string{
		"input_dir":          set.InputDir,
		"output_dir":         set.OutputDir,
		"completed_subdir":   set.CompletedSubdir,
		"cleanup_mode":       set.CleanupMode,
		"path_template":      set.PathTemplate,
		"metadata_provider":  set.MetadataProvider,
		"audnexus_url":       set.AudnexusURL,
		"region_default":     set.RegionDefault,
		"worker_concurrency": strconv.Itoa(set.WorkerConcurrency),
		"bitrate_kbps":       strconv.Itoa(set.BitrateKbps),
		"encoder":            set.Encoder,
		"write_chapters_txt": strconv.FormatBool(set.WriteChaptersTxt),
		"retag_move_default": strconv.FormatBool(set.RetagMoveDefault),
	}
	return s.inTx(func(tx *sql.Tx) error {
		for k, v := range kv {
			if _, err := tx.Exec(
				"INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
				k, v,
			); err != nil {
				return err
			}
		}
		return nil
	})
}

// Validate returns user-facing error strings, one per problem, so the
// settings form can show them all at once.
func (set Settings) Validate() []string {
	var errs []string
	if strings.TrimSpace(set.InputDir) == "" {
		errs = append(errs, "Input directory must not be empty.")
	}
	if strings.TrimSpace(set.OutputDir) == "" {
		errs = append(errs, "Output directory must not be empty.")
	}
	if !contains(CleanupModes, set.CleanupMode) {
		errs = append(errs, fmt.Sprintf("Cleanup mode must be one of %s.", strings.Join(CleanupModes, ", ")))
	}
	if err := ValidateCompletedSubdir(set.CompletedSubdir); err != nil {
		errs = append(errs, "Completed subfolder: "+err.Error())
	}
	if err := pathtmpl.Validate(set.PathTemplate); err != nil {
		errs = append(errs, fmt.Sprintf("Path template: %v.", err))
	}
	if set.MetadataProvider != "audnexus" {
		errs = append(errs, "Metadata provider must be \"audnexus\" (AudiobookDB support will arrive once its API launches).")
	}
	if !strings.HasPrefix(set.AudnexusURL, "http://") && !strings.HasPrefix(set.AudnexusURL, "https://") {
		errs = append(errs, "Audnexus URL must start with http:// or https://.")
	}
	if !contains(Regions, set.RegionDefault) {
		errs = append(errs, fmt.Sprintf("Region must be one of %s.", strings.Join(Regions, ", ")))
	}
	if set.WorkerConcurrency < 1 || set.WorkerConcurrency > 8 {
		errs = append(errs, "Worker concurrency must be between 1 and 8.")
	}
	if set.BitrateKbps != 0 && !contains([]int{64, 96, 128, 192, 256, 320}, set.BitrateKbps) {
		errs = append(errs, "Bitrate must be Auto or one of 64, 96, 128, 192, 256, 320 kbps.")
	}
	if set.Encoder != "aac" && set.Encoder != "fdkaac" {
		errs = append(errs, "Encoder must be \"aac\" or \"fdkaac\".")
	}
	return errs
}

// ValidateCompletedSubdir enforces that the value is a relative path that
// stays inside the mapped /completed volume. Absolute paths are rejected
// outright: they would resolve inside the container's own filesystem, where
// moved originals are lost the next time the container is recreated.
func ValidateCompletedSubdir(sub string) error {
	sub = strings.TrimSpace(sub)
	if sub == "" {
		return nil // the volume root itself
	}
	if strings.ContainsAny(sub, `\:`) {
		return fmt.Errorf("use forward slashes, and no drive letters (got %q)", sub)
	}
	if strings.HasPrefix(sub, "/") {
		return fmt.Errorf("must be a subfolder name, not an absolute path — drop the leading %q", "/")
	}
	clean := path.Clean(sub)
	if clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
		return fmt.Errorf("must stay inside the completed volume (got %q)", sub)
	}
	return nil
}

// CompletedPath resolves the configured subfolder against the mapped
// completed volume, returning the absolute directory to move sources into.
func (set Settings) CompletedPath(completedRoot string) string {
	sub := strings.TrimSpace(set.CompletedSubdir)
	if sub == "" {
		return filepath.Clean(completedRoot)
	}
	return filepath.Join(completedRoot, filepath.FromSlash(path.Clean(sub)))
}

// isWithin reports whether child is lexically inside parent (or equal).
func isWithin(child, parent string) bool {
	c := filepath.Clean(child)
	p := filepath.Clean(parent)
	if c == p {
		return true
	}
	rel, err := filepath.Rel(p, c)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func contains[T comparable](xs []T, x T) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func (s *Store) inTx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}
