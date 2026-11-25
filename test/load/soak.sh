#!/usr/bin/env bash
# NFR-11 availability soak (ADR-0074 §D9): gateway-1 runs
# test/verify/m7-soak.yaml with four tenants A-D (one consumer and static
# key each, made at run time, test/load/lib.sh). loadgen's per-tenant mode
# sends /bytes/1024 with A, B and C at 400 rps each (under their tenant
# limit of 600/s) and D at 800 rps (over it): 2,000 rps, 30 s warm-up + 10
# min. The SLI is the design's: gateway-caused 5xx =
# gw_requests_total{origin="gateway", code_class="5xx"} with reason not
# shed, circuit_open or fail_closed, over all gw_requests_total, as deltas
# of gateway-1's /metrics on the admin port before and after the run
# (scrape-independent). Passes at <= 0.1%, with every tenant at >= 99% of
# its schedule and gateway-1 not restarted; the deliberate 5xx, upstream
# 5xx and 429s are printed separately. About 12 minutes. Run through make,
# which recreates the gateways at the 1% trace ratio (ADR-0030):
#   make nfr-soak
# SOAK_DURATION (default 10m) shortens the run for development only.
set -uo pipefail
cd "$(dirname "$0")/../.."
# shellcheck source=test/load/lib.sh
. test/load/lib.sh

duration=${SOAK_DURATION:-10m}
max_pct=0.1
echo "== NFR-11 availability soak"

if ! build=$("${compose[@]}" build --quiet loadgen 2>&1); then printf '%s\n' "$build"; exit 1; fi
make_tenants 72 A B C D
swap "$tmp/identity.yaml" identity.yaml
swap test/verify/m7-soak.yaml config.yaml
if ! wait_versions 72 72 30; then
	result FAIL "gateway-1 runs the soak config" "config 72 and identity 72 not in use after 30 s"
	exit 1
fi

# counts <metrics file>: "<total> <gateway 5xx> <shed> <circuit_open>
# <fail_closed> <upstream 5xx> <429> <process start>" from gw_requests_total.
counts() {
	awk '
		/^gw_requests_total\{/ {
			v = $NF + 0; total += v
			if ($0 ~ /origin="gateway"/ && $0 ~ /code_class="5xx"/) {
				if ($0 ~ /reason="shed"/) shed += v
				else if ($0 ~ /reason="circuit_open"/) circuit += v
				else if ($0 ~ /reason="fail_closed"/) closed += v
				else gw += v
			}
			if ($0 ~ /origin="upstream"/ && $0 ~ /code_class="5xx"/) up += v
			if ($0 ~ /reason="ratelimited"/) limited += v
		}
		/^process_start_time_seconds / { start = $NF }
		END { printf "%d %d %d %d %d %d %d %s\n", total, gw, shed, circuit, closed, up, limited, start }' "$1"
}

metrics >"$tmp/before" || { result FAIL "gateway-1 metrics" "no /metrics on 127.0.0.1:8001"; exit 1; }
start=$SECONDS
tenant_loadgen -url http://gateway-1:8000/bytes/1024 -warmup 30s -duration "$duration" -slot 10s \
	-tenant "name=A,rate=400,key-file=/keys/A" -tenant "name=B,rate=400,key-file=/keys/B" \
	-tenant "name=C,rate=400,key-file=/keys/C" -tenant "name=D,rate=800,key-file=/keys/D" >"$tmp/soak.json" 2>"$tmp/soak.err"
code=$?
took=$((SECONDS - start))
metrics >"$tmp/after" || { result FAIL "gateway-1 metrics" "no /metrics on 127.0.0.1:8001 after the run"; exit 1; }
if ! jq -e .tenants "$tmp/soak.json" >/dev/null 2>&1; then
	result FAIL "loadgen" "exited $code without a result: $(grep -v '^ *Container' "$tmp/soak.err" | tail -3)"
	exit 1
fi
if [ -n "${NFR_OUT:-}" ]; then cp "$tmp/soak.json" "$NFR_OUT"; fi

read -r t0 g0 s0 c0 f0 u0 l0 p0 <<<"$(counts "$tmp/before")"
read -r t1 g1 s1 c1 f1 u1 l1 p1 <<<"$(counts "$tmp/after")"
total=$((t1 - t0)) gw=$((g1 - g0))
valid=$(jq '[.tenants[] | .valid] | all' "$tmp/soak.json")
check "$([ "$valid" = true ]; echo $?)" "every tenant sent >= 99% of its schedule" \
	"$(jq -r '[.tenants | to_entries[] | "\(.key) \(.value.achieved_rps | . * 10 | round / 10)/\(.value.target_rps) rps"] | join(", ")' "$tmp/soak.json"); $(jq -r '.requests.sent' "$tmp/soak.json") measured requests sent"
check "$([ "$p0" = "$p1" ]; echo $?)" "gateway-1 not restarted during the run" "process start $p0, then $p1"
pct=$(awk -v g="$gw" -v t="$total" 'BEGIN { if (t > 0) printf "%.4f", 100 * g / t; else print "n/a" }')
ok=$(awk -v g="$gw" -v t="$total" -v m="$max_pct" 'BEGIN { print (t > 0 && 100 * g / t <= m) ? 0 : 1 }')
check "$ok" "NFR-11 gateway-caused 5xx <= ${max_pct}%" \
	"$gw of $total requests ($pct%); deliberate 5xx: shed $((s1 - s0)), circuit_open $((c1 - c0)), fail_closed $((f1 - f0)); upstream 5xx $((u1 - u0)); 429 $((l1 - l0))"
jq -r '"      loadgen: \(.status["2xx"] // 0) 2xx, \(.status["4xx"] // 0) 4xx, \(.status["5xx"] // 0) 5xx, \((.errors | add) // 0) unanswered of \(.requests.sent) measured; p50 \(.latency_us.p50 / 1000) ms, p99 \(.latency_us.p99 / 1000) ms, max \(.latency_us.max / 1000) ms"' "$tmp/soak.json"
echo "took $took s"

echo
if [ "$fails" -eq 0 ]; then echo "NFR-11 availability soak: PASS"; else echo "NFR-11 availability soak: FAIL ($fails checks)"; fi
[ "$fails" -eq 0 ]
