#!/bin/sh
# Encodes the test pattern `streamkit room join --publish-test-pattern` publishes: a moving picture and a tone, about
# 0.5 Mbit/s in total. The results are committed, so building the CLI needs no ffmpeg.
set -e
cd "$(dirname "$0")"
ffmpeg -y -loglevel error -f lavfi -i testsrc=size=640x360:rate=30 -t 4 -c:v libvpx -b:v 450k -g 60 -deadline realtime -cpu-used 8 -f ivf pattern.ivf
ffmpeg -y -loglevel error -f lavfi -i sine=frequency=440:sample_rate=48000 -t 4 -c:a libopus -b:a 32k -page_duration 20000 -f ogg tone.ogg
ls -la pattern.ivf tone.ogg
