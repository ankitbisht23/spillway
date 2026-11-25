#!/usr/bin/env bash
# NFR-02 scaling run (docs/design.md, Benchmark method; ADR-0074 §D9,
# ADR-0083, ADR-0084). The gateway runs on 1 vCPU (CPU 0), then on 2 (CPUs 0
# and 2, never one SMT core), with loadgen and a no-op upstream (loadgen
# serve, 1,024 bytes) sharing CPUs 1 and 3 and no Redis, in a Docker network
# of its own.
# For n = 1, then 2:
#   1. probe: 3 runs of 10 s warm-up + 30 s at 3 × r × n rps (r = BENCH_RATE,
#      the arch's NFR-01 floor), -conns 200, -max-in-flight 400 (the excess is
#      dropped, not queued, ADR-0084); P(n) is the median of their 2xx
#      answers per second. During each probe's measured 30 s the script
#      reads every container's cgroup CPU usage (Docker's stats API, which
#      reports cpu.stat usage_usec): a probe is gateway-bound when the
#      gateway used >= 0.95 × n vCPU and loadgen and the upstream together
#      <= 1.80 vCPU.
#   2. measured: 5 runs of 30 s + 3 min at R(n) = 0.9 × P(n), rounded down
#      to 100 (ADR-0016), each valid as in bench.sh (99% of target, every
#      answer 2xx).
# The result is valid when both run sets are valid and every probe was
# gateway-bound, and passes when it is valid and S = R(2) ÷ R(1) >= 1.70.
# Prints one JSON result on stdout (setup.mode "scaling", never comparable
# with throughput results); progress goes to stderr. bench.yml runs it with
# mode scaling on both runners; on a laptop it needs Docker with at least 4
# CPUs (exploratory only, ADR-0016):
#   test/load/scaling.sh > scaling.json
#   SCALING_PROBE_RUNS=1 SCALING_PROBE_DURATION=10s BENCH_RUNS=1 BENCH_WARMUP=5s BENCH_DURATION=20s test/load/scaling.sh   # a quick look
set -euo pipefail
cd "$(dirname "$0")/../.."

r=${BENCH_RATE:-4600}
probe_runs=${SCALING_PROBE_RUNS:-3}
probe_warmup=${SCALING_PROBE_WARMUP:-10s}
probe_duration=${SCALING_PROBE_DURATION:-30s}
runs=${BENCH_RUNS:-5}
warmup=${BENCH_WARMUP:-30s}
duration=${BENCH_DURATION:-3m}
conns=200
probe_factor=3
probe_max_in_flight=$((2 * conns)) # ADR-0084: a probe's excess is dropped, not queued in loadgen
loadgen_cpus=1,3 upstream_cpus=1,3 # shared (ADR-0084)
gateway_cpus_1=0 gateway_cpus_2=0,2
sample_rate=0.01 # as bench.sh (ADR-0015); no OTLP exporter, so no Collector shares a CPU
min_gateway=0.95 max_side=1.80 min_scaling=1.70
net=spillway-scaling

log() { echo "scaling: $*" >&2; }

cleanup() {
	docker rm -f "$net-gateway" "$net-upstream" "$net-loadgen" >/dev/null 2>&1 || true
	docker network rm "$net" >/dev/null 2>&1 || true
}

ready() { # ready <container> <command...>: as bench.sh, up to 60 s
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

seconds() { # seconds <Go duration in s or m>
	case $1 in
	*m) echo $((${1%m} * 60)) ;;
	*s) echo "${1%s}" ;;
	*) log "bad duration $1"; exit 2 ;;
	esac
}

now() { perl -MTime::HiRes -e 'printf "%.6f\n", Time::HiRes::time()'; }

# usage <container>: its cgroup CPU time so far, in microseconds.
usage() {
	curl -sf --unix-socket /var/run/docker.sock "http://localhost/containers/$1/stats?stream=false&one-shot=true" |
		jq '.cpu_stats.cpu_usage.total_usage / 1000 | floor'
}

# cpu_snapshot: "<time> <gateway µs> <loadgen µs> <upstream µs>".
cpu_snapshot() {
	echo "$(now) $(usage "$net-gateway") $(usage "$net-loadgen") $(usage "$net-upstream")"
}

# loadgen_run <rate> <warmup> <duration> [probe]: one loadgen run on its
# CPUs; prints its JSON result, with the containers' vCPUs over the measured
# time when probe is set. A probe holds at most probe_max_in_flight requests
# in flight; a measured run keeps loadgen's default (10,000), so that a
# short stall cannot drop its requests.
loadgen_run() {
	local rate=$1 w=$2 d=$3 probe=${4:-} a b out extra=()
	if [ -n "$probe" ]; then extra=(-max-in-flight "$probe_max_in_flight"); fi
	docker run -d --name "$net-loadgen" --network "$net" --cpuset-cpus "$loadgen_cpus" --ulimit nofile=65536 --read-only \
		spillway-loadgen:bench -url http://gateway:8000/bytes/1024 -rate "$rate" -warmup "$w" -duration "$d" -conns "$conns" ${extra[@]+"${extra[@]}"} >/dev/null
	if [ -n "$probe" ]; then
		# From 1 s into the measured time to 1 s before its end.
		sleep $(($(seconds "$w") + 1))
		a=$(cpu_snapshot)
		sleep $(($(seconds "$d") - 2))
		b=$(cpu_snapshot)
	fi
	docker wait "$net-loadgen" >/dev/null
	out=$(docker logs "$net-loadgen" 2>/dev/null)
	docker rm -f "$net-loadgen" >/dev/null
	if ! jq -e .achieved_rps <<<"$out" >/dev/null 2>&1; then
		log "loadgen printed no result"
		exit 1
	fi
	if [ -n "$probe" ]; then
		read -r ta ga la ua <<<"$a"
		read -r tb gb lb ub <<<"$b"
		jq -c --argjson dt "$(perl -e "print $tb - $ta")" --argjson g "$((gb - ga))" --argjson l "$((lb - la))" --argjson u "$((ub - ua))" \
			'. + {vcpu: {window_s: $dt, gateway: ($g / 1e6 / $dt), loadgen: ($l / 1e6 / $dt), upstream: ($u / 1e6 / $dt)}}' <<<"$out"
	else
		jq -c . <<<"$out"
	fi
}

# step <n> <gateway cpus>: the probes and the measured run set with the
# gateway on n vCPUs; prints the step's JSON.
step() {
	local n=$1 cpus=$2 target probes=() set=() p rr out i
	docker rm -f "$net-gateway" >/dev/null 2>&1 || true
	docker run -d --name "$net-gateway" --network "$net" --network-alias gateway \
		--cpuset-cpus "$cpus" --ulimit nofile=65536 --read-only \
		-v "$PWD/test/load/gateway.yaml:/etc/spillway/config.yaml:ro" \
		-e SPILLWAY_GATEWAY_CONFIG=/etc/spillway/config.yaml \
		-e SPILLWAY_GATEWAY_HTTP_ADDR=:8000 \
		-e SPILLWAY_GATEWAY_ALLOWED_NETWORKS="$subnet" \
		-e SPILLWAY_GATEWAY_ACCESS_LOG_SAMPLE_RATE="$sample_rate" \
		spillway-gateway:bench >/dev/null
	ready "$net-gateway" /gateway healthcheck

	target=$((probe_factor * r * n))
	for i in $(seq "$probe_runs"); do
		log "n=$n probe $i/$probe_runs: $target rps offered, $probe_warmup + $probe_duration"
		out=$(loadgen_run "$target" "$probe_warmup" "$probe_duration" probe)
		log "n=$n probe $i: $(jq -r '"\((.status["2xx"] // 0) / .duration_s | round) 2xx/s; vCPU gateway \(.vcpu.gateway * 100 | round / 100), loadgen \(.vcpu.loadgen * 100 | round / 100), upstream \(.vcpu.upstream * 100 | round / 100), together \((.vcpu.loadgen + .vcpu.upstream) * 100 | round / 100)"' <<<"$out")"
		probes+=("$out")
	done
	p=$(printf '%s\n' "${probes[@]}" | jq -s '[.[] | (.status["2xx"] // 0) / .duration_s] | sort | .[(length - 1) / 2 | floor]')
	rr=$(jq -n --argjson p "$p" '($p * 0.9 / 100 | floor) * 100')
	log "n=$n: P = $(jq -n "$p | round") 2xx/s, R = $rr rps"
	if [ "$rr" -le 0 ]; then
		log "n=$n: R is 0, no measured run set"
	else
		for i in $(seq "$runs"); do
			log "n=$n run $i/$runs: $rr rps, $warmup warm-up, $duration measured"
			out=$(loadgen_run "$rr" "$warmup" "$duration")
			log "n=$n run $i: $(jq -r '"\(.achieved_rps | round) rps, p99 \(.latency_us.p99) µs"' <<<"$out")"
			set+=("$out")
		done
	fi
	{
		printf '%s\n' "${probes[@]}" | jq -cs .
		if [ "${#set[@]}" -gt 0 ]; then printf '%s\n' "${set[@]}" | jq -cs .; else echo '[]'; fi
	} | jq -s -L test/load --argjson n "$n" --arg cpus "$cpus" --argjson p "$p" --argjson r "$rr" \
		--argjson min_gw "$min_gateway" --argjson max_side "$max_side" '
		include "scaling";
		def stats(f): [.[] | f] | sort as $v | ($v | length) as $k
			| if $k == 0 then null else
				(if $k % 2 == 1 then $v[($k - 1) / 2] else ($v[$k / 2 - 1] + $v[$k / 2]) / 2 end) as $m
				| {median: $m, min: $v[0], max: $v[-1], spread: (if $m > 0 then ($v[-1] - $v[0]) / $m else 0 end)} end;
		def problems(i):
			(if .valid then empty else "n=\($n) run \(i): achieved \(.achieved_rps | round) rps, below 99% of \(.target_rps)" end),
			(.errors | add // 0 | if . > 0 then "n=\($n) run \(i): \(.) requests unanswered" else empty end),
			([.status | to_entries[] | select(.key != "2xx") | .value] | add // 0 | if . > 0 then "n=\($n) run \(i): \(.) answers not 2xx" else empty end);
		.[0] as $probes | .[1] as $set
		| [$probes[] | . + {rps_2xx: ((.status["2xx"] // 0) / .duration_s),
			gateway_bound: gateway_bound($n; $min_gw; $max_side)}] as $probes
		| ([$set | to_entries[] | .key as $k | .value | problems($k + 1)]
			+ (if ($set | length) == 0 then ["n=\($n): no measured run set (R = 0)"] else [] end)) as $set_reasons
		| ([$probes | to_entries[] | select(.value.gateway_bound | not)
			| .key as $k | .value | not_bound_reason($n; $k + 1; $min_gw; $max_side)]) as $probe_reasons
		| {
			gateway_vcpus: $n, gateway_cpus: $cpus,
			p: $p, r: $r,
			probes_gateway_bound: ($probe_reasons | length == 0),
			run_set_valid: ($set_reasons | length == 0),
			invalid_reasons: ($probe_reasons + $set_reasons),
			achieved_rps: ($set | stats(.achieved_rps)),
			latency_us: {p50: ($set | stats(.latency_us.p50)), p99: ($set | stats(.latency_us.p99)), p99_9: ($set | stats(.latency_us.p99_9))},
			probes: $probes,
			runs: $set
		}'
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

docker network create "$net" >/dev/null
subnet=$(docker network inspect -f '{{(index .IPAM.Config 0).Subnet}}' "$net")

docker run -d --name "$net-upstream" --network "$net" --network-alias upstream \
	--cpuset-cpus "$upstream_cpus" --ulimit nofile=65536 --read-only \
	spillway-loadgen:bench serve -addr :8080 -bytes 1024 >/dev/null
ready "$net-upstream" /loadgen serve -addr :8080 -bytes 1024 -healthcheck

one=$(step 1 "$gateway_cpus_1")
two=$(step 2 "$gateway_cpus_2")

hardware_model=$(jq -r '.runs[0].hardware.cpu_model // .probes[0].hardware.cpu_model // ""' <<<"$one")
result=$(jq -n --argjson one "$one" --argjson two "$two" \
	--arg timestamp "$timestamp" --arg commit "$commit" --arg ref "$ref" --arg run_url "$run_url" --arg arch "$arch" \
	--argjson host "$host" --argjson topology "$topology" --arg cpu_model "$hardware_model" \
	--argjson r "$r" --argjson conns "$conns" --argjson factor "$probe_factor" --argjson probe_runs "$probe_runs" --argjson probe_mif "$probe_max_in_flight" \
	--arg probe_warmup "$probe_warmup" --arg probe_duration "$probe_duration" --arg warmup "$warmup" --arg duration "$duration" \
	--arg loadgen_cpus "$loadgen_cpus" --arg upstream_cpus "$upstream_cpus" --argjson sample_rate "$sample_rate" \
	--argjson min_scaling "$min_scaling" --argjson min_gw "$min_gateway" --argjson max_side "$max_side" '
	($one.invalid_reasons + $two.invalid_reasons) as $reasons
	| (if $one.r > 0 then $two.r / $one.r else null end) as $s
	| ($reasons | length == 0) as $valid
	| {
		timestamp: $timestamp, commit: $commit, ref: $ref, run_url: $run_url, arch: $arch,
		valid: $valid, invalid_reasons: $reasons,
		scaling: $s, pass: ($valid and $s != null and $s >= $min_scaling),
		rule: {min_scaling: $min_scaling, gateway_bound: {gateway_min_vcpu_per_n: $min_gw, loadgen_plus_upstream_max_vcpu: $max_side}},
		setup: {
			mode: "scaling",
			cpus: {gateway_1: $one.gateway_cpus, gateway_2: $two.gateway_cpus, loadgen: $loadgen_cpus, upstream: $upstream_cpus},
			upstream: "loadgen serve -bytes 1024", url: "http://gateway:8000/bytes/1024",
			rate_per_vcpu: $r, conns: $conns,
			probe: {runs: $probe_runs, offered: "\($factor) x rate_per_vcpu x n", max_in_flight: $probe_mif, warmup: $probe_warmup, duration: $probe_duration},
			warmup: $warmup, duration: $duration,
			access_log_sample_rate: $sample_rate, otlp: "none"
		},
		hardware: ({cpu_model: $cpu_model} + ($host | del(.arch)) + {topology: $topology}),
		n1: $one, n2: $two
	}')
printf '%s\n' "$result"
log "$(jq -r '"\(.arch), \(.hardware.cpu_model): R(1) \(.n1.r), R(2) \(.n2.r) rps, S = \(.scaling // "n/a" | if type == "number" then (. * 1000 | round / 1000) else . end) (pass >= 1.70); "
	+ if .valid then (if .pass then "PASS" else "FAIL" end) else "INVALID: " + (.invalid_reasons | join("; ")) end' <<<"$result")"
