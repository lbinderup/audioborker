# -*- coding: utf-8 -*-
"""Python 2.7 / 3 shims.

Plex runs legacy agents on Python 2.7, while the tests run on Python 3, so
this package sticks to the common subset. Everything inside works on unicode
text: values arrive from Plex as utf-8 byte strings and from JSON as unicode,
and mixing the two on Python 2 raises UnicodeDecodeError on the first
non-ASCII title. text() at every entry point keeps them apart.
"""
from __future__ import absolute_import, unicode_literals

import re
import sys

PY2 = sys.version_info[0] == 2

if PY2:  # pragma: no cover - exercised only inside Plex
    text_type = unicode  # noqa: F821
    binary_type = str
else:
    text_type = str
    binary_type = bytes

# Go's RE2 classes (\b, \s, \w) are ASCII-only. Python 3 patterns are
# Unicode-aware unless told otherwise, which would make the ported regexes
# disagree with audioborker on accented names. Python 2 is ASCII by default.
RE_ASCII = getattr(re, 'ASCII', 0)


def text(value):
    """Coerces any value to unicode text; None becomes ''."""
    if value is None:
        return ''
    if isinstance(value, binary_type):
        return value.decode('utf-8', 'replace')
    if isinstance(value, text_type):
        return value
    return text_type(value)


def describe(exc):
    """An exception's message as unicode, for logging. On Python 2,
    u'%s' % exc decodes the message as ASCII and itself raises when an
    IOError names a path like '/books/Émile Zola' — turning a harmless log
    line into a failed search."""
    try:
        return text_type(exc)
    except UnicodeError:
        try:
            return text(str(exc))
        except UnicodeError:
            return text(repr(exc))


def utf8_len(s):
    """Byte length of s as UTF-8 — what Go's len() measures on a string."""
    return len(text(s).encode('utf-8'))
