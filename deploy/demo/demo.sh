#!/usr/bin/env bash
# The demo stack's commands (SC-01, ADR-0094 §1, §2, §7), run by the Makefile:
#
#   demo.sh up          make demo: boot the stack with the demo override and the seed
#   demo.sh reset       make demo-reset: drop and re-seed spillway_demo, restart the demo gateways
#   demo.sh down        make demo-down, before make dev: delete the demo proxies, remove the demo containers
#   demo.sh down-check  make demo-down, after make dev: no demo container, no demo proxy
#   demo.sh clean       make dev, before up: remove every demo container
#   demo.sh fault <redis-cut|upstream-slow> <1|0>, demo.sh fault clear
#                       make demo-fault-redis, -upstream, -clear: switch a demo
#                       fault through the control plane's setDemoFault (SC-03)
#
# None of them removes a volume, touches the dev database spillway or
# Redis, or prints a secret or key.
set -euo pipefail
cd "$(dirname "$0")/../.."

compose=(docker compose --env-file .env.example)
if [ -f .env.local ]; then compose+=(--env-file .env.local); fi
demo=("${compose[@]}" -f compose.yaml -f deploy/compose/demo.compose.yml --profile replicas --profile demo)

# Every long-running service of the demo. Naming them keeps gateway-4 and
# gateway-5 down although the replicas profile is on; the one-shots start
# as their dependencies.
services=(
	redis-0 redis-1 redis-2 redis-3 redis-4 redis-5 postgres keycloak
	otel-collector tempo loki prometheus redis-exporter grafana toxiproxy
	upstream-1 upstream-2 upstream-3 controlplane gateway-1 gateway-2 gateway-3
)

# demo_containers lists the IDs of the spillway project's demo containers.
demo_containers() {
	docker ps -aq --filter label=spillway.demo=true --filter label=com.docker.compose.project=spillway
}

# epoch prints an RFC 3339 time (Docker's) as Unix seconds, on GNU or BSD date.
epoch() {
	date -u -d "$1" +%s 2>/dev/null || date -u -j -f '%Y-%m-%dT%H:%M:%S' "${1%%.*}" +%s
}

# mtime prints a file's modification time as Unix seconds, on GNU or BSD stat.
mtime() {
	stat -c %Y "$1" 2>/dev/null || stat -f %m "$1"
}

# Keycloak imports the realm only when its container is created (setup.md
# Step 4), so a realm.json newer than the running container recreates it.
realm_fresh() {
	local id started
	id=$("${compose[@]}" ps -q keycloak)
	[ -n "$id" ] || return 0
	started=$(docker inspect -f '{{.State.StartedAt}}' "$id")
	if [ "$(mtime deploy/compose/keycloak/realm.json)" -gt "$(epoch "$started")" ]; then
		echo "demo: deploy/compose/keycloak/realm.json changed; recreating keycloak"
		"${demo[@]}" up -d --force-recreate --wait --wait-timeout 300 keycloak
	fi
}

up() {
	local start=$SECONDS
	realm_fresh
	DEMO_MODE=true "${demo[@]}" up -d --build --wait --wait-timeout 300 "${services[@]}"
	"${demo[@]}" logs --no-log-prefix demo-seed | tail -n 1
	cat <<-EOF
		demo: up in $((SECONDS - start)) s
		  admin UI       http://localhost:8080 (the realm's test users, docs/setup.md Step 4)
		  gateway-1      http://localhost:8000 (gateway-2 and gateway-3 inside the network)
		  Grafana        http://localhost:3000
		  make demo-reset restores the seed; make demo-down returns to make dev
	EOF
}

# fault switches a demo fault, or with clear both off, through the control
# plane's setDemoFault, as admin with the spillway-ci service token (SC-03,
# ADR-0095 §5): demo-traffic fault in a one-off container of the demo-seed
# service, which has the control plane's and Keycloak's URLs and the token's
# client. --no-deps, so it never starts or recreates another service. With
# demo mode off it fails with demo-traffic's demo-mode-off message.
fault() {
	local args
	case "${1:-} ${2:-}" in
	"redis-cut 1" | "upstream-slow 1") args=("$1" on) ;;
	"redis-cut 0" | "upstream-slow 0") args=("$1" off) ;;
	"clear ") args=(clear) ;;
	*)
		echo "usage: $0 fault <redis-cut|upstream-slow> <1|0>, or $0 fault clear (make demo-fault-redis ON=1|0, make demo-fault-upstream ON=1|0, make demo-fault-clear)" >&2
		exit 2
		;;
	esac
	"${demo[@]}" run --rm --no-deps demo-seed fault "${args[@]}"
}

reset() {
	local start=$SECONDS id
	id=$("${compose[@]}" ps -q controlplane)
	if [ -z "$id" ] || [ "$(docker inspect -f '{{index .Config.Labels "spillway.demo"}}' "$id")" != "true" ]; then
		echo "demo-reset: the demo is not running; start it with make demo" >&2
		exit 1
	fi
	# Both faults off first, while the control plane answers (ADR-0094 §1):
	# the reset keeps the proxies, so a fault left on would outlive it. If
	# the control plane cannot, the proxies are created again, without
	# toxics; the demo gateways restart below anyway.
	if ! fault clear; then
		echo "demo-reset: the faults could not be switched off through the control plane; creating the demo proxies again" >&2
		"${demo[@]}" run --rm --no-deps demo-init
	fi
	"${demo[@]}" stop controlplane
	"${compose[@]}" exec -T postgres psql -U spillway -d spillway -v ON_ERROR_STOP=1 -qc 'DROP DATABASE IF EXISTS spillway_demo WITH (FORCE)'
	# demo-db and demo-migrate run again as the control plane's
	# dependencies; up --wait cannot name a one-shot, so the seed runs on
	# its own, in the foreground.
	DEMO_MODE=true "${demo[@]}" up -d --wait --wait-timeout 300 controlplane
	DEMO_MODE=true "${demo[@]}" run --rm --no-deps demo-seed
	# From the new config, not from a snapshot of the dropped database.
	DEMO_MODE=true "${demo[@]}" up -d --wait --wait-timeout 300 --force-recreate --no-deps gateway-1 gateway-2 gateway-3
	echo "demo-reset: done in $((SECONDS - start)) s"
}

down() {
	local id
	# Both faults off through the API while the demo control plane runs
	# (ADR-0094 §2), so the switch is audited; deleting the proxies below
	# removes any toxic left either way.
	id=$("${compose[@]}" ps -q --status running controlplane)
	if [ -n "$id" ] && [ "$(docker inspect -f '{{index .Config.Labels "spillway.demo"}}' "$id")" = "true" ]; then
		fault clear || echo "demo-down: the faults could not be switched off through the control plane; deleting the proxies removes them" >&2
	fi
	if [ -n "$("${compose[@]}" ps -q --status running toxiproxy)" ]; then
		"${demo[@]}" run --rm --no-deps demo-init proxies -delete
	fi
	clean
}

down_check() {
	local left
	left=$(demo_containers | wc -l | tr -d ' ')
	if [ "$left" != 0 ]; then
		echo "demo-down: FAIL: $left demo containers left" >&2
		exit 1
	fi
	if curl -fsS http://127.0.0.1:8474/proxies | grep -q '"demo-'; then
		echo "demo-down: FAIL: toxiproxy still has demo proxies" >&2
		exit 1
	fi
	echo "demo-down: done"
}

clean() {
	local ids
	ids=$(demo_containers)
	if [ -n "$ids" ]; then
		# shellcheck disable=SC2086 # one ID per word
		docker rm -f $ids >/dev/null
		echo "removed $(wc -w <<<"$ids" | tr -d ' ') demo containers"
	fi
}

case "${1:-}" in
up) up ;;
reset) reset ;;
down) down ;;
down-check) down_check ;;
clean) clean ;;
fault) fault "${2:-}" "${3:-}" ;;
*)
	echo "usage: $0 up|reset|down|down-check|clean|fault" >&2
	exit 2
	;;
esac
