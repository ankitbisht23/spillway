#!/usr/bin/env bash
# Accuracy harness v1 (NFR-06, NFR-07, ADR-0021, ADR-0069) on the dev stack.
# gateway-1 … gateway-N run test/accuracy/gateway.yaml (strict) or
# test/accuracy/batched.yaml (batched: lease_k 10), swapped into their
# mounted config.yaml by rename as the verify scripts do (ADR-0019) and put
# back on exit; cmd/accuracy then drives them from the compose network.
# Prints the harness's JSON report on stdout and its summary lines on
# stderr, and exits as the harness does. Run through make:
#   make accuracy REPLICAS=3                 1 to 5 gateways, strict
#   make accuracy REPLICAS=3 MODE=batched    batched: over-admission <= 3 x 10
set -uo pipefail
cd "$(dirname "$0")/../.."

n=${1:-3}
case $n in
[1-5]) ;;
*) echo "accuracy: REPLICAS must be 1 to 5, not $n" >&2; exit 2 ;;
esac
mode=${2:-strict}
case $mode in
strict) config=test/accuracy/gateway.yaml harness=() ;;
batched) config=test/accuracy/batched.yaml harness=(-lease-k 10 -config-version 32) ;;
*) echo "accuracy: MODE must be strict or batched, not $mode" >&2; exit 2 ;;
esac

compose=(docker compose --env-file .env.example)
if [ -f .env.local ]; then compose+=(--env-file .env.local); fi

# gateway-2 … gateway-N, started here and removed on exit, so that make
# verify-m0 finds no stopped container.
replicas=()
for ((i = 2; i <= n; i++)); do replicas+=("gateway-$i"); done # not seq: BSD seq 2 1 counts down

dir=deploy/compose/gateway
tmp=$(mktemp -d)
cp "$dir/config.yaml" "$tmp/config.yaml"
swap() { cp "$1" "$dir/.config.yaml.swap" && mv -f "$dir/.config.yaml.swap" "$dir/config.yaml"; }
restore() {
	swap "$tmp/config.yaml"
	rm -rf "$tmp"
	if [ ${#replicas[@]} -gt 0 ]; then "${compose[@]}" --profile replicas rm -fsv "${replicas[@]}" >/dev/null 2>&1; fi
}
trap restore EXIT
trap 'exit 1' INT TERM

"${compose[@]}" build --quiet accuracy || exit 1
swap "$config"
# make dev has started their dependencies, so only the gateways start here.
if [ ${#replicas[@]} -gt 0 ]; then
	"${compose[@]}" --profile replicas up -d --no-deps --wait "${replicas[@]}" || exit 1
fi

# The harness waits until every gateway runs the config and decides its
# probe requests in Redis (warm-up, ADR-0024) before it measures.
"${compose[@]}" run --rm -T accuracy -replicas "$n" ${harness[@]+"${harness[@]}"} # bash 3.2: an empty array is unset under set -u
