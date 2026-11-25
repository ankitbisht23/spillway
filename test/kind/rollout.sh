#!/usr/bin/env bash
# make kind-rollout: NFR-14 on kind (ADR-0075 §D11 "NFR-14 on kind",
# ADR-0086). A Job runs loadgen inside the cluster against the gateway's
# Service at 300 rps for 4 minutes (test/kind/rollout-job.yaml); 60 s after
# the load starts, the gateways are restarted with
#   kubectl --context kind-spillway -n spillway rollout restart deployment/spillway-gateway
# and the rollout is waited for (5 minutes at most).
#
# Failed = any request with a transport error, a timeout or a non-2xx status,
# warm-up included (loadgen's failures, ADR-0034). PASS needs all of: zero
# failed requests; at least 99% of the target rate achieved; the rollout
# complete within 5 minutes with every new pod Ready; every old pod gone
# before the load ends. One run, no retries.
#
# Stalls (ADR-0026, ADR-0027, ADR-0034): no line measures latency; each
# request has 10 s, and the time rules (5 minutes, the 4-minute load) are
# minutes long, so a stall of up to ~0.8 s cannot fail a line by itself.
#
# Run from the repository root with BIN and KUBECONFIG from the Makefile;
# kubectl is a function that always passes --context kind-spillway.
set -uo pipefail

: "${BIN:?BIN is set by the Makefile}"
: "${KUBECONFIG:?KUBECONFIG is set by the Makefile}"
case "$KUBECONFIG" in */.kube/spillway-kind) ;; *) echo "KUBECONFIG must be ~/.kube/spillway-kind, not $KUBECONFIG" >&2; exit 2 ;; esac
kubectl() { "$BIN/kubectl" --context kind-spillway --request-timeout=20s "$@"; }

ns=spillway
fails=0
result() { # result PASS|FAIL check measured
	printf '%-4s  %-52s %s\n' "$1" "$2" "$3"
	if [ "$1" != PASS ]; then fails=$((fails + 1)); fi
}
check() { if [ "$1" -eq 0 ]; then result PASS "$2" "$3"; else result FAIL "$2" "$3"; fi; }
gateway_pods() {
	kubectl -n "$ns" get pods -l app.kubernetes.io/name=spillway-gateway -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort
}

want=$(kubectl -n "$ns" get deployment spillway-gateway -o jsonpath='{.spec.replicas}')
ready=$(kubectl -n "$ns" get deployment spillway-gateway -o jsonpath='{.status.readyReplicas}')
if [ -z "$want" ] || [ "${ready:-0}" != "$want" ]; then
	echo "the gateways are not ready ($ready of $want): run make kind-up first" >&2
	exit 1
fi
old=$(gateway_pods)

kubectl -n "$ns" delete job nfr14-rollout --ignore-not-found --wait=true >/dev/null 2>&1
kubectl apply -f test/kind/rollout-job.yaml >/dev/null || { echo "could not create the load Job" >&2; exit 1; }
if ! kubectl -n "$ns" wait --for=jsonpath='{.status.active}'=1 job/nfr14-rollout --timeout=60s >/dev/null; then
	echo "the load Job did not start" >&2
	exit 1
fi
kubectl -n "$ns" wait --for=condition=Ready pod -l app.kubernetes.io/name=nfr14-rollout --timeout=60s >/dev/null
load_start=$(date +%s)
echo "load started (300 rps, 4 min); restart at +60 s"
sleep 60

restart_at=$(($(date +%s) - load_start))
kubectl -n "$ns" rollout restart deployment/spillway-gateway >/dev/null
kubectl -n "$ns" rollout status deployment/spillway-gateway --timeout=5m >/dev/null
rollout_rc=$?
rolled_at=$(($(date +%s) - load_start))
# Old pods leave after their preStop sleep, drain and grace; wait for them
# while the load runs.
gone_at=
while [ $(($(date +%s) - load_start)) -lt 240 ]; do
	left=$(comm -12 <(printf '%s\n' "$old") <(gateway_pods))
	if [ -z "$left" ]; then
		gone_at=$(($(date +%s) - load_start))
		break
	fi
	sleep 1
done
new=$(gateway_pods)
new_ready=$(kubectl -n "$ns" get deployment spillway-gateway -o jsonpath='{.status.readyReplicas} {.status.updatedReplicas}')

# The load runs 240 s from its start; loadgen then prints its result. 60 s
# of slack past that, within the Job's deadline (330 s).
left=$((300 - ($(date +%s) - load_start)))
if ! kubectl -n "$ns" wait --for=condition=complete job/nfr14-rollout --timeout="$((left > 30 ? left : 30))s" >/dev/null 2>&1; then
	echo "the load Job did not complete" >&2
fi
res=$(kubectl -n "$ns" logs job/nfr14-rollout 2>/dev/null | sed -n '/^{/,/^}/p')
kubectl -n "$ns" delete job nfr14-rollout --ignore-not-found --wait=false >/dev/null 2>&1

failed=$(jq -r '.failures.count // 0' <<<"$res" 2>/dev/null)
sent=$(jq -r '.requests.sent // empty' <<<"$res" 2>/dev/null)
achieved=$(jq -r '.achieved_rps // empty' <<<"$res" 2>/dev/null)
target=$(jq -r '.target_rps // empty' <<<"$res" 2>/dev/null)
status=$(jq -c '.status // {}' <<<"$res" 2>/dev/null)
errors=$(jq -c '.errors // {}' <<<"$res" 2>/dev/null)
p99=$(jq -r '.latency_us.p99 // empty' <<<"$res" 2>/dev/null)
max=$(jq -r '.latency_us.max // empty' <<<"$res" 2>/dev/null)

[ -n "$sent" ] && [ "$failed" = 0 ]
check $? "zero failed requests (NFR-14)" "failed ${failed:-?} of ${sent:-?} sent; status $status; errors $errors; p99 ${p99:-?} us, max ${max:-?} us"
[ -n "$achieved" ] && awk -v a="$achieved" -v t="$target" 'BEGIN { exit !(a >= 0.99 * t) }'
check $? "at least 99% of the target rate" "achieved ${achieved:-?} of ${target:-?} rps"
[ "$rollout_rc" -eq 0 ] && [ "$new_ready" = "$want $want" ] && [ -z "$(comm -12 <(printf '%s\n' "$old") <(printf '%s\n' "$new"))" ]
check $? "rollout complete in 5 min, $want new pods Ready" "restart at +${restart_at} s, complete at +${rolled_at} s ($((rolled_at - restart_at)) s); ready/updated $new_ready"
[ -n "$gone_at" ]
check $? "every old pod gone before the load ended (+240 s)" "old pods gone at +${gone_at:-never} s"

if [ "$fails" -eq 0 ]; then echo "NFR-14 rolling restart: PASS"; else echo "NFR-14 rolling restart: FAIL ($fails)"; exit 1; fi
