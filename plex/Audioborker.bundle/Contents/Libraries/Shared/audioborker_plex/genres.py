# -*- coding: utf-8 -*-
"""Port of audioborker's internal/metadata/genres.go: drops genres that
contradict a book's fiction/nonfiction kind (Audible files The Lies of Locke
Lamora under "Relationships, Parenting & Personal Development") and orders
the rest by how many category ladders support them."""
from __future__ import absolute_import, unicode_literals

from .compat import text

KIND_FICTION = 'fiction'
KIND_NONFICTION = 'nonfiction'

# Audible's top-level categories as the English-language stores name them.
# Categories holding both kinds, and other stores' names, are absent on
# purpose: an unclassified genre is never dropped.
GENRE_KINDS = {
    'literature & fiction': KIND_FICTION,
    'science fiction & fantasy': KIND_FICTION,
    'mystery, thriller & suspense': KIND_FICTION,
    'romance': KIND_FICTION,
    'erotica': KIND_FICTION,

    'arts & entertainment': KIND_NONFICTION,
    'biographies & memoirs': KIND_NONFICTION,
    'business & careers': KIND_NONFICTION,
    'computers & technology': KIND_NONFICTION,
    'education & learning': KIND_NONFICTION,
    'health & wellness': KIND_NONFICTION,
    'history': KIND_NONFICTION,
    'home & garden': KIND_NONFICTION,
    'money & finance': KIND_NONFICTION,
    'politics & social sciences': KIND_NONFICTION,
    'relationships, parenting & personal development': KIND_NONFICTION,
    'religion & spirituality': KIND_NONFICTION,
    'science & engineering': KIND_NONFICTION,
    'sports & outdoors': KIND_NONFICTION,
    'travel & tourism': KIND_NONFICTION,
}

# strings.TrimSpace's set, as in match.go_trim_space.
GO_SPACE = (' \t\n\v\f\r\x85\xa0       '
            '         　')


def go_lower(s):
    return ''.join(c.lower()[:1] for c in text(s))


def genre_kind(name):
    return GENRE_KINDS.get(go_lower(text(name).strip(GO_SPACE)), '')


def literature_kind(book):
    """Audnexus' label when present, else the majority of the category
    paths; '' when unknown or tied."""
    label = go_lower(text(book.get('literature_type')).strip(GO_SPACE))
    if label in (KIND_FICTION, KIND_NONFICTION):
        return label
    fiction = nonfiction = 0
    for path in book.get('genre_paths') or []:
        if not path:
            continue
        kind = genre_kind(path[0])
        if kind == KIND_FICTION:
            fiction += 1
        elif kind == KIND_NONFICTION:
            nonfiction += 1
    if fiction > nonfiction:
        return KIND_FICTION
    if nonfiction > fiction:
        return KIND_NONFICTION
    return ''


def clean_genres(book):
    """(genres, sub_genres) with contradicting categories removed, the best
    supported genre first. With category paths a sub-genre survives only
    under a surviving genre; a flat list loses its (unattributable)
    sub-genres whenever a genre is dropped. Never empties the genres."""
    kind = literature_kind(book)

    def contradicts(name):
        k = genre_kind(name)
        return kind != '' and k != '' and k != kind

    paths = book.get('genre_paths') or []
    flat_genres = list(book.get('genres') or [])
    flat_subs = list(book.get('sub_genres') or [])
    if not paths:
        kept = [g for g in flat_genres if not contradicts(g)]
        if not kept:
            return flat_genres, flat_subs
        if len(kept) < len(flat_genres):
            return kept, []
        return kept, flat_subs

    order, support = [], {}
    for path in paths:
        if not path or not path[0]:
            continue
        if path[0] not in support:
            support[path[0]] = 0
            order.append(path[0])
        support[path[0]] += 1
    keep = [g for g in order if not contradicts(g)] or order
    # sorted() is stable, like sort.SliceStable.
    keep = sorted(keep, key=lambda g: -support[g])

    seen = set(keep)
    subs = []
    for g in keep:
        for path in paths:
            if not path or path[0] != g:
                continue
            for s in path[1:]:
                if s and s not in seen:
                    seen.add(s)
                    subs.append(s)
    return keep, subs
