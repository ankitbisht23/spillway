#!/usr/bin/env bash
# make kind-up and make kind-images (ADR-0075 §D10), run from the repository
# root with BIN (the pinned kind and kubectl) and KUBECONFIG
# (~/.kube/spillway-kind) from the Makefile.
#
#   up.sh          the cluster (created if absent), Secrets and ConfigMaps,
#                  dependencies and observability, images; waits until every
#                  dependency is ready
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

step "dependencies and observability"
kubectl apply -f deploy/kind/deps/ -f deploy/kind/obs/ >/dev/null

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

step "done: dependencies ready (the chart comes with the helm task); make kind-smoke checks them"
