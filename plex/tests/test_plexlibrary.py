# -*- coding: utf-8 -*-
from __future__ import absolute_import, unicode_literals

import unittest

import helpers  # noqa: F401 - puts the bundle's library on sys.path

from audioborker_plex import plexlibrary

try:
    from urllib.parse import parse_qsl, urlsplit
except ImportError:  # pragma: no cover
    from urlparse import parse_qsl, urlsplit

# The shape of GET /library/metadata/{id} for an album (PMS 1.43).
ITEM = b'''<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer size="1" allowSync="1" identifier="com.plexapp.plugins.library"
    librarySectionID="7" librarySectionTitle="Audiobooks" librarySectionUUID="x">
  <Directory ratingKey="6747" key="/library/metadata/6747/children" type="album"
      title="The Last Colony" parentTitle="John Scalzi">
    <Genre id="1" filter="genre=1" tag="Science Fiction &amp; Fantasy" />
    <Collection id="9" filter="collection=9" tag="Favourites" />
    <Collection id="10" filter="collection=10" tag="Sk\xc3\xb8nlitteratur" />
  </Directory>
</MediaContainer>'''


def query(url):
    return parse_qsl(urlsplit(url).query, keep_blank_values=True)


class PlexLibraryTest(unittest.TestCase):
    def test_parse_item(self):
        self.assertEqual(plexlibrary.parse_item(ITEM), ('7', ['Favourites', 'Skønlitteratur']))

    def test_adds_the_series_and_keeps_existing_collections(self):
        url = plexlibrary.add_collection_url('7', '6747', ['Favourites', 'Skønlitteratur'], "Old Man's War")
        self.assertTrue(url.startswith('http://127.0.0.1:32400/library/sections/7/all?'), url)
        self.assertEqual(query(url), [
            ('type', '9'), ('id', '6747'),
            ('collection[0].tag.tag', 'Favourites'),
            ('collection[1].tag.tag', 'Skønlitteratur'),
            ('collection[2].tag.tag', "Old Man's War"),
            ('collection.locked', '1'),
        ])

    def test_nothing_to_do(self):
        self.assertIsNone(plexlibrary.add_collection_url('7', '1', ["Old Man's War"], "old man's war"))
        self.assertIsNone(plexlibrary.add_collection_url('7', '1', [], ''))
        self.assertIsNone(plexlibrary.add_collection_url('', '1', [], 'Series'))


if __name__ == '__main__':
    unittest.main()
