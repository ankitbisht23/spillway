#!/usr/bin/env bash
# make demo-d1 … make demo-d7 (SC-02, ADR-0096 §1, §2): one scenario on the
# running demo.
#
#   scenario.sh <d1|…|d7>     BLOCK=1 with d7: block the leaked key once
#                             heavy hitters show it (make demo-d7 BLOCK=1)
#
# It stops the demo-traffic service (the base load), runs demo-traffic run
# --scenario=dN in the foreground in a one-off container of that service
# (the base load plus the scenario's bursts, faults or actions), then starts
# the service again, also after Ctrl-C, so exactly one base load runs at any
# time. --no-deps: the one-off never starts or recreates another service.
# demo-traffic switches off any fault it switched on, on every exit path.
# Nothing here prints a key, a token or a secret.
set -euo pipefail
cd "$(dirname "$0")/../.."

compose=(docker compose --env-file .env.example)
if [ -f .env.local ]; then compose+=(--env-file .env.local); fi
demo=("${compose[@]}" -f compose.yaml -f deploy/compose/demo.compose.yml --profile replicas --profile demo)
# While make demo-share runs, its settings and secret files apply here too
# (ADR-0105).
if [ -f deploy/demo/run/share.env ]; then
	demo+=(--env-file deploy/demo/run/share.env -f deploy/compose/share.compose.yml)
fi

case "${1:-}" in
d[1-7]) ;;
*)
	echo "usage: $0 <d1|d2|d3|d4|d5|d6|d7> (make demo-d1 … make demo-d7; make demo-d7 BLOCK=1)" >&2
	exit 2
	;;
esac
args=(run "--scenario=$1")
case "${BLOCK:-}" in
"" | 0) ;;
1)
	if [ "$1" != d7 ]; then
		echo "$0: BLOCK=1 is for d7 only" >&2
		exit 2
	fi
	args+=(--block)
	;;
*)
	echo "$0: BLOCK must be 1 or 0" >&2
	exit 2
	;;
esac

id=$("${compose[@]}" ps -q --status running controlplane)
if [ -z "$id" ] || [ "$(docker inspect -f '{{index .Config.Labels "spillway.demo"}}' "$id")" != "true" ]; then
	echo "demo-$1: the demo is not running; start it with make demo" >&2
	exit 1
fi

restart() {
	if ! "${demo[@]}" up -d --no-deps --wait --wait-timeout 120 demo-traffic >/dev/null 2>&1; then
		echo "demo-$1: the base load did not start again; make demo starts it" >&2
	fi
}
"${demo[@]}" stop demo-traffic >/dev/null 2>&1
trap 'restart "$1"' EXIT
"${demo[@]}" run --rm --no-deps demo-traffic "${args[@]}"
