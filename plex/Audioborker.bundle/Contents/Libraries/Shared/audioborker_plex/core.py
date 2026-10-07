# -*- coding: utf-8 -*-
"""The agent's behaviour, kept free of Plex framework objects so it can be
tested on a desktop Python. Contents/Code/__init__.py only translates between
these functions and Plex.

Matching mirrors audioborker's match screen:

  * A book whose file names its ASIN — audioborker's AUDIBLE_ASIN atom, or a
    "[B0...]" token its default path template writes — matches that ASIN
    directly, the way the Library screen re-matches a file in one click.
  * Otherwise the catalog is searched with the queries the match screen would
    build, candidates are ranked with the same scoring, and a result is only
    accepted automatically when audioborker would have pre-selected it
    (match.auto_select). Everything else is left for Fix Match, which lists
    the same ranking.

Metadata mirrors what audioborker tags: the Audnexus + Audible merge. For a
file audioborker tagged for this very ASIN, the file's own values win — they
carry whatever per-field choices were made on the match screen.
"""
from __future__ import absolute_import, unicode_literals

import datetime
import re

from . import aggregate, catalog, embedded, genres, match, mp4
from .compat import RE_ASCII, describe, text

try:
    from urllib import unquote as url_unquote  # Python 2
except ImportError:
    from urllib.parse import unquote as url_unquote  # novermin

POLICY_AUDIOBORKER = 'audioborker'
POLICY_LENIENT = 'lenient'

# Plex applies an automatic search result only at a score of 80-85 or more,
# so anything the agent is not sure about is kept well below that.
SURE = 100
UNSURE_CAP = 75

MAX_ROWS = 8  # the match screen shows 8 candidates too

BAD_ARTISTS = ('', '[unknown artist]', 'unknown artist', 'various artists', 'various')


class Settings(object):
    def __init__(self, region='us', policy=POLICY_AUDIOBORKER, authors_as_moods=True,
                 sort_author_by_last_name=True, audnexus_url=catalog.AUDNEXUS_URL,
                 series_collections=True):
        region = text(region).lower()
        self.region = region if region in embedded.REGIONS else 'us'
        self.policy = text(policy) or POLICY_AUDIOBORKER
        self.authors_as_moods = bool(authors_as_moods)
        self.sort_author_by_last_name = bool(sort_author_by_last_name)
        self.audnexus_url = text(audnexus_url).strip() or catalog.AUDNEXUS_URL
        self.series_collections = bool(series_collections)


def null_log(msg):
    pass


# --- What the files say ----------------------------------------------------

class Local(object):
    """Everything known about a book from its files and Plex's scanner."""

    def __init__(self):
        self.files = []
        self.tags = {}            # ffprobe-style tags of the first MP4 file
        self.file_book = None     # embedded.book(tags), or None without tags
        self.runtime_min = 0      # 0 = unknown
        self.path_asin = ''
        self.path_region = ''
        self.hint_title = ''      # Plex's album hint (tags it read, or folder)
        self.hint_author = ''
        self.name_for_import = ''  # what audioborker's Import screen would see

    def claimed_asins(self):
        """ASINs the book names for itself, strongest first."""
        out = []
        for a in ((self.file_book or {}).get('asin', ''), self.path_asin):
            if a and a not in out:
                out.append(a)
        return out

    def sort_title(self):
        n = embedded.normalize(self.tags)
        return embedded.first(n, 'sort_album', 'sort_name')


def tree_entries(tree):
    """(file path, duration ms or None) for every part under a Plex MediaTree
    — an album's tracks, or an artist's albums and their tracks — in index
    order."""
    out = []
    if tree is None:
        return out
    for item in getattr(tree, 'items', None) or []:
        for part in getattr(item, 'parts', None) or []:
            path = getattr(part, 'file', None)
            if path:
                out.append((path, ms_or_none(getattr(part, 'duration', None))
                            or ms_or_none(getattr(tree, 'duration', None))))
    for child in sorted_children(tree):
        out.extend(tree_entries(child))
    return out


def first_album_entries(artist_tree):
    """An author's evidence is one of their books; reading every book's header
    for an author with fifty of them would buy nothing."""
    children = sorted_children(artist_tree)
    return tree_entries(children[0]) if children else []


def sorted_children(tree):
    children = list(getattr(tree, 'children', None) or [])

    def key(pair):
        i, node = pair
        try:
            return (0, int(getattr(node, 'index', '')), i)
        except (TypeError, ValueError):
            return (1, 0, i)
    return [node for _, node in sorted(enumerate(children), key=key)]


def ms_or_none(v):
    try:
        n = int(v)
    except (TypeError, ValueError):
        return None
    return n if n > 0 else None


def filename_entries(quoted):
    """Plex's search hint carries one track's path, URL-quoted."""
    if not quoted:
        return []
    # Python 2 hands over utf-8 bytes, which unquote keeps as bytes; text()
    # decodes them wherever the path is used.
    return [(url_unquote(quoted), None)]


def gather(entries, album_hint='', artist_hint='', reader=mp4.read, log=null_log,
           measure=True):
    """measure=False skips reading every MP4 header for a runtime, for
    callers that only need the tags (metadata updates, author searches)."""
    local = Local()
    local.files = [p for p, _ in entries]
    local.hint_title = text(album_hint).strip()
    artist = text(artist_hint).strip()
    local.hint_author = '' if artist.lower() in BAD_ARTISTS else artist

    mp4_files = [p for p in local.files if mp4.is_mp4_path(p)]
    infos = {}
    if mp4_files:
        info = safe_read(reader, mp4_files[0], log)
        infos[0] = info
        if info is not None and info.tags:
            local.tags = info.tags
            local.file_book = embedded.book(info.tags)

    # Runtime: Plex's own part durations when it has them all (no I/O), else
    # the MP4 headers. A partial total would mis-rank candidates, so any gap
    # means unknown — the same rule audioborker's duration probe follows.
    durations = [d for _, d in entries]
    total_ms = 0
    if durations and None not in durations:
        total_ms = sum(durations)
    elif measure and local.files and len(mp4_files) == len(local.files):
        for i, path in enumerate(mp4_files):
            info = infos[i] if i in infos else safe_read(reader, path, log)
            if info is None or info.duration_ms <= 0:
                total_ms = 0
                break
            total_ms += info.duration_ms
    local.runtime_min = int((total_ms + 30000) // 60000)

    if local.files:
        local.path_asin, local.path_region = embedded.path_tokens(local.files[0])
        first = text(local.files[0]).replace('\\', '/')
        if len(local.files) == 1:
            local.name_for_import = match.path_base(first)
        else:
            local.name_for_import = match.path_base(first.rsplit('/', 1)[0]) if '/' in first else first
    return local


def safe_read(reader, path, log):
    try:
        return reader(path)
    except Exception as e:  # noqa: BLE001 - unreadable file = no evidence
        log('Could not read tags from %s: %s' % (text(path), describe(e)))
        return None


# --- Album search ----------------------------------------------------------

class Query(object):
    def __init__(self, keywords='', title='', author='', why=''):
        self.keywords = text(keywords).strip()
        self.title = text(title).strip()
        self.author = text(author).strip()
        self.why = why

    def key(self):
        return (self.keywords, self.title, self.author)

    def signals(self, local, region):
        # handleMatchCandidates: the title hint falls back to the keywords.
        return match.Signals(title=self.title or self.keywords, author=self.author,
                             runtime_min=local.runtime_min,
                             language=match.language_for_region(region))


def plan_queries(local):
    """The searches audioborker's match screen would run for this book, most
    faithful first. Later entries only run when earlier ones find nothing."""
    plan = []
    fb = local.file_book or {}
    if fb.get('title'):
        # Library screen: seeded from what the file claims.
        author = (fb.get('authors') or [''])[0]
        plan.append(Query((author + ' ' + fb['title']).strip(), fb['title'], author, 'file tags'))
    elif local.hint_title:
        # The same, from the tags Plex's scanner read (ID3 and friends).
        plan.append(Query((local.hint_author + ' ' + local.hint_title).strip(),
                          local.hint_title, local.hint_author, 'Plex album/artist'))
    if local.name_for_import:
        # Import screen: the folder (or lone file) name, normalized.
        normalized = match.normalize(local.name_for_import) or local.name_for_import
        author, title = match.split_author_title(normalized)
        plan.append(Query(normalized, title, author, 'folder name'))
    # Last resort: title words alone, as typed into the search box.
    title = fb.get('title') or local.hint_title
    if title:
        words = match.normalize(title) or title
        plan.append(Query(words, '', '', 'title keywords'))
    seen, out = set(), []
    for q in plan:
        if q.keywords and q.key() not in seen:
            seen.add(q.key())
            out.append(q)
    return out


def run_search(fetch, query, local, region, log):
    try:
        results = catalog.audible_search(fetch, region, query.keywords, query.title, query.author)
    except catalog.NotFound:
        results = []
    sig = query.signals(local, region)
    ranked = match.score(results, sig)
    log('Search (%s) keywords=%r title=%r author=%r: %d result(s), runtime %d min'
        % (query.why, query.keywords, query.title, query.author, len(ranked), local.runtime_min))
    return ranked, sig


def search_album(fetch, local, settings, manual=False, manual_text='', log=null_log):
    """Returns Plex result rows: dicts with id, name, year, score."""
    region = local.path_region or settings.region
    if manual:
        return manual_album_rows(fetch, local, settings, region, text(manual_text), log)

    for asin in local.claimed_asins():
        row = asin_row(fetch, asin, region, local, settings, log, try_other_regions=True)
        if row is not None:
            log('Matched by the ASIN the book names (%s)' % asin)
            return [row]

    for query in plan_queries(local):
        ranked, sig = run_search(fetch, query, local, region, log)
        if not ranked:
            continue
        sure = confident(ranked, sig, settings.policy)
        rows = []
        for i, r in enumerate(ranked[:MAX_ROWS]):
            if i == 0 and sure:
                score = SURE
            else:
                score = min(UNSURE_CAP - i, clamp(r['score']))
            rows.append(result_row(r, local, score))
        log('Top candidate %s (%s) is %s' % (ranked[0]['asin'], ranked[0]['title'],
                                            'accepted' if sure else 'left for Fix Match'))
        return rows if not sure else rows[:1]
    return []


TOKEN_RE = re.compile(r'\[([A-Za-z]{2})\]|\b([A-Za-z0-9]{10})\b', RE_ASCII)


def typed_tokens(typed):
    """(asin, region, remaining text) from what was typed into Fix Match. An
    ASIN anywhere in the text is a direct lookup, like the match screen's ASIN
    box (and the Audnexus agent's quick match); "[uk]" picks the storefront."""
    asin, region = '', ''
    for reg, word in TOKEN_RE.findall(typed):
        if reg and reg.lower() in embedded.REGIONS:
            region = region or reg.lower()
        elif word and not asin and embedded.is_asin_token(word.upper()):
            asin = word.upper()
    rest = re.sub(r'\[[A-Za-z]{2}\]', ' ', typed)
    return asin, region, re.sub(r'\s+', ' ', rest).strip()


def append_ranked(rows, row, wanted):
    """Plex sorts Fix Match rows by score, so each row must score below the
    one before it for audioborker's order to survive."""
    if rows:
        wanted = min(wanted, rows[-1]['score'] - 1)
    row['score'] = max(1, wanted)
    rows.append(row)


def manual_album_rows(fetch, local, settings, region, typed, log):
    typed_asin, typed_region, query_text = typed_tokens(typed)
    region = typed_region or region
    if typed_asin:
        # A typed region is deliberate; without one, find the storefront.
        row = asin_row(fetch, typed_asin, region, local, settings, log,
                       try_other_regions=not typed_region)
        return [row] if row is not None else []

    rows, listed = [], set()
    # The book's own claim leads the list, so re-applying it is one click.
    for asin in local.claimed_asins():
        row = asin_row(fetch, asin, region, local, settings, log, try_other_regions=True)
        if row is not None and row['asin'] not in listed:
            append_ranked(rows, row, SURE)
            listed.add(row['asin'])

    queries = [Query(query_text, '', '', 'Fix Match text')] if query_text else plan_queries(local)
    for query in queries:
        ranked, sig = run_search(fetch, query, local, region, log)
        if not ranked:
            continue
        sure = confident(ranked, sig, settings.policy)
        for i, r in enumerate(ranked[:MAX_ROWS]):
            if r['asin'] in listed:
                continue
            append_ranked(rows, result_row(r, local, 0), SURE if (i == 0 and sure) else clamp(r['score']))
            listed.add(r['asin'])
        break
    return rows


def confident(ranked, sig, policy):
    if match.auto_select(ranked, sig):
        return True
    return policy == POLICY_LENIENT and lenient_select(ranked, sig)


def lenient_select(ranked, sig):
    """Opt-in relaxation for books whose runtime cannot be measured or whose
    edition is not in the catalog: everything auto_select demands except the
    exact runtime. A runtime that disagrees still blocks it."""
    if not ranked or sig.title == '':
        return False
    best = ranked[0]
    if best.get('runtime_match') == 'off':
        return False
    if sig.language and sig.language.lower() != text(best.get('language')).lower():
        return False
    hint = match.go_lower(match.ascii_fold(sig.title))
    if match.fuzzy_distance(hint, match.go_lower(match.ascii_fold(best.get('title')))) > 2:
        return False
    if sig.author:
        a = match.go_lower(match.ascii_fold(sig.author))
        dists = [match.levenshtein(a, match.go_lower(match.ascii_fold(x))) for x in best.get('authors') or []]
        if not dists or min(dists) > 2:
            return False
    if len(ranked) > 1 and best['score'] - ranked[1]['score'] < 15:
        return False
    return True


def clamp(score):
    return max(1, min(99, int(score)))


def result_row(r, local, score):
    region = r.get('region') or 'us'
    try:
        year = int(text(r.get('year'))[:4])
    except ValueError:
        year = None
    return {
        'id': '%s_%s' % (r['asin'], region),
        'asin': r['asin'],
        'name': display_name(r, local.runtime_min),
        'year': year,
        'score': score,
    }


def asin_row(fetch, asin, region, local, settings, log, try_other_regions):
    """A result row for a known ASIN, validated against the catalogs. A file
    audioborker tagged may have been matched in another storefront than this
    agent's default, so a not-found retries the other regions."""
    regions = [region]
    if try_other_regions:
        regions += [r for r in embedded.REGIONS if r != region]
    for reg in regions:
        try:
            res = aggregate.get_book(fetch, asin, reg, settings.audnexus_url)
        except catalog.NotFound:
            continue
        except catalog.SourceError as e:
            # The catalogs are down; trust the file rather than leave the book
            # unmatched — the next refresh fetches the metadata.
            log('Lookup of %s failed (%s); trusting the file' % (asin, describe(e)))
            fb = local.file_book or {}
            r = {'asin': asin, 'region': reg, 'title': fb.get('title') or asin,
                 'authors': fb.get('authors') or [], 'narrators': fb.get('narrators') or [],
                 'year': fb.get('year') or '', 'runtime_min': 0}
            return result_row(r, local, SURE)
        b = res.book
        r = dict(b)
        r['region'] = reg
        match.classify_runtime(r, local.runtime_min)
        if reg != region:
            log('%s is not in the %s catalog; found it in %s' % (asin, region, reg))
        return result_row(r, local, SURE)
    log('No catalog knows ASIN %s' % asin)
    return None


def display_name(r, local_min):
    """'The Lies of Locke Lamora · S.Lynch · M.Page · 21h59m ✓' — Fix Match
    shows about 60 characters, so names are shortened to initials."""
    title = text(r.get('title'))
    if len(title) > 40:
        title = title[:38].rstrip() + '…'
    parts = [title]
    authors = r.get('authors') or []
    if authors:
        parts.append(initials(authors[0]))
    narrators = r.get('narrators') or []
    if narrators:
        parts.append(initials(narrators[0]))
    minutes = int(r.get('runtime_min') or 0)
    if minutes:
        badge = {'exact': ' ✓', 'close': ' ≈', 'near': ' ~', 'off': ' ≠'}.get(r.get('runtime_match') or '', '')
        parts.append('%dh%02dm%s' % (minutes // 60, minutes % 60, badge))
    return ' · '.join(parts)


def initials(name):
    """'Arthur Conan Doyle' -> 'A.C.Doyle' (the Audnexus agent's style)."""
    words = text(name).replace('"', '').split()
    if len(words) < 2:
        return text(name)
    return ''.join(w[0] + '.' for w in words[:-1]) + words[-1]


# --- Album metadata --------------------------------------------------------

# Fields an audioborker-tagged file is authoritative for: it holds the values
# that were actually written, including per-field source choices.
# (value that must be present, book keys copied, aggregate field key).
FILE_FIELDS = (
    ('title', ('title',), 'title'),
    ('subtitle', ('subtitle',), 'subtitle'),
    ('authors', ('authors',), 'authors'),
    ('narrators', ('narrators',), 'narrators'),
    ('series_name', ('series_name', 'series_position'), 'series'),
    ('release_date', ('year', 'release_date'), 'release'),
    ('year', ('year', 'release_date'), 'release'),
    ('publisher', ('publisher',), 'publisher'),
    ('language', ('language',), 'language'),
    ('summary', ('description', 'summary'), 'description'),
)


def split_id(metadata_id, default_region):
    asin, _, region = text(metadata_id).partition('_')
    region = region.lower() if region.lower() in embedded.REGIONS else default_region
    return asin, region


def album_metadata(fetch, metadata_id, local, settings, log=null_log):
    """The values to store on a Plex album, as a dict. Raises
    catalog.SourceError when Audnexus is unavailable, so Plex keeps the old
    metadata instead of storing a degraded book."""
    asin, region = split_id(metadata_id, settings.region)
    res = aggregate.get_book(fetch, asin, region, settings.audnexus_url)
    for note in res.notes:
        log(note)
    book = res.book
    from_file = []
    fb = local.file_book if local is not None else None
    if fb and fb.get('asin') == asin:
        for check, keys, field in FILE_FIELDS:
            if fb.get(check) and field not in from_file:
                for k in keys:
                    book[k] = list(fb[k]) if isinstance(fb[k], list) else fb[k]
                book['sources'][field] = 'file'
                from_file.append(field)

    # Genres: the catalogs' cleaned genres, then their sub-genres. A file's
    # own genre leads (it may be a source the user picked in audioborker),
    # unless it contradicts the book's kind: files tagged before genres were
    # cleaned can carry the miscategorized one.
    top = list(book['genres'])
    file_genre = (fb.get('genres') or [''])[0] if (fb and fb.get('asin') == asin) else ''
    if file_genre:
        kind, file_kind = genres.literature_kind(book), genres.genre_kind(file_genre)
        if kind and file_kind and file_kind != kind:
            log('Ignoring the file\'s genre %r: it is a %s category on a %s book' % (file_genre, file_kind, kind))
        else:
            top = [file_genre] + [g for g in top if g != file_genre]
            book['sources']['genres'] = 'file'
            from_file.append('genres')
    if from_file:
        log('Using the file\'s own %s' % ', '.join(from_file))

    sort_title = (local.sort_title() if (local is not None and from_file) else '') or default_sort(book)
    moods = []
    if settings.authors_as_moods:
        moods.extend(a for a in book['authors'] if not re.match(r'.+? -', a))
    if book['series_name']:
        moods.append('Series: ' + book['series_name'])
    return {
        'asin': asin,
        'region': region,
        'title': book['title'],
        'title_sort': sort_title,
        'summary': aggregate.blurb(book),
        'studio': book['publisher'],
        'originally_available_at': release_date(book),
        'genres': dedupe(top + list(book.get('sub_genres') or [])),
        'styles': dedupe(book['narrators']),
        'moods': dedupe(moods),
        'rating': catalog.audnexus_rating(res.raw_audnexus) * 2 or None,
        'poster': book['cover_url'],
        'sources': book.get('sources') or {},
        'from_file': from_file,
        # Plex's album model has no collections; the glue adds the book to one
        # through the server's API instead (plexlibrary.py).
        'collection': book['series_name'] if settings.series_collections else '',
    }


def default_sort(book):
    """'Discworld 1 - The Colour of Magic', the form tone writes, so books
    sort within their series."""
    if book['series_name'] and book['series_position']:
        return '%s %s - %s' % (book['series_name'], book['series_position'], book['title'])
    return book['title']


DATE_RE = re.compile(r'^(\d{4})-(\d{2})-(\d{2})$', RE_ASCII)
YEAR_RE = re.compile(r'^(\d{4})$', RE_ASCII)


def release_date(book):
    # Not strptime: on Python 2 its lazy import races between threads, and
    # Plex runs agent calls in parallel.
    for value, pattern in ((book.get('release_date'), DATE_RE), (book.get('year'), YEAR_RE)):
        m = pattern.match(text(value))
        if m:
            parts = [int(p) for p in m.groups()] + [1, 1]
            try:
                return datetime.date(parts[0], parts[1], parts[2])
            except ValueError:
                continue
    return None


def dedupe(values):
    out = []
    for v in values or []:
        v = text(v).strip()
        if v and v not in out:
            out.append(v)
    return out


# --- Authors ---------------------------------------------------------------

def primary_author(name):
    """'Author One, Author Two' -> 'Author One'; drops contributor suffixes
    like ' - editor'."""
    first_name = text(name).split(',')[0].strip()
    return re.sub(r'\s+-\s+.*$', '', first_name).strip()


def reduce_name(name):
    return re.sub(r'[\s.,\-]', '', match.go_lower(match.ascii_fold(name)))


def search_artist(fetch, artist_hint, local, settings, manual=False, manual_text='', log=null_log):
    name = primary_author(manual_text if manual and manual_text else artist_hint)
    if name.lower() in BAD_ARTISTS:
        return []
    region = (local.path_region if local is not None else '') or settings.region
    rows = []

    # A book that names its ASIN names its authors' ASINs too, which
    # disambiguates two authors with the same name.
    if local is not None:
        for asin in local.claimed_asins():
            try:
                body = fetch(catalog.audnexus_book_url(asin, region, settings.audnexus_url))
            except (catalog.NotFound, catalog.SourceError):
                continue
            want = reduce_name(name)
            best = None
            for a in (body or {}).get('authors') or []:
                if not a.get('asin'):
                    continue
                d = match.levenshtein(want, reduce_name(a.get('name')))
                if d <= 2 and (best is None or d < best[0]):
                    best = (d, a)
            if best is not None:
                log('Author matched through book %s' % asin)
                rows.append({'id': '%s_%s' % (best[1]['asin'], region),
                             'name': text(best[1]['name']), 'score': SURE, 'year': None})
                if not manual:
                    return rows
            break

    try:
        found = catalog.audnexus_author_search(fetch, name, region, settings.audnexus_url)
    except catalog.NotFound:
        found = []
    want = reduce_name(name)
    scored = []
    for i, a in enumerate(found):
        d = match.levenshtein(want, reduce_name(a['name']))
        scored.append((100 - 10 * d - i, d, a))
    scored.sort(key=lambda t: -t[0])
    # Only an exact, unique name is safe to apply unattended: two authors
    # sharing a name is exactly the case a wrong guess would get wrong.
    exact = [t for t in scored if t[1] == 0]
    sure = len(exact) == 1 and scored[0][1] == 0
    listed = set(r['id'].split('_')[0] for r in rows)
    for i, (score, d, a) in enumerate(scored[:MAX_ROWS]):
        if a['asin'] in listed:
            continue
        row = {'id': '%s_%s' % (a['asin'], region), 'name': a['name'], 'year': None}
        if manual:
            append_ranked(rows, row, SURE if (i == 0 and sure) else clamp(score))
        else:
            row['score'] = SURE if (i == 0 and sure) else min(UNSURE_CAP - i, clamp(score))
            rows.append(row)
    if not manual and rows and rows[0]['score'] == SURE:
        return rows[:1]
    return rows


def artist_metadata(fetch, metadata_id, settings, log=null_log):
    asin, region = split_id(metadata_id, settings.region)
    a = catalog.audnexus_author(fetch, asin, region, settings.audnexus_url)
    name = a['name']
    return {
        'title': name,
        'title_sort': sort_name(name) if settings.sort_author_by_last_name else name,
        'summary': a['description'],
        'genres': dedupe(a['genres']),
        'poster': a['image'],
    }


SORT_NAME_RE = re.compile(r'^(.+?)\s+([^\s,]+)(,?\s+(?:[JS]r\.?|III?|IV))?$')


def sort_name(name):
    """'Terry Pratchett' -> 'Pratchett, Terry'; 'Martin Luther King, Jr.' ->
    'King, Martin Luther, Jr.'; single names are left alone."""
    m = SORT_NAME_RE.match(text(name).strip())
    if not m:
        return text(name)
    out = m.group(2) + ', ' + m.group(1)
    if m.group(3):
        out += ', ' + m.group(3).lstrip(', ').strip()
    return out
