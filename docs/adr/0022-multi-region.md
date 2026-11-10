# 0022. Multi-region

- **Status:** Accepted
- **Date:** 2026-11-10
- **Requirement IDs:** RL-05, RL-11 – RL-14, RL-22, RL-23, NFR-04, NFR-06

## Context

The design lists multi-region global limits as a non-goal and asks for an ADR that documents the approach (Overview, M3). Spillway's limits are exact because every decision for a key is one atomic script on the one Redis primary that owns the key's slot, reading that primary's clock (RL-11 – RL-13, ADR-0021). A second region breaks that: a Redis Cluster cannot span regions without putting a cross-region round trip (tens to hundreds of milliseconds) inside a 5 ms budget (RL-22, NFR-04), and Redis has no synchronous multi-primary mode. Any multi-region scheme therefore trades accuracy for latency, and the trade should be written down before anyone deploys Spillway in two places.

## Decision

- **Non-goal.** Spillway builds nothing for multiple regions: no cross-region Redis, no replication of limiter state, no region-aware config. The data plane, control plane and Redis Cluster are deployed as one region.
- **If it is deployed in several regions**, each region is a full, independent deployment: its own gateways, its own Redis Cluster and the same published config. Limits are per region. A limit meant to be global is split by a configured share per region, `limit_r = floor(limit × share_r)` with the shares summing to at most 1 (for example 0.6 / 0.4 by expected traffic), and `burst` likewise. Requests are decided only by their own region's cluster, so latency, accuracy (NFR-06) and failure modes (RL-23) are exactly those of one region, and a region losing Redis or the network to the others affects only itself. Nothing in the config format supports shares today; an operator would publish a different config per region.
- **Optional asynchronous reconciliation** (not built). To move unused share to where traffic is, a reconciler could read each region's per-key usage every interval `I` and adjust the per-region limits (or grant leases, as RL-18 does within a region). Its over-admission bound is what the regions can admit between reconciliations beyond the global limit: in the worst case every region spends its whole adjusted limit in the same interval, so admission over a window of `I` can reach `Σ_r (burst_r + I·rate_r/period)`, and if shares were moved towards regions that then go quiet while others surge, up to the sum of all regions' limits in that interval, against a global limit of `burst + I·rate/period`. A region cut off from the reconciler keeps its last limits, so a partition can only make the bound last longer, never larger.
- **What is not offered.** A single global counter in one region, called from the others: every request elsewhere would pay a cross-region round trip, and the other regions would fail together whenever that region or the link is down.

## Consequences

- Per-region limits are simple and keep every single-region guarantee, but a client that spreads its traffic across regions gets up to the sum of the regional limits: with shares that sum to 1, never more than the global limit in steady state, and less when its traffic does not match the shares.
- Choosing shares is an operator's job and needs traffic data per region.
- If multi-region ever becomes a goal, it starts with a config field for region shares, a region label on metrics, and the reconciler above, each with its own ADR.

## Alternatives considered

- **A Redis Cluster stretched across regions** — every decision whose slot lives elsewhere pays the cross-region round trip, and a regional partition splits the cluster.
- **Active-active CRDT counters (for example Redis Enterprise Active-Active)** — not open source, and convergence is asynchronous, so the over-admission bound is the same as reconciliation's with less control over it.
- **One region decides for all** — see "What is not offered".
