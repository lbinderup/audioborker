package pipeline

import (
	"strings"
	"testing"

	"audioborker/internal/metadata"
)

func TestSnapBitrate(t *testing.T) {
	cases := map[int]int{0: 128, 63: 64, 70: 64, 81: 96, 110: 96, 125: 128, 130: 128, 200: 192, 300: 320, 999: 320}
	for in, want := range cases {
		if got := SnapBitrate(in); got != want {
			t.Errorf("SnapBitrate(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestParseFFmpegTime(t *testing.T) {
	ms, ok := parseFFmpegTime("size=  55296KiB time=01:02:03.50 bitrate= 121.4kbits/s speed=41.3x")
	if !ok || ms != 3_723_500 {
		t.Errorf("got %d, %v", ms, ok)
	}
	if _, ok := parseFFmpegTime("frame I/O info"); ok {
		t.Error("false positive")
	}
}

func TestUnwrapDuration(t *testing.T) {
	cases := []struct {
		name               string
		codec, timeBase    string
		durationTS, frames int64
		wantMs             int64
		wantOK             bool
	}{
		// Absolution Gap: 4,214,902 frames = 4,316,059,648 samples, stored
		// as 21,092,352 after the 32-bit header wrapped.
		{"wrapped once", "aac", "1/44100", 21_092_352, 4_214_902, 97_869_833, true},
		{"wrapped three times", "aac", "1/44100", 21_092_352, 4_214_902 + 2*(1<<32)/1024, 292_652_930, true},
		{"healthy long book", "aac", "1/44100", 4_316_059_648, 4_214_902, 0, false},
		{"healthy short file", "aac", "1/44100", 21_092_352, 20_598, 0, false},
		{"HE-AAC 2048 per tick never guesses", "aac", "1/44100", 21_092_352, 2_107_451, 0, false},
		{"not aac", "mp3", "1/44100", 21_092_352, 4_214_902, 0, false},
		{"no frame count", "aac", "1/44100", 21_092_352, 0, 0, false},
		{"bad time base", "aac", "", 21_092_352, 4_214_902, 0, false},
	}
	for _, c := range cases {
		ms, ok := unwrapDuration(c.codec, c.timeBase, c.durationTS, c.frames)
		if ok != c.wantOK || ms != c.wantMs {
			t.Errorf("%s: got %d, %v; want %d, %v", c.name, ms, ok, c.wantMs, c.wantOK)
		}
	}
}

func TestVerifyConversion(t *testing.T) {
	one := mergePlan{Files: []*FileInfo{{}}, TotalMs: 3_600_000}
	two := mergePlan{Files: []*FileInfo{{}, {}}, TotalMs: 3_600_000}
	chapters := []ProbedChapter{{Title: "One"}, {Title: "Two"}}
	cases := []struct {
		name string
		plan mergePlan
		out  FileInfo
		ok   bool
	}{
		{"good merge", two, FileInfo{DurationMs: 3_600_000, Chapters: chapters}, true},
		{"encoder padding drift", two, FileInfo{DurationMs: 3_600_046, Chapters: chapters}, true},
		{"truncated merge", two, FileInfo{DurationMs: 3_000_000, Chapters: chapters}, false},
		{"multi-file without chapters", two, FileInfo{DurationMs: 3_600_000}, false},
		{"single file may have none", one, FileInfo{DurationMs: 3_600_000}, true},
	}
	for _, c := range cases {
		if err := verifyConversion(c.plan, &c.out); (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestPlanMerge(t *testing.T) {
	aac := []*FileInfo{
		{Codec: "aac", Container: "mov,mp4,m4a,3gp,3g2,mj2", DurationMs: 1000, BitrateKbps: 125},
		{Codec: "aac", Container: "mov,mp4,m4a,3gp,3g2,mj2", DurationMs: 2000, BitrateKbps: 125},
	}
	p := planMerge(aac, 0)
	if !p.StreamCopy || p.TotalMs != 3000 {
		t.Errorf("aac plan: %+v", p)
	}

	mixed := []*FileInfo{
		{Codec: "mp3", Container: "mp3", DurationMs: 1000, BitrateKbps: 63, SampleRate: 44100},
		{Codec: "aac", Container: "mov,mp4,m4a,3gp,3g2,mj2", DurationMs: 2000},
	}
	p = planMerge(mixed, 0)
	if p.StreamCopy || p.BitrateKbps != 64 || p.SampleRate != 44100 {
		t.Errorf("mixed plan: %+v", p)
	}
	if p = planMerge(mixed, 192); p.BitrateKbps != 192 {
		t.Errorf("override ignored: %+v", p)
	}
}

func chapterFixtures(n int) []*FileInfo {
	files := make([]*FileInfo, n)
	for i := range files {
		files[i] = &FileInfo{
			Path:       "Track " + string(rune('1'+i)) + ".mp3",
			DurationMs: 60_000,
		}
	}
	return files
}

func TestResolveChaptersProviderMatch(t *testing.T) {
	files := chapterFixtures(3)
	provider := &metadata.ChapterInfo{
		RuntimeMs:  180_500, // within 10s of 180000
		IsAccurate: true,
		Chapters:   []metadata.Chapter{{Title: "One", StartMs: 0, LengthMs: 180_000}},
	}
	r := resolveChapters(ChapterModeAuto, provider, files, 180_000, "Book", metadata.ShiftSpec{})
	if r.Source != SourceProvider || len(r.Chapters) != 1 || len(r.Warnings) != 0 {
		t.Errorf("%+v", r)
	}
}

func TestResolveChaptersRuntimeMismatchFallsBack(t *testing.T) {
	files := chapterFixtures(3)
	provider := &metadata.ChapterInfo{
		RuntimeMs: 500_000, // way off vs 180000
		Chapters:  []metadata.Chapter{{Title: "One"}},
	}
	r := resolveChapters(ChapterModeAuto, provider, files, 180_000, "Book", metadata.ShiftSpec{})
	if r.Source != "files" || len(r.Chapters) != 3 || len(r.Warnings) == 0 {
		t.Errorf("%+v", r)
	}
	if r.Chapters[2].StartMs != 120_000 {
		t.Errorf("cumulative offsets wrong: %+v", r.Chapters)
	}
}

func TestResolveChaptersInaccurateWarns(t *testing.T) {
	provider := &metadata.ChapterInfo{
		RuntimeMs:  180_000,
		IsAccurate: false,
		Chapters:   []metadata.Chapter{{Title: "One"}},
	}
	r := resolveChapters(ChapterModeAuto, provider, chapterFixtures(2), 180_000, "Book", metadata.ShiftSpec{})
	if r.Source != SourceProvider || len(r.Warnings) != 1 {
		t.Errorf("%+v", r)
	}
}

func TestResolveChaptersNeverZeroForMultiFile(t *testing.T) {
	r := resolveChapters(ChapterModeAuto, nil, chapterFixtures(4), 240_000, "Book", metadata.ShiftSpec{})
	if r.Source != "files" || len(r.Chapters) != 4 {
		t.Errorf("%+v", r)
	}
}

func TestResolveChaptersSingleFileKeepsExisting(t *testing.T) {
	files := []*FileInfo{{
		Path: "book.m4b", DurationMs: 100_000,
		Chapters: []ProbedChapter{{Title: "Intro", StartMs: 0, EndMs: 5000}, {Title: "Ch1", StartMs: 5000, EndMs: 100_000}},
	}}
	r := resolveChapters(ChapterModeAuto, nil, files, 100_000, "Book", metadata.ShiftSpec{})
	if r.Source != "existing" || len(r.Chapters) != 2 || r.Chapters[1].LengthMs != 95_000 {
		t.Errorf("%+v", r)
	}
}

func TestResolveChaptersSingleFileNoData(t *testing.T) {
	files := []*FileInfo{{Path: "book.mp3", DurationMs: 100_000}}
	r := resolveChapters(ChapterModeAuto, nil, files, 100_000, "My Book", metadata.ShiftSpec{})
	if r.Source != "single" || len(r.Chapters) != 1 || r.Chapters[0].Title != "My Book" {
		t.Errorf("%+v", r)
	}
}

func TestResolveChaptersAutoPrefersProviderWhenBothExistAndRuntimeMatches(t *testing.T) {
	files := []*FileInfo{{
		Path: "book.m4b", DurationMs: 180_000,
		Chapters: []ProbedChapter{{Title: "Rip Chapter", StartMs: 0, EndMs: 180_000}},
	}}
	provider := &metadata.ChapterInfo{
		RuntimeMs: 180_000, IsAccurate: true,
		Chapters: []metadata.Chapter{{Title: "Official Chapter", StartMs: 0, LengthMs: 180_000}},
	}
	r := resolveChapters(ChapterModeAuto, provider, files, 180_000, "Book", metadata.ShiftSpec{})
	if r.Source != SourceProvider || r.Chapters[0].Title != "Official Chapter" {
		t.Errorf("runtime-matching provider should beat existing chapters in auto: %+v", r)
	}
}

func TestResolveChaptersModeExistingIgnoresProvider(t *testing.T) {
	files := []*FileInfo{{
		Path: "book.m4b", DurationMs: 180_000,
		Chapters: []ProbedChapter{{Title: "My Intro", StartMs: 0, EndMs: 180_000}},
	}}
	provider := &metadata.ChapterInfo{
		RuntimeMs: 180_000, IsAccurate: true,
		Chapters: []metadata.Chapter{{Title: "Provider Ch", StartMs: 0, LengthMs: 180_000}},
	}
	r := resolveChapters(ChapterModeExisting, provider, files, 180_000, "Book", metadata.ShiftSpec{})
	if r.Source != "existing" || r.Chapters[0].Title != "My Intro" {
		t.Errorf("%+v", r)
	}
}

func TestResolveChaptersModeProviderForcesDespiteMismatch(t *testing.T) {
	provider := &metadata.ChapterInfo{
		RuntimeMs: 900_000, IsAccurate: true, // way off vs 180000
		Chapters: []metadata.Chapter{{Title: "Provider Ch", StartMs: 0, LengthMs: 900_000}},
	}
	r := resolveChapters(ChapterModeProvider, provider, chapterFixtures(3), 180_000, "Book", metadata.ShiftSpec{})
	if r.Source != SourceProvider || len(r.Warnings) == 0 {
		t.Errorf("forced provider should be used with a warning: %+v", r)
	}
}

func TestResolveChaptersModeProviderWithNoneFallsBack(t *testing.T) {
	r := resolveChapters(ChapterModeProvider, nil, chapterFixtures(3), 180_000, "Book", metadata.ShiftSpec{})
	if r.Source != "files" || len(r.Warnings) == 0 {
		t.Errorf("%+v", r)
	}
}

func TestMixTitlesOnFileBoundariesExactCount(t *testing.T) {
	files := chapterFixtures(3)
	provider := &metadata.ChapterInfo{
		RuntimeMs: 999_000, // way off — mix modes must ignore the runtime gate
		Chapters: []metadata.Chapter{
			{Title: "The Beginning", StartMs: 0, LengthMs: 55_000},
			{Title: "The Middle", StartMs: 55_000, LengthMs: 62_000},
			{Title: "The End", StartMs: 117_000, LengthMs: 61_000},
		},
	}
	r := resolveChapters(ChapterModeTitlesFiles, provider, files, 180_000, "Book", metadata.ShiftSpec{})
	if r.Source != SourceTitlesFiles || len(r.Chapters) != 3 || len(r.Warnings) != 0 {
		t.Fatalf("%+v", r)
	}
	// Titles from the provider, timings from the files.
	if r.Chapters[1].Title != "The Middle" || r.Chapters[1].StartMs != 60_000 || r.Chapters[1].LengthMs != 60_000 {
		t.Errorf("mixed chapter wrong: %+v", r.Chapters[1])
	}
}

func TestMixTitlesDropsShortOpeningCredits(t *testing.T) {
	files := chapterFixtures(2)
	provider := &metadata.ChapterInfo{
		BrandIntroMs: 2043,
		Chapters: []metadata.Chapter{
			{Title: "Opening Credits", StartMs: 0, LengthMs: 6_000}, // short: brand intro + a breath
			{Title: "Chapter 1", StartMs: 6_000, LengthMs: 60_000},
			{Title: "Chapter 2", StartMs: 66_000, LengthMs: 60_000},
		},
	}
	r := resolveChapters(ChapterModeTitlesFiles, provider, files, 120_000, "Book", metadata.ShiftSpec{})
	if r.Source != SourceTitlesFiles || len(r.Chapters) != 2 {
		t.Fatalf("%+v", r)
	}
	if r.Chapters[0].Title != "Chapter 1" || r.Chapters[1].Title != "Chapter 2" {
		t.Errorf("titles: %+v", r.Chapters)
	}
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "Opening Credits") {
		t.Errorf("expected a dropped-chapter note, got %v", r.Warnings)
	}
}

func TestMixTitlesDropsIntroAndOutro(t *testing.T) {
	files := chapterFixtures(2)
	provider := &metadata.ChapterInfo{
		Chapters: []metadata.Chapter{
			{Title: "Opening Credits", LengthMs: 12_000},
			{Title: "Chapter 1", LengthMs: 60_000},
			{Title: "Chapter 2", LengthMs: 60_000},
			{Title: "End Credits", LengthMs: 30_000},
		},
	}
	r := resolveChapters(ChapterModeTitlesFiles, provider, files, 120_000, "Book", metadata.ShiftSpec{})
	if r.Source != SourceTitlesFiles || len(r.Chapters) != 2 ||
		r.Chapters[0].Title != "Chapter 1" || r.Chapters[1].Title != "Chapter 2" {
		t.Fatalf("%+v", r)
	}
}

func TestMixTitlesLongExtraChapterFallsBack(t *testing.T) {
	files := chapterFixtures(2)
	provider := &metadata.ChapterInfo{
		Chapters: []metadata.Chapter{
			{Title: "Prologue", LengthMs: 300_000}, // a real chapter, not a stinger
			{Title: "Chapter 1", LengthMs: 60_000},
			{Title: "Chapter 2", LengthMs: 60_000},
		},
	}
	r := resolveChapters(ChapterModeTitlesFiles, provider, files, 120_000, "Book", metadata.ShiftSpec{})
	// Falls through to auto; provider runtime 0 vs 120000 mismatches, so file
	// boundaries win — and the multi-file zero-chapters invariant holds.
	if r.Source != SourceFiles || len(r.Chapters) != 2 || len(r.Warnings) == 0 {
		t.Errorf("%+v", r)
	}
}

func TestMixTitlesCountMismatchFallsBack(t *testing.T) {
	files := chapterFixtures(2)
	provider := &metadata.ChapterInfo{
		Chapters: []metadata.Chapter{
			{Title: "1", LengthMs: 60_000}, {Title: "2", LengthMs: 60_000},
			{Title: "3", LengthMs: 60_000}, {Title: "4", LengthMs: 60_000},
			{Title: "5", LengthMs: 60_000},
		},
	}
	r := resolveChapters(ChapterModeTitlesFiles, provider, files, 120_000, "Book", metadata.ShiftSpec{})
	if r.Source != SourceFiles || len(r.Warnings) == 0 {
		t.Errorf("%+v", r)
	}
}

func TestMixTitlesOnExistingChapters(t *testing.T) {
	files := []*FileInfo{{
		Path: "book.m4b", DurationMs: 120_000,
		Chapters: []ProbedChapter{
			{Title: "Track 01", StartMs: 0, EndMs: 55_000},
			{Title: "Track 02", StartMs: 55_000, EndMs: 120_000},
		},
	}}
	provider := &metadata.ChapterInfo{
		Chapters: []metadata.Chapter{
			{Title: "A Real Name", StartMs: 0, LengthMs: 50_000},
			{Title: "Another Name", StartMs: 50_000, LengthMs: 70_000},
		},
	}
	r := resolveChapters(ChapterModeTitlesExisting, provider, files, 120_000, "Book", metadata.ShiftSpec{})
	if r.Source != SourceTitlesExisting || len(r.Chapters) != 2 {
		t.Fatalf("%+v", r)
	}
	// Embedded timings survive; provider names replace the junk titles.
	if r.Chapters[0].Title != "A Real Name" || r.Chapters[1].StartMs != 55_000 || r.Chapters[1].LengthMs != 65_000 {
		t.Errorf("%+v", r.Chapters)
	}
}

func TestMixTitlesExistingOnMultiFileFallsBack(t *testing.T) {
	provider := &metadata.ChapterInfo{
		Chapters: []metadata.Chapter{{Title: "One", LengthMs: 60_000}},
	}
	r := resolveChapters(ChapterModeTitlesExisting, provider, chapterFixtures(3), 180_000, "Book", metadata.ShiftSpec{})
	if r.Source != SourceFiles || len(r.Chapters) != 3 || len(r.Warnings) == 0 {
		t.Errorf("%+v", r)
	}
}

func TestMixTitlesNoProviderFallsBack(t *testing.T) {
	r := resolveChapters(ChapterModeTitlesFiles, nil, chapterFixtures(3), 180_000, "Book", metadata.ShiftSpec{})
	if r.Source != SourceFiles || len(r.Chapters) != 3 || len(r.Warnings) == 0 {
		t.Errorf("%+v", r)
	}
}

func TestFilenameTitles(t *testing.T) {
	named := []*FileInfo{
		{Path: "01 - Opening Credits.mp3"}, {Path: "02 - The Journey Begins.mp3"},
	}
	got := filenameTitles(named)
	if got[0] != "Opening Credits" || got[1] != "The Journey Begins" {
		t.Errorf("named: %v", got)
	}

	numbered := []*FileInfo{{Path: "Track 01.mp3"}, {Path: "Track 02.mp3"}}
	got = filenameTitles(numbered)
	if got[0] != "Chapter 01" || got[1] != "Chapter 02" {
		t.Errorf("numbered: %v", got)
	}

	identical := []*FileInfo{{Path: "CD1/dune.mp3"}, {Path: "CD2/dune.mp3"}}
	got = filenameTitles(identical)
	if got[0] != "Chapter 01" {
		t.Errorf("identical names should fall back: %v", got)
	}
}

func TestChaptersTxtFormat(t *testing.T) {
	txt := chaptersTxt([]metadata.Chapter{
		{Title: "Opening Credits", StartMs: 0, LengthMs: 11_264},
		{Title: "Chapter 1", StartMs: 11_264, LengthMs: 3_723_500},
	})
	want := "00:00:00.000 Opening Credits\n00:00:11.264 Chapter 1\n## total-duration: 01:02:14.764\n"
	if txt != want {
		t.Errorf("got:\n%s\nwant:\n%s", txt, want)
	}
}

func TestHiResCoverURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://m.media-amazon.com/images/I/81abc+L.jpg":                    "https://m.media-amazon.com/images/I/81abc+L._SL2000_.jpg",
		"https://m.media-amazon.com/images/I/51xyz-L._SL500_.jpg":            "https://m.media-amazon.com/images/I/51xyz-L._SL2000_.jpg",
		"https://m.media-amazon.com/images/I/51xyz-L._SL1000_.jpg":           "https://m.media-amazon.com/images/I/51xyz-L._SL2000_.jpg",
		"https://images-na.ssl-images-amazon.com/images/I/5z._AC_SX342_.jpg": "https://images-na.ssl-images-amazon.com/images/I/5z._SL2000_.jpg",
		"https://x/y._SL500_.jpg":                                            "https://x/y._SL500_.jpg",
		"https://m.media-amazon.com/images/I/81abc+L.png":                    "https://m.media-amazon.com/images/I/81abc+L.png",
	} {
		if got := hiResCoverURL(in); got != want {
			t.Errorf("hiResCoverURL(%s) = %s, want %s", in, got, want)
		}
	}
}

// embeddedBook is a single file whose own chapters are named well but start
// late — the case the shift on embedded chapters exists for.
func embeddedBook() []*FileInfo {
	return []*FileInfo{{
		Path: "book.m4b", DurationMs: 120_000,
		Chapters: []ProbedChapter{
			{Title: "Prologue", StartMs: 0, EndMs: 40_000},
			{Title: "One", StartMs: 40_000, EndMs: 80_000},
			{Title: "Two", StartMs: 80_000, EndMs: 120_000},
		},
	}}
}

func starts(chs []metadata.Chapter) []int64 {
	out := make([]int64, len(chs))
	for i, c := range chs {
		out[i] = c.StartMs
	}
	return out
}

func TestShiftAppliesToEmbeddedChapters(t *testing.T) {
	r := resolveChapters(ChapterModeExisting, nil, embeddedBook(), 120_000, "Book",
		metadata.ShiftSpec{Mode: "fixed", FixedMs: -1500})
	if r.Source != SourceExisting || r.Shift.FixedMs != -1500 {
		t.Fatalf("%+v", r)
	}
	// Names kept, timings moved; the first can't start before the audio.
	if got := starts(r.Chapters); got[0] != 0 || got[1] != 38_500 || got[2] != 78_500 {
		t.Errorf("starts %v", got)
	}
	if r.Chapters[1].Title != "One" || r.Chapters[2].LengthMs != 41_500 {
		t.Errorf("%+v", r.Chapters)
	}
}

func TestShiftInterpolatesEmbeddedChaptersToLast(t *testing.T) {
	// ToIdx 0 anchors the last chapter of whichever list is embedded.
	r := resolveChapters(ChapterModeAuto, nil, embeddedBook(), 120_000, "Book",
		metadata.ShiftSpec{Mode: "interp", FromIdx: 1, FromMs: 0, ToMs: 2000})
	if got := starts(r.Chapters); got[0] != 0 || got[1] != 41_000 || got[2] != 82_000 {
		t.Errorf("starts %v", got)
	}
}

func TestShiftStillAppliesToProviderChapters(t *testing.T) {
	provider := &metadata.ChapterInfo{
		RuntimeMs: 120_000,
		Chapters: []metadata.Chapter{
			{Title: "A", StartMs: 0, LengthMs: 60_000},
			{Title: "B", StartMs: 60_000, LengthMs: 60_000},
		},
	}
	r := resolveChapters(ChapterModeAuto, provider, embeddedBook(), 120_000, "Book",
		metadata.ShiftSpec{Mode: "fixed", FixedMs: 700})
	if r.Source != SourceProvider || starts(r.Chapters)[1] != 60_700 {
		t.Errorf("%+v", r)
	}
	if provider.Chapters[1].StartMs != 60_000 {
		t.Error("shifting must not mutate the provider's cached list")
	}
}

func TestShiftAppliesToTitlesOnEmbeddedTimings(t *testing.T) {
	provider := &metadata.ChapterInfo{Chapters: []metadata.Chapter{
		{Title: "P", LengthMs: 40_000}, {Title: "1", LengthMs: 40_000}, {Title: "2", LengthMs: 40_000},
	}}
	r := resolveChapters(ChapterModeTitlesExisting, provider, embeddedBook(), 120_000, "Book",
		metadata.ShiftSpec{Mode: "fixed", FixedMs: 1000})
	if r.Source != SourceTitlesExisting || r.Chapters[1].Title != "1" || starts(r.Chapters)[1] != 41_000 {
		t.Errorf("%+v", r)
	}
}

func TestShiftIgnoredForFileBoundaries(t *testing.T) {
	r := resolveChapters(ChapterModeAuto, nil, chapterFixtures(3), 180_000, "Book",
		metadata.ShiftSpec{Mode: "fixed", FixedMs: 1000})
	if r.Source != SourceFiles || starts(r.Chapters)[1] != 60_000 || !r.Shift.IsZero() {
		t.Errorf("%+v", r)
	}
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "Shift ignored") {
		t.Errorf("warnings %v", r.Warnings)
	}
}
