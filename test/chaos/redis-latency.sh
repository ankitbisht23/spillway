#!/usr/bin/env bash
# Redis latency +50 ms chaos test (ADR-0074 §D9; docs/design.md, Chaos
# scenarios). gateway-4 is recreated from test/chaos/latency.compose.yml:
# its Redis traffic goes through one toxiproxy proxy per node, made here
# through toxiproxy's API, and it runs test/chaos/latency.yaml (one route
# each for fail_open, fail_closed and degraded at 50/s, burst 50). cmd/chaos
# ("chaos latency", the compose service chaos) drives each route at 100 rps
# for 60 s and puts a 50 ms latency toxic on all six proxies from +20 s to
# +40 s. Over the toxic window minus its first second: fail_open >= 99.9%
# 2xx, fail_closed 100% 503 fail_closed, degraded admits within ±10% of
# 50 × 19 + 50, each route's median latency <= its pre-toxic median + 10 ms;
# from +45 s every answer is a Redis decision again. Medians and counts over
# whole windows, so a host stall cannot fail it alone (ADR-0074). Prints one
# PASS/FAIL line per check; on every exit gateway-4 and the proxies are
# removed (the cluster itself is never touched). About 2 minutes. Run
# through make:
#   make chaos-redis-latency
set -uo pipefail
cd "$(dirname "$0")/../.."
# shellcheck source=test/chaos/lib.sh
. test/chaos/lib.sh
restored=1 # nothing to bring back in the cluster

override=(-f compose.yaml -f test/chaos/latency.compose.yml)
toxiproxy=http://127.0.0.1:8474
delete_proxies() {
	local n
	for n in 0 1 2 3 4 5; do curl -s -o /dev/null -X DELETE "$toxiproxy/proxies/redis-$n"; done
}
create_proxies() {
	local n
	for n in 0 1 2 3 4 5; do
		curl -sf -o /dev/null -X POST "$toxiproxy/proxies" -H 'Content-Type: application/json' \
			-d "{\"name\": \"redis-$n\", \"listen\": \"0.0.0.0:2637$n\", \"upstream\": \"redis-$n:6379\", \"enabled\": true}" || return 1
	done
}
latency_cleanup() {
	"${compose[@]}" "${override[@]}" --profile replicas rm -fsv gateway-4 >/dev/null 2>&1
	delete_proxies
	cleanup
}
trap latency_cleanup EXIT

echo "== Redis latency +50 ms (chaos)"

if ! build=$("${compose[@]}" build --quiet chaos 2>&1); then printf '%s\n' "$build"; exit 1; fi
delete_proxies
if ! create_proxies; then
	result FAIL "toxiproxy proxies redis-0 … redis-5" "toxiproxy's API on 127.0.0.1:8474 refused them"
	exit 1
fi
if ! up=$("${compose[@]}" "${override[@]}" --profile replicas up -d --no-deps --wait gateway-4 2>&1); then
	printf '%s\n' "$up"
	result FAIL "gateway-4 through toxiproxy" "docker compose up gateway-4 failed"
	exit 1
fi

start=$SECONDS
"${compose[@]}" run --rm -T chaos latency >"$tmp/report.json" 2>"$tmp/chaos.err"
code=$?
grep -E '^(PASS|FAIL|chaos latency:) ' "$tmp/chaos.err" # not compose's progress lines
fails=$((fails + $(grep -c '^FAIL ' "$tmp/chaos.err")))
if [ "$code" -ne 0 ] && ! grep -q '^FAIL ' "$tmp/chaos.err"; then
	result FAIL "sender" "cmd/chaos exited $code"
fi
if [ -n "${CHAOS_OUT:-}" ]; then cp "$tmp/report.json" "$CHAOS_OUT"; fi
echo "took $((SECONDS - start)) s"

echo
if [ "$fails" -eq 0 ]; then echo "Redis latency +50 ms: PASS"; else echo "Redis latency +50 ms: FAIL ($fails checks)"; fi
[ "$fails" -eq 0 ]
