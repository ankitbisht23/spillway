# 0085. kind cluster: implementation details

- **Status:** Accepted
- **Date:** 2026-11-26
- **Requirement IDs:** SEC-04, SEC-06, GW-25, CP-07, OB-04, OB-05

## Context

ADR-0075 §D10 sets the kind cluster: pinned kind, kubectl and helm, a node image by digest, a 3-node cluster `spillway` mapping only `127.0.0.1:443` to NodePort 30443, dependencies and observability as plain manifests with compose's images in `spillway-deps` and `spillway-obs`, Redis with a password, Keycloak on HTTPS only with a `make certs` certificate (SEC-04), Secrets made from dev files by reference only, `kind load` for local images, a memory plan of about 5.5 GB that keeps compose stopped, and `make kind-up`, `kind-down`, `kind-smoke`, `kind-grafana`, `kind-prometheus`. ADR-0068 gives the kind task the tool pins, `deploy/kind/`, the certificates, the targets and the smoke test's dependency lines; the helm task adds the chart, its smoke lines and NFR-14. This ADR records the choices ADR-0075 left open, one measured deviation from its memory plan, and the names the helm task builds on. No requirement text changes; no design.md box is ticked (boxes 6 and 7 span the helm and observability tasks).

## Decision

### Pins

1. **kind v0.33.0** through the Makefile's `tool` macro (`go install sigs.k8s.io/kind@v0.33.0`).
2. **Node image** `kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5`, the newest Kubernetes minor kind v0.33.0's release notes list (1.37, the release's default), copied from those notes; all three nodes use it.
3. **kubectl v1.37.2** (released 2026-10-28): the newest 1.37 patch at least two weeks before the task; v1.37.3 came out on 2026-11-25, too new. **helm v4.3.1** (2026-10-15).
4. A new Makefile macro, **`download-tool`**, fetches an official release file with `curl`, compares its `shasum -a 256` with the checksum committed in the Makefile for the host's platform (darwin-arm64, darwin-amd64, linux-amd64, linux-arm64; the values are the `.sha256` / `.sha256sum` files published next to each release on dl.k8s.io and get.helm.sh), installs it into `./bin` and writes the same `.<name>@<version>` stamp as `tool`. A platform without a pinned checksum, or a mismatch, stops with a message and installs nothing. helm is taken out of its tarball with `tar -xzOf`. The download goes to `bin/.<name>.download`, removed on exit.
5. **redis_exporter v1.97.0** (2026-11-04), the newest release at least two weeks old, multi-arch (amd64, arm64). Digests for every third-party image come with the release task (ADR-0076), as for compose.

### Layout

| Path | What |
| --- | --- |
| `deploy/kind/cluster.yaml` | the cluster (ADR-0075 §D10) |
| `deploy/kind/namespaces.yaml` | `spillway`, `spillway-deps`, `spillway-obs` |
| `deploy/kind/deps/` | `redis.yaml` (StatefulSet, headless Service, `redis-init` Job), `postgres.yaml`, `keycloak.yaml`, `upstream.yaml` |
| `deploy/kind/obs/` | `prometheus.yaml` (with its service account, Role and RoleBinding), `grafana.yaml`, `loki-tempo-otel.yaml`, `redis-exporter.yaml` |
| `deploy/kind/config/prometheus.yml` | kind's Prometheus config (the only config file kind does not share with compose) |
| `deploy/kind/dev-secrets.env` | the dev values (all `dev-only-not-secret`, already allowlisted; `.gitleaks.toml` unchanged) |
| `deploy/kind/up.sh` | `make kind-up` and `make kind-images` |
| `test/kind/smoke.sh`, `test/kind/smoke-job.yaml` | `make kind-smoke` |
| `test/kind/kind_test.go` | static tests, run by `go test ./...` |
| `cmd/chaos/kindsmoke.go` | `chaos kindsmoke`, the smoke test's in-cluster checks |

### One source for config files

`make kind-up` makes the ConfigMaps from **compose's own files** by reference: `redis-config` from `deploy/compose/redis/redis.conf`, `keycloak-realm` from `deploy/compose/keycloak/realm.json`, `loki-config`, `tempo-config` and `otel-collector-config` from `deploy/compose/{loki,tempo,otel-collector}/`, and `grafana-datasources` from compose's provisioning file. The observability Services keep compose's names (`prometheus`, `tempo`, `loki`, `otel-collector`) in `spillway-obs`, so those files' short host names resolve unchanged. Compose's busybox image layers exist only for compose healthchecks; kind runs the base images with HTTP probes, so `deploy/compose/*/Dockerfile`'s bases are the images kind uses.

### Names the helm task builds on

- **Secrets** (created by `make kind-up` with `kubectl create secret … --dry-run=client -o yaml | kubectl apply -f -`, never printed): `dev-secrets` in each of the three namespaces, from `deploy/kind/dev-secrets.env` (keys `POSTGRES_PASSWORD`, `SPILLWAY_CP_DATABASE_PASSWORD`, `REDIS_PASSWORD`, `KEYCLOAK_ADMIN_PASSWORD`, `GRAFANA_ADMIN_PASSWORD`, `SPILLWAY_HEAVY_HITTER_SECRET`); `spillway-deps/keycloak-tls` (type `kubernetes.io/tls`, from `deploy/certs/kind-keycloak.pem`); `spillway/gateway-tls` (type tls, from `deploy/certs/kind-gateway.pem`); `spillway/gateway-stream` and `spillway/controlplane-stream` (`cert.pem`, `key.pem`, `ca.pem` from `deploy/stream-certs/kind/{gateway,controlplane}/`).
- **ConfigMap** `spillway/mkcert-ca` (`ca.pem`: the mkcert CA's public certificate, from `$(mkcert -CAROOT)/rootCA.pem`), which signed Keycloak's and the gateway's certificates.
- **Prometheus discovery:** pods in `spillway` labelled `app.kubernetes.io/name: spillway-gateway` (job `gateway`) or `spillway-controlplane` (job `controlplane`), phase Running, scraped on their container port **named `admin`**; label `replica` = the pod name (the gateway's replica ID in Kubernetes, ADR-0075 §D11). Job names match compose's. Prometheus pods carry `app.kubernetes.io/name: prometheus` for the chart's NetworkPolicies (GW-25).
- **Optional ConfigMaps** for the observability task: `spillway-obs/prometheus-rules` (mounted at `/etc/prometheus/rules`, loaded by `rule_files: /etc/prometheus/rules/*.yaml`) and `spillway-obs/grafana-dashboards` (mounted at `/var/lib/grafana-dashboards`, read by the file provider in `grafana.yaml`). Until they exist the directories are empty.
- **Certificates** (`make certs`, gitignored): `deploy/certs/kind-gateway.pem` (names `localhost`, `127.0.0.1`, `spillway-gateway`, `spillway-gateway.spillway.svc`, `….cluster.local`), `deploy/certs/kind-keycloak.pem` (`keycloak`, `keycloak.spillway-deps`, `keycloak.spillway-deps.svc`, `….cluster.local`), and from the stream CA `deploy/stream-certs/kind/controlplane/` (`spillway-controlplane`, `….spillway.svc`, `….cluster.local`) and `deploy/stream-certs/kind/gateway/` (client, `spillway-gateway`, the fleet certificate of P10). Replacing the stream CA (the Makefile's procedure) reissues these as well.

### Dependencies

6. **Redis password** without a command line: each node's container writes `requirepass` and `masterauth` from `REDIS_PASSWORD` (the Secret) into `/run/redis/auth.conf` on a memory-backed emptyDir and starts `redis-server <compose's redis.conf> --include /run/redis/auth.conf --cluster-announce-hostname <pod>.redis.spillway-deps.svc.cluster.local`. The probe, `redis-init` and the smoke test authenticate with `REDISCLI_AUTH` from the pod's own environment. The headless Service publishes not-ready addresses, so nodes resolve one another while the cluster forms. Nodes, `redis-init` and most workloads run non-root with a read-only root filesystem and all capabilities dropped (Postgres and Keycloak keep their images' own users and writable filesystems: Postgres's entrypoint changes the data directory's owner, Keycloak builds itself at start).
7. **Keycloak** runs `start --import-realm` (production mode, not compose's `start-dev`) with `KC_DB=dev-file`: the realm is re-imported at every start, so there is no state to keep and no Postgres database for it. `KC_HTTP_ENABLED=false`, the certificate and key from `keycloak-tls`; the management port 9000 follows the main interface's TLS, so the probes are `HTTPS` (`/auth/health/started`, `/ready`, `/live`), the startup probe allowing 5 minutes. `JAVA_OPTS_KC_HEAP="-Xms256m -Xmx512m"` (ADR-0075's `-Xmx512m`). Keycloak logs `Listening on: https://0.0.0.0:8443. Management interface listening on https://0.0.0.0:9000` and nothing on HTTP.
8. **redis_exporter** sits in `spillway-obs`; the Prometheus job `redis` scrapes each node through its `/scrape?target=redis://redis-N.redis.spillway-deps.svc.cluster.local:6379` with a `node` label (`redis-0` … `redis-5`); the password from `dev-secrets` applies to every target. Compose gets redis_exporter with the observability task, which adds the dashboard and alert that use it (ADR-0075 lists it for both; the compose half has no consumer before then).

### Memory plan: one measured deviation

9. **Grafana's limit is 512 Mi (request 256 Mi), not 256 Mi.** Grafana 13.3.0's first start (its storage migrations) was OOM-killed at 256 Mi on two starts in a row, the second with `GOMEMLIMIT=200MiB`; at 512 Mi it starts at once. The dependencies' and observability's limits add up to 3,328 Mi (ADR-0075 planned about 3.1 GB for them), so the whole plan becomes about **5.75 GB**, still within 8.3 GB with compose stopped. `test/kind/kind_test.go` checks every container's request is half its limit and that these limits stay under 3,500 Mi.
10. **Open point for the user (not changed):** ADR-0075 gives each Redis node a 64 Mi limit and compose's `maxmemory 256mb`. A node filled past ~60 MB is OOM-killed before `noeviction` and the `SpillwayRedisMemoryHigh` alert (used / maxmemory > 0.8) can act. Nothing on kind comes near (an idle node uses ~3 MB, and NFR-10's million-key sweep runs on compose), so both values stay as ADR-0075 sets them; aligning them (a kind-only `maxmemory 48mb`, or a larger limit) is a decision for the user.

### Targets

11. `make kind-up` (`deploy/kind/up.sh`): refuses while any container of the compose project `spillway` runs; checks the kind certificates exist; creates the cluster if `kind get clusters` lacks it (`--wait 180s`), else re-exports its kubeconfig; applies the namespaces, Secrets, ConfigMaps, `deploy/kind/deps/` and `deploy/kind/obs/`; builds and loads the images while the nodes pull theirs; then waits for Redis, `redis-init`, Postgres, the upstream, the six observability Deployments and Keycloak (5 to 8 minutes each at most). It runs again on a live cluster in about 15 s; from nothing it took 96, 133 and 102 s in three runs (the smoke test after each: 4 s, PASS; `make kind-down`: 1 to 2 s). The chart's `helm upgrade --install` is added by the helm task.
12. `make kind-images`: `docker buildx build --load --target <t> -t spillway-<t>:kind .` for gateway, controlplane, loadgen and chaos (the host's platform; kind requires it), then one `kind load docker-image --name spillway` for the four. Rebuilds hit compose's build cache.
13. `make kind-down`: `kind delete cluster --name spillway --kubeconfig ~/.kube/spillway-kind` and removes that file.
14. `export KUBECONFIG := $(HOME)/.kube/spillway-kind` is global in the Makefile (ADR-0075); `up.sh` and `smoke.sh` refuse any other `KUBECONFIG`, define `kubectl` as a function that always passes `--context kind-spillway`, and give kind `--name spillway` (and `--kubeconfig` where it writes one). `TestContextDiscipline` fails on any kubectl, helm or kind invocation in the Makefile or those scripts without them.

### Smoke test (dependency lines)

15. `make kind-smoke` prints, PASS/FAIL: three nodes Ready; every pod Ready in `spillway-deps`, `spillway-obs` and `spillway` (finished Jobs' pods excepted; `spillway` may be empty until the chart); Redis `cluster_state:ok` on all six nodes; the **host side of the edge** from Docker's port table: the control-plane node publishes `30443/tcp -> 127.0.0.1:443` and kind's API server (`6443/tcp -> 127.0.0.1:<random>`) and nothing else, the workers nothing, and no container of the cluster maps port 80; then the Job `kind-smoke` (`chaos kindsmoke`, trusting only `mkcert-ca`): Keycloak's discovery document over HTTPS whose `issuer` is exactly `https://keycloak.spillway-deps.svc.cluster.local:8443/auth/realms/spillway` and whose `jwks_uri` is https, and that JWKS served with at least one key. The host's `https://127.0.0.1:443/bytes/16`, the gateway, control-plane, NetworkPolicy and Prometheus-target lines need the chart and come with the helm task.
16. **Stalls (ADR-0026, ADR-0027, ADR-0034):** no check measures latency. Each HTTPS request in the Job has 10 s, each kubectl call 20 s, the Job 90 s (`activeDeadlineSeconds`) and the script waits up to 100 s for it; host stalls of up to ~0.8 s cannot fail a line by themselves, and the whole test stays within ADR-0075's 3 minutes. No retries.

## Consequences

- One command builds the dependencies of a production-shaped deployment on the laptop in under two and a half minutes from nothing (96, 133 and 102 s in the three measured runs; longer on a machine's very first run, while the nodes pull the third-party images), with HTTPS-only Keycloak and a password-protected Redis; the helm task only adds the chart and its smoke lines.
- Compose's config files serve both environments, so a change to one (a Redis setting, the realm, a Collector pipeline) reaches kind on the next `make kind-up`.
- kind needs compose stopped and about 5.75 GB; Grafana's larger limit is the one change to ADR-0075's plan.
- A Redis pod that restarts loses its node identity (emptyDir, as compose's tmpfs) and needs `make kind-down && make kind-up`.

## Alternatives considered

- **Keycloak in `start-dev` with HTTPS** — what compose runs, but development mode is not what Helm users get; production mode costs a build step at start (seconds) and nothing else here.
- **Redis password on the command line** (`--requirepass $(REDIS_PASSWORD)`) — simpler, but the value shows in the process list; the include file keeps it out.
- **Copies of compose's config files under `deploy/kind/`** — two files to keep in step for each service; ConfigMaps made from compose's files cannot drift.
- **Checking the host edge with an HTTPS request before the chart** — nothing listens behind the NodePort yet; Docker's port table checks the mapping itself, and the helm task adds the request.
- **Grafana at 256 Mi with `GOMEMLIMIT`** — tried; still OOM-killed on first start.
