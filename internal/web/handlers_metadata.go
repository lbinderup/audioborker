package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"audioborker/internal/metadata"
	"audioborker/internal/metadata/aggregate"
	"audioborker/internal/metadata/embedded"
	"audioborker/internal/pipeline"
	"audioborker/internal/scan"
)

// fieldRow is one line of the metadata source picker: the values each catalog
// offers for a field and which one the merge picked.
type fieldRow struct {
	Key, Label string
	Audnexus   string // display-truncated values; "" = source has nothing
	Audible    string
	Winner     string // source id of the default pick
	Value      string // the winner's value, for single-source rows
	Differs    bool   // both sources have a value and they disagree → radios
	IsCover    bool   // render values as image thumbnails

	// Current is what the file on disk says today, shown when retagging so the
	// table doubles as a before/after diff. Changed marks the rows a retag
	// would actually alter.
	Current string
	Changed bool
}

type metadataSourcesData struct {
	Path string
	Rows []fieldRow
	// HasCurrent adds the "In this file" column (library retags only).
	HasCurrent bool
	Notes      []string
	Err        string
}

// handleMatchMetadataSources renders the per-field source comparison for the
// selected candidate. The radios it emits live inside the match form, so the
// user's picks submit with POST /queue like every other per-item control.
func (s *Server) handleMatchMetadataSources(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	data := metadataSourcesData{Path: q.Get("path")}

	asin, region, ok := strings.Cut(q.Get("choice"), "|")
	if !ok || asin == "" {
		data.Err = "Select a match first."
		s.render.partial(w, "match", "metadata_sources", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	res, err := s.aggregator().GetBook(ctx, asin, region, nil)
	if err != nil {
		data.Err = "Lookup failed: " + err.Error()
		s.render.partial(w, "match", "metadata_sources", data)
		return
	}
	data.Notes = res.Notes

	// When retagging, read what the file currently claims so the table shows
	// before and after side by side. Reuses aggregate.Value, so the file's
	// column is formatted exactly like the catalogs'.
	var current *metadata.Book
	if isLibrary(q.Get) {
		if abs, rerr := scan.Resolve(s.settings().OutputDir, data.Path); rerr == nil {
			if info, perr := pipeline.ProbeFile(ctx, s.cfg.FFprobePath, abs); perr == nil {
				current = embedded.Book(info.Tags)
				data.HasCurrent = true
			}
		}
	}

	for _, key := range aggregate.Fields {
		nv := aggregate.SourceValue(res.PerSource, aggregate.SourceAudnexus, key)
		av := aggregate.SourceValue(res.PerSource, aggregate.SourceAudible, key)
		cv := aggregate.Value(current, key)
		if nv == "" && av == "" && cv == "" {
			continue
		}
		row := fieldRow{
			Key:      key,
			Label:    aggregate.Label(key),
			Audnexus: truncate(nv, 160),
			Audible:  truncate(av, 160),
			Winner:   res.Book.Sources[key],
			IsCover:  key == aggregate.FieldCover,
			Differs:  nv != "" && av != "" && nv != av,
			Current:  truncate(cv, 160),
			// Only flag a real difference. ffprobe cannot read every atom tone
			// writes (publisher, language, narrator's own atom), and cover and
			// runtime are not tags at all — marking those "changed" would cry
			// wolf on every single file.
			Changed: data.HasCurrent && cv != "" && cv != aggregate.Value(res.Book, key) &&
				key != aggregate.FieldCover && key != aggregate.FieldRuntime,
		}
		row.Value = row.Audnexus
		if row.Winner == aggregate.SourceAudible {
			row.Value = row.Audible
		}
		if row.IsCover {
			// Thumbnails need the full URL, not a truncated one.
			row.Audnexus, row.Audible = nv, av
			if row.Value = nv; row.Winner == aggregate.SourceAudible {
				row.Value = av
			}
		}
		data.Rows = append(data.Rows, row)
	}
	s.render.partial(w, "match", "metadata_sources", data)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	if i := strings.LastIndexByte(cut, ' '); i > n/2 {
		cut = cut[:i]
	}
	return cut + "…"
}
