# AGENTS.md

Guidance for AI coding agents working in this repository. Humans: see
[README.md](README.md) for what the app does and how to run it.

## What this is

A single-binary Go web app that converts audiobooks into standardized,
chapterized `.m4b` files with metadata from Audible/Audnexus. It is a
replacement for [bragibooks](https://github.com/djdembeck/bragibooks) and
deploys as a Docker container on a NAS alongside the *arr suite.

## Architecture

```
cmd/audioborker/main.go   wiring, graceful shutdown, converter selection
internal/
  config/     env-derived process config (paths, ports, binary locations)
  store/      SQLite (modernc.org/sqlite, CGO off): settings, jobs, metadata cache
  metadata/   Provider interface; audible/ = catalog search + product details,
              audnexus/ = books + chapters; aggregate/ = field-by-field merge
              of the per-source records, with provenance (Book.Sources);
              embedded/ = a local file's own atoms read as a Book;
              genres.go = drops genres contradicting fiction/nonfiction
  match/      filename normalization + candidate scoring (pure functions)
  scan/       input listing, junk filtering, natural sort, disc ordering, selection dedupe
  pathtmpl/   {author}/{title} output templates with safe segment dropping
  pipeline/   Converter interface; real.go = ffmpeg+tone, fake.go = simulation
  queue/      worker pool, job state machine, cancellation registry, SSE broker
  web/        net/http mux, handlers, embedded html/template + htmx assets
  parity/     test-only: golden fixtures pinning what plex/ ports from the above
plex/
  Audioborker.bundle/  Plex legacy (Python 2.7) metadata agent; Contents/Code is
                       sandboxed glue, Contents/Libraries/Shared/audioborker_plex
                       is the logic, ported from match/, metadata/embedded,
                       metadata/aggregate and the catalog clients
  tests/, tools/       Python 3 unittest suite; dry_run.py runs the agent without Plex
```

Data flow: **Import** (pick files) → **Match** (choose ASIN) → **Queue**
(persistent jobs) → **pipeline** (probe → plan → merge → chapters → tag →
move → verify → cleanup).

**Library** reuses that whole flow against the *output* volume: pick existing
`.m4b` files → the same Match screen (`Root: "library"`) → retag jobs
(`JobOptions.Kind == store.KindRetag`) → `pipeline.runRetag` (probe → plan →
copy → chapters → tag → verify → replace).

## Non-obvious invariants — do not break these

- **`pipeline.Converter` is the seam.** `FakeConverter` lets the entire web UX
  run on a machine without ffmpeg/tone. Keep the real and fake implementations
  interchangeable; never let queue code reach into ffmpeg specifics.
- **Jobs snapshot their inputs.** `metadata_json` / `options_json` on a job are
  point-in-time copies, so retries reproduce exactly and settings edits never
  mutate in-flight jobs. Do not "helpfully" re-read live settings in the pipeline.
- **Chapter decisions are computed in exactly one place.** The match screen's
  verdict line calls `pipeline.PlanChapters`, which calls the same
  `resolveChapters` the conversion runs. Never fork this logic for display.
  The title mix modes (`titles-files`/`titles-existing` — provider titles on
  local timings) live inside `resolveChapters` too, and degrade to the
  automatic decision on any alignment failure, never to zero chapters.
- **Only raw per-source records go in `metadata_cache`, never merged books.**
  The merge (`aggregate.Merge`) is a pure function over the cached records, so
  per-field overrides and precedence changes need no cache invalidation. The
  cache PK is `(source, asin, region)`. The exception is a raw record gaining
  fields the merge needs: then a migration blanks `book_json` (an empty value
  reads as a miss) so records are re-fetched, as 0003 did for the genre
  hierarchy. Chapter data is left alone.
- **Genres are cleaned inside `aggregate.Merge`, nowhere else.** Audible
  miscategorizes books (Locke Lamora under "Relationships, Parenting &
  Personal Development"), so `metadata.CleanGenres` drops top-level genres
  contradicting the book's kind (Audnexus' `literatureType`, else the majority
  of Audible's category ladders) with their sub-genres, never emptying the
  list, and orders by ladder support — `Genres[0]` is what tone writes.
  Genres are the one field where Audible takes precedence: only its ladders
  (`Book.GenrePaths`) tie a sub-genre to its genre; Audnexus flattens them.
  Classification covers the English storefronts' category names only;
  unknown names are never dropped.
- **Podcasts are not books.** The Audible search returns podcast episodes
  among results (and Audnexus refuses them); `audible.Search` filters them by
  `content_type` / `content_delivery_type`.
- **An Audible catalog failure must never block queueing.** The aggregator
  treats the audible source as best-effort (error → note), while an Audnexus
  transient error fails the queue action — jobs must not silently snapshot a
  degraded book.
- **A retag never edits a library file in place, and verifies before
  replacing.** `runRetag` stream-copies the audio into staging, tags *that*,
  probes it, and only then renames it over the original — so a failure, a
  cancel or a crash always leaves the user's file exactly as it was. Never
  reorder verify after replace, and never use `moveFile` for the swap: its
  copy+delete fallback would write a partial file over a good one
  (`replaceFile` requires a real rename for this reason).
- **The retag's copy is what makes tone's merge semantics safe.** `tone tag`
  merges rather than replaces, so retagging a file that still had its old atoms
  would leave stale values behind (a book that loses its series keeps `©mvn`).
  The copy is an ffmpeg stream-copy with `-map_metadata -1 -map_chapters -1`,
  so tone always writes onto a blank file. For the same reason the conversion's
  merge stage must never "optimize" a single AAC input back into a raw byte
  copy — that bug shipped once and carried foreign atoms (ISBN, RATING) into
  the library.
- **Cleanup is forced to `leave` for retag jobs.** `cleanupSource` resolves
  `InputDir + InputPath`, which for a retag *is* the file just written — a
  `delete` default would destroy the book it had just fixed.
- **A retag's chapters come from the pre-copy probe.** The staged copy has no
  chapters left, and the file's own timings are exactly what
  `titles-existing` exists to reuse.
- **Candidate ranking weighs title, author, runtime and language**
  (`match.Signals`). Runtime is measured from the actual files and separates
  abridged/unabridged editions, but is weighted *below* the author match so a
  coincidental duration never outranks the right author. `match.AutoSelect`
  pre-ticks a candidate only when runtime is near-exact, the title is a
  near-perfect hit, the language matches the region, and the runner-up is
  clearly worse — a confidently wrong pre-selection costs far more than a
  click, so keep it strict.
- **Multi-file input must never produce zero chapters.** Fallback order is
  provider (runtime-validated) → file's own → file boundaries → single chapter.
- **ffmpeg concat lists need absolute paths.** The demuxer resolves relative
  entries against the list file's directory, not the working directory.
- **tone args must be `--flag=value` single tokens.** Values starting with `-`
  (the `----:` freeform atom prefix) break its parser otherwise, and
  `--meta-recording-date` needs a full date, not a bare year.
- **Never write scratch files into the input tree** (bragibooks wrote cover.jpg
  there). Covers/concat lists live in `/config/work/{jobID}`; the staged m4b
  lives under the output volume so the final move is an atomic rename.
- **Path templates drop empty segments.** A standalone book skips its
  `{series_name}` folder; separators collapse. Substitution is single-pass so
  values containing `{token}` text are never re-substituted.
- **`/config` must be local disk.** SQLite WAL breaks on NFS/SMB.
- **Create files with permissive modes (`0o666` / `0o777`), never `0o644`.**
  The container sets a umask (default 002) and the OS subtracts from these
  modes; a umask can only clear bits, so hardcoding 0644 would make output
  files non-group-writable no matter how the NAS is configured — which breaks
  shared media libraries where a human account must also manage the files.
- **Selections are deduped** (`scan.DedupeSelection`): choosing a folder and a
  file inside it is one job, not two. Enforced server-side, not just in the UI.
- **The Plex agent is a port, pinned by parity fixtures.** `plex/` re-implements
  `match` (normalize, score, auto-select), `metadata/embedded`,
  `metadata/aggregate`, `metadata.HTMLToText` and the catalog clients' parsing
  in Python, so Plex matches books exactly as the match screen does.
  `go test ./internal/parity` fails on any behaviour change there; re-record
  with `-update` *only* together with porting the change until
  `python -m unittest discover -s plex/tests -t plex/tests` passes. Go-specific
  semantics are reproduced on purpose (byte lengths in `fuzzyDistance` and
  `longest`, per-rune `ToLower`, ASCII-only regex classes) — don't "simplify"
  them away on either side.
- **The agent only auto-applies what audioborker would pre-select.** An ASIN the
  book names (atom or `[B0…]` path token) matches directly; a search result is
  applied only when `match.AutoSelect` (or the opt-in lenient rule) holds, and
  is otherwise scored below Plex's auto-match threshold so it waits for Fix
  Match. For a file tagged for the matched ASIN, the file's own values win —
  they carry the user's per-field source choices.
- **`plex/` runs on Python 2.7 inside Plex's sandbox.** `Contents/Code` is
  RestrictedPython: no `_`-prefixed names, no `sum`/`any`/`all`, no augmented
  assignment on attributes — keep it glue. Logic lives in
  `Contents/Libraries/Shared` (unrestricted, `PlexPluginCodePolicy` Elevated)
  and stays in the 2.7/3 common subset: unicode internally via `compat.text`,
  bytes `struct` formats, no `strptime`. Plex's sandbox `__import__` returns a
  package that is already in `sys.modules` without loading the submodules
  named in `from pkg import x` — so `audioborker_plex/__init__.py` imports
  every submodule eagerly. Keep it that way: dropping it made a second
  `from audioborker_plex import …` fail inside Plex, and the agent never
  registered. `tests/test_glue.py` emulates that importer from a cold start.

## Conventions

- Server-rendered `html/template` + htmx. **No build step, no npm.** htmx and
  its SSE extension are vendored in `internal/web/static/`.
- **`hx-vals="js:..."` expressions MUST be object literals** (may use `...spread`
  for merging). htmx 2's evaluator silently rejects anything that doesn't start
  with `{` — `js:Object.assign(...)` or `js:someFn()` kills the request with an
  uncaught SyntaxError and the button appears dead. This was a real shipped bug;
  don't reintroduce it.
- SSE events carry pre-rendered HTML fragments; the broker itself knows nothing
  about templates. Slow subscribers drop events (progress is idempotent).
- Keep the dependency list tiny — currently only `modernc.org/sqlite` and
  `github.com/google/uuid`. Prefer stdlib; justify any addition.
- Comments explain *why*, not *what*. Several comments reference specific
  bragibooks bugs this code exists to avoid — keep that context if you edit them.
- Errors surfaced to the user should say what to do about it, not just what failed.

## Development

```bash
go run ./cmd/audioborker    # http://localhost:8684
go test ./...
gofmt -l .                 # must be empty
go vet ./...
python -m unittest discover -s plex/tests -t plex/tests   # Plex agent (Python 3)
uvx vermin --no-tips -t=2.7- -t=3.6- --violations plex/Audioborker.bundle/Contents
```

On non-Linux dev machines the app enables template hot-reloading and
auto-detects the toolchain: it uses the real pipeline when ffmpeg, ffprobe and
tone all resolve, otherwise falls back to `CONVERTER=fake`. Override with
`CONVERTER`, `FFMPEG_PATH`, `FFPROBE_PATH`, `TONE_PATH`, `FDKAAC_PATH`.

Audio fixtures for pipeline work: `testdata/make-fixtures.ps1` generates
sine-tone books (multi-mp3, multi-disc m4a, single m4b).

`devdata/` is git-ignored scratch space: `devdata/config` (db + logs),
`devdata/input`, `devdata/output`, `devdata/tools` (drop `tone.exe` here and
it is picked up automatically).

## Testing expectations

- Pure logic (`pathtmpl`, `scan`, `match`, chapter resolution, bitrate snapping)
  is table-driven unit tested. Add cases there rather than testing via HTTP.
- API clients are tested against `httptest` servers using fixtures captured
  from the real Audnexus and Audible catalog responses — keep those shapes
  accurate: re-capture from the live APIs rather than hand-editing them.
- Changes visible in the browser should be verified by actually driving the
  running app, not assumed. Real conversions can be verified with
  `tone dump <file> --format json --query '$.meta.chapters'`.
- The Plex agent can't be run here; `python plex/tools/dry_run.py <book>` runs
  its search and metadata logic against the live APIs instead.
  `internal/parity/testdata/api/` holds live captures shared by the Go parity
  test and the Python tests — re-capture, don't hand-edit.

## External tools (runtime dependencies, invoked as subprocesses)

`ffmpeg`/`ffprobe` (decode, merge, transcode, verify), `tone` (writes the mp4
atoms ffmpeg cannot: narrator, series, `stik`, freeform ASIN, chapters in both
Nero and QuickTime formats), optional `fdkaac`. These are separate processes —
never link against them, and keep invocation details inside `internal/pipeline`.
