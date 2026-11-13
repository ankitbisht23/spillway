#!/usr/bin/env bash
# M3 exit checks (docs/design.md, M3; ADR-0021, ADR-0027, ADR-0034). Prints one PASS/FAIL line per
# check with what it measured, and exits 1 if any check fails. Run through
# make:
#   make verify-m3   the accuracy harness at 3 replicas (NFR-06), the Redis
#                    full-outage and killed-primary chaos tests (NFR-13), then
#                    NFR-04 added latency with a Redis check
set -uo pipefail
cd "$(dirname "$0")/../.."

fails=0

result() { # result PASS|FAIL check measured
	printf '%-4s  %-44s %s\n' "$1" "$2" "$3"
	if [ "$1" != PASS ]; then fails=$((fails + 1)); fi
}
check() { # check <0 = pass> check measured
	if [ "$1" -eq 0 ]; then result PASS "$2" "$3"; else result FAIL "$2" "$3"; fi
}

compose=(docker compose --env-file .env.example)
if [ -f .env.local ]; then compose+=(--env-file .env.local); fi

# The harness and the chaos scripts swap their own configs in and put back
# the one they found; (c) swaps test/verify/m3-latency.yaml into gateway-1's
# mounted config.yaml as make verify-m2 does (ADR-0019), and the committed
# config is put back on exit.
dir=deploy/compose/gateway
tmp=$(mktemp -d)
cp "$dir/config.yaml" "$tmp/config.yaml"
swap() { cp "$1" "$dir/.config.yaml.swap" && mv -f "$dir/.config.yaml.swap" "$dir/config.yaml"; }
restore() {
	swap "$tmp/config.yaml"
	rm -rf "$tmp"
}
trap restore EXIT
trap 'exit 1' INT TERM

echo "== (a) NFR-06 accuracy, GCRA strict mode, 3 replicas"

# make accuracy REPLICAS=3: 10 keys per pattern at 2x a fail_closed 100/s
# limit (burst 100) for 60 s, round-robin over gateway-1 … gateway-3. Each
# pattern passes with zero over-admission in every window, >= 99% of its
# schedule sent and no failure-mode admissions; 503s are printed, not judged
# (ADR-0021, ADR-0025).
test/accuracy/run.sh 3 >"$tmp/accuracy.json" 2>"$tmp/accuracy.err"
for pattern in uniform bursty boundary; do
	read -r name verdict line < <(grep -E "^$pattern +(PASS|FAIL) " "$tmp/accuracy.err")
	if [ -n "${name:-}" ]; then
		result "$verdict" "accuracy: $pattern, zero over-admission" "$line"
	else
		result FAIL "accuracy: $pattern, zero over-admission" "the harness gave no result: $(tail -1 "$tmp/accuracy.err")"
	fi
	name=
done

# chaos <script> <name>: runs test/chaos/<script>.sh and prints its check
# lines; a script that failed without a FAIL line fails <name>.
chaos() {
	local out code n
	out=$("test/chaos/$1.sh" 2>&1)
	code=$?
	grep -E '^(PASS|FAIL) |^      ' <<<"$out"
	n=$(grep -c '^FAIL ' <<<"$out")
	fails=$((fails + n))
	if [ "$code" -ne 0 ] && [ "$n" -eq 0 ]; then
		result FAIL "$2" "test/chaos/$1.sh exited $code: $(tail -1 <<<"$out")"
	fi
}

echo "== (b) NFR-13 Redis full outage (make chaos-redis-outage)"
chaos redis-outage "Redis full outage"
echo "== (b) NFR-13 Redis primary killed (make chaos-redis-primary)"
chaos redis-primary "Redis primary killed"

echo "== (c) NFR-04 added latency with a Redis check"

# metric <series>: the value of the first sample of gateway-1 whose line starts
# with <series>, such as 'gw_config_version ' or a name with all its labels in
# exposition order; 0 if there is none.
metric() {
	curl -fsS --max-time 5 http://127.0.0.1:8001/metrics |
		awk -v s="$1" 'index($0, s) == 1 { v = $NF + 0; found = 1; exit } END { printf "%d\n", found ? v : 0 }'
}
# use <config>: swaps <config> in and waits up to 10 s for gateway-1 to run it.
use() {
	local want
	want=$(awk '/^version:/ { print $2; exit }' "$1")
	swap "$1"
	for _ in $(seq 20); do
		if [ "$(metric 'gw_config_version ')" = "$want" ]; then return 0; fi
		sleep 0.5
	done
	return 1
}
decisions() { # decisions <policy> <decision>: gateway-1's enforce decisions
	metric "gw_ratelimit_decisions_total{decision=\"$2\",mode=\"enforce\",policy=\"$1\"} "
}
redis_errors() { # gateway-1's gw_redis_errors_total, every kind
	curl -fsS --max-time 5 http://127.0.0.1:8001/metrics |
		awk 'index($0, "gw_redis_errors_total{") == 1 { s += $NF } END { printf "%d\n", s }'
}
loadgen() { # loadgen <url> <rate> <warm-up> <duration>: loadgen's JSON result
	"${compose[@]}" run --rm -T loadgen -url "$1" -rate "$2" -warmup "$3" -duration "$4" 2>"$tmp/loadgen.err"
}
# Failed: no answer, or an answer other than 2xx.
failed='((.errors | add // 0) + ([.status | to_entries[] | select(.key != "2xx") | .value] | add // 0))'
# shellcheck source=test/verify/added-latency.sh
. test/verify/added-latency.sh

if ! build=$("${compose[@]}" build --quiet loadgen 2>&1); then printf '%s\n' "$build"; fi

# Alternating paired runs (U4, added-latency.sh). The rate and the length can
# be changed for a quick look; the exit check uses these.
rate=${VERIFY_M3_RATE:-1000} warmup=${VERIFY_M3_WARMUP:-30s} duration=${VERIFY_M3_DURATION:-60s}
counts() {
	echo "verify-redis $(decisions verify-redis allow) $(decisions verify-redis deny) $(decisions verify-redis fail_closed) $(redis_errors) $(metric 'gw_redis_errors_total{kind="timeout"} ')"
}
# During a gateway run, warm-up included, the Redis policy must deny none and
# allow every measured request but the fail_closed ones. Failed Redis calls
# and their fail_closed 503s are excused when they are one stall burst
# (ADR-0034): every error a timeout, one fail_closed decision per error, no
# other failed answer in loadgen's run (warm-up included), and all of them
# within one window of at most burst_ms, from the earliest one's due time to
# the latest one's answer. The run's line reports them, and the run and its
# pair still count; anything else voids the run.
burst_ms=1000
judge() {
	local failures
	failures=$(jq -r '.failures // {} | "\(.count // 0) \(.span_ms // 0) \(.first // "-") \(.last // "-")"' <<<"$4" 2>/dev/null)
	paste -d ' ' <(echo "$1") <(echo "$2") <(echo "${failures:-0 0 - -}") | awk -v sent="$3" -v max="$burst_ms" '
		function clock(s, p) { if (split(s, p, "T") < 2) return s; sub(/Z$/, "", p[2]); return substr(p[2], 1, 12) }
		{
			a = $8 - $2; d = $9 - $3; f = $10 - $4; e = $11 - $5; t = $12 - $6; n = $13; span = $14
			why = ""
			if (e == 0 && f > 0) why = sprintf("%d fail_closed without a Redis error", f)
			else if (e > 0 && t != e) why = sprintf("%d of %d Redis errors not timeouts", e - t, e)
			else if (e > 0 && f != e) why = sprintf("%d fail_closed for %d Redis errors", f, e)
			else if (e > 0 && n != f) why = sprintf("%d failed answers in the run for %d fail_closed", n, f)
			else if (e > 0 && span > max) why = sprintf("failures span %.1f ms > %d ms, %s – %s UTC", span, max, clock($15), clock($16))
			line = sprintf("%s %d allow %d deny %d fail_closed, %d Redis errors", $1, a, d, f, e)
			if (why != "") line = line " (not excused: " why ", ADR-0034)"
			else if (e > 0) line = line sprintf(" (%d timeouts in one burst of %.1f ms, %s – %s UTC, excused, ADR-0034)", e, span, clock($15), clock($16))
			print (a + f >= sent && d == 0 && why == "" ? 0 : 1), f, line
		}'
}
if use test/verify/m3-latency.yaml; then
	added_latency "NFR-04 added p99 <= 5 ms" 5000 "Redis policy" "$rate" "$warmup" "$duration"
else
	result FAIL "NFR-04 added p99 <= 5 ms" "gateway-1 did not load test/verify/m3-latency.yaml"
fi

echo
if [ "$fails" -eq 0 ]; then echo "M3 exit: PASS"; else echo "M3 exit: FAIL ($fails checks)"; fi
[ "$fails" -eq 0 ]
