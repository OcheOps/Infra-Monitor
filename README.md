# Infra Monitor

A self-hosted observability stack in a single binary. Run it on one host with
Docker Compose, or on a Kubernetes cluster:

- **Metrics**: Prometheus + Node Exporter + Blackbox exporter
- **Container metrics**: cAdvisor (Compose) / kubelet cAdvisor (Kubernetes)
- **Logs**: Fluent Bit → Loki
- **Traces**: Jaeger (OTLP gRPC `4317`, OTLP HTTP `4318`)
- **Dashboards**: Grafana, provisioned with Prometheus, Loki and Jaeger datasources
- **Alerts**: Alertmanager → Telegram (optional)

---

## Install

```bash
make install      # builds, then installs the .deb / .rpm, or copies to /usr/local/bin
# or just:
make build        # → bin/infra-monitor
```

Building needs Go 1.22+. At runtime you need Docker with the Compose plugin
(for `compose`) or `kubectl` pointed at a cluster (for `k8s`).

## Configure

```bash
infra-monitor init --telegram-token 123456:ABC-xyz --telegram-chat-id 987654
```

This writes `~/.infra-monitor/.env` (mode `0600`) and generates a Grafana admin
password, which it prints once. Leave out the Telegram flags to run without
notifications: alerts still show in Alertmanager but aren't sent anywhere. Run
`init` again to change a value; settings you don't pass are kept.

Grafana only reads the admin password on its first start. To change it later,
use Grafana's UI or `grafana cli admin reset-admin-password`.

Use `--dir` or `INFRA_MONITOR_DIR` to keep config somewhere else.

## Single host: Docker Compose

```bash
infra-monitor compose up              # write the project to ~/.infra-monitor/compose and start it
infra-monitor compose status
infra-monitor compose logs grafana
infra-monitor compose restart         # after `init` changes
infra-monitor compose down            # stop; data volumes are kept
```

Every port is bound to `127.0.0.1`:

| Service      | URL                    |
|--------------|------------------------|
| Grafana      | http://localhost:3000  |
| Prometheus   | http://localhost:9090  |
| Alertmanager | http://localhost:9093  |
| Jaeger       | http://localhost:16686 |
| Loki         | http://localhost:3100  |
| cAdvisor     | http://localhost:8080  |

## Kubernetes

```bash
infra-monitor k8s apply                       # current kubectl context, namespace infra-monitor
infra-monitor k8s apply -n monitoring --context prod --storage-class gp3
infra-monitor k8s status
infra-monitor k8s port-forward grafana        # → http://localhost:3000
infra-monitor k8s render > stack.yaml         # inspect, or feed to GitOps (contains secrets!)
infra-monitor k8s delete                      # keeps PersistentVolumeClaims
infra-monitor k8s delete --purge              # also deletes the namespace and all data
```

What gets deployed:

| Component     | Kind        | Notes |
|---------------|-------------|-------|
| Prometheus    | StatefulSet | 10Gi volume, 15d retention; discovers targets through the Kubernetes API |
| Alertmanager  | Deployment  | config in a Secret |
| Grafana       | StatefulSet | 1Gi volume; admin password in a Secret |
| Loki          | StatefulSet | 10Gi volume, single binary |
| Jaeger        | StatefulSet | 5Gi Badger volume |
| Node Exporter | DaemonSet   | host network/PID, runs on every node (port 9100) |
| Fluent Bit    | DaemonSet   | tails `/var/log/containers`, adds pod metadata |
| Blackbox      | Deployment  | |

Prometheus scrapes node-exporter, the kubelet and its built-in cAdvisor on every
node, plus any pod annotated `prometheus.io/scrape: "true"` (with optional
`prometheus.io/port` and `prometheus.io/path`). Changing config and running
`apply` again rolls the affected pods.

`apply` uses server-side apply, because the bundled dashboards are too large for
client-side apply's annotation. The ClusterRoles are named after the namespace,
so you can install more than one copy in a cluster.

## Layout

```
cmd/infra-monitor/   CLI entrypoint
internal/            config, compose and k8s backends
deploy/compose/      docker-compose.yml and host-specific configs
deploy/k8s/          manifest templates and k8s-specific configs
deploy/shared/       alert rules, Alertmanager template, Grafana provisioning (both backends)
```

Everything under `deploy/` is embedded in the binary at build time. To change
alert rules, scrape targets or dashboards, edit them there and rebuild.
`make test` checks the rendered manifests and that both backends pin the same
image versions.

---

MIT License · Built for DevOps by Oche
