#!/usr/bin/env bash
# M4 exit checks (docs/design.md, M4; ADR-0031). With one of three endpoints
# failing, >= 99% of requests succeed within 10 s of the failure, retries
# stay within budget, and traces show each attempt. Two scenarios run once
# each: the endpoint answers 503 to everything (5xx), then it refuses
# connections (refused). For each, the compose service chaos ("chaos
# upstream") drives gateway-1 at 500 rps for 40 s, starts the fault on the
# mock upstream-fault ("chaos mock") after 10 s, and prints one PASS/FAIL line
# per rule: (0) the fault took effect, (a), (b) and (c), then the breaker
# state and the request counts. Exits 1 if any check fails; no check is ever
# run again. Run through make:
#   make verify-m4
set -uo pipefail
cd "$(dirname "$0")/../.."

fails=0

result() { # result PASS|FAIL check measured
	printf '%-4s  %-44s %s\n' "$1" "$2" "$3"
	if [ "$1" != PASS ]; then fails=$((fails + 1)); fi
}

compose=(docker compose --env-file .env.example)
if [ -f .env.local ]; then compose+=(--env-file .env.local); fi

# Each scenario swaps its config into gateway-1's mounted config.yaml as
# make verify-m2 does (ADR-0019). On exit the committed config is put back,
# the mock is set healthy and upstream-fault is removed, so make verify-m0
# finds no stopped container.
dir=deploy/compose/gateway
tmp=$(mktemp -d)
cp "$dir/config.yaml" "$tmp/config.yaml"
swap() { cp "$1" "$dir/.config.yaml.swap" && mv -f "$dir/.config.yaml.swap" "$dir/config.yaml"; }
restore() {
	swap "$tmp/config.yaml"
	rm -rf "$tmp"
	"${compose[@]}" --profile harness exec -T upstream-fault /chaos mock -set none >/dev/null 2>&1
	"${compose[@]}" --profile harness rm -fsv upstream-fault >/dev/null 2>&1
}
trap restore EXIT
trap 'exit 1' INT TERM

if ! build=$("${compose[@]}" build --quiet chaos 2>&1); then
	printf '%s\n' "$build"
	result FAIL "setup" "the chaos image did not build"
	exit 1
fi
if ! up=$("${compose[@]}" --profile harness up -d --wait upstream-fault 2>&1); then
	printf '%s\n' "$up"
	result FAIL "setup" "upstream-fault did not start"
	exit 1
fi

run_id=$(date +%s)
for scenario in 5xx refused; do
	case $scenario in
	5xx) echo "== one of three endpoints answers 503 (chaos table: upstream endpoint returns 5xx)" ;;
	refused) echo "== one of three endpoints refuses connections (GW-12: connect failure)" ;;
	esac
	swap "test/verify/m4-$scenario.yaml"
	"${compose[@]}" run --rm -T chaos upstream -fault "$scenario" -run-id "$run_id" >"$tmp/$scenario.json" 2>"$tmp/$scenario.err"
	code=$?
	grep -E '^(PASS|FAIL) |^      ' "$tmp/$scenario.err" # not compose's progress lines
	n=$(grep -c '^FAIL ' "$tmp/$scenario.err")
	fails=$((fails + n))
	if [ "$code" -ne 0 ] && [ "$n" -eq 0 ]; then
		result FAIL "$scenario: sender" "chaos upstream exited $code: $(tail -1 "$tmp/$scenario.err")"
	fi
done

echo
if [ "$fails" -eq 0 ]; then echo "M4 exit: PASS"; else echo "M4 exit: FAIL ($fails checks)"; fi
[ "$fails" -eq 0 ]
