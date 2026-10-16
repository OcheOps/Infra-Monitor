#!/usr/bin/env bash
# Usage: wait-for-targets.sh "<command printing /api/v1/targets JSON>" JOB...
# Waits until every target of each named job reports health "up".
set -uo pipefail
fetch=$1; shift
for i in $(seq 40); do
  json=$(eval "$fetch" 2>/dev/null) || json='{}'
  ok=1
  for job in "$@"; do
    states=$(echo "$json" | jq -r --arg j "$job" '[.data.activeTargets[]? | select(.labels.job == $j) | .health] | if length == 0 then "missing" else unique | join(",") end')
    [ "$states" = "up" ] || ok=0
    [ "$i" -eq 40 ] && echo "$job: $states"
  done
  [ "$ok" -eq 1 ] && { echo "All targets up: $*"; exit 0; }
  sleep 5
done
echo "$json" | jq '.data.activeTargets[]? | {job: .labels.job, instance: .labels.instance, health, lastError}'
exit 1
