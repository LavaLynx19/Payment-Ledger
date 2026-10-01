#!/usr/bin/env bash
# One command per rung or scenario: fresh stack → migrate → seed → api + worker
# → k6 load (with optional faults) → wait for captures to drain → report →
# acknowledged-Transfer check → checker. Exits non-zero if k6 thresholds, the
# ACK check, or the checker fail.
#
#   harness/run.sh 1                                 # harness/k6/rung1.js
#   harness/run.sh baseline                          # harness/k6/baseline.js
#   RATE=2000 WALLETS=100 harness/run.sh baseline
#   FAILPOINTS=capture.after_entries=0.0002 harness/run.sh 2
#   KILL="api:6 worker:9 postgres:20" AGE_KEYS=1 harness/run.sh 2
#
# Env passed to k6 when set: VUS DURATION FUNDING RATE SENDERS MAX_AMOUNT MAX_VUS
# Faults: FAILPOINTS (passed to api/worker via Compose), KILL ("svc:every ..."),
#         AGE_KEYS=1 (backdate settled keys so purge runs)
set -euo pipefail

NAME="${1:?usage: harness/run.sh <rung|scenario>}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/harness/out"
COMPOSE=(docker compose -f "$ROOT/deploy/docker-compose.yml")
SCRIPT="$NAME.js"
[[ -f "$ROOT/harness/k6/$SCRIPT" ]] || SCRIPT="rung${NAME}.js"
[[ -f "$ROOT/harness/k6/$SCRIPT" ]] || { echo "no scenario harness/k6/$NAME.js or rung$NAME.js" >&2; exit 2; }
mkdir -p "$OUT"
export FAILPOINTS="${FAILPOINTS:-}"

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

step "api + worker (failpoints: ${FAILPOINTS:-none})"
"${COMPOSE[@]}" up -d api worker
for _ in $(seq 1 50); do
  curl -s -o /dev/null http://127.0.0.1:18080/ && break
  sleep 0.2
done

faults=()
start_faults() {
  for spec in ${KILL:-}; do
    "$ROOT/harness/faults/kill.sh" "${spec%%:*}" "${spec##*:}" &
    faults+=($!)
  done
  if [[ -n "${AGE_KEYS:-}" ]]; then
    "$ROOT/harness/faults/age-keys.sh" 3 &
    faults+=($!)
  fi
}
stop_faults() {
  for pid in ${faults[@]+"${faults[@]}"}; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
  "${COMPOSE[@]}" up -d --wait postgres api worker >/dev/null 2>&1
}
trap stop_faults EXIT

step "k6 $SCRIPT ${k6_env[*]:-} ${KILL:+KILL=\"$KILL\"} ${AGE_KEYS:+AGE_KEYS=1}"
start_faults
set +e
"${COMPOSE[@]}" run --rm ${k6_env[@]+"${k6_env[@]}"} \
  k6 run --summary-export "/out/${NAME}-summary.json" "/scripts/$SCRIPT" 2>&1 | tee "$OUT/${NAME}-k6.log"
k6_status=${PIPESTATUS[0]}
set -e
stop_faults
trap - EXIT
if [[ -n "$FAILPOINTS" ]]; then
  echo "failpoint crashes: api $(docker inspect -f '{{.RestartCount}}' payment-ledger-api-1)," \
       "worker $(docker inspect -f '{{.RestartCount}}' payment-ledger-worker-1)"
fi

step "drain pending captures"
start=$(date +%s)
active=$(psql -c "SELECT count(*) FROM holds WHERE status = 'active' AND capture_mode = 'auto'")
echo "auto holds active when load stopped: $active"
deadline=$((start + ${DRAIN_TIMEOUT:-120}))
while [[ "$active" != "0" && $(date +%s) -lt $deadline ]]; do
  sleep 0.5
  active=$(psql -c "SELECT count(*) FROM holds WHERE status = 'active' AND capture_mode = 'auto'")
done
echo "drain took $(( $(date +%s) - start ))s; auto holds remaining: $active"

step "transfers (p2p)"
psql -F ' ' -c "SELECT status, count(*) FROM transfers WHERE type = 'p2p' GROUP BY status ORDER BY status"
psql -c "SELECT 'posted/s end to end: ' || coalesce(round(count(*) / nullif(extract(epoch FROM max(posted_at) - min(created_at)), 0)::numeric, 1)::text, 'n/a')
         FROM transfers WHERE type = 'p2p' AND status = 'posted' AND reverses_id IS NULL"

ack_status=0
acks="$OUT/${NAME}-acks.txt"
grep -o 'ACK [^ "]* [0-9a-f-]\{36\}' "$OUT/${NAME}-k6.log" | cut -d' ' -f2,3 > "$acks" || true
if [[ -s "$acks" ]]; then
  step "acknowledged transfers ($(wc -l < "$acks" | tr -d ' '))"
  result=$({ echo "CREATE TEMP TABLE acks (key text, id uuid); COPY acks FROM STDIN WITH (DELIMITER ' ');"
             cat "$acks"; echo '\.'
             echo "SELECT count(*) FILTER (WHERE t.id IS NULL) || ' ' ||
                          count(*) FILTER (WHERE t.status IS DISTINCT FROM 'posted') || ' ' ||
                          count(*) FILTER (WHERE k.key IS NOT NULL AND k.transfer_id <> a.id)
                   FROM acks a
                   LEFT JOIN transfers t ON t.id = a.id
                   LEFT JOIN idempotency_keys k ON k.key = a.key;"
           } | "${COMPOSE[@]}" exec -T postgres psql -U ledger -d ledger -tAq)
  read -r lost unposted diverged <<< "$result"
  echo "lost: $lost   not posted: $unposted   key → different transfer: $diverged"
  [[ "$lost" == "0" && "$unposted" == "0" && "$diverged" == "0" ]] || ack_status=1
fi

# Postgres stays up for inspection. The app stops, so its worker can't
# capture Holds that integration tests create in the same database.
"${COMPOSE[@]}" stop api worker >/dev/null 2>&1

step "checker"
set +e
"${COMPOSE[@]}" run --rm -T checker | tee "$OUT/${NAME}-checker.txt"
checker_status=${PIPESTATUS[0]}
set -e

step "result"
echo "k6:       exit $k6_status (thresholds)   harness/out/${NAME}-summary.json"
echo "acks:     exit $ack_status"
echo "checker:  exit $checker_status   harness/out/${NAME}-checker.txt"
[[ $k6_status -eq 0 && $ack_status -eq 0 && $checker_status -eq 0 ]]
