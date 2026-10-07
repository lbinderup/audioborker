# -*- coding: utf-8 -*-
"""Port of audioborker's internal/metadata/aggregate: merges per-source
records into one book, field by field, so the most complete data wins.
Audnexus is primary for every field and the Audible catalog fills gaps."""
from __future__ import absolute_import, unicode_literals

from . import catalog, genres
from .compat import describe

SOURCE_AUDNEXUS = 'audnexus'
SOURCE_AUDIBLE = 'audible'

DEFAULT_ORDER = (SOURCE_AUDNEXUS, SOURCE_AUDIBLE)

FIELD_LITERATURE_TYPE = 'literature_type'


def blurb(b):
    """Book.Blurb: the full publisher summary when present, else the teaser."""
    return b.get('summary') or b.get('description') or ''


def copy_keys(*keys):
    def copy(dst, src):
        for k in keys:
            v = src.get(k)
            dst[k] = list(v) if isinstance(v, list) else v
    return copy


# (field key, is-empty, copy). Composite fields move dependent values
# together, so a merged book never pairs a series name from one catalog with a
# position from the other.
SPECS = (
    ('title', lambda b: not b.get('title'), copy_keys('title')),
    ('subtitle', lambda b: not b.get('subtitle'), copy_keys('subtitle')),
    ('authors', lambda b: not b.get('authors'), copy_keys('authors')),
    ('narrators', lambda b: not b.get('narrators'), copy_keys('narrators')),
    ('series', lambda b: not b.get('series_name'), copy_keys('series_name', 'series_position')),
    ('release', lambda b: not b.get('year') and not b.get('release_date'), copy_keys('year', 'release_date')),
    ('publisher', lambda b: not b.get('publisher'), copy_keys('publisher')),
    ('language', lambda b: not b.get('language'), copy_keys('language')),
    ('genres', lambda b: not b.get('genres'), copy_keys('genres', 'sub_genres', 'genre_paths')),
    ('description', lambda b: blurb(b) == '', copy_keys('description', 'summary')),
    ('runtime_min', lambda b: not b.get('runtime_min'), copy_keys('runtime_min')),
    ('cover_url', lambda b: not b.get('cover_url'), copy_keys('cover_url')),
    # Not user-facing: only Audnexus has it, and it exists to clean genres.
    (FIELD_LITERATURE_TYPE, lambda b: not b.get('literature_type'), copy_keys('literature_type')),
)

DEFAULT_PRECEDENCE = dict((key, DEFAULT_ORDER) for key, _, _ in SPECS)
# Audible's ladders carry the genre -> sub-genre hierarchy Audnexus flattens.
DEFAULT_PRECEDENCE['genres'] = (SOURCE_AUDIBLE, SOURCE_AUDNEXUS)


def merge(books, overrides=None):
    """aggregate.Merge. For each field an override wins when set and
    non-empty at that source; otherwise the first source in precedence with
    a value. The description is completeness-aware: a source carrying the
    full summary beats one with only the truncated teaser. The merged genres
    are then cleaned (genres.clean_genres)."""
    overrides = overrides or {}
    out = catalog.empty_book()
    out['sources'] = {}
    for src in DEFAULT_ORDER:
        b = books.get(src)
        if b is not None:
            out['asin'], out['region'] = b.get('asin', ''), b.get('region', '')
            break
    for key, empty, copy in SPECS:
        src = pick_source(key, empty, books, overrides.get(key, ''))
        if src == '':
            continue
        copy(out, books[src])
        if key != FIELD_LITERATURE_TYPE:
            out['sources'][key] = src
    out['genres'], out['sub_genres'] = genres.clean_genres(out)
    return out


def pick_source(key, empty, books, override):
    b = books.get(override)
    if b is not None and not empty(b):
        return override
    if key == 'description':
        for src in DEFAULT_PRECEDENCE[key]:
            b = books.get(src)
            if b is not None and b.get('summary'):
                return src
    for src in DEFAULT_PRECEDENCE[key]:
        b = books.get(src)
        if b is not None and not empty(b):
            return src
    return ''


class Result(object):
    def __init__(self, book, per_source, notes, raw_audnexus):
        self.book = book                # merged, with 'sources'
        self.per_source = per_source    # raw parsed records by source
        self.notes = notes              # degradation notes for the log
        self.raw_audnexus = raw_audnexus  # undecoded Audnexus body (rating)


def get_book(fetch, asin, region, audnexus_base=catalog.AUDNEXUS_URL):
    """Aggregator.GetBook semantics:
      - Audnexus (primary) transient error -> raised, so Plex never stores a
        degraded book the user believes is complete
      - Audnexus not found -> continue; an Audible-only merge is allowed
      - Audible failure of any kind -> noted and skipped; the catalog API is
        unofficial and must never block a match
      - nothing anywhere -> catalog.NotFound
    """
    per_source, notes = {}, []
    raw = {}

    def fetch_audnexus(url):
        raw['body'] = fetch(url)
        return raw['body']

    try:
        per_source[SOURCE_AUDNEXUS] = catalog.audnexus_book(fetch_audnexus, asin, region, audnexus_base)
    except catalog.NotFound:
        pass  # this catalog simply doesn't know the ASIN; Audible may
    try:
        per_source[SOURCE_AUDIBLE] = catalog.audible_book(fetch, asin, region)
    except catalog.NotFound:
        pass
    except Exception as e:  # noqa: BLE001 - any Audible failure is a note
        notes.append('audible lookup failed (%s) - its fields are unavailable for this book.' % describe(e))
    if not per_source:
        raise catalog.NotFound('no catalog has a book with ASIN %s in region %s' % (asin, region))
    book = merge(per_source)
    # Identity is the request, never merged — a source echoing a different
    # (e.g. marketplace-redirected) ASIN must not change what gets recorded.
    book['asin'], book['region'] = asin, region
    return Result(book, per_source, notes, raw.get('body'))
