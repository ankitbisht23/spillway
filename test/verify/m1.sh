#!/usr/bin/env bash
# M1 exit checks (docs/design.md, M1; ADR-0015). Prints one PASS/FAIL line per
# check with what it measured, and exits 1 if any check fails. Run through make:
#   make verify-m1-reload   config reload under load on the dev stack (CI runs this part)
#   make verify-m1          the reload check, then the NFR-01 baseline on origin/bench-results
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

echo "== config reload under load (NFR-14)"

# loadgen sends a steady rate through gateway-1 for 60 s while gateway-1's
# mounted config is replaced by rename every 2 s, alternating between the
# committed config and test/verify/m1-reload.yaml. The committed config is put
# back on exit.
rate=1000 seconds=60 min_reloads=25
swaps=$((seconds / 2 - 1)) # the last one 2 s before the load ends
dir=deploy/compose/gateway
tmp=$(mktemp -d)
cp "$dir/config.yaml" "$tmp/config.yaml"
swap() { cp "$1" "$dir/.config.yaml.swap" && mv -f "$dir/.config.yaml.swap" "$dir/config.yaml"; }
restore() { # stops the swaps, then puts the committed config back
	touch "$tmp/stop"
	if [ -n "${swapper:-}" ]; then wait "$swapper" 2>/dev/null; fi
	swap "$tmp/config.yaml"
	rm -rf "$tmp"
}
trap restore EXIT
trap 'exit 1' INT TERM

if ! build=$("${compose[@]}" build --quiet loadgen 2>&1); then printf '%s\n' "$build"; fi
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
(
	for i in $(seq "$swaps"); do
		sleep 2
		if [ -e "$tmp/stop" ]; then break; fi
		if [ $((i % 2)) -eq 1 ]; then swap test/verify/m1-reload.yaml; else swap "$tmp/config.yaml"; fi
	done
) &
swapper=$!
out=$("${compose[@]}" run --rm -T loadgen -url http://gateway-1:8000/bytes/1024 \
	-rate "$rate" -warmup 0s -duration "${seconds}s" 2>"$tmp/loadgen.err")
wait "$swapper"

# Failed: no answer, or an answer other than 2xx. A run that could not send
# 99% of its requests (valid: false) fails too.
r=$(jq -r '((.errors | add // 0) + ([.status | to_entries[] | select(.key != "2xx") | .value] | add // 0)) as $failed
	| "\(if $failed == 0 and .valid then 0 else 1 end) \(.status["2xx"] // 0)/\(.requests.sent) answered 2xx, \($failed) failed, \(.requests.dropped) not sent; p99 \(.latency_us.p99 / 1000) ms at \(.target_rps) rps"' <<<"$out" 2>/dev/null)
if [ -n "$r" ]; then
	check "${r%% *}" "zero failed requests during reloads" "${r#* }"
else
	result FAIL "zero failed requests during reloads" "loadgen gave no result: $(tail -1 "$tmp/loadgen.err")"
fi
# The gateway logs "config loaded" for each snapshot it swaps in; the restore
# on exit comes after this count.
reloads=$("${compose[@]}" logs --no-color --no-log-prefix --since "$since" gateway-1 2>/dev/null |
	grep -c '"msg":"config loaded"')
check $([ "$reloads" -ge "$min_reloads" ]; echo $?) "config reloads applied under load" \
	"$reloads of $swaps swaps (at least $min_reloads)"

if [ "$mode" = all ]; then
	echo "== NFR-01 baseline on bench-results"

	# The newest valid result per arch from a bench.yml run on main (ADR-0003).
	files=""
	if git fetch -q origin bench-results 2>/dev/null; then
		files=$(git ls-tree -r --name-only origin/bench-results results/)
	fi
	for arch in x64 arm64; do
		found=""
		for f in $(grep "^results/$arch/" <<<"$files" | sort -r); do
			if git show "origin/bench-results:$f" | jq -e '.valid and .ref == "refs/heads/main"' >/dev/null; then
				found=$f
				break
			fi
		done
		if [ -n "$found" ]; then
			result PASS "NFR-01 baseline $arch" "$(git show "origin/bench-results:$found" | jq -r --arg f "$found" \
				'"\(.rps_per_vcpu | round) rps per gateway vCPU at \(.target_rps) offered (NFR-01 target 10000), p99 \(.latency_us.p99.median / 1000) ms; \(.hardware.cpu_model), commit \(.commit[:7]), \($f)"')"
		elif [ -z "$files" ]; then
			result FAIL "NFR-01 baseline $arch" "pending: origin has no bench-results branch yet"
		else
			result FAIL "NFR-01 baseline $arch" "pending: no valid result from a bench.yml run on main"
		fi
	done
fi

echo
if [ "$fails" -eq 0 ]; then echo "M1 exit: PASS"; else echo "M1 exit: FAIL ($fails checks)"; fi
[ "$fails" -eq 0 ]
