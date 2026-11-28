#!/usr/bin/env bash
# M8 exit (docs/design.md, M8: "make verify-m8 runs D1–D7 and checks
# each"). Implements ADR-0098's verify-m8 plan with the decisions that bind
# it: ADR-0101 §13 (d6.3's retry budget), ADR-0102 §1 (D4's route-level
# orders-guard) and §14 (d6.1's before-the-fault window); ADR-0106 records
# this script's own choices.
#
#   make verify-m8                  the full run, about 18 minutes on a built stack
#   test/verify/m8.sh report <dir>  prints a run's check lines and verdict again
#                                   from its logs, and writes <dir>.json again
#
# Order (ADR-0098 §1), each part one step with its log in the run's
# directory:
#   build   the demo-traffic image (G0 and G1 run in one-offs of it)
#   G0      demo mode off on the dev stack: demo-traffic verify guards
#           --phase G0 (g1 – g5), then g6 (make demo-fault-redis ON=1 is
#           refused with the demo-mode-off message)
#   up      make demo, make demo-reset (a fresh seed), the base-load
#           service demo-traffic stopped (D25)
#   K       demo-traffic verify keys (k1 – k4)
#   D1 … D7 demo-traffic run --scenario=dN --check, each in a one-off of
#           the demo-traffic service with --no-deps: the base load plus the
#           scenario's streams, judged by ADR-0098 §2's rules
#   S       the base-load service started again, then make demo-share-test
#           (s1 – s13 against the stand-in tunnel; never a real tunnel)
#   down    make demo-down (which ends with make dev)
#   G1      demo-traffic verify guards --phase G1 (g1 – g5), g6, and g7:
#           no demo container, gateway-2 and gateway-3 not running, make
#           verify-m0-stack passes
#
# Every check prints one line, "PASS <id> <what>: <measured>" or "FAIL
# <id> <what>: <measured> (want <rule>)"; the last line is "verify-m8:
# PASS (<n> checks)" or "verify-m8: FAIL (<k> of <n> checks: <ids>)". A
# failing part does not stop later parts; a part that cannot run, or whose
# output lacks a check, prints that check as "FAIL <id> setup: <reason>".
# No check, part or run is ever repeated, and no failure is excused
# (ADR-0098, Stall tolerance).
#
# Results: test/verify/out/m8-<UTC timestamp>/ (each part's log and JSON
# report) and test/verify/out/m8-<UTC timestamp>.json (every check line
# with its measured values, and each part's report); test/verify/out/ is
# git-ignored. On every exit before the end, the trap stops a running
# share (make demo-share-stop), switches the faults off (make
# demo-fault-clear) and runs make demo-down, so the stack ends as make dev
# leaves it. No tracked file is changed.
set -uo pipefail
cd "$(dirname "$0")/../.."

# ids_of <step>: the checks a step must print, in order.
ids_of() {
	case $1 in
	g0) echo G0.g1 G0.g2 G0.g3 G0.g4 G0.g5 ;;
	g0-fault) echo G0.g6 ;;
	k) echo k1 k2 k3 k4 ;;
	d1) echo d1.1 d1.2 d1.3 d1.4 ;;
	d2) echo d2.1 d2.2 d2.3 d2.4 ;;
	d3) echo d3.1 d3.2 d3.3 d3.4 ;;
	d4) echo d4.1 d4.2 d4.3 d4.4 d4.5 d4.6 ;;
	d5) echo d5.0 d5.1 d5.2 d5.3 d5.4 d5.5 d5.6 d5.7 d5.8 ;;
	d6) echo d6.1 d6.2 d6.3 d6.4 ;;
	d7) echo d7.1 d7.2 d7.3 d7.4 ;;
	s) echo s1 s2 s3 s4 s5 s6 s7 s8 s9a s9b s9c s10 s11 s12 s13 ;;
	g1) echo G1.g1 G1.g2 G1.g3 G1.g4 G1.g5 ;;
	g1-fault) echo G1.g6 ;;
	g1-stack) echo G1.g7 ;;
	esac
}

# The check steps of a full run, in order; build, up and down are setup
# steps without checks of their own.
check_steps="g0 g0-fault k d1 d2 d3 d4 d5 d6 d7 s g1 g1-fault g1-stack"

# --- reading a run's logs ------------------------------------------------------
# A run's directory holds <step>.log for each step it ran, <step>.json for
# the parts that write a report, steps.tsv (step, exit code or "skip",
# seconds, note) and state.env.

# step_field <dir> <step> <column>: a column of the step's steps.tsv row.
step_field() {
	awk -F '\t' -v s="$2" -v c="$3" '$1 == s { v = $c } END { print v }' "$1/steps.tsv"
}

# lines_of <dir> <step>: the step's check lines. demo-traffic's parts and
# this script's own checks print "PASS|FAIL <id> …"; make demo-share-test
# prints "share-check: <id> PASS|FAIL: …", written here as "PASS|FAIL <id>
# share: …". Then a FAIL setup line for each check the step should have
# printed and did not, and, for a step that exited non-zero without any
# FAIL line, one FAIL line for the step itself. A skipped step prints
# each of its checks as FAIL setup with the reason.
lines_of() {
	local dir=$1 step=$2 code note log id found
	code=$(step_field "$dir" "$step" 2)
	note=$(step_field "$dir" "$step" 4)
	log=$dir/$step.log
	if [ -z "$code" ]; then
		code=skip note="not run"
	fi
	if [ "$code" = skip ]; then
		for id in $(ids_of "$step"); do
			echo "FAIL $id setup: $note"
		done
		return
	fi
	found=$(if [ "$step" = s ]; then
		sed -nE 's/^share-check: (s[0-9]+[a-c]?) (PASS|FAIL): (.*)$/\2 \1 share: \3/p' "$log"
	else
		grep -E '^(PASS|FAIL) [^ ]+ ' "$log"
	fi)
	[ -z "$found" ] || echo "$found"
	for id in $(ids_of "$step"); do
		if ! awk -v id="$id" '$2 == id { f = 1 } END { exit !f }' <<<"$found"; then
			echo "FAIL $id setup: no result (step $step exit $code; $log)"
			found="$found
FAIL $id"
		fi
	done
	if [ "$code" != 0 ] && ! grep -q '^FAIL ' <<<"$found"; then
		echo "FAIL $step exit: exit $code without a FAIL line ($log)"
	fi
}

mins() { awk -v s="$1" 'BEGIN { printf "%d min %02d s", s / 60, s % 60 }'; }

# report <dir>: prints every check line and the verdict, and writes
# <dir>.json. Exits 0 on PASS, 1 on FAIL.
report() {
	local dir=${1%/} all n k failed verdict step started finished commit branch duration
	if [ ! -f "$dir/steps.tsv" ]; then
		echo "verify-m8: $dir has no steps.tsv" >&2
		return 2
	fi
	all=$(for step in $check_steps; do
		lines_of "$dir" "$step" | sed "s/^/$step	/"
	done)
	cut -f 2- <<<"$all"
	n=$(grep -c . <<<"$all")
	k=$(grep -c '	FAIL ' <<<"$all")
	failed=$(awk -F '\t' '$2 ~ /^FAIL / { split($2, f, " "); if (!seen[f[2]]++) l = l (l ? ", " : "") f[2] } END { print l }' <<<"$all")
	if [ "$k" = 0 ] && [ "$n" -gt 0 ]; then
		verdict="verify-m8: PASS ($n checks)"
	else
		verdict="verify-m8: FAIL ($k of $n checks: $failed)"
	fi
	started=$(state "$dir" started) finished=$(state "$dir" finished)
	commit=$(state "$dir" commit) branch=$(state "$dir" branch) duration=$(state "$dir" duration)
	awk -F '\t' '$2 != "skip" && $2 != 0 && ($1 == "build" || $1 == "up" || $1 == "down") { printf "note: setup step %s exited %s (%s/%s.log)\n", $1, $2, d, $1 }' d="$dir" "$dir/steps.tsv"
	[ -z "$duration" ] || echo "verify-m8: $(mins "$duration") on $commit ($branch)"
	echo "$verdict"
	{
		jq -R -n --arg started "$started" --arg finished "$finished" --arg commit "$commit" --arg branch "$branch" \
			--arg duration "$duration" --arg verdict "$verdict" --arg pass "$([ "$k" = 0 ] && [ "$n" -gt 0 ] && echo true || echo false)" \
			--rawfile steps "$dir/steps.tsv" --slurpfile reports <(for f in "$dir"/*.json; do [ -f "$f" ] && jq -c --arg f "$(basename "$f" .json)" '{($f): .}' "$f"; done | jq -s 'add // {}') '
			{
				started: $started, finished: $finished, commit: $commit, branch: $branch,
				duration_s: ($duration | tonumber? // null), pass: ($pass == "true"), verdict: $verdict,
				steps: [$steps | split("\n")[] | select(. != "") | split("\t") | {step: .[0], code: .[1], seconds: (.[2] | tonumber? // null), note: .[3]}],
				checks: [inputs | select(. != "") | (split("\t")[0]) as $step | sub("^[^\t]*\t"; "") | capture("^(?<verdict>PASS|FAIL) (?<id>[^ ]+) (?<text>.*)$") | {part: $step, id, verdict, text}],
				reports: $reports[0]
			}' <<<"$all"
	} >"$dir.json.tmp" && mv "$dir.json.tmp" "$dir.json"
	[ "$k" = 0 ] && [ "$n" -gt 0 ]
}

# state <dir> <key>: a value of the run's state.env.
state() {
	[ -f "$1/state.env" ] && sed -n "s/^$2=//p" "$1/state.env" | tail -n 1
}

if [ "${1:-}" = report ]; then
	[ -n "${2:-}" ] || { echo "usage: $0 report <dir>" >&2; exit 2; }
	report "$2"
	exit
fi
if [ $# -gt 0 ]; then
	echo "usage: $0 (make verify-m8), or $0 report <dir>" >&2
	exit 2
fi

# --- a run -------------------------------------------------------------------

compose=(docker compose --env-file .env.example)
if [ -f .env.local ]; then compose+=(--env-file .env.local); fi
demo=("${compose[@]}" -f compose.yaml -f deploy/compose/demo.compose.yml --profile replicas --profile demo)

stamp=$(date -u +%Y%m%dT%H%M%SZ)
out=test/verify/out/m8-$stamp
mkdir -p "$out"
# The one-offs run as the image's non-root user and write their reports here.
chmod 0777 "$out"
: >"$out/steps.tsv"
start=$SECONDS
{
	echo "started=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	echo "commit=$(git rev-parse HEAD)"
	echo "branch=$(git rev-parse --abbrev-ref HEAD)"
} >"$out/state.env"

demo_containers() {
	docker ps -aq --filter label=spillway.demo=true --filter label=com.docker.compose.project=spillway
}

demo_cp_running() {
	local id
	id=$("${compose[@]}" ps -q --status running controlplane)
	[ -n "$id" ] && [ "$(docker inspect -f '{{index .Config.Labels "spillway.demo"}}' "$id")" = "true" ]
}

finished=0
cleanup() {
	local rc=$?
	trap - EXIT INT TERM
	if [ "$finished" != 1 ]; then
		echo "verify-m8: interrupted; restoring the dev stack (log: $out/cleanup.log)" >&2
		{
			if deploy/demo/share.sh active; then make --no-print-directory demo-share-stop; fi
			if demo_cp_running; then make --no-print-directory demo-fault-clear; fi
			if [ -n "$(demo_containers)" ]; then make --no-print-directory demo-down; fi
		} >>"$out/cleanup.log" 2>&1
	fi
	exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# step <name> <command…>: runs the command with its output in the step's
# log, records its exit code and time, and prints its check lines.
step() {
	local name=$1 t0=$SECONDS rc
	shift
	"$@" >"$out/$name.log" 2>&1
	rc=$?
	printf '%s\t%s\t%s\t\n' "$name" "$rc" "$((SECONDS - t0))" >>"$out/steps.tsv"
	if [ -n "$(ids_of "$name")" ]; then
		lines_of "$out" "$name"
	fi
	return "$rc"
}

# skip <name> <reason>: a step that cannot run; its checks FAIL as setup.
skip() {
	printf '%s\tskip\t0\t%s\n' "$1" "$2" >>"$out/steps.tsv"
	lines_of "$out" "$1"
}

header() { echo "== $* ($(mins $((SECONDS - start))) in)"; }

# oneoff <args…>: demo-traffic in a one-off container of the demo-traffic
# service, in the compose network, with --no-deps (it never starts or
# recreates another service) and the run's directory at /out.
oneoff() {
	"${demo[@]}" run --rm --no-deps -v "$PWD/$out:/out" demo-traffic "$@"
}

# fault_check <G0|G1>: g6, make demo-fault-redis ON=1 as a user would run
# it, refused with demo mode off.
fault_check() {
	local said rc
	said=$(make --no-print-directory demo-fault-redis ON=1 2>&1)
	rc=$?
	echo "$said"
	if [ "$rc" -ne 0 ] && grep -q 'demo mode is off (GET /api/v1/demo answered 404)' <<<"$said"; then
		echo "PASS $1.g6 make demo-fault-redis ON=1 refused with demo mode off: exit $rc, \"demo mode is off (GET /api/v1/demo answered 404)\""
	else
		echo "FAIL $1.g6 make demo-fault-redis ON=1 refused with demo mode off: exit $rc, last line: $(tail -n 1 <<<"$said") (want a non-zero exit with the demo-mode-off message)"
	fi
}

# stack_check: g7, the dev stack is back as make dev leaves it.
stack_check() {
	local left running rc
	left=$(docker ps -aq --filter label=spillway.demo=true | grep -c .)
	running=$("${compose[@]}" --profile replicas ps --status running --format '{{.Service}}' gateway-2 gateway-3 2>/dev/null | tr '\n' ' ')
	make --no-print-directory verify-m0-stack >"$out/g1-m0-stack.log" 2>&1
	rc=$?
	local measured="$left containers labelled spillway.demo=true; gateway-2 and gateway-3 running: ${running:-none}; make verify-m0-stack exit $rc ($out/g1-m0-stack.log)"
	if [ "$left" = 0 ] && [ -z "$running" ] && [ "$rc" = 0 ]; then
		echo "PASS G1.g7 the dev stack as make dev leaves it: $measured"
	else
		echo "FAIL G1.g7 the dev stack as make dev leaves it: $measured (want no demo container, neither replica running, make verify-m0-stack passing)"
	fi
}

up_demo() {
	make --no-print-directory demo || return 1
	make --no-print-directory demo-reset || return 1
	"${demo[@]}" stop demo-traffic
}

share_part() {
	if ! "${demo[@]}" up -d --no-deps --wait --wait-timeout 120 demo-traffic; then
		echo "the base load did not start again"
		return 1
	fi
	make --no-print-directory demo-share-test
}

echo "verify-m8: results in $out/ and $out.json"
header "build: the demo-traffic image"
if ! step build "${demo[@]}" build demo-traffic; then
	echo "verify-m8: the demo-traffic image did not build ($out/build.log)"
	built=0
else
	built=1
fi

header "G0: demo mode off on the dev stack (SC-08)"
if [ "$built" = 1 ]; then
	step g0 oneoff verify guards --phase G0 --out /out/g0.json
	step g0-fault fault_check G0
else
	skip g0 "the demo-traffic image did not build"
	skip g0-fault "the demo-traffic image did not build"
fi

header "up: make demo, make demo-reset, base load stopped"
if [ "$built" = 1 ] && step up up_demo; then
	header "K: demo keys (SC-08)"
	step k oneoff verify keys --out /out/k.json
	for d in d1 d2 d3 d4 d5 d6 d7; do
		header "$(tr d D <<<"$d"): demo-traffic run --scenario=$d --check"
		step "$d" oneoff run "--scenario=$d" --check --out "/out/$d.json"
	done
	header "S: make demo-share-test (SC-09, SEC-05; the stand-in tunnel, never a real one)"
	step s share_part
else
	why="make demo or make demo-reset failed ($out/up.log)"
	[ "$built" = 1 ] || why="the demo-traffic image did not build"
	echo "verify-m8: $why"
	for s in k d1 d2 d3 d4 d5 d6 d7 s; do skip "$s" "$why"; done
fi

header "down: make demo-down"
step down make --no-print-directory demo-down

header "G1: demo mode off again (SC-08)"
if [ "$built" = 1 ]; then
	step g1 oneoff verify guards --phase G1 --out /out/g1.json
	step g1-fault fault_check G1
else
	skip g1 "the demo-traffic image did not build"
	skip g1-fault "the demo-traffic image did not build"
fi
step g1-stack stack_check
finished=1

{
	echo "finished=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	echo "duration=$((SECONDS - start))"
} >>"$out/state.env"
echo "== verdict"
report "$out" | grep -v '^\(PASS\|FAIL\) '
