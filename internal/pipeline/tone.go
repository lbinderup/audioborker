package pipeline

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"audioborker/internal/metadata"
)

type toneRunner struct {
	tone string
}

// tag applies all metadata, the cover and chapters in one `tone tag` pass.
// tone writes both Nero (chpl) and QuickTime chapter formats.
func (t toneRunner) tag(ctx context.Context, m4bPath string, book *metadata.Book, coverPath, chaptersPath string, logf LogFunc) error {
	// Every option is passed as a single "--flag=value" token: values that
	// start with dashes (the ---- freeform atom prefix) would otherwise be
	// parsed as option names, and it removes any ambiguity for free-text
	// fields like descriptions.
	args := []string{"tag", m4bPath, "--assume-yes",
		"--meta-title=" + book.Title,
		"--meta-album=" + book.Title,
		"--meta-itunes-media-type=Audiobook",
	}
	if len(book.Authors) > 0 {
		authors := strings.Join(book.Authors, ", ")
		args = append(args, "--meta-artist="+authors, "--meta-album-artist="+authors)
	}
	if len(book.Narrators) > 0 {
		narrators := strings.Join(book.Narrators, ", ")
		// ©nrt via --meta-narrator; ©wrt composer carries the narrator too by
		// audiobook convention (Audiobookshelf, m4b-tool do the same).
		args = append(args, "--meta-narrator="+narrators, "--meta-composer="+narrators)
	}
	if book.Subtitle != "" {
		args = append(args, "--meta-subtitle="+book.Subtitle)
	}
	// Write the full publisher blurb to both the short (desc) and long (ldes)
	// atoms: players disagree about which one they read, and the provider's
	// own "description" field is a truncated teaser we don't want to embed.
	if blurb := normalizeText(book.Blurb()); blurb != "" {
		args = append(args,
			"--meta-description="+blurb,
			"--meta-long-description="+blurb,
		)
	}
	if len(book.Genres) > 0 {
		args = append(args, "--meta-genre="+book.Genres[0])
	}
	// tone parses this as a DateTime, so a bare year is rejected.
	switch {
	case book.ReleaseDate != "":
		args = append(args, "--meta-recording-date="+book.ReleaseDate)
	case book.Year != "":
		args = append(args, "--meta-recording-date="+book.Year+"-01-01")
	}
	if book.Publisher != "" {
		args = append(args, "--meta-publisher="+book.Publisher)
	}
	if book.SeriesName != "" {
		args = append(args, "--meta-movement-name="+book.SeriesName)
		if book.SeriesPosition != "" {
			// --meta-part, not --meta-movement: MVIN only holds integers and
			// series positions like "1.5" or "1-3" are real.
			args = append(args, "--meta-part="+book.SeriesPosition)
		}
	}
	if book.ASIN != "" {
		args = append(args, "--meta-additional-field=----:com.pilabor.tone:AUDIBLE_ASIN="+book.ASIN)
	}
	// The genre atom holds one value; GENRES keeps them all (sub-genres too),
	// for readers that look — the Plex agent does, for books tagged by hand.
	if all := bookGenres(book); len(all) > 1 {
		args = append(args, "--meta-additional-field=----:com.pilabor.tone:GENRES="+strings.Join(all, "; "))
	}
	if book.LocalID != "" {
		args = append(args,
			"--meta-additional-field=----:com.pilabor.tone:AUDIOBORKER_SOURCE=manual",
			"--meta-additional-field=----:com.pilabor.tone:AUDIOBORKER_ID="+book.LocalID)
	}
	if coverPath != "" {
		args = append(args, "--meta-cover-file="+coverPath)
	}
	if chaptersPath != "" {
		args = append(args, "--meta-chapters-file="+chaptersPath)
	}

	logf("tone tag %s (%d args)", m4bPath, len(args))
	cmd := exec.CommandContext(ctx, t.tone, args...)
	setupProcessKill(cmd)
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		logf("  %s", strings.TrimSpace(string(out)))
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("tone tag failed: %w", err)
	}
	return nil
}

// bookGenres is Genres then SubGenres, deduplicated, without blanks or ";"
// (the GENRES separator).
func bookGenres(book *metadata.Book) []string {
	var out []string
	seen := map[string]bool{}
	for _, g := range append(append([]string(nil), book.Genres...), book.SubGenres...) {
		g = strings.TrimSpace(strings.ReplaceAll(g, ";", ","))
		if g != "" && !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	return out
}

// normalizeText strips carriage returns from free-text tag values so
// Windows-sourced descriptions don't embed CRLF into atoms.
func normalizeText(s string) string {
	return strings.ReplaceAll(s, "\r", "")
}
