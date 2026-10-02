#!/usr/bin/env bash
# Cuts the app off from one shard every EVERY seconds for FOR seconds, by
# disabling its toxiproxy proxy (connections drop, new ones are refused),
# until this script is killed. run.sh re-enables every proxy afterwards.
#
#   harness/faults/partition.sh <shardN> <every-seconds> <for-seconds>
set -euo pipefail

PROXY="${1:?usage: partition.sh <shardN> <every-seconds> <for-seconds>}"
EVERY="${2:?usage: partition.sh <shardN> <every-seconds> <for-seconds>}"
FOR="${3:?usage: partition.sh <shardN> <every-seconds> <for-seconds>}"
API=http://127.0.0.1:8474/proxies/$PROXY

while true; do
  sleep "$EVERY"
  echo "fault: partition $PROXY for ${FOR}s"
  curl -s -o /dev/null -X POST "$API" -d '{"enabled":false}' || true
  sleep "$FOR"
  curl -s -o /dev/null -X POST "$API" -d '{"enabled":true}' || true
done
