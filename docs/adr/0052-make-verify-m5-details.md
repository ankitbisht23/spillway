# 0052. make verify-m5: details

- **Status:** Accepted
- **Date:** 2026-11-14
- **Requirement IDs:** NFR-08, NFR-09, NFR-12, CP-07, CP-08, CP-09, CP-17, CP-22

## Context

ADR-0042 settles `make verify-m5`'s three checks, their pass rules and their timeouts. Building it left these open, and showed three places where ADR-0042's wording, taken literally, would fail a run in which nothing is wrong:

- **The data a run leaves.** ADR-0042 keeps tenant `m5-verify`, its consumer and keys in Postgres for the next run. The tenant is on plan `m5-verify`, and every publish checks that the plans identity refers to stay (ADR-0037): any later import that drops the plan, such as `make test-integration`'s compose tests importing the compose config, then fails its publish with 422.
- **When the control plane is healthy.** `docker compose up -d --wait controlplane` returns when compose's healthcheck, run every 2 s, sees the control plane ready. The control plane opens its gRPC listener right after it becomes ready, so a gateway can reconnect and sync before compose notices: in a dry run the gRPC listener opened 2.0 s before `up` returned. A scrape between that sync and the script's "healthy" line shows a small sync age and breaks (c3) "every scrape between t_stop and t_healthy shows sync age ≥ (send − t_stop) − 10 s", although the outage was over.
- **Scrapes in flight at an edge.** A scrape sent just before t_healthy, or just before gateway-3's pause, can be answered after a resync or after the restart began.
- **Who keeps the timeline,** how the script and the sender talk, where gateway-3's share goes, how setup steps follow each other, and how a re-run handles an import that changes nothing (ADR-0044).

## Decision

### The sender

- `chaos controlplane <step>` (cmd/chaos/controlplane.go, outage.go) runs one step per `docker compose run` of the compose service `chaos`: `setup`, `ready`, `propagation` (a), `revocation` (b), `outage` (c) and `cleanup`. Each prints its JSON report on stdout and its PASS/FAIL lines on stderr, as `chaos upstream` does.
- Each step takes its own service token for `spillway-ci` (client credentials from Keycloak in the compose network), replaced before a call once it is 60 s old: Keycloak's tokens live 5 minutes and the outage step runs about 13. The client secret is `SPILLWAY_CI_CLIENT_SECRET` or `_FILE` in the container (the script passes the variable with `-e` when its own environment sets it; otherwise `.env.local`, which compose gives every container; otherwise `.env.example`'s dev value). No step prints a token, the secret or a key; the unit tests check every step's output for them.
- **setup** (stdin: `test/verify/m5.yaml`): removes what an earlier run left (below), exports the working set for its revision, imports the file with `If-Match` and a note, validates and publishes. A 409 there means the working set is already the latest version (ADR-0044's no-op import): setup takes the latest version and goes on. Then it creates tenant `m5-verify` on plan `m5-verify` and consumer `m5-verify`.
- The script then recreates gateway-1 … gateway-5 in managed mode. They come after setup because a managed gateway is not ready without a published version, and readiness is their compose healthcheck.
- **ready** waits up to 60 s for all five to be live and watching in `GET /replicas` at the latest config version and at the identity version on the first line of `GET /identity/export` (the rest of that answer is not read). Otherwise every check fails as `setup`.
- **(a)** is ADR-0042's loop. The header value is `<run id>-<i>`, so every publish is a real change. Only the five replicas' entries of the applied list count. A 409 here is a failure.
- **(b)** runs `chaos revocation`'s rounds (ADR-0048), 20 of them, with the same token source. After it, the script prints the identity path's own figure for information: per revoked key, `api_keys.revoked_at` to the last of the five replicas' `replica_versions.first_reported_at` for the key's identity version, both on Postgres's clock (read with `psql` in the postgres container), p50 and max.

### The outage (c)

- **The sender keeps the timeline**, as the killed-primary test's sender does (ADR-0021): it asks for each action with a line on stderr, `action stop` at load start + 30 s, `action restart gateway-3` at t_stop + 300 s and `action start` at t_stop + 600 s. The script polls that file every 200 ms and answers on the sender's stdin through a FIFO: `stopping` immediately before `docker compose stop controlplane postgres` and `stopped <exit code>` after it, `restarted <exit code>` after `docker compose restart gateway-3`, `healthy <exit code>` after `docker compose up -d --wait --wait-timeout 180 controlplane`. Every time is the sender's clock; t_stop is when it reads `stopping`. ADR-0042's "the script tells the sender" is the same exchange in the other direction.
- **The pause.** Before asking for the restart, the sender stops giving gateway-3 requests, then waits for gateway-3's requests in flight (at most the 20 s client timeout plus 1 s) and its scrapes in flight (at most 9 s). The paused share goes round robin to the other four. After the restart it polls gateway-3's `/readyz` every 100 ms (4 s timeout each) for up to 60 s, closes gateway-3's idle connections and resumes it; a replica that does not come back stays paused and (c4) fails.
- **t_healthy.** From the start request on, the sender polls the control plane's admin `/readyz`, the endpoint `controlplane healthcheck` and so compose's healthcheck read, every 100 ms with a 4 s timeout. t_healthy is the send time of the last poll before the first one answering 200, or the start request's time if the first answers 200: a moment the control plane was certainly not yet ready. The control plane answers 200 only after its REST and admin listeners open and before its election opens the gRPC listener (cmd/controlplane/main.go), so no gateway can have synced before t_healthy. The script's `healthy` line is reported beside it; the load and the scrapes run until 60 s after the later of the two.
- **The windows.** (c2) and (c3) take the scrapes sent at or after t_stop and **answered** before t_healthy; (c3)'s bound keeps ADR-0042's send time. (c5) takes the scrapes sent at or after t_healthy. A scrape sent before t_healthy and answered after it is in neither, at most one per replica. gateway-3's exemption runs from its pause to its first ready scrape sent after `docker compose restart` returned; a scrape sent before the pause is not exempt, hence the wait for scrapes in flight.
- **Scrape failures.** A scrape fails on a transport error or timeout, a `/metrics` answer other than 200, or a missing `gw_config_version`, `gw_identity_version` or `gw_config_sync_age_seconds`; it breaks (c2) and (c3) in the outage and (c5) after it. A `/readyz` other than 200 is a successful scrape that breaks (c2).
- **(c4)** also needs gateway-3's first ready scrape to be sent before t_healthy: it booted while the control plane was down, from its snapshot.

### Clean-up

- On every exit the script starts the control plane and Postgres if not both run, runs `chaos controlplane cleanup` (revokes consumer `m5-verify`'s keys that are not revoked, deletes the consumer, whose keys go with it, and the tenant), recreates gateway-1 in static mode and removes gateway-2 … gateway-5. Published config versions stay, and the working set stays at `m5.yaml` with the last header edit. Unlike ADR-0042, tenant, consumer and keys do not stay, so later imports may drop plan `m5-verify`; setup removes them too when a run was killed before its trap.
- The gateways run at compose's trace ratio of 1, as in `make verify-m4`: nothing measured is the gateway's added latency.

## Consequences

- Only the stated edges move: (c5)'s 60 s count from a t_healthy up to compose's 2 s healthcheck interval earlier than the script's line, which is stricter; (c2) and (c3) end where the outage can first have ended instead of where compose noticed. No rule is checked fewer times than once per scrape in the outage, and nothing re-runs.
- `make test-integration` and other imports work after `make verify-m5`; a run's tenant cannot outlive it.
- The script holds no timing logic beyond its 200 ms poll; the sender's unit tests drive the whole timeline against a fake control plane, fake gateways and a fake script.

## Alternatives considered

- **t_healthy from the script's line** — ADR-0042's literal reading: a gateway that resyncs in the up-to-2 s before compose notices fails (c3) although the outage is over.
- **A shorter healthcheck interval in compose** — narrows the window without closing it, and changes compose for one check.
- **Keeping the tenant and having the integration tests delete it** — every test that imports would have to know about `make verify-m5`.
- **The script keeping the timeline** — two processes' sleeps to keep in step; the sender already schedules its load and scrapes.
- **gateway-3's share all to its neighbour** — 200 rps on one replica for the restart; ADR-0042 says the other four.
