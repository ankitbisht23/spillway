#!/usr/bin/env bash
# EXPLORATORY ONLY (task nfr-scaling): diagnostic probes of the NFR-02
# layout, never a measured result. Prints a text report on stdout.
set -euo pipefail
cd "$(dirname "$0")/../.."
r=${BENCH_RATE:-4600}
net=spillway-scaling
cleanup() { docker rm -f $net-gateway $net-upstream $net-loadgen >/dev/null 2>&1 || true; docker network rm $net >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup
echo "== host"; lscpu | grep -E "Model name|^CPU\(s\)|Thread|Core|Socket|NUMA|L2|L3" || true
for c in 0 1 2 3; do echo "cpu$c caches: $(for i in /sys/devices/system/cpu/cpu$c/cache/index*; do printf '%s:%s ' "$(cat $i/level)$(cat $i/type | cut -c1)" "$(cat $i/shared_cpu_list)"; done)"; done
uname -r; nproc
docker build -q --target gateway -t spillway-gateway:bench . >/dev/null
docker build -q --target loadgen -t spillway-loadgen:bench . >/dev/null
docker network create $net >/dev/null
subnet=$(docker network inspect -f '{{(index .IPAM.Config 0).Subnet}}' $net)
usage() { curl -sf --unix-socket /var/run/docker.sock "http://localhost/containers/$1/stats?stream=false&one-shot=true" | jq '.cpu_stats.cpu_usage.total_usage / 1000 | floor'; }
sched() { local pid; pid=$(docker inspect -f '{{.State.Pid}}' $net-gateway); cat /proc/"$pid"/task/*/schedstat | awk '{r+=$1; w+=$2; n++} END{printf "%d %d %d", r, w, n}'; }
now() { date +%s.%N; }
# probe <label> <gateway cpus> <n> <loadgen cpus> <upstream cpus> <conns> <max-in-flight> [gateway env...]
probe() {
	local label=$1 gw=$2 n=$3 lc=$4 uc=$5 conns=$6 mif=$7; shift 7
	docker rm -f $net-gateway $net-upstream >/dev/null 2>&1 || true
	docker run -d --name $net-upstream --network $net --network-alias upstream --cpuset-cpus "$uc" --ulimit nofile=65536 --read-only \
		spillway-loadgen:bench serve -addr :8080 -bytes 1024 >/dev/null
	local envs=()
	for e in "$@"; do envs+=(-e "$e"); done
	docker run -d --name $net-gateway --network $net --network-alias gateway --cpuset-cpus "$gw" --ulimit nofile=65536 --read-only \
		-v "$PWD/test/load/gateway.yaml:/etc/spillway/config.yaml:ro" \
		-e SPILLWAY_GATEWAY_CONFIG=/etc/spillway/config.yaml -e SPILLWAY_GATEWAY_HTTP_ADDR=:8000 -e SPILLWAY_GATEWAY_ADMIN_ADDR=:8001 \
		-e SPILLWAY_GATEWAY_ALLOWED_NETWORKS="$subnet" -e SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE=0.01 "${envs[@]}" spillway-gateway:bench >/dev/null
	sleep 3
	local ip; ip=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$net\").IPAddress}}" $net-gateway)
	local rate=$((3 * r * n))
	docker run -d --name $net-loadgen --network $net --cpuset-cpus "$lc" --ulimit nofile=65536 --read-only \
		spillway-loadgen:bench -url http://gateway:8000/bytes/1024 -rate "$rate" -warmup 8s -duration 24s -conns "$conns" -max-in-flight "$mif" >/dev/null
	sleep 9
	local t0 g0 l0 u0 s0 q0 t1 g1 l1 u1 s1 q1
	t0=$(now); g0=$(usage $net-gateway); l0=$(usage $net-loadgen); u0=$(usage $net-upstream); s0=$(grep '^cpu[0-3] ' /proc/stat); q0=$(sched)
	sleep 6
	local gr; gr=$(curl -s "http://$ip:8001/debug/pprof/goroutine?debug=1" | grep -cE "persistConn\)\.roundTrip" || true)
	curl -s "http://$ip:8001/metrics" | grep -E '^go_sched_gomaxprocs_threads|^go_gc_duration_seconds_count' | tr '\n' ' ' || true
	echo
	sleep 6
	t1=$(now); g1=$(usage $net-gateway); l1=$(usage $net-loadgen); u1=$(usage $net-upstream); s1=$(grep '^cpu[0-3] ' /proc/stat); q1=$(sched)
	docker wait $net-loadgen >/dev/null
	local res; res=$(docker logs $net-loadgen 2>/dev/null); docker rm -f $net-loadgen >/dev/null
	local dt; dt=$(awk -v a="$t0" -v b="$t1" 'BEGIN{print b - a}')
	read -r r0 w0 n0 <<<"$q0"; read -r r1 w1 n1 <<<"$q1"
	echo "== $label: gw cpus $gw, loadgen $lc, upstream $uc, conns $conns, max-in-flight $mif, offered $rate, env [$*]"
	echo "  $(jq -r '"\((.status["2xx"]//0)/.duration_s|round) 2xx/s, p50 \(.latency_us.p50/1000|round) ms, p99 \(.latency_us.p99/1000|round) ms, errors \(.errors)"' <<<"$res")"
	awk -v dt="$dt" -v g="$((g1 - g0))" -v l="$((l1 - l0))" -v u="$((u1 - u0))" -v rw="$((w1 - w0))" -v rr="$((r1 - r0))" -v nt="$n1" \
		'BEGIN{printf "  vCPU gateway %.3f, loadgen %.3f, upstream %.3f (loadgen+upstream %.3f); gateway threads %d: run %.3f, runqueue wait %.3f s/s\n", g/1e6/dt, l/1e6/dt, u/1e6/dt, (l+u)/1e6/dt, nt, rr/1e9/dt, rw/1e9/dt}'
	echo "  upstream round trips in flight (one snapshot): $gr"
	paste <(echo "$s0") <(echo "$s1") | awk -v dt="$dt" -v hz="$(getconf CLK_TCK)" '{n=NF/2; printf "  %s", $1; split("user nice sys idle iow irq sirq steal",h," "); for(i=2;i<=9;i++) printf " %s=%.3f", h[i-1], ($(i+n)-$i)/hz/dt; printf "\n"}'
}
for rep in 1 2; do
	echo "######## repetition $rep"
	probe "n=1 new layout, loadgen default in-flight" 0 1 1,3 1,3 200 10000
	probe "n=1 new layout, in-flight 400" 0 1 1,3 1,3 200 400
	probe "n=2 new layout, loadgen default in-flight" 0,2 2 1,3 1,3 200 10000
	probe "n=2 new layout, in-flight 400" 0,2 2 1,3 1,3 200 400
	probe "n=2 new layout, in-flight 800, conns 400" 0,2 2 1,3 1,3 400 800
	probe "n=2 new layout, in-flight 400, GOMAXPROCS=4 (diagnostic)" 0,2 2 1,3 1,3 200 400 GOMAXPROCS=4
	probe "n=2 old layout (loadgen 1, upstream 3), in-flight 10000" 0,2 2 1 3 200 10000
done
