# 0002. Repository layout: one Go module, root compose file, embedded UI

- **Status:** Accepted
- **Date:** 2026-11-02
- **Requirement IDs:** UI-09

## Context

The repository holds four Go binaries (`gateway`, `controlplane`, `loadgen`, `demo-traffic`) that share packages under `internal/` and ship together, a React admin UI under `web/`, and the local stack in Docker Compose. CLAUDE.md and docs/setup.md use `docker compose down -v` from the repository root as the full reset. Tool versions must be identical on every machine and in CI.

## Decision

- **One Go module** `github.com/ankitbisht23/spillway` at the root, with `go` and `toolchain` directives pinned in `go.mod` (the `toolchain` line is a minimum under `GOTOOLCHAIN=auto`, so CI and the Makefile set `GOTOOLCHAIN` to the exact version). No `go.work`.
- **pnpm workspace** for `web/`, with `packageManager: pnpm@<exact>`, `.npmrc` `save-exact=true` and `.node-version`.
- **`compose.yaml` at the root**; per-service config under `deploy/compose/<service>/` (e.g. `deploy/compose/keycloak/realm.json`).
- **Pinned project tools** (sqlc, goose, golangci-lint, buf, gitleaks, oasdiff, govulncheck, protoc-gen-go, protoc-gen-go-grpc, oapi-codegen) installed by `make tools` into `./bin`, which is gitignored; the Makefile calls them from there.
- **UI embedded in the control plane** (UI-09): the production build is compiled into the control-plane binary and served at `/`, same origin as `/api/v1`. Two images: `spillway-gateway` and `spillway-controlplane`.

## Consequences

- One `go.mod` means one dependency graph and one CI job for Go; a dependency upgrade lands in every binary at once, which matches how they ship. Split a package into its own module only if it needs its own release cycle.
- Every compose command works from the root without `-f`.
- The control-plane image build needs a Node stage to build the UI before `go build`. No CORS configuration; in dev the Vite server proxies `/api` to the control plane.
- `make tools` must run once after cloning (part of the M0 checklist).

## Alternatives considered

- **`go.work` with a module per binary** — several `go.mod` files to keep in step and per-module CI, for independent releases nothing needs.
- **Compose file under `deploy/compose/`** — every command needs `-f deploy/compose/compose.yaml`, and `docker compose down -v` from the root finds nothing.
- **Separate UI image** (static server) — a third image to build, sign and deploy, plus CORS or ingress routing between UI and API.
