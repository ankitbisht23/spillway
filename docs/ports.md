# Ports

This file is the source of truth for every host port the stack uses; `compose.yaml` and the Helm values must match it, and changes follow the rule at the end. Compose publishes every port on `127.0.0.1` only (for example `127.0.0.1:5432:5432`), so nothing is reachable from the local network.

| Service | Host port(s) | Notes |
| --- | --- | --- |
| Postgres | 5432 | Control-plane store |
| Redis Cluster | 7000–7005 | 3 primaries + 3 replicas. Cluster bus 17000–17005 stays inside the compose network (not published). Needs macOS AirPlay Receiver off |
| Keycloak | 8180 | OIDC provider, admin console |
| Gateway | 8443 HTTPS traffic · 8000 plain HTTP (dev/bench only, off by default) · 8001 admin | Admin port: `/metrics`, `pprof`, `/healthz`, `/readyz`; internal only (GW-25, SEC-02) |
| Control plane | 8080 REST + SSE (`/api/v1`, embedded UI at `/`) · 8081 admin · 9090 gRPC config stream | Admin: `/metrics`, `pprof`. gRPC uses mTLS from M5 |
| Admin UI (Vite dev server) | 5173 | Proxies `/api` to 8080 |
| Grafana | 3000 | |
| Prometheus | 9091 | 9090 is taken by the control-plane gRPC port |
| Loki | 3100 | |
| Tempo | 3200 | |
| OTel Collector | 4317 gRPC · 4318 HTTP | OTLP |
| Mock upstreams | 9101–9103 | |
| Toxiproxy | 8474 | API only; proxied ports stay internal |
| Demo proxy (M8) | 8888 | Caddy; the only port a tunnel points at |
| kind gateway edge (M7) | 443 | No ingress controller: `extraPortMappings` in `deploy/kind/cluster.yaml` (`listenAddress: 127.0.0.1`) map it to the gateway's NodePort Service |

The gateway ports belong to `gateway-1`. Multi-replica runs add `gateway-2` … `gateway-5` (compose profile `replicas`), which publish no host ports. Processes run on the host reach the Redis nodes through the address map in [ADR-0004](adr/0004-dev-networking.md).

## Before `make dev`

On macOS, turn off AirPlay Receiver (System Settings → General → AirDrop & Handoff → AirPlay Receiver): it listens on 5000 and 7000, and Redis Cluster needs 7000–7005. See [setup.md](setup.md) Step 1.

Check that every port is free (macOS/Linux). The command prints nothing when all are free; any line it prints names the process holding a port:

```bash
lsof -nP -iTCP -sTCP:LISTEN | grep -E ':(443|3000|3100|3200|4317|4318|5173|5432|700[0-5]|8000|8001|8080|8081|8180|8443|8474|8888|9090|9091|910[1-3]) '
```

On Linux, run it with `sudo` (without root, lsof lists only your own processes), or use `ss`, which needs no root:

```bash
ss -Hltn | awk '{print $4}' | grep -E ':(443|3000|3100|3200|4317|4318|5173|5432|700[0-5]|8000|8001|8080|8081|8180|8443|8474|8888|9090|9091|910[1-3])$'
```

## Changing a port

Edit docs/ports.md, `compose.yaml` and the Helm values in the same PR, and say so in the PR description.
