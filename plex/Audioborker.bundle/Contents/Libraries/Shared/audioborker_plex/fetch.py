# -*- coding: utf-8 -*-
"""Retry and rate-limit policy around the injected HTTP fetch, ported from
audioborker's audnexus.getJSON: up to 3 attempts on transient failures with
exponential backoff and jitter, and a token bucket keeping Audnexus traffic
well under its 100 requests/minute. A Plex library scan fires many lookups
in parallel threads, so the limiter is shared and thread-safe."""
from __future__ import absolute_import, unicode_literals

import random
import re
import threading
import time

from .catalog import NotFound, SourceError
from .compat import text


class TransientError(SourceError):
    """429/408/5xx or a network failure: worth retrying."""


STATUS_IN_TEXT = re.compile(r'HTTP(?: Error)?\s*(\d{3})')


def status_of(exc):
    """The HTTP status behind an exception, or None for network failures.
    urllib2.HTTPError carries .code; anything wrapping it is read from its
    message, so a 404 is never mistaken for an outage — that distinction is
    what lets a lookup move on to the next storefront."""
    for attr in ('code', 'status'):
        try:
            code = int(getattr(exc, attr))
        except (AttributeError, TypeError, ValueError):
            continue
        if 100 <= code <= 599:
            return code
    m = STATUS_IN_TEXT.search(text(exc))
    return int(m.group(1)) if m else None


def classify(code, url):
    """Maps an HTTP status (None for a network error) to an exception."""
    if code == 404:
        return NotFound('not found (check the ASIN and region): ' + url)
    if code is None or code in (408, 429) or code >= 500:
        return TransientError('HTTP %s from %s' % (code if code is not None else 'error', url))
    return SourceError('HTTP %s from %s' % (code, url))


class Limiter(object):
    """n requests per period, refilled continuously."""

    def __init__(self, n, period, clock=time.time, sleep=time.sleep):
        self.capacity = float(n)
        self.rate = float(n) / float(period)
        self.tokens = float(n)
        self.stamp = clock()
        self.clock = clock
        self.sleep = sleep
        self.lock = threading.Lock()

    def wait(self):
        while True:
            with self.lock:
                now = self.clock()
                self.tokens = min(self.capacity, self.tokens + (now - self.stamp) * self.rate)
                self.stamp = now
                if self.tokens >= 1:
                    self.tokens -= 1
                    return
                needed = (1 - self.tokens) / self.rate
            self.sleep(needed)


AUDNEXUS_LIMIT = Limiter(80, 60)


def make_fetcher(fetch_once, sleep=time.sleep, limiter=AUDNEXUS_LIMIT,
                 limited_host='audnex.us'):
    """Wraps fetch_once(url) -> JSON (raising NotFound/TransientError/
    SourceError) with the retry policy. Only Audnexus is rate limited; the
    Audible catalog has no published limit."""
    def fetch(url):
        last = None
        for attempt in range(3):
            if attempt > 0:
                sleep((1 << attempt) * 0.5 + random.random() * 0.3)
            if limiter is not None and limited_host in url:
                limiter.wait()
            try:
                return fetch_once(url)
            except TransientError as e:
                last = e
        raise last
    return fetch
