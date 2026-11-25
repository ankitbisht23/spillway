# Gateway started without a snapshot (CP-07)

A managed gateway keeps its last good config and identity on disk
(`SPILLWAY_GATEWAY_SNAPSHOT`, `/var/lib/spillway/snapshot.pb`) and boots from
it when the control plane does not answer within 5 s (CP-07, ADR-0038). In
the Helm chart that file lives on an `emptyDir` under the read-only root
filesystem (SEC-10, ADR-0075 §D11), so it lives as long as the pod. A
**replacement** pod (a rollout, an eviction, a node failure, a crash that
recreates the pod) started **while the control plane is unreachable** has an
empty volume, no snapshot, and cannot serve.

Commands below use the kind cluster's names: context `kind-spillway`,
namespace `spillway`. Substitute your own.

## What it means

- The new pod stays unready (`READY 0/1`) and receives no traffic; its log
  says `no control plane within 5s and no on-disk snapshot; not ready until
  the control plane answers`.
- Pods that were running before the outage keep serving their last good
  config (their snapshot and memory are intact); capacity is reduced by
  every pod that could not start.
- The chart limits voluntary disruptions to one gateway at a time
  (PodDisruptionBudget `spillway-gateway`, `maxUnavailable: 1`), so a drain
  does not replace every snapshotted pod at once; it cannot stop
  involuntary ones.

## First checks

1. Is the control plane up?
   `kubectl --context kind-spillway -n spillway get pods -l app.kubernetes.io/name=spillway-controlplane`
   and its log: `kubectl --context kind-spillway -n spillway logs deploy/spillway-controlplane --tail 50`.
2. Is Postgres up? The control plane needs it to serve the stream
   (`kubectl --context kind-spillway -n spillway-deps get pods -l app.kubernetes.io/name=postgres` on kind).
3. Which gateways still serve? `gw_config_sync_age_seconds` on the others
   grows while the stream is down (Prometheus, job `gateway`); `READY 1/1`
   pods are serving their last good config.
4. Confirm the unready pod's reason in its log (the line above), not a bad
   certificate or a crash.

## Mitigation

**Preferred: restore the control plane.** Once it answers, every gateway,
the new one included, receives the current config and identity within
seconds and becomes ready; nothing else is needed. After a database
restore, too, no restore-specific step is needed (the stream epoch makes
gateways take the restored versions, ADR-0055).

**If the control plane cannot come back soon: seed the new pods** from a
gateway that still serves.

1. Export a live pod's snapshot into a Secret (the image has no shell or
   `tar`, so `gateway snapshot-export` writes the file to stdout; the
   snapshot is checked first):

   ```sh
   kubectl --context kind-spillway -n spillway exec <ready-gateway-pod> -c gateway -- \
     /gateway snapshot-export \
     | kubectl --context kind-spillway -n spillway create secret generic spillway-gateway-seed \
         --from-file=snapshot.pb=/dev/stdin
   ```

2. Point the chart at it and upgrade:

   ```sh
   helm --kube-context kind-spillway -n spillway upgrade spillway deploy/helm/spillway \
     --reuse-values --set gateway.snapshotSeed.secretName=spillway-gateway-seed
   ```

   Every gateway pod gets an init container, `gateway snapshot-seed`, which
   copies `snapshot.pb` into an **empty** snapshot volume after checking it
   parses and has this gateway's schema major; a pod that already has a
   snapshot is left alone. The upgrade rolls the gateways one at a time
   (`maxUnavailable: 0`), each new pod starting from the seed.

   The upgrade also runs the migrate hook Job, which needs Postgres: if
   Postgres is down too, restore it first, or keep serving on the pods that
   still have their snapshot.

3. **Once the control plane is back, delete the seed Secret** and unset the
   value: the snapshot holds API key hashes, and an old seed is stale.

   ```sh
   kubectl --context kind-spillway -n spillway delete secret spillway-gateway-seed
   helm --kube-context kind-spillway -n spillway upgrade spillway deploy/helm/spillway \
     --reuse-values --set gateway.snapshotSeed.secretName=
   ```

   The seed volume is `optional`, so pods started between the two commands
   start without it (the init container finds no seed and does nothing).

## Why an emptyDir

A PersistentVolume per gateway would need a StatefulSet, which cannot surge,
so `maxUnavailable: 0` and a zero-failure rolling restart (NFR-14) would be
impossible (ADR-0075, alternatives). The control plane is the source of
truth; the snapshot only bridges an outage for pods that were already
running.
