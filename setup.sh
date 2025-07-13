#!/bin/bash

set -e

# =============== CONFIG =================
STACK_DIR="infra-monitor"
ENV_FILE="$STACK_DIR/.env"

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

# ============= CLEAN =====================
echo -e "${YEL}Cleaning previous $STACK_DIR if it exists...${NC}"
rm -rf "$STACK_DIR"

# ============= SETUP ====================
echo -e "${GRN}Setting up Infra Monitor stack in $STACK_DIR...${NC}"
mkdir -p $STACK_DIR/config $STACK_DIR/alerts $STACK_DIR/provisioning/{dashboards,datasources}

# 1. Restore full file generation
cp ./setup.sh $STACK_DIR/setup.sh
chmod +x $STACK_DIR/setup.sh

# Generate Prometheus config
cat > $STACK_DIR/config/prometheus.yml <<EOF
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

# Generate Alertmanager config
cat > $STACK_DIR/config/alertmanager.yml <<EOF
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

# Generate Fluent Bit config
cat > $STACK_DIR/config/fluentbit.conf <<EOF
[SERVICE]
    Flush        5
    Daemon       Off
    Log_Level    info
[INPUT]
    Name              tail
    Path              /var/log/syslog,/var/log/messages
    Tag               host.*
    Refresh_Interval  5
    DB                /var/log/flb_syslog.db
    Mem_Buf_Limit     5MB
    Skip_Long_Lines   On
[INPUT]
    Name              tail
    Path              /var/lib/docker/containers/*/*.log
    Parser            docker
    Tag               containers.*
    Refresh_Interval  5
    Mem_Buf_Limit     5MB
    Skip_Long_Lines   On
[FILTER]
    Name                modify
    Match               containers.*
    Add                 job container-logs
[OUTPUT]
    Name          loki
    Match         host.*
    Host          loki
    Port          3100
    Labels        job=infra-monitor,host=\${HOSTNAME}
[OUTPUT]
    Name          loki
    Match         containers.*
    Host          loki
    Port          3100
    Labels        job=container-logs,container=\${HOSTNAME}
EOF

# Alerts rules
cat > $STACK_DIR/alerts/rules.yml <<EOF
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

# Grafana datasources
cat > $STACK_DIR/provisioning/datasources/prometheus.yml <<EOF
apiVersion: 1
datasources:
  - name: Prometheus
    type: prometheus
    access: proxy
    url: http://prometheus:9090
    isDefault: true
  - name: Loki
    type: loki
    access: proxy
    url: http://loki:3100
EOF

# Grafana dashboard loader
cat > $STACK_DIR/provisioning/dashboards/dashboard.yml <<EOF
apiVersion: 1
providers:
  - name: 'default'
    folder: 'Infra Monitor'
    type: file
    updateIntervalSeconds: 30
    options:
      path: /etc/grafana/provisioning/dashboards
EOF

# Download dashboard JSON
curl -s https://grafana.com/api/dashboards/1860/revisions/34/download \
  -o $STACK_DIR/provisioning/dashboards/node-exporter-full.json

# 2. .env
cat > $ENV_FILE <<EOF
TELEGRAM_API_KEY=your_bot_api_key
TELEGRAM_CHAT_ID=your_chat_id
EOF

# 3. docker-compose.yml
cat > $STACK_DIR/docker-compose.yml <<EOF
version: '3.8'
services:
  prometheus:
    image: prom/prometheus:latest
    volumes:
      - ./config/prometheus.yml:/etc/prometheus/prometheus.yml
      - ./alerts/rules.yml:/etc/prometheus/rules.yml
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

  loki:
    image: grafana/loki:latest
    ports:
      - "3100:3100"
    command: -config.file=/etc/loki/local-config.yaml

  fluentbit:
    image: cr.fluentbit.io/fluent/fluent-bit:latest
    volumes:
      - /var/log:/var/log:ro
      - /var/lib/docker/containers:/var/lib/docker/containers:ro
      - ./config/fluentbit.conf:/fluent-bit/etc/fluent-bit.conf
    depends_on:
      - loki

  jaeger:
    image: jaegertracing/all-in-one:latest
    ports:
      - "16686:16686"
      - "14268:14268"
      - "4317:4317"

volumes:
  grafana-storage:
EOF

# 4. Start stack
echo -e "${YEL}Launching Docker stack...${NC}"
cd $STACK_DIR
docker compose up -d || docker-compose up -d

echo -e "${GRN}Infra Monitor is live!${NC}"
echo "Prometheus:      http://localhost:9090"
echo "Grafana:         http://localhost:3000 (admin/admin)"
echo "Alertmanager:    http://localhost:9093"
echo "Jaeger Tracing:  http://localhost:16686"
echo "Loki Logs:       http://localhost:3100 (via Grafana Explore tab)"


# 5. Create README.md
cat > README.md <<'EOF'
# Infra Monitor as a Service 🚨📊

A one-liner self-hosted observability stack that gives you:
- **Metrics** (Prometheus + Node Exporter)
- **Logs** (Fluent Bit + Loki)
- **Traces** (Jaeger)
- **Dashboards** (Grafana)
- **Alerts** (via Alertmanager & Telegram)

---

## 🛠 Requirements
- Docker
- Docker Compose

## ⚡ Setup
```bash
chmod +x setup.sh
./setup.sh
```

Once deployed:
- Grafana: `http://localhost:3000` (admin/admin)
- Prometheus: `http://localhost:9090`
- Alertmanager: `http://localhost:9093`
- Jaeger: `http://localhost:16686`
- Loki (logs via Grafana Explore): `http://localhost:3100`

## 📦 Build as .deb or .rpm
```bash
make deb       # For Debian/Ubuntu
make rpm       # For Fedora/RedHat
make install   # Auto-detect distro and install
```

Set your Telegram `.env` values:
```
TELEGRAM_API_KEY=your_bot_token
TELEGRAM_CHAT_ID=your_chat_id
```

---

MIT License · Built for DevOps by Devs 🚀
EOF

# 6. Create Makefile
cat > Makefile <<EOF
NAME = infra-monitor
VERSION = 1.0.0
ARCH = all
PACKAGE_DIR = \$(NAME)-pkg
DISTRO = \$(shell . /etc/os-release && echo \$ID)

.PHONY: all deb rpm install clean

all: install

deb:
	rm -rf \$(PACKAGE_DIR)
	mkdir -p \$(PACKAGE_DIR)/DEBIAN
	mkdir -p \$(PACKAGE_DIR)/opt/\$(NAME)
	cp -r * \$(PACKAGE_DIR)/opt/\$(NAME)
	echo "Package: \$(NAME)\nVersion: \$(VERSION)\nArchitecture: \$(ARCH)\nMaintainer: Your Name <you@example.com>\nDepends: docker.io, docker-compose, curl\nDescription: Self-hosted infra monitor with Prometheus, Grafana, Loki, Jaeger." > \$(PACKAGE_DIR)/DEBIAN/control
	dpkg-deb --build \$(PACKAGE_DIR)
	mv \$(PACKAGE_DIR).deb \$(NAME)_\$(VERSION)_\$(ARCH).deb

rpm:
	@if ! command -v fpm >/dev/null 2>&1; then \
		echo "❌ fpm not installed. Use: gem install fpm"; \
		exit 1; \
	fi
	fpm -s dir -t rpm -n \$(NAME) -v \$(VERSION) --prefix /opt/\$(NAME) \
		--depends docker --depends docker-compose --depends curl \
		--description "Self-hosted infra monitor with Prometheus, Grafana, Loki, Jaeger." \
		setup.sh docker-compose.yml config provisioning alerts .env

install:
	@echo "📦 Detecting distro: \$(DISTRO)..."
ifeq (\$(DISTRO),debian)
	make deb
	sudo dpkg -i \$(NAME)_\$(VERSION)_\$(ARCH).deb
endif
ifeq (\$(DISTRO),ubuntu)
	make deb
	sudo dpkg -i \$(NAME)_\$(VERSION)_\$(ARCH).deb
endif
ifeq (\$(DISTRO),fedora)
	make rpm
	sudo rpm -i \$(NAME)-\$(VERSION)-1.noarch.rpm
endif
	@echo "✅ Installed \$(NAME)!"

clean:
	rm -rf \$(PACKAGE_DIR) *.deb *.rpm
EOF
