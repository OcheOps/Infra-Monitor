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
