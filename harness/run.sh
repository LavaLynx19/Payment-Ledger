#!/usr/bin/env bash
# One command per rung or scenario: fresh stack → migrate → seed → api + worker
# → k6 load (with optional faults) → wait for captures and relays to drain →
# report → checker, which also verifies every acknowledged Transfer. Exits
# non-zero if the k6 thresholds or the checker fail.
#
#   harness/run.sh 1                                 # harness/k6/rung1.js
#   harness/run.sh baseline                          # harness/k6/baseline.js
#   RATE=2000 WALLETS=100 harness/run.sh baseline
#   FAILPOINTS=capture.after_entries=0.0002 harness/run.sh 2
#   KILL="api:6 worker:9 postgres:20" AGE_KEYS=1 harness/run.sh 2
#   SHARDS=2 CROSS_SHARD=saga harness/run.sh baseline
#   SHARDS=2 TOXI=1 PARTITION="shard1:12:3" harness/run.sh 2
#   ENGINE=tigerbeetle harness/run.sh baseline
#
# Env passed to k6 when set: VUS DURATION FUNDING RATE SENDERS MAX_AMOUNT MAX_VUS HOT_DEST_SHARE TOPUP_SHARE
# Faults: FAILPOINTS (passed to api/worker via Compose), KILL ("svc:every ..."),
#         AGE_KEYS=1 (backdate settled keys so purge runs),
#         TOXI=1 (api/worker reach the shards through toxiproxy),
#         PARTITION ("shardN:every:for ...", needs TOXI=1)
# Engine: ENGINE=postgres (default) | tigerbeetle (A§9.7)
# Shards: SHARDS=2 runs on two Postgres shards (Rung 4); default 1.
#         CROSS_SHARD=2pc (default) | saga is passed to api/worker via Compose
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

ENGINE="${ENGINE:-postgres}"
case "$ENGINE" in
  postgres) ;;
  tigerbeetle) COMPOSE+=(-f "$ROOT/deploy/docker-compose.tigerbeetle.yml") ;;
  *) echo "ENGINE must be postgres or tigerbeetle" >&2; exit 2 ;;
esac

pw="${POSTGRES_PASSWORD:-ledger-dev}"
url() { echo "postgres://ledger:$pw@$1/ledger?sslmode=disable"; }
DBS=(postgres)
DIRECT=("$(url postgres:5432)")
PROXIED=("$(url toxiproxy:25432)")
case "${SHARDS:-1}" in
  1) ;;
  2) COMPOSE+=(--profile shards); DBS+=(postgres-shard1)
     DIRECT+=("$(url postgres-shard1:5432)"); PROXIED+=("$(url toxiproxy:25433)") ;;
  *) echo "SHARDS must be 1 or 2" >&2; exit 2 ;;
esac
join() { local IFS=,; echo "$*"; }
# Compose reads SHARD_URLS for every service. api and worker get the proxied
# URLs under TOXI=1; the tools (migrate, seed, checker) always connect
# directly, so a partition never touches setup or the verdict.
direct_urls="$(join "${DIRECT[@]}")"
export SHARD_URLS="$direct_urls"
if [[ -n "${TOXI:-}" ]]; then
  COMPOSE+=(--profile faults)
  SHARD_URLS="$(join "${PROXIED[@]}")"
fi
[[ -z "${PARTITION:-}" || -n "${TOXI:-}" ]] || { echo "PARTITION needs TOXI=1" >&2; exit 2; }

step() { printf '\n== %s\n' "$*"; }
tool() { "${COMPOSE[@]}" run --rm -T -e SHARD_URLS="$direct_urls" "$@"; }

k6_env=()
for v in VUS DURATION FUNDING RATE SENDERS MAX_AMOUNT MAX_VUS HOT_DEST_SHARE TOPUP_SHARE; do
  [[ -n "${!v:-}" ]] && k6_env+=(-e "$v=${!v}")
done

step "fresh stack (engine $ENGINE, ${SHARDS:-1} shard(s)${TOXI:+, via toxiproxy})"
"${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1
"${COMPOSE[@]}" build --quiet
"${COMPOSE[@]}" up -d --wait "${DBS[@]}"
if [[ "$ENGINE" == "tigerbeetle" ]]; then
  "${COMPOSE[@]}" run --rm tigerbeetle-format >/dev/null 2>&1
  "${COMPOSE[@]}" up -d tigerbeetle >/dev/null 2>&1
fi
if [[ -n "${TOXI:-}" ]]; then
  "${COMPOSE[@]}" up -d toxiproxy >/dev/null 2>&1
  for _ in $(seq 1 50); do curl -s -o /dev/null http://127.0.0.1:8474/version && break; sleep 0.2; done
  for i in "${!DBS[@]}"; do
    curl -sf -o /dev/null -X POST http://127.0.0.1:8474/proxies \
      -d "{\"name\":\"shard$i\",\"listen\":\"0.0.0.0:$((25432 + i))\",\"upstream\":\"${DBS[$i]}:5432\"}"
  done
fi

step "migrate + seed ${WALLETS:-10} wallets"
if [[ "$ENGINE" == "postgres" ]]; then tool migrate; fi
tool seed -wallets "${WALLETS:-10}" > "$OUT/seed.json"

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
  for spec in ${PARTITION:-}; do
    IFS=: read -r proxy every for <<< "$spec"
    "$ROOT/harness/faults/partition.sh" "$proxy" "$every" "$for" &
    faults+=($!)
  done
}
stop_faults() {
  for pid in ${faults[@]+"${faults[@]}"}; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
  if [[ -n "${TOXI:-}" ]]; then
    for i in "${!DBS[@]}"; do
      curl -s -o /dev/null -X POST "http://127.0.0.1:8474/proxies/shard$i" -d '{"enabled":true}' || true
    done
  fi
  "${COMPOSE[@]}" up -d --wait "${DBS[@]}" api worker >/dev/null 2>&1
}
trap stop_faults EXIT

step "k6 $SCRIPT ${k6_env[*]:-} ${KILL:+KILL=\"$KILL\"} ${PARTITION:+PARTITION=\"$PARTITION\"} ${AGE_KEYS:+AGE_KEYS=1}"
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

if [[ "$ENGINE" == "postgres" ]]; then
  step "drain pending captures and saga relays"
  # Pending work across every shard: auto Holds not yet captured, saga
  # credits not yet relayed (A§9.5), and prepared 2PCs the resolver hasn't
  # finished yet (A§9.4). The worker must outlive them.
  pending() {
    local total=0 n
    for db in "${DBS[@]}"; do
      n=$("${COMPOSE[@]}" exec -T "$db" psql -U ledger -d ledger -tA -c \
        "SELECT (SELECT count(*) FROM holds WHERE status = 'active' AND capture_mode = 'auto') + (SELECT count(*) FROM outbox) + (SELECT count(*) FROM pg_prepared_xacts)")
      total=$((total + n))
    done
    echo "$total"
  }
  start=$(date +%s)
  active=$(pending)
  echo "pending when load stopped: $active"
  deadline=$((start + ${DRAIN_TIMEOUT:-120}))
  while [[ "$active" != "0" && $(date +%s) -lt $deadline ]]; do
    sleep 0.5
    active=$(pending)
  done
  echo "drain took $(( $(date +%s) - start ))s; pending remaining: $active"

  step "transfers (p2p, all shards)"
  for db in "${DBS[@]}"; do
    "${COMPOSE[@]}" exec -T "$db" psql -U ledger -d ledger -tA -F ' ' -c \
      "SELECT status, count(*), coalesce(extract(epoch FROM min(created_at)), 0), coalesce(extract(epoch FROM max(posted_at)), 0)
       FROM transfers WHERE type = 'p2p' AND reverses_id IS NULL GROUP BY status"
  done | awk '
    { n[$1] += $2 }
    $1 == "posted" { if (lo == "" || $3 < lo) lo = $3; if ($4 > hi) hi = $4 }
    END {
      for (s in n) print s, n[s]
      if (hi > lo) printf "posted/s end to end: %.1f\n", n["posted"] / (hi - lo); else print "posted/s end to end: n/a"
    }'
else
  step "transfers"
  echo "single-phase engine: posted when accepted, nothing to drain (k6 iterations/s is the posting rate)"
fi

# The databases stay up for inspection. The app stops, so its worker can't
# capture Holds that integration tests create in the same database. A
# graceful stop makes each process log its final CAS totals.
"${COMPOSE[@]}" stop api worker >/dev/null 2>&1

step "CAS counters (attempts / conflicts / exhausted per op, last process lifetime)"
for svc in api worker; do
  echo "$svc: $("${COMPOSE[@]}" logs --no-log-prefix "$svc" 2>/dev/null | grep -o 'cas: {.*}' | tail -1)"
done

acks="$OUT/${NAME}-acks.txt"
grep -o 'ACK [^ "]* [0-9a-f-]\{36\}' "$OUT/${NAME}-k6.log" | cut -d' ' -f2,3 > "$acks" || true
step "checker (+ $(wc -l < "$acks" | tr -d ' ') acknowledged transfers)"
set +e
tool checker --once --acks - < "$acks" | tee "$OUT/${NAME}-checker.txt"
checker_status=${PIPESTATUS[0]}
set -e

step "result"
echo "k6:       exit $k6_status (thresholds)   harness/out/${NAME}-summary.json"
echo "checker:  exit $checker_status   harness/out/${NAME}-checker.txt"
[[ $k6_status -eq 0 && $checker_status -eq 0 ]]
