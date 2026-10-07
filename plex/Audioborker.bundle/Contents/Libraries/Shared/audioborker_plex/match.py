# -*- coding: utf-8 -*-
"""Port of audioborker's internal/match: query derivation and candidate
scoring. Kept line-for-line equivalent so Plex ranks candidates exactly as
audioborker's match screen does; internal/parity's fixtures pin that
equivalence (tests/test_parity.py). Change both sides together.

Candidates are dicts shaped like metadata.SearchResult, with snake_case keys:
asin, region, title, subtitle, authors, narrators, year, language,
runtime_min, cover_url, and — filled in here — score, runtime_delta_min,
runtime_match.
"""
from __future__ import absolute_import, unicode_literals

import re

from .compat import RE_ASCII, text, utf8_len

BRACKET_RE = re.compile(r'\[[^\]]*\]|\([^)]*\)|\{[^}]*\}', RE_ASCII)
LEAD_NUM_RE = re.compile(r'^\s*\d+[\s.\-_]*', RE_ASCII)
JUNK_WORDS = re.compile(
    r'\b(official|audiobook|unabridged|abridged|complete|retail|mp3|m4b|m4a|flac|64k|128k|320k|kbps|web|rip)\b',
    re.IGNORECASE | RE_ASCII)
NON_WORD_RE = re.compile(r'[_.]+', RE_ASCII)
SPACE_RE = re.compile(r'\s{2,}', RE_ASCII)

ASCII_FOLD = {
    'á': 'a', 'à': 'a', 'â': 'a', 'ä': 'a', 'ã': 'a', 'å': 'a',
    'é': 'e', 'è': 'e', 'ê': 'e', 'ë': 'e',
    'í': 'i', 'ì': 'i', 'î': 'i', 'ï': 'i',
    'ó': 'o', 'ò': 'o', 'ô': 'o', 'ö': 'o', 'õ': 'o', 'ø': 'o',
    'ú': 'u', 'ù': 'u', 'û': 'u', 'ü': 'u',
    'ç': 'c', 'ñ': 'n', 'æ': 'ae', 'œ': 'oe', 'ß': 'ss',
    'Á': 'A', 'À': 'A', 'Â': 'A', 'Ä': 'A', 'Ã': 'A', 'Å': 'A',
    'É': 'E', 'È': 'E', 'Ê': 'E', 'Ë': 'E',
    'Í': 'I', 'Ì': 'I', 'Î': 'I', 'Ï': 'I',
    'Ó': 'O', 'Ò': 'O', 'Ô': 'O', 'Ö': 'O', 'Õ': 'O', 'Ø': 'O',
    'Ú': 'U', 'Ù': 'U', 'Û': 'U', 'Ü': 'U',
    'Ç': 'C', 'Ñ': 'N', 'Æ': 'AE', 'Œ': 'OE',
}


def ascii_fold(s):
    return ''.join(ASCII_FOLD.get(c, c) for c in text(s))


def go_lower(s):
    """strings.ToLower maps each rune on its own; Python's lower() applies the
    context-sensitive final-sigma rule and expands U+0130 to two code points,
    either of which would shift Levenshtein distances."""
    return ''.join(c.lower()[:1] for c in text(s))


def go_trim_space(s):
    """strings.TrimSpace: Python's strip() also removes a few separators Go
    keeps (e.g. U+001C), so spell out Go's set."""
    return s.strip(' \t\n\v\f\r\x85\xa0      '
                   '         '
                   ' 　')


def path_base(name):
    """path.Base after normalizing backslashes, as Normalize does."""
    s = text(name).replace('\\', '/')
    if s == '':
        return '.'
    s = s.rstrip('/')
    if s == '':
        return '/'
    return s.rsplit('/', 1)[-1]


def path_ext(name):
    """path.Ext: the suffix from the final dot of the final element."""
    i = len(name) - 1
    while i >= 0 and name[i] != '/':
        if name[i] == '.':
            return name[i:]
        i -= 1
    return ''


def normalize(name):
    """Cleans a folder or file name into search keywords: strips the
    extension, bracketed blocks, leading track numbers, quality/format words,
    and separator noise."""
    s = path_base(name)
    ext = path_ext(s)
    # Go measures the extension in bytes.
    if 1 < utf8_len(ext) <= 6:
        s = s[:-len(ext)]
    s = BRACKET_RE.sub(' ', s)
    s = NON_WORD_RE.sub(' ', s)  # before junk words: "_64k" must become " 64k"
    s = LEAD_NUM_RE.sub('', s, count=1)
    s = JUNK_WORDS.sub(' ', s)
    s = ascii_fold(s)
    s = SPACE_RE.sub(' ', s)
    return go_trim_space(s)


def split_author_title(normalized):
    """Guesses (author, title) from "Author - Title" style names. Returns an
    empty author when there's no clear separator."""
    normalized = text(normalized)
    parts = normalized.split(' - ', 1)
    if len(parts) == 2 and len(parts[0].split()) <= 4:
        return go_trim_space(parts[0]), go_trim_space(parts[1])
    return '', normalized


class Signals(object):
    """What a candidate is judged against (match.Signals)."""

    def __init__(self, title='', author='', runtime_min=0, language=''):
        self.title = text(title)
        self.author = text(author)
        # Measured duration of the local audio; 0 when unknown.
        self.runtime_min = int(runtime_min or 0)
        # Expected edition language ("english"); empty disables the check.
        self.language = text(language)


def language_for_region(region):
    return {
        'us': 'english', 'uk': 'english', 'au': 'english', 'ca': 'english', 'in': 'english',
        'de': 'german',
        'fr': 'french',
        'es': 'spanish',
        'it': 'italian',
        'jp': 'japanese',
    }.get(text(region).lower(), '')


def score(results, sig):
    """Ranks results against the signals (title distance x2, author distance
    x10, catalog relevance as index penalty), adjusts for runtime agreement,
    penalizes the wrong language, and returns them sorted best-first.

    Runtime separates abridged from unabridged editions but is deliberately
    weighted below the author match, so a coincidental runtime never outranks
    the right author."""
    title_hint = go_lower(ascii_fold(sig.title))
    author_hint = go_lower(ascii_fold(sig.author))
    for i, r in enumerate(results):
        s = 100
        if title_hint != '':
            s -= 2 * fuzzy_distance(title_hint, go_lower(ascii_fold(r.get('title'))))
        authors = r.get('authors') or []
        if author_hint != '' and len(authors) > 0:
            best = 1 << 30
            for a in authors:
                d = levenshtein(author_hint, go_lower(ascii_fold(a)))
                if d < best:
                    best = d
            s -= 10 * best
        s -= i  # preserve some of the catalog's relevance ordering
        s += classify_runtime(r, sig.runtime_min)
        if not language_matches(sig.language, r.get('language')):
            s -= 25
        r['score'] = s
    # sorted() is stable, like sort.SliceStable.
    return sorted(results, key=lambda r: -r['score'])


def runtime_badge(official_min, local_min):
    r = {'runtime_min': official_min}
    classify_runtime(r, local_min)
    return r['runtime_delta_min'], r['runtime_match']


def language_matches(want, got):
    """Permissive: unknown on either side is not a mismatch."""
    want, got = text(want), text(got)
    if want == '' or got == '':
        return True
    return want.lower() == got.lower()


def classify_runtime(r, local_min):
    official = int(r.get('runtime_min') or 0)
    if local_min <= 0 or official <= 0:
        r['runtime_match'], r['runtime_delta_min'] = '', 0
        return 0
    delta = official - local_min
    r['runtime_delta_min'] = delta
    if delta < 0:
        delta = -delta
    pct = float(delta) / float(local_min)

    if delta <= 1 or pct <= 0.005:
        r['runtime_match'] = 'exact'
        return 25
    if pct <= 0.02:
        r['runtime_match'] = 'close'
        return 12
    if pct <= 0.05:
        r['runtime_match'] = 'near'
        return 3
    # Beyond 5%: a different edition, an abridgement, or the wrong book.
    r['runtime_match'] = 'off'
    if pct <= 0.15:
        return -12
    return -30


def auto_select(results, sig):
    """Whether the top result is safe to pick without asking. Intentionally
    strict: runtime must match almost exactly, the title must be a
    near-perfect hit, and the runner-up must be clearly worse — a silently
    wrong pick costs far more than one manual fix."""
    if len(results) == 0 or sig.title == '':
        return False
    best = results[0]
    if best.get('runtime_match') != 'exact':
        return False
    if sig.language != '' and sig.language.lower() != text(best.get('language')).lower():
        return False
    hint = go_lower(ascii_fold(sig.title))
    if fuzzy_distance(hint, go_lower(ascii_fold(best.get('title')))) > 2:
        return False
    if len(results) > 1:
        if best['score'] - results[1]['score'] < 15 or results[1].get('runtime_match') == 'exact':
            return False
    return True


def fuzzy_distance(hint, candidate):
    """Levenshtein softened for subtitles: a candidate containing the whole
    hint is nearly exact. Lengths are UTF-8 byte counts, as in Go."""
    d = levenshtein(hint, candidate)
    if utf8_len(hint) >= 4 and hint in candidate:
        extra = (utf8_len(candidate) - utf8_len(hint)) // 8
        if extra + 1 < d:
            return extra + 1
    return d


def levenshtein(a, b):
    """Classic two-row edit distance over code points (Go: runes)."""
    a, b = text(a), text(b)
    if len(a) == 0:
        return len(b)
    if len(b) == 0:
        return len(a)
    prev = list(range(len(b) + 1))
    cur = [0] * (len(b) + 1)
    for i in range(1, len(a) + 1):
        cur[0] = i
        for j in range(1, len(b) + 1):
            cost = 0 if a[i - 1] == b[j - 1] else 1
            cur[j] = min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + cost)
        prev, cur = cur, prev
    return prev[len(b)]
