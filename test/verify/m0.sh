#!/usr/bin/env bash
# M0 exit checks (docs/design.md, M0). Prints one PASS/FAIL line per check with
# what it measured, and exits 1 if any check fails. Run through make:
#   make verify-m0-stack   the running dev stack (CI runs this part)
#   make verify-m0         the stack, then CI status and the main ruleset on GitHub
set -uo pipefail
cd "$(dirname "$0")/../.."

mode=${1:-all}
fails=0

result() { # result PASS|FAIL check measured
	printf '%-4s  %-44s %s\n' "$1" "$2" "$3"
	if [ "$1" != PASS ]; then fails=$((fails + 1)); fi
}
check() { # check <0 = pass> check measured
	if [ "$1" -eq 0 ]; then result PASS "$2" "$3"; else result FAIL "$2" "$3"; fi
}

# Last value of a variable in the env files compose reads; the shell wins, as
# for compose. Used for the Grafana password, which is never printed.
env_value() {
	local v
	v=$(cat .env.example $([ -f .env.local ] && echo .env.local) | grep -E "^$1=" | tail -1 | cut -d= -f2-)
	printf '%s' "${!1:-$v}"
}

compose=(docker compose --env-file .env.example)
if [ -f .env.local ]; then compose+=(--env-file .env.local); fi

echo "== dev stack"

# Every long-running service healthy (and has a healthcheck); every one-shot
# service (label spillway.oneshot) exited 0 (ADR-0004).
ps=$("${compose[@]}" ps -a --format json | jq -s 'flatten | map(.oneshot = (.Labels | test("(^|,)spillway.oneshot=true(,|$)")))')
running=$(jq '[.[] | select(.State == "running")] | length' <<<"$ps")
healthy=$(jq '[.[] | select(.State == "running" and .Health == "healthy")] | length' <<<"$ps")
nohc=$(jq -r '[.[] | select(.State == "running" and .Health == "") | .Service] | join(" ")' <<<"$ps")
other=$(jq -r '[.[] | select(.State != "running" and (.oneshot | not)) | "\(.Service)=\(.State)"] | join(" ")' <<<"$ps")
oneshot_ok=$(jq -r '[.[] | select(.oneshot and .State == "exited" and .ExitCode == 0) | .Service] | join(" ")' <<<"$ps")
oneshot_bad=$(jq -r '[.[] | select(.oneshot and (.State != "exited" or .ExitCode != 0)) | "\(.Service)=\(.State):\(.ExitCode)"] | join(" ")' <<<"$ps")
check $([ "$running" -gt 0 ] && [ "$healthy" -eq "$running" ] && [ -z "$other" ]; echo $?) \
	"long-running services healthy" "$healthy/$running${other:+ (not running: $other)}"
check $([ -z "$nohc" ]; echo $?) "services without a healthcheck" "${nohc:-none}"
check $([ -n "$oneshot_ok" ] && [ -z "$oneshot_bad" ]; echo $?) \
	"one-shot services exited 0" "${oneshot_ok:-none}${oneshot_bad:+; failed: $oneshot_bad}"

# Redis Cluster from the host through the address map (ADR-0004).
out=$(go test -tags integration -count=1 -run '^TestRedisCluster$' -v ./test/integration/ 2>&1)
rc=$?
measures=$(grep -o 'measure: .*' <<<"$out" | sed 's/^measure: //')
if [ -z "$measures" ]; then measures="no measurements; go test output follows"; fi
while IFS= read -r m; do check "$rc" "redis cluster" "$m"; done <<<"$measures"
if [ "$rc" -ne 0 ]; then printf '%s\n' "$out" | tail -20; fi

# Keycloak realm discovery, with the browser-facing issuer (ADR-0004).
want=http://localhost:8180/auth/realms/spillway
iss=$(curl -fsS --max-time 10 "$want/.well-known/openid-configuration" | jq -r .issuer)
check $([ "$iss" = "$want" ]; echo $?) "keycloak OIDC discovery issuer" "${iss:-unreachable}"

# Grafana datasources provisioned and passing Grafana's own health check.
ok=""
for uid in prometheus tempo loki; do
	status=$(printf 'user = "admin:%s"\n' "$(env_value GRAFANA_ADMIN_PASSWORD)" |
		curl -sS --max-time 20 -K - "http://localhost:3000/api/datasources/uid/$uid/health" | jq -r .status)
	if [ "$status" = OK ]; then ok="$ok $uid"; fi
done
n=$(wc -w <<<"$ok" | tr -d ' ')
check $([ "$n" -eq 3 ]; echo $?) "grafana datasources healthy" "$n/3${ok:+ (${ok# })}"

if [ "$mode" = all ]; then
	echo "== github"

	# CI on this commit: the PR's workflow run, and its aggregate ci-ok job.
	branch=$(git rev-parse --abbrev-ref HEAD)
	sha=$(git rev-parse HEAD)
	run=$(gh run list --workflow ci.yml --branch "$branch" --commit "$sha" --limit 1 --json databaseId,url 2>/dev/null)
	id=$(jq -r '.[0].databaseId // empty' <<<"$run")
	cionk=""
	if [ -n "$id" ]; then
		cionk=$(gh run view "$id" --json jobs --jq '.jobs[] | select(.name == "ci-ok") | .conclusion')
	fi
	check $([ "$cionk" = success ]; echo $?) "ci-ok on ${sha:0:7}" \
		"${cionk:-no run}${id:+ $(jq -r '.[0].url' <<<"$run")}"

	# The ruleset protecting main requires ci-ok.
	repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
	checks=$(gh api "repos/$repo/rulesets" --jq '.[] | select(.target == "branch" and .enforcement == "active") | .id' |
		while read -r rid; do
			gh api "repos/$repo/rulesets/$rid" --jq 'select(.conditions.ref_name.include | index("~DEFAULT_BRANCH") or index("refs/heads/main"))
				| .rules[] | select(.type == "required_status_checks") | .parameters.required_status_checks[].context'
		done | sort -u | tr '\n' ' ')
	check $(grep -qw ci-ok <<<"$checks"; echo $?) "main ruleset requires ci-ok" "required: ${checks:-none}"
fi

echo
if [ "$fails" -eq 0 ]; then echo "M0 exit: PASS"; else echo "M0 exit: FAIL ($fails checks)"; fi
[ "$fails" -eq 0 ]
