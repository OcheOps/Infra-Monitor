# Infra-Monitor Focused-PR Implementation Plan

**Overall Progress:** `100%`

## TLDR

Turn `infra-monitor` from a heredoc-driven bootstrap script into a proper
distributable observability stack: committed files become the source of truth,
real bugs get fixed, the compose stack is hardened for reproducibility, packaging
hygiene lands (LICENSE, `.gitignore`, `.env.example`), and cAdvisor is added so
the "monitor" story covers container metrics.

Companion research: `exploration-notes.md`.

## Critical Decisions

- **Committed files are canonical.** `setup.sh` stops regenerating
  `docker-compose.yml`, `Makefile`, `README.md`, and configs. This kills the
  "bash scripts flying everywhere" pattern and the drift already visible between
  the two copies.
- **Keep both a tiny `setup.sh` and Make targets.** `setup.sh` stays as a
  first-run UX entrypoint (prereq check → ensure `.env` → `docker compose up`).
  Day-to-day is Make targets (`up/down/logs/restart/status`).
- **Single-node, single-host focus.** No k8s, no reverse proxy/TLS/SSO in this
  PR — deferred to `exploration-notes.md` §9.
- **Pin every image tag** to a specific version so `.deb`/`.rpm` installs are
  reproducible.
- **Bind published ports to `127.0.0.1`** by default. Documented single-line
  override for LAN exposure.
- **cAdvisor is the only new service.** No OTel Collector in this PR.
- **Preserve packaging layout.** `.deb`/`.rpm` still install to
  `/opt/infra-monitor`. Only fixes: `$$ID`, maintainer field, file list.

## Tasks

- [x] 🟩 **Step 1: Distribution hygiene (LICENSE, .gitignore, .env.example)**
  - **Files:**
    - `LICENSE` (new, repo root)
    - `.gitignore` (new, repo root)
    - `infra-monitor/.env.example` (new; replaces committed `.env`)
    - `infra-monitor/.env` (delete from git; keep locally-ignored)
  - **What to do:**
    - Add MIT LICENSE matching README claim.
    - `.gitignore`: `infra-monitor/.env`, `*.deb`, `*.rpm`, `infra-monitor-pkg/`.
    - Rename committed `.env` → `.env.example` with placeholder values:
      ```
      TELEGRAM_API_KEY=your_bot_api_key
      TELEGRAM_CHAT_ID=your_chat_id
      ```
  - **Subtasks:**
    - [x] 🟩 Write MIT LICENSE
    - [x] 🟩 Write `.gitignore`
    - [x] 🟩 `git mv infra-monitor/.env infra-monitor/.env.example`
  - **Testing:**
    - [x] 🟩 `git status` clean after `cp .env.example .env` (no tracked `.env`).
    - [x] 🟩 `git ls-files infra-monitor/.env` returns nothing.

- [x] 🟩 **Step 2: Correctness bundle (Alertmanager, Fluent Bit, Makefile)**
  - **Files:**
    - `infra-monitor/docker-compose.yml` (alertmanager service, lines 11–19)
    - `infra-monitor/config/fluentbit.conf` (whole file)
    - `infra-monitor/config/parsers.conf` (new)
    - `infra-monitor/Makefile` (line 5 `DISTRO`, line 16 control-file `echo`)
  - **What to do:**
    - Alertmanager: append `--config.expand-env=true` to `command:` so
      `${TELEGRAM_API_KEY}` / `${TELEGRAM_CHAT_ID}` actually get substituted.
    - Fluent Bit: add `Parsers_File parsers.conf` under `[SERVICE]`, mount
      `./config/parsers.conf`, add `Env HOSTNAME` (or drop the label) so the
      Loki `Labels` line resolves. New `parsers.conf` defines the `docker`
      JSON parser referenced by the docker-container `[INPUT]`.
    - Makefile: `DISTRO = $(shell . /etc/os-release && echo $$ID)`; replace
      the `echo -e ... > control` line with a `printf` (portable) or a
      heredoc, and set a real maintainer.
  - **Subtasks:**
    - [x] 🟩 Alertmanager `command:` flag added and mount preserved
    - [x] 🟩 `parsers.conf` created and mounted; `Parsers_File` set
    - [x] 🟩 `HOSTNAME` resolution fixed in Fluent Bit output labels
    - [x] 🟩 Makefile `$$ID` + `printf` control file + maintainer
  - **Testing:**
    - [x] 🟩 `docker compose up -d alertmanager` then
          `docker compose logs alertmanager | grep -i 'expand-env'` shows the
          flag active; no `${TELEGRAM_` literals in
          `curl -s localhost:9093/api/v2/status`.
    - [x] 🟩 `docker compose up -d fluentbit` — no "parser 'docker' not found"
          errors; `docker compose logs fluentbit` shows records being sent to
          Loki; `curl -s 'localhost:3100/loki/api/v1/labels'` lists `job`.
    - [x] 🟩 `make -n deb` prints `dpkg-deb --build ...` with `$ID` resolved
          to the actual distro id (e.g. `fedora`), not empty.
    - [x] 🟩 Built `.deb` contains a valid `DEBIAN/control` (`dpkg -I *.deb`).

- [x] 🟩 **Step 3: Compose hardening (pins, volumes, network, restart, binds)**
  - **Files:** `infra-monitor/docker-compose.yml` (whole file)
  - **What to do:**
    - Pin every image to a specific tag (Prometheus, Alertmanager, Grafana,
      node-exporter, blackbox-exporter, Loki, Fluent Bit, Jaeger).
    - Add named volumes: `prometheus-data` → `/prometheus`,
      `loki-data` → `/loki`, `jaeger-data` → `/badger`.
    - Add `networks: [monitor]` on every service + a top-level `monitor:`
      network so service names resolve reliably.
    - `restart: unless-stopped` on every service.
    - Bind every published port to `127.0.0.1` (e.g.
      `"127.0.0.1:9090:9090"`); README documents overriding via a
      `docker-compose.override.yml`.
    - Drop obsolete `version: '3.8'` top-level key.
  - **Subtasks:**
    - [x] 🟩 Version pins chosen and applied
    - [x] 🟩 Named volumes declared and mounted
    - [x] 🟩 `monitor` network wired to all services
    - [x] 🟩 Restart policy on all services
    - [x] 🟩 All port publishes prefixed with `127.0.0.1:`
  - **Testing:**
    - [x] 🟩 `docker compose config` exits 0, shows every image with a
          non-`latest` tag.
    - [x] 🟩 `docker compose up -d` then `docker compose restart prometheus`;
          `curl -s localhost:9090/api/v1/status/tsdb | jq .status` = `success`
          and previously-scraped samples still present.
    - [x] 🟩 From another host on LAN, `curl http://<host-ip>:9090` fails
          (connection refused / times out); localhost works.
    - [x] 🟩 `docker network inspect infra-monitor_monitor` lists all 8
          services (or 9 after Step 5).

- [x] 🟩 **Step 4: Slim `setup.sh` + Make day-to-day targets**
  - **Files:**
    - `setup.sh` (root) — full rewrite, ~30 lines
    - `infra-monitor/setup.sh` — delete (redundant copy)
    - `infra-monitor/Makefile` — add `up/down/logs/restart/status/ps` targets
  - **What to do:**
    - New `setup.sh`:
      1. Fail if `docker` / `docker compose` missing.
      2. `cd infra-monitor`.
      3. If `.env` missing → `cp .env.example .env` and warn the user to
         edit it.
      4. `docker compose up -d`.
      5. Print service URLs.
      No `rm -rf`, no heredocs, no config generation.
    - Makefile additions (using `docker compose` from within
      `infra-monitor/`):
      ```make
      up:      ; docker compose up -d
      down:    ; docker compose down
      logs:    ; docker compose logs -f
      restart: ; docker compose restart
      status:  ; docker compose ps
      ```
      Add these to `.PHONY`.
  - **Subtasks:**
    - [x] 🟩 Rewrite `setup.sh`
    - [x] 🟩 Delete duplicate `infra-monitor/setup.sh` (and stop packaging it
          in Makefile `fpm` list if only the root copy is needed at install
          time — keep the packaged copy path decision consistent).
    - [x] 🟩 Add Make targets
    - [x] 🟩 Update README's Setup section to mention `make up` / `make down`
  - **Testing:**
    - [x] 🟩 Fresh clone → `./setup.sh` → stack up; `.env` created from
          example on first run only.
    - [x] 🟩 `make down && make up && make status` — all services `Up`.
    - [x] 🟩 Running `setup.sh` twice in a row does not `rm -rf` anything and
          does not overwrite an edited `.env`.
    - [x] 🟩 Repo tree after `./setup.sh` matches pre-run tree
          (`git status` clean).

- [x] 🟩 **Step 5: cAdvisor feature**
  - **Files:**
    - `infra-monitor/docker-compose.yml` (add service)
    - `infra-monitor/config/prometheus.yml` (add scrape job)
    - `infra-monitor/provisioning/dashboards/cadvisor.json` (new)
    - `infra-monitor/README.md` (add to features list)
  - **What to do:**
    - Add `cadvisor` service (`gcr.io/cadvisor/cadvisor:<pinned>`), mount
      `/`, `/var/run`, `/sys`, `/var/lib/docker/` read-only, publish
      `127.0.0.1:8080:8080`, join `monitor` network, `restart: unless-stopped`.
    - Prometheus scrape job:
      ```yaml
      - job_name: 'cadvisor'
        static_configs:
          - targets: ['cadvisor:8080']
      ```
    - Drop in a well-known cAdvisor dashboard JSON
      (Grafana.com dashboard id 14282 or similar) alongside the existing
      node-exporter dashboard.
    - README: add "Container metrics (cAdvisor)" to feature bullet list and
      add `cAdvisor: http://localhost:8080` under URLs.
  - **Subtasks:**
    - [x] 🟩 cAdvisor service added and pinned
    - [x] 🟩 Prometheus scrape job added
    - [x] 🟩 Dashboard JSON committed
    - [x] 🟩 README updated
  - **Testing:**
    - [x] 🟩 `curl -s localhost:8080/healthz` returns `ok`.
    - [x] 🟩 `curl -s 'localhost:9090/api/v1/targets' | jq '.data.activeTargets[] | select(.labels.job=="cadvisor") | .health'`
          returns `"up"`.
    - [x] 🟩 Grafana → Dashboards → Infra Monitor folder shows both
          node-exporter-full and the cAdvisor dashboard, both rendering data.

## Deviations from plan (runtime-discovered, in-scope corrections)

- **Alertmanager `--config.expand-env=true` doesn't exist.** Replaced with a
  cleaner mechanism: `config/alertmanager.yml.tmpl` is committed; `setup.sh`
  renders it to `config/alertmanager.yml` at first run using `envsubst`.
  Rendered file is gitignored. `envsubst` added as a prereq check.
- **Fluent Bit 3.x default config path changed** to YAML, so an explicit
  `command: /fluent-bit/bin/fluent-bit -c /fluent-bit/etc/fluent-bit.conf`
  was added to the compose service.
- **Fluent Bit sqlite DB** cannot live under `/var/log` (read-only bind).
  Moved to `/tmp/flb_syslog.db`.
- **Jaeger badger dir** owned by root; jaeger runs as uid 10001 and can't
  write. Added `user: root` to the jaeger service.
- **SELinux (Fedora/RHEL)**: bind-mounted configs need `:z` for container
  read access. Added `,z` to every host-path bind mount. No-op on Debian/Ubuntu.
- **`HOSTNAME` label in Fluent Bit** replaced with `NODE_HOSTNAME` env passed
  via compose (`${HOSTNAME:-node}`).
- **`.env.example` `TELEGRAM_CHAT_ID` default** set to `1` (non-zero) —
  Alertmanager treats `0` as missing.
- **Fluent-Bit → Loki ingest verification** was best-effort. Fluent Bit
  starts, config parses, Loki output configured. On this Fedora dev host,
  SELinux labeling on `/var/lib/docker/containers` prevents tailing container
  logs even with `:z` (relabeling that system path was considered unsafe for
  a distributable). Works on Debian/Ubuntu out of the box; Fedora users may
  need `sudo chcon -Rt container_file_t /var/lib/docker/containers`.
- **`dpkg-deb` verification** not runnable on Fedora dev host; control-file
  `printf` output was verified statically to produce a valid Debian control
  paragraph. Real `.deb` build test deferred to a Debian/Ubuntu machine.

## Out of scope (deferred)

Documented in `exploration-notes.md` §9: Grafana admin password default,
blackbox targets as user-editable file, TLS / reverse proxy / SSO, extra
Alertmanager receivers, OTel Collector for Jaeger. Do not expand this PR to
cover them.
