#!/usr/bin/env bash
# SIGKILLs a Compose service every EVERY seconds until this script is killed,
# bringing it back each time. Docker skips the restart policy after a manual
# kill, so the restart is explicit here.
#
#   harness/faults/kill.sh <service> <every-seconds>
set -euo pipefail

SVC="${1:?usage: kill.sh <service> <every-seconds>}"
EVERY="${2:?usage: kill.sh <service> <every-seconds>}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
# run.sh exports COMPOSE_FILE/COMPOSE_PROFILES so this drives the same stack.
COMPOSE=(docker compose)
[[ -n "${COMPOSE_FILE:-}" ]] || COMPOSE+=(-f "$ROOT/deploy/docker-compose.yml")

while true; do
  sleep "$EVERY"
  echo "fault: kill -9 $SVC"
  "${COMPOSE[@]}" kill -s SIGKILL "$SVC" >/dev/null 2>&1 || true
  "${COMPOSE[@]}" up -d --wait "$SVC" >/dev/null 2>&1 || true
done
