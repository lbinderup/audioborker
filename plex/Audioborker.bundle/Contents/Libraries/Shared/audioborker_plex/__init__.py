# -*- coding: utf-8 -*-
"""Matching and metadata logic of the Audioborker Plex agent.

Ported from audioborker (internal/match, internal/metadata/...) and pinned to
it by internal/parity's golden fixtures. Runs on Python 2.7 inside Plex and on
Python 3 in the tests, so it sticks to the common subset (see compat.py).
"""
from __future__ import absolute_import

# Every submodule is loaded here, eagerly. Plex's sandbox replaces __import__
# with one that, once a package is in sys.modules, returns it as is and skips
# loading the names in the import's fromlist. So in Contents/Code a second
# `from audioborker_plex import x` fails with "cannot import name x" unless x
# was already loaded. That shipped once: the agent never registered.
from . import compat, htmltext, match, embedded, mp4, genres, catalog, aggregate, fetch, plexlibrary, core  # noqa: F401
