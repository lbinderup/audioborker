# audioborker

Self-hosted audiobook converter for the *arr crowd. Point it at a folder of
raw audiobooks (mp3, m4a, m4b, flac, ogg, opus, wav — anything ffmpeg
decodes), match each book against Audible/Audnexus, and get back a clean,
chapterized **`.m4b`** in your library — tagged with author, narrator, series,
cover art and its Audible ASIN, ready for Plex or Prologue.

A spiritual successor to [bragibooks](https://github.com/djdembeck/bragibooks),
rebuilt in Go around ffmpeg + [tone](https://github.com/sandreas/tone), with enhanced
chapterization abilities.

## What it does

- **Import** — browse your input volume (newest first, junk like `@eaDir` and
  `.DS_Store` hidden), tick the books to process. Multi-disc layouts (`CD1`,
  `Disc 2`, …) and numbered files are merged in the right order (natural sort:
  `file_2` before `file_10`). Selecting a folder *and* something inside it
  collapses to one job rather than two.
- **Match** — every selection is auto-searched against the Audible catalog
  with scored candidates (cover, narrator, runtime). Custom search, manual
  ASIN entry with live validation, and per-book region, cleanup and chapter
  overrides. Drop books from the batch with **Remove**. Nothing is queued
  until you confirm.
- **Metadata aggregation** — book metadata is merged field-by-field from
  Audnexus *and* the Audible catalog, so a missing series, description or
  low-res cover in one source is filled from the other. Every field's origin
  is recorded on the job, and a **Metadata…** panel on the match screen shows
  both catalogs side by side and lets you pick the source per field where they
  disagree. Genres are cleaned up on the way: a category that contradicts
  the book's fiction/non-fiction label (Audible files *The Lies of Locke
  Lamora* under "Relationships, Parenting & Personal Development") is
  dropped, and the best-supported genre is the one written into the file.
- **Tag by hand** — for an edition no catalog has, the match dialog switches
  to a prompt you copy into any LLM: paste its JSON reply, review the fields,
  pick a cover (the file's, another edition's, or one you drop in), and the
  book is matched like any other, queued with the rest of the batch.
- **Chapter preview** — before converting, play the source straight from the
  server (HTTP Range streaming, so it works remotely too) and click any
  embedded chapter to hear whether it lines up. Load the Audible chapter
  timings beside them for comparison, then pick a side with **Use these
  chapters** — or straight from the match list, where each book lists its
  chapter sources with their counts (in the file, `.chapters.txt`, Audible)
  and the selected one is what gets embedded — decided by the same code the
  conversion runs, with a warning when it had to fall back (e.g. Audible's
  chapters expect 8h18m but your audio is 6h57m).
- **Convert** — a persistent queue with **live progress**, per-job logs,
  cancel and retry. Jobs interrupted by a restart are marked as such — nothing
  hangs in "Processing" forever. Already-AAC sources are stream-copied without
  re-encoding; everything else is transcoded to AAC at the source bitrate
  (snapped to 64–320 kbps, overridable).
- **Chapters, always embedded** — in both Nero (`chpl`) and QuickTime formats.
  Audible chapter data is used when its runtime matches your audio, otherwise
  the file's own chapters, otherwise chapters derived from file boundaries with
  cleaned filename titles. A multi-file book never ends up chapterless.
  A `Book.chapters.txt` next to the file is a third choice beside the
  file's own chapters and Audible's (and the default when the file's are
  missing or cut short); a book that came with one gets it rewritten to
  match what was embedded. Optionally writes that sidecar for every book.
- **Chapter title mix-and-match** — when your rip's *timings* are right but its
  chapter *names* are junk ("Track 01"), put Audible's titles onto your file
  boundaries or the file's own embedded timings. Counts are matched strictly
  (short "Opening/End Credits" stingers the rip lacks are skipped
  automatically); on any mismatch it falls back to the automatic decision.
- **Chapter shift** — the opposite case: names right, timings off. Shift
  whichever chapters get embedded — Audible's or the file's own — by a fixed
  offset, or interpolated between two anchor chapters for drift that grows
  over the book. Clicking a chapter plays it where the shift puts it, or
  pause where a chapter really begins and set the shift from the player.
  File boundaries are exact and never shifted.
- **Books past 27 hours** — some taggers store a track's length in 32 bits,
  which overflows at about 27 hours, so players show such a book as a few
  minutes. audioborker measures the true length, flags affected files on the
  match list, plays them in full in the preview, and writes output with
  correct headers.
- **Library — retag what you already have** — browse the output volume (or
  filter the whole thing from one search box), pick one or more `.m4b` files,
  and rewrite their tags and chapters through the same match screen imports
  use. Files this app produced re-match in one click from the ASIN in their own
  atoms; files from other tools are recognized too. Optionally rename and move
  them to the current path template, with an old → new preview per file. The
  audio is never re-encoded, and the file is never edited in place: it is
  stream-copied, tagged, verified, and only then swapped in — so a failure or a
  cancel always leaves the original untouched. **Sort selected** moves files
  to the path template by the tags they already have, without a retag —
  previewed first, never over another file.
- **Output where you want it** — a dedicated `/output` volume with a safe path
  template (`{author}/{series_name}/{title}/{title} [{asin}]` by default; also
  `{narrator}`, `{subtitle}`, `{series_position}`, `{year}`). Missing variables
  drop cleanly — a standalone book simply skips its series folder level, with
  no ` - Title` stubs and no empty folders. Text inside a variable's braces
  only appears with a value, and `:00` zero-pads a number:
  `{Book series_position:00 - }{title}` is `Book 01 - Title` in a series and
  plain `Title` without one. The settings page previews both a series book
  and a standalone as you type.
- **Source cleanup you control** — after a *verified* conversion: leave the
  sources, move them into the mapped `/completed` volume (optionally a
  subfolder of it), or delete them (only the audio files it consumed; other
  files are left alone). Global default plus per-book override.

## Quick start

A multi-arch image (amd64 + arm64) is published to GitHub's container registry
on every push to `main`:

```yaml
services:
  audioborker:
    image: ghcr.io/lbinderup/audioborker:latest
    environment:
      - PUID=1000   # must read /input and write /output
      - PGID=1000
      - TZ=Etc/UTC
    volumes:
      - /appdata/audioborker:/config       # local disk only (not NFS/SMB)
      - /downloads/audiobooks:/input
      - /library/audiobooks:/output
      - /downloads/processed:/completed   # optional, for "move" cleanup
    ports:
      - "8684:8684"
    restart: unless-stopped
```

Open `http://nas:8684`, check **Settings**, then **Import → Match → Queue**.

- Full compose example: [docker-compose.example.yml](docker-compose.example.yml)
- **QNAP / Container Station walkthrough: [docs/qnap.md](docs/qnap.md)**
  (with [docker-compose.qnap.yml](docker-compose.qnap.yml))

Building it yourself instead:

```bash
docker build -t audioborker .
docker buildx build --platform linux/amd64,linux/arm64 -t audioborker:latest .
```

## Configuration

Runtime settings (directories, path template, cleanup mode, metadata region,
bitrate, encoder, worker concurrency, chapters.txt sidecar) live in the web UI
under **Settings** and persist in `/config/audioborker.db`.

Environment variables (container-level):

| Variable | Default | Purpose |
|---|---|---|
| `PUID` / `PGID` | `1000` | Runtime user; use ids that own your media |
| `UMASK` | `002` | Permissions for created files (`002` = group-writable `664`/`775`; use `022` to keep them owner-only) |
| `SUPPLEMENTARY_GIDS` | unset | Extra group ids for the runtime user, comma separated |
| `TZ` | `Etc/UTC` | Timestamps in logs/UI |
| `PORT` | `8684` | HTTP port |
| `CONFIG_DIR` / `INPUT_DIR` / `OUTPUT_DIR` / `COMPLETED_DIR` | `/config` `/input` `/output` `/completed` | Volume mount points |
| `CONVERTER` | `real` | `fake` simulates conversions (UI development) |
| `FFMPEG_PATH` / `FFPROBE_PATH` / `TONE_PATH` / `FDKAAC_PATH` | on `PATH` | Binary overrides |

### Encoders

The default encoder is ffmpeg's native `aac`, which is fine for spoken word at
the 64k+ bitrates this app uses. The image also ships the standalone `fdkaac`
encoder (better psychoacoustics at low bitrates) — select it under Settings →
Conversion. No redistributable ffmpeg build may bundle libfdk_aac, which is
why it is a separate binary.

### Notes

- **No authentication.** This is a single-user LAN tool; put it behind your
  reverse proxy / VPN like the rest of your *arr stack. For SSE progress
  updates through nginx, disable proxy buffering for `/events`
  (`proxy_buffering off;`).
- `/config` should be local disk, not an NFS/SMB mount — SQLite WAL and
  network filesystems don't mix.
- Audible catalog search is an unofficial API. If it ever breaks, manual ASIN
  entry keeps working (metadata comes from Audnexus).
- Region matters: an ASIN from audible.co.uk does not exist in the `us` region.
  Set your default region in Settings and per-book on Match.

## Plex agent

[`plex/Audioborker.bundle`](plex/README.md) is a Plex metadata agent for
audiobook libraries that matches books with audioborker's own logic: the ASIN
in a file's tags or name first, then the same search, runtime-aware scoring
and strict auto-select as the match screen. It fills in the same merged
Audnexus + Audible metadata, and for files audioborker tagged, the values in
the file win. It runs stand-alone inside Plex and works on books that never
passed through audioborker too. See [plex/README.md](plex/README.md).

## Development

```bash
go run ./cmd/audioborker   # http://localhost:8684
go test ./...
```

On a non-Linux dev machine the app enables template hot-reloading and picks the
converter automatically: the real pipeline when ffmpeg, ffprobe and tone all
resolve, otherwise a simulated one so the whole UI still works. Fixture books:
`testdata/make-fixtures.ps1`. See [AGENTS.md](AGENTS.md) for architecture and
the invariants worth knowing before changing anything.

Architecture in one paragraph: `internal/scan` lists and orders input files;
`internal/metadata` talks to the Audible catalog (search + product details) and
Audnexus (books + chapters) behind a provider interface, with
`internal/metadata/aggregate` merging the per-source records field by field
with provenance; `internal/match` normalizes names and
scores candidates; `internal/queue` runs a persistent job queue (SQLite via
`internal/store`) with worker goroutines and an SSE broker; `internal/pipeline`
probes with ffprobe, merges/transcodes with ffmpeg, resolves chapters, tags
with tone, then verifies the result before moving it into the library;
`internal/web` is a server-rendered html/template + htmx UI. The
`pipeline.Converter` interface separates the queue from the conversion, which
is what makes the simulated converter possible.

## License

GPL-3.0-or-later — see [LICENSE](LICENSE). The same license the rest of the
*arr ecosystem uses.

Third-party components and their licenses are listed in
[THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md). Note that publishing a built
Docker image redistributes GPLv3 FFmpeg, which carries its own source-offer
obligation.

audioborker is not affiliated with Audible, Amazon, Audnexus, or the *arr
projects.
