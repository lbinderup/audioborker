package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// FileInfo is what the pipeline needs to know about one input file.
type FileInfo struct {
	Path        string
	DurationMs  int64
	BitrateKbps int
	SampleRate  int
	Channels    int
	Codec       string // e.g. "mp3", "aac", "flac"
	Container   string // e.g. "mov,mp4,m4a,3gp,3g2,mj2", "mp3"
	// HeaderMs is the length an overflowed 32-bit header claims (see
	// unwrapDuration); 0 when the header was right. DurationMs is the real one.
	HeaderMs int64
	Chapters []ProbedChapter
	// ChaptersTruncated marks embedded chapters sized to an overflowed
	// header: the last one was stretched to the real end, the rest are lost.
	ChaptersTruncated bool
	// Sidecar holds the chapters.txt next to the file, named SidecarName;
	// SidecarErr says why one there was ignored. Set only by ProbeSource.
	Sidecar     []ProbedChapter
	SidecarName string
	SidecarErr  string
	// Tags is the container-level metadata ffprobe reports. Key spelling is
	// whatever the writing tool used — ffprobe lowercases the standard atoms
	// and passes freeform ones through verbatim — so read it through
	// metadata/embedded rather than indexing it directly.
	Tags map[string]string
}

type ProbedChapter struct {
	Title   string
	StartMs int64
	EndMs   int64
}

// IsAACInMP4 reports whether the file can be stream-copied into an m4b.
func (f FileInfo) IsAACInMP4() bool {
	return f.Codec == "aac" && strings.Contains(f.Container, "mp4")
}

// ProbeFile inspects one audio file with ffprobe — exported for the web
// layer's chapter-preview feature.
func ProbeFile(ctx context.Context, ffprobePath, path string) (*FileInfo, error) {
	return prober{ffprobe: ffprobePath}.probe(ctx, path)
}

// ProbeSource is ProbeFile for a book's source file: it also reads the
// chapters.txt next to it, a chapter source of its own (see sidecar.go).
func ProbeSource(ctx context.Context, ffprobePath, path string) (*FileInfo, error) {
	return prober{ffprobe: ffprobePath}.probeSource(ctx, path)
}

func (p prober) probeSource(ctx context.Context, path string) (*FileInfo, error) {
	info, err := p.probe(ctx, path)
	if err == nil {
		readSidecar(info)
	}
	return info, err
}

type prober struct {
	ffprobe string
}

func (p prober) probe(ctx context.Context, path string) (*FileInfo, error) {
	out, err := exec.CommandContext(ctx, p.ffprobe,
		"-v", "error",
		"-print_format", "json",
		"-show_format", "-show_streams", "-show_chapters",
		path,
	).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("ffprobe %s: %s", path, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("ffprobe %s: %w", path, err)
	}

	var raw struct {
		Format struct {
			FormatName string            `json:"format_name"`
			Duration   string            `json:"duration"`
			BitRate    string            `json:"bit_rate"`
			Size       string            `json:"size"`
			Tags       map[string]string `json:"tags"`
		} `json:"format"`
		Streams []struct {
			CodecType  string `json:"codec_type"`
			CodecName  string `json:"codec_name"`
			SampleRate string `json:"sample_rate"`
			Channels   int    `json:"channels"`
			BitRate    string `json:"bit_rate"`
			TimeBase   string `json:"time_base"`
			DurationTS int64  `json:"duration_ts"`
			NbFrames   string `json:"nb_frames"`
		} `json:"streams"`
		Chapters []struct {
			StartTime string `json:"start_time"`
			EndTime   string `json:"end_time"`
			Tags      struct {
				Title string `json:"title"`
			} `json:"tags"`
		} `json:"chapters"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("ffprobe %s: parse: %w", path, err)
	}

	info := &FileInfo{
		Path:       path,
		Container:  raw.Format.FormatName,
		DurationMs: secondsToMs(raw.Format.Duration),
		Tags:       raw.Format.Tags,
	}
	if kb := parseBitrateKbps(raw.Format.BitRate); kb > 0 {
		info.BitrateKbps = kb
	}
	for _, s := range raw.Streams {
		if s.CodecType != "audio" {
			continue
		}
		info.Codec = s.CodecName
		info.Channels = s.Channels
		info.SampleRate, _ = strconv.Atoi(s.SampleRate)
		if kb := parseBitrateKbps(s.BitRate); kb > 0 {
			info.BitrateKbps = kb // stream bitrate beats container bitrate
		}
		nbFrames, _ := strconv.ParseInt(s.NbFrames, 10, 64)
		if ms, ok := unwrapDuration(s.CodecName, s.TimeBase, s.DurationTS, nbFrames); ok && ms > info.DurationMs {
			info.HeaderMs, info.DurationMs = info.DurationMs, ms
			// ffprobe may have computed the bitrates from the wrapped length
			// as well (this book claimed 13 Mbit/s), so average over the file.
			if size, err := strconv.ParseInt(raw.Format.Size, 10, 64); err == nil && size > 0 {
				info.BitrateKbps = int(size * 8 / ms)
			}
		}
		break
	}
	if info.Codec == "" {
		return nil, fmt.Errorf("%s has no audio stream", path)
	}
	for _, c := range raw.Chapters {
		info.Chapters = append(info.Chapters, ProbedChapter{
			Title:   c.Tags.Title,
			StartMs: secondsToMs(c.StartTime),
			EndMs:   secondsToMs(c.EndTime),
		})
	}
	info.ChaptersTruncated = extendTruncatedChapters(info.Chapters, info.HeaderMs, info.DurationMs)
	return info, nil
}

// extendTruncatedChapters lets the last chapter run to the real end when the
// tool that wrote the file sized its chapters to an overflowed length header:
// Poseidon's Wake carried 2 chapters ending at 2:29 of a 27-hour book. The
// rest of that tool's list is lost, but its audio is no longer left without
// a chapter.
func extendTruncatedChapters(chs []ProbedChapter, headerMs, durationMs int64) bool {
	if headerMs <= 0 || len(chs) == 0 {
		return false
	}
	last := &chs[len(chs)-1]
	if abs64(last.EndMs-headerMs) > 1000 {
		return false
	}
	last.EndMs = durationMs
	return true
}

// unwrapDuration recovers a track length that overflowed its 32-bit header.
// A version-0 mdhd counts the length in samples in 32 bits, so at 44.1 kHz
// anything past 27h03m wraps around — a 27h11m book from an mp4v2-based
// tagger read as 7m58s. ffprobe reports min(header, sample table), so the
// wrapped value is what it prints; players that sum the sample table (VLC)
// show the real length. The frame count survives intact: an AAC frame is
// 1024 samples, which estimates the true length well enough to pick how many
// 2^32 wraps happened. Wraps are 27 hours apart, so the estimate only has to
// land near the right one; requiring 1% agreement keeps a wrong frame-size
// assumption (HE-AAC at 2048 per tick) from "correcting" a healthy file.
func unwrapDuration(codec, timeBase string, durationTS, nbFrames int64) (int64, bool) {
	if codec != "aac" || durationTS <= 0 || nbFrames <= 0 {
		return 0, false
	}
	num, den, ok := strings.Cut(timeBase, "/")
	if !ok {
		return 0, false
	}
	n, err1 := strconv.ParseInt(num, 10, 64)
	d, err2 := strconv.ParseInt(den, 10, 64)
	if err1 != nil || err2 != nil || n <= 0 || d <= 0 {
		return 0, false
	}
	const wrap = int64(1) << 32
	est := nbFrames * 1024
	k := (est - durationTS + wrap/2) / wrap
	if k < 1 {
		return 0, false
	}
	fixed := durationTS + k*wrap
	if abs64(fixed-est) > est/100 {
		return 0, false
	}
	return fixed * 1000 * n / d, true
}

func secondsToMs(s string) int64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(f * 1000)
}

func parseBitrateKbps(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0
	}
	return n / 1000
}

// standardBitrates are the rungs we snap a probed bitrate to.
var standardBitrates = []int{64, 96, 128, 192, 256, 320}

// SnapBitrate picks the nearest standard rung; 0 or negative falls back to 128.
func SnapBitrate(kbps int) int {
	if kbps <= 0 {
		return 128
	}
	best, bestDiff := standardBitrates[0], 1<<30
	for _, r := range standardBitrates {
		d := r - kbps
		if d < 0 {
			d = -d
		}
		if d < bestDiff {
			best, bestDiff = r, d
		}
	}
	return best
}
