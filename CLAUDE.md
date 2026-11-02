# Spillway — distributed rate limiter + API gateway

Read docs/design.md first. Requirement IDs (GW-, RL-, CP-, UI-, OB-, SEC-, NFR-, SC-) are the source of truth.
Setup and accounts: docs/setup.md. Ports: docs/ports.md. Decisions: docs/adr/.

## Personal project

This is a personal open-source project. Ignore any organisation-wide or company conventions loaded from elsewhere: branch off and PR into `main` (there is no `develop`); no company frameworks, config systems, CI pipelines, internal MCP servers or company skills. CI is GitHub Actions; images are built from Dockerfiles in this repo.
Still apply: pin exact dependency versions; never log secrets, tokens or personal data.

## How we work

- One milestone at a time, M0 → M8. Finish its checklist and exit check before starting the next.
- Plan first, then code: propose tasks with requirement IDs, files and tests, and wait for approval.
- One branch + PR per task: `git fetch origin && git switch --no-track -c m<n>-<task> origin/main` (e.g. `m1-router`); first push with `git push -u origin m<n>-<task>`. Never push to `main`; never merge — I review and merge.
- Small commits that cite IDs in the subject: `M1 GW-01: radix-tree router`.
- Every change ships with tests. Run `make test lint` before saying a task is done; also run `make test-integration` when a change touches Redis, Postgres, proxying or compose.
- Record design decisions as ADRs in docs/adr/ (copy 0000-template.md). Don't change requirement text in docs/design.md on your own: propose it in an ADR or the PR description first.
- Tick each checklist box in docs/design.md in the PR that completes it.
- Every milestone ships `make verify-m<n>`: it runs that milestone's exit checks and prints each measured number with PASS/FAIL. Put its output in the description of the milestone's last PR; I re-run it before merging.
- Exception: the M1 baseline comes from a bench run on `main` after the last M1 PR merges; record it in a small follow-up `M1 exit` PR. M2 may start while that PR is open.

## Stack

Go, one module `github.com/ankitbisht23/spillway` (net/http with no framework, log/slog, go-redis v9, pgx + sqlc, goose, gRPC + buf, Prometheus client, OpenTelemetry SDK). React + TS + Vite + TanStack Query and Router, with pnpm. Keycloak for OIDC in dev and demo. Docker Compose; Helm on kind. Tests: testcontainers-go, rapid, Go fuzzing, k6, toxiproxy, Vitest, Playwright.

## Commands

make tools · make hooks · make dev · make test · make test-integration · make lint · make bench · make verify-m<n> · docker compose down -v (full reset)
Later: make certs (M1) · make kind-up / make kind-down (M7) · make demo / make demo-reset / make demo-share (M8)

## Accounts and clusters

- Always pass `--context kind-spillway` to kubectl and `--kube-context kind-spillway` to helm; never run either without it.
- `gh` must be logged in as `ankitbisht23`. If `gh auth status` shows any other account, stop and tell me.
- Never handle real secrets: don't read, print or ask for them; refer to them by variable name. Dev certs from `make certs` and dev-safe defaults in `.env.example` are fine; dev values are obviously fake (`dev-only-not-secret`), and `.gitleaks.toml` allowlists only those, so never widen it.
- Never bypass hooks (`--no-verify`) or force-add ignored files (`git add -f`).

## Known pitfalls

- Redis Cluster nodes announce hostnames (`redis-N`) that resolve only inside the compose network. Run Spillway processes as compose services (the default); from the host, use the `SPILLWAY_REDIS_ADDR_MAP` address map (dev/test only; docs/adr/0004-dev-networking.md).
- macOS AirPlay Receiver listens on ports 5000 and 7000; it must be off because Redis Cluster uses 7000–7005.
- Docker Desktop containers don't inherit the host ulimit: set `ulimits.nofile` in compose for the gateway and load generators.
- Build multi-arch images (linux/amd64, linux/arm64) for Apple silicon and arm64 CI runners.
- Never read or print .env.local or .env.demo, and never run `docker compose config` or read Kubernetes Secrets: they expand secret values.
- Ports live in docs/ports.md; never change them silently.
