# -*- coding: utf-8 -*-
"""Shows what the Plex agent would do for a book, without Plex.

    python plex/tools/dry_run.py "/books/Author/Title"            # folder or file
    python plex/tools/dry_run.py book.m4b --runtime 1319          # pretend Plex knows the length
    python plex/tools/dry_run.py book.m4b --manual "Locke Lamora"  # a Fix Match search
    python plex/tools/dry_run.py --album "Title" --artist "Author" --runtime 1319

Talks to the live Audible catalog and Audnexus, exactly as the agent does.
"""
from __future__ import absolute_import, print_function, unicode_literals

import argparse
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, '..', 'Audioborker.bundle', 'Contents', 'Libraries', 'Shared'))

from audioborker_plex import core, mp4  # noqa: E402
from audioborker_plex.fetch import classify, make_fetcher  # noqa: E402

try:
    from urllib.request import Request, urlopen  # novermin
    from urllib.error import HTTPError, URLError  # novermin
except ImportError:  # Python 2
    from urllib2 import HTTPError, Request, URLError, urlopen  # novermin

AUDIO = ('.m4b', '.m4a', '.mp4', '.aac', '.mp3', '.flac', '.ogg', '.opus', '.wma')


def fetch_once(url):
    req = Request(url, headers={'User-Agent': 'audioborker-plex/dry-run', 'Accept': 'application/json'})
    try:
        return json.loads(urlopen(req, timeout=30).read().decode('utf-8'))  # novermin
    except HTTPError as e:
        raise classify(e.code, url)
    except URLError:
        raise classify(None, url)


def audio_files(path):
    if os.path.isfile(path):
        return [path]
    found = []
    for root, _, files in os.walk(path):
        found.extend(os.path.join(root, f) for f in files if f.lower().endswith(AUDIO))
    return sorted(found)


def main():
    p = argparse.ArgumentParser(description=__doc__.split('\n')[0])
    p.add_argument('path', nargs='?', help='book file or folder')
    p.add_argument('--album', default='', help="Plex's album hint (default: the file's title)")
    p.add_argument('--artist', default='', help="Plex's artist hint")
    p.add_argument('--runtime', type=int, default=0, help='total minutes Plex reports (default: read MP4 headers)')
    p.add_argument('--manual', default=None, help='run a Fix Match search with this text')
    p.add_argument('--region', default='us')
    p.add_argument('--lenient', action='store_true', help='use the lenient auto-match policy')
    args = p.parse_args()

    files = audio_files(args.path) if args.path else []
    per_file = (args.runtime * 60000 // len(files)) if (args.runtime and files) else None
    entries = [(f, per_file) for f in files]
    local = core.gather(entries, args.album, args.artist, log=print)
    if not files and args.runtime:
        local.runtime_min = args.runtime
    settings = core.Settings(region=args.region,
                             policy=core.POLICY_LENIENT if args.lenient else core.POLICY_AUDIOBORKER)
    fetch = make_fetcher(fetch_once)

    print('Files: %d, runtime: %d min, file ASIN: %r, path ASIN/region: %r/%r'
          % (len(files), local.runtime_min, (local.file_book or {}).get('asin', ''),
             local.path_asin, local.path_region))
    rows = core.search_album(fetch, local, settings, manual=args.manual is not None,
                             manual_text=args.manual or '', log=lambda m: print('  ' + m))
    print('\nResults:')
    for r in rows:
        print('  %3d  %-16s %s (%s)' % (r['score'], r['id'], r['name'], r['year']))
    if not rows:
        return
    top = rows[0]
    if top['score'] < core.SURE and args.manual is None:
        print('\nNot confident enough to match automatically; Fix Match would list the above.')
        return
    print('\nMetadata for %s:' % top['id'])
    md = core.album_metadata(fetch, top['id'], local, settings, log=lambda m: print('  ' + m))
    md['summary'] = md['summary'][:160] + ('…' if len(md['summary']) > 160 else '')
    md['originally_available_at'] = str(md['originally_available_at'])
    if md.get('cover_bytes'):
        md['cover_bytes'] = '<%d bytes embedded in the file>' % len(md['cover_bytes'])
    print(json.dumps(md, indent=2, ensure_ascii=False))


if __name__ == '__main__':
    if hasattr(sys.stdout, 'reconfigure'):
        sys.stdout.reconfigure(encoding='utf-8')
    main()
