#!/usr/bin/env bash
# make kind-smoke (ADR-0075 §D10): checks the kind cluster that make kind-up
# built, one PASS/FAIL line per check, in at most 3 minutes (every wait
# below has its own timeout). Run from the repository root with BIN and
# KUBECONFIG from the Makefile; kubectl is a function that always passes
# --context kind-spillway. The Redis password stays in the pods' own
# environment and is never printed.
#
# With the chart (ADR-0086) it also checks the gateway and control-plane
# rollouts, the gateway through its Service, the admin ports closed to an
# ordinary pod (GW-25's NetworkPolicies), Prometheus's targets, and the edge
# from the host: https://127.0.0.1:443 with the mkcert CA, nothing on :80.
# With observability as code (ADR-0087), Prometheus's alert rules and
# Grafana's dashboards loaded.
set -uo pipefail

: "${BIN:?BIN is set by the Makefile}"
: "${KUBECONFIG:?KUBECONFIG is set by the Makefile}"
case "$KUBECONFIG" in */.kube/spillway-kind) ;; *) echo "KUBECONFIG must be ~/.kube/spillway-kind, not $KUBECONFIG" >&2; exit 2 ;; esac
kubectl() { "$BIN/kubectl" --context kind-spillway --request-timeout=20s "$@"; }

start=$(date +%s)
fails=0
result() { # result PASS|FAIL check measured
	printf '%-4s  %-44s %s\n' "$1" "$2" "$3"
	if [ "$1" != PASS ]; then fails=$((fails + 1)); fi
}
check() { # check <0 = pass> check measured
	if [ "$1" -eq 0 ]; then result PASS "$2" "$3"; else result FAIL "$2" "$3"; fi
}

# 1. Three nodes, all Ready.
ready=$(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}={.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>&1)
n=$(grep -c '=True$' <<<"$ready")
[ "$n" -eq 3 ] && [ "$(grep -c . <<<"$ready")" -eq 3 ]
check $? "three nodes Ready" "$n of $(grep -c . <<<"$ready") Ready"

# 2. Every pod of the three namespaces Ready, finished Jobs' pods excepted;
# the dependency namespaces must not be empty.
for ns in spillway-deps spillway-obs spillway; do
	pods=$(kubectl -n "$ns" get pods -o jsonpath='{range .items[*]}{.metadata.name} {.status.phase} {.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>&1)
	total=$(awk 'NF && $2 != "Succeeded"' <<<"$pods" | grep -c .)
	notready=$(awk 'NF && $2 != "Succeeded" && $3 != "True" {printf "%s ", $1}' <<<"$pods")
	ok=1
	if [ -n "$notready" ]; then ok=0; fi
	if [ "$total" -eq 0 ]; then ok=0; fi
	if [ "$ok" -eq 1 ]; then result PASS "pods Ready in $ns" "$total pods"; else result FAIL "pods Ready in $ns" "$total pods; not Ready: ${notready:-none}"; fi
done

# 3. Redis Cluster: cluster_state:ok on all six nodes, asked inside each pod
# with its own password (REDISCLI_AUTH from its environment).
okn=0
for i in 0 1 2 3 4 5; do
	if kubectl -n spillway-deps exec "redis-$i" -c redis -- sh -c 'REDISCLI_AUTH="$REDIS_PASSWORD" redis-cli cluster info' 2>/dev/null | grep -q '^cluster_state:ok'; then
		okn=$((okn + 1))
	fi
done
[ "$okn" -eq 6 ]
check $? "redis cluster_state:ok" "$okn of 6 nodes"

# 4. The host side of the edge: the control-plane node publishes
# 127.0.0.1:443 → NodePort 30443 and, besides kind's API server on
# 127.0.0.1, nothing else; the workers publish nothing; no port 80 anywhere.
cp_ports=$(docker port spillway-control-plane 2>&1 | sort)
edge=$(grep -c '^30443/tcp -> 127.0.0.1:443$' <<<"$cp_ports")
others=$(grep -v -e '^30443/tcp -> 127.0.0.1:443$' -e '^6443/tcp -> 127.0.0.1:[0-9]*$' <<<"$cp_ports" | grep -c .)
workers=$(for w in spillway-worker spillway-worker2; do docker port "$w" 2>&1; done | grep -c .)
port80=$(docker ps --filter label=io.x-k8s.kind.cluster=spillway --format '{{.Ports}}' | grep -c ':80->')
[ "$edge" -eq 1 ] && [ "$others" -eq 0 ] && [ "$workers" -eq 0 ] && [ "$port80" -eq 0 ]
check $? "edge 127.0.0.1:443 -> 30443 only, no :80" "$(tr '\n' ',' <<<"$cp_ports" | sed 's/,$//'); worker mappings $workers"

# 5. The chart: the gateways and the control plane fully rolled out and
# Ready (the kubelet's /readyz on their admin ports, GW-24).
gw=$(kubectl -n spillway get deployment spillway-gateway -o jsonpath='{.spec.replicas} {.status.readyReplicas} {.status.updatedReplicas}' 2>&1)
read -r gw_want gw_ready gw_updated <<<"$gw"
[ -n "$gw_want" ] && [ "$gw_want" -ge 1 ] 2>/dev/null && [ "${gw_ready:-0}" = "$gw_want" ] && [ "${gw_updated:-0}" = "$gw_want" ]
check $? "gateway Deployment ready" "ready/updated/wanted ${gw_ready:-0}/${gw_updated:-0}/${gw_want:-?}"
cp=$(kubectl -n spillway get deployment spillway-controlplane -o jsonpath='{.spec.replicas} {.status.readyReplicas}' 2>&1)
read -r cp_want cp_ready <<<"$cp"
[ "$cp_want" = 1 ] && [ "${cp_ready:-0}" = 1 ]
check $? "control plane ready, replicas: 1 (CP-22)" "ready ${cp_ready:-0} of ${cp_want:-?} (its /readyz, by the kubelet)"

# The admin addresses an ordinary pod must not reach (GW-25): every gateway
# pod's :8001 and the control plane's :8081, on the pod IPs.
closed=$(kubectl -n spillway get pods -l 'app.kubernetes.io/name in (spillway-gateway,spillway-controlplane)' \
	-o jsonpath='{range .items[*]}{.metadata.labels.app\.kubernetes\.io/name} {.status.podIP}{"\n"}{end}' 2>/dev/null |
	awk '$2 != "" { printf "%s%s:%s", sep, $2, ($1 == "spillway-gateway" ? 8001 : 8081); sep = "," }')

# 6. From inside the cluster (chaos kindsmoke): Keycloak over HTTPS, the
# gateway through its Service, the admin ports closed, Prometheus's targets.
kubectl -n spillway delete job kind-smoke --ignore-not-found --wait=true >/dev/null 2>&1
if [ -n "$closed" ] && sed -e "s/CLOSED_ADDRS/$closed/" -e "s/GATEWAY_REPLICAS/${gw_want:-3}/" test/kind/smoke-job.yaml | kubectl apply -f - >/dev/null 2>&1; then
	state=
	# A deadline, not an iteration count, so slow kubectl calls cannot
	# stretch the wait past 110 s (the Job's own deadline is 100 s).
	deadline=$((SECONDS + 110))
	while [ "$SECONDS" -lt "$deadline" ]; do
		state=$(kubectl -n spillway get job kind-smoke -o jsonpath='{.status.conditions[?(@.status=="True")].type}' 2>/dev/null)
		case "$state" in *Complete* | *Failed*) break ;; esac
		sleep 1
	done
	out=$(kubectl -n spillway logs job/kind-smoke 2>&1)
	if [ -n "$out" ]; then printf '%s\n' "$out"; fi
	fails=$((fails + $(grep -c '^FAIL' <<<"$out")))
	case "$state" in
	*Complete*) result PASS "in-cluster checks (Job kind-smoke)" "complete" ;;
	*) result FAIL "in-cluster checks (Job kind-smoke)" "state: ${state:-not finished in 110 s}" ;;
	esac
	kubectl -n spillway delete job kind-smoke --ignore-not-found --wait=false >/dev/null 2>&1
else
	result FAIL "in-cluster checks (Job kind-smoke)" "could not create the Job (admin addresses: ${closed:-none found})"
fi

# 7. The edge from the host (GW-25, SEC-01): HTTPS on 127.0.0.1:443 through
# kind's port mapping and the NodePort, trusting only the mkcert CA; and no
# listener on 127.0.0.1:80 (curl: 7 = refused, 28 = timed out). 10 s and 3 s
# are far above a host stall (ADR-0034).
edge=$(curl -sS --max-time 10 --cacert "$("$BIN/mkcert" -CAROOT)/rootCA.pem" -o /dev/null -w '%{http_code} %{size_download}' https://127.0.0.1:443/bytes/16 2>&1)
[ "$edge" = "200 16" ]
check $? "host https://127.0.0.1:443/bytes/16 -> 200" "$edge"
curl -sS --max-time 3 -o /dev/null http://127.0.0.1:80/ >/dev/null 2>&1
rc=$?
[ "$rc" -eq 7 ] || [ "$rc" -eq 28 ]
check $? "nothing listening on host 127.0.0.1:80" "curl exit $rc"

# 8. Observability as code (OB-04, OB-05, ADR-0087): Prometheus loaded
# every rule of deploy/prometheus/rules and none fails to evaluate, and
# Grafana provisioned every dashboard of deploy/grafana/dashboards. Asked
# inside the pods; Grafana's admin password stays in its own environment.
want_rules=$(cat deploy/prometheus/rules/*.yaml | grep -cE '^ *- (alert|record): ')
rules=$(kubectl -n spillway-obs exec deploy/prometheus -c prometheus -- wget -qO- -T 10 http://127.0.0.1:9090/api/v1/rules 2>/dev/null |
	jq -r '[.data.groups[].rules[]] | "\(length) \([.[] | select(.health == "err")] | length)"' 2>/dev/null)
read -r got_rules err_rules <<<"$rules"
[ "${got_rules:-0}" -eq "$want_rules" ] && [ "${err_rules:-1}" -eq 0 ]
check $? "prometheus rules loaded (OB-05)" "${got_rules:-none} of $want_rules rules, ${err_rules:-?} failing"
want_dash=$(find deploy/grafana/dashboards -name '*.json' | grep -c .)
# shellcheck disable=SC2016 # expanded in the Grafana container
dash=$(kubectl -n spillway-obs exec deploy/grafana -c grafana -- sh -c \
	'wget -qO- -T 10 --header "Authorization: Basic $(printf "admin:%s" "$GF_SECURITY_ADMIN_PASSWORD" | base64)" "http://127.0.0.1:3000/api/search?tag=spillway&type=dash-db"' 2>/dev/null |
	jq -r '[.[].uid] | sort | join(",")' 2>/dev/null)
n=$(tr ',' '\n' <<<"$dash" | grep -c .)
[ "$n" -eq "$want_dash" ]
check $? "grafana dashboards provisioned (OB-04)" "$n of $want_dash${dash:+: $dash}"

echo "kind smoke: $(($(date +%s) - start)) s"
if [ "$fails" -eq 0 ]; then echo "kind smoke: PASS"; else echo "kind smoke: FAIL ($fails)"; exit 1; fi
