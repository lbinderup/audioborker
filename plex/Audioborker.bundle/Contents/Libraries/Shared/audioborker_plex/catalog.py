# -*- coding: utf-8 -*-
"""Audible catalog and Audnexus clients, ported from audioborker's
internal/metadata/audible and internal/metadata/audnexus.

HTTP itself is injected: the Plex glue passes a fetch(url) callable built on
the framework's HTTP.Request (which caches raw responses — the same "only raw
per-source records are cached" rule audioborker keeps), and tests pass a
dict-backed fake. fetch returns decoded JSON, raises NotFound for a 404, and
raises SourceError for anything else that went wrong.

Books and search results are plain dicts using the JSON field names of
audioborker's metadata.Book / SearchResult, so parity fixtures compare
directly.
"""
from __future__ import absolute_import, unicode_literals

from .compat import text
from .embedded import valid_asin
from .htmltext import html_to_text

try:
    from urllib import quote as url_quote  # Python 2
except ImportError:
    from urllib.parse import quote as url_quote  # novermin

REGION_TLD = {
    'us': '.com', 'ca': '.ca', 'uk': '.co.uk', 'au': '.com.au', 'fr': '.fr',
    'de': '.de', 'jp': '.co.jp', 'it': '.it', 'in': '.in', 'es': '.es',
}

AUDNEXUS_URL = 'https://api.audnex.us'


class NotFound(Exception):
    """The catalog does not know this ASIN (in this region)."""


class SourceError(Exception):
    """A transient or unexpected failure talking to a catalog."""


def query_escape(s):
    """url.QueryEscape: spaces become '+', everything else percent-encoded."""
    return url_quote(text(s).encode('utf-8'), safe=b'').replace('%20', '+')


def encode_params(params):
    """url.Values.Encode: keys sorted, values query-escaped. A stable URL is
    also a stable cache key for Plex's HTTP cache."""
    return '&'.join(
        query_escape(k) + '=' + query_escape(params[k]) for k in sorted(params))


def tld(region):
    if region not in REGION_TLD:
        raise SourceError('unknown region %r' % region)
    return REGION_TLD[region]


def empty_book():
    return {
        'asin': '', 'region': '', 'title': '', 'subtitle': '',
        'authors': [], 'narrators': [],
        'series_name': '', 'series_position': '',
        'year': '', 'release_date': '',
        'publisher': '', 'language': '', 'genres': [],
        'sub_genres': [], 'genre_paths': [], 'literature_type': '',
        'description': '', 'summary': '',
        'runtime_min': 0, 'cover_url': '',
    }


def names(items):
    return [text(i.get('name')) for i in (items or []) if isinstance(i, dict)]


def year_of(date):
    date = text(date)
    if len(date.encode('utf-8')) >= 4:
        return date.split('-', 1)[0]
    return ''


def int_or_zero(v):
    try:
        return int(v or 0)
    except (TypeError, ValueError):
        return 0


# --- Audible catalog -------------------------------------------------------

def audible_search_url(region, keywords='', title='', author=''):
    params = {
        'num_results': '25',
        'products_sort_by': 'Relevance',
        'response_groups': 'contributors,product_desc,media,product_attrs',
        'image_sizes': '500',
    }
    if keywords:
        params['keywords'] = keywords
    if title:
        params['title'] = title
    if author:
        params['author'] = author
    return 'https://api.audible%s/1.0/catalog/products?%s' % (tld(region), encode_params(params))


def audible_search(fetch, region, keywords='', title='', author=''):
    return parse_audible_search(fetch(audible_search_url(region, keywords, title, author)), region)


def is_podcast(p):
    """The search mixes podcast episodes in with books (a fan podcast titled
    "The Last Colony - John Scalzi" outranked the book itself), and Audnexus
    refuses them, so picking one could only end in a failed lookup."""
    return (text(p.get('content_type')).lower() == 'podcast'
            or text(p.get('content_delivery_type')).lower().startswith('podcast'))


def parse_audible_search(body, region):
    out = []
    for p in (body or {}).get('products') or []:
        asin, title = text(p.get('asin')), text(p.get('title'))
        if asin == '' or title == '' or is_podcast(p):
            continue
        out.append({
            'asin': asin,
            'region': region,
            'title': title,
            'subtitle': text(p.get('subtitle')),
            'authors': names(p.get('authors')),
            'narrators': names(p.get('narrators')),
            'year': year_of(p.get('release_date')),
            'language': text(p.get('language')),
            'runtime_min': int_or_zero(p.get('runtime_length_min')),
            'cover_url': text((p.get('product_images') or {}).get('500')),
        })
    return out


def audible_product_url(asin, region):
    params = {
        'response_groups': 'contributors,product_desc,media,product_attrs,product_extended_attrs,series,category_ladders',
        'image_sizes': '500,1000',
    }
    return 'https://api.audible%s/1.0/catalog/products/%s?%s' % (
        tld(region), url_quote(text(asin).encode('utf-8'), safe=b''), encode_params(params))


def audible_book(fetch, asin, region):
    return parse_audible_product(fetch(audible_product_url(asin, region)), region)


def parse_audible_product(body, region):
    """audible.GetBook. A product without a title is what non-audiobook or
    region-restricted ASINs return, so it counts as not found."""
    p = (body or {}).get('product') or {}
    if text(p.get('asin')) == '' or text(p.get('title')) == '':
        raise NotFound('Audible has no audiobook data for this ASIN in region ' + region)
    images = p.get('product_images') or {}
    release = text(p.get('release_date'))
    b = empty_book()
    b.update({
        'asin': text(p.get('asin')),
        'region': region,
        'title': text(p.get('title')),
        'subtitle': text(p.get('subtitle')),
        'publisher': text(p.get('publisher_name')),
        'language': text(p.get('language')),
        'description': html_to_text(p.get('merchandising_summary')),
        'summary': html_to_text(p.get('publisher_summary')),
        'runtime_min': int_or_zero(p.get('runtime_length_min')),
        'year': year_of(release),
        'authors': names(p.get('authors')),
        'narrators': names(p.get('narrators')),
        'cover_url': text(images.get('1000')) or text(images.get('500')),
    })
    if len(release.encode('utf-8')) >= 10:
        b['release_date'] = release[:10]
    # The series list can carry umbrella collections with no sequence (e.g.
    # "The Cosmere" alongside "The Stormlight Archive") — prefer the first
    # numbered entry, which matches what Audnexus calls the primary series.
    series = [s for s in (p.get('series') or []) if isinstance(s, dict)]
    for s in series:
        if text(s.get('sequence')) != '':
            b['series_name'], b['series_position'] = text(s.get('title')), text(s.get('sequence'))
            break
    if b['series_name'] == '' and series:
        b['series_name'] = text(series[0].get('title'))
    # Each category ladder is a path like Science Fiction & Fantasy -> Fantasy
    # -> Epic: the first rung is the genre, the rest are sub-genres. Ladders
    # repeat rungs, so dedupe preserving order. The paths are kept raw for
    # genres.clean_genres, which drops a miscategorized genre with its
    # sub-genres.
    seen_genre, seen_sub = set(), set()
    for cl in p.get('category_ladders') or []:
        ladder = cl.get('ladder') or []
        if not ladder:
            continue
        path = [text(rung.get('name')) for rung in ladder]
        b['genre_paths'].append(path)
        if path[0] not in seen_genre:
            seen_genre.add(path[0])
            b['genres'].append(path[0])
        for s in path[1:]:
            if s not in seen_sub:
                seen_sub.add(s)
                b['sub_genres'].append(s)
    return b


# --- Audnexus --------------------------------------------------------------

def audnexus_book_url(asin, region, base=AUDNEXUS_URL):
    return '%s/books/%s?region=%s' % (base.rstrip('/'), asin, region)


def audnexus_book(fetch, asin, region, base=AUDNEXUS_URL):
    if not valid_asin(asin):
        raise SourceError('%r is not a valid ASIN (must be 10 letters/digits)' % asin)
    return parse_audnexus_book(fetch(audnexus_book_url(asin, region, base)), region)


def parse_audnexus_book(ab, region):
    ab = ab or {}
    release = text(ab.get('releaseDate'))
    b = empty_book()
    b.update({
        'asin': text(ab.get('asin')),
        'region': region,
        'title': text(ab.get('title')),
        'subtitle': text(ab.get('subtitle')),
        'publisher': text(ab.get('publisherName')),
        'language': text(ab.get('language')),
        'description': text(ab.get('description')),
        # `summary` is the full blurb but arrives as HTML; `description` is a
        # ~250-char teaser ending in "...". Keep both, flattened to text.
        'summary': html_to_text(ab.get('summary')),
        'runtime_min': int_or_zero(ab.get('runtimeLengthMin')),
        'cover_url': text(ab.get('image')),
        'authors': names(ab.get('authors')),
        'narrators': names(ab.get('narrators')),
    })
    raw = release.encode('utf-8')
    if len(raw) >= 4:
        b['year'] = raw[:4].decode('utf-8', 'replace')
    if len(raw) >= 10:
        b['release_date'] = raw[:10].decode('utf-8', 'replace')
    series = ab.get('seriesPrimary')
    if isinstance(series, dict):
        b['series_name'] = text(series.get('name'))
        b['series_position'] = text(series.get('position'))
    # Audnexus flattens Audible's ladders into top-level "genre" entries and
    # "tag" entries for every rung below, without saying which tag belongs to
    # which genre.
    for g in ab.get('genres') or []:
        kind = text(g.get('type'))
        if kind == 'genre':
            b['genres'].append(text(g.get('name')))
        elif kind == 'tag':
            b['sub_genres'].append(text(g.get('name')))
    b['literature_type'] = text(ab.get('literatureType'))
    return b


def audnexus_rating(ab):
    """Audnexus' Audible star rating (0-5), which audioborker has no use for
    but Plex displays. 0.0 when absent."""
    try:
        return float((ab or {}).get('rating') or 0)
    except (TypeError, ValueError):
        return 0.0


# Authors are a Plex-only concern (audioborker never looks them up), so this
# follows the Audnexus agent's approach: name search, then the author record.

def audnexus_author_search(fetch, name, region, base=AUDNEXUS_URL):
    url = '%s/authors?region=%s&name=%s' % (base.rstrip('/'), region, query_escape(name))
    body = fetch(url)
    if isinstance(body, dict):
        body = [body]
    return [{'asin': text(a.get('asin')), 'name': text(a.get('name'))}
            for a in (body or []) if isinstance(a, dict) and a.get('asin') and a.get('name')]


def audnexus_author(fetch, asin, region, base=AUDNEXUS_URL):
    body = fetch('%s/authors/%s?region=%s' % (base.rstrip('/'), asin, region)) or {}
    return {
        'asin': text(body.get('asin')),
        'name': text(body.get('name')),
        'description': html_to_text(body.get('description')),
        'image': text(body.get('image')),
        'genres': [text(g.get('name')) for g in (body.get('genres') or [])
                   if isinstance(g, dict) and text(g.get('type')) == 'genre'],
    }
