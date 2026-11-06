#!/bin/sh
# Runs the load ladder against a server and appends one JSON line per step to results-<label>.jsonl. Results only
# mean something for the machine they were measured on, and the generator's CPU competes with the server's if they
# share one. Usage: tests/load/ladder.sh [label] [server ws url] [seconds per step]
LABEL=${1:-local}
URL=${2:-ws://127.0.0.1:7880}
SECONDS_PER_STEP=${3:-30}
OUT="tests/load/results-$LABEL.jsonl"
go build -o bin/loadgen ./tests/load/cmd/loadgen || exit 1
for STEP in "2 0" "2 8" "5 20" "5 45" "5 95" "5 195" "5 495" "10 0" "25 0"; do
  set -- $STEP
  echo "--- $1 publishers, $2 subscribers" >&2
  bin/loadgen -url "$URL" -publishers "$1" -subscribers "$2" -duration "${SECONDS_PER_STEP}s" | tee -a "$OUT"
  # Let the server finish closing the last room.
  sleep 5
done
