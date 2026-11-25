#!/usr/bin/env bash
# NFR-15 noisy neighbour (ADR-0074 §D9): gateway-1 runs
# test/verify/m7-noisy.yaml with four tenants A-D (one consumer and static
# key each, made at run time, test/load/lib.sh) and a tenant-level Redis
# GCRA limit of 200/s, burst 200. loadgen's per-tenant mode sends quiet
# tenants B, C and D at 100 rps each all the time and noisy tenant A at
# 2,000 rps (10x its limit) in odd 10 s slots only: 30 s warm-up + 10 min =
# 60 slots = 30 (off, on) pairs. Passes when, for each quiet tenant, the
# median over the 30 pairs of p99(on slot) / p99(off slot) is below 1.10,
# and every tenant got at least 99% of its scheduled requests sent. The
# pooled p99s over the off and the on slots are printed too. About 12
# minutes. Run through make, which recreates the gateways at the 1% trace
# ratio (ADR-0030):
#   make nfr-noisy
# NOISY_DURATION (default 10m) shortens the run for development only.
set -uo pipefail
cd "$(dirname "$0")/../.."
# shellcheck source=test/load/lib.sh
. test/load/lib.sh

duration=${NOISY_DURATION:-10m}
quiet_rate=100 noisy_rate=2000 slot=10s max_ratio=1.10
echo "== NFR-15 noisy neighbour"

if ! build=$("${compose[@]}" build --quiet loadgen 2>&1); then printf '%s\n' "$build"; exit 1; fi
make_tenants 71 A B C D
swap "$tmp/identity.yaml" identity.yaml
swap test/verify/m7-noisy.yaml config.yaml
if ! wait_versions 71 71 30; then
	result FAIL "gateway-1 runs the noisy config" "config 71 and identity 71 not in use after 30 s"
	exit 1
fi

start=$SECONDS
tenant_loadgen -url http://gateway-1:8000/bytes/1024 -warmup 30s -duration "$duration" -slot "$slot" \
	-tenant "name=A,rate=$noisy_rate,key-file=/keys/A,toggle" \
	-tenant "name=B,rate=$quiet_rate,key-file=/keys/B" \
	-tenant "name=C,rate=$quiet_rate,key-file=/keys/C" \
	-tenant "name=D,rate=$quiet_rate,key-file=/keys/D" >"$tmp/noisy.json" 2>"$tmp/noisy.err"
code=$?
took=$((SECONDS - start))
if ! jq -e .tenants "$tmp/noisy.json" >/dev/null 2>&1; then
	result FAIL "loadgen" "exited $code without a result: $(grep -v '^ *Container' "$tmp/noisy.err" | tail -3)"
	exit 1
fi
if [ -n "${NFR_OUT:-}" ]; then cp "$tmp/noisy.json" "$NFR_OUT"; fi

valid=$(jq '[.tenants[] | .valid] | all' "$tmp/noisy.json")
check "$([ "$valid" = true ]; echo $?)" "every tenant sent >= 99% of its schedule" \
	"$(jq -r '[.tenants | to_entries[] | "\(.key) \(.value.achieved_rps | . * 10 | round / 10)/\(.value.target_rps) rps\(if .value.toggle then " while on" else "" end) (\(.value.requests.sent)/\(.value.requests.scheduled))"] | join(", ")' "$tmp/noisy.json")"
for q in B C D; do
	line=$(jq -r --arg q "$q" --argjson max "$max_ratio" '
		.tenants[$q] as $t | ($t.slots | length / 2 | floor) as $n
		| [range(0; $n) as $k | {off: $t.slots[2 * $k], on: $t.slots[2 * $k + 1]}] as $pairs
		| [$pairs[] | select(.off.requests > 0 and .on.requests > 0 and .off.p99 > 0) | .on.p99 / .off.p99] | sort as $r
		| ($r | length) as $m
		| (if $m == 0 then null elif $m % 2 == 1 then $r[($m - 1) / 2] else ($r[$m / 2 - 1] + $r[$m / 2]) / 2 end) as $median
		| [(if $m == $n and $n > 0 and $median < $max then 0 else 1 end),
			"median \($median // "n/a" | if type == "number" then (. * 1000 | round / 1000) else . end) over \($m) of \($n) pairs (min \($r[0] // 0 | . * 100 | round / 100), max \($r[-1] // 0 | . * 100 | round / 100)); pooled p99 noisy off \($t.pooled_p99.even_slots / 1000) ms, on \($t.pooled_p99.odd_slots / 1000) ms; \($t.status["2xx"] // 0) 2xx, \(($t.status | to_entries | map(select(.key != "2xx") | .value) | add) // 0) other, \(($t.errors | add) // 0) unanswered"]
		| "\(.[0])\t\(.[1])"' "$tmp/noisy.json")
	check "${line%%$'\t'*}" "NFR-15 quiet tenant $q: p99 on/off < $max_ratio" "${line#*$'\t'}"
done
jq -r '.tenants.A | "      noisy tenant A: \(.status["2xx"] // 0) 2xx, \(.status["4xx"] // 0) 4xx (429 above 200/s), \(.status["5xx"] // 0) 5xx, \((.errors | add) // 0) unanswered; p99 \(.latency_us.p99 / 1000) ms"' "$tmp/noisy.json"
echo "took $took s"

echo
if [ "$fails" -eq 0 ]; then echo "NFR-15 noisy neighbour: PASS"; else echo "NFR-15 noisy neighbour: FAIL ($fails checks)"; fi
[ "$fails" -eq 0 ]
