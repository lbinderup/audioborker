# -*- coding: utf-8 -*-
from __future__ import absolute_import, unicode_literals

import os
import shutil
import struct
import tempfile
import unittest

from helpers import FIXTURES, load_json

from audioborker_plex import embedded, mp4

TAGGED = os.path.join(FIXTURES, 'tagged.m4b')

# Atoms ffprobe reads from outside ilst (ftyp) or that the reader does not map
# because nothing downstream uses them.
NOT_MAPPED = ('major_brand', 'minor_version', 'compatible_brands', 'gapless_playback')


def box(kind, payload=b''):
    return struct.pack('>I4s', 8 + len(payload), kind) + payload


def text_item(kind, value):
    return box(kind, box(b'data', struct.pack('>II', 1, 0) + value.encode('utf-8')))


def mvhd(timescale, duration):
    return box(b'mvhd', b'\x00' * 12 + struct.pack('>II', timescale, duration) + b'\x00' * 80)


class ReaderTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp()

    def tearDown(self):
        shutil.rmtree(self.dir)

    def write(self, data, name='book.m4b'):
        path = os.path.join(self.dir, name)
        with open(path, 'wb') as f:
            f.write(data)
        return path

    def test_reads_what_ffprobe_reads_from_a_tone_tagged_file(self):
        want = load_json(os.path.join(FIXTURES, 'tagged.ffprobe.json'))['format']
        info = mp4.read(TAGGED)
        for key, value in want['tags'].items():
            if key in NOT_MAPPED:
                continue
            self.assertEqual(info.tags.get(key), value, key)
        self.assertAlmostEqual(info.duration_ms, float(want['duration']) * 1000, delta=50)

    def test_surfaces_atoms_ffprobe_drops(self):
        tags = mp4.read(TAGGED).tags
        self.assertEqual(tags['narrator'], 'Michael Page')
        self.assertEqual(tags['publisher'], 'Gollancz')
        self.assertEqual(tags['movement_name'], 'Gentleman Bastard Sequence')

    def test_tone_tagged_file_as_a_book(self):
        b = embedded.book(mp4.read(TAGGED).tags)
        self.assertEqual(b['asin'], 'B004K50434')
        self.assertEqual(b['title'], 'The Lies of Locke Lamora')
        self.assertEqual(b['authors'], ['Scott Lynch'])
        self.assertEqual(b['narrators'], ['Michael Page'])
        self.assertEqual((b['series_name'], b['series_position']), ('Gentleman Bastard Sequence', '1'))
        self.assertEqual(b['release_date'], '2011-01-21')
        self.assertEqual(b['publisher'], 'Gollancz')
        self.assertIn('Thorn of Camorr', b['summary'])

    def test_moov_after_the_audio(self):
        # Files written without faststart keep moov at the end.
        moov = box(b'moov', mvhd(1000, 61000) + box(b'udta', box(b'meta', b'\x00' * 4 + box(b'ilst', text_item(b'\xa9nam', 'Late')))))
        path = self.write(box(b'ftyp', b'M4B ') + box(b'mdat', b'\x00' * 5000) + moov)
        info = mp4.read(path)
        self.assertEqual(info.tags['title'], 'Late')
        self.assertEqual(info.duration_ms, 61000)

    def test_quicktime_meta_without_version_header(self):
        meta = box(b'meta', box(b'hdlr', b'\x00' * 20) + box(b'ilst', text_item(b'\xa9ART', 'QT Author')))
        path = self.write(box(b'moov', box(b'udta', meta)))
        self.assertEqual(mp4.read(path).tags['artist'], 'QT Author')

    def test_64bit_box_sizes_and_version_1_mvhd(self):
        mdat = struct.pack('>I4sQ', 1, b'mdat', 16 + 10) + b'\x00' * 10
        mvhd1 = box(b'mvhd', b'\x01' + b'\x00' * 19 + struct.pack('>IQ', 44100, 44100 * 90) + b'\x00' * 80)
        path = self.write(mdat + box(b'moov', mvhd1))
        self.assertEqual(mp4.read(path).duration_ms, 90000)

    def test_freeform_and_integer_atoms(self):
        freeform = box(b'----', box(b'mean', b'\x00' * 4 + b'com.pilabor.tone')
                       + box(b'name', b'\x00' * 4 + b'AUDIBLE_ASIN')
                       + box(b'data', struct.pack('>II', 1, 0) + b'B0TESTASIN'))
        stik = box(b'stik', box(b'data', struct.pack('>II', 21, 0) + b'\x02'))
        path = self.write(box(b'moov', box(b'udta', box(b'meta', b'\x00' * 4 + box(b'ilst', freeform + stik)))))
        tags = mp4.read(path).tags
        self.assertEqual(tags['AUDIBLE_ASIN'], 'B0TESTASIN')
        self.assertEqual(tags['media_type'], '2')

    def test_not_an_mp4(self):
        self.assertIsNone(mp4.read(self.write(b'ID3\x04\x00' + b'\x00' * 200, 'a.mp3')))

    def test_truncated_file_does_not_raise(self):
        with open(TAGGED, 'rb') as f:
            data = f.read()
        for cut in (3, 10, 40, len(data) // 2, len(data) - 7):
            info = mp4.read(self.write(data[:cut]))
            self.assertTrue(info is None or isinstance(info.tags, dict))

    def test_is_mp4_path(self):
        self.assertTrue(mp4.is_mp4_path('/x/Book.M4B'))
        self.assertTrue(mp4.is_mp4_path(b'/x/b.m4a'))
        self.assertFalse(mp4.is_mp4_path('/x/01.mp3'))


if __name__ == '__main__':
    unittest.main()
