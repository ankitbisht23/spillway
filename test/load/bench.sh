#!/usr/bin/env bash
# NFR-01 throughput benchmark (docs/design.md, Benchmark method; ADR-0003,
# ADR-0015). Runs the gateway, the mock upstream and loadgen in a Docker
# network of their own, each pinned to its own CPUs (gateway 0, loadgen 1-2,
# upstream 3), and drives the gateway's plain-HTTP listener at a constant
# rate: 30 s warm-up and 3 min measured, 5 times. Prints one JSON result for
# the run set on stdout: the median and spread of each number, every run's
# loadgen result, the commit and the hardware, CPU topology included
# (ADR-0016). Progress goes to stderr.
# bench.yml runs it on GitHub Actions; on a laptop it needs Docker with at
# least 4 CPUs:
#   test/load/bench.sh > result.json
#   BENCH_RATE=5000 BENCH_RUNS=1 BENCH_WARMUP=5s BENCH_DURATION=30s test/load/bench.sh   # a quick look
# The run set is valid only if every run reached 99% of its target rate and
# every request got a 2xx; an invalid one is still printed, with the reasons.
set -euo pipefail
cd "$(dirname "$0")/../.."

rate=${BENCH_RATE:-10000}
runs=${BENCH_RUNS:-5}
warmup=${BENCH_WARMUP:-30s}
duration=${BENCH_DURATION:-3m}
gateway_cpus=0 loadgen_cpus=1-2 upstream_cpus=3
gateway_vcpus=1
# Logging every successful request would cost the gateway about 30% of its
# throughput; benchmarks log a fixed 1% of them (ADR-0015).
sample_rate=0.01
net=spillway-bench

log() { echo "bench: $*" >&2; }

cleanup() {
	docker rm -f "$net-gateway" "$net-upstream" >/dev/null 2>&1 || true
	docker network rm "$net" >/dev/null 2>&1 || true
}

# ready <container> <command...>: waits up to 60 s for the command to succeed
# in the container. The images have no shell, so docker run's --health-cmd
# can't run there.
ready() {
	local c=$1
	shift
	for _ in $(seq 60); do
		if docker exec "$c" "$@" >/dev/null 2>&1; then return 0; fi
		sleep 1
	done
	log "$c is not ready; its last logs:"
	docker logs --tail 20 "$c" >&2
	return 1
}

timestamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)
commit=$(git rev-parse HEAD)
ref=${GITHUB_REF:-$(git symbolic-ref -q HEAD || echo HEAD)}
run_url=${GITHUB_RUN_ID:+$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID}
host=$(docker info --format '{{json .}}' | jq -c '{cpus: .NCPU, memory_bytes: .MemTotal, os: .OperatingSystem, kernel: .KernelVersion, arch: .Architecture}')
case $(jq -r .arch <<<"$host") in
	x86_64) arch=x64 ;;
	aarch64) arch=arm64 ;;
	*) arch=$(jq -r .arch <<<"$host") ;;
esac
# Which logical CPUs share a core (SMT) or a socket, one entry per CPU from
# sysfs (ADR-0016). Null without sysfs, as on macOS, where Docker runs in a VM.
topology=null
if [ -d /sys/devices/system/cpu/cpu0/topology ]; then
	topology=$(for t in /sys/devices/system/cpu/cpu[0-9]*/topology; do
		cpu=${t%/topology}
		printf '{"cpu": %s, "core": %s, "socket": %s, "thread_siblings": "%s"}\n' \
			"${cpu##*cpu}" "$(<"$t/core_id")" "$(<"$t/physical_package_id")" "$(<"$t/thread_siblings_list")"
	done | jq -cs 'sort_by(.cpu)')
fi

trap cleanup EXIT
cleanup

log "building images"
docker build -q --target gateway -t spillway-gateway:bench . >/dev/null
docker build -q --target loadgen -t spillway-loadgen:bench . >/dev/null
docker build -q -t spillway-upstream:bench deploy/compose/upstream >/dev/null

docker network create "$net" >/dev/null
subnet=$(docker network inspect -f '{{(index .IPAM.Config 0).Subnet}}' "$net")

# go-httpbin with its per-request log off (ADR-0015).
docker run -d --name "$net-upstream" --network "$net" --network-alias upstream \
	--cpuset-cpus "$upstream_cpus" --ulimit nofile=65536 \
	spillway-upstream:bench -log-level OFF >/dev/null
ready "$net-upstream" /busybox wget -q -O /dev/null http://127.0.0.1:8080/status/200
# The gateway may reach only this network (SEC-06).
docker run -d --name "$net-gateway" --network "$net" --network-alias gateway \
	--cpuset-cpus "$gateway_cpus" --ulimit nofile=65536 --read-only \
	-v "$PWD/test/load/gateway.yaml:/etc/spillway/config.yaml:ro" \
	-e SPILLWAY_GATEWAY_CONFIG=/etc/spillway/config.yaml \
	-e SPILLWAY_GATEWAY_HTTP_ADDR=:8000 \
	-e SPILLWAY_GATEWAY_ALLOWED_NETWORKS="$subnet" \
	-e SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE="$sample_rate" \
	spillway-gateway:bench >/dev/null
ready "$net-gateway" /gateway healthcheck

results=()
for i in $(seq "$runs"); do
	log "run $i/$runs: $rate rps, $warmup warm-up, $duration measured"
	# loadgen exits 1 on an invalid run, which still counts in the set.
	out=$(docker run --rm --network "$net" --cpuset-cpus "$loadgen_cpus" --ulimit nofile=65536 --read-only \
		spillway-loadgen:bench -url http://gateway:8000/bytes/1024 -rate "$rate" -warmup "$warmup" -duration "$duration") || true
	if ! jq -e .achieved_rps <<<"$out" >/dev/null 2>&1; then
		log "run $i: loadgen printed no result"
		exit 1
	fi
	log "run $i: $(jq -r '"\(.achieved_rps | round) rps, p99 \(.latency_us.p99) µs"' <<<"$out")"
	results+=("$out")
done

result=$(printf '%s\n' "${results[@]}" | jq -s \
	--arg timestamp "$timestamp" --arg commit "$commit" --arg ref "$ref" --arg run_url "$run_url" --arg arch "$arch" \
	--argjson host "$host" --argjson topology "$topology" --argjson gateway_vcpus "$gateway_vcpus" --argjson sample_rate "$sample_rate" \
	--arg gateway_cpus "$gateway_cpus" --arg loadgen_cpus "$loadgen_cpus" --arg upstream_cpus "$upstream_cpus" \
	--arg warmup "$warmup" --arg duration "$duration" '
	# median, min, max and spread ((max - min) / median) of f across the runs
	def stats(f): [.[] | f] | sort as $v | ($v | length) as $n
		| (if $n % 2 == 1 then $v[($n - 1) / 2] else ($v[$n / 2 - 1] + $v[$n / 2]) / 2 end) as $m
		| {median: $m, min: $v[0], max: $v[-1], spread: (if $m > 0 then ($v[-1] - $v[0]) / $m else 0 end)};
	# why run i does not count
	def problems(i):
		(if .valid then empty else "run \(i): achieved \(.achieved_rps | round) rps, below 99% of \(.target_rps)" end),
		(.errors | add // 0 | if . > 0 then "run \(i): \(.) requests unanswered" else empty end),
		([.status | to_entries[] | select(.key != "2xx") | .value] | add // 0 | if . > 0 then "run \(i): \(.) answers not 2xx" else empty end);
	[to_entries[] | .key as $k | .value | problems($k + 1)] as $reasons
	| stats(.achieved_rps) as $rps
	| {
		timestamp: $timestamp, commit: $commit, ref: $ref, run_url: $run_url, arch: $arch,
		valid: ($reasons | length == 0), invalid_reasons: $reasons,
		target_rps: .[0].target_rps,
		rps_per_vcpu: ($rps.median / $gateway_vcpus),
		achieved_rps: $rps,
		latency_us: {p50: stats(.latency_us.p50), p99: stats(.latency_us.p99), p99_9: stats(.latency_us.p99_9)},
		setup: {
			gateway_vcpus: $gateway_vcpus,
			cpus: {gateway: $gateway_cpus, loadgen: $loadgen_cpus, upstream: $upstream_cpus},
			url: .[0].url, protocol: .[0].protocol, warmup: $warmup, duration: $duration,
			access_log_sample_rate: $sample_rate
		},
		hardware: ({cpu_model: .[0].hardware.cpu_model} + ($host | del(.arch)) + {topology: $topology}),
		runs: .
	}')
printf '%s\n' "$result"
log "$(jq -r '"\(.arch), \(.hardware.cpu_model): \(.rps_per_vcpu | round) rps per gateway vCPU (spread \(.achieved_rps.spread * 1000 | round / 10)%), p99 \(.latency_us.p99.median) µs; "
	+ if .valid then "valid" else "INVALID: " + (.invalid_reasons | join("; ")) end' <<<"$result")"
