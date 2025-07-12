#!/bin/bash

set -e

# =============== CONFIG =================
STACK_DIR="infra-monitor"
ENV_FILE=".env"

# Colors
GRN="\033[0;32m"
RED="\033[0;31m"
YEL="\033[1;33m"
NC="\033[0m"

# ============= PREREQS ==================
echo -e "${YEL}Checking Docker and Docker Compose...${NC}"
if ! command -v docker >/dev/null 2>&1; then
  echo -e "${RED}Docker is not installed. Aborting.${NC}"
  exit 1
fi
if ! command -v docker compose >/dev/null 2>&1 && ! command -v docker-compose >/dev/null 2>&1; then
  echo -e "${RED}Docker Compose is not installed. Aborting.${NC}"
  exit 1
fi

# ============= SETUP ====================
echo -e "${GRN}Setting up Infra Monitor stack...${NC}"
mkdir -p $STACK_DIR/config $STACK_DIR/alerts $STACK_DIR/provisioning/{dashboards,datasources}
cd $STACK_DIR

# 1. .env file
echo -e "${YEL}Creating .env...${NC}"
cat > $ENV_FILE <<EOF
TELEGRAM_API_KEY=your_bot_api_key
TELEGRAM_CHAT_ID=your_chat_id
EOF

# 2. docker-compose.yml
cat > docker-compose.yml <<EOF
version: '3.8'
services:
  prometheus:
    image: prom/prometheus:latest
    volumes:
      - ./config/prometheus.yml:/etc/prometheus/prometheus.yml
      - ./alerts/rules.yml:/etc/prometheus/rules.yml
    command:
      - '--config.file=/etc/prometheus/prometheus.yml'
      - '--web.enable-lifecycle'
    ports:
      - 9090:9090

  alertmanager:
    image: prom/alertmanager:latest
    volumes:
      - ./config/alertmanager.yml:/etc/alertmanager/alertmanager.yml
    ports:
      - 9093:9093
    environment:
      - TELEGRAM_API_KEY=\${TELEGRAM_API_KEY}
      - TELEGRAM_CHAT_ID=\${TELEGRAM_CHAT_ID}

  grafana:
    image: grafana/grafana:latest
    ports:
      - 3000:3000
    environment:
      - GF_SECURITY_ADMIN_PASSWORD=admin
      - GF_PATHS_PROVISIONING=/etc/grafana/provisioning
    volumes:
      - grafana-storage:/var/lib/grafana
      - ./provisioning/datasources:/etc/grafana/provisioning/datasources
      - ./provisioning/dashboards:/etc/grafana/provisioning/dashboards

  node-exporter:
    image: prom/node-exporter:latest
    ports:
      - 9100:9100

  blackbox:
    image: prom/blackbox-exporter:latest
    ports:
      - 9115:9115

volumes:
  grafana-storage:
EOF

# 3. Prometheus config
mkdir -p config
cat > config/prometheus.yml <<EOF
global:
  scrape_interval: 15s

rule_files:
  - /etc/prometheus/rules.yml

scrape_configs:
  - job_name: 'node'
    static_configs:
      - targets: ['node-exporter:9100']

  - job_name: 'blackbox'
    metrics_path: /probe
    params:
      module: [http_2xx]
    static_configs:
      - targets:
        - https://example.com
        - http://1.1.1.1
    relabel_configs:
      - source_labels: [__address__]
        target_label: __param_target
      - source_labels: [__param_target]
        target_label: instance
      - target_label: __address__
        replacement: blackbox:9115

  - job_name: 'alertmanager'
    static_configs:
      - targets: ['alertmanager:9093']
EOF

# 4. Alertmanager config
cat > config/alertmanager.yml <<EOF
global:
  resolve_timeout: 5m

route:
  group_by: ['alertname']
  receiver: 'telegram'

receivers:
  - name: 'telegram'
    telegram_configs:
      - bot_token: "\${TELEGRAM_API_KEY}"
        chat_id: "\${TELEGRAM_CHAT_ID}"
        message: "{{ .CommonAnnotations.summary }}"
EOF

# 5. Alerts rule
cat > alerts/rules.yml <<EOF
groups:
  - name: uptime-alerts
    rules:
      - alert: InstanceDown
        expr: up == 0
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "Instance {{ \$labels.instance }} is DOWN"
EOF

# 6. Grafana provisioning
cat > provisioning/datasources/prometheus.yml <<EOF
apiVersion: 1

datasources:
  - name: Prometheus
    type: prometheus
    access: proxy
    url: http://prometheus:9090
    isDefault: true
EOF

cat > provisioning/dashboards/dashboard.yml <<EOF
apiVersion: 1

providers:
  - name: 'default'
    folder: 'Infra Monitor'
    type: file
    updateIntervalSeconds: 30
    options:
      path: /etc/grafana/provisioning/dashboards
EOF

# 7. Download Node Exporter Full Dashboard JSON
echo -e "${YEL}Downloading Node Exporter Full dashboard...${NC}"
curl -s https://grafana.com/api/dashboards/1860/revisions/34/download \
  -o provisioning/dashboards/node-exporter-full.json

# 8. Launch stack
echo -e "${YEL}Launching services with Docker Compose...${NC}"
docker compose up -d || docker-compose up -d

echo -e "${GRN}Infra Monitor is live!${NC}"
echo "Prometheus: http://localhost:9090"
echo "Grafana: http://localhost:3000 (admin/admin)"
echo "Alertmanager: http://localhost:9093"
echo -e "${GRN}Setup complete!${NC}"
