#!/usr/bin/env bash
# NFR-10 limiter keys (ADR-0074 §D9): cmd/chaos's key-count sweep ("chaos
# keys", the compose service chaos) in the compose network against the dev
# stack's Redis Cluster: 250,000, 500,000 and 1,000,000 GCRA keys, then as
# many sliding-window keys, with Σ Δused_memory ÷ Σ Δ DBSIZE of the three
# primaries per step. Prints each step and one PASS/FAIL line per kind, the
# JSON report goes to $NFR_OUT when set, and the test's keys are removed on
# every exit. About 8 minutes; it does not use the gateway. Run through make:
#   make nfr-keys
# Extra arguments go to chaos keys, e.g. test/load/keys.sh -steps 10000,20000
set -uo pipefail
cd "$(dirname "$0")/../.."

compose=(docker compose --env-file .env.example)
if [ -f .env.local ]; then compose+=(--env-file .env.local); fi
tmp=$(mktemp -d)
cleanup() {
	"${compose[@]}" run --rm -T chaos keys -cleanup >/dev/null 2>&1
	rm -rf "$tmp"
}

echo "== NFR-10 limiter keys"
if ! build=$("${compose[@]}" build --quiet chaos 2>&1); then printf '%s\n' "$build"; exit 1; fi
trap cleanup EXIT
trap 'exit 1' INT TERM
start=$SECONDS
"${compose[@]}" run --rm -T chaos keys "$@" >"$tmp/report.json" 2>"$tmp/keys.err"
code=$?
grep -E '^(PASS|FAIL|chaos keys:|      )' "$tmp/keys.err" # not compose's progress lines
if [ -n "${NFR_OUT:-}" ]; then cp "$tmp/report.json" "$NFR_OUT"; fi
echo "took $((SECONDS - start)) s"
if [ "$code" -ne 0 ] && ! grep -q '^FAIL ' "$tmp/keys.err"; then
	printf '%-4s  %-44s %s\n' FAIL "chaos keys" "exited $code"
fi
echo
if [ "$code" -eq 0 ]; then echo "NFR-10 limiter keys: PASS"; else echo "NFR-10 limiter keys: FAIL"; fi
exit "$code"
