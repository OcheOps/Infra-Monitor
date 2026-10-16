#!/usr/bin/env bash
# Usage: wait-for-query.sh "<fetch command>" '<PromQL>'
# Runs the fetch command with a Prometheus /api/v1/query URL appended until
# the query returns at least one series and every value is non-zero.
set -uo pipefail
fetch=$1 query=$2
url="http://localhost:9090/api/v1/query?query=$(jq -rn --arg q "$query" '$q|@uri')"
for i in $(seq ${WAIT_TRIES:-40}); do
  json=$(eval "$fetch \"$url\"" 2>/dev/null) || json='{}'
  if echo "$json" | jq -e '(.data.result | length) > 0 and all(.data.result[]; .value[1] != "0")' >/dev/null 2>&1; then
    echo "OK: $query"
    exit 0
  fi
  sleep 5
done
echo "Query never returned non-zero results: $query"
echo "$json" | jq . 2>/dev/null || echo "$json"
exit 1
