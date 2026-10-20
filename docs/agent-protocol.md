# Agent protocol

How agent hosts send data to an Infra Monitor server, and how a hosted
backend could accept the same agents without any change on their side.

## Overview

```
agent host                                   server
┌──────────────────────────────┐   HTTPS    ┌──────────────────────────────────┐
│ node-exporter, cAdvisor      │            │ gateway (Caddy)                  │
│        │ scrape              │            │  /ingest/metrics ─► Prometheus   │
│ Prometheus (agent mode) ─────┼──────────► │                 remote-write rcv │
│                              │ Basic auth │  /ingest/logs ───► Loki push API  │
│ Fluent Bit ──────────────────┼──────────► │  /ingest/ping, /ingest/ca.crt    │
└──────────────────────────────┘            └──────────────────────────────────┘
```

Agents only make outgoing HTTPS connections, so they work behind NAT and
firewalls without opening ports.

## Endpoints

All under `https://<server>/ingest/`.

| Path | Method | Auth | Body | Upstream |
|------|--------|------|------|----------|
| `/metrics` | POST | Basic | [Prometheus remote write](https://prometheus.io/docs/specs/remote_write_spec/) (snappy protobuf) | Prometheus `/api/v1/write` |
| `/logs` | POST | Basic | [Loki push](https://grafana.com/docs/loki/latest/reference/loki-http-api/#ingest-logs) (JSON or protobuf) | Loki `/loki/api/v1/push` |
| `/ping` | GET | Basic | – | answered by the gateway: `200 ok` |
| `/ca.crt` | GET | none | – | the gateway's own root CA (only with `expose --tls internal`) |

Anything else under `/ingest/` returns 404. Wrong or missing credentials
return 401.

## Authentication

- **Credentials:** each agent has a name and a 256-bit random token, created
  by `infra-monitor agent add NAME` on the server and sent as HTTP Basic auth
  (`NAME:TOKEN`).
- **Storage:** the server keeps only a bcrypt hash of each token, and the
  token is shown once. bcrypt runs at its minimum cost: with 256-bit random
  tokens, a slower hash adds nothing against guessing, and every push is
  checked.
- **Revoking:** `infra-monitor agent remove NAME` revokes an agent once the
  server applies the change.
- **Identity header:** after authentication the gateway adds
  `X-Infra-Monitor-Agent: NAME` to the upstream request.

### Trusting the server

- **Public certificates** (Let's Encrypt, or the Ingress on Kubernetes): agents
  use the system trust store.
- **`expose --tls internal`:** the join command includes `--ca-sha256`, the
  SHA-256 of the gateway's root CA certificate (DER). `agent join` downloads
  `/ingest/ca.crt` without verification, compares the fingerprint, and from
  then on trusts only that CA for this server.
- **`--server-ip`:** when the server's hostname doesn't resolve from the agent
  host, this flag maps it to an IP, both for the CLI and inside the agent's
  containers. TLS still verifies the hostname.

## Labels

- **Metrics:** every series carries `agent="NAME"`, set by Prometheus as an
  external label, and its `instance` is relabelled to `NAME`, so Node Exporter
  and cAdvisor dashboards list each host by name.
- **Logs:** log streams carry `agent`, `host` and `job`, where `job` is
  `infra-monitor` for host logs and `container-logs` for Docker containers.
- **Offline alert:** for each registered agent the server generates an
  `AgentDown` rule,
  `absent_over_time(up{job="node",agent="NAME"}[5m])`. It fires when the
  agent stops sending, including when the whole host goes down.

## Trust model and limits

Self-hosted servers are single-tenant: every agent belongs to the same owner.
Agents could still set labels that impersonate each other; the gateway
doesn't rewrite them.

## Hosted backend (future)

The join command takes any server URL, so a hosted service can accept these
agents unchanged by implementing the four endpoints above with the same
semantics:

| Concern | Self-hosted today | Hosted backend |
|---------|-------------------|----------------|
| Metrics store | Prometheus remote-write receiver | A multi-tenant store such as Grafana Mimir |
| Log store | Loki, auth disabled | Loki with `auth_enabled: true` |
| Tenant | one per server | derived from the agent's credentials: look up `NAME:TOKEN` → tenant, then set `X-Scope-OrgID` upstream |
| Label integrity | trusted | enforce `agent` from the authenticated identity (relabel on ingest) |
| Agent changes | – | none: same join command with the hosted URL |

The `X-Infra-Monitor-Agent` header and the per-agent credentials are there so
the gateway can be replaced by a tenant-aware proxy without touching agents.
