# 0057. Admin UI: app, toolchain, embedding and serving

- **Status:** Accepted
- **Date:** 2026-11-15
- **Requirement IDs:** UI-05, UI-06, UI-09, CP-10, SEC-02, SEC-10

## Context

M6 turns the `web/` skeleton (ESLint 10.14.0, TypeScript 6.0.3, typescript-eslint 8.72.1, `src/main.ts`) into the admin UI. The user set D1 (React 19, TypeScript 6.x strict, Vite, TanStack Router and Query, pnpm, no component kit, plain CSS, one small chart library), D2 (openapi-typescript and openapi-fetch from `api/openapi.yaml`, generated code committed and checked for drift in CI) and D7 (`go:embed` of the build into the control plane, hashed assets immutable, `index.html` no-cache, SPA fallback, a strict CSP). ADR-0002 already puts the UI in the control-plane image with a Node stage before `go build`, and UI-09 serves it at `/`, same origin as `/api/v1`, with the Vite server proxying `/api` in dev.

Open: the app's layout and routes; every dependency and its version; how `make` and CI build, lint and test it; how `go build` and the Go tests work on a machine without Node; how the control plane routes `/`, `/api/v1` and the rest; the security headers and the exact CSP; how the browser learns the Keycloak issuer without a build-time value; how the typed client is generated, checked and used for errors and ETags.

Facts measured for this ADR on 2026-11-15:

- The repository has a `.dockerignore` that admits only `go.mod`, `go.sum`, `cmd/` and `internal/`, and the root `Dockerfile` builds every binary in one `build` stage.
- `.gitignore` already ignores `node_modules/`, `web/dist/`, `.vite/`, `playwright-report/` and `test-results/` (the last two match at any depth).
- The control plane's REST server has `WriteTimeout` and `ReadTimeout` of 30 s (cmd/controlplane/main.go); ADR-0059 deals with them for the stream.
- A trial install of the dependency set below into an empty workspace in the scratch directory (pnpm 12.16.1, the repository's overrides and `saveExact`) resolved 321 packages and `pnpm audit` reported **no known vulnerabilities**. `openapi-typescript` 7.13.0 generated 3,422 lines from today's `api/openapi.yaml` in 46 ms, and a client calling `GET /policies/{id}` and reading its `ETag` type-checked under TypeScript 6.0.3 with `strict` and `noUncheckedIndexedAccess`.

## Decision

### Dependencies (exact versions)

Rule: the newest release of each package published on or before 2026-11-01, two weeks before this ADR (the same rule as ADR-0035's library pins), checked against each other's peer ranges; every version is exact in `web/package.json` and recorded in `pnpm-lock.yaml`. TypeScript stays 6.0.3 (the Dependabot TypeScript 7 PR is not wanted).

| Package | Version | Kind | Why |
| --- | --- | --- | --- |
| `react`, `react-dom` | 19.3.1 | runtime | D1 |
| `@tanstack/react-router` | 1.170.46 | runtime | D1; code-based routes, below |
| `@tanstack/react-query` | 5.105.1 | runtime | D1, UI-05 server state |
| `openapi-fetch` | 0.17.0 | runtime | D2; 1 dependency, typed `GET`/`PUT`/… from the generated `paths` |
| `oauth4webapi` | 3.8.8 | runtime | OIDC code flow with PKCE (ADR-0058); 0 dependencies |
| `uplot` | 1.6.32 | runtime | the one chart library (below); 0 dependencies |
| `typescript` | 6.0.3 | dev | already pinned |
| `eslint`, `@eslint/js`, `typescript-eslint` | 10.14.0, 10.0.1, 8.72.1 | dev | already pinned |
| `eslint-plugin-react-hooks` | 7.1.1 | dev | rules of hooks and exhaustive deps |
| `@types/react`, `@types/react-dom` | 19.3.0 | dev | types for React 19 |
| `@types/node` | 26.6.6 | dev | types for the Node-side files (configs, scripts, e2e); matches `.node-version` 26 |
| `vite` | 8.3.5 | dev | D1, build and dev server |
| `@vitejs/plugin-react` | 6.1.2 | dev | JSX and fast refresh; peer `vite ^8` |
| `openapi-typescript` | 7.13.0 | dev | D2 generator |
| `yaml` | 2.9.1 | dev | 0 dependencies; reads `api/openapi.yaml` for the role table generator (ADR-0058) |
| `vitest`, `@vitest/coverage-v8` | 5.0.5 | dev | D10 (ADR-0061) |
| `jsdom` | 30.1.3 | dev | Vitest DOM environment (ADR-0061) |
| `@testing-library/react`, `@testing-library/dom`, `@testing-library/user-event`, `@testing-library/jest-dom` | 16.3.3, 10.4.2, 14.6.8, 7.0.1 | dev | D10 |
| `msw` | 3.0.3 | dev | D10, network mocked at the fetch boundary only |
| `@playwright/test` | 1.64.1 | dev | D10 (ADR-0061) |

- `openapi-typescript` 7.13.0 declares a peer range of `typescript ^5.x`. It works under 6.0.3 (measured above), so `pnpm-workspace.yaml` gets `peerDependencyRules.allowedVersions: { "openapi-typescript>typescript": "6.0.3" }` with a comment naming this ADR, instead of a warning nobody reads. The drift check (below) proves on every PR that generation still works.
- pnpm 12 lists `@tanstack/react-router` under "agent skills awaiting approval" after install; nothing is approved: the package's runtime code is what is used.
- No other runtime dependency: no component kit, no CSS framework, no date or state library, no React Query devtools (they would hold server responses, key secrets included, in a panel; ADR-0058).

**The chart library is uPlot 1.6.32.** About 50 KB minified with no dependencies (the npm package's 545 KB unpacked holds several builds), canvas-based so a 5 s refresh of a few hundred points per series redraws cheaply, and it sets element styles through the CSSOM (`el.style.x = …`), which a CSP without `'unsafe-inline'` allows; its stylesheet is imported into the bundle, so it is a same-origin file. Recharts (the design's Stack row names "Recharts or ECharts") is 7.5 MB unpacked with d3 dependencies and re-renders SVG through React on every refresh; ECharts is larger still. The Stack row's wording change is proposed in ADR-0056 (P13). If the user keeps the Stack row, Recharts 3.x works under the same CSP (React applies its `style` props through the CSSOM too) and replaces uPlot in the live-screens task.

### Layout under `web/`

```
web/
├── index.html                 # <div id="root">, one module script, one stylesheet; no inline script or style
├── public/favicon.svg         # served at /favicon.svg
├── package.json               # scripts: dev, build, gen, gen:check, lint, test, e2e
├── tsconfig.json              # the app and its unit tests (src/)
├── tsconfig.node.json         # vite.config.ts, playwright.config.ts, scripts/, e2e/
├── vite.config.ts             # build, dev proxy, Vitest settings
├── playwright.config.ts       # ADR-0061
├── eslint.config.js           # the existing config plus react-hooks; ignores generated files
├── scripts/gen-roles.mjs      # operation → roles table from x-spillway-roles (ADR-0058)
├── e2e/                       # Playwright specs, fixtures and the leak checks (ADR-0061)
└── src/
    ├── main.tsx               # loads /api/ui-config, starts the session, renders <App>
    ├── app/                   # router (code routes), root layout and navigation, error boundary
    ├── api/
    │   ├── schema.gen.ts      # openapi-typescript output (generated, committed)
    │   ├── roles.gen.ts       # operationId → roles (generated, committed)
    │   ├── client.ts          # openapi-fetch client over the authenticated fetch
    │   ├── errors.ts          # ApiError and the status → UI mapping (ADR-0060)
    │   └── <resource>.ts      # query and mutation hooks per resource
    ├── auth/                  # OIDC code flow, in-memory session, refresh, permissions (ADR-0058)
    ├── stream/                # the SSE client and useSubscription (ADR-0059)
    ├── components/            # QueryState (loading/empty/error), Pager, DataTable, Chart (uPlot),
    │                          # Dialog, ActionButton (disabled with a reason), DiffView, PublishDialog
    ├── screens/               # overview, throttling, policies, publish, routes, consumers,
    │                          # tenants, replicas, history, audit, signed-out, not-found, forbidden
    ├── styles.css             # the one stylesheet: CSS custom properties, layout, tables, states
    └── test/                  # Vitest setup and MSW handlers (ADR-0061)
```

Unit tests sit next to their code (`*.test.ts(x)`). One plain stylesheet (D1 allows modules or one stylesheet): the screens share few enough styles that modules would add a naming layer for nothing.

### Routes

**Code-based routes** (`createRootRoute`, `createRoute`, one `routeTree` in `src/app/router.tsx`). File-based routes need `@tanstack/router-plugin` and a generated `routeTree.gen.ts`, a second generator and a second drift check for about twenty routes; code routes give the same typed links, params and search params with no generator.

| Path | Screen | Notes |
| --- | --- | --- |
| `/` | redirect to `/overview` | |
| `/overview`, `/throttling` | live screens (ADR-0059) | search: `route`, `tenant`, `range` |
| `/policies`, `/policies/new`, `/policies/$id`, `/policies/$id/shadow` | list, create, edit, shadow report (ADR-0060) | search on the report: `window` |
| `/publish` | diff preview and publish (UI-02) | |
| `/routes`, `/routes/$id`, `/upstreams`, `/upstreams/$id` | Routes & upstreams | |
| `/consumers`, `/consumers/$id`, `/access-lists`, `/tenants` | Consumers & keys | `/tenants` admin only (ADR-0058) |
| `/replicas` | Replicas | |
| `/history`, `/history/$version`, `/audit` | History | `/audit` admin only |
| `/oidc/callback`, `/signed-out` | session (ADR-0058) | |
| anything else | Not found | |

List screens keep their cursor and filters in search params (validated with each route's `validateSearch`), so a link reproduces a page. Search params never hold a token, a key or a key's secret (ADR-0058).

### TypeScript and ESLint

- `tsconfig.json` (src): `target` and `lib` ES2023 plus `DOM` and `DOM.Iterable`; `module` `ESNext`, `moduleResolution` `Bundler`, `jsx` `react-jsx`; `strict`, `noUncheckedIndexedAccess`, `noImplicitOverride`, `noFallthroughCasesInSwitch`, `noUnusedLocals`, `noUnusedParameters`, `verbatimModuleSyntax`, `isolatedModules`, `noEmit`, `skipLibCheck`; `types` listed explicitly (TypeScript 6 defaults it to none): `vite/client`, `@testing-library/jest-dom/vitest`. `exactOptionalPropertyTypes` stays off: openapi-fetch's optional `params` and generated optional fields fight it, and `strict` plus `noUncheckedIndexedAccess` catch what matters here.
- `tsconfig.node.json`: the same strictness, `types: ["node"]`, for `vite.config.ts`, `playwright.config.ts`, `scripts/` and `e2e/`.
- `eslint.config.js` keeps `js.configs.recommended` and `tseslint.configs.strict`, adds eslint-plugin-react-hooks' recommended flat config and `no-console` as an error outside tests and `scripts/` (ADR-0058), and ignores `src/api/*.gen.ts`, `playwright-report/` and `test-results/`.
- The `lint` script runs `eslint .`, `tsc --noEmit -p tsconfig.json` and `tsc --noEmit -p tsconfig.node.json`.

### Make targets and CI

| Target | Runs | Needs |
| --- | --- | --- |
| `make web-gen` | `pnpm -C web gen`: `openapi-typescript ../api/openapi.yaml -o src/api/schema.gen.ts` and `node scripts/gen-roles.mjs` | `pnpm install` |
| `make lint-web` (part of `make lint`) | `pnpm -C web lint` and `pnpm -C web gen:check`: both generators into a temporary directory, compared with the committed files (`web/src/api/*.gen.ts is stale: run make web-gen`), as `make lint-api` does for Go | `pnpm install` |
| `make web-build` | `pnpm -C web build` (`vite build`) into `internal/controlplane/webui/dist` | `pnpm install` |
| `make test-go` | today's `make test`: Go unit, property and fuzz smoke | Go |
| `make test-web` | `pnpm -C web test` (`vitest run --coverage`) | `pnpm install` |
| `make test` | `test-go` and `test-web` | both |
| `make test-e2e` | `pnpm -C web e2e` (Playwright) against the compose stack; depends on `dev` like `test-integration` | the stack, Chromium (ADR-0061) |
| `make verify-m6` | `test/verify/m6.sh` (ADR-0061) | the stack |

`pnpm install` (from the root) is a one-time step after cloning, like `make tools`; the shell task adds it to docs/setup.md. CI: the `go` job runs `make test-go FUZZTIME=10s` instead of `make test` (it has no Node); the `web` job runs `pnpm install --frozen-lockfile`, `make lint-web`, `make test-web` and `pnpm audit`; the new `e2e` job is ADR-0061's. `ci-ok` needs every job, as now.

### The typed client (UI-05, D2)

- `web/src/api/schema.gen.ts` is generated by openapi-typescript 7.13.0 from `api/openapi.yaml` (default options; `paths`, `components` and `operations` exports) and committed. `make web-gen` regenerates it; `make lint-web` fails on drift, so a spec change without `make web-gen` fails the CI `web` job. openapi-typescript does not emit vendor extensions, which is why the role table has its own small generator (ADR-0058).
- `createClient<paths>({ baseUrl: "/api/v1", fetch: authFetch })` in `src/api/client.ts`. `authFetch` adds the bearer token and handles 401 (ADR-0058). Same origin, so no CORS and no credentials mode.
- **Errors.** Every error answer of the API is the `Error` schema (`error`, `code`, `request_id`, `details`). The client turns any non-2xx into one `ApiError { status, code, message, requestId, details, retryAfter }`, where `code` is typed from the schema's enum; a body that does not parse (a proxy's 502) becomes `ApiError` with status and `code: "unavailable"` for 502–504 or `"internal"` otherwise. Hooks throw `ApiError`; screens map it with ADR-0060's table. The `error` text is shown to the user: the server writes it for people and never echoes secrets (ADR-0036, ADR-0053).
- **ETags.** Every `GET` of an entity returns `{ data, etag }`: `etag` is `response.headers.get("ETag")` as sent (the quoted version, ADR-0036). Query keys hold both, so an edit form starts from a known version. Mutations that need it pass `params: { header: { "If-Match": etag } }`, which the spec declares, so a missing `If-Match` is a type error. `POST /config/publish`, `/config/versions/{v}/rollback` and `/config/import` take `If-Match: "<working_set_revision>"` from the validate answer the user reviewed (ADR-0060). The new `ETag` of a `PUT` answer replaces the cached one.

### Building the UI into the control plane (UI-09, D7)

- **Go package `internal/controlplane/webui`.** `webui.go` holds the handler (below) over an `fs.FS`. Two files choose the files:
  - `embed_dist.go`, `//go:build webui`: `//go:embed all:dist`, the Vite build;
  - `embed_placeholder.go`, `//go:build !webui`: `//go:embed placeholder`, a committed `placeholder/index.html` that says the binary was built without the UI and how to build it (`make web-build`, then `go build -tags webui ./cmd/controlplane`; images always include it). It has no script and no inline style.
  
  So `go build`, `go test`, `golangci-lint` and `make test-go` work on a machine without Node, and nothing they do depends on a build output. `internal/controlplane/webui/dist/` is gitignored and dockerignored; a stale local build can never leak into an image.
- **Vite** builds into `../internal/controlplane/webui/dist` (`build.outDir`, `emptyOutDir: true`), assets under `assets/` with content hashes in their names (Vite's default `[name]-[hash]`), `assetsInlineLimit: 0` (no `data:` URIs, so `img-src` stays `'self'`), `modulePreload.polyfill: false`, no source maps. `web/dist/` stays ignored but unused.
- **Dockerfile.** A new stage `web` on `$BUILDPLATFORM` (the output is platform-independent, so arm64 needs no emulation):
  ```dockerfile
  FROM --platform=$BUILDPLATFORM node:26.13.0-trixie-slim AS web
  WORKDIR /src
  RUN npm install -g pnpm@12.16.1
  COPY package.json pnpm-workspace.yaml pnpm-lock.yaml ./
  COPY web/package.json web/
  RUN --mount=type=cache,target=/root/.local/share/pnpm/store pnpm install --frozen-lockfile
  COPY web/ web/
  RUN pnpm -C web build
  ```
  `node:26.13.0-trixie-slim` matches `.node-version` (the tag exists, checked 2026-11-15); Node 25 and later ship without corepack, so pnpm is installed by npm at the `packageManager` version; Dependabot's docker ecosystem bumps the tag. The control plane leaves the shared `build` stage for its own stage `build-cp` (`FROM build`), which copies `--from=web /src/internal/controlplane/webui/dist` into `internal/controlplane/webui/dist` and builds `./cmd/controlplane` with `-tags webui`. The gateway, loadgen, accuracy and chaos images do not wait for Node. `.dockerignore` admits `package.json`, `pnpm-workspace.yaml`, `pnpm-lock.yaml` and `web/`, and excludes `web/node_modules`, `web/playwright-report`, `web/test-results` and `internal/controlplane/webui/dist`.
- **CI** builds the real UI in the `image` job (both architectures; the `web` stage runs once) and in every `make dev` (`--build`), so the integration and e2e jobs and `make verify-m6` run the embedded production build. A local `make dev` gets the UI the same way; the first build after this change takes a minute or two longer, then the layers are cached.
- **Dev.** `make dev`, then `pnpm -C web dev`: Vite on `127.0.0.1:5173` (`strictPort`) with `server.proxy: { "/api": "http://127.0.0.1:8080" }`, which also carries `/api/ui-config` and the stream. The Vite dev server serves its own `index.html` with its own inline HMR scripts, so the CSP below applies to the embedded build only; Playwright and `make verify-m6` test that build on 8080.

### Serving at `/` (UI-09, D7)

The control plane's 8080 listener gets one top-level mux in `cmd/controlplane` (the admin listener on 8081 keeps `/healthz`, `/readyz`, `/metrics`, `pprof`; none of them exist on 8080):

| Request | Answer |
| --- | --- |
| `/api/v1/…` | the REST API as today (ADR-0043's guard first; unknown paths 404 JSON) |
| `GET /api/ui-config` | runtime config (below); `Cache-Control: no-store` |
| any other `/api` or `/api/…` | 404 JSON `{"error":"no such path","code":"not_found",…}` |
| `GET`/`HEAD /assets/<file>` that exists | the file, `Cache-Control: public, max-age=31536000, immutable` |
| `GET`/`HEAD /assets/…` that does not exist | 404, plain text, never `index.html` (a missing script must not come back as HTML) |
| `GET`/`HEAD` of another file at the build's root (`/favicon.svg`, `/index.html`) | the file, `Cache-Control: no-cache` |
| any other `GET`/`HEAD` path | `index.html`, 200, `Cache-Control: no-cache` (the router shows Not found for unknown routes) |
| any other method on a non-API path | 405 with `Allow: GET, HEAD` |

- Files are served with `http.ServeContent` from the embedded FS: `Content-Type` from the extension, a strong `ETag` (SHA-256 prefix computed at start) so `no-cache` revalidates to 304, no directory listings, and paths checked with `fs.ValidPath` (Go's mux already cleans `..`).
- **Headers on every response of this handler** (UI files, the placeholder, `/api/ui-config`, its 404s and 405s):
  - `Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; connect-src 'self' <issuer-origin>; form-action 'self' <issuer-origin>; frame-src 'none'; frame-ancestors 'none'; object-src 'none'; base-uri 'none'; worker-src 'none'; manifest-src 'self'`
  - `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`, `Cross-Origin-Opener-Policy: same-origin`, `Cross-Origin-Resource-Policy: same-origin`,
  - `Permissions-Policy: accelerometer=(), camera=(), geolocation=(), gyroscope=(), magnetometer=(), microphone=(), payment=(), usb=()`.
- **The CSP, line by line.** `<issuer-origin>` is the scheme, host and port of `SPILLWAY_CP_OIDC_ISSUER` (the browser-facing issuer, ADR-0040; `http://localhost:8180` in compose), computed once at start. `connect-src` needs it for discovery, the token endpoint and refresh (ADR-0058); `form-action` needs it because logout is a form POST to Keycloak's end-session endpoint, and `'self'` because Chromium checks `form-action` on that POST's redirect back to `/signed-out`. `script-src 'self'` with no inline scripts and no `'unsafe-eval'`; `style-src 'self'` with no `'unsafe-inline'`: the app has no `<style>` elements or `style` attributes in markup, React and uPlot set styles through the CSSOM, which the CSP does not restrict. `img-src 'self'` (no `data:`: `assetsInlineLimit: 0`). Navigations to Keycloak's login are top-level and not governed by the CSP. A CSP violation shows as a browser console error, which every Playwright flow fails on (ADR-0061), so a regression cannot pass unnoticed.
- **HSTS** is not sent: this listener is plain HTTP in compose (on loopback, ADR-0040), and HSTS only means something behind TLS. When a TLS front (M8's demo proxy, M7's Helm) serves the UI, that front sets it.
- **API responses** (`/api/…`) get `X-Content-Type-Options: nosniff` and `Cache-Control: no-store` from the same top-level wrapper, so no API answer (a key listing, the audit log) lands in the browser's disk cache. Answers that already set `Cache-Control` (key issue, ADR-0039) keep theirs.

### Runtime config

`GET /api/ui-config` answers `{"issuer": "<SPILLWAY_CP_OIDC_ISSUER>", "client_id": "admin-ui"}`: public values, no token, no authentication (the UI needs them before it has a token). It lives under `/api` so the one Vite proxy rule carries it in dev, and outside `/api/v1` because it is not an operation of the admin API (no RBAC, no OpenAPI entry; the UI types it by hand). Nothing is baked into the build: the same image serves any issuer, and the issuer origin in the CSP comes from the same setting, so the two cannot disagree. The client ID is a constant (ADR-0058 keeps `admin-ui`); a setting can be added when a deployment needs another.

## Consequences

- `go build` and every Go test keep working without Node; only images and `make web-build` + `-tags webui` carry the real UI. A binary built without the tag says so at `/` instead of failing.
- A spec change now needs both `make api` and `make web-gen`; CI fails the `go` or `web` job when either is forgotten.
- `make test` and `make lint` need `pnpm install` once; CI splits them by job so the `go` job needs no Node.
- `make dev` builds a Node stage the first time; later runs hit the layer cache.
- No inline scripts or styles can ever be added without changing the CSP, which a reviewer sees.
- Follow-up: the shell task implements all of this; the live-screens task adds uPlot's usage.

## Alternatives considered

- **Vite output in `web/dist` with a committed placeholder `web/dist/index.html`** — every local build overwrites a tracked file and dirties the tree; and the Go package would have to live in `web/`, next to `node_modules`.
- **Build tags vs a runtime check for an empty `dist`** — an untagged embed of a gitignored directory fails `go build` on a fresh clone.
- **File-based TanStack routes** — a plugin and a generated route tree for about twenty routes.
- **Recharts or ECharts** — the design's Stack row; both far larger for a few line charts (P13).
- **The issuer in a `<meta>` tag templated into `index.html`** — the Vite dev server serves its own `index.html` without it, and templating makes the embedded file differ from the build.
- **`/ui-config.json` at the root** — needs a second Vite proxy rule; the design names only `/api`.
- **CSS modules** — allowed by D1; a naming layer the app's few shared styles do not need.
- **happy-dom** — ADR-0061.
