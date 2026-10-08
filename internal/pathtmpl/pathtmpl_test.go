package pathtmpl

import "testing"

func full() Vars {
	return Vars{
		Author: "Andy Weir", Narrator: "Ray Porter", Title: "Project Hail Mary",
		Subtitle: "A Novel", SeriesName: "Hail Mary", SeriesPosition: "1",
		Year: "2021", ASIN: "B08G9PRS1K",
	}
}

func TestRender(t *testing.T) {
	tests := []struct {
		name     string
		template string
		mutate   func(*Vars)
		want     string
		wantErr  bool
	}{
		{name: "default", template: "{author}/{title}", want: "Andy Weir/Project Hail Mary"},
		{
			name:     "series template fully populated",
			template: "{author}/{series_name}/{series_position} - {title}",
			want:     "Andy Weir/Hail Mary/1 - Project Hail Mary",
		},
		{
			name:     "missing series drops the segment",
			template: "{author}/{series_name}/{series_position} - {title}",
			mutate:   func(v *Vars) { v.SeriesName = ""; v.SeriesPosition = "" },
			want:     "Andy Weir/Project Hail Mary",
		},
		{
			name:     "missing position collapses dangling separator",
			template: "{author}/{series_position} - {title}",
			mutate:   func(v *Vars) { v.SeriesPosition = "" },
			want:     "Andy Weir/Project Hail Mary",
		},
		{
			name:     "missing subtitle collapses trailing separator",
			template: "{author}/{title} - {subtitle}",
			mutate:   func(v *Vars) { v.Subtitle = "" },
			want:     "Andy Weir/Project Hail Mary",
		},
		{
			name:     "empty asin drops its brackets (books tagged by hand)",
			template: "{author}/{series_name}/{title}/{title} [{asin}]",
			mutate:   func(v *Vars) { v.ASIN = "" },
			want:     "Andy Weir/Hail Mary/Project Hail Mary/Project Hail Mary",
		},
		{
			name:     "conditional text shows with a series position",
			template: "{author}/{series_name}/{Book series_position:00 - }{title}/{title} [{asin}]",
			want:     "Andy Weir/Hail Mary/Book 01 - Project Hail Mary/Project Hail Mary [B08G9PRS1K]",
		},
		{
			name:     "and disappears for a standalone",
			template: "{author}/{series_name}/{Book series_position:00 - }{title}/{title} [{asin}]",
			mutate:   func(v *Vars) { v.SeriesName = ""; v.SeriesPosition = "" },
			want:     "Andy Weir/Project Hail Mary/Project Hail Mary [B08G9PRS1K]",
		},
		{
			name:     "padding keeps a fraction and leaves wide numbers alone",
			template: "{series_position:00} {year:000} {title:00}",
			mutate:   func(v *Vars) { v.SeriesPosition = "1.5"; v.Year = "2021" },
			want:     "01.5 2021 Project Hail Mary",
		},
		{
			name:     "text in a token's braces is text, even a name capitalized",
			template: "{author}/{Title title (series_name)}",
			want:     "Andy Weir/Title Project Hail Mary (series_name)",
		},
		{
			name:     "a bracket group with a value stays",
			template: "{author}/{title} ({year}) [{asin}]",
			mutate:   func(v *Vars) { v.ASIN = "" },
			want:     "Andy Weir/Project Hail Mary (2021)",
		},
		{
			name:     "a group is empty only when all its tokens are",
			template: "{author}/{title} [{asin} {year}]",
			mutate:   func(v *Vars) { v.ASIN = "" },
			want:     "Andy Weir/Project Hail Mary [ 2021]",
		},
		{
			name:     "literal text in an all-empty group goes with it",
			template: "{author}/{title} [ASIN {asin}]",
			mutate:   func(v *Vars) { v.ASIN = "" },
			want:     "Andy Weir/Project Hail Mary",
		},
		{
			name:     "brackets without tokens are literal",
			template: "{author}/{title} [Unabridged]",
			want:     "Andy Weir/Project Hail Mary [Unabridged]",
		},
		{
			name:     "brackets inside a value are untouched",
			template: "{author}/{title} [{asin}]",
			mutate:   func(v *Vars) { v.Title = "Book [Remastered]"; v.ASIN = "" },
			want:     "Andy Weir/Book [Remastered]",
		},
		{
			name:     "value containing a token name is not re-substituted",
			template: "{author}/{title}",
			mutate:   func(v *Vars) { v.Author = "{title} year" },
			want:     "{title} year/Project Hail Mary",
		},
		{
			name:     "invalid filesystem chars stripped per segment",
			template: "{author}/{title}",
			mutate:   func(v *Vars) { v.Title = `Who? What: "Why" <How>`; v.Author = "AC/DC" },
			want:     "ACDC/Who What Why How",
		},
		{
			name:     "trailing dots trimmed for windows",
			template: "{author}/{title}",
			mutate:   func(v *Vars) { v.Title = "Vol. 1." },
			want:     "Andy Weir/Vol. 1",
		},
		{
			name:     "everything empty errors",
			template: "{series_name}/{subtitle}",
			mutate: func(v *Vars) {
				v.SeriesName = ""
				v.Subtitle = ""
			},
			wantErr: true,
		},
		{
			name:     "literal-only segment is kept",
			template: "Audiobooks/{author}/{title}",
			want:     "Audiobooks/Andy Weir/Project Hail Mary",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := full()
			if tt.mutate != nil {
				tt.mutate(&v)
			}
			got, err := Render(tt.template, v)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("Render(%q) = %q, want %q", tt.template, got, tt.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	if err := Validate("{author}/{title}"); err != nil {
		t.Errorf("valid template rejected: %v", err)
	}
	if err := Validate("{authr}/{title}"); err == nil {
		t.Error("unknown token accepted")
	}
	if err := Validate("plain text"); err == nil {
		t.Error("token-free template accepted")
	}
	if err := Validate(""); err == nil {
		t.Error("empty template accepted")
	}
	for _, ok := range []string{
		"{author}/{series_name}/{Book series_position:00 - }{title}",
		"{author}/{title} [{asin}]",
	} {
		if err := Validate(ok); err != nil {
			t.Errorf("Validate(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{
		"{author}/{Book position - }{title}", // no variable in the braces
		"{author}/{title",                    // unclosed
		"{author/title}",                     // braces across a folder
	} {
		if err := Validate(bad); err == nil {
			t.Errorf("Validate(%q) accepted", bad)
		}
	}
}
