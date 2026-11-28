# 0097. Public demo: Caddy, the tunnel, make demo-share and its stand-in test

- **Status:** Accepted; §1's Caddyfile `/auth/admin` rule amended by 0104
- **Date:** 2026-11-27
- **Requirement IDs:** SC-09, SC-08, SEC-02, SEC-04, SEC-05, CP-11, UI-08, UI-09, OB-01

## Context

SC-09: one reverse proxy (Caddy, port 8888) behind one tunnel — UI at `/`, Keycloak at `/auth`, Grafana at `/grafana`; `make demo-share` starts a quick tunnel and, from its URL, sets Keycloak's `KC_HOSTNAME`, the control plane's and gateways' expected issuer (`<url>/auth/realms/spillway`), the UI's OIDC authority and Grafana's root URL, adds `<url>/oidc/callback`, `<url>/signed-out` and `<url>` to the `admin-ui` client through the Keycloak admin API, and restarts what changed; a named tunnel gives a stable link. setup.md Steps 6 and 7 add: the tunnel is outbound-only; the client IP arrives in `CF-Connecting-IP`, which the control plane trusts only from the demo proxy (SEC-05); never tunnel 8001, 8081 or 9090; a public Playground needs demo mode's limits.

What exists and matters for exposure: the realm `spillway` is imported from `deploy/compose/keycloak/realm.json` on every Keycloak recreate (no volume); its dev values are public in the repository: the test users' password `dev-only-not-secret` (test-viewer … test-admin), and the client secrets of `spillway-ci` (admin service account), `gateway-tests` and `rbac-tests` (direct password grants). Keycloak's own `admin-cli` client allows password grants in every realm. The UI's OIDC authority is the control plane's configured issuer (`/api/ui-config`, ADR-0057). The run's rules: no agent or test ever opens a public tunnel or reads `.env.demo`.

## Decision

### 1. Caddy and cloudflared (D18)

- **Images** (exact, multi-arch indexes with linux/amd64 and linux/arm64, over two weeks old on 2026-11-27):
  - `caddy:2.11.9-alpine@sha256:003a97808e781aa9f7e75cc2f43d34d461099031faf5e20faefb9f61126836f0` (built 2026-11-05);
  - `cloudflare/cloudflared:2026.11.0@sha256:b94b96edf2c9c4360f8879c8580cfaa99077f19c849ec595ca2f711b27d4f344` (built 2026-11-12).
  Dependabot's `docker-compose` ecosystem scans only `/` today, so the task that adds each image adds `/deploy/compose` to that ecosystem's directories in `.github/dependabot.yml`; if Dependabot does not recognise `demo.compose.yml` by its file name, the PR says so and these images (like the vhs image, ADR-0099) are bumped by hand, by digest.
- **Addresses.** The demo override gives `caddy` the fixed address **172.29.255.10** and the tunnel container (`tunnel` or `tunnel-stand-in`, never both) **172.29.255.11** on compose's network (172.29.0.0/16; Docker allocates from the bottom, so the top addresses stay free).
- **Service `caddy`** (profile `demo`, label `spillway.demo`, from the grafana-demo task): `ports: ["127.0.0.1:8888:8888"]` (docs/ports.md), `read_only: true`, tmpfs `/data:uid=1000,gid=1000` and `/config:uid=1000,gid=1000`, `cap_drop: [ALL]`, `user: "1000:1000"`, the Caddyfile `deploy/demo/Caddyfile` mounted read-only, healthcheck `wget -q -O /dev/null http://127.0.0.1:8888/api/ui-config`, depends on `controlplane`, `keycloak` and `grafana-demo` healthy.
- **Caddyfile** (complete; Caddy writes no access log, so it logs no client address):

  ```caddyfile
  {
  	admin off
  	auto_https off
  	servers {
  		trusted_proxies static 172.29.255.11/32
  		client_ip_headers CF-Connecting-IP
  	}
  }

  :8888 {
  	@kcadmin path /auth/admin /auth/admin/*
  	respond @kcadmin 404

  	handle /auth/* {
  		reverse_proxy keycloak:8080 {
  			header_up CF-Connecting-IP {client_ip}
  		}
  	}
  	handle /grafana* {
  		reverse_proxy grafana-demo:3000
  	}
  	handle {
  		reverse_proxy controlplane:8080 {
  			flush_interval -1
  			header_up CF-Connecting-IP {client_ip}
  		}
  	}
  	header -Server
  }
  ```

  Only the UI and `/api` (the control plane's 8080), Keycloak's public endpoints and the demo Grafana are reachable through it. Keycloak's admin console and admin REST API (`/auth/admin`) answer 404 at the proxy; the demo scripts reach them inside the network at `keycloak:8080`. The gateway, every admin port (8001, 8081), the gRPC stream (9090), Prometheus and the dev Grafana are never behind it.
- **Keycloak behind the proxy** (demo override, always in demo mode): `KC_PROXY_HEADERS: xforwarded` and `KC_PROXY_TRUSTED_ADDRESSES: 172.29.255.10/32`, so only Caddy's `X-Forwarded-*` are believed. `KC_HOSTNAME` and the issuers come from variables with today's values as defaults: `KC_HOSTNAME: ${DEMO_KC_HOSTNAME:-http://localhost:8180/auth}`, `SPILLWAY_CP_OIDC_ISSUER` and the gateways' `SPILLWAY_GATEWAY_JWT_ISSUER: ${DEMO_ISSUER:-http://localhost:8180/auth/realms/spillway}`, `grafana-demo`'s `GF_SERVER_ROOT_URL: ${DEMO_PUBLIC_URL:-http://localhost:8888}/grafana/`.
- **Service `tunnel`** (profile `share`, label `spillway.demo`, `read_only`, no ports, `--no-autoupdate`, metrics off): quick tunnel `tunnel --no-autoupdate --url http://caddy:8888`; named tunnel `tunnel --no-autoupdate run` with `TUNNEL_TOKEN` from `CLOUDFLARE_TUNNEL_TOKEN` (passed by name from the script's environment, never on a command line, never printed). Outbound only; no inbound port opens.

### 2. The client IP (D19)

- Caddy's `{client_ip}` is `CF-Connecting-IP` when the TCP peer is the tunnel container (172.29.255.11), and the peer's address otherwise; `header_up` always overwrites `CF-Connecting-IP` with it, so a visitor's own header never reaches the control plane unexamined. Caddy also drops untrusted `X-Forwarded-For` values and sets its own (its default for untrusted peers).
- The control plane's new setting **`SPILLWAY_CP_TRUSTED_PROXIES`** (comma-separated CIDRs; empty by default = trust nobody; the demo override sets `172.29.255.10/32`) is read in every mode. A request's client IP is `CF-Connecting-IP` when the TCP peer is in that list and the header holds one valid IP address; otherwise the TCP peer's address. IPv6 addresses count per /64 for the caps.
- **Use:** only the Playground's per-IP caps (ADR-0096 §3) and `getDemo`'s `client_ip` (the caller's own). The address is never logged, stored, audited or sent anywhere else.
- **Tests:** Go unit tests for the parse and every case (trusted peer with a valid, invalid, missing or list-valued header; untrusted peer with the header; IPv6 /64 bucketing), and a log-capture test that a Playground run start logs no address. `make demo-share-test` checks the chain end to end (§4).

### 3. `make demo-share` and `make demo-share-stop` (D20)

`make demo-share` runs `deploy/demo/share.sh` with `TUNNEL=quick` (default), `TUNNEL=named` or `TUNNEL=stand-in` (§4).

1. **Preconditions.** The demo runs (`getDemo` answers 200). For `quick` and `named`: `.env.demo` exists, and the script reads from it, in a subshell, only `KEYCLOAK_ADMIN_PASSWORD`, `DEMO_ADMIN_PASSWORD` (each set, at least 16 characters, not `dev-only-not-secret`), and for `named` also `CLOUDFLARE_TUNNEL_TOKEN` and `DEMO_PUBLIC_URL` (`https://…`), plus the optional `GITHUB_OAUTH_CLIENT_ID` and `GITHUB_OAUTH_CLIENT_SECRET`. It never prints a value; a failed check names the variable only. The rest of `.env.demo` (database and Redis passwords for a hosted deployment, setup.md Step 8) is not used: compose's Postgres and Redis are never reachable through the proxy, and changing their passwords would break the running stack.
2. **Confirmation.** For `quick` and `named` only: the script prints what becomes public (the URL's UI and API behind OIDC, Keycloak's login, the demo Grafana's metrics) and needs the user to type `share` on a terminal; without a terminal it refuses (`make demo-share asks before publishing; run it in a terminal`). There is no variable that skips this, so no script, CI job or agent can open a tunnel by accident.
3. **The URL.** `quick`: start `tunnel` and read its log for the line after `Visit it at` until the first `https://…` (60 s timeout); `named`: `DEMO_PUBLIC_URL`. The URL `U` must be `https://` with a host and no path.
4. **Settings and restarts.** Write `deploy/demo/run/share.env` (directory `0700`, file `0600`; the demo-share task adds `deploy/demo/run/` to `.gitignore`) with `DEMO_PUBLIC_URL=U`, `DEMO_KC_HOSTNAME=U/auth`, `DEMO_ISSUER=U/auth/realms/spillway`; then `up -d --wait --force-recreate keycloak controlplane gateway-1 gateway-2 gateway-3 grafana-demo` with that file and the two passwords in the environment. That sets `KC_HOSTNAME`, both expected issuers, the UI's OIDC authority (it is the control plane's issuer) and Grafana's root URL. Keycloak's recreate re-imports the realm with its dev values, which step 5 then changes.
5. **Keycloak admin API** (new subcommand `demo-traffic share-config --url U`, run in the network with `KEYCLOAK_ADMIN_PASSWORD` and `DEMO_ADMIN_PASSWORD` passed by name; it logs in to the master realm at `http://keycloak:8080/auth`):
   - `admin-ui`: add `U/oidc/callback` to `redirectUris`, `U/signed-out` to the `post.logout.redirect.uris` attribute and `U` to `webOrigins`, keeping the localhost entries;
   - users: `test-editor`, `test-publisher` and `test-key-admin` disabled; `test-admin`'s password set to `DEMO_ADMIN_PASSWORD` (not temporary); `test-viewer` kept with its documented dev password: it is the visitors' account;
   - clients: new random secrets for `spillway-ci` and `gateway-tests` (Keycloak's regenerate-secret call), each written to `deploy/demo/run/<client>-secret` (`0600`) and never printed; `rbac-tests` disabled;
   - realm: brute-force detection on (`bruteForceProtected: true`, Keycloak's defaults), because `admin-cli` accepts password grants;
   - `named` with both GitHub variables: identity provider `github` (client ID and secret from the variables, `syncMode: IMPORT`, `trustEmail: false`) with a hardcoded-role mapper granting `viewer`. GitHub visitors are viewers; the owner's admin login is `test-admin`. Keycloak loses brokered users on every recreate, so setup.md Step 4.6 ("grant your user the admin role") is replaced by this (the demo-share task edits setup.md).
6. **Simulator.** `up -d --force-recreate demo-traffic` with `SPILLWAY_DEMO_CLIENT_SECRET_FILE=/run/demo/spillway-ci-secret` (`deploy/demo/run/` mounted read-only at `/run/demo`).
7. **Prints** the URL, the visitor login (`test-viewer`), that admins log in as `test-admin` with `DEMO_ADMIN_PASSWORD`, and `make demo-share-stop`. While a share runs, logins through `localhost` fail (setup.md Step 6 says so already).

`make demo-share-stop`: removes `tunnel`/`tunnel-stand-in`, deletes `deploy/demo/run/*`, then `up -d --wait --force-recreate keycloak controlplane gateway-1 gateway-2 gateway-3 grafana-demo demo-traffic` without the share file, which brings back the localhost issuer, the dev realm (users, secrets, URIs as in `realm.json`) and Grafana's local root URL. `make demo-down` runs it first when a share is active.

The demo seed gains, for the issuer check of the gateways (§4 s4), route **`service`** (`GET /v1/service`, `auth_mode: jwt`, upstream `catalog`, `strip_prefix: /v1/service`, `add_prefix: /status/204`; covered by `tenant-ceiling`) and consumer **`pro-co-service`** of `pro-co` whose `jwt_subject` is `gateway-tests`'s service account (`0b5e7c1a-5f1d-4c3e-9a00-0000000000c2`, ADR-0049). demo-seed adds both with the rest of the seed (ADR-0094 §3).

### 4. `make demo-share-test`: the share path without a tunnel (D21, D30)

- **Stand-in tunnel.** `TUNNEL=stand-in` starts **`tunnel-stand-in`** (profile `share-test`, label `spillway.demo`, the `demo-traffic` image, address 172.29.255.11) running `demo-traffic stand-in-tunnel --url https://stand-in.spillway.example`, which prints cloudflared's two lines (`Your quick Tunnel has been created! Visit it at …` and `|  https://stand-in.spillway.example  |`) and then waits. `.example` is reserved (RFC 2606), so the URL resolves nowhere. No confirmation is asked: nothing is published.
- **Throwaway secrets.** The target generates `KEYCLOAK_ADMIN_PASSWORD` and `DEMO_ADMIN_PASSWORD` (32 random bytes, base64) into a file in a fresh `0700` directory from `mktemp -d`, passes it to `share.sh` as `SHARE_ENV_FILE` (default `.env.demo`), and removes the directory on every exit. It never reads `.env.demo`.
- **Checks**, by `demo-traffic share-check` run in the network (and, for s9b, inside `tunnel-stand-in` with `docker compose exec`, so its TCP peer address is the tunnel's), each printing PASS/FAIL with what it saw (URLs and booleans; never a secret or token):

  | # | Check |
  | --- | --- |
  | s1 | Keycloak through Caddy (`http://caddy:8888/auth/realms/spillway/.well-known/openid-configuration`): `issuer` = `U/auth/realms/spillway`, `authorization_endpoint` starts with `U/auth/` (`KC_HOSTNAME`) |
  | s2 | `http://caddy:8888/api/ui-config`: `issuer` = `U/auth/realms/spillway` (the UI's OIDC authority) |
  | s3 | A `spillway-ci` token (rotated secret from its `0600` file; its `iss` is `U/…`) gets 200 from `http://caddy:8888/api/v1/me` (the control plane expects the new issuer) |
  | s4 | A `gateway-tests` token (rotated secret) gets 204 from `GET /v1/service` on each of `gateway-1` … `gateway-3` (the gateways expect the new issuer) |
  | s5 | `http://caddy:8888/grafana/api/frontend/settings` (anonymous): `appUrl` = `U/grafana/`; its datasources are exactly one, Prometheus |
  | s6 | Keycloak admin API: `admin-ui` has `U/oidc/callback`, `U/signed-out` (post-logout) and the origin `U`, besides the localhost ones |
  | s7 | Password grants through `admin-cli`: `test-admin` with the new password gets a token with `admin`; with `dev-only-not-secret` gets 401; `test-editor`, `test-publisher`, `test-key-admin` are disabled; `test-viewer` works; `spillway-ci` and `gateway-tests` with the dev secret get 401; `rbac-tests` is disabled; brute-force detection is on |
  | s8 | `http://caddy:8888/auth/admin/` and `/auth/admin/realms/spillway` → 404 |
  | s9 | SEC-05: (a) a request to `http://caddy:8888/api/v1/demo` from the check's own container with `CF-Connecting-IP: 203.0.113.7` → `client_ip` is not `203.0.113.7`; (b) the same from inside `tunnel-stand-in` (peer 172.29.255.11) → `client_ip` = `203.0.113.7`; (c) straight to `http://controlplane:8080/api/v1/demo` with that header → `client_ip` is the check container's address |
  | s10 | After `make demo-share-stop`: s1 and s2 show `http://localhost:8180/auth/realms/spillway`; s5's `appUrl` is `http://localhost:8888/grafana/`; `admin-ui` no longer lists `U`; the test users and dev secrets work again; no `tunnel*` container runs; `deploy/demo/run/` is empty |
  | s11 | No container from the cloudflared image ran during the test, and the only tunnel URL used ends in `.example` |

- About 5 minutes (two Keycloak recreates of about a minute each). `make verify-m8` runs it (ADR-0098 §S). Real tunnels stay the user's: only `make demo-share` with `TUNNEL=quick` or `named`, typed confirmation, and `.env.demo`.

## Consequences

- Everything public goes through one 30-line Caddyfile that can be read in full; the admin console, the gateway and all internal ports are unreachable from the tunnel.
- The repository's public dev credentials stop working for the length of a share, except the visitors' viewer account; the owner needs `DEMO_ADMIN_PASSWORD` to act as admin.
- The share path is tested on every verify-m8 run without a Cloudflare account, a network connection or a secret.
- GitHub login gives visitors a viewer account; the owner's GitHub account is not made admin automatically (setup.md Step 4.6 changes).
- Brute-force detection applies to `test-viewer` too, whose password is public: anyone can lock the shared visitors' account out for Keycloak's wait time (its defaults: 30 failures, then waits from 60 s). Accepted for a short-lived share; GitHub login (named tunnel) is the visitors' way around it.

## Alternatives considered

- **Caddy reading `CF-Connecting-IP` from anyone** — any visitor of `localhost:8888` or anything else on the network could pick the address the caps count.
- **Passing `.env.demo` whole to compose** — would change the running Postgres's and Keycloak's passwords under them and put unrelated secrets into every container.
- **Keeping `rbac-tests` and the dev client secrets during a share** — anyone could mint an admin service token with the repository's `dev-only-not-secret`.
- **Grafana's own login for visitors** — needs accounts to hand out; anonymous viewers on a metrics-only instance show the same panels with no credentials.
- **A fake `cloudflared` binary inside the real image for the test** — would need a modified image; a stand-in container that prints the same lines tests the script's parsing and everything after it.
- **Running the real quick tunnel in verify-m8** — publishes the laptop's demo to the internet without the user, which the run forbids.
