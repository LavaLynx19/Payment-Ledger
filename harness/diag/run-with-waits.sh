#!/usr/bin/env bash
# Runs harness/run.sh with the wait-event sampler alongside, then prints the
# top waits seen under load (samples with >= 5 active backends).
#
#   HOT_DEST_SHARE=0.8 TOPUP_SHARE=0 WALLETS=100 harness/diag/run-with-waits.sh 3
set -uo pipefail

NAME="${1:?usage: run-with-waits.sh <rung|scenario>}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
OUT="$ROOT/harness/out"
SAMPLES="$OUT/waits-${NAME}.txt"
mkdir -p "$OUT"

"$ROOT/harness/diag/waits.sh" > "$SAMPLES" &
sampler=$!
trap 'kill "$sampler" 2>/dev/null' EXIT

"$ROOT/harness/run.sh" "$NAME"
status=$?
kill "$sampler" 2>/dev/null

printf '\n== top waits under load (%s backend-samples)\n' "$(awk -F'|' '$1 >= 5' "$SAMPLES" | wc -l | tr -d ' ')"
awk -F'|' '$1 >= 5' "$SAMPLES" | cut -d'|' -f2,3 | sort | uniq -c | sort -rn | head -15
exit "$status"
