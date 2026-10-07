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
