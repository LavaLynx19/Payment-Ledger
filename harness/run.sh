#!/usr/bin/env bash
# One command per rung or scenario: fresh stack → migrate → seed → api + worker
# → k6 load → wait for captures to drain → report → checker. Exits with the
# checker's status.
#
#   harness/run.sh 1                                 # harness/k6/rung1.js
#   harness/run.sh baseline                          # harness/k6/baseline.js
#   RATE=2000 WALLETS=100 harness/run.sh baseline
#
# Env passed to k6 when set: VUS DURATION FUNDING RATE SENDERS MAX_AMOUNT MAX_VUS
set -euo pipefail

NAME="${1:?usage: harness/run.sh <rung|scenario>}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/harness/out"
COMPOSE=(docker compose -f "$ROOT/deploy/docker-compose.yml")
SCRIPT="$NAME.js"
[[ -f "$ROOT/harness/k6/$SCRIPT" ]] || SCRIPT="rung${NAME}.js"
[[ -f "$ROOT/harness/k6/$SCRIPT" ]] || { echo "no scenario harness/k6/$NAME.js or rung$NAME.js" >&2; exit 2; }
mkdir -p "$OUT"

step() { printf '\n== %s\n' "$*"; }
psql() { "${COMPOSE[@]}" exec -T postgres psql -U ledger -d ledger -tA "$@"; }

k6_env=()
for v in VUS DURATION FUNDING RATE SENDERS MAX_AMOUNT MAX_VUS; do
  [[ -n "${!v:-}" ]] && k6_env+=(-e "$v=${!v}")
done

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

step "k6 $SCRIPT ${k6_env[*]:-}"
"${COMPOSE[@]}" run --rm ${k6_env[@]+"${k6_env[@]}"} \
  k6 run --summary-export "/out/${NAME}-summary.json" "/scripts/$SCRIPT"

step "drain pending captures"
start=$(date +%s)
active=$(psql -c "SELECT count(*) FROM holds WHERE status = 'active'")
echo "active holds when load stopped: $active"
deadline=$((start + ${DRAIN_TIMEOUT:-120}))
while [[ "$active" != "0" && $(date +%s) -lt $deadline ]]; do
  sleep 0.5
  active=$(psql -c "SELECT count(*) FROM holds WHERE status = 'active'")
done
echo "drain took $(( $(date +%s) - start ))s; active holds remaining: $active"

step "transfers (p2p)"
psql -F ' ' -c "SELECT status, count(*) FROM transfers WHERE type = 'p2p' GROUP BY status ORDER BY status"
psql -c "SELECT 'posted/s end to end: ' || coalesce(round(count(*) / nullif(extract(epoch FROM max(posted_at) - min(created_at)), 0)::numeric, 1)::text, 'n/a')
         FROM transfers WHERE type = 'p2p' AND status = 'posted'"

# Postgres stays up for inspection. The app stops, so its worker can't
# capture Holds that integration tests create in the same database.
"${COMPOSE[@]}" stop api worker >/dev/null 2>&1

step "checker"
set +e
"${COMPOSE[@]}" run --rm -T checker | tee "$OUT/${NAME}-checker.txt"
status=${PIPESTATUS[0]}
set -e

step "result"
echo "k6 summary:   harness/out/${NAME}-summary.json"
echo "checker:      harness/out/${NAME}-checker.txt (exit $status)"
exit "$status"
