# 0004. Dev networking: every process in compose, Redis Cluster by hostname

- **Status:** Accepted
- **Date:** 2026-11-02
- **Requirement IDs:** SEC-04, SEC-06, RL-13, RL-23, NFR-06, NFR-13, SC-03, SC-09

## Context

Redis Cluster tells clients where each slot lives (`CLUSTER SLOTS`, `MOVED`, `ASK`). In Docker those answers carry container IPs, which processes on the host cannot reach (Docker Desktop runs containers in a VM); setting `cluster-announce-ip` to a host address instead breaks clients inside the network. Script and cluster tests need real slots and redirects, not a single node. Keycloak has the same split: the browser reaches it at `localhost:8180`, containers at `keycloak:8080`, and a token’s `iss` must match one configured issuer. The accuracy harness, chaos tests and the demo need several gateway replicas side by side. `make dev` must work from a clean clone on macOS, Linux and in CI, with `.env.local` optional.

## Decision

- **Every Spillway process runs as a compose service**: gateway, control plane, loadgen and the accuracy harness, demo traffic. `make dev` = `docker compose up -d --wait`, so every long-running service has a healthcheck and one-shot services (`redis-init`, `migrate`) must exit 0. Multi-replica runs use `gateway-1` … `gateway-5` under the compose profile `replicas`; only `gateway-1` publishes host ports (docs/ports.md).
- **Host-run processes and tests** (`go test`, `go run`, a debugger) reach the cluster through a go-redis `Dialer` that maps `redis-N:6379` → `127.0.0.1:700N`. It is enabled only by `SPILLWAY_REDIS_ADDR_MAP` (dev and test only, which includes the compose demo; never set in Helm), e.g. `redis-0:6379=127.0.0.1:7000,…,redis-5:6379=127.0.0.1:7005`.
- **Redis nodes** `redis-0` … `redis-5` (3 primaries + 3 replicas): `cluster-announce-hostname redis-N`, `cluster-preferred-endpoint-type hostname`, `cluster-node-timeout 3000`, no persistence (`appendonly no`, `save ""`), `maxmemory 256mb`, `maxmemory-policy noeviction`. Eviction would silently reset limiter state; with `noeviction` a full node fails writes, which go through each policy’s failure mode (RL-23) and alert. `nodes.conf` lives on tmpfs, and a one-shot `redis-init` service creates the cluster fresh on every `up`.
- **Tests**: cluster and script integration tests run against the compose cluster through the Dialer, locally and in CI; testcontainers covers Postgres and single-node Redis cases.
- **Keycloak in dev**: `KC_HOSTNAME=http://localhost:8180/auth`, `KC_HTTP_RELATIVE_PATH=/auth` (the same path the demo proxy uses), `KC_HOSTNAME_BACKCHANNEL_DYNAMIC=true`. Services check `iss` against the browser-facing issuer (`http://localhost:8180/auth/realms/spillway`) and fetch JWKS from a separately configured URL (`http://keycloak:8080/auth/realms/spillway/protocol/openid-connect/certs` inside the network). Plain-HTTP issuers and JWKS URLs are accepted only with `dev_mode: true`, never in Helm (SEC-04).
- **Upstream allow-list** (SEC-06): the dev list includes the compose subnet; kind ranges are added in M7.
- **Fault injection** (SC-03): for the Redis latency test and the demo fault switches, the same address map points gateways at per-node toxiproxy proxies instead of the nodes. No container gets the Docker socket.
- **Env loading**: the Makefile passes `--env-file .env.example` and adds `--env-file .env.local` only if the file exists; services list `.env.local` as an `env_file` entry with `required: false` (Compose 2.24 or later). Every `${VAR}` in `compose.yaml` has a dev default (`${VAR:-…}`), so bare `docker compose` commands such as `docker compose down -v` work without the Makefile.

## Consequences

- Limiter state and the cluster itself vanish on every `down` / `up`. Nothing in dev depends on them surviving: limiter state is ephemeral by design and quotas re-seed from Postgres (RL-10).
- `--wait` turns a failing healthcheck into a failed `make dev`; `make verify-m0` also fails if a long-running service has no healthcheck or a one-shot service did not exit 0.
- Six nodes at 256 MB cap Redis at 1.5 GB of the Docker VM’s memory; the NFR-10 sweep (1M keys at ≤ 200 B, about 200 MB across three primaries) fits.
- Hostname endpoints need Redis 7+ (RL-30) and a client that follows `cluster-preferred-endpoint-type`; go-redis v9 does.
- The address map is a dev and test code path in the Redis client, off unless the variable is set; the Helm chart never sets it.
- CI boots the same compose cluster, which adds its start-up time to the integration job but tests exactly what developers run.
- The issuer check uses the browser-facing URL, so tokens minted through another hostname (the demo tunnel) fail until `KC_HOSTNAME`, the services’ configured issuer and the `admin-ui` redirect URIs all match it; `make demo-share` sets them (SC-09).

## Alternatives considered

- **`cluster-announce-ip` set to a host address** — host clients work, in-network clients break, and the address differs per OS and network.
- **Host networking** — behaves differently on Docker Desktop than on Linux, and six nodes on one host need distinct ports that in-network clients would then have to know.
- **Single-node Redis in dev** — no slots, `MOVED` or cross-slot behaviour, so the hash-tag design (RL-13) and failover go untested until kind.
- **Redis Cluster in testcontainers** — the same announce problem, slower per test, and a second cluster definition to keep in step with compose.
- **`/etc/hosts` entries for `redis-N` and `keycloak`** — edits outside the repository on every machine and runner, and all six nodes would resolve to `127.0.0.1:6379`.
