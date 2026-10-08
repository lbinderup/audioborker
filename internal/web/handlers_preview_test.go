package web

import "testing"

func TestOverflowNote(t *testing.T) {
	cases := []struct {
		name string
		a    localAudio
		want string
	}{
		{"none", localAudio{totalMs: 3_600_000, files: 1}, ""},
		{"one file", localAudio{totalMs: 97_540_690, files: 1, overflowed: 1, headerMs: 149_141},
			"Length header overflowed: the file claims 02:29 (fixed in the output)"},
		{"some of several", localAudio{files: 3, overflowed: 2, headerMs: 149_141},
			"Length headers overflowed in 2 of 3 files (fixed in the output)"},
	}
	for _, c := range cases {
		if got := c.a.overflowNote(); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
