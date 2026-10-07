# Audioborker agent for Plex

A Plex metadata agent for audiobook libraries that matches books the way
audioborker does and fills in the metadata audioborker would tag. It is
stand-alone: at runtime it talks to the Audible catalog and Audnexus directly
and never needs the audioborker app. It works on any audiobook, but books
audioborker produced match instantly and keep exactly the values you chose
on its match screen.

It replaces the [Audnexus agent](https://github.com/djdembeck/Audnexus.bundle)
for the same kind of library (Music type, Author → Book).

## Why not the Audnexus agent?

When a filename contains an ASIN (audioborker's default path template writes
`Title [B004K50434].m4b`), the Audnexus agent skips searching entirely and
shows a placeholder result: the ASIN as the name, a score of 100 and the year
**1969** — hardcoded values, not a real match. It then ignores the file and
re-fetches the book from Audnexus. It also:

- appends the catalog subtitle to the title, which is where some publishers
  put marketing copy (*"The Lies of Locke Lamora: The deviously twisty fantasy
  adventure you will not want to put down"*);
- adds every Audible genre *and* sub-category tag;
- always uses its default region unless the path contains `[uk]`;
- ranks search results by title and author only, so editions (abridged,
  dramatized, another narrator) are a coin toss.

This agent instead:

- **reads the file's own tags.** The `AUDIBLE_ASIN` atom audioborker writes is
  the strongest match signal, and for that ASIN the title, authors,
  narrators, series, date, publisher, genre and description written into the
  file win over the catalogs. That's what keeps your per-field choices from
  audioborker's **Metadata…** panel.
- **ranks candidates with audioborker's scoring:** title, author, catalog
  relevance, language, and how closely each edition's runtime matches your
  audio. That last signal is what separates two editions of the same book.
- **only applies a search result automatically when audioborker would have
  pre-selected it.** That means a near-exact runtime, a near-perfect title,
  the right language and no close runner-up. Everything else is left
  unmatched for **Fix Match**, which lists audioborker's ranking with
  runtimes. A wrong match costs far more than a click.
- **fetches metadata the way audioborker does:** a field-by-field merge of
  Audnexus (primary) and the Audible catalog (fills gaps). If Audnexus is
  down, the update fails and Plex keeps the previous metadata rather than
  storing a degraded book.
- **cleans up genres.** Audible sometimes files a book under categories that
  contradict what it is: *The Lies of Locke Lamora* sits in "Relationships,
  Parenting & Personal Development" next to Science Fiction & Fantasy.
  Genres that contradict the book's fiction/non-fiction label (from Audnexus,
  or the majority of Audible's category paths) are dropped along with their
  sub-genres. Plex then gets the remaining genres, best supported first,
  followed by their sub-genres (Fantasy, Epic, Humorous, …). audioborker
  applies the same cleanup to what it writes into files.
- **puts books in series collections.** Plex's agent framework can't set an
  album's collections, so after updating a book the agent adds it to a
  collection named after its series through the Plex server's own API.
  Collections already on the book, including your own, are kept.
- **ignores podcasts.** Audible's search mixes podcast episodes in with books
  (a fan podcast titled "The Last Colony - John Scalzi" outranked the book);
  they are filtered out, here and in audioborker.

## Install

> Plex has said it will remove legacy (Python) agents from new server
> releases in 2026. Its HTTP-based replacement ("custom metadata providers")
> did not support Music libraries at the time of writing. This agent works
> wherever the Audnexus agent still works.

1. Copy the `Audioborker.bundle` folder into Plex's `Plug-ins` folder:
   - Docker (linuxserver/plex and friends):
     `/config/Library/Application Support/Plex Media Server/Plug-ins/`
   - QNAP (App Center package):
     `/share/CACHEDEV1_DATA/.qpkg/PlexMediaServer/Library/Plex Media Server/Plug-ins/`
   - Linux: `/var/lib/plexmediaserver/Library/Application Support/Plex Media Server/Plug-ins/`
   - Windows: `%LOCALAPPDATA%\Plex Media Server\Plug-ins\`
2. Restart Plex Media Server.
3. Edit the audiobook library → **Advanced**:
   - Scanner: `Plex Music Scanner`
   - Agent: `Audioborker`
   - Album art: `Local Files Only` is fine, since the agent supplies the
     cover itself
4. Agent settings (Settings → Agents → Artists/Albums → Audioborker, or in
   the library's advanced settings):
   - **Audible storefront**: use the same region as audioborker's default.
     A book whose ASIN isn't in that storefront is looked up in the others
     automatically.
   - **Apply search results automatically**: `audioborker` (default) or
     `lenient`. Lenient also accepts a clear title + author match when the
     runtime can't be measured. A runtime that clearly disagrees still blocks it.
   - **Add authors as Mood tags** / **Sort authors as Last, First**: same as
     the Audnexus agent, so existing collection setups keep working. Series
     are added as `Series: <name>` moods for the same reason.
   - **Put books in a collection named after their series** (default on).
     A book whose series changes keeps its old collection; remove it in
     Plex if that happens.
5. Switching an existing library from Audnexus: **Manage Library → Refresh All
   Metadata** re-matches every book with the new agent.

## How a book is matched

1. **The book names its ASIN.** This is the `AUDIBLE_ASIN` (or `ASIN`) atom,
   or a `[B0…]` token in the file or folder name. The ASIN is validated
   against the catalogs and matched at once. A `[uk]`-style token picks the
   storefront.
2. **Otherwise, search.** The agent builds the queries audioborker's match
   screen would, stopping at the first that finds anything:
   1. title and author from the file's tags (the Library screen), or from
      Plex's album/artist hints for formats the agent doesn't parse itself;
   2. the folder name, cleaned and split into author and title (the Import
      screen);
   3. the title words alone.
3. **Rank and decide** with audioborker's scoring and auto-select rule, using
   the book's runtime. Plex usually reports per-file durations. Otherwise
   the agent reads the MP4 header. A book with any unknown part counts as
   unknown, never as a partial sum.

**Fix Match**: typing an ASIN (optionally `[uk]`) looks it up directly. Any
other text is searched as keywords, exactly like audioborker's search box. The
book's own ASIN, if it has one, is always listed first.

Authors (artists) are matched through the book's ASIN where possible: the
Audnexus record lists the author's ASIN, which tells apart two authors with
the same name. Otherwise they're found by name, and only an exact, unique
name is applied unattended.

## Troubleshooting

**"Audioborker" doesn't appear in Plex's agent lists.** Plex only lists an
agent once its plug-in has started and registered.

1. Restart Plex Media Server (the whole container on a NAS). Plex looks for
   new bundles only at startup.
2. Check that Plex can read the bundle. Files copied over SMB often end up
   owned by another user than the one Plex runs as: compare
   `ls -la Plug-ins/` and `ls -laR Plug-ins/Audioborker.bundle` with
   Audnexus.bundle, and `chown -R` to match if they differ.
3. Look in `Logs/PMS Plugin Logs/`:
   - `com.plexapp.agents.audioborker.log` exists → the plug-in started. If the
     library failed to load, the agent still registers and this log explains
     why on every search.
   - It doesn't exist → Plex never started the bundle. The reason is in
     `com.plexapp.system.log` (same folder) or `Logs/Plex Media Server.log`.
     Search both for `audioborker`.

**Other problems:**

- Plex's log for the agent: `Logs/PMS Plugin Logs/com.plexapp.agents.audioborker.log`
  in Plex's data folder. Every search logs the hints, runtime, ASINs found,
  each query, and why the top candidate was or wasn't accepted.
- See what the agent would do for a book, without Plex (uses the live APIs):

  ```bash
  python plex/tools/dry_run.py "/library/Scott Lynch/The Lies of Locke Lamora"
  python plex/tools/dry_run.py --album "The Lies of Locke Lamora" --artist "Scott Lynch" --runtime 1319
  python plex/tools/dry_run.py book.m4b --manual "locke lamora"
  ```

## Development

```
Audioborker.bundle/Contents/
  Code/__init__.py              Plex glue only (sandboxed: see the header comment)
  Libraries/Shared/audioborker_plex/
    match.py      port of internal/match (normalize, score, auto-select)
    embedded.py   port of internal/metadata/embedded (+ [ASIN]/[region] path tokens)
    catalog.py    port of the audible/ and audnexus/ clients' parsing
    genres.py     port of internal/metadata/genres.go (genre cleanup)
    aggregate.py  port of internal/metadata/aggregate (merge + error semantics)
    htmltext.py   port of metadata.HTMLToText
    mp4.py        MP4 tag/duration reader producing ffprobe's key names
    fetch.py      retry + rate-limit policy (port of audnexus.getJSON)
    plexlibrary.py  series collections through the Plex server's own API
    core.py       the agent's behaviour, free of Plex objects
tests/                          unittest suite (Python 3)
tools/dry_run.py                the agent without Plex
```

```bash
python -m unittest discover -s plex/tests -t plex/tests
go test ./internal/parity
```

The ported modules must behave exactly like the Go code they mirror.
`internal/parity` runs the Go implementations over a fixed set of cases
(including captured live API responses) and records the results in
`internal/parity/testdata/golden.json`. `tests/test_parity.py` checks the
port against the same file. After changing matching, tag interpretation,
parsing or merging in Go:

```bash
go test ./internal/parity -update   # re-record from Go
python -m unittest discover -s plex/tests -t plex/tests   # then fix the port until it passes
```

Plex runs agents on **Python 2.7**, while the tests run on Python 3, so the
library sticks to the common subset. Text is unicode internally (`compat.text`
at every boundary), struct formats are bytes, and there's no `strptime`
(thread-unsafe on 2.7). A static check:

```bash
uvx vermin --no-tips -t=2.7- -t=3.6- --violations plex/Audioborker.bundle/Contents
```

`tests/test_glue.py` executes `Contents/Code/__init__.py` against stand-ins
for the plug-in framework's globals (shaped after its `agentkit.py`), covering
agent registration, search, update and a library that fails to load.

`tests/fixtures/tagged.m4b` is three seconds of silence tagged with the exact
tone flags audioborker's pipeline uses. Regenerate it with
`tests/fixtures/make-fixture.sh`.
