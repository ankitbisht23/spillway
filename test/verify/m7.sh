#!/usr/bin/env bash
# M7 exit (docs/design.md, M7: "every NFR row (NFR-01 – NFR-15) meets its
# target, with the measured result in the README, or an ADR re-baselines
# it"). Implements ADR-0074's verify-m7 plan NFR by NFR, with the later
# decisions that bind it: NFR-01 at the per-arch floors on any CPU model
# (ADR-0080, ADR-0082), the bench gate's verdicts (ADR-0081), the M7 load and
# chaos targets (ADR-0083), NFR-02 as an open known limit (ADR-0084 §4), the
# rolling restart on kind (ADR-0086) and (c6) in verify-m5 (ADR-0087);
# ADR-0088 records this script's own choices, and ADR-0089 NFR-05's x64
# NOT DEMONSTRATED rule (a CPU model with no main baseline yet).
#
# Prints every sub-check's PASS/FAIL line, then one line per NFR row and per
# extra chaos scenario with its measured number against its target, and
# exits 1 if any counted line fails. No line is ever re-run. Run through
# make:
#   make verify-m7                    the full run (the M7 exit evidence), about 2¼ hours
#   make verify-m7 README=1           the same, then rewrites the README's verify-m7 block
#   make verify-m7 PARTS="10 11 15"   a subset for development (bench, compose,
#                                     kind, chaos or NFR numbers); never exit evidence
#   test/verify/m7.sh readme <json>   rewrites the README block from a full run's JSON
#   test/verify/m7.sh report <dir>    prints the rows and writes <dir>.json again
#                                     from a run's saved logs and state
#
# Order of a full run (ADR-0074, "Order and total time of one full run"):
#   preflight  clean tree apart from the three local files, gh logged in as
#              the repository's owner, HEAD pushed to origin; otherwise
#              NFR-01 and NFR-05 fail with the reason
#   bench      dispatches bench.yml -f gate=true on HEAD (VERIFY_M7_BENCH_RUNS
#              =<run id> reuses a run already dispatched for HEAD). With P19
#              approved, this one run set at the floors serves NFR-01 and
#              NFR-05 (ADR-0074's table, ADR-0080). NFR-02 reads the newest
#              scaling results on bench-results (ADR-0084 §4: no dispatch).
#   compose    make verify-m1-reload, verify-m2, verify-m3, accuracy (strict
#              1 and 5, batched 1, 3 and 5), verify-m5, nfr-keys, nfr-soak,
#              nfr-noisy, chaos-redis-latency, chaos-gateway-kill, verify-m4,
#              each through its make target, so each runs with the
#              environment and the dev prerequisite its target sets
#   kind       docker compose down (no -v: memory, ADR-0075), make kind-up,
#              kind-smoke, kind-rollout, kind-down, then make dev
#   then the bench run is waited for, at most 120 minutes from its dispatch
#
# Stall tolerance (ADR-0074, ADR-0026/0027/0034): every line keeps the rule
# of the check it runs, and none is re-run. The bench lines run on GitHub's
# runners. verify-m3's full-outage and killed-primary lines can still fail
# on a laptop stall, which the user accepted; verify-m7 inherits that.
#
# Output: the rows as JSON in test/verify/out/m7-<UTC timestamp>.json
# (git-ignored), with every step's full log and the run's state in
# test/verify/out/m7-<UTC timestamp>/. On every exit the gateway config and
# identity are put back, gateway-2 … gateway-5 and the toxiproxy proxies
# are removed, kind is deleted if this run created it, and make dev's stack
# is left running.
set -uo pipefail
cd "$(dirname "$0")/../.."

readme_begin='<!-- verify-m7:begin -->'
readme_end='<!-- verify-m7:end -->'
readme_file=${VERIFY_M7_README:-README.md}
floor_x64=4600 floor_arm64=6300 # NFR-01's floors (P19, ADR-0080, ADR-0090)

# readme <json>: rewrites the block between the markers from a full run's JSON.
readme() {
	local json=$1 block
	if ! jq -e '.parts == "all"' "$json" >/dev/null 2>&1; then
		echo "verify-m7: $json is not a full run (parts all); the README block is written from full runs only" >&2
		return 1
	fi
	if ! grep -qxF "$readme_begin" "$readme_file" || ! grep -qxF "$readme_end" "$readme_file"; then
		echo "verify-m7: $readme_file has no verify-m7 markers" >&2
		return 1
	fi
	block=$(mktemp)
	jq -r --arg begin "$readme_begin" --arg end "$readme_end" '
		def cell: tostring | gsub("\\|"; "\\|");
		def links: [.links[] | "[\(.name)](\(.url))"] | join(", ");
		$begin,
		"",
		"Measured by `make verify-m7` on commit [`\(.commit[:7])`](\(.repo_url)/commit/\(.commit)) (`\(.branch)`), \(.started[:10]), \(.started[11:16])–\(.finished[11:16]) UTC, \(.duration_s / 60 | floor) min (budget 150). Laptop: \(.laptop). **M7 exit: \(.exit)**\(if .excluded != "" then ". \(.excluded)" else "" end).",
		"",
		"| Row | Target | Measured | Where | Date | Commit | Result |",
		"| --- | --- | --- | --- | --- | --- | --- |",
		(.rows[] | "| \(.id) | \(.target | cell) | \(.measured | cell)\(if (.links | length) > 0 then " (\(links))" else "" end) | \(.where | cell) | \(.date) | `\(.commit[:7])` | \(if .verdict == "PASS" then "PASS" elif .counted then "**\(.verdict)**" else "**\(.verdict)** (not counted)" end) |"),
		"",
		"A row marked \"not counted\" is left out of the exit status by the user'"'"'s decision alone and is never shown as PASS. The run'"'"'s full output and the pass rate over the M7 exit runs are in the PR that committed this block.",
		"",
		$end' "$json" >"$block" || { rm -f "$block"; return 1; }
	awk -v b="$readme_begin" -v e="$readme_end" -v f="$block" '
		$0 == b { while ((getline l < f) > 0) print l; skip = 1; next }
		$0 == e { skip = 0; next }
		!skip' "$readme_file" >"$readme_file.tmp" && mv "$readme_file.tmp" "$readme_file"
	rm -f "$block"
	echo "$readme_file: verify-m7 block rewritten from $json"
}

# --- reading a run's logs ------------------------------------------------------
# A run's directory holds <step>.log and <step>.code for each step it ran,
# steps.tsv, state.env and the bench-results files it read (bench-*.json).

mins() { awk -v s="$1" 'BEGIN { printf "%d min %02d s", s / 60, s % 60 }'; }
ran() { [ -f "$out/$1.code" ]; }
code_of() { cat "$out/$1.code" 2>/dev/null || echo 255; }

# checks <step> [section regex]: the step's PASS/FAIL lines, only those
# under a "== " header matching the regex when one is given, plus any FAIL
# line under a setup header (a setup failure fails every row of its step).
checks() {
	awk -v re="${2:-}" '
		/^== / { sec = $0; next }
		/^(PASS|FAIL) / { if (re == "" || sec ~ re || (/^FAIL / && sec ~ /setup/)) print }' "$out/$1.log"
}

# meas <step> <check name>: the measured text of the step's first line for
# that check (the text after the padded name).
meas() {
	awk -v n="$2" 'substr($0, 7, length(n)) == n { s = substr($0, 7 + length(n)); sub(/^ +/, "", s); print s; exit }' "$out/$1.log"
}

# verdict_of <step> [section regex]: PASS when the selected lines are all
# PASS and there is at least one, and the step did not end without a FAIL
# line it should have printed (a non-zero exit with no FAIL line anywhere;
# for a whole-step row, any non-zero exit).
verdict_of() {
	local l
	l=$(checks "$1" "${2:-}")
	if [ -z "$l" ] || grep -q '^FAIL ' <<<"$l"; then echo FAIL; return; fi
	if [ "$(code_of "$1")" -ne 0 ] && { [ -z "${2:-}" ] || ! grep -q '^FAIL ' "$out/$1.log"; }; then echo FAIL; return; fi
	echo PASS
}

# tally <step> [section regex]: "k of n checks PASS", with the failed names.
tally() {
	checks "$1" "${2:-}" | awk '
		{ n++; if (/^PASS /) k++; else { s = substr($0, 7); sub(/  +.*/, "", s); f = f (f ? "; " : "") s } }
		END { printf "%d of %d checks PASS%s", k, n, (f ? " (FAIL: " f ")" : "") }'
}

# row <id> <verdict> <counted true|false> <target> <where> <measured> [name=url ...]
row() {
	local id=$1 verdict=$2 counted=$3 target=$4 where=$5 measured=$6 links="" l
	shift 6
	for l in "$@"; do links="$links${links:+|}$l"; done
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$id" "$verdict" "$counted" "$target" "$where" "$measured" "$links" >>"$rows"
}
# not_run <id> <target> <where>: a row whose step did not run; only a full
# run lists it (and fails it), a partial run leaves it out.
not_run() { [ "$parts" != all ] || row "$1" "NOT RUN" true "$2" "$3" "not run"; }

show() { cat "$out/bench-$(tr / _ <<<"$1")"; }
# result_files <arch>: the bench run's stored files for <arch>.
result_files() {
	local f
	[ -n "$bench_url" ] || return 0
	for f in $(grep "^results/$1/" "$out/bench-files.txt"); do
		if show "$f" | jq -e --arg u "$bench_url" '.run_url == $u' >/dev/null 2>&1; then echo "$f"; fi
	done
}
blob() { echo "$repo_url/blob/bench-results/$1"; }
# main_models <result file>: the CPU models, one per line, of the results
# that could be its nightly baseline but for the CPU model: benchgate's
# eligible (from main, valid, gate pass or first, or no gate) and comparable
# (same arch, target rate and setup) results stored before it (ADR-0081,
# ADR-0089).
main_models() {
	local c=$1 f
	for f in $(grep "^results/$(show "$c" | jq -r .arch)/" "$out/bench-files.txt"); do
		[[ $f < $c ]] || continue
		show "$f" | jq -c '{ref, valid, gate, target_rps, setup, model: .hardware.cpu_model}' 2>/dev/null
	done | jq -rs --argjson c "$(show "$c")" '
		[.[] | select(.ref == "refs/heads/main" and .valid == true
			and (.gate == null or .gate.verdict == "pass" or .gate.verdict == "first")
			and .target_rps == $c.target_rps and .setup == $c.setup and (.model // "") != "") | .model]
		| unique | .[]'
}

# acc <step>: "<verdict>\t<max over-admission uniform/bursty/boundary>\t<boundary
# 503 share>" from an accuracy run's summary lines, or verify-m3's accuracy lines.
acc() {
	local l p pl v=PASS over="" b503="?"
	if [ "$1" = m3 ]; then
		l=$(checks m3 'NFR-06' | sed -E 's/^(PASS|FAIL)  accuracy: ([a-z]+), zero over-admission +/\2 \1  /')
	else
		l=$(grep -E '^(uniform|bursty|boundary) +(PASS|FAIL) ' "$out/$1.log")
		[ "$(code_of "$1")" -eq 0 ] || v=FAIL
	fi
	for p in uniform bursty boundary; do
		pl=$(grep -E "^$p +(PASS|FAIL) " <<<"$l" | head -1)
		if [ -z "$pl" ]; then v=FAIL over="$over${over:+/}?"; continue; fi
		grep -qE "^$p +PASS " <<<"$pl" || v=FAIL
		over="$over${over:+/}$(sed -E 's/.*max over-admission ([0-9]+).*/\1/' <<<"$pl")"
		if [ "$p" = boundary ]; then
			b503=$(awk '{ if (match($0, /[0-9]+\/[0-9]+ sent/)) { split(substr($0, RSTART, RLENGTH), a, "/"); sent = a[1] }
				if (match($0, /, 503 [0-9]+/)) n = substr($0, RSTART + 6, RLENGTH - 6) }
				END { if (sent > 0) printf "%.0f%%", 100 * n / sent; else print "?" }' <<<"$pl")
		fi
	done
	printf '%s\t%s\t%s\n' "$v" "$over" "$b503"
}

# added <measured>: verify-m2/m3's added-latency line, shortened.
added() { sed -E 's/^median p99 [0-9.]+ \(of ([^)]*)\) - [0-9.]+ \(of ([^)]*)\) = ([-0-9.]+) ms added.*/added p99 \3 ms (median of gateway runs \1 ms minus median of direct runs \2 ms)/'; }
# failed_runs <step> <section regex>: the section's failed lines other than
# its summary, as "; FAIL <check>: <measured>" (why an added-latency check
# had no three valid pairs).
failed_runs() {
	checks "$1" "$2" | awk '/^FAIL / && !/ added p99 <= / { s = substr($0, 7); sub(/  +/, ": ", s); printf "; FAIL %s", s }'
}

# report <dir>: the rows of the run in <dir> (from its state.env, logs and
# bench files): prints them, writes <dir>.json and returns 1 if a counted
# row is not PASS.
report() {
	out=$1
	rows=$out/rows.tsv
	: >"$rows"
	# shellcheck source=/dev/null
	. "$out/state.env"
	local where="laptop compose" today=${started:0:10} v m s n sv over b503 q

	echo
	echo "== M7 exit: NFR by NFR"

	# NFR-01 and NFR-05 (CI)
	local t01="≥ 4,600 rps per gateway vCPU on x64, ≥ 6,300 on arm64: a valid run set at the floor, on any CPU model (ADR-0080, ADR-0082, ADR-0090)"
	local t05="per arch, the gate's final verdict (after its confirming re-run when flagged) is pass against the nightly baseline; first, invalid or fail is FAIL (ADR-0074, ADR-0081), except an x64 first on a CPU model with no main baseline yet: NOT DEMONSTRATED, not counted (ADR-0089)"
	if [ -z "$want_bench" ]; then
		not_run NFR-01 "$t01" "CI x64 + arm64"
		not_run NFR-05 "$t05" "CI x64 + arm64"
	elif [ -z "$bench_url" ]; then
		row NFR-01 FAIL true "$t01" "CI x64 + arm64" "$bench_why"
		row NFR-05 FAIL true "$t05" "CI x64 + arm64" "$bench_why"
	else
		local m01="" v01=PASS m05="" v05=PASS nd05="" links=("run $bench_run=$bench_url") arch floor fs f first rerun final line models
		for arch in x64 arm64; do
			floor=$floor_x64
			[ "$arch" = arm64 ] && floor=$floor_arm64
			fs=$(result_files "$arch")
			first="" rerun=""
			for f in $fs; do
				if show "$f" | jq -e '.gate.rerun_of == null' >/dev/null 2>&1; then first=$f; else rerun=$f; fi
			done
			if [ -z "$first" ]; then
				v01=FAIL v05=FAIL
				m01="$m01${m01:+; }$arch: no result stored by run $bench_run${bench_why:+ ($bench_why)}"
				m05="$m05${m05:+; }$arch: no result stored by run $bench_run${bench_why:+ ($bench_why)}"
				continue
			fi
			links+=("$arch=$(blob "$first")")
			[ -z "$rerun" ] || links+=("$arch re-run=$(blob "$rerun")")
			line=$(show "$first" | jq -r --argjson floor "$floor" '
				((.rps_per_vcpu // 0) | round) as $r
				| [(if .valid and .target_rps == $floor and $r >= $floor then "PASS" else "FAIL" end),
				   "\(.arch) \($r) rps per vCPU at \(.target_rps) offered (floor \($floor)), \(if .valid then "valid" else "INVALID: \(.invalid_reasons | join("; "))" end), runs \(.achieved_rps.min | round)–\(.achieved_rps.max | round), p99 median \(.latency_us.p99.median / 1000) ms, CPU \(.hardware.cpu_model)"] | @tsv')
			[ "${line%%$'\t'*}" = PASS ] || v01=FAIL
			m01="$m01${m01:+; }${line#*$'\t'}"
			final=${rerun:-$first}
			line=$(show "$final" | jq -r --arg rerun "$rerun" '
				def pct: . * 1000 | round / 10 | tostring + "%";
				def metric: sub("_median$"; "") | sub("achieved_rps"; "rps") | sub("p99_us"; "p99");
				.gate as $g
				| [(if $g.verdict == "pass" and $g.baseline != null then "PASS" else "FAIL" end),
				   "\(.arch) \($g.verdict // "no gate")\(if $rerun != "" then " after the confirming re-run" else "" end) on CPU \(.hardware.cpu_model)"
				   + (if $g.baseline != null then " vs nightly \($g.baseline | sub("^.*/"; ""))" else ", no main baseline on this CPU model" end)
				   + (if ($g.comparisons // [] | length) > 0 then " (" + ([$g.comparisons[] | "\(.against) \(.metric | metric) \(.change | pct)\(if .flagged then " flagged" else "" end)"] | join(", ")) + ")" else "" end)
				   + (if $g.cross_model != null then "; information only (P34): vs \($g.cross_model.cpu_model) " + ([$g.cross_model.comparisons[] | "\(.metric | metric) \(.change | pct)"] | join(", ")) else "" end)] | @tsv')
			# ADR-0089: an x64 first because the runner's CPU model has no
			# eligible main baseline yet, while another model has one, is
			# NOT DEMONSTRATED; any other first (and every arm64 first) fails.
			if [ "$arch" = x64 ] && [ "$(show "$final" | jq -r '.gate.verdict // ""')" = first ]; then
				models=$(main_models "$final")
				if [ -n "$models" ] && ! grep -qxF "$(show "$final" | jq -r '.hardware.cpu_model // ""')" <<<"$models"; then
					nd05=1
					line="NOT DEMONSTRATED"$'\t'"${line#*$'\t'}; NOT DEMONSTRATED: main baselines exist only on $(paste -sd '|' - <<<"$models" | sed 's/|/, /g') (ADR-0089)"
				fi
			fi
			case ${line%%$'\t'*} in PASS | "NOT DEMONSTRATED") ;; *) v05=FAIL ;; esac
			m05="$m05${m05:+; }${line#*$'\t'}"
		done
		row NFR-01 "$v01" true "$t01" "CI x64 + arm64, bench.yml run $bench_run" "$m01" "${links[@]}"
		if [ "$v05" = PASS ] && [ -n "$nd05" ]; then
			row NFR-05 "NOT DEMONSTRATED" false "$t05" "CI x64 + arm64, bench.yml run $bench_run" "$m05" "${links[@]}"
		else
			row NFR-05 "$v05" true "$t05" "CI x64 + arm64, bench.yml run $bench_run" "$m05" "${links[@]}"
		fi
	fi

	# NFR-02 (CI, read only: ADR-0084 §4, the user's decision)
	local t02="S = R(2) ÷ R(1) ≥ 1.70 per arch, from valid run sets with gateway-bound probes (ADR-0074, ADR-0084)"
	if [ -z "$want_bench" ]; then
		not_run NFR-02 "$t02" "CI x64 + arm64"
	else
		local m02="" any=0 pass=1 runs="" latest url text r
		links=()
		for arch in x64 arm64; do
			latest=""
			for f in $(grep "^results/$arch/" "$out/bench-files.txt"); do
				if show "$f" | jq -e '.setup.mode == "scaling"' >/dev/null 2>&1; then latest=$f; fi
			done
			if [ -z "$latest" ]; then
				pass=0
				m02="$m02${m02:+; }$arch: no scaling result on bench-results"
				continue
			fi
			any=1
			links+=("$arch=$(blob "$latest")")
			IFS=$'\t' read -r v url text <<<"$(show "$latest" | jq -r '
				(if .valid and .pass then "PASS" elif .valid then "FAIL" else "INVALID" end) as $v
				| [$v, .run_url,
				   "\(.arch) \($v): R(1) \(.n1.r), R(2) \(.n2.r), \(if .valid then "" else "would-be " end)S \(.scaling // "n/a" | if type == "number" then (. * 1000 | round / 1000) else . end) on CPU \(.hardware.cpu_model), commit \(.commit[:7])"
				   + ([.n1, .n2 | .gateway_vcpus as $n | [.probes[] | select(.gateway_bound | not)] | select(length > 0)
					| "n=\($n): \(length) of 3 probes not gateway-bound, gateway \([.[].vcpu.gateway * 100 | round / 100 | tostring] | join("/")) vCPU (needs ≥ \($n * 0.95)), load side \([.[] | (.vcpu.loadgen + .vcpu.upstream) * 100 | round / 100 | tostring] | join("/")) (≤ 1.8)"]
					| if length > 0 then " (" + join("; ") + ")" else "" end)
				   + (if .valid or ([.n1, .n2 | .probes[] | select(.gateway_bound | not)] | length) > 0 then "" else " (\(.invalid_reasons | join("; ")))" end)] | @tsv')"
			[ "$v" = PASS ] || pass=0
			case " $runs " in *" ${url##*/} "*) ;; *) runs="$runs${runs:+ }${url##*/}" ;; esac
			m02="$m02${m02:+; }$text"
		done
		for r in $runs; do links+=("run $r=$repo_url/actions/runs/$r"); done
		if [ "$any" -eq 1 ] && [ "$pass" -eq 1 ]; then
			row NFR-02 PASS true "$t02" "CI x64 + arm64, newest scaling results" "$m02" ${links[@]+"${links[@]}"}
		else
			row NFR-02 "KNOWN LIMIT" false "$t02" "CI x64 + arm64, newest scaling results" "$m02" ${links[@]+"${links[@]}"}
		fi
	fi

	# NFR-03
	local t03="added p99 ≤ 2 ms over direct-to-upstream, local limits"
	if ran m2; then
		row NFR-03 "$(verdict_of m2 'NFR-03')" true "$t03" "$where, make verify-m2" "$(meas m2 'NFR-03 added p99 <= 2 ms' | added)$(failed_runs m2 'NFR-03')"
	else not_run NFR-03 "$t03" "$where"; fi

	# NFR-04
	local t04="0 ≤ added p99 ≤ 5 ms with a Redis policy (ADR-0021, ADR-0027, ADR-0034)"
	if ran m3; then
		row NFR-04 "$(verdict_of m3 'NFR-04')" true "$t04" "$where, make verify-m3" "$(meas m3 'NFR-04 added p99 <= 5 ms' | added)$(failed_runs m3 'NFR-04')"
	else not_run NFR-04 "$t04" "$where"; fi

	# NFR-06
	local t06="zero over-admission (GCRA strict bound) at 1, 3 and 5 replicas"
	if ran m3 || ran acc-strict-1 || ran acc-strict-5; then
		v=PASS m=""
		for s in acc-strict-1 m3 acc-strict-5; do
			case $s in m3) n=3 ;; *) n=${s#acc-strict-} ;; esac
			if ! ran "$s"; then v=FAIL m="$m${m:+; }$n replicas: not run"; continue; fi
			IFS=$'\t' read -r sv over b503 <<<"$(acc "$s")"
			[ "$sv" = PASS ] || v=FAIL
			m="$m${m:+; }$n replicas $over ($sv; boundary 503s $b503)"
		done
		row NFR-06 "$v" true "$t06" "$where, make accuracy REPLICAS=1 and 5, make verify-m3" \
			"max over-admission uniform/bursty/boundary: $m; the boundary 503s are a known limit (ADR-0079)"
	else not_run NFR-06 "$t06" "$where"; fi

	# NFR-07
	local t07="over-admission ≤ replicas × 10 (lease_k 10) at 1, 3 and 5 replicas"
	if ran acc-batched-1 || ran acc-batched-3 || ran acc-batched-5; then
		v=PASS m=""
		for n in 1 3 5; do
			s=acc-batched-$n
			if ! ran "$s"; then v=FAIL m="$m${m:+; }$n replicas: not run"; continue; fi
			IFS=$'\t' read -r sv over b503 <<<"$(acc "$s")"
			[ "$sv" = PASS ] || v=FAIL
			m="$m${m:+; }$n replicas $over of $((n * 10)) ($sv; boundary 503s $b503)"
		done
		row NFR-07 "$v" true "$t07" "$where, make accuracy MODE=batched" "max over-admission uniform/bursty/boundary: $m"
	else not_run NFR-07 "$t07" "$where"; fi

	# NFR-08, NFR-09, NFR-12
	local t08="publish → applied on all 5 replicas, p99 < 5 s (60 publishes)"
	local t09="a revoked key rejected on all 5 replicas in < 5 s (20 revocations)"
	local t12="500 rps through a 10-min control-plane and Postgres outage: zero failed requests, config frozen, recovery ≤ 60 s, staleness alert fires"
	if ran m5; then
		row NFR-08 "$(verdict_of m5 'NFR-08')" true "$t08" "$where, 5 managed gateways, make verify-m5 (a)" "$(meas m5 '(a) publish on all 5 replicas, p99 < 5 s')"
		row NFR-09 "$(verdict_of m5 'NFR-09')" true "$t09" "$where, 5 managed gateways, make verify-m5 (b)" "$(meas m5 '(b) revoked key rejected on all 5 replicas < 5 s')"
		row NFR-12 "$(verdict_of m5 'NFR-12')" true "$t12" "$where, make verify-m5 (c)" \
			"$(tally m5 'NFR-12'); (c1) $(meas m5 '(c1) zero failed requests'); (c6) $(meas m5 '(c6) staleness alert fires in the outage')"
	else
		not_run NFR-08 "$t08" "$where"
		not_run NFR-09 "$t09" "$where"
		not_run NFR-12 "$t12" "$where"
	fi

	# NFR-10
	local t10="≥ 1M keys at ≤ 200 B Redis memory per key, GCRA and sliding window; every call allowed, Σ Δ DBSIZE within 1%"
	if ran keys; then
		row NFR-10 "$(verdict_of keys)" true "$t10" "$where, make nfr-keys" \
			"GCRA $(meas keys 'NFR-10 gcra keys: <= 200 B per key at 1000000' | sed -E 's/ \(Σ[^)]*\)//; s/, DBSIZE off by/, Σ Δ DBSIZE off by/'); sliding window $(meas keys 'NFR-10 sliding_window keys: <= 200 B per key at 1000000' | sed -E 's/ \(Σ[^)]*\)//; s/, DBSIZE off by/, Σ Δ DBSIZE off by/')"
	else not_run NFR-10 "$t10" "$where"; fi

	# NFR-11
	local t11="gateway-caused 5xx ≤ 0.1% of requests (2,000 rps from 4 tenants, 10 min)"
	if ran soak; then
		row NFR-11 "$(verdict_of soak)" true "$t11" "$where, make nfr-soak" "$(meas soak 'NFR-11 gateway-caused 5xx <= 0.1%')"
	else not_run NFR-11 "$t11" "$where"; fi

	# NFR-13
	local t13="each policy follows its failure mode within one Redis budget: full outage and killed primary (ADR-0021, ADR-0027, ADR-0034)"
	if ran m3; then
		row NFR-13 "$(verdict_of m3 'NFR-13')" true "$t13" "$where, make verify-m3 (b)" "$(tally m3 'NFR-13')"
	else not_run NFR-13 "$t13" "$where"; fi

	# NFR-14
	local t14="zero failed requests through config reloads (1,000 rps, ≥ 25 reloads) and a rolling restart on kind (300 rps HTTPS, all 3 pods replaced)"
	if ran m1-reload || [ -n "$want_kind" ]; then
		v=PASS m=""
		if ran m1-reload; then
			[ "$(verdict_of m1-reload)" = PASS ] || v=FAIL
			m="reload: $(meas m1-reload 'zero failed requests during reloads'); $(meas m1-reload 'config reloads applied under load')"
		else v=FAIL m="reload: not run"; fi
		if [ -n "$kind_why" ] || ! ran kind-rollout; then
			v=FAIL m="$m; kind: ${kind_why:-not run}"
		else
			[ "$(verdict_of kind-smoke)" = PASS ] || v=FAIL
			[ "$(verdict_of kind-rollout)" = PASS ] || v=FAIL
			m="$m; kind smoke: $(tally kind-smoke); rolling restart: $(tally kind-rollout), $(meas kind-rollout 'zero failed requests (NFR-14)' | sed -E 's/; status .*//'); $(meas kind-rollout 'rollout complete in 5 min, 3 new pods Ready')"
		fi
		row NFR-14 "$v" true "$t14" "$where (reload), kind (rolling restart)" "$m"
	else not_run NFR-14 "$t14" "$where, kind"; fi

	# NFR-15
	local t15="each quiet tenant: median over 30 slot pairs of p99(noisy on) ÷ p99(noisy off) < 1.10, the noisy tenant at 10× its limit"
	if ran noisy; then
		m=""
		for q in B C D; do
			m="$m${m:+; }$q $(meas noisy "NFR-15 quiet tenant $q: p99 on/off < 1.10" | sed -E 's/; pooled.*//')"
		done
		row NFR-15 "$(verdict_of noisy)" true "$t15" "$where, make nfr-noisy" "$m"
	else not_run NFR-15 "$t15" "$where"; fi

	# The chaos suite's extra scenarios count like any line (ADR-0074).
	if ran latency; then
		row "Chaos: Redis +50 ms" "$(verdict_of latency)" true "each policy follows its failure mode within the budget; Redis decides again within 5 s (ADR-0074, ADR-0083)" \
			"$where, make chaos-redis-latency" "$(tally latency); degraded $(meas latency 'degraded: admits within ±10% of expected')"
	elif [ "$parts" = all ]; then not_run "Chaos: Redis +50 ms" "Redis latency +50 ms" "$where"; fi
	if ran gwkill; then
		row "Chaos: gateway kill -9" "$(verdict_of gwkill)" true "only the killed gateway's in-flight requests fail (ADR-0074, ADR-0083)" \
			"$where, make chaos-gateway-kill" "$(tally gwkill); gateway-2: $(meas gwkill "only gateway-2's in-flight requests failed")"
	elif [ "$parts" = all ]; then not_run "Chaos: gateway kill -9" "gateway kill -9" "$where"; fi
	if ran m4; then
		row "Chaos: upstream 5xx" "$(verdict_of m4)" true "≥ 99% of requests succeed within 10 s of an endpoint failing, retries within budget (ADR-0031)" \
			"$where, make verify-m4" "$(tally m4)"
	elif [ "$parts" = all ]; then not_run "Chaos: upstream 5xx" "upstream endpoint returns 5xx" "$where"; fi

	awk -F'\t' '{ printf "%-16s  %-22s %s\n", $2, $1, $6 }' "$rows"
	local failed excluded="" exit_word
	failed=$(awk -F'\t' '$3 == "true" && $2 != "PASS"' "$rows" | wc -l | tr -d ' ')
	if awk -F'\t' '$1 == "NFR-02" && $3 == "false"' "$rows" | grep -q .; then
		excluded="NFR-02 is not counted: an open known limit the user accepted (ADR-0084 §4), so it is left out of the exit status; its line is not a PASS"
	fi
	if awk -F'\t' '$1 == "NFR-05" && $3 == "false"' "$rows" | grep -q .; then
		excluded="$excluded${excluded:+. }NFR-05 is not counted: x64 landed on a CPU model with no main baseline yet, so the gate had nothing to compare (NOT DEMONSTRATED, the user's decision, ADR-0089), and arm64 passed against main's baseline; its line is not a PASS"
	fi
	if [ "$failed" -eq 0 ]; then exit_word=PASS; else exit_word="FAIL ($failed rows)"; fi

	jq -n --rawfile rows "$rows" --rawfile steps "$out/steps.tsv" \
		--arg commit "$commit" --arg branch "$branch" --arg repo_url "$repo_url" --arg parts "$parts" \
		--arg started "$started" --arg finished "$finished" --argjson duration "$duration" \
		--arg exit "$exit_word" --arg excluded "$excluded" --arg laptop "$laptop" --arg date "$today" \
		--arg bench_run "$bench_run" --arg logs "$out" '
		def tsv: split("\n") | map(select(length > 0) | split("\t"));
		{commit: $commit, branch: $branch, repo_url: $repo_url, parts: $parts, started: $started, finished: $finished,
		 duration_s: $duration, laptop: $laptop, exit: $exit, excluded: $excluded, bench_run: $bench_run, logs: $logs,
		 steps: [$steps | tsv[] | {name: .[0], exit_code: (.[1] | tonumber), seconds: (.[2] | tonumber), budget_s: (.[3] | tonumber)}],
		 rows: [$rows | tsv[] | {id: .[0], verdict: .[1], counted: (.[2] == "true"), target: .[3], where: .[4], measured: .[5],
			date: $date, commit: $commit,
			links: [(.[6] // "") | split("|")[] | select(length > 0) | capture("^(?<name>[^=]+)=(?<url>.+)$")]}]}' >"$out.json"

	echo
	[ -z "$excluded" ] || echo "$excluded."
	[ "$parts" = all ] || echo "Partial run ($parts): not the M7 exit evidence."
	echo "took $(mins "$duration") (budget 150 min for a full run$([ "$parts" = all ] && [ "$duration" -gt 9000 ] && echo ": OVER BUDGET")); results in $out.json, logs in $out/"
	echo "M7 exit: $exit_word"
	[ "$failed" -eq 0 ]
}

case ${1:-} in
readme)
	[ -n "${2:-}" ] || { echo "usage: test/verify/m7.sh readme <test/verify/out/m7-<stamp>.json>" >&2; exit 2; }
	readme "$2"
	exit
	;;
report)
	[ -d "${2:-}" ] || { echo "usage: test/verify/m7.sh report <test/verify/out/m7-<stamp>>" >&2; exit 2; }
	report "${2%/}"
	exit
	;;
esac

# --- what to run -------------------------------------------------------------

parts=${*:-all}
want_bench="" want_kind="" steps=()
add_step() {
	local s t
	for s in "$@"; do
		for t in ${steps[@]+"${steps[@]}"}; do [ "$t" = "$s" ] && continue 2; done
		steps+=("$s")
	done
}
compose_steps=(m1-reload m2 m3 acc-strict-1 acc-strict-5 acc-batched-1 acc-batched-3 acc-batched-5 m5 keys soak noisy latency gwkill m4)
for p in $parts; do
	case $p in
	all) want_bench=1 want_kind=1; add_step "${compose_steps[@]}" ;;
	bench | 1 | 2 | 5) want_bench=1 ;;
	compose) add_step "${compose_steps[@]}" ;;
	kind) want_kind=1 ;;
	chaos) add_step latency gwkill m4 ;;
	3) add_step m2 ;;
	4 | 13) add_step m3 ;;
	6) add_step m3 acc-strict-1 acc-strict-5 ;;
	7) add_step acc-batched-1 acc-batched-3 acc-batched-5 ;;
	8 | 9 | 12) add_step m5 ;;
	10) add_step keys ;;
	11) add_step soak ;;
	14) add_step m1-reload; want_kind=1 ;;
	15) add_step noisy ;;
	*) echo "verify-m7: unknown part '$p' (all, bench, compose, kind, chaos or NFR numbers 1-15)" >&2; exit 2 ;;
	esac
done
ordered=() # the full run's order
for s in "${compose_steps[@]}"; do
	for t in ${steps[@]+"${steps[@]}"}; do [ "$t" = "$s" ] && ordered+=("$s"); done
done
steps=(${ordered[@]+"${ordered[@]}"})

compose=(docker compose --env-file .env.example)
if [ -f .env.local ]; then compose+=(--env-file .env.local); fi

stamp=$(date -u +%Y%m%dT%H%M%SZ)
started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
t_start=$SECONDS
out=test/verify/out/m7-$stamp
mkdir -p "$out"
: >"$out/steps.tsv"
commit=$(git rev-parse HEAD)
branch=$(git rev-parse --abbrev-ref HEAD)
repo_url=https://github.com/$(git remote get-url origin | sed -E 's#^(git@github\.com:|https://github\.com/)##; s#\.git$##')
owner=$(sed -E 's#^https://github\.com/([^/]+)/.*#\1#' <<<"$repo_url")
laptop="$(sysctl -n machdep.cpu.brand_string 2>/dev/null || uname -m), Docker Desktop $(docker info --format '{{.NCPU}} {{.MemTotal}}' 2>/dev/null | awk '{ printf "%s CPUs, %.1f GB", $1, $2 / 1e9 }')"
echo "verify-m7 ($parts) on ${commit:0:7} ($branch), $started; logs in $out/"

# --- cleanup -----------------------------------------------------------------

dir=deploy/compose/gateway
cp "$dir/config.yaml" "$out/config.yaml.orig"
cp "$dir/identity.yaml" "$out/identity.yaml.orig"
kind_up=""      # this run created the kind cluster
compose_down="" # this run stopped the compose stack
touched=""      # this run ran a compose step
cleanup() {
	local f n
	for f in config.yaml identity.yaml; do
		if ! cmp -s "$out/$f.orig" "$dir/$f"; then
			cp "$out/$f.orig" "$dir/.$f.swap" && mv -f "$dir/.$f.swap" "$dir/$f"
			echo "      restored $dir/$f"
		fi
	done
	if [ -n "$kind_up" ]; then
		echo "      restoring: make kind-down"
		make --no-print-directory kind-down >>"$out/cleanup.log" 2>&1
	fi
	if [ -n "$touched" ] || [ -n "$compose_down" ]; then
		"${compose[@]}" --profile replicas rm -fsv gateway-2 gateway-3 gateway-4 gateway-5 >>"$out/cleanup.log" 2>&1
		for n in 0 1 2 3 4 5; do curl -s -o /dev/null --max-time 5 -X DELETE "http://127.0.0.1:8474/proxies/redis-$n"; done
		# make dev recreates gateway-1 in static mode at compose's default
		# trace ratio, as a plain make dev leaves it.
		if ! make --no-print-directory dev >>"$out/cleanup.log" 2>&1; then
			echo "      restoring: make dev failed (see $out/cleanup.log)"
		elif [ -n "$compose_down" ]; then
			echo "      restored the compose stack (make dev)"
		fi
	fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# run_step <step> <budget s> <command>...: runs it with its output in
# $out/<step>.log, prints its check lines indented and its time against its
# budget (over budget is reported, not failed: ADR-0074).
run_step() {
	local name=$1 budget=$2 t0 code took over=""
	shift 2
	t0=$SECONDS
	"$@" >"$out/$name.log" 2>&1
	code=$?
	took=$((SECONDS - t0))
	echo "$code" >"$out/$name.code"
	printf '%s\t%s\t%s\t%s\n' "$name" "$code" "$took" "$budget" >>"$out/steps.tsv"
	grep -E '^(PASS|FAIL) |^(uniform|bursty|boundary) +(PASS|FAIL) ' "$out/$name.log" | sed 's/^/      /'
	if [ "$took" -gt "$budget" ]; then over=": OVER BUDGET"; fi
	echo "      $* exited $code; took $(mins "$took") (budget $(mins "$budget")$over)"
}

# --- preflight and the bench dispatch ------------------------------------------

bench_run="" bench_url="" bench_why="" bench_dispatched=""
if [ -n "$want_bench" ]; then
	echo "== preflight (ADR-0074)"
	dirty=$(git status --porcelain | grep -vxE ' M \.claude/settings\.json|\?\? docs/m1-code-guide\.md|\?\? dump\.rdb')
	login=$(gh api user --jq .login 2>/dev/null)
	remote=$(git ls-remote origin "refs/heads/$branch" 2>/dev/null | awk '{print $1}')
	if [ -n "$dirty" ]; then
		bench_why="preflight: the tree is not clean: $(tr '\n' ' ' <<<"$dirty")"
	elif [ "$login" != "$owner" ]; then
		bench_why="preflight: gh is logged in as '${login:-nobody}', not $owner"
	elif [ "$remote" != "$commit" ]; then
		bench_why="preflight: HEAD ${commit:0:7} is not origin/$branch (${remote:0:7})"
	fi
	if [ -n "$bench_why" ]; then
		printf '%-4s  %-44s %s\n' FAIL "preflight" "${bench_why#preflight: }"
	else
		printf '%-4s  %-44s %s\n' PASS "preflight" "clean tree, gh as $login, ${commit:0:7} = origin/$branch"
		echo "== NFR-01 and NFR-05: bench.yml -f gate=true on ${commit:0:7} (both arches on GitHub's runners, about 20-40 min)"
		if [ -n "${VERIFY_M7_BENCH_RUNS:-}" ]; then
			bench_run=${VERIFY_M7_BENCH_RUNS%% *}
			sha=$(gh run view "$bench_run" --json headSha --jq .headSha 2>/dev/null)
			title=$(gh run view "$bench_run" --json displayTitle --jq .displayTitle 2>/dev/null)
			if [ "$sha" != "$commit" ]; then
				bench_why="run $bench_run is for commit ${sha:0:7}, not HEAD ${commit:0:7}"
				bench_run=""
			elif [ "$title" != "bench throughput x64=$floor_x64 arm64=$floor_arm64 gate=true" ]; then
				bench_why="run $bench_run is '$title', not a gated run at the floors"
				bench_run=""
			else
				echo "      reusing run $bench_run (VERIFY_M7_BENCH_RUNS)"
			fi
		else
			since=$(date -u -v-60S +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '-60 seconds' +%Y-%m-%dT%H:%M:%SZ)
			if gh workflow run bench.yml --ref "$branch" -f gate=true >"$out/dispatch.log" 2>&1; then
				for _ in $(seq 36); do
					sleep 5
					bench_run=$(gh run list --workflow bench.yml --branch "$branch" --event workflow_dispatch -L 20 \
						--json databaseId,headSha,createdAt,displayTitle \
						--jq "[.[] | select(.headSha == \"$commit\" and .createdAt >= \"$since\" and .displayTitle == \"bench throughput x64=$floor_x64 arm64=$floor_arm64 gate=true\")] | sort_by(.createdAt) | last | .databaseId // empty" 2>/dev/null)
					[ -z "$bench_run" ] || break
				done
				[ -n "$bench_run" ] || bench_why="dispatched, but the run did not appear within 3 minutes"
			else
				bench_why="gh workflow run failed: $(tail -1 "$out/dispatch.log")"
			fi
		fi
		if [ -n "$bench_run" ]; then
			bench_dispatched=$SECONDS
			bench_url=$repo_url/actions/runs/$bench_run
			echo "      run $bench_url"
		else
			echo "      $bench_why"
		fi
	fi
fi

# --- the compose part ----------------------------------------------------------

step_title() {
	case $1 in
	m1-reload) echo "NFR-14 config reload under load (make verify-m1-reload)" ;;
	m2) echo "NFR-03 added latency, local limits (make verify-m2)" ;;
	m3) echo "NFR-04, NFR-06 at 3 replicas, NFR-13 (make verify-m3)" ;;
	acc-strict-*) echo "NFR-06 accuracy, strict, ${1#acc-strict-} replica(s) (make accuracy REPLICAS=${1#acc-strict-})" ;;
	acc-batched-*) echo "NFR-07 accuracy, batched, ${1#acc-batched-} replica(s) (make accuracy REPLICAS=${1#acc-batched-} MODE=batched)" ;;
	m5) echo "NFR-08, NFR-09, NFR-12 (make verify-m5)" ;;
	keys) echo "NFR-10 limiter keys (make nfr-keys)" ;;
	soak) echo "NFR-11 availability soak (make nfr-soak)" ;;
	noisy) echo "NFR-15 noisy neighbour (make nfr-noisy)" ;;
	latency) echo "chaos: Redis latency +50 ms (make chaos-redis-latency)" ;;
	gwkill) echo "chaos: gateway kill -9 under load (make chaos-gateway-kill)" ;;
	m4) echo "chaos: upstream endpoint returns 5xx (make verify-m4)" ;;
	esac
}
# ADR-0074's budgets: accuracy strict 8 min for two runs, batched 12 for
# three, the three chaos steps 8 together.
budget_of() {
	case $1 in
	m1-reload) echo 120 ;; m2) echo 720 ;; m3) echo 900 ;;
	acc-strict-* | acc-batched-*) echo 240 ;;
	m5) echo 1080 ;; keys) echo 480 ;; soak | noisy) echo 720 ;;
	latency | gwkill) echo 180 ;; m4) echo 120 ;;
	esac
}
target_of() {
	case $1 in
	m1-reload) echo verify-m1-reload ;; m2) echo verify-m2 ;; m3) echo verify-m3 ;;
	acc-strict-*) echo "accuracy REPLICAS=${1#acc-strict-}" ;;
	acc-batched-*) echo "accuracy REPLICAS=${1#acc-batched-} MODE=batched" ;;
	m5) echo verify-m5 ;; keys) echo nfr-keys ;; soak) echo nfr-soak ;; noisy) echo nfr-noisy ;;
	latency) echo chaos-redis-latency ;; gwkill) echo chaos-gateway-kill ;; m4) echo verify-m4 ;;
	esac
}
for s in ${steps[@]+"${steps[@]}"}; do
	touched=1
	echo "== $(step_title "$s")"
	# shellcheck disable=SC2046 # the target and its variables, one word each
	run_step "$s" "$(budget_of "$s")" make --no-print-directory $(target_of "$s")
done

# --- the kind part ---------------------------------------------------------------

kind_why=""
if [ -n "$want_kind" ]; then
	echo "== NFR-14 rolling restart on kind (ADR-0075, ADR-0086)"
	t_kind=$SECONDS
	if [ -n "$(docker ps -aq --filter label=io.x-k8s.kind.cluster=spillway)" ]; then
		kind_why="a kind cluster 'spillway' is already up; verify-m7 needs it down at the start (ADR-0074) and leaves it alone"
		echo "      $kind_why"
	else
		echo "      stopping the compose stack (docker compose down, no -v): kind and compose do not fit in Docker Desktop's memory together (ADR-0075); make dev brings it back after"
		compose_down=1
		"${compose[@]}" down >"$out/compose-down.log" 2>&1
		kind_up=1
		run_step kind-up 900 make --no-print-directory kind-up
		if [ "$(code_of kind-up)" -eq 0 ]; then
			run_step kind-smoke 180 make --no-print-directory kind-smoke
			run_step kind-rollout 420 make --no-print-directory kind-rollout
		else
			kind_why="make kind-up exited $(code_of kind-up): $(tail -1 "$out/kind-up.log")"
			echo "      $kind_why"
		fi
		run_step kind-down 120 make --no-print-directory kind-down
		[ "$(code_of kind-down)" -ne 0 ] || kind_up=""
		run_step dev 600 make --no-print-directory dev
		[ "$(code_of dev)" -ne 0 ] || compose_down=""
	fi
	took=$((SECONDS - t_kind))
	over=""
	[ "$took" -le 1500 ] || over=": OVER BUDGET"
	echo "      kind part took $(mins "$took") (budget 25 min$over)"
fi

# --- waiting for the bench run ---------------------------------------------------

: >"$out/bench-files.txt"
if [ -n "$want_bench" ]; then
	if [ -n "$bench_run" ]; then
		echo "== waiting for bench run $bench_run (at most 120 min from its dispatch)"
		state=""
		while :; do
			state=$(gh run view "$bench_run" --json status,conclusion --jq '"\(.status) \(.conclusion)"' 2>/dev/null)
			case $state in completed*) break ;; esac
			if [ $((SECONDS - bench_dispatched)) -ge 7200 ]; then
				bench_why="bench run not finished within 120 min of its dispatch ($state)"
				break
			fi
			sleep 60
		done
		echo "      run $bench_run: ${state:-unknown}, $(mins $((SECONDS - bench_dispatched))) after its dispatch"
	fi
	if git fetch -q origin bench-results 2>/dev/null; then
		git ls-tree -r --name-only origin/bench-results results/ | sort >"$out/bench-files.txt"
	fi
	while read -r f; do
		git show "origin/bench-results:$f" >"$out/bench-$(tr / _ <<<"$f")" 2>/dev/null
	done <"$out/bench-files.txt"
fi

# --- the report --------------------------------------------------------------------

finished=$(date -u +%Y-%m-%dT%H:%M:%SZ)
duration=$((SECONDS - t_start))
for v in parts want_bench want_kind bench_run bench_url bench_why kind_why commit branch repo_url started finished duration laptop; do
	printf '%s=%q\n' "$v" "${!v}"
done >"$out/state.env"
report "$out"
code=$?
if [ "${README:-}" = 1 ]; then readme "$out.json" || code=1; fi
exit "$code"
