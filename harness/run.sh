#!/usr/bin/env bash
# One command per rung: fresh stack → migrate → seed → api + worker → k6 load
# → wait for captures to drain → checker. Exits with the checker's status.
#
#   harness/run.sh 1                       # defaults
#   VUS=100 DURATION=60s WALLETS=10 harness/run.sh 1
set -euo pipefail

RUNG="${1:?usage: harness/run.sh <rung>}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/harness/out"
COMPOSE=(docker compose -f "$ROOT/deploy/docker-compose.yml")
SCRIPT="rung${RUNG}.js"
[[ -f "$ROOT/harness/k6/$SCRIPT" ]] || { echo "no scenario harness/k6/$SCRIPT" >&2; exit 2; }
mkdir -p "$OUT"

step() { printf '\n== %s\n' "$*"; }

step "fresh stack"
"${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1
"${COMPOSE[@]}" build --quiet
"${COMPOSE[@]}" up -d --wait postgres

step "migrate + seed ${WALLETS:-10} wallets"
"${COMPOSE[@]}" run --rm migrate
"${COMPOSE[@]}" run --rm -T seed -wallets "${WALLETS:-10}" > "$OUT/seed.json"

step "api + worker"
"${COMPOSE[@]}" up -d api worker
for _ in $(seq 1 50); do
  curl -s -o /dev/null http://127.0.0.1:18080/ && break
  sleep 0.2
done

step "k6 $SCRIPT (VUS=${VUS:-50} DURATION=${DURATION:-30s})"
"${COMPOSE[@]}" run --rm \
  -e VUS="${VUS:-50}" -e DURATION="${DURATION:-30s}" -e FUNDING="${FUNDING:-1000}" \
  k6 run --summary-export "/out/rung${RUNG}-summary.json" "/scripts/$SCRIPT"

step "drain pending captures"
for _ in $(seq 1 300); do
  active=$("${COMPOSE[@]}" exec -T postgres psql -U ledger -d ledger -tAc \
    "SELECT count(*) FROM holds WHERE status = 'active'")
  [[ "$active" == "0" ]] && break
  sleep 0.2
done
echo "active holds remaining: $active"

step "checker"
set +e
"${COMPOSE[@]}" run --rm -T checker | tee "$OUT/rung${RUNG}-checker.txt"
status=${PIPESTATUS[0]}
set -e

# Postgres stays up for inspection. The app stops, so its worker can't
# capture Holds that integration tests create in the same database.
"${COMPOSE[@]}" stop api worker >/dev/null 2>&1

step "result"
echo "k6 summary:   harness/out/rung${RUNG}-summary.json"
echo "checker:      harness/out/rung${RUNG}-checker.txt (exit $status)"
exit "$status"
