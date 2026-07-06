# Infra Monitor as a Service

A one-liner self-hosted observability stack that gives you:
- **Metrics** (Prometheus + Node Exporter)
- **Container metrics** (cAdvisor)
- **Logs** (Fluent Bit + Loki)
- **Traces** (Jaeger)
- **Dashboards** (Grafana)
- **Alerts** (Alertmanager → Telegram)

---

## Requirements
- Docker + Docker Compose plugin
- `envsubst` (part of `gettext`)

## First run
```bash
./setup.sh          # creates .env from .env.example on first run
# edit infra-monitor/.env with your Telegram bot token and chat id
./setup.sh          # renders alertmanager config and starts the stack
```

## Day-to-day
From `infra-monitor/`:
```bash
make up       # docker compose up -d
make down     # docker compose down
make logs     # tail all logs
make restart  # restart all services
make status   # docker compose ps
```

## Endpoints
By default every port is bound to `127.0.0.1`. To expose on the LAN drop a
`docker-compose.override.yml` next to `docker-compose.yml` and re-publish the
port you need.

| Service      | URL                              |
|--------------|----------------------------------|
| Grafana      | http://localhost:3000 (admin/admin) |
| Prometheus   | http://localhost:9090            |
| Alertmanager | http://localhost:9093            |
| Jaeger       | http://localhost:16686           |
| Loki         | http://localhost:3100            |
| cAdvisor     | http://localhost:8080            |

## Package as .deb / .rpm
```bash
make deb       # Debian/Ubuntu
make rpm       # Fedora/RHEL (needs fpm)
make install   # auto-detect distro and install
```

## Configuration
- `.env` — Telegram credentials (never committed).
- `config/alertmanager.yml.tmpl` — template; `setup.sh` renders it to
  `alertmanager.yml` using values from `.env`.
- `config/prometheus.yml`, `config/fluentbit.conf`, `alerts/rules.yml` — edit
  directly and `make restart`.

---

MIT License · Built for DevOps by Oche
