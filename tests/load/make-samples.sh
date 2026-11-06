#!/bin/sh
# Generates the media the load generator sends: moving test video and a tone, about 1 Mbit/s in total.
set -e
cd "$(dirname "$0")"
ffmpeg -y -loglevel error -f lavfi -i testsrc=size=640x360:rate=30 -t 10 -c:v libvpx -b:v 800k -g 60 -deadline realtime -cpu-used 8 sample.ivf
ffmpeg -y -loglevel error -f lavfi -i sine=frequency=440:sample_rate=48000 -t 10 -c:a libopus -b:a 32k -page_duration 20000 sample.ogg
ls -la sample.ivf sample.ogg
