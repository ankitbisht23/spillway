#!/usr/bin/env bash
# M5 exit checks (docs/design.md, M5; ADR-0042). gateway-1 … gateway-5 run
# in managed mode (--profile replicas) against the compose control plane,
# which serves test/verify/m5.yaml, and the compose service chaos ("chaos
# controlplane", in the compose network) checks, each once:
#   (a) NFR-08: 60 publishes each reach all five replicas, p99 < 5 s, from
#       published_at to the last replica's first apply heartbeat, both on
#       Postgres's clock;
#   (b) NFR-09: 20 revoked keys are each rejected on all five replicas
#       within 5 s, probed every 50 ms on the sender's clock;
#   (c) NFR-12: the control plane and Postgres stopped for 600 s under
#       500 rps, gateway-3 restarted from its on-disk snapshot at +300 s:
#       zero failed requests, config frozen at last good, sync age growing,
#       the restarted replica ready from its snapshot, recovery within 60 s;
#       (c6) compose Prometheus's SpillwayConfigSyncStale alert fires for
#       gateway-1 before the outage ends (OB-05, ADR-0074, ADR-0087).
# Every measured number is printed with PASS or FAIL. Exits 1 if any check
# fails; no check is ever run again. On every exit the control plane and
# Postgres are started again if they are stopped, the run's keys are
# revoked and its tenant and consumer deleted (published versions stay),
# gateway-1 is recreated in static mode and gateway-2 … gateway-5 are
# removed, as make dev leaves the stack. No committed file is changed.
# Run through make, about 18 minutes:
#   make verify-m5         (a), (b) and (c)
#   make chaos-cp-outage   (c) alone, about 14 minutes
set -uo pipefail
cd "$(dirname "$0")/../.."

mode=${1:-all}
case $mode in
all) checks=("(a)" "(b)" "(c1)" "(c2)" "(c3)" "(c4)" "(c5)" "(c6)") ;;
outage) checks=("(c1)" "(c2)" "(c3)" "(c4)" "(c5)" "(c6)") ;;
*)
	echo "usage: $0 [all|outage]" >&2
	exit 2
	;;
esac

fails=0
result() { # result PASS|FAIL check measured
	printf '%-4s  %-44s %s\n' "$1" "$2" "$3"
	if [ "$1" != PASS ]; then fails=$((fails + 1)); fi
}

compose=(docker compose --env-file .env.example)
if [ -f .env.local ]; then compose+=(--env-file .env.local); fi
gateways=(gateway-1 gateway-2 gateway-3 gateway-4 gateway-5)
run_id=$(date +%s)
tmp=$(mktemp -d)

# chaos <step> [flags]: one step of the sender. The spillway-ci client's
# secret reaches it from this environment if set there (never on a command
# line), else from .env.local, else it uses the dev value.
chaos() {
	local env=()
	if [ -n "${SPILLWAY_CI_CLIENT_SECRET:-}" ]; then env=(-e SPILLWAY_CI_CLIENT_SECRET); fi
	"${compose[@]}" run --rm -T ${env[@]+"${env[@]}"} chaos controlplane "$@" -run-id "$run_id"
}

# show <file>: the sender's PASS/FAIL and detail lines, not compose's
# progress lines.
show() { grep -E '^(PASS|FAIL) |^      |^chaos' "$1"; }

# sender_failed <code> <file> <what>: a sender that exited non-zero without
# a FAIL line is a failed check too.
sender_failed() {
	fails=$((fails + $(grep -c '^FAIL ' "$2")))
	if [ "$1" -ne 0 ] && ! grep -q '^FAIL ' "$2"; then
		result FAIL "$3: sender" "chaos controlplane exited $1: $(tail -1 "$2")"
	fi
}

# not_run <reason>: every check fails as setup.
not_run() {
	local c
	for c in "${checks[@]}"; do result FAIL "$c not run" "setup: $1"; done
	exit 1
}

managed= # the gateways run in managed mode
pid=     # the outage sender, while it runs
poller=  # (c6)'s alert poller, while the outage lasts
cleanup() {
	if [ -n "$pid" ]; then kill "$pid" 2>/dev/null; fi
	if [ -n "$poller" ]; then kill "$poller" 2>/dev/null; fi
	if [ "$("${compose[@]}" ps -q --status running controlplane postgres | wc -l)" -lt 2 ]; then
		echo "      restoring: starting the control plane and Postgres"
		"${compose[@]}" up -d --wait --wait-timeout 180 controlplane >/dev/null 2>&1 ||
			echo "      restoring: docker compose up controlplane failed"
	fi
	if ! chaos cleanup >/dev/null 2>"$tmp/cleanup.err"; then
		show "$tmp/cleanup.err"
		echo "      restoring: tenant m5-verify not removed; the next run removes it"
	fi
	if [ -n "$managed" ]; then
		# Removed, then created, not recreated in place (see
		# test/integration/managed_test.go's composeManaged).
		"${compose[@]}" rm -fsv gateway-1 >/dev/null 2>&1
		"${compose[@]}" up -d --no-deps --wait --wait-timeout 120 gateway-1 >/dev/null 2>&1 ||
			echo "      restoring: gateway-1 did not come back in static mode"
		"${compose[@]}" --profile replicas rm -fsv gateway-2 gateway-3 gateway-4 gateway-5 >/dev/null 2>&1
	fi
	rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 1' INT TERM

echo "== setup: test/verify/m5.yaml published, tenant and consumer m5-verify, gateway-1 … gateway-5 managed (ADR-0042)"
if ! build=$("${compose[@]}" build --quiet chaos 2>&1); then
	printf '%s\n' "$build"
	not_run "the chaos image did not build"
fi
if ! chaos setup <test/verify/m5.yaml >"$tmp/setup.json" 2>"$tmp/setup.err"; then
	show "$tmp/setup.err"
	not_run "chaos controlplane setup failed"
fi
managed=1
if ! up=$(SPILLWAY_GATEWAY_CONFIG='' SPILLWAY_GATEWAY_IDENTITY='' SPILLWAY_GATEWAY_CONTROL_PLANE=controlplane:9090 \
	"${compose[@]}" --profile replicas up -d --no-deps --force-recreate --wait --wait-timeout 120 "${gateways[@]}" 2>&1); then
	printf '%s\n' "$up"
	not_run "the managed gateways did not become ready"
fi
if ! chaos ready >"$tmp/ready.json" 2>"$tmp/ready.err"; then
	show "$tmp/ready.err"
	not_run "the replicas were not live at the published versions within 60 s"
fi
jq -r '"      config version \(.config_version), identity version \(.identity_version): all 5 replicas live after \(.took_ms / 1000 | floor) s"' "$tmp/ready.json"

if [ "$mode" = all ]; then
	echo "== (a) NFR-08: a publish reaches all replicas in < 5 s"
	chaos propagation >"$tmp/a.json" 2>"$tmp/a.err"
	code=$?
	show "$tmp/a.err"
	sender_failed "$code" "$tmp/a.err" "(a)"

	echo "== (b) NFR-09: a revoked key is rejected on all replicas in < 5 s"
	chaos revocation >"$tmp/b.json" 2>"$tmp/b.err"
	code=$?
	show "$tmp/b.err"
	sender_failed "$code" "$tmp/b.err" "(b)"
	# For information: the identity path on Postgres's clock, from the
	# revocation's revoked_at to the last replica's first heartbeat
	# reporting its identity version.
	ids=$(jq -r '[.rounds[]?.key_id | select(. > 0)] | map(tostring) | join(",")' "$tmp/b.json" 2>/dev/null)
	if [ -n "$ids" ]; then
		in=$(printf "'%s'," "${gateways[@]}")
		"${compose[@]}" exec -T postgres psql -U spillway -d spillway -AtF ' ' -c "
			WITH s AS (
				SELECT k.id, count(rv.replica_id) AS n,
					extract(epoch FROM max(rv.first_reported_at) - k.revoked_at) * 1000 AS ms
				FROM api_keys k
				LEFT JOIN replica_versions rv ON rv.stream = 'identity' AND rv.version = k.version AND rv.replica_id IN (${in%,})
				WHERE k.id IN ($ids) AND k.revoked_at IS NOT NULL
				GROUP BY k.id, k.revoked_at)
			SELECT count(*), count(*) FILTER (WHERE n = ${#gateways[@]}),
				coalesce(round(percentile_disc(0.5) WITHIN GROUP (ORDER BY ms) FILTER (WHERE n = ${#gateways[@]})), 0),
				coalesce(round(max(ms) FILTER (WHERE n = ${#gateways[@]})), 0)
			FROM s" 2>/dev/null |
			awk '{ printf "      identity path (Postgres clock, revoked_at to the last first report of its identity version): %d of %d on all 5, p50 %d ms, max %d ms\n", $2, $1, $3, $4 }'
	fi
fi

echo "== (c) NFR-12: gateways serve unchanged through a 10-minute control-plane outage (about 12.5 minutes)"
# The sender asks for each action on stderr ("action stop", "action
# restart gateway-3", "action start"), polled here; the answers go to its
# stdin through this FIFO: "stopping" just before docker compose stop,
# then "stopped <code>", "restarted <code>", "healthy <code>".
#
# (c6): from the stop until the start, a background poller asks compose's
# Prometheus every 5 s whether SpillwayConfigSyncStale fires for gateway-1
# and notes the first time it does, in seconds after the stop. It runs
# beside this loop so it never delays an answer to the sender. The alert
# needs 60 s of staleness and rules run every 15 s, so it fires about
# 65-80 s into the 600 s outage: a stall of up to a second cannot keep it
# from firing in time (ADR-0074, stall tolerance).
stale_poll() {
	local t0=$SECONDS
	while :; do
		if curl -sS --max-time 3 -G http://127.0.0.1:9091/api/v1/query \
			--data-urlencode 'query=ALERTS{alertname="SpillwayConfigSyncStale",alertstate="firing",replica="gateway-1"}' 2>/dev/null |
			jq -e '.data.result | length > 0' >/dev/null 2>&1; then
			echo "$((SECONDS - t0))" >"$tmp/c6"
			return
		fi
		sleep 5
	done
}
outage_s= # the outage's length as this loop saw it
mkfifo "$tmp/stdin"
chaos outage <"$tmp/stdin" >"$tmp/c.json" 2>"$tmp/c.err" &
pid=$!
exec 3>"$tmp/stdin"
handled=0
while kill -0 "$pid" 2>/dev/null; do
	n=$(grep -c '^action ' "$tmp/c.err")
	while [ "$handled" -lt "$n" ]; do
		handled=$((handled + 1))
		action=$(grep '^action ' "$tmp/c.err" | sed -n "${handled}p")
		case $action in
		"action stop")
			stale_poll &
			poller=$!
			outage_s=$SECONDS
			echo stopping >&3
			"${compose[@]}" stop controlplane postgres >"$tmp/stop.log" 2>&1
			echo "stopped $?" >&3
			;;
		"action restart gateway-"[1-5])
			"${compose[@]}" --profile replicas restart "${action#action restart }" >"$tmp/restart.log" 2>&1
			echo "restarted $?" >&3
			;;
		"action start")
			if [ -n "$poller" ]; then kill "$poller" 2>/dev/null; fi
			poller=
			outage_s=$((SECONDS - outage_s))
			"${compose[@]}" up -d --wait --wait-timeout 180 controlplane >"$tmp/start.log" 2>&1
			echo "healthy $?" >&3
			;;
		*)
			echo "unknown action" >&3
			;;
		esac
	done
	sleep 0.2
done
exec 3>&-
if [ -n "$poller" ]; then # the sender ended without asking for the start
	kill "$poller" 2>/dev/null
	poller=
	outage_s=$((SECONDS - outage_s))
fi
wait "$pid"
code=$?
pid=
show "$tmp/c.err"
sender_failed "$code" "$tmp/c.err" "(c)"
if [ -s "$tmp/c6" ]; then
	result PASS "(c6) staleness alert fires in the outage" "SpillwayConfigSyncStale firing for gateway-1 at +$(cat "$tmp/c6") s of the ${outage_s:-?} s outage (threshold 60 s)"
else
	result FAIL "(c6) staleness alert fires in the outage" "SpillwayConfigSyncStale not seen firing for gateway-1 in the ${outage_s:-?} s outage (polled every 5 s)"
fi

echo
name="M5 exit"
if [ "$mode" = outage ]; then name="Control-plane outage"; fi
if [ "$fails" -eq 0 ]; then echo "$name: PASS"; else echo "$name: FAIL ($fails checks)"; fi
[ "$fails" -eq 0 ]
