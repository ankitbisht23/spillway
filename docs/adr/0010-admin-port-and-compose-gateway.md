# 0010. Admin port and the gateway in compose

- **Status:** Accepted
- **Date:** 2026-11-04
- **Requirement IDs:** GW-24, GW-25, OB-07, SEC-02, SEC-06

## Context

GW-24 asks for `/healthz` and `/readyz`; GW-25 and OB-07 ask for a separate admin port with `/metrics`, `pprof` and Go runtime metrics, bound to an internal interface (SEC-02). ADR-0008's settings have no admin listener yet. The gateway also joins compose now (ADR-0004), which raises three questions the design does not settle:

- The image is distroless with no shell or HTTP client, so a compose healthcheck has nothing to run (ADR-0006 copies busybox into third-party images for this).
- Prometheus scrapes from inside the compose network, while the host may reach the admin port only on `127.0.0.1`.
- SEC-06 wants the dev allow-list to include the compose subnet, but Docker picks a default network's subnet at random unless it is pinned.

## Decision

- **Setting.** `SPILLWAY_GATEWAY_ADMIN_ADDR` is the admin listener, default `127.0.0.1:8001`. Like ADR-0008's settings it comes from the environment only. Compose sets `:8001` inside the container and publishes it on `127.0.0.1:8001`; Kubernetes (M7) binds it to the pod IP.
- **Admin endpoints**, on their own `http.ServeMux`:

  | Path | Answer |
  | --- | --- |
  | `/healthz` | 200 while the process runs |
  | `/readyz` | 200 while a config snapshot is loaded and shutdown has not started, else 503; Redis never counts |
  | `/metrics` | Go runtime (`go_*`) and process (`process_*`) collectors, and `gw_config_version`, read from the current snapshot at scrape time |
  | `/debug/pprof/` | `net/http/pprof`, registered on the admin mux only, never on `http.DefaultServeMux` |

  On the traffic port these paths are routed like any other path.
- **Admin server limits.** Read-header timeout 10 s and idle timeout 120 s, but no write timeout, so CPU profiles and traces can run longer than the traffic port's 60 s.
- **Shutdown.** On SIGTERM or SIGINT, `/readyz` turns 503 at once, the traffic server gets up to 10 s to finish in-flight requests, then the admin server closes. The drain period between the two is GW-23, M4.
- **`gateway healthcheck`.** The binary GETs `/readyz` on the address in `SPILLWAY_GATEWAY_ADMIN_ADDR`, asking a wildcard host (`:8001`, `0.0.0.0`) on `127.0.0.1`, with a 2 s timeout; it exits 0 on 200 and 1 otherwise. The compose healthcheck runs `/gateway healthcheck`.
- **Compose services.** `gateway-1` is built from the root `Dockerfile` target `gateway` as `spillway-gateway:dev`; `gateway-2` … `gateway-5` are the same service under the profile `replicas` and publish no host ports. Each has a read-only root filesystem, `nofile` 65536, and waits for the three mock upstreams to be healthy. The directory `deploy/compose/gateway/` is mounted read-only at `/etc/spillway`, not the file, because editors that save by rename would leave a file mount on the old inode and the gateway would never reload.
- **Compose network.** The default network is pinned to `172.29.0.0/16`, and `SPILLWAY_GATEWAY_ALLOWED_NETWORKS` is exactly that subnet.
- **Scraping.** Prometheus scrapes `gateway-1:8001` as job `gateway`. Replicas are added when a test needs their metrics (the M3 accuracy harness).

## Consequences

- The admin port has no authentication; only the compose network and the host's loopback reach it. `/debug/pprof/cmdline` shows the command line, which carries no settings, since those come from the environment.
- Readiness stays true from start to shutdown in M1, because the gateway exits when its first config load fails. It starts to matter when the config arrives from the control plane (M5) and when the drain period lands (GW-23, M4).
- An existing dev stack needs `docker compose down` (without `-v`) once before `make dev`: Compose recreates the network for the new subnet but re-attaches running containers without their service-name aliases, so `redis-init` cannot resolve `redis-0`. Fresh clones and CI are unaffected.
- `172.29.0.0/16` sits inside Docker's default address pools. If another network on the machine already holds it, `make dev` fails with an overlapping-pool error; pick a free subnet and change it in both places in `compose.yaml`.
- Every replica service builds the same image; after the first build the rest come from the cache.

## Alternatives considered

- **Admin endpoints on the traffic port under a reserved prefix** — a route could shadow them and they would face the internet, which GW-25 rules out.
- **Default admin address on all interfaces** — pprof would be exposed wherever the gateway runs unless someone remembered to restrict it; SEC-02 wants admin surfaces closed by default.
- **A busybox `wget` healthcheck, as in ADR-0006** — a second binary in our own image for a check the gateway binary can do itself.
- **No pinned subnet, allow `172.16.0.0/12`** — that allows every Docker network on the host, which is wider than SEC-06's "compose subnet".
- **The Prometheus default registry** — global state that tests would share; a registry built in `main` keeps them independent.
