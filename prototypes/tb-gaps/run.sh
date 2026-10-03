#!/usr/bin/env bash
# PROTOTYPE, wipe me: runs the TigerBeetle gap prototypes against a throwaway
# single-replica TigerBeetle in Docker, then removes the container and data.
#
#   prototypes/tb-gaps/run.sh
set -euo pipefail
cd "$(dirname "$0")"

IMG=ghcr.io/tigerbeetle/tigerbeetle:0.17.9 # must match the Go client version
NAME=tb-prototype-wipe-me
DATA=tb-prototype-wipe-me # Docker named volume: TigerBeetle preallocates its data file, which a macOS bind mount refuses
cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; docker volume rm -f "$DATA" tb-proto-gomod >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

run_tb() {
  docker run --rm --security-opt seccomp=unconfined --cap-add IPC_LOCK -v "$DATA:/data" "$IMG" "$@"
}
run_tb format --cluster=0 --replica=0 --replica-count=1 --development /data/0_0.tigerbeetle
docker run -d --name "$NAME" --security-opt seccomp=unconfined --cap-add IPC_LOCK \
  -p 3033:3000 -v "$DATA:/data" "$IMG" \
  start --addresses=0.0.0.0:3000 --development /data/0_0.tigerbeetle >/dev/null
sleep 2

# The Go client's macOS native library fails to link with Apple's ld ("not
# 8-byte aligned"), so build and run in a Linux container that shares the
# TigerBeetle container's network. The client needs io_uring too, which
# Docker's default seccomp profile blocks.
docker run --rm --network "container:$NAME" -e TB_ADDRESS=3000 --security-opt seccomp=unconfined \
  -v "$PWD:/src" -v tb-proto-gomod:/go/pkg/mod -w /src golang:1.27 go run .
