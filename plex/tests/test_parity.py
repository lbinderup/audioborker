# -*- coding: utf-8 -*-
"""The Python port must reproduce audioborker's Go behaviour case for case.
The expected values come from internal/parity/testdata/golden.json, which the
Go test both generates (-update) and checks."""
from __future__ import absolute_import, unicode_literals

import unittest

from helpers import api_fixture, golden

from audioborker_plex import aggregate, catalog, embedded, genres, htmltext, match

LIST_FIELDS = ('authors', 'narrators', 'genres', 'sub_genres', 'genre_paths')


def book_json(b):
    """Go marshals nil slices as null and omits empty Sources; compare on the
    same footing."""
    if b is None:
        return None
    out = dict(b)
    for k in LIST_FIELDS:
        out[k] = list(out.get(k) or [])
    if not out.get('sources'):
        out.pop('sources', None)
    out['literature_type'] = out.get('literature_type') or ''  # omitempty in Go
    return out


def results_json(rs):
    out = []
    for r in rs or []:
        r = dict(r)
        for k in ('authors', 'narrators'):
            r[k] = list(r.get(k) or [])
        out.append(r)
    return out


class ParityTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.g = golden()

    def test_normalize(self):
        for c in self.g['normalize']:
            self.assertEqual(match.normalize(c['in']), c['out'], c['in'])

    def test_split_author_title(self):
        for c in self.g['split_author_title']:
            self.assertEqual(match.split_author_title(c['in']), (c['author'], c['title']), c['in'])

    def test_language_for_region(self):
        for c in self.g['language_for_region']:
            self.assertEqual(match.language_for_region(c['region']), c['language'], c['region'])

    def test_score_and_auto_select(self):
        for c in self.g['score']:
            s = c['signals']
            sig = match.Signals(s['title'], s['author'], s['runtime_min'], s['language'])
            results = [dict(r) for r in c['results']]
            ranked = match.score(results, sig)
            got = [{'asin': r['asin'], 'score': r['score'],
                    'runtime_match': r.get('runtime_match', ''),
                    'runtime_delta_min': r.get('runtime_delta_min', 0)} for r in ranked]
            self.assertEqual(got, c['ranked'] or [], c['name'])
            self.assertEqual(match.auto_select(ranked, sig), c['auto_select'], c['name'])

    def test_embedded_book(self):
        for c in self.g['embedded']:
            self.assertEqual(book_json(embedded.book(c['tags'])), book_json(c['book']), c['name'])

    def test_html_to_text(self):
        for c in self.g['html_to_text']:
            self.assertEqual(htmltext.html_to_text(c['in']), c['out'], repr(c['in']))

    def test_merge(self):
        for c in self.g['merge']:
            books = dict((k, book_json(v)) for k, v in (c['books'] or {}).items())
            got = aggregate.merge(books, c.get('overrides') or {})
            self.assertEqual(book_json(got), book_json(c['out']), c['name'])

    def test_clean_genres(self):
        for c in self.g['clean_genres']:
            book = book_json(c['book'])
            self.assertEqual(genres.literature_kind(book), c['kind'], c['name'])
            got_genres, got_subs = genres.clean_genres(book)
            self.assertEqual((got_genres, got_subs), (c['genres'] or [], c['sub_genres'] or []), c['name'])

    def test_catalog_parsing(self):
        for c in self.g['catalog']:
            body = api_fixture(c['fixture'])
            if c['kind'] == 'audible_search':
                got = catalog.parse_audible_search(body, c['region'])
                self.assertEqual(results_json(got), results_json(c.get('results')), c['fixture'])
                continue
            parse = {'audnexus_book': catalog.parse_audnexus_book,
                     'audible_product': catalog.parse_audible_product}[c['kind']]
            if c.get('not_found'):
                self.assertRaises(catalog.NotFound, parse, body, c['region'])
            else:
                self.assertEqual(book_json(parse(body, c['region'])), book_json(c['book']), c['fixture'])


if __name__ == '__main__':
    unittest.main()
