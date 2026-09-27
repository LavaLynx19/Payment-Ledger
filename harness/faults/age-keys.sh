#!/usr/bin/env bash
# Backdates idempotency keys so the worker purges them during a run, which
# exercises purge.mid_batch without a 24h wait. Only keys whose Transfer has
# settled and that are older than the client's 10s retry window are aged, so
# no in-flight retry can lose its key.
#
#   harness/faults/age-keys.sh <every-seconds>
set -euo pipefail

EVERY="${1:?usage: age-keys.sh <every-seconds>}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
COMPOSE=(docker compose -f "$ROOT/deploy/docker-compose.yml")

while true; do
  sleep "$EVERY"
  "${COMPOSE[@]}" exec -T postgres psql -U ledger -d ledger -tAq -c "
    UPDATE idempotency_keys SET created_at = created_at - interval '25 hours'
    WHERE key IN (
      SELECT k.key FROM idempotency_keys k JOIN transfers t ON t.id = k.transfer_id
      WHERE t.status <> 'pending' AND k.created_at BETWEEN clock_timestamp() - interval '1 hour'
                                                      AND clock_timestamp() - interval '15 seconds'
      LIMIT 500)" >/dev/null 2>&1 || true
done
