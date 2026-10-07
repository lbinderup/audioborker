// Package config holds process-level configuration sourced from the
// environment. Runtime-editable settings live in the database instead
// (internal/store settings) and are merely seeded from these values.
package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
)

type Config struct {
	Port         int
	ConfigDir    string // db, logs, work dir
	InputDir     string
	OutputDir    string
	CompletedDir string // mount root for the "move" cleanup mode
	Converter    string // "real" | "fake"
	Dev          bool   // reload templates from disk per request
	Version      string

	// External binaries (real converter). Bare names resolve via PATH.
	FFmpegPath  string
	FFprobePath string
	TonePath    string
	FdkaacPath  string
}

func Load(version string) Config {
	// On a dev machine (anything but linux) default to template reloading;
	// the docker image is linux and gets production defaults.
	isDev := runtime.GOOS != "linux"

	// Dev convenience: a tone binary dropped into devdata/tools is picked up
	// without setting TONE_PATH.
	toneDefault := "tone"
	if isDev {
		if local := filepath.Join("devdata", "tools", "tone.exe"); fileExists(local) {
			toneDefault = local
		}
	}

	cfg := Config{
		Port:        envInt("PORT", 8684),
		Dev:         envBool("DEV", isDev),
		Version:     version,
		FFmpegPath:  envStr("FFMPEG_PATH", ffBinDefault("ffmpeg", isDev)),
		FFprobePath: envStr("FFPROBE_PATH", ffBinDefault("ffprobe", isDev)),
		TonePath:    envStr("TONE_PATH", toneDefault),
		FdkaacPath:  envStr("FDKAAC_PATH", "fdkaac"),
	}

	// Converter default: real whenever the full toolchain is reachable, fake
	// otherwise (UI development without ffmpeg/tone installed).
	defConverter := "real"
	if isDev && !(binaryAvailable(cfg.FFmpegPath) && binaryAvailable(cfg.FFprobePath) && binaryAvailable(cfg.TonePath)) {
		defConverter = "fake"
	}
	cfg.Converter = envStr("CONVERTER", defConverter)
	if runtime.GOOS == "linux" {
		cfg.ConfigDir = envStr("CONFIG_DIR", "/config")
		cfg.InputDir = envStr("INPUT_DIR", "/input")
		cfg.OutputDir = envStr("OUTPUT_DIR", "/output")
		cfg.CompletedDir = envStr("COMPLETED_DIR", "/completed")
	} else {
		// Developer machine: keep everything under ./devdata so `go run` works
		// without any setup.
		cfg.ConfigDir = envStr("CONFIG_DIR", filepath.Join("devdata", "config"))
		cfg.InputDir = envStr("INPUT_DIR", filepath.Join("devdata", "input"))
		cfg.OutputDir = envStr("OUTPUT_DIR", filepath.Join("devdata", "output"))
		cfg.CompletedDir = envStr("COMPLETED_DIR", filepath.Join("devdata", "completed"))
	}
	return cfg
}

func (c Config) DBPath() string  { return filepath.Join(c.ConfigDir, "audioborker.db") }
func (c Config) LogsDir() string { return filepath.Join(c.ConfigDir, "logs") }

// CoversDir holds cover images chosen by hand in the manual tagging flow.
// Not the work dir: that is per job and removed when the job ends, and a
// retry must still find the image.
func (c Config) CoversDir() string { return filepath.Join(c.ConfigDir, "covers") }
func (c Config) WorkDir() string   { return filepath.Join(c.ConfigDir, "work") }

// ffBinDefault resolves ffmpeg/ffprobe. On dev machines a parent process
// started before a winget install may carry a stale PATH, so fall back to
// searching winget's package directory for the Gyan.FFmpeg layout.
func ffBinDefault(name string, isDev bool) string {
	if !isDev || runtime.GOOS != "windows" {
		return name
	}
	if _, err := exec.LookPath(name); err == nil {
		return name
	}
	for _, pattern := range []string{
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "WinGet", "Links", name+".exe"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "WinGet", "Packages", "Gyan.FFmpeg*", "ffmpeg-*", "bin", name+".exe"),
	} {
		if matches, _ := filepath.Glob(pattern); len(matches) > 0 {
			return matches[len(matches)-1] // highest version last, lexically
		}
	}
	return name
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// binaryAvailable reports whether the binary resolves: either an existing
// explicit path or a name found on PATH.
func binaryAvailable(bin string) bool {
	if fileExists(bin) {
		return true
	}
	_, err := exec.LookPath(bin)
	return err == nil
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
