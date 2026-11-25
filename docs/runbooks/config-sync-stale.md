# Config sync stale

Alert: **`SpillwayConfigSyncStale`** (severity `page`), from
[`spillway.yaml`](../../deploy/prometheus/rules/spillway.yaml), per gateway
(`replica` label).

## What it means

A managed gateway has not had a heartbeat answered by the control plane on
an established Watch stream for over 60 s (`gw_config_sync_age_seconds > 60`,
CP-08). Static-mode gateways have no such metric and never fire it.

The gateway **keeps serving** its last good config and identity, from memory
or from its on-disk snapshot after a restart (CP-07, NFR-12). What it misses
while stale: new config versions, new and revoked API keys, suspensions and
access-list changes, and its heartbeats (so the control plane's replica
count for degraded mode and its heavy-hitter view age). A revoked key keeps
working on a stale gateway until it syncs again: that is the security cost
of this alert.

## First checks

1. All gateways or one? Prometheus:
   `gw_config_sync_age_seconds` by `replica`, or Grafana
   **Spillway / Control plane** (sync age and config version per replica).
2. **All of them**: is the control plane up and active?
   `cp_active` should be 1 on exactly one replica, `cp_watch_streams` equal to
   the number of gateways. Compose: `docker compose ps controlplane postgres`
   and `docker compose logs --tail 50 controlplane`; kind:
   `kubectl --context kind-spillway -n spillway get pods -l app.kubernetes.io/name=spillway-controlplane`.
   The control plane needs Postgres to serve the stream.
3. **One gateway**: its log says why the stream fails: TLS (an expired or
   reissued stream certificate, an unknown fleet identity on kind), DNS or
   the network path to port 9090.
4. After a restore of the control plane's database, gateways resync by
   themselves (the stream epoch, ADR-0055); a long-stale replica after a
   restore points at the stream, not the data.

## Mitigation

- **Control plane or Postgres down**: bring them back (compose
  `docker compose up -d controlplane`, which starts Postgres too; kind: check
  the pods and their events). Gateways reconnect with backoff and sync within
  seconds; the alert resolves at the next evaluation.
- **Stream certificates**: reissue with `make certs` (compose mounts the
  directories, processes reload within a second; kind: `make kind-up`
  re-creates the Secrets) and see ADR-0040 for the names.
- **A key must stop working now** while gateways are stale: suspend or
  revoke it as usual (it applies once they sync) and, if it cannot wait,
  restart the stale gateways only after the control plane is back. A
  restarted gateway without the control plane serves its snapshot, with the
  same keys.
- A replacement pod on kind started during the outage stays unready: see
  [gateway-snapshot.md](gateway-snapshot.md).
