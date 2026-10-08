package web

import (
	"testing"

	"audioborker/internal/pipeline"
)

func TestOverflowNote(t *testing.T) {
	wrapped := &pipeline.FileInfo{DurationMs: 97_540_690, HeaderMs: 149_141}
	healthy := &pipeline.FileInfo{DurationMs: 3_600_000}
	cases := []struct {
		name  string
		infos []*pipeline.FileInfo
		want  string
	}{
		{"none", []*pipeline.FileInfo{healthy}, ""},
		{"one file", []*pipeline.FileInfo{wrapped}, "Length header overflowed: the file claims 02:29 (fixed in the output)."},
		{"some of several", []*pipeline.FileInfo{wrapped, healthy, wrapped},
			"Length headers overflowed in 2 of 3 files (fixed in the output)."},
	}
	for _, c := range cases {
		if got := overflowNote(c.infos); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
