#!/usr/bin/env bash
set -euo pipefail

GRN="\033[0;32m"; RED="\033[0;31m"; YEL="\033[1;33m"; NC="\033[0m"

require() {
  command -v "$1" >/dev/null 2>&1 || { echo -e "${RED}$1 not found. Aborting.${NC}"; exit 1; }
}

require docker
require envsubst
require make
docker compose version >/dev/null 2>&1 || { echo -e "${RED}docker compose plugin not found.${NC}"; exit 1; }

cd "$(dirname "$0")/infra-monitor"

if [ ! -f .env ]; then
  cp .env.example .env
  echo -e "${YEL}.env created from .env.example — edit it and re-run.${NC}"
  exit 0
fi

echo -e "${YEL}Starting stack…${NC}"
make up

cat <<EOF
${GRN}Infra Monitor is live${NC}
  Grafana:      http://localhost:3000  (admin/admin)
  Prometheus:   http://localhost:9090
  Alertmanager: http://localhost:9093
  Jaeger:       http://localhost:16686
  Loki:         http://localhost:3100
  cAdvisor:     http://localhost:8080
EOF
