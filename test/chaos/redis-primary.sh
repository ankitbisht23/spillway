#!/usr/bin/env bash
# Killed-primary chaos test (NFR-13, ADR-0021; breakers per ADR-0025).
# gateway-1 runs test/chaos/primary.yaml, and cmd/chaos (the compose service
# chaos) drives 30 keys, 10 on each primary, at 20 rps each for 40 s. After
# 10 s it names the primary that owns slot 0; this script kills it with
# docker kill and passes the container's exit time back on the sender's
# stdin. The sender prints one PASS/FAIL line per check; the cluster is
# always brought back (lib.sh): the killed node returns empty and rejoins as
# a replica. Exits 1 if any check fails. Run through make:
#   make chaos-redis-primary
# It needs cluster-require-full-coverage no (deploy/compose/redis/redis.conf),
# which Redis nodes started before that line was added lack: docker compose
# down -v and make dev recreate them.
set -uo pipefail
cd "$(dirname "$0")/../.."
# shellcheck source=test/chaos/lib.sh
. test/chaos/lib.sh

probe=(-H 'X-Chaos-Key: restore-probe' http://127.0.0.1:8000/anything/chaos)

echo "== Redis primary killed (NFR-13)"

if ! build=$("${compose[@]}" build --quiet chaos 2>&1); then printf '%s\n' "$build"; exit 1; fi
swap test/chaos/primary.yaml
if ! cluster_ok && ! restore_cluster; then
	result FAIL "Redis up before the kill" "the cluster is not healthy"
	exit 1
fi

# The sender reads the kill time on stdin from this FIFO and writes the
# kill request ("kill redis-N") on stderr, which is polled here.
mkfifo "$tmp/stdin"
"${compose[@]}" run --rm -T chaos <"$tmp/stdin" >"$tmp/report.json" 2>"$tmp/chaos.err" &
pid=$!
exec 3>"$tmp/stdin"
node=
while kill -0 "$pid" 2>/dev/null; do
	node=$(sed -n 's/^kill \(redis-[0-5]\)$/\1/p' "$tmp/chaos.err")
	if [ -n "$node" ]; then break; fi
	sleep 0.05
done
if [ -n "$node" ]; then
	if "${compose[@]}" kill "$node" >/dev/null 2>&1; then
		docker inspect -f '{{.State.FinishedAt}}' "$("${compose[@]}" ps -aq "$node")" >&3
	fi
fi
exec 3>&-
wait "$pid"
code=$?

grep -E '^(PASS|FAIL|chaos:) ' "$tmp/chaos.err" # not compose's progress lines
jq -r '.failure_mode_outside_window_samples[]? | "      outside the window: " + .' "$tmp/report.json" 2>/dev/null
fails=$((fails + $(grep -c '^FAIL ' "$tmp/chaos.err")))
if [ "$code" -ne 0 ] && ! grep -q '^FAIL ' "$tmp/chaos.err"; then
	result FAIL "sender" "cmd/chaos exited $code${node:+ after the kill of $node}"
fi
restore

echo
if [ "$fails" -eq 0 ]; then echo "Redis primary killed: PASS"; else echo "Redis primary killed: FAIL ($fails checks)"; fi
[ "$fails" -eq 0 ]
