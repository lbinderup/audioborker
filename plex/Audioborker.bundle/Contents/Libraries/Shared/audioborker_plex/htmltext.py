# -*- coding: utf-8 -*-
"""Port of audioborker's metadata.HTMLToText: flattens a provider's HTML
blurb into plain text — block-level tags become paragraph breaks, other tags
are dropped, entities are decoded."""
from __future__ import absolute_import, unicode_literals

import re

from .compat import RE_ASCII, text

try:
    from html import unescape as html_unescape  # novermin
except ImportError:  # pragma: no cover - Python 2 inside Plex
    from HTMLParser import HTMLParser  # novermin
    html_unescape = HTMLParser().unescape

HTML_BREAK = re.compile(r'<br\s*/?>|</p\s*>|</div\s*>|</li\s*>', re.IGNORECASE | RE_ASCII)
HTML_TAG = re.compile(r'<[^>]*>')
HTML_SPACE = re.compile(r'[ \t]{2,}')
HTML_BLANK = re.compile(r'\n{3,}')

# strings.TrimSpace's set (see match.go_trim_space).
GO_SPACE = (' \t\n\v\f\r\x85\xa0       '
            '         　')


def html_to_text(s):
    s = text(s)
    if s == '':
        return ''
    s = s.replace('\r\n', '\n')
    s = HTML_BREAK.sub('\n\n', s)
    s = HTML_TAG.sub('', s)
    s = html_unescape(s)
    s = s.replace('\xa0', ' ')  # non-breaking space
    s = HTML_SPACE.sub(' ', s)
    s = HTML_BLANK.sub('\n\n', s)
    # Tag removal leaves stray spaces at line edges (e.g. from "</p> <p>").
    lines = [line.strip(GO_SPACE) for line in s.split('\n')]
    return '\n'.join(lines).strip(GO_SPACE)
