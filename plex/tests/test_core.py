# -*- coding: utf-8 -*-
from __future__ import absolute_import, unicode_literals

import datetime
import os
import unittest

from helpers import FIXTURES, FakeFetch, api_fixture

from audioborker_plex import catalog, core
from audioborker_plex.fetch import TransientError, make_fetcher

TAGGED = os.path.join(FIXTURES, 'tagged.m4b')
LOCKE_MS = 1319 * 60000


def locke_routes(region='us'):
    """The captured Locke Lamora records, served as if they lived in `region`
    only — every other storefront answers not found."""
    host = 'api.audible' + catalog.REGION_TLD[region] + '/1.0/catalog/products'
    return [
        ('audnex.us/books/B004K50434?region=' + region, api_fixture('audnexus_book_B004K50434_us.json')),
        (host + '/B004K50434', api_fixture('audible_product_B004K50434_us.json')),
        (host + '?', api_fixture('audible_search_locke_us.json')),
    ]


class Node(object):
    """Stands in for the framework's MediaTree / MediaItem / MediaPart."""

    def __init__(self, **kw):
        self.__dict__.update(kw)


def album_tree(paths_and_ms):
    tracks = []
    for i, (path, ms) in enumerate(paths_and_ms):
        part = Node(file=path, duration=str(ms) if ms else None)
        tracks.append(Node(index=str(len(paths_and_ms) - i), items=[Node(parts=[part])], children=[]))
    return Node(items=[], children=tracks)


def mp3_book(minutes=0, n=3):
    per = minutes * 60000 // n if minutes else 0
    return core.gather([('/books/Scott Lynch/The Lies of Locke Lamora/%02d.mp3' % (i + 1), per or None)
                        for i in range(n)], 'The Lies of Locke Lamora', 'Scott Lynch')


class GatherTest(unittest.TestCase):
    def test_tagged_file(self):
        local = core.gather([(TAGGED, LOCKE_MS)], 'Plex Album Hint', 'Plex Artist')
        self.assertEqual(local.file_book['asin'], 'B004K50434')
        self.assertEqual(local.claimed_asins(), ['B004K50434'])
        self.assertEqual(local.runtime_min, 1319)
        self.assertEqual(local.sort_title(), 'Gentleman Bastard Sequence 1 - The Lies of Locke Lamora')

    def test_runtime_from_mp4_headers_when_plex_has_none(self):
        local = core.gather([(TAGGED, None)])
        self.assertEqual(local.runtime_min, 0)  # 3 s rounds to zero minutes
        calls = []

        def reader(path):
            calls.append(path)
            return core.mp4.Mp4Info({}, 30 * 60000)
        local = core.gather([('/b/1.m4a', None), ('/b/2.m4a', None)], reader=reader)
        self.assertEqual(local.runtime_min, 60)
        self.assertEqual(calls, ['/b/1.m4a', '/b/2.m4a'])

    def test_measure_false_reads_only_the_first_file(self):
        calls = []

        def reader(path):
            calls.append(path)
            return core.mp4.Mp4Info({'AUDIBLE_ASIN': 'B004K50434'}, 60000)
        local = core.gather([('/b/1.m4b', None), ('/b/2.m4b', None)], reader=reader, measure=False)
        self.assertEqual(calls, ['/b/1.m4b'])
        self.assertEqual(local.claimed_asins(), ['B004K50434'])
        self.assertEqual(local.runtime_min, 0)

    def test_a_partial_total_is_unknown(self):
        local = core.gather([('/b/1.mp3', 1000000), ('/b/2.mp3', None)])
        self.assertEqual(local.runtime_min, 0)

    def test_path_tokens_and_import_name(self):
        local = core.gather([('/lib/Author/Series/Title [B004K50434] [uk]/01.mp3', 1), ('/lib/x/02.mp3', 1)])
        self.assertEqual((local.path_asin, local.path_region), ('B004K50434', 'uk'))
        self.assertEqual(local.name_for_import, 'Title [B004K50434] [uk]')
        single = core.gather([('/lib/Author/Book Name.m4b', 1)], reader=lambda p: None)
        self.assertEqual(single.name_for_import, 'Book Name.m4b')

    def test_unreadable_file_is_no_evidence(self):
        def boom(path):
            raise IOError('denied')
        local = core.gather([('/x/a.m4b', 5 * 60000)], 'Hint', 'Author', reader=boom)
        self.assertIsNone(local.file_book)
        self.assertEqual(local.runtime_min, 5)

    def test_bad_artist_hints_are_dropped(self):
        self.assertEqual(core.gather([], 'T', '[Unknown Artist]').hint_author, '')

    def test_tree_entries_in_track_order(self):
        tree = album_tree([('/b/c.mp3', 3000), ('/b/b.mp3', 2000), ('/b/a.mp3', 1000)])
        self.assertEqual(core.tree_entries(tree), [('/b/a.mp3', 1000), ('/b/b.mp3', 2000), ('/b/c.mp3', 3000)])

    def test_filename_hint_is_unquoted(self):
        self.assertEqual(core.filename_entries('/b/The%20Book%20%5BB004K50434%5D.m4b'),
                         [('/b/The Book [B004K50434].m4b', None)])


class SearchTest(unittest.TestCase):
    def test_file_asin_matches_directly(self):
        fetch = FakeFetch(locke_routes())
        local = core.gather([(TAGGED, LOCKE_MS)])
        rows = core.search_album(fetch, local, core.Settings())
        self.assertEqual([(r['id'], r['score'], r['year']) for r in rows], [('B004K50434_us', 100, 2011)])
        self.assertTrue(rows[0]['name'].startswith('The Lies of Locke Lamora · S.Lynch · M.Page · 21h59m'))
        self.assertFalse([u for u in fetch.calls if 'catalog/products?' in u], 'no search needed')

    def test_file_asin_found_in_another_region(self):
        fetch = FakeFetch([
            ('audnex.us/books/B09LYZPFLZ?region=us', api_fixture('audnexus_book_B09LYZPFLZ_us.json')),
            ('api.audible.com/1.0/catalog/products/B09LYZPFLZ', api_fixture('audible_product_B09LYZPFLZ_us.json')),
            ('api.audible.co.uk/1.0/catalog/products/B09LYZPFLZ', api_fixture('audible_product_B09LYZPFLZ_uk.json')),
        ])
        local = core.gather([('/lib/The Colour of Magic [B09LYZPFLZ].m4b', None)], reader=lambda p: None)
        rows = core.search_album(fetch, local, core.Settings(region='uk'))
        self.assertEqual([r['id'] for r in rows], ['B09LYZPFLZ_us'])

    def test_catalog_outage_trusts_the_file(self):
        fetch = FakeFetch([('audnex.us', TransientError('HTTP 503'))])
        rows = core.search_album(fetch, core.gather([(TAGGED, None)]), core.Settings())
        self.assertEqual([(r['id'], r['score']) for r in rows], [('B004K50434_us', 100)])
        self.assertIn('The Lies of Locke Lamora', rows[0]['name'])

    def test_search_auto_selects_like_audioborker(self):
        fetch = FakeFetch(locke_routes())
        rows = core.search_album(fetch, mp3_book(1319), core.Settings())
        self.assertEqual([(r['id'], r['score']) for r in rows], [('B004K50434_us', 100)])
        url = [u for u in fetch.calls if 'catalog/products?' in u][0]
        for part in ('keywords=Scott+Lynch+The+Lies+of+Locke+Lamora', 'title=The+Lies+of+Locke+Lamora',
                     'author=Scott+Lynch', 'num_results=25', 'products_sort_by=Relevance'):
            self.assertIn(part, url)

    def test_other_edition_runtime_picks_other_edition(self):
        rows = core.search_album(FakeFetch(locke_routes()), mp3_book(1356), core.Settings())
        self.assertEqual([(r['id'], r['score']) for r in rows], [('0593163389_us', 100)])

    def test_unknown_runtime_leaves_it_for_fix_match(self):
        rows = core.search_album(FakeFetch(locke_routes()), mp3_book(0), core.Settings())
        self.assertEqual(len(rows), 2)
        self.assertTrue(all(r['score'] <= core.UNSURE_CAP for r in rows), rows)

    def test_lenient_still_refuses_two_equal_editions(self):
        rows = core.search_album(FakeFetch(locke_routes()), mp3_book(0),
                                 core.Settings(policy=core.POLICY_LENIENT))
        self.assertTrue(all(r['score'] <= core.UNSURE_CAP for r in rows), rows)

    def test_lenient_accepts_a_clear_title_and_author_match(self):
        search = {'products': [
            {'asin': 'B000000001', 'title': 'Unique Book', 'authors': [{'name': 'Some Author'}], 'language': 'english'},
            {'asin': 'B000000002', 'title': 'Other Thing', 'authors': [{'name': 'Else'}], 'language': 'english'},
        ]}
        local = core.gather([('/b/01.mp3', None)], 'Unique Book', 'Some Author')
        strict = core.search_album(FakeFetch([('products?', search)]), local, core.Settings())
        lenient = core.search_album(FakeFetch([('products?', search)]), local,
                                    core.Settings(policy=core.POLICY_LENIENT))
        self.assertLessEqual(strict[0]['score'], core.UNSURE_CAP)
        self.assertEqual([(r['id'], r['score']) for r in lenient], [('B000000001_us', 100)])

    def test_falls_back_to_the_folder_name(self):
        hits = {'products': [{'asin': 'B000000009', 'title': 'Found It', 'authors': [{'name': 'X'}]}]}
        fetch = FakeFetch([('title=Bad+Tag+Title', {'products': []}), ('keywords=Found+It', hits)])
        local = core.gather([('/lib/Found It/01.mp3', None), ('/lib/Found It/02.mp3', None)], 'Bad Tag Title', '')
        rows = core.search_album(fetch, local, core.Settings())
        self.assertEqual(rows[0]['id'], 'B000000009_us')
        self.assertEqual(len([u for u in fetch.calls if 'products?' in u]), 2)


class ManualSearchTest(unittest.TestCase):
    def test_typed_asin_and_region(self):
        fetch = FakeFetch(locke_routes('uk'))
        rows = core.search_album(fetch, mp3_book(), core.Settings(), manual=True, manual_text='[uk] b004k50434')
        self.assertEqual([r['id'] for r in rows], ['B004K50434_uk'])

    def test_file_claim_leads_then_ranked_search(self):
        fetch = FakeFetch(locke_routes())
        local = core.gather([(TAGGED, 1356 * 60000)])
        rows = core.search_album(fetch, local, core.Settings(), manual=True,
                                 manual_text='The Lies of Locke Lamora')
        self.assertEqual([r['id'] for r in rows], ['B004K50434_us', '0593163389_us'])
        scores = [r['score'] for r in rows]
        self.assertEqual(scores, sorted(scores, reverse=True))
        self.assertEqual(len(set(scores)), len(scores))
        url = [u for u in fetch.calls if 'products?' in u][0]
        self.assertIn('keywords=The+Lies+of+Locke+Lamora', url)
        self.assertNotIn('title=', url)

    def test_typed_asin_without_region_finds_its_storefront(self):
        fetch = FakeFetch(locke_routes('us'))
        rows = core.search_album(fetch, mp3_book(), core.Settings(region='de'), manual=True,
                                 manual_text='B004K50434')
        self.assertEqual([r['id'] for r in rows], ['B004K50434_us'])
        rows = core.search_album(FakeFetch(locke_routes('us')), mp3_book(), core.Settings(region='de'),
                                 manual=True, manual_text='B004K50434 [de]')
        self.assertEqual(rows, [], 'an explicit region is not second-guessed')

    def test_typed_tokens(self):
        self.assertEqual(core.typed_tokens('Dune [uk]'), ('', 'uk', 'Dune'))
        self.assertEqual(core.typed_tokens('Unabridged edition'), ('', '', 'Unabridged edition'))
        self.assertEqual(core.typed_tokens('0593163389'), ('0593163389', '', '0593163389'))


class AlbumMetadataTest(unittest.TestCase):
    def test_catalog_merge(self):
        md = core.album_metadata(FakeFetch(locke_routes()), 'B004K50434_us', None, core.Settings())
        self.assertEqual(md['title'], 'The Lies of Locke Lamora')
        self.assertEqual(md['title_sort'], 'Gentleman Bastard Sequence 1 - The Lies of Locke Lamora')
        self.assertEqual(md['studio'], 'Gollancz')
        self.assertEqual(md['originally_available_at'], datetime.date(2011, 1, 21))
        self.assertEqual(md['styles'], ['Michael Page'])
        self.assertEqual(md['moods'], ['Scott Lynch', 'Series: Gentleman Bastard Sequence'])
        self.assertAlmostEqual(md['rating'], 9.2)
        self.assertIn('Thorn of Camorr', md['summary'])
        self.assertNotIn('<b>', md['summary'])
        self.assertTrue(md['poster'].startswith('https://'))
        self.assertEqual(md['from_file'], [])

    def test_audioborker_file_wins(self):
        local = core.gather([(TAGGED, None)])
        local.file_book['genres'] = ['Comedy & Humor']  # as if picked on the match screen
        md = core.album_metadata(FakeFetch(locke_routes()), 'B004K50434_us', local, core.Settings())
        self.assertEqual(md['genres'][:2], ['Comedy & Humor', 'Science Fiction & Fantasy'])
        self.assertIn('genres', md['from_file'])
        self.assertTrue(md['summary'].startswith('They say the Thorn of Camorr'))

    def test_genres_are_cleaned_and_include_sub_genres(self):
        md = core.album_metadata(FakeFetch(locke_routes()), 'B004K50434_us', None, core.Settings())
        self.assertEqual(md['genres'], ['Science Fiction & Fantasy', 'Fantasy', 'Epic', 'Humorous',
                                        'Paranormal & Urban', 'Urban'])

    def test_a_files_miscategorized_genre_is_ignored(self):
        # What audioborker wrote into this book before genres were cleaned.
        local = core.gather([(TAGGED, None)])
        local.file_book['genres'] = ['Relationships, Parenting & Personal Development']
        notes = []
        md = core.album_metadata(FakeFetch(locke_routes()), 'B004K50434_us', local, core.Settings(),
                                 log=notes.append)
        self.assertEqual(md['genres'][0], 'Science Fiction & Fantasy')
        self.assertNotIn('Relationships, Parenting & Personal Development', md['genres'])
        self.assertNotIn('genres', md['from_file'])
        self.assertTrue([n for n in notes if 'Ignoring the file' in n])

    def test_series_collection(self):
        md = core.album_metadata(FakeFetch(locke_routes()), 'B004K50434_us', None, core.Settings())
        self.assertEqual(md['collection'], 'Gentleman Bastard Sequence')
        md = core.album_metadata(FakeFetch(locke_routes()), 'B004K50434_us', None,
                                 core.Settings(series_collections=False))
        self.assertEqual(md['collection'], '')

    def test_file_for_another_book_is_ignored(self):
        local = core.gather([(TAGGED, None)])
        local.file_book['asin'] = 'B000000000'
        md = core.album_metadata(FakeFetch(locke_routes()), 'B004K50434_us', local, core.Settings())
        self.assertEqual(md['from_file'], [])

    def test_audnexus_outage_raises_instead_of_degrading(self):
        fetch = FakeFetch([('audnex.us', TransientError('HTTP 503'))] + locke_routes())
        self.assertRaises(catalog.SourceError, core.album_metadata, fetch, 'B004K50434_us', None, core.Settings())

    def test_audible_outage_is_only_a_note(self):
        notes = []
        fetch = FakeFetch([('/catalog/products/', TransientError('HTTP 500'))] + locke_routes())
        md = core.album_metadata(fetch, 'B004K50434_us', None, core.Settings(), log=notes.append)
        self.assertEqual(md['title'], 'The Lies of Locke Lamora')
        self.assertTrue([n for n in notes if 'audible lookup failed' in n])

    def test_authors_as_moods_can_be_off(self):
        md = core.album_metadata(FakeFetch(locke_routes()), 'B004K50434', None,
                                 core.Settings(authors_as_moods=False))
        self.assertEqual(md['moods'], ['Series: Gentleman Bastard Sequence'])
        self.assertEqual(md['region'], 'us')


class ArtistTest(unittest.TestCase):
    def test_author_through_the_books_asin(self):
        fetch = FakeFetch(locke_routes())
        rows = core.search_artist(fetch, 'Scott Lynch', core.gather([(TAGGED, None)]), core.Settings())
        self.assertEqual([(r['id'], r['score']) for r in rows], [('B001DABSBQ_us', 100)])

    def test_name_search(self):
        found = [{'asin': 'B0001', 'name': 'Terry Pratchett'}, {'asin': 'B0002', 'name': 'Terry Practchett'}]
        rows = core.search_artist(FakeFetch([('/authors?', found)]), 'Terry Pratchett, Neil Gaiman', None, core.Settings())
        self.assertEqual([(r['id'], r['score']) for r in rows], [('B0001_us', 100)])

    def test_two_authors_with_one_name_are_not_guessed(self):
        found = [{'asin': 'B0001', 'name': 'John Smith'}, {'asin': 'B0002', 'name': 'John Smith'}]
        rows = core.search_artist(FakeFetch([('/authors?', found)]), 'John Smith', None, core.Settings())
        self.assertTrue(all(r['score'] <= core.UNSURE_CAP for r in rows), rows)

    def test_sort_name(self):
        self.assertEqual(core.sort_name('Terry Pratchett'), 'Pratchett, Terry')
        self.assertEqual(core.sort_name('Martin Luther King, Jr.'), 'King, Martin Luther, Jr.')
        self.assertEqual(core.sort_name('Homer'), 'Homer')

    def test_initials(self):
        self.assertEqual(core.initials('Arthur Conan Doyle'), 'A.C.Doyle')
        self.assertEqual(core.initials('Plato'), 'Plato')


class FetchPolicyTest(unittest.TestCase):
    def test_status_of(self):
        from audioborker_plex.fetch import status_of

        class HTTPError(Exception):
            code = 404
        self.assertEqual(status_of(HTTPError('x')), 404)
        self.assertEqual(status_of(Exception('HTTP Error 429: Too Many Requests')), 429)
        self.assertEqual(status_of(Exception('Wrapped: HTTP 503 from upstream')), 503)
        self.assertIsNone(status_of(IOError('timed out')))

    def test_classify(self):
        from audioborker_plex.fetch import classify
        self.assertIsInstance(classify(404, 'u'), catalog.NotFound)
        for code in (None, 408, 429, 500, 503):
            self.assertIsInstance(classify(code, 'u'), TransientError)
        self.assertNotIsInstance(classify(403, 'u'), TransientError)

    def test_retries_transient_then_succeeds(self):
        attempts = []

        def once(url):
            attempts.append(url)
            if len(attempts) < 3:
                raise TransientError('503')
            return {'ok': True}
        fetch = make_fetcher(once, sleep=lambda s: None, limiter=None)
        self.assertEqual(fetch('https://api.audnex.us/x'), {'ok': True})
        self.assertEqual(len(attempts), 3)

    def test_not_found_is_not_retried(self):
        attempts = []

        def once(url):
            attempts.append(url)
            raise catalog.NotFound('404')
        fetch = make_fetcher(once, sleep=lambda s: None, limiter=None)
        self.assertRaises(catalog.NotFound, fetch, 'u')
        self.assertEqual(len(attempts), 1)


if __name__ == '__main__':
    unittest.main()


MANUAL = os.path.join(FIXTURES, 'manual.m4b')
MANUAL_ID = 'local_6f0c1a52-1b8e-4f0e-9a51-3c6c1f7d2e10'


class ManualBookTest(unittest.TestCase):
    """Books audioborker tagged by hand (its last-resort flow) are their own
    metadata source: matched by the ID in the file, never searched."""

    def test_matches_without_touching_the_network(self):
        fetch = FakeFetch([])
        local = core.gather([(MANUAL, 701 * 60000)])
        self.assertEqual(local.manual_id(), MANUAL_ID)
        rows = core.search_album(fetch, local, core.Settings())
        self.assertEqual([(r['id'], r['score']) for r in rows], [(MANUAL_ID, 100)])
        self.assertIn('tagged by hand', rows[0]['name'])
        self.assertEqual(fetch.calls, [])

    def test_fix_match_lists_it_first(self):
        fetch = FakeFetch(locke_routes())
        rows = core.search_album(fetch, core.gather([(MANUAL, None)]), core.Settings(), manual=True,
                                 manual_text='The Lies of Locke Lamora')
        self.assertEqual(rows[0]['id'], MANUAL_ID)
        self.assertGreater(len(rows), 1)

    def test_metadata_comes_from_the_file(self):
        fetch = FakeFetch([])
        md = core.album_metadata(fetch, MANUAL_ID, core.gather([(MANUAL, None)], measure=False), core.Settings())
        self.assertEqual(fetch.calls, [])
        self.assertEqual(md['title'], 'Mockingjay')
        self.assertEqual(md['title_sort'], 'The Hunger Games 3 - Mockingjay')
        self.assertEqual(md['genres'], ['Teen & Young Adult', 'Science Fiction & Fantasy', 'Dystopian'])
        self.assertEqual(md['styles'], ['Carolyn McCormick'])
        self.assertEqual(md['moods'], ['Suzanne Collins', 'Series: The Hunger Games'])
        self.assertEqual(md['studio'], 'Scholastic Audio')
        self.assertEqual(md['originally_available_at'], datetime.date(2010, 8, 24))
        self.assertEqual(md['collection'], 'The Hunger Games')
        self.assertTrue(md['cover_bytes'].startswith(b'\xff\xd8'))
        self.assertTrue(md['cover_key'].startswith('embedded-'))
        self.assertIsNone(md['rating'])

    def test_unreadable_file_is_an_error_not_an_empty_book(self):
        local = core.gather([('/gone/book.m4b', None)], reader=lambda p: None)
        self.assertRaises(catalog.SourceError, core.album_metadata, FakeFetch([]), MANUAL_ID, local, core.Settings())

    def test_marker_without_an_id_is_ignored(self):
        def reader(path):
            return core.mp4.Mp4Info({'AUDIOBORKER_SOURCE': 'manual', 'title': 'X'}, 0)
        self.assertEqual(core.gather([('/b/x.m4b', None)], reader=reader).manual_id(), '')
