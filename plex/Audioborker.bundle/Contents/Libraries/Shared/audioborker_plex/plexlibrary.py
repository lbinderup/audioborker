# -*- coding: utf-8 -*-
"""Series collections through the Plex server's own HTTP API.

The legacy agent framework's album model has no collections field (the
Audnexus agent documents the same limit), so an agent cannot set them the
normal way. The plug-in framework does, however, authenticate requests to
http://127.0.0.1 with the server's token, so the agent can make the same edit
Plex Web makes when you type a collection into an album's Tags.

Only the book's own series is ever added; collections already on the album —
including ones you made yourself — are kept, because the edit replaces the
whole list.
"""
from __future__ import absolute_import, unicode_literals

import xml.etree.ElementTree as ET

from .compat import text

try:
    from urllib import quote as url_quote  # Python 2
except ImportError:
    from urllib.parse import quote as url_quote  # novermin

PLEX = 'http://127.0.0.1:32400'
ALBUM_TYPE = 9  # Plex's metadata type number for albums


def item_url(rating_key):
    return '%s/library/metadata/%s' % (PLEX, text(rating_key))


def parse_item(xml_body):
    """(library section id, [collection names]) from a /library/metadata/{id}
    response; section id is '' when the response doesn't name one."""
    root = ET.fromstring(xml_body)
    section = text(root.get('librarySectionID'))
    names = []
    for item in list(root):
        section = section or text(item.get('librarySectionID'))
        for tag in item.findall('Collection'):
            name = text(tag.get('tag')).strip()
            if name:
                names.append(name)
    return section, names


def quote(s):
    return url_quote(text(s).encode('utf-8'), safe=b'')


def add_collection_url(section, rating_key, existing, wanted):
    """The PUT URL that puts the album in `wanted` while keeping `existing`,
    or None when there is nothing to do."""
    wanted = text(wanted).strip()
    if not wanted or not section:
        return None
    if wanted.lower() in [e.lower() for e in existing]:
        return None
    names = list(existing) + [wanted]
    params = [('type', text(ALBUM_TYPE)), ('id', text(rating_key))]
    params += [('collection[%d].tag.tag' % i, n) for i, n in enumerate(names)]
    # Locked like an edit in Plex Web, so no later refresh clears it.
    params.append(('collection.locked', '1'))
    return '%s/library/sections/%s/all?%s' % (
        PLEX, quote(section), '&'.join(quote(k) + '=' + quote(v) for k, v in params))
