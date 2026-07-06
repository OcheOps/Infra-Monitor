# Infra-Monitor — Exploration Notes

Scope of this document: research findings only. No implementation. Basis for a
subsequent focused-PR plan.

## 1. What the repo is

A self-hosted observability stack distributed as a `.deb` / `.rpm` package. The
"product" is: a user installs the package, gets a directory in `/opt/infra-monitor`
containing Docker Compose + configs, runs it, and has Prometheus + Grafana + Loki
+ Jaeger + Alertmanager + Node Exporter + Blackbox + Fluent Bit running locally.

Target user (confirmed): people installing the packaged artifact on a host.

## 2. Layout

```
Infra-Monitor/
├── setup.sh                 # top-level bootstrap (regenerates infra-monitor/)
└── infra-monitor/           # what setup.sh writes AND what's committed
    ├── .env                 # placeholder Telegram creds (committed)
    ├── docker-compose.yml
    ├── Makefile             # deb/rpm packaging
    ├── README.md
    ├── setup.sh             # copied from top-level by the top-level setup.sh
    ├── alerts/rules.yml
    ├── config/
    │   ├── alertmanager.yml
    │   ├── fluentbit.conf
    │   └── prometheus.yml
    └── provisioning/
        ├── dashboards/{dashboard.yml, node-exporter-full.json}
        └── datasources/prometheus.yml
```

## 3. How it works end-to-end

1. User runs `./setup.sh` from repo root.
2. `setup.sh` checks Docker + Compose, `rm -rf infra-monitor/`, recreates the
   directory, and writes every config/compose/Makefile/README from heredocs.
3. It curls a Grafana.com dashboard JSON into `provisioning/dashboards/`.
4. It writes a placeholder `.env`.
5. It runs `docker compose up -d` in the new directory.
6. For packaging, `make deb` / `make rpm` bundle the whole `infra-monitor/`
   directory under `/opt/infra-monitor` in the resulting package.

## 4. File-by-file roles

| Path | Role |
|------|------|
| `setup.sh` (root) | Bootstrap: writes the stack directory and starts it. Heavy — every file is inline. |
| `infra-monitor/docker-compose.yml` | Defines 8 services on default network, no volumes except grafana-storage. |
| `infra-monitor/Makefile` | `deb`, `rpm`, `install` (distro-detect), `clean`. |
| `infra-monitor/config/prometheus.yml` | Scrapes node-exporter, blackbox (targets: example.com, 1.1.1.1), alertmanager. |
| `infra-monitor/config/alertmanager.yml` | Single Telegram receiver; uses `${TELEGRAM_API_KEY}` / `${TELEGRAM_CHAT_ID}` env refs. |
| `infra-monitor/config/fluentbit.conf` | Tails syslog + docker container logs → ships to Loki. |
| `infra-monitor/alerts/rules.yml` | One rule: `InstanceDown` when `up == 0` for 1m. |
| `infra-monitor/provisioning/datasources/prometheus.yml` | Prometheus + Loki datasources. |
| `infra-monitor/provisioning/dashboards/dashboard.yml` | Grafana file-provider loader. |
| `infra-monitor/provisioning/dashboards/node-exporter-full.json` | Grafana dashboard #1860. |

## 5. Non-obvious findings (facts, not fixes)

- **Duplication.** `setup.sh` regenerates every file that is already committed.
  The two copies have already drifted: committed README credits "Oche",
  script-generated README credits "Devs".
- **Destructive bootstrap.** `setup.sh` does `rm -rf infra-monitor/` with no
  guard — running from a clean checkout deletes the checked-in copies.
- **Alertmanager env expansion.** Alertmanager does not expand `${VAR}` in its
  config unless started with `--config.expand-env=true`. Currently no such flag
  is passed, so the Telegram receiver's `bot_token` would be the literal string
  `${TELEGRAM_API_KEY}`.
- **Fluent Bit `docker` parser.** The tail input references `Parser docker`
  but no parsers file is declared and none is mounted. `${HOSTNAME}` in
  `Labels` requires an `Env HOSTNAME` line or shell interpolation, neither is
  present.
- **Makefile `$ID` bug.** `DISTRO = $(shell . /etc/os-release && echo $ID)` —
  `$I` is expanded by Make (empty), so the shell sees `echo D`. Needs `$$ID`.
  Same file uses `echo -e` with `\n` inside a control-file heredoc; that only
  works with certain `echo` binaries.
- **`:latest` everywhere.** Every image is unpinned — packaging a `.deb` and
  shipping it produces non-reproducible installs.
- **No persistent volumes** for Prometheus TSDB, Loki chunks, or Jaeger badger.
  Grafana is the only service with a named volume.
- **Ports on 0.0.0.0.** All service ports are published without a bind
  address — publicly reachable if the host isn't firewalled.
- **Committed `.env`** with placeholder tokens; no `.gitignore`; no
  `LICENSE` file despite README claiming MIT.
- **Jaeger receives OTLP on 4317** but nothing in the stack instruments or
  forwards traces to it.
- **Blackbox targets** are hardcoded to `https://example.com` and
  `http://1.1.1.1` — demo values, not user config.

## 6. Q&A / decisions from clarification

- **Improvement focus:** correctness fixes, DX cleanup, feature additions,
  and moving away from "bash scripts flying everywhere."
- **Audience:** distributable `.deb` / `.rpm` product — treat it as software
  shipped to third parties, not a personal homelab toy.
- **PR scope:** small and focused — top ~5 changes.
- **`setup.sh` direction:** keep both a *tiny* `setup.sh` (for first-run UX)
  **and** Make targets for day-to-day (`make up` / `make down` / `make logs`).
  Heredoc-driven regeneration goes away; committed files become the source of
  truth.
- **Feature add:** cAdvisor (container metrics) plus a matching Prometheus
  scrape job and a Grafana dashboard.

## 7. Confirmed shortlist for the PR

1. **Slim `setup.sh` + add Make targets.** `setup.sh` shrinks to: prereq check,
   copy `.env.example`→`.env` if missing, run `docker compose up -d` inside
   `infra-monitor/`. Add `make up/down/logs/restart/status` in
   `infra-monitor/Makefile` alongside existing `deb/rpm/install/clean`.
2. **Correctness bundle:** Alertmanager `--config.expand-env=true`; Fluent Bit
   parsers file + docker parser + `Env HOSTNAME`; Makefile `$$ID` and safer
   control-file generation.
3. **Compose hardening:** pin every image tag; named volumes for
   `prometheus-data`, `loki-data`, `jaeger-data`; shared `monitor` network;
   `restart: unless-stopped`; bind published ports to `127.0.0.1` by default
   (documented override for LAN exposure).
4. **Distribution hygiene:** add `.gitignore`, add `LICENSE` (MIT), rename
   committed `.env` → `.env.example`, fix Makefile maintainer field and the
   file list packaged by `fpm` to match the real layout.
5. **cAdvisor feature:** add service to compose, add scrape job to
   `prometheus.yml`, drop in a cAdvisor Grafana dashboard JSON, mention in
   README.

## 8. Constraints / non-goals

- Still single-node — no Kubernetes, no clustering.
- No auth reverse proxy, no TLS in this PR (flag for follow-up).
- No OTel Collector in this PR (deferred; Jaeger port stays exposed but
  unused).
- No behavior change for `.deb` / `.rpm` install layout beyond fixing the
  packaging bugs.

## 9. Open items (deferred, not in this PR)

- Grafana admin password default (`admin/admin`) — should be a generated or
  prompted value; requires touching setup.sh and packaging story.
- Blackbox targets should come from a user-editable file rather than being
  baked in.
- TLS / reverse proxy / SSO in front of Grafana + Prometheus + Alertmanager.
- Alertmanager receivers beyond Telegram (Slack, email, PagerDuty).
- OTel Collector to actually make Jaeger useful.
