# 0042. The M5 exit check

- **Status:** Accepted
- **Date:** 2026-11-13
- **Requirement IDs:** NFR-08, NFR-09, NFR-12, CP-07, CP-08, CP-09, CP-17, CP-20, CP-22

## Context

M5's exit reads: "a publish reaches all replicas in < 5 s (NFR-08); a revoked key is rejected on all replicas in < 5 s (NFR-09); gateways serve unchanged through a 10-minute control-plane outage (NFR-12)". The NFR table says NFR-08 is measured from "the immediate heartbeat each replica sends on apply", NFR-09 by an integration test on the identity path, and NFR-12 by a chaos test with "zero failed requests; config frozen at last good"; the chaos table injects the outage with `docker stop` and expects the staleness alert to fire. The user set D10: at least 50 publishes per run, p99 of (publish committed → last replica's apply heartbeat), the replica count fixed here, default 5 gateways under `--profile replicas`. None of these says how each time is taken, on which clock, what exactly is stopped, or what "frozen" is checked against.

This laptop stalls: every process, for 4–9 ms about every 15 s under load, and in bursts up to 788 ms (ADR-0026, ADR-0027, ADR-0034; 820 ms is the worst figure quoted for planning). M3's checks failed runs where a rule sat within a stall's reach. The rules below are set so that no stall of that size can fail one by itself, with the reasons written down before any run.

## Decision

### Setup (`make verify-m5`, `test/verify/m5.sh`)

1. `make dev`, then the gateways are recreated in managed mode with `--profile replicas`: `SPILLWAY_GATEWAY_CONFIG=` and `SPILLWAY_GATEWAY_IDENTITY=` (set, empty) and `SPILLWAY_GATEWAY_CONTROL_PLANE=controlplane:9090` exported (ADR-0038), `docker compose --profile replicas up -d --wait gateway-1 … gateway-5`.
2. The sender, a new `cmd/chaos` subcommand `chaos controlplane` in the compose service `chaos` (in the compose network, read-only, `nofile` 65536), gets a token for the `spillway-ci` client (client credentials; secret from `SPILLWAY_CI_CLIENT_SECRET`, never printed; its service account holds `admin`, ADR-0040, because creating a tenant is admin-only) and through the REST API, each call with a 10 s timeout: imports `test/verify/m5.yaml` (with `If-Match`), publishes it, and creates tenant `m5-verify` (plan `m5-verify`) and consumer `m5-verify` if they do not exist. `m5.yaml` has upstream `m5` (`upstream-1` … `upstream-3`, no health check), route `m5-propagation` (`/anything/m5`, `auth_mode: none`), route `m5-probe` (`/status/200`, `auth_mode: api_key`), both with the default timeouts (connect 5 s, total and per try 15 s; no `timeouts` block), no retries, plan `m5-verify` with no entries, and **no rate-limit policy**, so Redis is not in the path.
3. It waits up to 60 s for `GET /api/v1/replicas` to list `gateway-1` … `gateway-5` live at the published config version and the current identity version. Otherwise every check fails as `setup`.
4. On exit (trap), whatever happened: the control plane and Postgres are started again if they are stopped, gateway-1 is recreated in static mode and gateway-2 … 5 are removed, so the stack is as `make dev` leaves it and `make verify-m0` finds no stopped container. The script swaps no committed file. Every key it issued is revoked. Data it wrote (versions, a tenant, a consumer, keys) stays in Postgres; it is reused by the next run.

The three checks run in this order, each once.

### (a) NFR-08: publish → applied on all replicas, p99 < 5 s

- **N = 60 publishes** (D10: ≥ 50), 5 gateways (D10's default).
- Each iteration: `PUT /routes/<m5-propagation>` setting a response header `X-Spillway-Verify: <i>` (with the route's `If-Match`); `POST /config/validate` for the revision; `POST /config/publish` with that revision and note `make verify-m5 <run id> <i>`; then poll `GET /config/versions/<v>` every 100 ms until its `applied` list (ADR-0036, from `replica_versions`) holds all 5 replicas, for at most 30 s; then wait 1 s and go on. The edit is real so each publish is not a 409 `nothing_to_publish`, and the measured path is the UI's: edit, preview, publish.
- **Times, both on Postgres's clock** (`clock_timestamp()`): **published** = `config_versions.published_at`, taken inside the publish transaction just before its audit insert, `NOTIFY` and commit (ADR-0037), so it is never later than the commit and the sample can only overstate; **applied on replica r** = `replica_versions.first_reported_at` for (r, `config`, v), set by the first heartbeat reporting v, which is the immediate heartbeat sent after the swap (ADR-0038). Sample = max over the 5 replicas of applied − published. A replica missing after 30 s makes the sample 30 s.
- **PASS** when p99 of the 60 samples < 5 s. p99 is nearest-rank: the ⌈0.99 × 60⌉ = 60th smallest, which at N = 60 is the maximum, so in practice every publish must reach every replica within 5 s. The line prints p50, p99, max and N, and the JSON report every sample.

### (b) NFR-09: a revoked key is rejected on all replicas, < 5 s

- **R = 20 revocations**, on the same 5 gateways.
- Each iteration: issue a key for consumer `m5-verify` (`POST /consumers/{id}/keys`; the key stays in the sender's memory only); probe `GET http://gateway-N:8000/status/200` with it on every replica until each answers 200 (at most 10 s, else the check fails as `setup`), which shows the key's identity version is applied everywhere; then revoke it (`POST /keys/{id}/revoke`).
- **Times, both on the sender's clock**: **revoked** = just before the revoke request is sent (earlier than the commit, so the measure can only overstate); **rejected on replica r** = the answer time of the first probe that r answers 401. From the revoke on, one probe loop per replica sends a probe every 50 ms (each with a 4 s timeout; a transport error, a timeout or a status other than 200 or 401 fails the check) until a 401, then 10 more at 50 ms that must all be 401 (a 200 after a 401 fails the check: the key came back). Sample = max over the 5 replicas of rejected − revoked.
- **PASS** when every one of the 20 samples < 5 s (a stricter reading than p99). The line prints p50, max and R. It also prints, for information, the identity path's own figure from the database: the revocation's identity version committed → the last replica's `replica_versions` time for it.

### (c) NFR-12: a 10-minute control-plane outage

- **What is stopped:** the control plane **and Postgres** (`docker compose stop controlplane postgres`, the chaos table's `docker stop`; the control plane first). Gateways never talk to Postgres, so this is the same outage for them as stopping the control plane alone; stopping both also makes the recovery step restart the whole control-plane side from cold.
- **Load:** the sender issues one key for consumer `m5-verify` and probes it to 200 on every replica, as in (b); then it drives `GET /status/200` on `m5-probe` with that key, **500 rps** in all, round-robin over `gateway-1` … `gateway-5` (100 rps each), open model as loadgen (ADR-0014): fixed due times, overdue requests sent at once, a request due while 2,000 are in flight counted as dropped, HTTP/1.1 keep-alive with one transport per gateway, 20 s client timeout. Timeline: 30 s of load before the stop; the stop; **600 s** of outage; restart (`docker compose up -d --wait controlplane`, which starts Postgres, re-runs the no-op `migrate` and waits for health); load continues for 60 s after the control plane is healthy. About 345,000 requests.
- **A gateway restart during the outage, booting from its on-disk snapshot** (CP-07, D9). At outage + 300 s the script tells the sender (one line on stdin) to stop sending to `gateway-3`; the sender moves its share to the other four, waits for gateway-3's in-flight requests to finish and says so (one line on stdout); the script runs `docker compose restart gateway-3` and tells the sender to resume once gateway-3's `/readyz` answers 200; the sender closes gateway-3's idle connections (all to the old process) before it resumes. A load balancer would do the same with readiness; the point is the boot, not the restart's own requests. Without it, nothing in M5's exit would exercise CP-07.
- **Scrapes:** every 5 s, from 10 s before the stop until 60 s after the control plane is healthy, the sender reads each running gateway's `/readyz` and `/metrics` (`gw_config_version`, `gw_identity_version`, `gw_config_sync_age_seconds`) with a 4 s timeout, recording send times. A scrape that fails (transport error, timeout, unparsable metrics) is a scrape that breaks (c2). The script passes `t_stop` (one stdin line right before `docker compose stop`) and `t_healthy`.
- **Rules**, each a PASS/FAIL line:
  - **(c1) zero failed requests.** Every request of the whole run answered 2xx: no transport error, no timeout, no drop, no other status. The line prints the counts.
  - **(c2) config frozen.** Every scrape between `t_stop` and `t_healthy` shows each running gateway at the `gw_config_version` and `gw_identity_version` it had at the last scrape before `t_stop`, and `/readyz` 200 (gateway-3 is exempt between its pause and its first ready scrape).
  - **(c3) staleness grows.** Every such scrape shows `gw_config_sync_age_seconds` ≥ (scrape send time − `t_stop`) − 10 s, and the last scrape before `t_healthy` shows at least 590 s on all five. 60 s, OB-05's alert threshold, is crossed on every replica; the alert itself is M7's.
  - **(c4) boot from the snapshot.** gateway-3 is ready within 60 s of `docker compose restart` returning, at the versions of (c2), with a sync age that satisfies (c3), and serves the resumed load (counted in c1).
  - **(c5) recovery.** Within 60 s of `t_healthy`, every gateway's sync age is below 10 s, `GET /api/v1/replicas` lists all five live, and the versions are still those of (c2) (nothing was published).

### Stall tolerance: why no stall can fail a rule by itself

Planning figure: a stall of the whole machine lasts 4–9 ms typically and at most **820 ms** (ADR-0034 measured 788 ms). Every timeout and budget a stall could run into is at least 4× that (3.3 s).

- **(a) NFR-08.** The path is a commit, a `NOTIFY`, one marshalled snapshot sent per stream, a compile, a swap, and a heartbeat insert: tens of milliseconds in compose. A stall anywhere on it adds at most 0.82 s against a 5 s budget. The immediate heartbeat has a 4 s deadline, so a stall cannot make it fail (which would push the first report to the next 5 s heartbeat). gRPC keepalive needs 10 s idle plus a 5 s ping timeout to drop a stream, and the broadcaster's safety read every 10 s is a backstop only. The 30 s per-publish cap is far outside any stall.
- **(b) NFR-09.** The same identity path, plus the 50 ms probe interval: a stall delays the first 401 by its length, at most 0.82 s + 0.05 s against 5 s. A probe delayed by a stall still gets an answer well inside its 4 s timeout. The "no 200 after a 401" rule cannot be broken by a stall: a gateway that has applied a newer identity never goes back to an older one.
- **(c1) zero failed requests.** A stall delays answers; it does not fail them unless a timeout fires. The shortest timeout on the path is the route's default 5 s connect, against new connections that dial in milliseconds within compose; the per-try and total timeouts are 15 s, the client's 20 s. (M4's checks used a 1 s connect timeout, which an 820 ms stall would come within 0.2 s of; nothing in this check needs a short one.) A 0.82 s stall at 500 rps releases about 410 requests at once, against the sender's 2,000 in-flight cap, the gateways' 4,096 in-flight and 8,192 connection caps (GW-26) and the upstream's default `max_concurrent`, `max_pending` and `max_connections` of 1,024 each (ADR-0030). Nothing ejects endpoints: the upstream has no health check, outlier ejection needs 5 consecutive failures and a stall makes none, and there are no retries to exhaust a budget. There is no rate-limit policy, so no Redis budget.
- **(c2)–(c5).** Scrapes have 4 s timeouts on a 5 s schedule, so a stall delays a scrape but cannot fail it, and no rule needs a scrape at an exact time. (c3) allows 10 s of slack. (c4) and (c5) allow 60 s for what takes about 6 s (the 5 s boot deadline, ADR-0038) and about 10 s (one reconnect backoff of at most 5 s plus one 5 s heartbeat). `live_replicas` uses a 15 s window, so a stall cannot drop a replica from it.
- **No re-run rule.** Each check runs once per `make verify-m5`. A FAIL is reported as it is, and the script never runs a check again. No count allowance exists: every rule holds because a stall creates nothing it counts.

### Run time and pass rate

About 2 minutes of setup (images, five gateways, the import), 2 minutes for (a) (60 × about 2 s), 1 minute for (b), 12.5 minutes for (c): **about 18 minutes** in all. `make chaos-cp-outage` runs (c) alone. The verify task runs `make verify-m5` at least 3 times and (c) at least 3 full times, and records every run's numbers and the pass rate in its PR description (this ADR is not edited once accepted). `make verify-m3`'s known laptop failure rate (accepted on 2026-11-13) is not part of this check.

## Consequences

- NFR-08 is timed on one clock end to end; NFR-09 on another single clock, the client's, which is what "rejected" means to a client.
- The NFR-12 run takes 12.5 minutes; it is not part of CI's integration job, which keeps its shorter integration tests (including a short control-plane outage with the same rules at 30 s).
- The check needs `--profile replicas` and managed mode, which `make dev` does not set; the script sets and resets both, so other checks are unaffected.
- `cmd/chaos` grows a `controlplane` subcommand and `make` grows `verify-m5` and `chaos-cp-outage`.

## Alternatives considered

- **NFR-08 from gateway clocks (`applied_at`)** — five clocks against Postgres's; skew would enter the result.
- **NFR-09 from heartbeats only** — shows a version was applied, not that a request with the key is refused; the probes measure the requirement itself.
- **Stopping only the control plane** — the gateways see the same outage; stopping Postgres too also tests the cold restart of the control-plane side.
- **`docker kill` instead of `docker stop`** — the chaos table names `docker stop`; a vanished peer is covered by the integration tests through gRPC keepalive.
- **No gateway restart during the outage** — would leave CP-07's boot-from-disk without an end-to-end check.
- **p99 over more publishes (N = 200)** — makes p99 differ from the max but adds about 5 minutes for no extra assurance.
- **Re-running a failed check** — ruled out by the user (ADR-0027, ADR-0034).
