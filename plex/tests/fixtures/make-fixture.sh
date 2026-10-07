#!/usr/bin/env sh
# Regenerates tagged.m4b: three seconds of silence tagged with exactly the
# flags audioborker's pipeline passes to tone (internal/pipeline/tone.go), plus
# ffprobe's view of it (tagged.ffprobe.json) for the reader test to compare
# against. Needs ffmpeg, ffprobe and tone on PATH (or TONE=/path/to/tone).
set -eu
cd "$(dirname "$0")"
TONE="${TONE:-tone}"
BLURB='They say the Thorn of Camorr can beat anyone in a fight – a man, a myth… and wrong on every count.'
ffmpeg -v error -y -f lavfi -i anullsrc=r=22050:cl=mono -t 3 -c:a aac -b:a 16k -map_metadata -1 tagged.m4b
"$TONE" tag tagged.m4b --assume-yes \
  "--meta-title=The Lies of Locke Lamora" \
  "--meta-album=The Lies of Locke Lamora" \
  "--meta-itunes-media-type=Audiobook" \
  "--meta-artist=Scott Lynch" "--meta-album-artist=Scott Lynch" \
  "--meta-narrator=Michael Page" "--meta-composer=Michael Page" \
  "--meta-subtitle=The deviously twisty fantasy adventure you will not want to put down" \
  "--meta-description=$BLURB" "--meta-long-description=$BLURB" \
  "--meta-genre=Science Fiction & Fantasy" \
  "--meta-recording-date=2011-01-21" \
  "--meta-publisher=Gollancz" \
  "--meta-movement-name=Gentleman Bastard Sequence" "--meta-part=1" \
  "--meta-additional-field=----:com.pilabor.tone:AUDIBLE_ASIN=B004K50434" >/dev/null
ffprobe -v error -show_entries format=duration:format_tags -of json tagged.m4b > tagged.ffprobe.json

# manual.m4b: what audioborker's manual (last-resort) flow writes for an
# edition no catalog has — no ASIN, the AUDIOBORKER_SOURCE/ID marker the Plex
# agent matches on, every genre in GENRES, and an embedded cover.
ffmpeg -v error -y -f lavfi -i color=c=0x2a6f97:s=32x32 -frames:v 1 cover.jpg
ffmpeg -v error -y -f lavfi -i anullsrc=r=22050:cl=mono -t 3 -c:a aac -b:a 16k -map_metadata -1 manual.m4b
"$TONE" tag manual.m4b --assume-yes \
  "--meta-title=Mockingjay" \
  "--meta-album=Mockingjay" \
  "--meta-itunes-media-type=Audiobook" \
  "--meta-artist=Suzanne Collins" "--meta-album-artist=Suzanne Collins" \
  "--meta-narrator=Carolyn McCormick" "--meta-composer=Carolyn McCormick" \
  "--meta-description=The final book of the Hunger Games trilogy." \
  "--meta-long-description=The final book of the Hunger Games trilogy." \
  "--meta-genre=Teen & Young Adult" \
  "--meta-recording-date=2010-08-24" \
  "--meta-publisher=Scholastic Audio" \
  "--meta-movement-name=The Hunger Games" "--meta-part=3" \
  "--meta-additional-field=----:com.pilabor.tone:GENRES=Teen & Young Adult; Science Fiction & Fantasy; Dystopian" \
  "--meta-additional-field=----:com.pilabor.tone:AUDIOBORKER_SOURCE=manual" \
  "--meta-additional-field=----:com.pilabor.tone:AUDIOBORKER_ID=6f0c1a52-1b8e-4f0e-9a51-3c6c1f7d2e10" \
  "--meta-cover-file=cover.jpg" >/dev/null
rm -f cover.jpg
