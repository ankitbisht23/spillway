#!/usr/bin/env bash
# M6 exit check (docs/design.md, M6; ADR-0061): a new policy goes shadow →
# reviewed → enforced → rolled back entirely from the UI. The compose
# service chaos ("chaos m6 setup") imports and publishes
# test/verify/m6.yaml as the base version B; gateway-1 … gateway-5 run in
# managed mode (--profile replicas), as make verify-m5's do; "chaos m6
# probe", in the compose network, drives route m6-probe on all five with
# a heavy and a light client; and Playwright runs web/e2e/m6-exit.spec.ts
# once as test-admin: 20 timed route publishes, then the policy m6-limit
# in shadow (S), its shadow report, promoted and published (E), and the
# rollback to B from History (R). The probe then judges, each once:
#   (a)  NFR-08: every one of the 23 UI publishes, click → response (host
#        clock) plus published_at → the last of the five apply
#        heartbeats (Postgres clock), < 5 s;
#   (b)  shadow: no 429s in S's window, both clients answered by all five;
#   (b2) reviewed: the report has would-denies, the heavy client's digest
#        among its keys, the light one's without, and the UI showed them;
#   (c)  enforced: >= 50 % of heavy requests 429 with the policy's
#        RateLimit item and Retry-After, the rest 200, light all 200;
#   (d)  rolled back: no 429s in R's window;
#   (e)  the whole run: only 200 and 429, no light 429, every heavy 429
#        received after E's published_at and due before R's last apply
#        + 1 s;
#   (f1) zero console errors and (f2) no token in storage, cookies or
#        URLs, from the leak guard; (g) the exit spec passed.
# A window is 20 s from 1 s after its version's last apply heartbeat.
# Every measured number is printed with PASS or FAIL. Exits 1 if any
# check fails; no check is ever run again and Playwright has no retries
# (ADR-0061's stall tolerance). On every exit the probe is stopped,
# gateway-1 is recreated in static mode and gateway-2 … gateway-5 are
# removed, as make dev leaves the stack; after a PASS Playwright's
# web/test-results and web/playwright-report are removed (after a FAIL
# they stay for diagnosis; both are gitignored). The working set is B
# again after the rollback; published versions stay. No committed file
# is changed. Run through make verify-m6, about 3 minutes.
set -uo pipefail
cd "$(dirname "$0")/../.."

checks=("(a)" "(b)" "(b2)" "(c)" "(d)" "(e)" "(f1)" "(f2)" "(g)")
fails=0
result() { # result PASS|FAIL check measured
	printf '%-4s  %-44s %s\n' "$1" "$2" "$3"
	if [ "$1" != PASS ]; then fails=$((fails + 1)); fi
}

compose=(docker compose --env-file .env.example)
if [ -f .env.local ]; then compose+=(--env-file .env.local); fi
gateways=(gateway-1 gateway-2 gateway-3 gateway-4 gateway-5)
run_id=$(date +%s)
probe=spillway-m6-probe-$run_id
tmp=$(mktemp -d)

# chaos [--name <container>] <command> <step> [flags]: one step of the
# chaos service. The spillway-ci client's secret reaches it from this
# environment if set there (never on a command line), else from
# .env.local, else it uses the dev value.
chaos() {
	local opts=()
	if [ -n "${SPILLWAY_CI_CLIENT_SECRET:-}" ]; then opts=(-e SPILLWAY_CI_CLIENT_SECRET); fi
	if [ "$1" = --name ]; then
		opts+=(--name "$2")
		shift 2
	fi
	"${compose[@]}" run --rm -T ${opts[@]+"${opts[@]}"} chaos "$@" -run-id "$run_id"
}

# show <file>: the PASS/FAIL and detail lines, not compose's progress.
show() { grep -E '^(PASS|FAIL) |^      |^chaos' "$1"; }

# not_run <reason>: every check fails as setup.
not_run() {
	local c
	for c in "${checks[@]}"; do result FAIL "$c not run" "setup: $1"; done
	exit 1
}

managed= # the gateways run in managed mode
pid=     # the probe, while it runs
cleanup() {
	exec 3>&-
	if [ -n "$pid" ]; then kill "$pid" 2>/dev/null; fi
	docker rm -f "$probe" >/dev/null 2>&1
	if [ -n "$managed" ]; then
		# Removed, then created, not recreated in place (see
		# test/integration/managed_test.go's composeManaged).
		"${compose[@]}" rm -fsv gateway-1 >/dev/null 2>&1
		"${compose[@]}" up -d --no-deps --wait --wait-timeout 120 gateway-1 >/dev/null 2>&1 ||
			echo "      restoring: gateway-1 did not come back in static mode"
		"${compose[@]}" --profile replicas rm -fsv gateway-2 gateway-3 gateway-4 gateway-5 >/dev/null 2>&1
	fi
	if [ "$fails" -eq 0 ]; then
		rm -rf web/test-results web/playwright-report
	elif [ -d web/test-results ] || [ -d web/playwright-report ]; then
		echo "      Playwright's results stay in web/test-results and web/playwright-report (gitignored) for diagnosis"
	fi
	rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 1' INT TERM

echo "== setup: test/verify/m6.yaml published as the base version, gateway-1 … gateway-5 managed (ADR-0061)"
if ! pnpm -C web exec playwright --version >/dev/null 2>&1; then
	not_run "Playwright is not installed: pnpm install, then pnpm -C web exec playwright install chromium"
fi
if ! build=$("${compose[@]}" build --quiet chaos 2>&1); then
	printf '%s\n' "$build"
	not_run "the chaos image did not build"
fi
if ! chaos m6 setup <test/verify/m6.yaml >"$tmp/setup.json" 2>"$tmp/setup.err"; then
	show "$tmp/setup.err"
	not_run "chaos m6 setup failed"
fi
base=$(jq -r '.config_version' "$tmp/setup.json")
managed=1
if ! up=$(SPILLWAY_GATEWAY_CONFIG='' SPILLWAY_GATEWAY_IDENTITY='' SPILLWAY_GATEWAY_CONTROL_PLANE=controlplane:9090 \
	"${compose[@]}" --profile replicas up -d --no-deps --force-recreate --wait --wait-timeout 120 "${gateways[@]}" 2>&1); then
	printf '%s\n' "$up"
	not_run "the managed gateways did not become ready"
fi
if ! chaos controlplane ready >"$tmp/ready.json" 2>"$tmp/ready.err"; then
	show "$tmp/ready.err"
	not_run "the replicas were not live at the published versions within 60 s"
fi
jq -r '"      base version \(.config_version), identity version \(.identity_version): all 5 replicas live after \(.took_ms / 1000 | floor) s"' "$tmp/ready.json"

echo "== the probe: route m6-probe on all 5 gateways, heavy 200 rps and light 10 rps"
# The probe reads the spec's record, the leak guard's report and the
# spec's exit code from this FIFO once the UI run is over, then judges.
mkfifo "$tmp/stdin"
chaos --name "$probe" m6 probe <"$tmp/stdin" >"$tmp/probe.json" 2>"$tmp/probe.err" &
pid=$!
exec 3>"$tmp/stdin"
for _ in $(seq 1 600); do
	if grep -q '^probe started' "$tmp/probe.err" || ! kill -0 "$pid" 2>/dev/null; then break; fi
	sleep 0.2
done
if ! grep -q '^probe started' "$tmp/probe.err"; then
	show "$tmp/probe.err"
	not_run "the probe did not start"
fi

echo "== the UI run: web/e2e/m6-exit.spec.ts as test-admin, 20 publishes then shadow → reviewed → enforced → rolled back (about 2 minutes)"
# An earlier failed run's records must never reach this run's probe.
rm -f web/test-results/m6-exit.json web/test-results/m6-leak-guard.json
SPILLWAY_VERIFY_M6_RUN=$run_id SPILLWAY_VERIFY_M6_BASE=$base \
	pnpm -C web exec playwright test -c playwright.m6.config.ts >"$tmp/playwright.log" 2>&1
spec=$?
if [ "$spec" -eq 0 ]; then
	grep -E '^ +[0-9]+ passed' "$tmp/playwright.log" | sed 's/^ */      Playwright: /'
else
	sed 's/^/      /' "$tmp/playwright.log"
fi
if [ -f web/test-results/m6-exit.json ]; then echo "exit $(jq -c . web/test-results/m6-exit.json)" >&3; fi
if [ -f web/test-results/m6-leak-guard.json ]; then echo "guard $(jq -c . web/test-results/m6-leak-guard.json)" >&3; fi
echo "spec $spec" >&3
exec 3>&-

echo "== the checks"
wait "$pid"
code=$?
pid=
show "$tmp/probe.err"
fails=$((fails + $(grep -c '^FAIL ' "$tmp/probe.err")))
if [ "$code" -ne 0 ] && ! grep -q '^FAIL ' "$tmp/probe.err"; then
	result FAIL "probe" "chaos m6 probe exited $code: $(tail -1 "$tmp/probe.err")"
fi

echo
if [ "$fails" -eq 0 ]; then echo "M6 exit: PASS"; else echo "M6 exit: FAIL ($fails checks)"; fi
[ "$fails" -eq 0 ]
