# 0006. Dev containers: healthchecks, mock upstreams, image layout

- **Status:** Accepted
- **Date:** 2026-11-03
- **Requirement IDs:** SEC-10, SC-03, CP-19

## Context

M0 asks for a healthcheck on every long-running compose service, one-shot services that exit 0 under `make dev` (`docker compose up --wait`), mock upstreams, and a multi-arch image that runs as non-root with a read-only root filesystem (docs/design.md, M0; ADR-0004). Building it turned up gaps the design does not settle:

- The pinned OTel Collector, Tempo, Loki and go-httpbin images ship without a shell or an HTTP client, so a compose healthcheck has nothing to run.
- `docker compose up --wait` (Compose 5.6) fails on a one-shot container that has already exited 0, unless another service depends on it with `service_completed_successfully`. Without that, `make dev` on a running stack fails at random.
- The design names no mock upstream image, no Dockerfile location and no base image.
- pnpm 12 no longer reads `save-exact` from `.npmrc` (ADR-0002 put it there); it saved `^` ranges.
- The M0 checklist lists a `migrate` one-shot, but `controlplane migrate` arrives with the control plane in M5 (CP-19).

## Decision

- **Healthchecks in shell-less images.** A two-line Dockerfile per image under `deploy/compose/<service>/` copies a static busybox (`busybox:<exact>-musl`) to `/busybox`; the healthcheck runs `/busybox wget` against the service's own ready endpoint. Images that have a shell or a client keep it: `redis-cli`, `pg_isready`, `wget` in Prometheus and Grafana, bash `/dev/tcp` in Keycloak, `toxiproxy-cli`.
- **One-shot services** carry the label `spillway.oneshot: "true"`, which `make verify-m0` uses to tell them from a stopped long-running service. Each has a long-running dependant with `condition: service_completed_successfully`: `toxiproxy` for `redis-init` (it fronts the Redis nodes for fault injection, SC-03), and the gateway from M1. `redis-init` exits 0 at once when the cluster already exists.
- **`migrate`** joins compose with the control plane in M5 (CP-19); M0 has no one-shot besides `redis-init`.
- **Mock upstreams** are three `mccutchen/go-httpbin` containers (`upstream-1` … `upstream-3`, ports 9101–9103): `/bytes/1024` for benchmark bodies, `/status/503` and `/delay/<s>` for faults. An endpoint-level fault switch, if M4 needs one, goes through toxiproxy.
- **Images** are built from one root `Dockerfile` with a target per binary (`gateway` now, `controlplane` in M5). The build stage runs on `$BUILDPLATFORM` and cross-compiles, so arm64 needs no emulation; the final stage is `gcr.io/distroless/static-debian13:nonroot`, pinned by digest because distroless publishes no version tags, running as `65532:65532`.
- **pnpm settings** live in `pnpm-workspace.yaml` (`saveExact: true`, overrides), replacing ADR-0002's `.npmrc` line; there is no `.npmrc`.

## Consequences

- Four small local builds in `make dev`; they are cached after the first run and Dependabot's docker ecosystem covers their base images.
- Every new one-shot needs the label and a dependant, or `make dev` and `make verify-m0` fail; that is the point.
- go-httpbin answers per request, not per instance, so "one endpoint of three failing" (M4 exit) needs toxiproxy in front of that upstream.
- The distroless digest has to be bumped by Dependabot or by hand; a tag would not change underneath us either way.
- docs/design.md still names `.npmrc` in the M0 checklist and repo layout; the M0 PR proposes updating that text.

## Alternatives considered

- **No healthcheck for shell-less images** — breaks the M0 rule and lets `--wait` pass on a service that is up but not ready.
- **A Spillway healthcheck binary mounted into containers** — more code than a busybox copy, and a binary built for the host would not run in the Linux containers on macOS.
- **Running one-shots in a second `make dev` step** — `up` restarts exited one-shots, so the second `--wait` hits the same failure.
- **Writing our own mock upstream now** — go-httpbin covers M1–M4 needs; write one only if a test needs behaviour it lacks.
- **`scratch` base image** — needs CA certificates, `/etc/passwd` and a non-root user added by hand, which distroless already provides.
