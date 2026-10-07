# -*- coding: utf-8 -*-
"""Executes Contents/Code/__init__.py against stand-ins for the Plex plug-in
framework's globals, so the glue — the one file Plex runs directly — is
exercised end to end: agent registration, search, update, prefs, HTTP error
mapping. The stand-ins mirror the framework's shapes (agentkit.py)."""
from __future__ import absolute_import, unicode_literals

import io
import json
import os
import sys
import unittest

from helpers import FIXTURES, REPO, api_fixture

BUNDLE = os.path.join(REPO, 'plex', 'Audioborker.bundle', 'Contents')
TAGGED = os.path.join(FIXTURES, 'tagged.m4b')


class NS(object):
    def __init__(self, **kw):
        self.__dict__.update(kw)


class HTTPError(Exception):
    """Shaped like urllib2.HTTPError, which the framework lets through."""

    def __init__(self, code):
        Exception.__init__(self, 'HTTP Error %d' % code)
        self.code = code


class FakeHTTP(object):
    def __init__(self, routes):
        self.routes = routes
        self.Headers = {}
        self.CacheTime = 0
        self.calls = []
        self.methods = []

    def Request(self, url, **kw):
        self.calls.append(url)
        if kw.get('method'):
            self.methods.append((kw['method'], url))
        for needle, body in self.routes:
            if needle in url:
                if isinstance(body, Exception):
                    raise body
                return NS(content=body if isinstance(body, bytes) else json.dumps(body))
        raise HTTPError(404)


class Log(object):
    def __init__(self):
        self.lines = []

    def __call__(self, fmt, *args):
        self.lines.append(fmt % args if args else fmt)

    Info = Debug = Warn = Error = __call__

    def Exception(self, fmt, *args):
        # The framework's Log.Exception appends the active traceback.
        import traceback
        self.lines.append((fmt % args if args else fmt) + '\n' + traceback.format_exc())


class TagSet(set):
    """Album genres/styles/moods: the framework's Set has clear() and add()."""


class Posters(dict):
    def validate_keys(self, keys):
        for k in list(self):
            if k not in keys:
                del self[k]


def default_prefs():
    prefs = {}
    with io.open(os.path.join(BUNDLE, 'DefaultPrefs.json'), encoding='utf-8') as f:
        for p in json.load(f):
            v = p['default']
            prefs[p['id']] = (v == 'true') if p['type'] == 'bool' else v
    return prefs


def load_glue(routes, prefs=None, fail_import=False):
    registered = []

    class Base(object):
        def __init_subclass__(cls, **kw):  # stands in for AgentMetaclass
            if 'search' in cls.__dict__:  # agents, not the Artist/Album bases
                registered.append(cls)

    env = {
        '__name__': '__code__',
        'Agent': NS(Artist=type('Artist', (Base,), {}), Album=type('Album', (Base,), {})),
        'Locale': NS(Language=NS(English='en')),
        'Log': Log(),
        'HTTP': FakeHTTP(routes),
        'JSON': NS(ObjectFromString=json.loads),
        'Prefs': prefs or default_prefs(),
        'MetadataSearchResult': lambda **kw: kw,
        'Proxy': NS(Media=lambda content, sort_order=0: ('proxy', content)),
        'CACHE_1WEEK': 604800,
    }
    import builtins
    real_import = builtins.__import__

    def sandbox_import(name, globs=None, locs=None, fromlist=(), level=0):
        """Plex's Sandbox.__import__ (Framework/code/sandbox.py): a package
        already in sys.modules is returned as is, without loading the names in
        fromlist; only a first import goes through the real machinery."""
        if fail_import and name.startswith('audioborker_plex'):
            raise ImportError('simulated: no module named audioborker_plex')
        if name in sys.modules:
            return sys.modules[name]
        return real_import(name, globs, locs, fromlist, level)
    env['__builtins__'] = dict(vars(builtins), __import__=sandbox_import)

    path = os.path.join(BUNDLE, 'Code', '__init__.py')
    with io.open(path, encoding='utf-8') as f:
        exec(compile(f.read(), path, 'exec'), env)
    env['registered'] = registered
    return env


class ColdModules(object):
    """Inside Plex the agent process starts with none of the library loaded;
    the test process has usually imported it already. Unload it for the
    duration, then put the original modules back so other tests keep theirs."""

    def __enter__(self):
        self.saved = dict((k, v) for k, v in sys.modules.items()
                          if k == 'audioborker_plex' or k.startswith('audioborker_plex.'))
        for k in self.saved:
            del sys.modules[k]

    def __exit__(self, *exc):
        for k in [k for k in sys.modules if k == 'audioborker_plex' or k.startswith('audioborker_plex.')]:
            del sys.modules[k]
        sys.modules.update(self.saved)


PLEX_ITEM = (b'<MediaContainer librarySectionID="7"><Directory ratingKey="6747" type="album">'
             b'<Collection tag="Favourites" /></Directory></MediaContainer>')


def locke_routes():
    return [
        ('audnex.us/books/B004K50434?region=us', api_fixture('audnexus_book_B004K50434_us.json')),
        ('audnex.us/authors/B001DABSBQ', {'asin': 'B001DABSBQ', 'name': 'Scott Lynch',
                                          'description': '<p>Scott Lynch was born in 1978.</p>',
                                          'image': 'https://img/lynch.jpg',
                                          'genres': [{'name': 'Fantasy', 'type': 'genre'}]}),
        ('api.audible.com/1.0/catalog/products/B004K50434', api_fixture('audible_product_B004K50434_us.json')),
        ('api.audible.com/1.0/catalog/products?', api_fixture('audible_search_locke_us.json')),
        ('127.0.0.1:32400/library/metadata/6747', PLEX_ITEM),
        ('127.0.0.1:32400/library/sections/', b''),
        ('m.media-amazon.com', b'JPEGDATA'),
        ('https://img/', b'JPEGDATA'),
    ]


def album_media(path, ms):
    part = NS(file=path, duration=str(ms))
    tree = NS(id='6747', items=[], children=[NS(index='1', items=[NS(parts=[part])], children=[])])
    return NS(album='The Lies of Locke Lamora', artist='Scott Lynch', title=None, name='Fix Match text',
              filename=None, tree=tree), tree


def metadata(id):
    return NS(id=id, title=None, title_sort=None, summary=None, studio=None, rating=None,
              originally_available_at=None, genres=TagSet(), styles=TagSet(), moods=TagSet(),
              posters=Posters())


class GlueTest(unittest.TestCase):
    def test_registers_both_agents_like_audnexus(self):
        env = load_glue([])
        names = sorted((c.__name__, c.name, c.primary_provider) for c in env['registered'])
        self.assertEqual(names, [('AudioborkerAlbum', 'Audioborker', True),
                                 ('AudioborkerArtist', 'Audioborker', True)])
        for cls in env['registered']:
            # The framework silently drops an agent with any language code it
            # doesn't know; keep to the set the Audnexus agent proves works.
            self.assertEqual(cls.languages, ['en', 'de', 'es', 'fr', 'it', 'ja'])
            self.assertEqual(cls.accepts_from, ['com.plexapp.agents.localmedia'])
        env['Start']()
        self.assertIn('audioborker', env['HTTP'].Headers['User-Agent'])

    def test_album_search_and_update(self):
        env = load_glue(locke_routes())
        album = [c for c in env['registered'] if c.__name__ == 'AudioborkerAlbum'][0]()
        media, tree = album_media(TAGGED, 1319 * 60000)
        results = NS(items=[])
        results.Append = results.items.append
        album.search(results, media, 'en', False)
        self.assertEqual([(r['id'], r['score'], r['year']) for r in results.items], [('B004K50434_us', 100, 2011)])

        md = metadata('B004K50434_us')
        album.update(md, tree, 'en', False)
        self.assertEqual(md.title, 'The Lies of Locke Lamora')
        self.assertEqual(md.title_sort, 'Gentleman Bastard Sequence 1 - The Lies of Locke Lamora')
        self.assertEqual(md.studio, 'Gollancz')
        self.assertEqual(str(md.originally_available_at), '2011-01-21')
        self.assertEqual(md.genres, {'Science Fiction & Fantasy', 'Fantasy', 'Epic', 'Humorous',
                                     'Paranormal & Urban', 'Urban'})
        puts = env['HTTP'].methods
        self.assertEqual(len(puts), 1, puts)
        self.assertEqual(puts[0][0], 'PUT')
        self.assertIn('/library/sections/7/all?type=9&id=6747', puts[0][1])
        self.assertIn('=Favourites', puts[0][1])
        self.assertIn('=Gentleman%20Bastard%20Sequence', puts[0][1])
        self.assertEqual(md.styles, {'Michael Page'})
        self.assertEqual(md.moods, {'Scott Lynch', 'Series: Gentleman Bastard Sequence'})
        self.assertAlmostEqual(md.rating, 9.2)
        self.assertEqual(len(md.posters), 1)

    def test_collection_failure_does_not_cost_the_metadata(self):
        routes = [('127.0.0.1:32400', HTTPError(401))] + locke_routes()
        env = load_glue(routes)
        album = [c for c in env['registered'] if c.__name__ == 'AudioborkerAlbum'][0]()
        _, tree = album_media(TAGGED, 0)
        md = metadata('B004K50434_us')
        album.update(md, tree, 'en', False)
        self.assertEqual(md.title, 'The Lies of Locke Lamora')
        self.assertTrue([l for l in env['Log'].lines if 'Could not add book 6747' in l])

    def test_manual_album_search_uses_typed_text(self):
        env = load_glue(locke_routes())
        album = [c for c in env['registered'] if c.__name__ == 'AudioborkerAlbum'][0]()
        media, _ = album_media('/books/01.mp3', 0)
        media.name = 'B004K50434'
        results = NS(items=[])
        results.Append = results.items.append
        album.search(results, media, 'en', True)
        self.assertEqual([r['id'] for r in results.items], ['B004K50434_us'])

    def test_artist_search_and_update(self):
        env = load_glue(locke_routes())
        artist = [c for c in env['registered'] if c.__name__ == 'AudioborkerArtist'][0]()
        album_node, _ = album_media(TAGGED, 0)
        media = NS(artist='Scott Lynch', title='Scott Lynch', tree=NS(items=[], children=[album_node.tree]))
        results = NS(items=[])
        results.Append = results.items.append
        artist.search(results, media, 'en', False)
        self.assertEqual([(r['id'], r['score']) for r in results.items], [('B001DABSBQ_us', 100)])

        md = metadata('B001DABSBQ_us')
        artist.update(md, None, 'en', False)
        self.assertEqual((md.title, md.title_sort), ('Scott Lynch', 'Lynch, Scott'))
        self.assertEqual(md.summary, 'Scott Lynch was born in 1978.')
        self.assertEqual(md.genres, {'Fantasy'})

    def test_loads_from_cold_under_the_sandbox_importer(self):
        # The regression this guards: a second `from audioborker_plex import x`
        # in the glue failed inside Plex with "cannot import name fetch".
        with ColdModules():
            env = load_glue(locke_routes())
            self.assertIsNone(env['LOAD_ERROR'])
            album = [c for c in env['registered'] if c.__name__ == 'AudioborkerAlbum'][0]()
            media, _ = album_media(TAGGED, 1319 * 60000)
            results = NS(items=[])
            results.Append = results.items.append
            album.search(results, media, 'en', False)
            self.assertEqual([r['id'] for r in results.items], ['B004K50434_us'])

    def test_library_failure_still_registers_and_explains(self):
        env = load_glue(locke_routes(), fail_import=True)
        self.assertEqual(len(env['registered']), 2, 'agents must still appear in Plex')
        album = [c for c in env['registered'] if c.__name__ == 'AudioborkerAlbum'][0]()
        media, tree = album_media(TAGGED, 0)
        results = NS(items=[])
        results.Append = results.items.append
        env['Start']()
        album.search(results, media, 'en', False)
        md = metadata('B004K50434_us')
        album.update(md, tree, 'en', False)
        self.assertEqual(results.items, [])
        self.assertIsNone(md.title)
        log = '\n'.join(env['Log'].lines)
        self.assertIn('could not load its library', log)
        self.assertIn('simulated: no module named audioborker_plex', log)

    def test_http_404_is_not_found_not_an_outage(self):
        env = load_glue([])
        from audioborker_plex import catalog
        self.assertRaises(catalog.NotFound, env['fetch_once'], 'https://api.audnex.us/books/X?region=us')

    def test_glue_is_ascii(self):
        # Plex compiles Contents/Code with a Python 2 restricted compiler; keep
        # its input plain.
        with io.open(os.path.join(BUNDLE, 'Code', '__init__.py'), 'rb') as f:
            f.read().decode('ascii')

    def test_python2_source_encoding(self):
        # Python 2 refuses to compile a file holding any non-ASCII byte (even
        # in a comment) unless line 1 or 2 declares the encoding (PEP 263);
        # Python 3 doesn't care, so nothing else here would notice. Shipped
        # once: an em dash in a comment kept the library from loading.
        import glob
        import re
        for path in glob.glob(os.path.join(BUNDLE, '**', '*.py'), recursive=True):
            with io.open(path, 'rb') as f:
                raw = f.read()
            if any(b > 127 for b in bytearray(raw)):
                head = raw.splitlines()[:2]
                self.assertTrue([l for l in head if re.search(br'coding[:=]\s*utf-?8', l)],
                                '%s has non-ASCII bytes but no "# -*- coding: utf-8 -*-" line' % path)

    def test_info_plist_is_plain(self):
        import plistlib
        with io.open(os.path.join(BUNDLE, 'Info.plist'), 'rb') as f:
            raw = f.read()
        self.assertNotIn(b'<!--', raw, 'keep Info.plist free of comments: Plex reads it outside Python too')
        plist = plistlib.loads(raw)
        self.assertEqual(plist['CFBundleIdentifier'], 'com.plexapp.agents.audioborker')
        self.assertEqual(plist['PlexPluginClass'], 'Agent')
        self.assertEqual(plist['PlexPluginCodePolicy'], 'Elevated')


if __name__ == '__main__':
    unittest.main()
