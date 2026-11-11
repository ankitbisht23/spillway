# Added latency by alternating paired runs (U4 in ADR-0021), sourced by
# m2.sh (NFR-03) and m3.sh (NFR-04). Three pairs, each run with its own
# warm-up, at the same rate and length, with the same loadgen and the same
# upstream: straight to upstream-1, then through gateway-1, three times. The
# added p99 is the median of the three gateway runs' p99s minus the median of
# the three direct runs' p99s, and passes from 0 to the target. A run below
# 99% of its target rate (loadgen's valid), with any answer other than 2xx,
# or that the caller's judge rejects fails its line and leaves no three
# pairs to compare; a negative added p99 fails too: the gateway cannot make
# requests faster, so it means the measurement was noise.
#
# The caller defines result, check, $failed and loadgen <url> <rate>
# <warm-up> <duration> as m2.sh does, and for the gateway runs:
#   counts                         prints gateway-1's counters, one line each
#   judge <before> <after> <sent>  prints "<0 = pass> <what it saw>"
# shellcheck shell=bash

# runline <check> <loadgen result> [extra] [judged]: prints the run's line
# and sets p50 and p99 in µs; returns 0 if the run passes.
runline() {
	local r ok line
	r=$(jq -r --arg extra "${3:-}" --argjson judged "${4:-0}" "$failed"' as $failed
		| "\(if $failed == 0 and .valid and $judged == 0 then 0 else 1 end) \(.latency_us.p50) \(.latency_us.p99) \(.status["2xx"] // 0)/\(.requests.sent) answered 2xx, \($failed) failed, \(.achieved_rps | round) of \(.target_rps) rps; p50 \(.latency_us.p50 / 1000) ms, p99 \(.latency_us.p99 / 1000) ms\($extra)"' <<<"$2" 2>/dev/null)
	if [ -z "$r" ]; then
		result FAIL "$1" "loadgen gave no result: $(tail -1 "$tmp/loadgen.err")"
		return 1
	fi
	read -r ok p50 p99 line <<<"$r"
	check "$ok" "$1" "$line"
	return "$ok"
}

median() { printf '%s\n' "$@" | sort -n | sed -n 2p; } # median of three

# added_latency <check> <target µs> <gateway label> <rate> <warm-up> <duration>
added_latency() {
	local name=$1 target=$2 label=$3 rate=$4 warmup=$5 duration=$6
	local i out before sent j valid=1 d50=() d99=() g50=() g99=()
	for i in 1 2 3; do
		out=$(loadgen http://upstream-1:8080/bytes/1024 "$rate" "$warmup" "$duration")
		if runline "direct to upstream-1, run $i" "$out"; then d50+=("$p50") d99+=("$p99"); else valid=0; fi
		before=$(counts)
		out=$(loadgen http://gateway-1:8000/bytes/1024 "$rate" "$warmup" "$duration")
		sent=$(jq -r '.requests.sent // 0' <<<"$out" 2>/dev/null)
		j=$(judge "$before" "$(counts)" "${sent:-0}")
		if runline "gateway-1, $label, run $i" "$out" "; ${j#* }" "${j%% *}"; then g50+=("$p50") g99+=("$p99"); else valid=0; fi
	done
	if [ "$valid" -eq 0 ]; then
		result FAIL "$name" "no three valid pairs of runs to compare"
		return
	fi
	local g d added
	g=$(median "${g99[@]}") d=$(median "${d99[@]}")
	added=$((g - d))
	check $([ "$added" -ge 0 ] && [ "$added" -le "$target" ]; echo $?) "$name" \
		"$(awk -v g="$g" -v d="$d" -v gm="$(median "${g50[@]}")" -v dm="$(median "${d50[@]}")" \
			-v gs="${g99[*]}" -v ds="${d99[*]}" 'function ms(s, a, n, i, o) {
				n = split(s, a, " "); for (i = 1; i <= n; i++) o = o (i > 1 ? ", " : "") sprintf("%.3f", a[i] / 1000); return o
			}
			BEGIN { printf "median p99 %.3f (of %s) - %.3f (of %s) = %.3f ms added; median p50 %.3f - %.3f = %.3f ms added",
				g / 1000, ms(gs), d / 1000, ms(ds), (g - d) / 1000, gm / 1000, dm / 1000, (gm - dm) / 1000 }')"
}
