#!/usr/bin/env bash
# Rung 2 fault matrix: one harness/run.sh 2 per A§5 failpoint, then a kill -9
# run against api, worker and postgres. Prints a verdict table; exits non-zero
# if any run fails.
#
# Probabilities aim at about one crash per ~12s of process uptime. Docker's
# restart policy doubles its backoff on each crash and only resets after 10s
# up, so crashing faster pushes restarts past the clients' 10s retry window.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
OUT="$ROOT/harness/out"
mkdir -p "$OUT"
export WALLETS="${WALLETS:-100}" DURATION="${DURATION:-40s}" AGE_KEYS=1

# Hits per second at RATE=2000 (80% P2P, 10% Holds, 10% reversals).
prob() {
  case "$1" in
    accept.*) echo 0.00005 ;;
    capture.*) echo 0.00015 ;; # batched capture: ~1/3 the txs (Rung 3)
    release.*) echo 0.001 ;;
    reversal.*) echo 0.0004 ;;
    sweeper.*) echo 0.08 ;;
    purge.*) echo 0.3 ;; # fewer hits: only aged keys, every ~3s
  esac
}

# The A§5 failpoints (internal/failpoint.Names).
POINTS="accept.before_commit accept.after_commit capture.after_claim capture.after_entries
capture.after_commit release.before_commit sweeper.mid_batch purge.mid_batch
reversal.before_commit reversal.after_commit"

failed=0
verdict() { # name log status
  local crashes line
  crashes=$(grep -o 'failpoint crashes: .*' "$2" || grep -c '^fault: kill' "$2" | sed 's/^/kills: /')
  line=$(grep -o 'lost: .*' "$2" || echo "no ACK check")
  printf '%-24s %-4s  %-34s  %s\n' "$1" "$([[ $3 == 0 ]] && echo PASS || echo FAIL)" "$crashes" "$line"
  [[ $3 == 0 ]] || failed=1
}

printf '%-24s %-4s  %-34s  %s\n' "run" "" "crashes" "acknowledged transfers"
for fp in $POINTS; do
  log="$OUT/rung2-$fp.log"
  FAILPOINTS="$fp=$(prob "$fp")" "$ROOT/harness/run.sh" 2 >"$log" 2>&1
  verdict "$fp" "$log" $?
done

log="$OUT/rung2-kill.log"
KILL="${KILL:-api:12 worker:15 postgres:25}" "$ROOT/harness/run.sh" 2 >"$log" 2>&1
verdict "kill -9 (${KILL:-api:12 worker:15 postgres:25})" "$log" $?

exit "$failed"
