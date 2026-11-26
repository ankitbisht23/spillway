#!/usr/bin/env bash
# make kind-up and make kind-images (ADR-0075 §D10), run from the repository
# root with BIN (the pinned kind and kubectl) and KUBECONFIG
# (~/.kube/spillway-kind) from the Makefile.
#
#   up.sh          the cluster (created if absent), Secrets and ConfigMaps,
#                  dependencies and observability, images; waits until every
#                  dependency is ready; then the chart (ADR-0086): helm
#                  upgrade --install, the control plane, the seed Job that
#                  publishes deploy/kind/config.yaml, and the gateways
#   up.sh images   only builds the local images and loads them into kind
#
# kubectl below is a function that always passes --context kind-spillway,
# and kind always gets --name spillway and --kubeconfig, so nothing here can
# reach another cluster or write another kubeconfig. Secrets are made from
# the dev files by reference (--from-file, --from-env-file) and piped to
# apply; no value is printed.
set -euo pipefail

: "${BIN:?BIN is set by the Makefile}"
: "${KUBECONFIG:?KUBECONFIG is set by the Makefile}"
case "$KUBECONFIG" in */.kube/spillway-kind) ;; *) echo "KUBECONFIG must be ~/.kube/spillway-kind, not $KUBECONFIG" >&2; exit 2 ;; esac

CLUSTER=spillway
kubectl() { "$BIN/kubectl" --context kind-spillway "$@"; }
helm() { "$BIN/helm" --kube-context kind-spillway "$@"; }
kind() { "$BIN/kind" "$@"; }

start=$(date +%s)
step() { printf '[kind-up %4ss] %s\n' "$(($(date +%s) - start))" "$*"; }

# The local images (ADR-0075 §D10): the root Dockerfile's targets for the
# host's platform, tagged spillway-<name>:kind and loaded into every node;
# the chart and the smoke Job use them with imagePullPolicy: Never.
images() {
	for t in gateway controlplane loadgen chaos; do
		step "image spillway-$t:kind"
		docker buildx build --quiet --load --target "$t" -t "spillway-$t:kind" . >/dev/null
	done
	step "loading images into kind"
	kind load docker-image --name "$CLUSTER" spillway-gateway:kind spillway-controlplane:kind spillway-loadgen:kind spillway-chaos:kind >/dev/null
}

if [ "${1:-}" = images ]; then
	images
	step "done"
	exit 0
fi

# kind and compose do not fit in Docker Desktop's 8.3 GB together (the
# memory plan of ADR-0075 §D10): refuse while compose's project runs.
if [ -n "$(docker ps -q --filter label=com.docker.compose.project=spillway)" ]; then
	echo "the spillway compose stack is running: run \`docker compose down\` first; kind and compose do not fit together in 8.3 GB" >&2
	exit 1
fi

for f in deploy/certs/kind-gateway.pem deploy/certs/kind-keycloak.pem deploy/stream-certs/kind/controlplane/cert.pem deploy/stream-certs/kind/gateway/cert.pem; do
	[ -f "$f" ] || { echo "missing $f: run make certs" >&2; exit 1; }
done

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
	step "cluster $CLUSTER exists"
	kind export kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG" >/dev/null 2>&1
else
	step "creating cluster $CLUSTER"
	kind create cluster --config deploy/kind/cluster.yaml --name "$CLUSTER" --kubeconfig "$KUBECONFIG" --wait 180s
fi

step "namespaces"
kubectl apply -f deploy/kind/namespaces.yaml >/dev/null

# apply_secret <namespace> <name> <kubectl create secret arguments…>
apply_secret() {
	local ns=$1 name=$2
	shift 2
	kubectl -n "$ns" create secret "$@" "$name" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
}
# apply_configmap <namespace> <name> <kubectl create configmap arguments…>
apply_configmap() {
	local ns=$1 name=$2
	shift 2
	kubectl -n "$ns" create configmap "$name" "$@" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
}

step "secrets and config maps"
for ns in spillway spillway-deps spillway-obs; do
	apply_secret "$ns" dev-secrets generic --from-env-file=deploy/kind/dev-secrets.env
done
apply_secret spillway-deps keycloak-tls tls --cert=deploy/certs/kind-keycloak.pem --key=deploy/certs/kind-keycloak-key.pem
# The chart's (ADR-0075 §D11): the edge certificate and the config stream's.
apply_secret spillway gateway-tls tls --cert=deploy/certs/kind-gateway.pem --key=deploy/certs/kind-gateway-key.pem
for s in gateway controlplane; do
	d=deploy/stream-certs/kind/$s
	apply_secret spillway "$s-stream" generic --from-file=cert.pem="$d/cert.pem" --from-file=key.pem="$d/key.pem" --from-file=ca.pem="$d/ca.pem"
done
# The mkcert CA's certificate (public), which signed Keycloak's and the
# gateway's: the chart and the smoke Job trust it.
apply_configmap spillway mkcert-ca --from-file=ca.pem="$("$BIN/mkcert" -CAROOT)/rootCA.pem"
apply_configmap spillway-deps redis-config --from-file=redis.conf=deploy/compose/redis/redis.conf
apply_configmap spillway-deps keycloak-realm --from-file=realm.json=deploy/compose/keycloak/realm.json
apply_configmap spillway-obs prometheus-config --from-file=prometheus.yml=deploy/kind/config/prometheus.yml
apply_configmap spillway-obs grafana-datasources --from-file=datasources.yaml=deploy/compose/grafana/provisioning/datasources/datasources.yaml
apply_configmap spillway-obs loki-config --from-file=loki.yaml=deploy/compose/loki/loki.yaml
apply_configmap spillway-obs tempo-config --from-file=tempo.yaml=deploy/compose/tempo/tempo.yaml
apply_configmap spillway-obs otel-collector-config --from-file=config.yaml=deploy/compose/otel-collector/config.yaml
# Observability as code (OB-04, OB-05, ADR-0087): the alert rules and the
# dashboards compose mounts. Grafana's file provider picks up a changed
# dashboard by itself; Prometheus reads its rule files at start, so a
# changed rules ConfigMap restarts it once it is applied.
rules=$(kubectl -n spillway-obs create configmap prometheus-rules --from-file=deploy/prometheus/rules/ --dry-run=client -o yaml | kubectl apply -f -)
apply_configmap spillway-obs grafana-dashboards --from-file=deploy/grafana/dashboards/
apply_configmap spillway kind-config --from-file=config.yaml=deploy/kind/config.yaml

step "dependencies and observability"
kubectl apply -f deploy/kind/deps/ -f deploy/kind/obs/ >/dev/null
if [ "$rules" = "configmap/prometheus-rules configured" ]; then
	step "alert rules changed: restarting Prometheus"
	kubectl -n spillway-obs rollout restart deployment/prometheus >/dev/null
fi

# Built while the nodes pull the third-party images.
images

step "waiting for dependencies"
kubectl -n spillway-deps rollout status statefulset/redis --timeout=5m >/dev/null
kubectl -n spillway-deps wait --for=condition=complete job/redis-init --timeout=5m >/dev/null
kubectl -n spillway-deps rollout status statefulset/postgres --timeout=5m >/dev/null
kubectl -n spillway-deps rollout status deployment/upstream --timeout=5m >/dev/null
for d in prometheus grafana loki tempo otel-collector redis-exporter; do
	kubectl -n spillway-obs rollout status "deployment/$d" --timeout=5m >/dev/null
done
kubectl -n spillway-deps rollout status deployment/keycloak --timeout=8m >/dev/null

# The chart (ADR-0075 §D11, ADR-0086). The kubelet's probes come from the
# node network, kind's Docker network, which the NetworkPolicies admit to
# the admin ports (GW-25); its IPv4 subnet is read here.
kubelet_cidr=$(docker network inspect kind -f '{{range .IPAM.Config}}{{.Subnet}} {{end}}' | tr ' ' '\n' | grep -m1 -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+$' || true)
[ -n "$kubelet_cidr" ] || { echo "no IPv4 subnet on the kind Docker network" >&2; exit 1; }
# helm waits for the migrate hook Job (hookOnly) but not for the gateways:
# a managed gateway is not ready before its first config, which the seed
# Job publishes once the control plane is up.
step "helm upgrade --install spillway (kubelet network $kubelet_cidr)"
helm upgrade --install spillway deploy/helm/spillway -n spillway -f deploy/kind/values.yaml \
	--set-json "networkPolicy.kubeletCIDRs=[\"$kubelet_cidr\"]" --timeout 10m >/dev/null
kubectl -n spillway rollout status deployment/spillway-controlplane --timeout=5m >/dev/null
step "publishing deploy/kind/config.yaml (Job kind-seed)"
kubectl -n spillway delete job kind-seed --ignore-not-found --wait=true >/dev/null
kubectl apply -f deploy/kind/seed-job.yaml >/dev/null
# Complete or Failed, whichever comes first (the Job's own deadline is
# 180 s).
state=
deadline=$((SECONDS + 200))
while [ "$SECONDS" -lt "$deadline" ]; do
	state=$(kubectl -n spillway get job kind-seed -o jsonpath='{.status.conditions[?(@.status=="True")].type}' 2>/dev/null)
	case "$state" in *Complete* | *Failed*) break ;; esac
	sleep 1
done
case "$state" in
*Complete*) ;;
*)
	kubectl -n spillway logs job/kind-seed >&2 || true
	echo "the seed Job did not complete (${state:-no state in 200 s})" >&2
	exit 1
	;;
esac
kubectl -n spillway logs job/kind-seed | grep -E '^(PASS|FAIL)' || true
kubectl -n spillway delete job kind-seed --ignore-not-found --wait=false >/dev/null
kubectl -n spillway rollout status deployment/spillway-gateway --timeout=5m >/dev/null

step "done: make kind-smoke checks the cluster"
