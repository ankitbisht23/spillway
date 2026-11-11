#!/usr/bin/env bash
# Redis full-outage chaos test (NFR-13, ADR-0021; breakers per ADR-0025).
# gateway-1 … gateway-3 run test/chaos/outage.yaml: one route per failure
# mode. loadgen drives 200 rps on the fail_open and the fail_closed route
# through gateway-1 and 200 rps on the degraded route through each of
# gateway-1 … gateway-3: an 8 s warm-up, then 20 s measured. 6 s after the
# runs start, while they are still warming up, all six nodes are paused
# with docker pause, so the measured 20 s are all outage and the budget
# check also covers the calls that time out until the breakers open
# (ADR-0025). Prints one PASS/FAIL line per check with what it measured,
# and exits 1 if any check fails. The cluster is always brought back
# (lib.sh). Run through make:
#   make chaos-redis-outage
set -uo pipefail
cd "$(dirname "$0")/../.."
# shellcheck source=test/chaos/lib.sh
. test/chaos/lib.sh

probe=(http://127.0.0.1:8000/anything/chaos-open)
gateways=(gateway-1 gateway-2 gateway-3)
rate=200 warmup=8 pause_after=6 seconds=20

# metrics <gateway>: its admin metrics, read from inside the compose network.
metrics() { "${compose[@]}" exec -T prometheus wget -qO- "http://$1:8001/metrics"; }
# sample <file> <series>: the value of the first line of <file> that starts
# with <series> (a name with all its labels, in exposition order); 0 if none.
sample() {
	awk -v s="$2" 'index($0, s) == 1 { v = $NF + 0; found = 1; exit } END { printf "%d\n", found ? v : 0 }' "$1"
}
snapshot() { # snapshot <prefix>: every gateway's metrics in <prefix>-<gateway>
	local g
	for g in "${gateways[@]}"; do metrics "$g" >"$1-$g" || return 1; done
}
delta() { # delta <gateway> <series>: its increase from before to after
	echo $(($(sample "$tmp/after-$1" "$2") - $(sample "$tmp/before-$1" "$2")))
}
# drive: starts the five loadgen runs at once, in the background (pids);
# each JSON result goes to $tmp/outage-<run>.json.
runs="open:gateway-1:chaos-open closed:gateway-1:chaos-closed degraded-1:gateway-1:chaos-degraded degraded-2:gateway-2:chaos-degraded degraded-3:gateway-3:chaos-degraded"
pids=()
drive() {
	local run name gateway route
	for run in $runs; do
		IFS=: read -r name gateway route <<<"$run"
		"${compose[@]}" run --rm -T loadgen -url "http://$gateway:8000/anything/$route" -rate "$rate" -warmup "${warmup}s" -duration "${seconds}s" \
			>"$tmp/outage-$name.json" 2>"$tmp/outage-$name.err" &
		pids+=($!)
	done
}

echo "== Redis full outage (NFR-13)"

if ! build=$("${compose[@]}" build --quiet loadgen 2>&1); then printf '%s\n' "$build"; exit 1; fi
swap test/chaos/outage.yaml
"${compose[@]}" --profile replicas up -d --no-deps --wait gateway-2 gateway-3 >/dev/null 2>&1 || {
	result FAIL "gateway-2 and gateway-3 start" "docker compose --profile replicas up gateway-2 gateway-3 failed"
	exit 1
}
for g in "${gateways[@]}"; do
	for _ in $(seq 40); do
		if metrics "$g" | grep -q '^gw_config_version 41$'; then continue 2; fi
		sleep 0.5
	done
	result FAIL "gateways run test/chaos/outage.yaml" "$g does not report gw_config_version 41"
	exit 1
done
if ! cluster_ok && ! restore_cluster || ! wait_decided 30; then
	result FAIL "Redis up before the outage" "the cluster is not healthy, or gateway-1 does not decide in Redis"
	exit 1
fi

# A loadgen container starts within a second or two, so the pause lands
# well inside every run's warm-up, after a few seconds of Redis decisions.
snapshot "$tmp/before" || { result FAIL "metrics before the outage" "could not read the gateways' metrics"; exit 1; }
drive
sleep "$pause_after"
"${compose[@]}" pause "${nodes[@]}" >/dev/null 2>&1 || {
	result FAIL "docker pause every Redis node" "docker compose pause failed"
	exit 1
}
wait "${pids[@]}"
snapshot "$tmp/after" || { result FAIL "metrics after the outage" "could not read the gateways' metrics"; exit 1; }
restore

# run <name> <jq: 0 = pass> <jq: measured>: one loadgen run's check line;
# every run must also be valid (>= 99% of its target sent, ADR-0014).
run() {
	local r
	r=$(jq -r "\"\(if .valid and (($2) == 0) then 0 else 1 end) \(.requests.sent) sent, \(.status[\"2xx\"] // 0) 2xx, \(.status[\"4xx\"] // 0) 4xx, \(.status[\"5xx\"] // 0) 5xx, \(.errors | add // 0) errors, \(.achieved_rps | round) of \(.target_rps) rps\(if .valid then \"\" else \" (invalid run)\" end)$3\"" \
		"$tmp/outage-$1.json" 2>/dev/null)
	if [ -z "$r" ]; then
		result FAIL "$4" "loadgen gave no result: $(tail -1 "$tmp/outage-$1.err")"
		return
	fi
	check "${r%% *}" "$4" "${r#* }"
}
decisions() { # decisions <policy> <decision>: gateway-1's enforce decisions during the runs
	delta gateway-1 "gw_ratelimit_decisions_total{decision=\"$2\",mode=\"enforce\",policy=\"$1\"} "
}
# closed_other <file>: the chaos-closed route's 5xx answers whose reason is
# not fail_closed, the reason of the gateway's fail_closed 503.
closed_other() {
	awk 'index($0, "gw_requests_total{code_class=\"5xx\"") == 1 && index($0, "route=\"chaos-closed\"") && !index($0, "reason=\"fail_closed\"") { s += $NF }
		END { printf "%d\n", s }' "$1"
}

run open 'if (.status["2xx"] // 0) * 1000 >= .requests.sent * 999 then 0 else 1 end' \
	"; $(decisions chaos-open fail_open) fail_open decisions, warm-up included" "fail_open: >= 99.9% allowed"
closed=$(decisions chaos-closed fail_closed)
other=$(($(closed_other "$tmp/after-gateway-1") - $(closed_other "$tmp/before-gateway-1")))
run closed "if (.status[\"5xx\"] // 0) == .requests.sent and $closed >= .requests.sent and $other == 0 then 0 else 1 end" \
	"; $closed fail_closed decisions, warm-up included; $other 5xx for another reason" "fail_closed: every request 503"
for i in 1 2 3; do
	run "degraded-$i" '(((.status["2xx"] // 0) - 10) / '"$seconds"') as $r | if $r >= 90 and $r <= 110 then 0 else 1 end' \
		"; admitted (2xx - burst 10) / ${seconds} s = \((((.status[\"2xx\"] // 0) - 10) / $seconds * 10 | round) / 10)/s, limit 300/s / 3 replicas" \
		"degraded: gateway-$i within 10% of 100/s"
done

# Every Redis check during the runs, warm-up and pause included, ended
# within the 5 ms budget plus the 2 ms slack (ADR-0021): all of gateway-1 …
# gateway-3's observations of gw_ratelimit_check_duration_seconds{backend=
# "redis"} in the le="0.007" bucket. The timeouts are the calls sent before
# the breakers opened.
count=0 within=0 within5=0 per=() errors=
for g in "${gateways[@]}"; do
	n=$(delta "$g" 'gw_ratelimit_check_duration_seconds_count{backend="redis"} ')
	count=$((count + n))
	within=$((within + $(delta "$g" 'gw_ratelimit_check_duration_seconds_bucket{backend="redis",le="0.007"} ')))
	within5=$((within5 + $(delta "$g" 'gw_ratelimit_check_duration_seconds_bucket{backend="redis",le="0.005"} ')))
	per+=("$g $n")
done
for kind in timeout conn script; do
	n=0
	for g in "${gateways[@]}"; do n=$((n + $(delta "$g" "gw_redis_errors_total{kind=\"$kind\"} "))); done
	errors+="${errors:+, }$kind $n"
done
check $([ "$count" -gt 0 ] && [ "$within" -eq "$count" ]; echo $?) "every decision within one budget (7 ms)" \
	"$within/$count Redis checks <= 7 ms ($within5 <= 5 ms; $(IFS=,; echo "${per[*]}" | sed 's/,/, /g')); redis errors: $errors"

echo
if [ "$fails" -eq 0 ]; then echo "Redis full outage: PASS"; else echo "Redis full outage: FAIL ($fails checks)"; fi
[ "$fails" -eq 0 ]
