#!/usr/bin/env bash
# M2 exit checks (docs/design.md, M2; ADR-0017, ADR-0019; NFR-03's method is
# U4 in ADR-0021). Prints one PASS/FAIL line per check with what it measured,
# and exits 1 if any check fails. Run through make:
#   make verify-m2-shadow   shadow mode never rejects, on the dev stack (CI runs this part)
#   make verify-m2          property tests, the shadow check, then NFR-03 added latency
set -uo pipefail
cd "$(dirname "$0")/../.."

mode=${1:-all}
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

# gateway-1 runs each check's config from test/verify, swapped into its
# mounted config.yaml by rename as make verify-m1 does (ADR-0015); the
# committed config is put back on exit.
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
decisions() { # decisions <policy> <allow|deny> <enforce|shadow>
	metric "gw_ratelimit_decisions_total{decision=\"$2\",mode=\"$3\",policy=\"$1\"} "
}
loadgen() { # loadgen <url> <rate> <warm-up> <duration>: loadgen's JSON result
	"${compose[@]}" run --rm -T loadgen -url "$1" -rate "$2" -warmup "$3" -duration "$4" 2>"$tmp/loadgen.err"
}
# Failed: no answer, or an answer other than 2xx.
failed='((.errors | add // 0) + ([.status | to_entries[] | select(.key != "2xx") | .value] | add // 0))'
# shellcheck source=test/verify/added-latency.sh
. test/verify/added-latency.sh

if ! build=$("${compose[@]}" build --quiet loadgen 2>&1); then printf '%s\n' "$build"; fi

if [ "$mode" = all ]; then
	echo "== (a) property tests, each algorithm against its own bound"

	# rapid's default is 100 random cases per test; the exit check runs more.
	checks=10000
	out=$(go test -count=1 -run '^TestProperty' -v ./internal/ratelimit/local/ -args -rapid.checks="$checks" 2>&1)
	for t in GCRA:"admitted in any window w <= burst + w*rate/period" \
		FixedWindow:"admitted per aligned window <= limit" \
		SlidingWindow:"estimate <= limit at every admission, any window <= 2 x limit" \
		Concurrency:"in flight <= limit"; do
		name=${t%%:*}
		if grep -q "^--- PASS: TestProperty$name " <<<"$out"; then
			result PASS "property $name" "$(grep -A1 "^=== RUN   TestProperty$name\$" <<<"$out" | sed -n 's/.*\[rapid\] OK, passed \([0-9]*\) tests.*/\1/p') random cases: ${t#*:}"
		else
			result FAIL "property $name" "${t#*:}; see go test -run TestProperty$name ./internal/ratelimit/local/"
		fi
	done
fi

echo "== (b) shadow mode never rejects (RL-26)"

# 500 rps for 10 s against a shadow GCRA limit of 10/s on the route: about
# 98% of the requests are would-denies, and every one must get its 2xx.
rate=500 seconds=10
if use test/verify/m2-shadow.yaml; then
	before=$(decisions verify-shadow deny shadow)
	out=$(loadgen http://gateway-1:8000/bytes/1024 "$rate" 0s "${seconds}s")
	denies=$(($(decisions verify-shadow deny shadow) - before))
	r=$(jq -r --argjson denies "$denies" "$failed"' as $failed
		| "\(if $failed == 0 and .valid and $denies > 0 then 0 else 1 end) \(.status["2xx"] // 0)/\(.requests.sent) answered 2xx, \(.status["4xx"] // 0) 4xx, \($failed) failed; \($denies) shadow would-denies at \(.target_rps) rps against 10/s"' <<<"$out" 2>/dev/null)
	if [ -n "$r" ]; then
		check "${r%% *}" "shadow would-denies, zero 429s" "${r#* }"
	else
		result FAIL "shadow would-denies, zero 429s" "loadgen gave no result: $(tail -1 "$tmp/loadgen.err")"
	fi
else
	result FAIL "shadow would-denies, zero 429s" "gateway-1 did not load test/verify/m2-shadow.yaml"
fi

if [ "$mode" = all ]; then
	echo "== (c) NFR-03 added latency with local limits"

	# Alternating paired runs (U4, added-latency.sh). The rate and the
	# length can be changed for a quick look; the exit check uses these.
	rate=${VERIFY_M2_RATE:-1000} warmup=${VERIFY_M2_WARMUP:-30s} duration=${VERIFY_M2_DURATION:-60s}
	policies="verify-per-ip verify-per-route verify-inflight"
	counts() { for p in $policies; do echo "$p $(decisions "$p" allow enforce) $(decisions "$p" deny enforce)"; done; }
	# What each policy decided during a gateway run, warm-up included: it
	# must allow every measured request and deny none (a deny is a 429 anyway).
	judge() {
		paste -d ' ' <(echo "$1") <(echo "$2") | awk -v sent="$3" '
			{ a = $5 - $2; d = $6 - $3; if (a < sent || d > 0) bad = 1
				s = s (NR > 1 ? ", " : "") sprintf("%s %d allow %d deny", $1, a, d) }
			END { print (bad ? 1 : 0), 0, s }'
	}
	if use test/verify/m2-latency.yaml; then
		added_latency "NFR-03 added p99 <= 2 ms" 2000 "local policies" "$rate" "$warmup" "$duration"
	else
		result FAIL "NFR-03 added p99 <= 2 ms" "gateway-1 did not load test/verify/m2-latency.yaml"
	fi
fi

echo
if [ "$fails" -eq 0 ]; then echo "M2 exit: PASS"; else echo "M2 exit: FAIL ($fails checks)"; fi
[ "$fails" -eq 0 ]
