#!/usr/bin/env bash
# Gateway kill -9 under load (ADR-0074 §D9; docs/design.md, Chaos
# scenarios). gateway-1 … gateway-3 run test/chaos/gwkill.yaml in static
# mode, and cmd/chaos ("chaos gwkill", the compose service chaos) sends 300
# rps round robin over them (one transport per gateway, 10 s timeout) for
# 40 s, ejecting a gateway at its first failed request as a passive-checking
# load balancer would. At +20 s it asks for the kill and this script runs
# docker kill -s KILL on gateway-2. Passes when no request to gateway-1 or
# gateway-3 failed and every failed request went to gateway-2 and was sent
# before the sender's first observed failure from it: only its in-flight
# requests failed. Judged by order, not time (ADR-0074). Prints one
# PASS/FAIL line per check; on every exit the committed config goes back and
# gateway-2 … gateway-5 are removed (lib.sh). About 1 minute. Run through
# make:
#   make chaos-gateway-kill
set -uo pipefail
cd "$(dirname "$0")/../.."
# shellcheck source=test/chaos/lib.sh
. test/chaos/lib.sh
restored=1 # Redis is not involved

echo "== Gateway kill -9 under load (chaos)"

if ! build=$("${compose[@]}" build --quiet chaos 2>&1); then printf '%s\n' "$build"; exit 1; fi
swap test/chaos/gwkill.yaml
if ! up=$("${compose[@]}" --profile replicas up -d --no-deps --wait gateway-2 gateway-3 2>&1); then
	printf '%s\n' "$up"
	result FAIL "gateway-2 and gateway-3 start" "docker compose --profile replicas up gateway-2 gateway-3 failed"
	exit 1
fi

start=$SECONDS
"${compose[@]}" run --rm -T chaos gwkill >"$tmp/report.json" 2>"$tmp/chaos.err" &
pid=$!
killed=
while kill -0 "$pid" 2>/dev/null; do
	if grep -q '^kill gateway-2$' "$tmp/chaos.err"; then
		"${compose[@]}" kill -s SIGKILL gateway-2 >/dev/null 2>&1 && killed=1
		break
	fi
	sleep 0.05
done
wait "$pid"
code=$?
grep -E '^(PASS|FAIL|chaos gwkill:) ' "$tmp/chaos.err" # not compose's progress lines
fails=$((fails + $(grep -c '^FAIL ' "$tmp/chaos.err")))
if [ -z "$killed" ]; then result FAIL "docker kill -s KILL gateway-2" "not run: the sender never asked, or the kill failed"; fi
if [ "$code" -ne 0 ] && ! grep -q '^FAIL ' "$tmp/chaos.err"; then
	result FAIL "sender" "cmd/chaos exited $code"
fi
if [ -n "${CHAOS_OUT:-}" ]; then cp "$tmp/report.json" "$CHAOS_OUT"; fi
echo "took $((SECONDS - start)) s"

echo
if [ "$fails" -eq 0 ]; then echo "Gateway kill -9 under load: PASS"; else echo "Gateway kill -9 under load: FAIL ($fails checks)"; fi
[ "$fails" -eq 0 ]
