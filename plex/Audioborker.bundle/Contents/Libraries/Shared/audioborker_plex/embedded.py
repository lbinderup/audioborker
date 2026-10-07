# -*- coding: utf-8 -*-
"""Port of audioborker's internal/metadata/embedded: interprets the tags a
file already carries as a book. A file on disk is just another metadata
source — an ASIN in a file's own atoms is what lets an audioborker-tagged
book match in one step, and a file from another tool usually still carries
title and author.

Also home to the path-token conventions (an ASIN or region code in square
brackets in a file or folder name), which audioborker writes through its
default path template and the Audnexus agent established for manual hints.
"""
from __future__ import absolute_import, unicode_literals

import re

from .compat import RE_ASCII, text, utf8_len

REGIONS = ('us', 'ca', 'uk', 'au', 'fr', 'de', 'jp', 'it', 'in', 'es')

BRACKET_TOKEN_RE = re.compile(r'\[([^\[\]]+)\]', RE_ASCII)
ALNUM_RE = re.compile(r'^[A-Za-z0-9]*$', RE_ASCII)


def valid_asin(s):
    """metadata.ValidASIN: 10 ASCII letters/digits."""
    s = text(s)
    return len(s) == 10 and ALNUM_RE.match(s) is not None


def book(tags):
    """embedded.Book: container tags as a book dict. Absent fields stay empty —
    a tag nobody wrote is unknown, not blank."""
    n = normalize(tags)
    b = {
        # Every metadata.Book field is present, so a file book can stand in for
        # a catalog record anywhere one is expected.
        'region': '', 'description': '', 'runtime_min': 0, 'cover_url': '',
        'sub_genres': [], 'genre_paths': [], 'literature_type': '',
        'asin': asin(tags),
        'title': first(n, 'title', 'album'),
        'subtitle': first(n, 'subtitle'),
        'publisher': first(n, 'publisher', 'label'),
        'language': first(n, 'language'),
        # Series lives in a freeform SERIES atom (what tone writes) or the
        # movement name; the position is PART for audioborker's files and
        # SERIES-PART for several other taggers.
        'series_name': first(n, 'series', 'movement_name', 'show'),
        'series_position': first(n, 'part', 'series_part', 'movement', 'episode_id'),
        'authors': split_people(first(n, 'artist', 'album_artist', 'author')),
        # By audiobook convention (and in audioborker's tagger) the narrator is
        # also written to composer, which is where ffprobe exposes it.
        'narrators': split_people(first(n, 'narrator', 'composer')),
        'genres': [],
        'summary': longest(n, 'synopsis', 'long_description', 'description', 'comment'),
        'release_date': '',
        'year': '',
    }
    g = first(n, 'genre')
    if g != '':
        b['genres'] = [g]
    date = first(n, 'date', 'releasetime', 'originaldate', 'recording_date', 'year')
    if date != '':
        raw = date.encode('utf-8')
        if len(raw) >= 10 and raw[4:5] == b'-' and raw[7:8] == b'-':
            b['release_date'] = raw[:10].decode('utf-8', 'replace')
        b['year'] = leading_year(date)
    return b


def asin(tags):
    """The Audible ASIN a file claims, or ''. Spellings seen in the wild:
    AUDIBLE_ASIN (audioborker/tone), a bare ASIN (m4b-tool and friends), and
    namespaced freeform variants. Values are validated so a stray free-text
    atom can never send a lookup off after garbage."""
    n = normalize(tags)
    for k in ('audible_asin', 'asin'):
        if valid_asin(n.get(k, '')):
            return n[k].upper()
    # A tagger not seen before: any key ending in "asin" with an ASIN-shaped
    # value. Sorted so the same file always resolves to the same ASIN.
    for k in sorted(n.keys()):
        if k.endswith('asin') and valid_asin(n[k]):
            return n[k].upper()
    return ''


def normalize(tags):
    """Folds key spellings together: lowercase, drop any freeform namespace
    prefix (everything up to the last ':'), and treat '-' and '_' alike.
    Blank values are dropped."""
    out = {}
    for k, v in (tags or {}).items():
        k = text(k).strip().lower()
        i = k.rfind(':')
        if i >= 0:
            k = k[i + 1:]
        k = k.replace('-', '_')
        v = text(v).strip()
        if v != '' and out.get(k, '') == '':
            out[k] = v
    return out


def first(n, *keys):
    for k in keys:
        v = n.get(k, '')
        if v != '':
            return v
    return ''


def longest(n, *keys):
    best = ''
    for k in keys:
        v = n.get(k, '')
        if utf8_len(v) > utf8_len(best):
            best = v
    return best


def split_people(s):
    """Splits a contributor list. "; " and " & " are unambiguous; ", " is
    accepted too even though it mis-splits "Doe, John" — these values are
    only search hints and display, so over-splitting costs nothing while
    under-splitting would lose the second author of most books."""
    s = text(s)
    if s == '':
        return []
    for sep in (';', ' & ', ','):
        if sep in s:
            return [p.strip() for p in s.split(sep) if p.strip() != '']
    return [s.strip()]


def leading_year(s):
    raw = text(s).encode('utf-8')
    if len(raw) < 4 or not raw[:4].isdigit():
        return ''
    return raw[:4].decode('ascii')


def path_tokens(path):
    """Returns (asin, region) from square-bracket tokens in a path, nearest
    component first: "Title [B004K50434]" and "Title [uk]". An ASIN token must
    contain a digit, which keeps tags like "[Unabridged]" — ten letters, and
    so a syntactically valid ASIN — from being mistaken for one."""
    found_asin, found_region = '', ''
    parts = [p for p in re.split(r'[\\/]', text(path)) if p]
    for part in reversed(parts):
        for token in BRACKET_TOKEN_RE.findall(part):
            token = token.strip()
            if not found_asin and is_asin_token(token):
                found_asin = token
            elif not found_region and token.lower() in REGIONS:
                found_region = token.lower()
    return found_asin, found_region


def is_asin_token(token):
    return valid_asin(token) and token == token.upper() and any_digit(token)


def any_digit(s):
    for c in s:
        if '0' <= c <= '9':
            return True
    return False
