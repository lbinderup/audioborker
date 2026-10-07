package pipeline

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// mergePlan describes how the input files become one m4b.
type mergePlan struct {
	Files       []*FileInfo
	TotalMs     int64
	StreamCopy  bool // all inputs are AAC-in-MP4: no re-encode
	BitrateKbps int  // transcode only
	SampleRate  int  // transcode only, 0 = keep
}

func planMerge(files []*FileInfo, bitrateOverride int) mergePlan {
	p := mergePlan{Files: files, StreamCopy: true}
	for _, f := range files {
		p.TotalMs += f.DurationMs
		if !f.IsAACInMP4() {
			p.StreamCopy = false
		}
	}
	if !p.StreamCopy {
		if bitrateOverride > 0 {
			p.BitrateKbps = bitrateOverride
		} else {
			p.BitrateKbps = SnapBitrate(files[0].BitrateKbps)
		}
		p.SampleRate = files[0].SampleRate
	}
	return p
}

// writeConcatList writes the ffmpeg concat-demuxer input list. Entries must
// be absolute — the demuxer resolves relative entries against the list
// file's own directory, not the working directory. Single quotes in paths
// are escaped per the demuxer's quoting rules.
func writeConcatList(path string, files []*FileInfo) error {
	var b strings.Builder
	b.WriteString("ffconcat version 1.0\n")
	for _, f := range files {
		abs, err := filepath.Abs(f.Path)
		if err != nil {
			abs = f.Path
		}
		escaped := strings.ReplaceAll(filepath.ToSlash(abs), "'", `'\''`)
		fmt.Fprintf(&b, "file '%s'\n", escaped)
	}
	return os.WriteFile(path, []byte(b.String()), 0o666)
}

type ffmpegRunner struct {
	ffmpeg string
}

// merge produces a pure-audio m4b at outPath. Progress is parsed from
// ffmpeg's CR-terminated "time=HH:MM:SS.mm" status lines and reported as
// 0..1 of plan.TotalMs.
func (f ffmpegRunner) merge(ctx context.Context, plan mergePlan, listPath, outPath string, progress func(float64), logf LogFunc) error {
	args := []string{"-hide_banner", "-nostdin", "-y"}

	single := len(plan.Files) == 1
	if single {
		args = append(args, "-i", plan.Files[0].Path)
	} else {
		args = append(args, "-f", "concat", "-safe", "0", "-i", listPath)
	}
	// -map 0:a drops embedded cover-art video streams, which otherwise break
	// concat stream-copying (and we re-embed the cover with tone anyway).
	args = append(args, "-map", "0:a", "-vn", "-map_chapters", "-1", "-map_metadata", "-1")

	if plan.StreamCopy {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-b:a", fmt.Sprintf("%dk", plan.BitrateKbps))
		if plan.SampleRate > 0 {
			args = append(args, "-ar", strconv.Itoa(plan.SampleRate))
		}
	}
	args = append(args, "-movflags", "+faststart", "-f", "ipod", outPath)

	return f.run(ctx, args, plan.TotalMs, progress, logf)
}

// run executes ffmpeg, forwarding its stderr to the job log and translating the
// CR-terminated "time=HH:MM:SS.mm" status lines into 0..1 progress against
// totalMs. Shared by every ffmpeg invocation so they all report progress and
// surface the same error detail.
func (f ffmpegRunner) run(ctx context.Context, args []string, totalMs int64, progress func(float64), logf LogFunc) error {
	logf("ffmpeg %s", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, f.ffmpeg, args...)
	setupProcessKill(cmd)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	cmd.Stdout = cmd.Stderr // nothing useful on stdout
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	var lastLines []string
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	scanner.Split(scanCRorLF)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if ms, ok := parseFFmpegTime(line); ok {
			if totalMs > 0 {
				progress(clamp01(float64(ms) / float64(totalMs)))
			}
			continue // status spam doesn't go to the log
		}
		lastLines = append(lastLines, line)
		if len(lastLines) > 12 {
			lastLines = lastLines[1:]
		}
		logf("  %s", line)
	}

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("ffmpeg failed: %w\n%s", err, strings.Join(lastLines, "\n"))
	}
	progress(1)
	return nil
}

// remuxStripped stream-copies an existing m4b's audio into a new file with
// every tag, chapter and cover dropped. It is both halves of the retag safety
// contract in one pass: the library file is never edited in place, and because
// `tone tag` MERGES metadata rather than replacing it, writing onto a stripped
// copy is the only way a retag cannot leave a stale atom (an old series, a
// wrong ASIN) behind from whatever tagged the file before. The flags are
// deliberately the same ones merge uses, so a retagged file is
// indistinguishable from a freshly converted one.
func (f ffmpegRunner) remuxStripped(ctx context.Context, src, dst string, totalMs int64, progress func(float64), logf LogFunc) error {
	return f.run(ctx, []string{
		"-hide_banner", "-nostdin", "-y", "-i", src,
		"-map", "0:a", "-vn", "-c:a", "copy",
		"-map_chapters", "-1", "-map_metadata", "-1",
		"-movflags", "+faststart", "-f", "ipod", dst,
	}, totalMs, progress, logf)
}

// extractCover pulls a file's own embedded artwork out to the work dir. The
// strip drops the art stream, so without this a failed cover download (or a
// catalog with no image) would silently destroy artwork the file already had.
// Best effort: no art, or art ffmpeg won't demux as an image, yields "".
func extractCover(ctx context.Context, ffmpeg, src, workDir string, logf LogFunc) string {
	dst := filepath.Join(workDir, "cover-existing.jpg")
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-y",
		"-i", src, "-map", "0:v:0", "-frames:v", "1", "-f", "mjpeg", dst)
	setupProcessKill(cmd)
	if err := cmd.Run(); err != nil {
		return ""
	}
	if fi, err := os.Stat(dst); err != nil || fi.Size() == 0 {
		return ""
	}
	logf("cover: kept the artwork already in the file")
	return dst
}

// CoverImage returns the artwork embedded in a media file — its attached
// picture, as JPEG — or nil when it has none. Exported for the web layer's
// manual tagging page, which offers to keep it; the pipeline itself uses
// extractCover.
func CoverImage(ctx context.Context, ffmpeg, path string) []byte {
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "error",
		"-i", path, "-map", "0:v:0", "-frames:v", "1", "-f", "mjpeg", "pipe:1")
	setupProcessKill(cmd)
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return nil
	}
	return out
}

// mergeFdkaac transcodes via a pipe: ffmpeg decodes to WAV on stdout,
// fdkaac encodes to AAC-LC in an .m4a/.m4b container. Used when the user
// selects the fdkaac encoder (better quality at low bitrates than ffmpeg's
// native aac; ffmpeg builds can't legally bundle libfdk_aac).
func (f ffmpegRunner) mergeFdkaac(ctx context.Context, fdkaac string, plan mergePlan, listPath, outPath string, progress func(float64), logf LogFunc) error {
	ffArgs := []string{"-hide_banner", "-nostdin"}
	if len(plan.Files) == 1 {
		ffArgs = append(ffArgs, "-i", plan.Files[0].Path)
	} else {
		ffArgs = append(ffArgs, "-f", "concat", "-safe", "0", "-i", listPath)
	}
	ffArgs = append(ffArgs, "-map", "0:a", "-vn", "-f", "wav")
	if plan.SampleRate > 0 {
		ffArgs = append(ffArgs, "-ar", strconv.Itoa(plan.SampleRate))
	}
	ffArgs = append(ffArgs, "-")

	fdArgs := []string{
		"--ignorelength", // WAV from a pipe has no valid length header
		"-p", "2",        // AAC-LC
		"-b", strconv.Itoa(plan.BitrateKbps * 1000),
		"-o", outPath,
		"-",
	}
	logf("ffmpeg %s | fdkaac %s", strings.Join(ffArgs, " "), strings.Join(fdArgs, " "))

	dec := exec.CommandContext(ctx, f.ffmpeg, ffArgs...)
	enc := exec.CommandContext(ctx, fdkaac, fdArgs...)
	setupProcessKill(dec)
	setupProcessKill(enc)

	pipe, err := dec.StdoutPipe()
	if err != nil {
		return err
	}
	enc.Stdin = pipe
	decErr, err := dec.StderrPipe()
	if err != nil {
		return err
	}
	var encOut strings.Builder
	enc.Stdout, enc.Stderr = &encOut, &encOut

	if err := dec.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}
	if err := enc.Start(); err != nil {
		dec.Process.Kill()
		dec.Wait()
		return fmt.Errorf("start fdkaac: %w", err)
	}

	scanner := bufio.NewScanner(decErr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	scanner.Split(scanCRorLF)
	for scanner.Scan() {
		if ms, ok := parseFFmpegTime(scanner.Text()); ok && plan.TotalMs > 0 {
			progress(clamp01(float64(ms) / float64(plan.TotalMs)))
		}
	}

	decWaitErr := dec.Wait()
	encWaitErr := enc.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if decWaitErr != nil {
		return fmt.Errorf("ffmpeg (decode) failed: %w", decWaitErr)
	}
	if encWaitErr != nil {
		return fmt.Errorf("fdkaac failed: %w\n%s", encWaitErr, strings.TrimSpace(encOut.String()))
	}
	progress(1)
	return nil
}

var timeRe = regexp.MustCompile(`time=(\d+):(\d+):(\d+(?:\.\d+)?)`)

func parseFFmpegTime(line string) (int64, bool) {
	m := timeRe.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	h, _ := strconv.ParseInt(m[1], 10, 64)
	min, _ := strconv.ParseInt(m[2], 10, 64)
	sec, _ := strconv.ParseFloat(m[3], 64)
	return h*3_600_000 + min*60_000 + int64(sec*1000), true
}

// scanCRorLF splits on \n or \r — ffmpeg overwrites its status line with
// bare carriage returns.
func scanCRorLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}
