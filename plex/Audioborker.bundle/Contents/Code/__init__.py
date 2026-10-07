# Audioborker agent for Plex: matches audiobooks the way audioborker does and
# fills in the metadata audioborker would tag.
#
# This file is only glue between Plex's plug-in framework and the library in
# Contents/Libraries/Shared/audioborker_plex, where all matching and metadata
# logic lives, tested against audioborker's own Go code (see plex/README.md).
# That library runs as ordinary Python outside the plug-in sandbox and opens
# the audio files to read their tags, which is why Info.plist asks for the
# Elevated code policy.
#
# Sandbox rules apply to this file: no names starting with an underscore, no
# augmented assignment on attributes, no sum/any/all builtins. Keep it ASCII.
VERSION = '1.2.0'

# An import failure here must not stop the agent classes below from being
# defined: an agent that never registers simply vanishes from Plex's agent
# lists with nothing to explain why. Registered but unloaded, every call logs
# the reason instead.
try:
    # One statement on purpose: Plex's sandbox importer skips submodule
    # loading for a package it has already imported (see the package's
    # __init__.py, which now loads every submodule up front anyway).
    from audioborker_plex import catalog, compat, core, plexlibrary, fetch as fetchpolicy
    LOAD_ERROR = None
except Exception as e:
    LOAD_ERROR = e
    Log.Exception('Audioborker could not load its library from Contents/Libraries/Shared '
                  '(is PlexPluginCodePolicy Elevated in Info.plist?)')


def not_loaded():
    if LOAD_ERROR is None:
        return False
    Log.Error('Audioborker agent %s is not working: its library failed to load (%s). '
              'See the first lines of this log for the full error.', VERSION, LOAD_ERROR)
    return True


# The framework silently drops an agent if it rejects any of these codes, so
# keep to the set the Audnexus agent is known to register with.
LANGUAGES = [Locale.Language.English, 'de', 'es', 'fr', 'it', 'ja']


def Start():
    HTTP.CacheTime = CACHE_1WEEK
    HTTP.Headers['User-Agent'] = 'audioborker-plex/' + VERSION
    HTTP.Headers['Accept-Encoding'] = 'gzip'
    if LOAD_ERROR is None:
        Log.Info('Audioborker agent %s started', VERSION)
    else:
        not_loaded()


def ValidatePrefs():
    pass


def settings():
    return core.Settings(
        region=Prefs['region'],
        policy=Prefs['auto_match'],
        authors_as_moods=Prefs['store_author_as_mood'],
        sort_author_by_last_name=Prefs['sort_author_by_last_name'],
        audnexus_url=Prefs['audnexus_url'],
        series_collections=Prefs['series_collections'],
    )


def fetch_once(url):
    Log.Debug('GET %s', url)
    try:
        body = HTTP.Request(url, timeout=30, headers={'Accept': 'application/json'}).content
    except Exception as e:
        raise fetchpolicy.classify(fetchpolicy.status_of(e), url)
    try:
        return JSON.ObjectFromString(body)
    except Exception:
        raise catalog.SourceError('Invalid JSON from ' + url)


FETCH = fetchpolicy.make_fetcher(fetch_once) if LOAD_ERROR is None else None


def log_info(msg):
    Log.Info(msg)


def log_debug(msg):
    Log.Debug(msg)


def set_values(container, values):
    container.clear()
    for value in values:
        container.add(value)


def set_poster(metadata, url, force):
    if not url:
        return
    if url not in metadata.posters or force:
        try:
            metadata.posters[url] = Proxy.Media(HTTP.Request(url, timeout=60).content, sort_order=0)
        except Exception as e:
            Log.Warn('Could not download cover %s: %s', url, compat.describe(e))
            return
    metadata.posters.validate_keys([url])


def set_embedded_poster(metadata, key, data):
    # A hand-tagged book's cover is the one embedded in its file.
    if key not in metadata.posters:
        metadata.posters[key] = Proxy.Media(data, sort_order=0)
    metadata.posters.validate_keys([key])


def add_to_collection(media, name):
    # Best effort: a refused edit must never cost the book its metadata.
    rating_key = getattr(media, 'id', None)
    if not rating_key:
        Log.Warn('No library id for this book; cannot add it to the "%s" collection', name)
        return
    try:
        body = HTTP.Request(plexlibrary.item_url(rating_key), cacheTime=0).content
        section, existing = plexlibrary.parse_item(body)
        url = plexlibrary.add_collection_url(section, rating_key, existing, name)
        if url:
            HTTP.Request(url, method='PUT', cacheTime=0).content
            Log.Info('Added book %s to the "%s" collection', rating_key, name)
    except Exception as e:
        Log.Warn('Could not add book %s to the "%s" collection: %s', rating_key, name, compat.describe(e))


class AudioborkerArtist(Agent.Artist):
    name = 'Audioborker'
    languages = LANGUAGES
    primary_provider = True
    accepts_from = ['com.plexapp.agents.localmedia']

    def search(self, results, media, lang, manual):
        if not_loaded():
            return
        hint = media.artist or getattr(media, 'title', None) or ''
        tree = getattr(media, 'tree', None)
        local = core.gather(core.first_album_entries(tree), log=log_debug, measure=False) if tree is not None else None
        Log.Info('Author search: %r (manual=%s)', hint, manual)
        rows = core.search_artist(FETCH, hint, local, settings(), manual=manual,
                                  manual_text=hint if manual else '', log=log_info)
        for row in rows:
            results.Append(MetadataSearchResult(id=row['id'], name=row['name'], score=row['score'], lang=lang))

    def update(self, metadata, media, lang, force):
        if not_loaded():
            return
        info = core.artist_metadata(FETCH, metadata.id, settings(), log=log_info)
        metadata.title = info['title']
        metadata.title_sort = info['title_sort']
        metadata.summary = info['summary']
        set_values(metadata.genres, info['genres'])
        set_poster(metadata, info['poster'], force)
        Log.Info('Updated author %s (%s)', info['title'], metadata.id)


class AudioborkerAlbum(Agent.Album):
    name = 'Audioborker'
    languages = LANGUAGES
    primary_provider = True
    accepts_from = ['com.plexapp.agents.localmedia']

    def search(self, results, media, lang, manual):
        if not_loaded():
            return
        tree = getattr(media, 'tree', None)
        entries = core.tree_entries(tree)
        if not entries:
            entries = core.filename_entries(getattr(media, 'filename', None))
        album = media.album or getattr(media, 'title', None) or ''
        local = core.gather(entries, album, media.artist, log=log_debug)
        typed = (getattr(media, 'name', None) or '') if manual else ''
        Log.Info('Book search: album=%r artist=%r typed=%r files=%d runtime=%d min file ASIN=%r path ASIN=%r',
                 album, media.artist, typed, len(entries), local.runtime_min,
                 (local.file_book or {}).get('asin', ''), local.path_asin)
        rows = core.search_album(FETCH, local, settings(), manual=manual, manual_text=typed, log=log_info)
        for row in rows:
            results.Append(MetadataSearchResult(id=row['id'], name=row['name'], year=row['year'],
                                                score=row['score'], lang=lang))

    def update(self, metadata, media, lang, force):
        if not_loaded():
            return
        local = core.gather(core.tree_entries(media), log=log_debug, measure=False)
        info = core.album_metadata(FETCH, metadata.id, local, settings(), log=log_info)
        metadata.title = info['title']
        metadata.title_sort = info['title_sort']
        metadata.summary = info['summary']
        metadata.studio = info['studio']
        if info['originally_available_at'] is not None:
            metadata.originally_available_at = info['originally_available_at']
        if info['rating'] is not None:
            metadata.rating = info['rating']
        set_values(metadata.genres, info['genres'])
        set_values(metadata.styles, info['styles'])
        set_values(metadata.moods, info['moods'])
        if info['cover_bytes']:
            set_embedded_poster(metadata, info['cover_key'], info['cover_bytes'])
        else:
            set_poster(metadata, info['poster'], force)
        if info['collection']:
            add_to_collection(media, info['collection'])
        Log.Info('Updated book %s (%s); fields from the file: %s; sources: %s',
                 info['title'], metadata.id, ', '.join(info['from_file']) or 'none', info['sources'])
