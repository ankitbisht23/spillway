# Shared by the M7 tenant load tests (NFR-11 soak.sh, NFR-15 noisy.sh;
# ADR-0074 §D9), sourced from the repository root. They give gateway-1 a
# config and an identity made for the run, as the verify scripts swap
# configs (ADR-0019): one consumer and one static API key per tenant, made
# at run time. Each key is written to its own file in a temp directory that
# only loadgen's container mounts (read-only, running as the caller's user),
# and is never printed or logged; the identity holds only each key's prefix
# and SHA-256 (ADR-0017). On every exit the committed config and identity go
# back and the temp directory is removed.
# shellcheck shell=bash

fails=0
result() { # result PASS|FAIL check measured
	printf '%-4s  %-44s %s\n' "$1" "$2" "$3"
	if [ "$1" != PASS ]; then fails=$((fails + 1)); fi
}
check() { # check <0 = pass> check measured
	if [ "$1" -eq 0 ]; then result PASS "$2" "$3"; else result FAIL "$2" "$3"; fi
}

compose=(docker compose --env-file .env.example)
if [ -f .env.local ]; then compose+=(--env-file .env.local); fi

dir=deploy/compose/gateway
tmp=$(mktemp -d)
cp "$dir/config.yaml" "$tmp/config.yaml.orig"
cp "$dir/identity.yaml" "$tmp/identity.yaml.orig"
swapped=
swap() { # swap <file> <name in dir>: replaces it by rename, so the gateway reloads it
	swapped=1
	cp "$1" "$dir/.$2.swap" && mv -f "$dir/.$2.swap" "$dir/$2"
}
cleanup() {
	if [ -n "$swapped" ]; then
		swap "$tmp/identity.yaml.orig" identity.yaml
		swap "$tmp/config.yaml.orig" config.yaml
	fi
	rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 1' INT TERM

# make_tenants <identity version> <tenant>...: consumer c-<tenant> of tenant
# <tenant> with a random static key in $tmp/keys/<tenant>, and the identity
# in $tmp/identity.yaml.
make_tenants() {
	local version=$1 t key
	shift
	mkdir -p "$tmp/keys"
	chmod 700 "$tmp/keys"
	{
		echo "# Made by test/load/lib.sh for one run; never committed."
		echo "version: $version"
		echo "consumers:"
		for t in "$@"; do echo "  - { name: c-$t, tenant: $t }"; done
		echo "api_keys:"
		for t in "$@"; do
			key=$(openssl rand -hex 24)
			(umask 077 && printf '%s' "$key" >"$tmp/keys/$t")
			printf "  - { prefix: '%s', hash: %s, consumer: c-%s }\n" "${key:0:12}" "$(printf '%s' "$key" | openssl dgst -sha256 | awk '{print $NF}')" "$t"
		done
	} >"$tmp/identity.yaml"
	key=
}

# metrics: gateway-1's admin metrics.
metrics() { curl -sf --max-time 5 http://127.0.0.1:8001/metrics; }

# wait_versions <config> <identity> <seconds>: gateway-1 runs both.
wait_versions() {
	local deadline=$((SECONDS + $3)) m
	while [ "$SECONDS" -lt "$deadline" ]; do
		m=$(metrics || true)
		if grep -qx "gw_config_version $1" <<<"$m" && grep -qx "gw_identity_version $2" <<<"$m"; then return 0; fi
		sleep 0.5
	done
	return 1
}

# tenant_loadgen <loadgen args>...: loadgen in the compose network with the
# key directory mounted at /keys; its JSON result on stdout.
tenant_loadgen() {
	"${compose[@]}" run --rm -T --user "$(id -u):$(id -g)" -v "$tmp/keys:/keys:ro" loadgen "$@"
}
