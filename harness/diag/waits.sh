#!/usr/bin/env bash
# Samples what active Postgres backends are waiting on, every ~0.2s until
# killed. Each line is "<active backends>|<wait type:event>|<statement>",
# with statements normalized to their first words so they group. Pair it with
# a harness run, then summarize with:
#   awk -F'|' '$1 >= 5' out.txt | cut -d'|' -f2,3 | sort | uniq -c | sort -rn
#
#   harness/diag/waits.sh > harness/out/waits.txt &
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
COMPOSE=(docker compose -f "$ROOT/deploy/docker-compose.yml")

while true; do
  "${COMPOSE[@]}" exec -T postgres psql -U ledger -d ledger -tA -F'|' -c "
    WITH a AS (
      SELECT coalesce(wait_event_type || ':' || wait_event, 'CPU') AS wait,
             regexp_replace(left(regexp_replace(query, '\s+', ' ', 'g'), 48), '\\\$[0-9]+', '?', 'g') AS stmt
      FROM pg_stat_activity
      WHERE datname = 'ledger' AND state = 'active' AND pid <> pg_backend_pid())
    SELECT (SELECT count(*) FROM a), wait, stmt FROM a" 2>/dev/null
  sleep 0.2
done
