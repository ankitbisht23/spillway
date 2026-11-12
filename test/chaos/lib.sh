# Shared by the Redis chaos scripts (NFR-13, ADR-0021), sourced from the
# repository root. They swap their config into the gateways' mounted
# config.yaml as the verify scripts do (ADR-0019) and, on every exit, bring
# the cluster back, put the committed config back and stop gateway-2 …
# gateway-5. Each script sets probe, the curl arguments of a request that
# gateway-1 decides in Redis under its config.
# shellcheck shell=bash

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

nodes=(redis-0 redis-1 redis-2 redis-3 redis-4 redis-5)

dir=deploy/compose/gateway
tmp=$(mktemp -d)
cp "$dir/config.yaml" "$tmp/config.yaml"
swapped=
swap() { swapped=1; cp "$1" "$dir/.config.yaml.swap" && mv -f "$dir/.config.yaml.swap" "$dir/config.yaml"; }

# Nothing touches the cluster before the config swap, so an exit before it
# (a failed image build) has nothing to restore.
restored=
cleanup() {
	if [ -z "$restored" ] && [ -n "$swapped" ]; then restore; fi
	swap "$tmp/config.yaml"
	rm -rf "$tmp"
	"${compose[@]}" --profile replicas stop gateway-2 gateway-3 gateway-4 gateway-5 >/dev/null 2>&1
}
trap cleanup EXIT
trap 'exit 1' INT TERM

# rcli <node> <args>: redis-cli on <node>, without the replies' CRs.
rcli() { "${compose[@]}" exec -T "$1" redis-cli "${@:2}" 2>/dev/null | tr -d '\r'; }

# cluster_ok: every node reports cluster_state:ok, all 16384 slots ok and
# six known nodes, and redis-0 sees three primaries with slots and one
# replica each, every link connected and no node failed.
cluster_ok() {
	local n info
	for n in "${nodes[@]}"; do
		info=$(rcli "$n" cluster info)
		grep -q '^cluster_state:ok$' <<<"$info" && grep -q '^cluster_slots_ok:16384$' <<<"$info" &&
			grep -q '^cluster_known_nodes:6$' <<<"$info" || return 1
	done
	rcli redis-0 cluster nodes | awk '
		$3 ~ /fail|handshake|noaddr/ || $8 != "connected" { bad = 1 }
		$3 ~ /master/ && NF > 8 { primary[$1] = 1; primaries++ }
		$3 ~ /slave/ { replicas[$4]++; nreplicas++ }
		END {
			if (bad || primaries != 3 || nreplicas != 3) exit 1
			for (id in primary) if (replicas[id] != 1) exit 1
		}'
}

wait_cluster() { # wait_cluster <seconds>
	local deadline=$((SECONDS + $1))
	until cluster_ok; do
		if [ "$SECONDS" -ge "$deadline" ]; then return 1; fi
		sleep 0.5
	done
}

# rejoin <node>: a node back from docker kill is empty (its nodes.conf is on
# tmpfs). Every other node forgets the failed node IDs it knows, and <node>
# joins as a replica of the primary with the fewest replicas.
rejoin() {
	local m id seed=
	for m in "${nodes[@]}"; do
		if [ "$m" = "$1" ]; then continue; fi
		seed=$m
		for id in $(rcli "$m" cluster nodes | awk '$3 ~ /fail/ { print $1 }'); do
			rcli "$m" cluster forget "$id" >/dev/null
		done
	done
	"${compose[@]}" exec -T "$1" redis-cli --cluster add-node "$1:6379" "$seed:6379" --cluster-slave >/dev/null 2>&1
}

# restore_cluster: unpauses or starts every node that is not running,
# rejoins the empty ones and waits for cluster_ok; if that fails, recreates
# the cluster (ADR-0021).
restore_cluster() {
	local n id deadline
	for n in "${nodes[@]}"; do
		id=$("${compose[@]}" ps -aq "$n")
		case $(docker inspect -f '{{.State.Status}}' "$id" 2>/dev/null) in
		running) ;;
		paused) "${compose[@]}" unpause "$n" >/dev/null 2>&1 ;;
		*) "${compose[@]}" start "$n" >/dev/null 2>&1 ;;
		esac
	done
	deadline=$((SECONDS + 30))
	for n in "${nodes[@]}"; do
		until [ "$(rcli "$n" ping)" = PONG ] || [ "$SECONDS" -ge "$deadline" ]; do sleep 0.2; done
		if [ "$(rcli "$n" cluster info | sed -n 's/^cluster_known_nodes://p')" = 1 ]; then rejoin "$n"; fi
	done
	if wait_cluster 60; then return 0; fi
	echo "the cluster did not recover in place; recreating it" >&2
	"${compose[@]}" up -d --wait --force-recreate "${nodes[@]}" >/dev/null 2>&1 &&
		"${compose[@]}" run --rm --no-deps -T redis-init >/dev/null 2>&1 &&
		wait_cluster 60
}

# decided: gateway-1 answers probe with Spillway-Decision-Time.
decided() {
	curl -sS -o /dev/null -D - --max-time 2 "${probe[@]}" 2>/dev/null | grep -qi '^spillway-decision-time:'
}
wait_decided() { # wait_decided <seconds>
	local deadline=$((SECONDS + $1))
	until decided; do
		if [ "$SECONDS" -ge "$deadline" ]; then return 1; fi
		sleep 0.2
	done
}

# restore: restore_cluster, then gateway-1 decides probe in Redis again;
# prints its check line.
restore() {
	restored=1
	local start=$SECONDS
	if restore_cluster && wait_decided 30; then
		result PASS "cluster restored" "cluster_state:ok on all 6 nodes, 16384 slots, 3 primaries with a replica each, gateway-1 decides in Redis; took $((SECONDS - start)) s"
	else
		result FAIL "cluster restored" "see docker compose ps and redis-cli cluster nodes; docker compose down -v and make dev reset the stack"
	fi
}
