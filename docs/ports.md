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
| Grafana on kind (M7) | 13000 | `make kind-grafana`: a `kubectl port-forward` on `127.0.0.1` while the kind cluster runs (ADR-0075) |
| Prometheus on kind (M7) | 19091 | `make kind-prometheus`: a `kubectl port-forward` on `127.0.0.1` while the kind cluster runs (ADR-0075) |

The gateway ports belong to `gateway-1`. Multi-replica runs add `gateway-2` … `gateway-5` (compose profile `replicas`), which publish no host ports. Processes run on the host reach the Redis nodes through the address map in [ADR-0004](adr/0004-dev-networking.md).

The compose service `redis-exporter` (ADR-0087) listens on **9121** inside the compose network only, where Prometheus scrapes it; it is not published on the host.

`make chaos-redis-latency` (ADR-0074) creates toxiproxy proxies `redis-0` … `redis-5` listening on **26370–26375** inside the compose network (`toxiproxy:2637N` → `redis-N:6379`); they are not published on the host and are deleted when the test ends.

The demo (M8, ADR-0094 – ADR-0097) publishes only Caddy's **8888** on the host. Everything else it adds stays inside the compose network: the toxiproxy proxies `demo-redis-0` … `demo-redis-5` on **26380–26385** (`toxiproxy:2638N` → `redis-N:6379`, the demo gateways' Redis path) and `demo-upstream-1` … `demo-upstream-3` on **26391–26393** (`toxiproxy:2639N` → `upstream-N:8080`, the `orders` upstream), created by `make demo` and deleted by `make demo-down`; `demo-traffic`'s health listener on **8090** (loopback inside its container); and the demo Grafana `grafana-demo` on **3000** inside the network, which Caddy serves at `/grafana`. Caddy and the tunnel container use the fixed addresses 172.29.255.10 and 172.29.255.11 on the compose network. A tunnel points only at Caddy; never at 8001, 8081, 9090 or the gateway.

Inside the kind cluster (M7, ADR-0075), the gateway's NodePort is **30443**, the node-side end of the 443 mapping; it is a port on the kind node container, not on the host, and nothing else in the cluster is published on the host. kind itself maps the cluster's API server to a random free port on `127.0.0.1` (written into `~/.kube/spillway-kind`), so it needs no row here; `make kind-smoke` checks that the control-plane node publishes only that and 443, and the workers nothing (ADR-0085). kind and the compose stack are not run together (memory), so the kind port-forwards use their own ports rather than compose's 3000 and 9091.

## Before `make dev`

On macOS, turn off AirPlay Receiver (System Settings → General → AirDrop & Handoff → AirPlay Receiver): it listens on 5000 and 7000, and Redis Cluster needs 7000–7005. See [setup.md](setup.md) Step 1.

Check that every port is free (macOS/Linux). The command prints nothing when all are free; any line it prints names the process holding a port:

```bash
lsof -nP -iTCP -sTCP:LISTEN | grep -E ':(443|3000|3100|3200|4317|4318|5173|5432|700[0-5]|8000|8001|8080|8081|8180|8443|8474|8888|9090|9091|910[1-3]|13000|19091) '
```

On Linux, run it with `sudo` (without root, lsof lists only your own processes), or use `ss`, which needs no root:

```bash
ss -Hltn | awk '{print $4}' | grep -E ':(443|3000|3100|3200|4317|4318|5173|5432|700[0-5]|8000|8001|8080|8081|8180|8443|8474|8888|9090|9091|910[1-3]|13000|19091)$'
```

## Changing a port

Edit docs/ports.md, `compose.yaml` and the Helm values in the same PR, and say so in the PR description.
