#!/bin/sh
# One-shot redis-init service (ADR-0004): creates the Redis Cluster on fresh
# nodes and waits until every node reports cluster_state:ok. Exits 0 when the
# cluster already exists, so `make dev` on a running stack still succeeds.
set -eu

nodes="redis-0 redis-1 redis-2 redis-3 redis-4 redis-5"

state_ok() { redis-cli -h "$1" cluster info | grep -q '^cluster_state:ok'; }

if state_ok redis-0; then
	echo "cluster already formed"
	exit 0
fi

addrs=""
for n in $nodes; do addrs="$addrs $n:6379"; done
# shellcheck disable=SC2086 # one argument per node
redis-cli --cluster create $addrs --cluster-replicas 1 --cluster-yes

for _ in $(seq 1 60); do
	ok=0
	for n in $nodes; do
		if state_ok "$n"; then ok=$((ok + 1)); fi
	done
	if [ "$ok" -eq 6 ]; then
		echo "cluster_state:ok on all 6 nodes"
		exit 0
	fi
	sleep 1
done
echo "cluster did not reach cluster_state:ok on every node" >&2
exit 1
