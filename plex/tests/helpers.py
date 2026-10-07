# -*- coding: utf-8 -*-
"""Shared test setup: puts the bundle's library on sys.path and locates the
parity fixtures recorded by audioborker's Go code (internal/parity)."""
from __future__ import absolute_import, unicode_literals

import io
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(HERE))
SHARED = os.path.join(REPO, 'plex', 'Audioborker.bundle', 'Contents', 'Libraries', 'Shared')
PARITY = os.path.join(REPO, 'internal', 'parity', 'testdata')
FIXTURES = os.path.join(HERE, 'fixtures')

if SHARED not in sys.path:
    sys.path.insert(0, SHARED)


def load_json(path):
    with io.open(path, encoding='utf-8') as f:
        return json.load(f)


def golden():
    return load_json(os.path.join(PARITY, 'golden.json'))


def api_fixture(name):
    return load_json(os.path.join(PARITY, 'api', name))


class FakeFetch(object):
    """fetch(url) backed by a list of (url substring, response) pairs; the
    response is a JSON value or an exception instance to raise."""

    def __init__(self, routes):
        self.routes = routes
        self.calls = []

    def __call__(self, url):
        self.calls.append(url)
        for needle, response in self.routes:
            if needle in url:
                if isinstance(response, Exception):
                    raise response
                return response
        from audioborker_plex.catalog import NotFound
        raise NotFound('no route for ' + url)
