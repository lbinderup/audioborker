# -*- coding: utf-8 -*-
"""Reads the iTunes-style metadata atoms and duration of an MP4/M4B file.

audioborker reads a file's tags through ffprobe; Plex agents cannot run
subprocesses, so this is a minimal pure-Python reader that produces the same
key spellings ffprobe reports (title, album_artist, synopsis, AUDIBLE_ASIN, …)
and feeds them through the same interpretation rules (embedded.py). It also
surfaces a few atoms ffprobe silently drops — the narrator (©nrt), publisher
(©pub) and movement name (©mvn) tone writes — under the names embedded.py
already looks for, so a tone-tagged file yields more, never less.

Only the moov box is read, never the audio: a 500 MB book costs a handful of
seeks and one read of a few hundred KB.
"""
from __future__ import absolute_import, unicode_literals

import os
import struct

# Format strings are bytes: Python 2.7 releases before 2.7.7 reject unicode
# ones, and unicode_literals would make every bare literal unicode.

from .compat import PY2, binary_type, text

# Atom -> ffprobe key, from libavformat/mov.c (mov_read_udta_string). Only
# text-valued atoms that could plausibly carry book metadata are listed.
ATOM_KEYS = {
    b'\xa9nam': 'title',
    b'\xa9alb': 'album',
    b'\xa9ART': 'artist',
    b'\xa9aut': 'artist',
    b'aART': 'album_artist',
    b'\xa9wrt': 'composer',
    b'\xa9com': 'composer',
    b'\xa9cmt': 'comment',
    b'\xa9inf': 'comment',
    b'\xa9gen': 'genre',
    b'\xa9day': 'date',
    b'desc': 'description',
    b'ldes': 'synopsis',
    b'\xa9st3': 'subtitle',
    b'cprt': 'copyright',
    b'\xa9cpy': 'copyright',
    b'tvsh': 'show',
    b'tven': 'episode_id',
    b'soal': 'sort_album',
    b'sonm': 'sort_name',
    b'soar': 'sort_artist',
    b'soaa': 'sort_album_artist',
    b'\xa9grp': 'grouping',
    b'\xa9too': 'encoder',
    # Not surfaced by ffprobe, but written by tone and other audiobook taggers.
    b'\xa9nrt': 'narrator',
    b'\xa9pub': 'publisher',
    b'\xa9mvn': 'movement_name',
}

# Integer-valued atoms, rendered as decimal strings like ffprobe does.
INT_ATOMS = {
    b'stik': 'media_type',
    b'\xa9mvi': 'movement',
}

CONTAINERS = (b'moov', b'udta', b'ilst')

# A moov larger than this is not a sane audiobook header (chapter tables for
# a 100-hour book are a few MB); refuse rather than allocate it.
MAX_MOOV = 64 * 1024 * 1024

MP4_EXTENSIONS = ('.m4b', '.m4a', '.mp4', '.aac', '.m4p', '.mov')


class Mp4Info(object):
    def __init__(self, tags, duration_ms, cover=None):
        self.tags = tags              # dict of ffprobe-style key -> text
        self.duration_ms = duration_ms  # 0 when unknown
        self.cover = cover            # embedded cover image bytes, or None


def is_mp4_path(path):
    return os.path.splitext(text(path).lower())[1] in MP4_EXTENSIONS


def read(path):
    """Returns Mp4Info for the file, or None when it is not a readable MP4."""
    with open(fs_path(path), 'rb') as f:
        moov = find_top_level(f, b'moov')
    if moov is None:
        return None
    tags = {}
    found = {'duration_ms': 0, 'cover': None}
    walk(moov, tags, found)
    return Mp4Info(tags, found['duration_ms'], found['cover'])


def fs_path(path):
    """Plex hands Python 2 agents utf-8 byte paths; Windows wants unicode."""
    if PY2 and os.name == 'nt' and isinstance(path, binary_type):
        return path.decode('utf-8')
    return path


def find_top_level(f, wanted):
    """Seeks through top-level boxes and returns the payload of `wanted`."""
    while True:
        header = f.read(8)
        if len(header) < 8:
            return None
        size, kind = struct.unpack(b'>I4s', header)
        header_len = 8
        if size == 1:
            large = f.read(8)
            if len(large) < 8:
                return None
            size = struct.unpack(b'>Q', large)[0]
            header_len = 16
        if kind == wanted:
            if size == 0:
                payload = f.read(MAX_MOOV + 1)
            else:
                if size - header_len > MAX_MOOV:
                    return None
                payload = f.read(size - header_len)
            if len(payload) > MAX_MOOV:
                return None
            return payload
        if size == 0:
            return None  # box runs to EOF and it is not the one we want
        if size < header_len:
            return None  # corrupt
        f.seek(size - header_len, 1)


def boxes(data):
    """Splits a byte string into (kind, payload) child boxes."""
    out = []
    pos = 0
    end = len(data)
    while pos + 8 <= end:
        size, kind = struct.unpack(b'>I4s', data[pos:pos + 8])
        header_len = 8
        if size == 1:
            if pos + 16 > end:
                break
            size = struct.unpack(b'>Q', data[pos + 8:pos + 16])[0]
            header_len = 16
        elif size == 0:
            size = end - pos
        if size < header_len or pos + size > end:
            break
        out.append((kind, data[pos + header_len:pos + size]))
        pos += size
    return out


def walk(data, tags, found):
    for kind, payload in boxes(data):
        if kind == b'mvhd':
            found['duration_ms'] = mvhd_duration_ms(payload)
        elif kind == b'meta':
            walk(meta_children(payload), tags, found)
        elif kind in CONTAINERS:
            if kind == b'ilst':
                read_ilst(payload, tags, found)
            else:
                walk(payload, tags, found)


def meta_children(payload):
    """'meta' is a FullBox in ISO files but a plain box in QuickTime ones; the
    plain form starts directly with a child box header (hdlr)."""
    if len(payload) >= 8 and payload[4:8] == b'hdlr':
        return payload
    return payload[4:]


def mvhd_duration_ms(payload):
    if len(payload) < 20:
        return 0
    version = struct.unpack(b'>B', payload[0:1])[0]
    if version == 1:
        if len(payload) < 32:
            return 0
        timescale, duration = struct.unpack(b'>IQ', payload[20:32])
    else:
        timescale, duration = struct.unpack(b'>II', payload[12:20])
    if timescale == 0:
        return 0
    return int(duration * 1000 // timescale)


def read_ilst(payload, tags, found):
    for kind, item in boxes(payload):
        if kind == b'covr':
            # The cover is already in memory with the rest of moov, so keeping
            # it costs nothing; only the first image counts.
            if found['cover'] is None:
                found['cover'] = cover_bytes(item)
            continue
        if kind == b'----':
            key, value = freeform(item)
        elif kind in ATOM_KEYS:
            key, value = ATOM_KEYS[kind], data_text(item)
        elif kind in INT_ATOMS:
            key, value = INT_ATOMS[kind], data_int(item)
        else:
            continue
        # First occurrence wins, matching ffprobe's handling of repeats.
        if key and value and key not in tags:
            tags[key] = value


def cover_bytes(item):
    """The image in a covr atom (data type 13 = JPEG, 14 = PNG, 0 = some
    writers' 'implicit'), or None."""
    type_code, value = first_data(item)
    if value and type_code in (0, 13, 14, 27):
        return value
    return None


def first_data(item):
    for kind, payload in boxes(item):
        if kind == b'data' and len(payload) >= 8:
            type_code = struct.unpack(b'>I', payload[0:4])[0] & 0xFFFFFF
            return type_code, payload[8:]
    return None, None


def data_text(item):
    type_code, value = first_data(item)
    if value is None:
        return ''
    if type_code == 2:  # UTF-16
        return value.decode('utf-16-be', 'replace')
    if type_code in (1, 0):  # UTF-8, or implicit (some writers mislabel text)
        return text(value).rstrip('\x00')
    return ''


def data_int(item):
    type_code, value = first_data(item)
    if value is None or type_code not in (0, 21, 22) or not value:
        return ''
    if len(value) > 8:
        return ''
    n = 0
    for byte in bytearray(value):
        n = (n << 8) | byte
    return text(str(n))


def freeform(item):
    """'----' atoms carry their own name ('AUDIBLE_ASIN', 'SERIES', …);
    ffprobe reports them under that bare name, without the mean namespace."""
    name = ''
    for kind, payload in boxes(item):
        if kind == b'name' and len(payload) > 4:
            name = text(payload[4:]).rstrip('\x00')
    if not name:
        return '', ''
    return name, data_text(item)
