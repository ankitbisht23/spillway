#!/usr/bin/env bash
# The public demo (SC-09, ADR-0097 §3, §4, ADR-0105), run by the Makefile
# on the running demo (make demo):
#
#   share.sh start   make demo-share: TUNNEL=quick (default), TUNNEL=named,
#                    or TUNNEL=stand-in (make demo-share-test only)
#   share.sh stop    make demo-share-stop: remove the tunnel, restore the
#                    localhost settings and the dev realm
#   share.sh active  exit 0 when a share runs (make demo-down)
#   share.sh test    make demo-share-test: the whole share path against a
#                    stand-in tunnel, with throwaway secrets, then the stop
#
# A share sets, from the tunnel's URL U: Keycloak's KC_HOSTNAME (U/auth),
# the control plane's and gateways' expected issuer (U/auth/realms/spillway,
# which is also the UI's OIDC authority) and the demo Grafana's root URL
# (U/grafana/), through deploy/demo/run/share.env and a recreate of what
# changed; then demo-traffic share-config adds U to the admin-ui client and
# hardens the realm through Keycloak's admin API (ADR-0097 §3 step 5).
#
# Real tunnels: only start with TUNNEL=quick or named, from a terminal,
# after the user types "share", with .env.demo. Of .env.demo this reads
# only KEYCLOAK_ADMIN_PASSWORD, DEMO_ADMIN_PASSWORD, and for named
# CLOUDFLARE_TUNNEL_TOKEN, DEMO_PUBLIC_URL, GITHUB_OAUTH_CLIENT_ID and
# GITHUB_OAUTH_CLIENT_SECRET, and never prints a value. make
# demo-share-test opens no tunnel and contacts no one: its stand-in prints
# a URL under .example and the checks run inside the compose network.
set -euo pipefail
cd "$(dirname "$0")/../.."

run_dir=deploy/demo/run
share_env=$run_dir/share.env
stand_in_url=https://stand-in.spillway.example
dev_value=dev-only-not-secret

compose=(docker compose --env-file .env.example)
if [ -f .env.local ]; then compose+=(--env-file .env.local); fi
demo=("${compose[@]}" -f compose.yaml -f deploy/compose/demo.compose.yml --profile replicas --profile demo)
tunnels=("${demo[@]}" --profile share --profile share-test)
share=("${tunnels[@]}" --env-file "$share_env" -f deploy/compose/share.compose.yml)
# The demo control plane runs in demo mode; every recreate here keeps it.
export DEMO_MODE=true

# The services a share's URL changes (ADR-0097 §3 step 4); Keycloak first,
# alone, so the others start on its new realm and keys.
changed=(controlplane gateway-1 gateway-2 gateway-3 grafana-demo)

say() { echo "demo-share: $*"; }
die() {
	echo "demo-share: $*" >&2
	exit 1
}

# demo_running: the demo's control plane runs, in demo mode (getDemo needs
# a token, so 401 rather than demo mode off's 404), behind the proxy.
demo_running() {
	local id code
	id=$("${compose[@]}" ps -q --status running controlplane)
	[ -n "$id" ] && [ "$(docker inspect -f '{{index .Config.Labels "spillway.demo"}}' "$id")" = "true" ] || return 1
	[ -n "$("${demo[@]}" ps -q --status running caddy)" ] || return 1
	code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 http://127.0.0.1:8080/api/v1/demo || true)
	[ "$code" = 401 ]
}

# tunnel_containers lists the IDs of the tunnel and stand-in containers.
tunnel_containers() {
	docker ps -aq --filter label=com.docker.compose.project=spillway --filter label=com.docker.compose.service=tunnel
	docker ps -aq --filter label=com.docker.compose.project=spillway --filter label=com.docker.compose.service=tunnel-stand-in
}

active() {
	[ -f "$share_env" ] || [ -n "$(tunnel_containers)" ]
}

# envval NAME FILE prints NAME's value in FILE (the last NAME= line, one
# pair of surrounding quotes dropped). It reads that one line only and
# never prints it anywhere else.
envval() {
	local v
	v=$(sed -n "s/^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}$1=//p" "$2" | tail -n 1)
	v=${v%$'\r'}
	case $v in
	\"*\") v=${v#\"} v=${v%\"} ;;
	\'*\') v=${v#\'} v=${v%\'} ;;
	esac
	printf '%s' "$v"
}

# need_password NAME: set, 16 characters or more, not the dev value.
need_password() {
	local v=${!1:-}
	[ -n "$v" ] || die "$1 is not set in $SHARE_ENV_FILE"
	[ "${#v}" -ge 16 ] || die "$1 in $SHARE_ENV_FILE is shorter than 16 characters"
	[ "$v" != "$dev_value" ] || die "$1 in $SHARE_ENV_FILE is the public dev value; set your own (docs/setup.md Step 8)"
}

# url_ok U: https, a host (and port), no path.
url_ok() {
	[[ $1 =~ ^https://[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$ ]]
}

# tunnel_url SERVICE waits up to 60 s for the first https URL after the
# "Visit it at" line in the tunnel's log, as cloudflared prints it.
tunnel_url() {
	local u="" i
	for i in $(seq 1 60); do
		u=$("${tunnels[@]}" logs --no-log-prefix "$1" 2>/dev/null |
			awk '/Visit it at/ { f = 1; next } f && match($0, /https:\/\/[^ |]+/) { print substr($0, RSTART, RLENGTH); exit }')
		if [ -n "$u" ]; then
			printf '%s' "$u"
			return 0
		fi
		sleep 1
	done
	return 1
}

start() {
	local mode=${TUNNEL:-quick} u answer gh_id="" gh_secret="" token="" service
	case $mode in
	quick | named) SHARE_ENV_FILE=${SHARE_ENV_FILE:-.env.demo} ;;
	stand-in)
		[ -n "${SHARE_ENV_FILE:-}" ] || die "TUNNEL=stand-in takes SHARE_ENV_FILE with throwaway passwords; make demo-share-test makes one"
		[ "$SHARE_ENV_FILE" != .env.demo ] || die "TUNNEL=stand-in never uses .env.demo; make demo-share-test makes throwaway passwords"
		;;
	*) die "TUNNEL must be quick, named or stand-in" ;;
	esac
	demo_running || die "the demo is not running (or not in demo mode); start it with make demo"
	! active || die "a share is already running; make demo-share-stop ends it"
	[ -f "$SHARE_ENV_FILE" ] || die "$SHARE_ENV_FILE does not exist (docs/setup.md Step 8: cp .env.example .env.demo, then set your own values)"

	# Only the values a share needs, in this process, never printed.
	KEYCLOAK_ADMIN_PASSWORD=$(envval KEYCLOAK_ADMIN_PASSWORD "$SHARE_ENV_FILE")
	DEMO_ADMIN_PASSWORD=$(envval DEMO_ADMIN_PASSWORD "$SHARE_ENV_FILE")
	need_password KEYCLOAK_ADMIN_PASSWORD
	need_password DEMO_ADMIN_PASSWORD
	if [ "$mode" = named ]; then
		token=$(envval CLOUDFLARE_TUNNEL_TOKEN "$SHARE_ENV_FILE")
		[ -n "$token" ] || die "CLOUDFLARE_TUNNEL_TOKEN is not set in $SHARE_ENV_FILE (TUNNEL=named, docs/setup.md Step 7)"
		u=$(envval DEMO_PUBLIC_URL "$SHARE_ENV_FILE")
		u=${u%/}
		url_ok "$u" || die "DEMO_PUBLIC_URL in $SHARE_ENV_FILE must be https://<host> with no path (TUNNEL=named)"
		gh_id=$(envval GITHUB_OAUTH_CLIENT_ID "$SHARE_ENV_FILE")
		gh_secret=$(envval GITHUB_OAUTH_CLIENT_SECRET "$SHARE_ENV_FILE")
		if { [ -n "$gh_id" ] && [ -z "$gh_secret" ]; } || { [ -z "$gh_id" ] && [ -n "$gh_secret" ]; }; then
			die "set GITHUB_OAUTH_CLIENT_ID and GITHUB_OAUTH_CLIENT_SECRET in $SHARE_ENV_FILE together, or neither"
		fi
	fi

	if [ "$mode" != stand-in ]; then
		if [ ! -t 0 ]; then
			die "make demo-share asks before publishing; run it in a terminal"
		fi
		cat <<-EOF
			demo-share: this publishes the demo on the internet through a Cloudflare $mode tunnel:
			  - the admin UI and /api/v1 (behind Keycloak logins and RBAC; the Playground for every role)
			  - Keycloak's login pages at /auth (its admin console and admin API stay refused)
			  - the demo Grafana's metrics at /grafana (anonymous viewers)
			  The realm's test users except test-viewer are disabled, test-admin gets DEMO_ADMIN_PASSWORD,
			  spillway-ci and gateway-tests get new secrets, rbac-tests is disabled and brute-force detection
			  goes on, until make demo-share-stop. Localhost logins fail while the share runs.
		EOF
		printf 'demo-share: type share to publish, anything else to stop: '
		read -r answer || answer=""
		[ "$answer" = share ] || die "not published"
	fi

	mkdir -p "$run_dir"
	chmod 700 "$run_dir"
	# From here any failure, an interrupt included, undoes the share, so
	# nothing half-configured stays public.
	start_done=0
	trap 'if [ "$start_done" != 1 ]; then echo "demo-share: FAIL; restoring the localhost settings" >&2; stop; fi' EXIT

	case $mode in
	quick)
		service=tunnel
		"${tunnels[@]}" up -d --no-deps tunnel
		;;
	named)
		service=tunnel
		TUNNEL_TOKEN=$token DEMO_TUNNEL_COMMAND="tunnel --no-autoupdate --metrics 127.0.0.1:0 --management-diagnostics=false run" \
			"${tunnels[@]}" up -d --no-deps tunnel
		;;
	stand-in)
		service=tunnel-stand-in
		"${tunnels[@]}" up -d --no-deps tunnel-stand-in
		;;
	esac
	if [ "$mode" != named ]; then
		u=$(tunnel_url "$service") || {
			echo "demo-share: no tunnel URL in 60 s" >&2
			false
		}
		u=${u%/}
	fi
	url_ok "$u" || {
		echo "demo-share: the tunnel's URL $u is not https://<host>" >&2
		false
	}
	if [ "$mode" = stand-in ] && [ "$u" != "$stand_in_url" ]; then
		echo "demo-share: the stand-in's URL is $u, want $stand_in_url" >&2
		false
	fi
	say "public URL $u"

	(
		umask 077
		cat >"$share_env" <<-EOF
			# Written by make demo-share (deploy/demo/share.sh); removed by make demo-share-stop.
			DEMO_PUBLIC_URL=$u
			DEMO_KC_HOSTNAME=$u/auth
			DEMO_ISSUER=$u/auth/realms/spillway
			DEMO_SHARE_UID=$(id -u)
			DEMO_SHARE_GID=$(id -g)
		EOF
	)
	chmod 600 "$share_env"

	export KEYCLOAK_ADMIN_PASSWORD DEMO_ADMIN_PASSWORD
	say "recreating keycloak with KC_HOSTNAME $u/auth"
	"${share[@]}" up -d --wait --wait-timeout 300 --force-recreate --no-deps keycloak
	say "recreating ${changed[*]} with the issuer $u/auth/realms/spillway and Grafana's root URL $u/grafana/"
	"${share[@]}" up -d --wait --wait-timeout 300 --force-recreate --no-deps "${changed[@]}"
	local gh=()
	if [ -n "$gh_id" ]; then
		export GITHUB_OAUTH_CLIENT_ID=$gh_id GITHUB_OAUTH_CLIENT_SECRET=$gh_secret
		gh=(-e GITHUB_OAUTH_CLIENT_ID -e GITHUB_OAUTH_CLIENT_SECRET)
	fi
	"${share[@]}" run --rm --no-deps -e KEYCLOAK_ADMIN_PASSWORD -e DEMO_ADMIN_PASSWORD ${gh[@]+"${gh[@]}"} demo-seed share-config --url "$u"
	say "restarting the base load with spillway-ci's new secret"
	"${share[@]}" up -d --wait --wait-timeout 300 --force-recreate --no-deps demo-traffic
	start_done=1
	cat <<-EOF
		demo-share: shared at $u
		  visitors       log in as test-viewer (password in docs/setup.md Step 4): a viewer, the Playground included
		  you            log in as test-admin with DEMO_ADMIN_PASSWORD
		  demo Grafana   $u/grafana
		  stop           make demo-share-stop (localhost logins work again after it)
	EOF
}

stop() {
	local start=$SECONDS
	"${tunnels[@]}" rm -sf tunnel tunnel-stand-in >/dev/null 2>&1 || true
	if [ -d "$run_dir" ]; then
		find "$run_dir" -mindepth 1 -delete
	fi
	unset KEYCLOAK_ADMIN_PASSWORD DEMO_ADMIN_PASSWORD GITHUB_OAUTH_CLIENT_ID GITHUB_OAUTH_CLIENT_SECRET
	if [ -z "$("${compose[@]}" ps -q --status running controlplane)" ]; then
		say "the tunnel is gone and deploy/demo/run/ is empty; the demo is not running, so there is nothing else to restore"
		return 0
	fi
	echo "demo-share-stop: restoring keycloak (the dev realm) and the localhost issuer and Grafana root URL"
	"${demo[@]}" up -d --wait --wait-timeout 300 --force-recreate --no-deps keycloak
	"${demo[@]}" up -d --wait --wait-timeout 300 --force-recreate --no-deps "${changed[@]}" demo-traffic
	echo "demo-share-stop: done in $((SECONDS - start)) s; log in through localhost again"
}

# test_cleanup runs when make demo-share-test exits, whatever the result:
# the event watcher stops, a share left running is stopped, and the
# throwaway passwords and secrets go with their directory.
test_watcher=""
test_tmp=""
test_cleanup() {
	if [ -n "$test_watcher" ]; then kill "$test_watcher" 2>/dev/null || true; fi
	if active; then stop || true; fi
	if [ -n "$test_tmp" ]; then rm -rf "$test_tmp"; fi
}

# test: make demo-share-test (ADR-0097 §4, ADR-0105). Every check prints a
# PASS or FAIL line; the target fails on any FAIL.
test_share() {
	local rc=0 u fails
	demo_running || die "the demo is not running (or not in demo mode); start it with make demo"
	! active || die "a share is running; make demo-share-stop ends it first"
	test_tmp=$(mktemp -d)
	chmod 700 "$test_tmp"
	local tmp=$test_tmp
	# Every container the test creates, with its image (s11).
	docker events --filter type=container --filter event=create --filter label=com.docker.compose.project=spillway \
		--format '{{index .Actor.Attributes "image"}} {{index .Actor.Attributes "name"}}' >"$tmp/created" &
	test_watcher=$!
	trap test_cleanup EXIT
	(
		umask 077
		printf 'KEYCLOAK_ADMIN_PASSWORD=%s\nDEMO_ADMIN_PASSWORD=%s\n' "$(openssl rand -base64 32)" "$(openssl rand -base64 32)" >"$tmp/share.env"
	)
	local start=$SECONDS
	set +e
	(
		set -e
		TUNNEL=stand-in SHARE_ENV_FILE=$tmp/share.env start
		u=$(envval DEMO_PUBLIC_URL "$share_env")
		cp "$run_dir"/*-secret "$tmp/"
		KEYCLOAK_ADMIN_PASSWORD=$(envval KEYCLOAK_ADMIN_PASSWORD "$tmp/share.env") \
			DEMO_ADMIN_PASSWORD=$(envval DEMO_ADMIN_PASSWORD "$tmp/share.env") \
			"${share[@]}" run --rm --no-deps -e KEYCLOAK_ADMIN_PASSWORD -e DEMO_ADMIN_PASSWORD demo-seed share-check --url "$u" || echo "share-check: FAIL: s1-s9, s12 (exit $?)"
		"${share[@]}" exec -T tunnel-stand-in /demo-traffic share-check --url "$u" --only s9b || echo "share-check: FAIL: s9b (exit $?)"
		stop
		"${demo[@]}" run --rm --no-deps demo-seed share-check --url "$u" --after-stop || echo "share-check: FAIL: s10 (exit $?)"
		left=$(tunnel_containers | wc -l | tr -d ' ')
		files=$(find "$run_dir" -mindepth 1 2>/dev/null | wc -l | tr -d ' ')
		if [ "$left" = 0 ] && [ "$files" = 0 ]; then
			echo "share-check: s10 PASS: no tunnel container left, deploy/demo/run/ empty"
		else
			echo "share-check: s10 FAIL: $left tunnel containers left, $files files in deploy/demo/run/"
		fi
		case $u in
		*.example) echo "share-check: s11 PASS: the only tunnel URL used: $u (RFC 2606, resolves nowhere)" ;;
		*) echo "share-check: s11 FAIL: the tunnel URL used: $u" ;;
		esac
	) 2>&1 | tee "$tmp/out.log"
	rc=${PIPESTATUS[0]}
	set -e
	sleep 1
	kill "$test_watcher" 2>/dev/null || true
	wait "$test_watcher" 2>/dev/null || true
	if grep -q cloudflared "$tmp/created"; then
		echo "share-check: s11 FAIL: a container from the cloudflared image was created: $(grep cloudflared "$tmp/created" | tr '\n' ' ')" | tee -a "$tmp/out.log"
	else
		echo "share-check: s11 PASS: no container from the cloudflared image; created: $(awk '{print $2}' "$tmp/created" | sort -u | tr '\n' ' ')" | tee -a "$tmp/out.log"
	fi
	# s13: no throwaway password, rotated secret or token in any output.
	local leaked=0 f v
	for v in "$(envval KEYCLOAK_ADMIN_PASSWORD "$tmp/share.env")" "$(envval DEMO_ADMIN_PASSWORD "$tmp/share.env")"; do
		if grep -qF -- "$v" "$tmp/out.log"; then leaked=1; fi
	done
	for f in "$tmp"/*-secret; do
		[ -f "$f" ] || continue
		if grep -qF -- "$(cat "$f")" "$tmp/out.log"; then leaked=1; fi
	done
	if grep -qE 'eyJ[A-Za-z0-9_-]{10,}' "$tmp/out.log"; then leaked=1; fi
	if [ "$leaked" = 0 ] && [ -n "$(find "$tmp" -name '*-secret' -print -quit)" ]; then
		echo "share-check: s13 PASS: no password, rotated secret or token in the test's output"
	else
		echo "share-check: s13 FAIL: a password, secret or token reached the output (or no rotated secret was seen)"
		rc=1
	fi | tee -a "$tmp/out.log"
	fails=$(grep -c '^share-check: .*FAIL' "$tmp/out.log" || true)
	if [ "$rc" = 0 ] && [ "$fails" = 0 ]; then
		echo "demo-share-test: PASS ($(grep -cE '^share-check: s[0-9]+[a-c]? PASS' "$tmp/out.log") PASS lines) in $((SECONDS - start)) s"
		return 0
	fi
	echo "demo-share-test: FAIL ($fails FAIL lines, exit $rc) in $((SECONDS - start)) s"
	return 1
}

case "${1:-}" in
start) start ;;
stop) stop ;;
active) active ;;
test) test_share ;;
*)
	echo "usage: $0 start|stop|active|test (make demo-share, demo-share-stop, demo-share-test)" >&2
	exit 2
	;;
esac
