# Spillway developer entry points (CLAUDE.md, Commands).
# Keep this file compatible with GNU Make 3.81 (macOS) as well as 4.x (CI):
# no .ONESHELL, no `!=`, no $(file ...).

SHELL := /bin/bash
export GOTOOLCHAIN := go1.27.4

BIN := $(CURDIR)/bin
FUZZTIME ?= 5s

# Committed dev defaults first, then local overrides if present (ADR-0004).
COMPOSE := docker compose --env-file .env.example $(if $(wildcard .env.local),--env-file .env.local)

.PHONY: tools hooks certs dev demo demo-reset demo-down test-demo kind-up kind-down kind-smoke kind-rollout kind-images kind-grafana kind-prometheus test test-go test-web test-e2e fuzz test-integration proto api sqlc web-gen web-build lint lint-go lint-web lint-proto lint-api lint-sql lint-helm lint-obs lint-actions release-build release-publish bench bench-gate verify-m0 verify-m0-stack verify-m1 verify-m1-reload verify-m2 verify-m2-shadow verify-m3 verify-m4 verify-m5 verify-m6 verify-m7 accuracy chaos-redis-outage chaos-redis-primary chaos-cp-outage nfr-keys nfr-soak nfr-noisy chaos-redis-latency chaos-gateway-kill chaos

# Host-run processes and tests reach Redis Cluster nodes through this map
# (ADR-0004). Set only here, never in compose or .env files.
REDIS_ADDR_MAP := redis-0:6379=127.0.0.1:7000,redis-1:6379=127.0.0.1:7001,redis-2:6379=127.0.0.1:7002,redis-3:6379=127.0.0.1:7003,redis-4:6379=127.0.0.1:7004,redis-5:6379=127.0.0.1:7005

# Pinned project tools, installed into ./bin (ADR-0002). Each tool has a stamp
# file named after its version, so changing a version reinstalls only that tool.
# `make tool-<name>` installs one tool (CI uses this to skip the slow ones).
TOOL_STAMPS :=
define tool
TOOL_STAMPS += $(BIN)/.$(1)@$(3)
.PHONY: tool-$(1)
tool-$(1): $(BIN)/.$(1)@$(3)
$(BIN)/.$(1)@$(3):
	GOBIN=$(BIN) go install $(2)@$(3)
	@rm -f $(BIN)/.$(1)@* && touch $$@
endef

$(eval $(call tool,sqlc,github.com/sqlc-dev/sqlc/cmd/sqlc,v1.31.1))
$(eval $(call tool,goose,github.com/pressly/goose/v3/cmd/goose,v3.29.0))
$(eval $(call tool,golangci-lint,github.com/golangci/golangci-lint/v2/cmd/golangci-lint,v2.15.0))
$(eval $(call tool,buf,github.com/bufbuild/buf/cmd/buf,v1.75.0))
$(eval $(call tool,gitleaks,github.com/zricethezav/gitleaks/v8,v8.30.1))
$(eval $(call tool,oasdiff,github.com/oasdiff/oasdiff,v1.35.0))
$(eval $(call tool,govulncheck,golang.org/x/vuln/cmd/govulncheck,v1.9.0))
$(eval $(call tool,protoc-gen-go,google.golang.org/protobuf/cmd/protoc-gen-go,v1.36.12))
$(eval $(call tool,protoc-gen-go-grpc,google.golang.org/grpc/cmd/protoc-gen-go-grpc,v1.6.2))
$(eval $(call tool,oapi-codegen,github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen,v2.8.2))
$(eval $(call tool,mkcert,filippo.io/mkcert,v1.4.4))
$(eval $(call tool,benchstat,golang.org/x/perf/cmd/benchstat,v0.0.0-20261009192801-be2c69fb417e))
$(eval $(call tool,kind,sigs.k8s.io/kind,v0.33.0))
# Release tooling (ADR-0076, ADR-0092): each the newest release at least two
# weeks old at the release task.
$(eval $(call tool,actionlint,github.com/rhysd/actionlint/cmd/actionlint,v1.7.12))
$(eval $(call tool,crane,github.com/google/go-containerregistry/cmd/crane,v0.22.1))

# Official release binaries, downloaded into ./bin and checked against the
# SHA-256 pinned here for each platform (ADR-0075 §D10), with the same stamp
# files as above. $(call download-tool,name,version,url,sha256,install):
# install reads the checked download, $(BIN)/.<name>.download, removed after.
HOST_OS := $(shell uname -s | tr '[:upper:]' '[:lower:]')
HOST_ARCH := $(patsubst aarch64,arm64,$(patsubst x86_64,amd64,$(shell uname -m)))
PLATFORM := $(HOST_OS)-$(HOST_ARCH)
define download-tool
TOOL_STAMPS += $(BIN)/.$(1)@$(2)
.PHONY: tool-$(1)
tool-$(1): $(BIN)/.$(1)@$(2)
$(BIN)/.$(1)@$(2):
	@test -n "$(4)" || { echo "$(1) $(2): no pinned checksum for $(PLATFORM)"; exit 1; }
	@mkdir -p $(BIN) && dl=$(BIN)/.$(1).download && trap 'rm -f "$$$$dl"' EXIT && \
		curl -fsSL --retry 3 -o "$$$$dl" "$(3)" && \
		got=$$$$(shasum -a 256 "$$$$dl" | cut -d ' ' -f 1) && \
		{ [ "$$$$got" = "$(4)" ] || { echo "$(1) $(2): checksum $$$$got, want $(4)"; exit 1; }; } && \
		$(5) && chmod 0755 $(BIN)/$(1)
	@rm -f $(BIN)/.$(1)@* && touch $$@
	@echo "installed $(1) $(2) for $(PLATFORM)"
endef

# kubectl: the newest patch of the kind node image's minor (1.37) released
# at least two weeks before the kind task (ADR-0075 §D10).
KUBECTL_VERSION := v1.37.2
KUBECTL_SHA256_darwin-arm64 := 55701af0d51815ee335f240e48acf0959c3c23017ed3fa816e880c4a92bae581
KUBECTL_SHA256_darwin-amd64 := 88379aff5c8111de6be0a394adffba2819e44cde269f3ab23cef76de1a48eaa8
KUBECTL_SHA256_linux-amd64 := c04487523e7aa7d1493f7808174c899db93d7a9cf57531420cd76b9bd415f8f4
KUBECTL_SHA256_linux-arm64 := fcc16f63186659d9722014b7e02d5fdd2d4e669cb8c0ab66365d7d3b8ba03509
$(eval $(call download-tool,kubectl,$(KUBECTL_VERSION),https://dl.k8s.io/release/$(KUBECTL_VERSION)/bin/$(HOST_OS)/$(HOST_ARCH)/kubectl,$(KUBECTL_SHA256_$(PLATFORM)),cp "$$$$dl" $(BIN)/kubectl))

HELM_VERSION := v4.3.1
HELM_SHA256_darwin-arm64 := 6b4e61c8cd27d99e5a801e63ff25dd5c70b36875c2cd659d3e9ce6b76fc23198
HELM_SHA256_darwin-amd64 := 34bec4eba761ba5c16d1965ee6b8f5b0abe5993974a802274e2a4f0418893d89
HELM_SHA256_linux-amd64 := 0403a88ae20913d11b10a2467b50087721fea7581736da6b9d3992f5357fe54b
HELM_SHA256_linux-arm64 := 8cf8f66ecbc252ac8f7f06cca68aba88508ddfde3a6a34feb1678ad1694b4f88
$(eval $(call download-tool,helm,$(HELM_VERSION),https://get.helm.sh/helm-$(HELM_VERSION)-$(PLATFORM).tar.gz,$(HELM_SHA256_$(PLATFORM)),tar -xzOf "$$$$dl" $(PLATFORM)/helm > $(BIN)/helm))

# Syft (SBOMs) and cosign (keyless signing) for the release (ADR-0076,
# ADR-0092), checked against the sums their releases publish.
SYFT_VERSION := v1.57.1
SYFT_SHA256_darwin-arm64 := e0a74a5d2688d1254b4e17b71f068a83215e3625f846265bf6614c7ba06f3a96
SYFT_SHA256_darwin-amd64 := 2355758c1142982a67b309604c5d75effdbe638a57c771979ea8fd1a16656abc
SYFT_SHA256_linux-amd64 := bf52519eb57f675d47c07d43213cb6dff6449f3d6d1147ff6701ed91f44eef0c
SYFT_SHA256_linux-arm64 := aa3d3e07344f590bcce8d31043b752daa76c8a56c2aa34ceeee63c6168152582
$(eval $(call download-tool,syft,$(SYFT_VERSION),https://github.com/anchore/syft/releases/download/$(SYFT_VERSION)/syft_$(SYFT_VERSION:v%=%)_$(HOST_OS)_$(HOST_ARCH).tar.gz,$(SYFT_SHA256_$(PLATFORM)),tar -xzOf "$$$$dl" syft > $(BIN)/syft))

COSIGN_VERSION := v3.1.4
COSIGN_SHA256_darwin-arm64 := 9f9b8cd557cf2e5b3df3373a00baa005a86b02391e3e06dd0ce37b3a1533d3a4
COSIGN_SHA256_darwin-amd64 := 9d1e4abe2b6be279ed0d2ff706fde1ccd738747c834e3e6d44039c238867334f
COSIGN_SHA256_linux-amd64 := 424e2c97d0b45fd7a6b608a87f090f35633f4c23ece5f092d48bba11aaf4bbfa
COSIGN_SHA256_linux-arm64 := ade83b7aac62e131cacfe96d958948b2150cc9efec46a570a2649b8e27dedad2
$(eval $(call download-tool,cosign,$(COSIGN_VERSION),https://github.com/sigstore/cosign/releases/download/$(COSIGN_VERSION)/cosign-$(PLATFORM),$(COSIGN_SHA256_$(PLATFORM)),cp "$$$$dl" $(BIN)/cosign))

tools: $(TOOL_STAMPS)

# Pre-commit hook: gitleaks scans staged changes with .gitleaks.toml.
hooks: tool-gitleaks
	@hook="$$(git rev-parse --git-path hooks)/pre-commit"; \
	printf '%s\n' '#!/bin/sh' '# Installed by make hooks: block commits that contain secrets.' \
		'exec "$$(git rev-parse --show-toplevel)/bin/gitleaks" git --pre-commit --staged --redact --no-banner' > "$$hook"; \
	chmod +x "$$hook"; echo "installed $$hook"

# Dev TLS certificate for the gateways (GW-04, SEC-01), minted once into the
# gitignored deploy/certs. To mint a new one, delete the two files (not the
# directory, which the containers mount) and run this again; running
# gateways load it within a second. mkcert creates its local CA on first use;
# `mkcert -install` (docs/setup.md) only makes browsers trust it. The key is
# made readable to all, because the gateway containers run as uid 65532 and,
# on Linux, a bind mount keeps the host's owner and mode (ADR-0012).
#
# Then the config stream's mTLS certificates (SEC-02, D7, ADR-0040,
# ADR-0047), in the gitignored deploy/stream-certs: a second mkcert CA in
# ca/, never installed in a trust store and never mounted into a container; a
# server certificate for the control plane (controlplane, localhost,
# 127.0.0.1); and a client certificate per gateway replica whose only name is
# the replica's. Each other directory holds cert.pem, key.pem and ca.pem (the
# stream CA's certificate) and is mounted read-only at /etc/spillway-stream by
# its own container only. To replace the stream CA, which revokes every
# stream certificate (the only revocation in M5), delete deploy/stream-certs/ca
# (not the other directories, which containers mount) and run this again:
# every certificate is reissued, and running processes load them within a
# second. Keys are readable to all, as above.
STREAM := deploy/stream-certs
STREAM_CA := $(STREAM)/ca/rootCA.pem
STREAM_GATEWAYS := gateway-1 gateway-2 gateway-3 gateway-4 gateway-5

# The kind cluster's certificates (ADR-0075 §D10), minted the same way: the
# gateway's edge and Keycloak's HTTPS from the mkcert CA, the config stream's
# control-plane server and its one fleet client certificate, spillway-gateway
# (P10), from the stream CA. make kind-up turns them into Secrets.
KIND_CERTS := deploy/certs/kind-gateway.pem deploy/certs/kind-keycloak.pem $(STREAM)/kind/controlplane/cert.pem $(STREAM)/kind/gateway/cert.pem

certs: deploy/certs/gateway.pem $(STREAM_CA) $(STREAM)/controlplane/cert.pem $(STREAM_GATEWAYS:%=$(STREAM)/%/cert.pem) $(KIND_CERTS)

deploy/certs/gateway.pem: | tool-mkcert
	@mkdir -p deploy/certs
	$(BIN)/mkcert -cert-file $@ -key-file deploy/certs/gateway-key.pem \
		localhost 127.0.0.1 ::1 gateway-1 gateway-2 gateway-3 gateway-4 gateway-5
	chmod 644 deploy/certs/gateway-key.pem

# $(call stream-cert,directory,mkcert flags and names) issues one stream
# certificate into directory.
define stream-cert
	@mkdir -p $(1)
	CAROOT=$(STREAM)/ca $(BIN)/mkcert -cert-file $(1)/cert.pem -key-file $(1)/key.pem $(2)
	cp $(STREAM_CA) $(1)/ca.pem
	chmod 644 $(1)/key.pem
endef

# mkcert makes a CA when it first issues a certificate, so the CA's rule
# issues the control plane's; every certificate is older than a new CA, so
# each is reissued after it.
$(STREAM_CA): | tool-mkcert
	@mkdir -p $(STREAM)/ca
	$(call stream-cert,$(STREAM)/controlplane,controlplane localhost 127.0.0.1)

$(STREAM)/controlplane/cert.pem: $(STREAM_CA)
	$(call stream-cert,$(@D),controlplane localhost 127.0.0.1)

$(STREAM)/gateway-%/cert.pem: $(STREAM_CA)
	$(call stream-cert,$(@D),-client gateway-$*)

deploy/certs/kind-gateway.pem: | tool-mkcert
	@mkdir -p deploy/certs
	$(BIN)/mkcert -cert-file $@ -key-file deploy/certs/kind-gateway-key.pem \
		localhost 127.0.0.1 spillway-gateway spillway-gateway.spillway.svc spillway-gateway.spillway.svc.cluster.local
	chmod 644 deploy/certs/kind-gateway-key.pem

deploy/certs/kind-keycloak.pem: | tool-mkcert
	@mkdir -p deploy/certs
	$(BIN)/mkcert -cert-file $@ -key-file deploy/certs/kind-keycloak-key.pem \
		keycloak keycloak.spillway-deps keycloak.spillway-deps.svc keycloak.spillway-deps.svc.cluster.local
	chmod 644 deploy/certs/kind-keycloak-key.pem

$(STREAM)/kind/controlplane/cert.pem: $(STREAM_CA)
	$(call stream-cert,$(@D),spillway-controlplane spillway-controlplane.spillway.svc spillway-controlplane.spillway.svc.cluster.local)

$(STREAM)/kind/gateway/cert.pem: $(STREAM_CA)
	$(call stream-cert,$(@D),-client spillway-gateway)

# Boots the dev stack and returns once every service is healthy and every
# one-shot service has exited 0. Any demo container (label spillway.demo)
# goes first, volumes kept, so nothing after it runs on the demo's
# configuration (ADR-0094 §2).
dev: certs
	@deploy/demo/demo.sh clean
	$(COMPOSE) up -d --build --wait --wait-timeout 300

# The demo (SC-01, ADR-0094): the dev stack plus deploy/compose/demo.compose.yml,
# with the seeded database spillway_demo, gateway-1 … gateway-3 in managed
# mode through toxiproxy's demo proxies, and DEMO_MODE=true. Running it
# again recreates only what changed. make demo-reset drops and re-seeds
# spillway_demo and restarts the demo gateways, without touching the dev
# database or Redis; make demo-down deletes the demo proxies, removes every
# demo container and runs make dev (deploy/demo/demo.sh).
demo: certs
	@deploy/demo/demo.sh up

demo-reset:
	@deploy/demo/demo.sh reset

demo-down:
	@deploy/demo/demo.sh down
	@$(MAKE) --no-print-directory dev
	@deploy/demo/demo.sh down-check

# The demo's tests (SC-01, ADR-0094, test/demo, build tag demo): make demo
# prints no secret, token or key; make demo-reset puts a modified demo back
# to the seed and leaves the dev database as it was. Then make demo-down,
# whatever the result. About 3 minutes on a built stack.
test-demo:
	@go test -tags demo -count=1 -timeout 30m -v ./test/demo/; rc=$$?; \
		$(MAKE) --no-print-directory demo-down; exit $$rc

# The kind cluster `spillway` (ADR-0075 §D10, docs/setup.md Step 6) lives in
# its own kubeconfig: `:=`, so a KUBECONFIG inherited from the shell cannot
# send kind into another file. Every kubectl here and in the scripts passes
# --context kind-spillway, every helm --kube-context kind-spillway, and kind
# gets --name spillway --kubeconfig; ~/.kube/config is never touched.
export KUBECONFIG := $(HOME)/.kube/spillway-kind
KIND_TOOLS := tool-kind tool-kubectl tool-helm tool-mkcert
KUBECTL := $(BIN)/kubectl --context kind-spillway

# Creates the cluster if absent, its Secrets and ConfigMaps (from the dev
# files, by reference), the dependencies and observability, the local
# images, and waits until all are ready; then the Helm chart, the published
# kind config (deploy/kind/config.yaml) and the gateways (ADR-0086). Refuses
# while the compose stack runs (memory).
kind-up: certs $(KIND_TOOLS)
	@BIN=$(BIN) deploy/kind/up.sh

# Deletes the cluster and its kubeconfig file; compose stays down (make dev
# brings it back).
kind-down: tool-kind
	$(BIN)/kind delete cluster --name spillway --kubeconfig $(KUBECONFIG)
	rm -f $(KUBECONFIG)

# The cluster's checks, PASS/FAIL, at most 3 minutes (test/kind/smoke.sh).
kind-smoke: tool-kubectl tool-mkcert
	@BIN=$(BIN) test/kind/smoke.sh

# NFR-14 on kind: zero failed requests through a rolling restart of the
# gateways under 300 rps (test/kind/rollout.sh, ADR-0086); about 5 minutes.
kind-rollout: tool-kubectl
	@BIN=$(BIN) test/kind/rollout.sh

# Rebuilds the local images and loads them into the running cluster.
kind-images: tool-kind
	@BIN=$(BIN) deploy/kind/up.sh images

# Grafana and Prometheus of the running cluster on 127.0.0.1 (docs/ports.md);
# each runs until Ctrl-C.
kind-grafana: tool-kubectl
	$(KUBECTL) -n spillway-obs port-forward --address 127.0.0.1 service/grafana 13000:3000

kind-prometheus: tool-kubectl
	$(KUBECTL) -n spillway-obs port-forward --address 127.0.0.1 service/prometheus 19091:9090

# Every unit test: Go's and the admin UI's (ADR-0057). No Docker. Both need
# `pnpm install` once after cloning; CI runs them in separate jobs.
test: test-go test-web

# Go unit and property tests, then a short run of every fuzz target.
test-go:
	go test -race -count=1 ./...
	@$(MAKE) --no-print-directory fuzz

# The admin UI's Vitest suite with its coverage thresholds (ADR-0061).
test-web:
	pnpm -C web test

# The admin UI's Playwright flows (ADR-0061): Chromium against the compose
# stack's embedded UI and the real Keycloak. Needs Chromium once:
# pnpm -C web exec playwright install chromium. After a passing run the
# report and results are removed; after a failing one they stay for
# diagnosis (both gitignored).
test-e2e: dev
	pnpm -C web e2e
	rm -rf web/playwright-report web/test-results

# Runs each Fuzz* target for FUZZTIME (go test -fuzz takes one target per run).
fuzz:
	@set -eo pipefail; \
	go test -list '^Fuzz' ./... | awk '/^Fuzz/ {f[n++]=$$1; next} /^ok/ {for (i = 0; i < n; i++) print $$2, f[i]; n = 0}' | \
	while read -r pkg fn; do \
		echo "fuzz $$pkg $$fn ($(FUZZTIME))"; \
		go test -run '^$$' -fuzz "^$$fn$$" -fuzztime $(FUZZTIME) "$$pkg"; \
	done

# Tests against the compose stack (build tag integration); boots it first.
test-integration: dev
	SPILLWAY_REDIS_ADDR_MAP=$(REDIS_ADDR_MAP) go test -tags integration -count=1 ./test/integration/...

# Go code from api/proto into internal/gen (CP-21); the generated code is
# committed, so builds need no protoc or buf.
proto: tool-buf tool-protoc-gen-go tool-protoc-gen-go-grpc
	PATH="$(BIN):$$PATH" $(BIN)/buf generate

# REST server stubs from api/openapi.yaml into internal/gen/apiv1 (CP-10,
# ADR-0036); committed, like internal/gen/configpb.
api: tool-oapi-codegen
	$(BIN)/oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml

# The admin UI's typed client from api/openapi.yaml into web/src/api
# (UI-05, ADR-0057); committed, like internal/gen/apiv1.
web-gen:
	pnpm -C web gen

# The admin UI's production build into internal/controlplane/webui/dist
# (gitignored), which `go build -tags webui ./cmd/controlplane` embeds
# (UI-09, ADR-0057). Images always build it themselves.
web-build:
	pnpm -C web build

# The control plane's sqlc queries into internal/controlplane/store/db
# (ADR-0036); committed.
sqlc: tool-sqlc
	cd internal/controlplane/store && $(BIN)/sqlc generate

lint: lint-go lint-web lint-proto lint-api lint-sql lint-helm lint-obs lint-actions

# The Helm chart (ADR-0075 §D11, ADR-0086): helm lint --strict and helm
# template with the chart's values.yaml under deploy/kind/values.yaml, then
# the chart's Go tests on the rendered manifests (test/kind/helm_test.go)
# with the pinned helm, so they cannot skip. No cluster needed.
lint-helm: tool-helm
	$(BIN)/helm --kube-context kind-spillway lint --strict deploy/helm/spillway -f deploy/kind/values.yaml --set 'networkPolicy.kubeletCIDRs={172.18.0.0/16}'
	$(BIN)/helm --kube-context kind-spillway template spillway deploy/helm/spillway -n spillway -f deploy/kind/values.yaml --set 'networkPolicy.kubeletCIDRs={172.18.0.0/16}' >/dev/null
	SPILLWAY_HELM=$(BIN)/helm go test -count=1 -run '^TestChart' ./test/kind/

# Observability as code (OB-04, OB-05, OB-08; ADR-0075, ADR-0087): promtool
# check rules and the rules' unit tests (test/obs/rules-test.yaml) with the
# promtool of compose's pinned Prometheus image, then the dashboards',
# rules' and runbooks' Go tests (test/obs). Needs Docker, no stack; CI's
# integration job runs it.
PROMETHEUS_IMAGE := prom/prometheus:v3.15.1@sha256:fa3302279710be2f21a8301ec17a8b4f2301fd829bd113839b3b6b02b6f191a2
PROMTOOL := docker run --rm --entrypoint promtool -v "$(CURDIR):/src:ro" -w /src $(PROMETHEUS_IMAGE)
lint-obs:
	$(PROMTOOL) check rules deploy/prometheus/rules/*.yaml
	$(PROMTOOL) test rules test/obs/rules-test.yaml
	go test -count=1 ./test/obs/

# The workflows (ADR-0092): actionlint with its shellcheck and pyflakes
# passes off, so a machine with shellcheck installed (CI's runners) and one
# without give the same result.
lint-actions: tool-actionlint
	$(BIN)/actionlint -shellcheck= -pyflakes=

# Then the webui build tag's files (ADR-0057) against the real build, or an
# empty stand-in when there is none, removed after.
WEBUI_DIST := internal/controlplane/webui/dist
lint-go: tool-golangci-lint
	$(BIN)/golangci-lint run ./...
	@if [ -d $(WEBUI_DIST) ]; then $(BIN)/golangci-lint run --build-tags webui ./internal/controlplane/webui/; else \
		trap 'rm -rf $(WEBUI_DIST)' EXIT && mkdir $(WEBUI_DIST) && touch $(WEBUI_DIST)/index.html && \
		$(BIN)/golangci-lint run --build-tags webui ./internal/controlplane/webui/; fi
	go mod tidy -diff

# ESLint and both typechecks, then web/src/api matches what
# api/openapi.yaml generates (ADR-0057).
lint-web:
	pnpm -C web lint
	pnpm -C web gen:check

# buf lint; internal/gen matches what api/proto generates; no breaking change
# against origin/main, skipped while main has no buf.yaml (the first M1 PR).
lint-proto: tool-buf tool-protoc-gen-go tool-protoc-gen-go-grpc
	$(BIN)/buf lint
	@tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && \
	PATH="$(BIN):$$PATH" $(BIN)/buf generate -o "$$tmp" && \
	diff -r "$$tmp/internal/gen/configpb" internal/gen/configpb || { echo "internal/gen/configpb is stale: run make proto"; exit 1; }
	@if git cat-file -e origin/main:buf.yaml 2>/dev/null; then \
		$(BIN)/buf breaking --against '.git#ref=origin/main'; \
	else echo "buf breaking: origin/main has no buf.yaml yet, skipped"; fi

# internal/gen/apiv1 matches what api/openapi.yaml generates; no breaking
# change against origin/main (oasdiff, CP-10), skipped while main has no spec,
# except the user-approved ones that api/oasdiff-ignore.txt lists.
lint-api: tool-oapi-codegen tool-oasdiff
	@cfg=$$(mktemp) && out=$$(mktemp) && trap 'rm -f "$$cfg" "$$out"' EXIT && \
	sed "s#^output: .*#output: $$out#" api/oapi-codegen.yaml > "$$cfg" && \
	$(BIN)/oapi-codegen -config "$$cfg" api/openapi.yaml && \
	cmp -s "$$out" internal/gen/apiv1/apiv1.gen.go || { echo "internal/gen/apiv1 is stale: run make api"; exit 1; }
	@if git cat-file -e origin/main:api/openapi.yaml 2>/dev/null; then \
		base=$$(mktemp) && trap 'rm -f "$$base"' EXIT && git show origin/main:api/openapi.yaml > "$$base" && \
		$(BIN)/oasdiff breaking --fail-on ERR --err-ignore api/oasdiff-ignore.txt "$$base" api/openapi.yaml; \
	else echo "oasdiff breaking: origin/main has no api/openapi.yaml yet, skipped"; fi

# internal/controlplane/store/db matches what the queries and migrations
# generate.
lint-sql: tool-sqlc
	@cd internal/controlplane/store && $(BIN)/sqlc diff >/dev/null || { echo "internal/controlplane/store/db is stale: run make sqlc"; exit 1; }

bench:
	go test -run '^$$' -bench . -benchmem ./...

# The NFR-05 gate (ADR-0073 §D8) on a local bench.sh result, against a copy
# of origin/bench-results in bin/bench-results and the newest local v* tag:
# prints the verdict and the summary bench.yml writes, and changes no file.
#   make bench-gate FILE=result.json
bench-gate:
	@test -n "$(FILE)" || { echo "usage: make bench-gate FILE=<result.json>"; exit 2; }
	@git fetch -q origin bench-results
	@rm -rf bin/bench-results && mkdir -p bin/bench-results
	@git archive origin/bench-results results | tar -x -C bin/bench-results
	@tag=$$(git tag -l 'v*' | sort -V | tail -n 1); \
		go run ./cmd/benchgate -results bin/bench-results -candidate "$(FILE)" \
			$$([ -z "$$tag" ] || echo "-release-tag $$tag -release-commit $$(git rev-parse "$$tag^{commit}")")

# Checks that measure latency run the gateways at the benchmarks' fixed 1%
# trace head ratio (OB-02, D6, ADR-0030): their dev prerequisite recreates
# the gateways with it. make dev alone, and make verify-m4, recreate them at
# compose's default of 1.
verify-m1 verify-m2 verify-m3 accuracy chaos-redis-outage chaos-redis-primary nfr-soak nfr-noisy chaos-redis-latency chaos-gateway-kill: export SPILLWAY_GATEWAY_TRACE_SAMPLE_RATIO = 0.01

# M0 exit checks (docs/design.md): the dev stack, then CI and the main ruleset
# on GitHub. CI runs only the stack part, since it can't check its own result.
verify-m0: dev
	@SPILLWAY_REDIS_ADDR_MAP=$(REDIS_ADDR_MAP) test/verify/m0.sh all

verify-m0-stack: dev
	@SPILLWAY_REDIS_ADDR_MAP=$(REDIS_ADDR_MAP) test/verify/m0.sh stack

# M1 exit checks (docs/design.md, ADR-0015): config reload under load on the
# dev stack, then the NFR-01 baseline on origin/bench-results. CI runs only the
# reload part; the baseline exists once bench.yml has run on main.
verify-m1: dev
	@test/verify/m1.sh all

verify-m1-reload: dev
	@test/verify/m1.sh reload

# M2 exit checks (docs/design.md, ADR-0019): the property tests, shadow mode
# never rejecting and NFR-03's added latency on the dev stack. CI runs only the
# shadow part; the latency pairs (U4, ADR-0021) take 10 minutes and need a
# quiet machine.
verify-m2: dev
	@test/verify/m2.sh all

verify-m2-shadow: dev
	@test/verify/m2.sh shadow

# M3 exit checks (docs/design.md, ADR-0021): the accuracy harness at 3
# replicas (NFR-06), both Redis chaos tests (NFR-13) and NFR-04's added
# latency with a Redis check on the dev stack, about 15 minutes. CI runs none
# of it: the chaos tests take the shared cluster down and the latency pairs
# need a quiet machine.
verify-m3: dev
	@test/verify/m3.sh

# M4 exit check (docs/design.md, ADR-0031): one of three endpoints of an
# upstream fails under 500 rps, first answering 503, then refusing
# connections; >= 99% of requests succeed within 10 s of the failure,
# retries stay within budget and Tempo shows each attempt, about 3 minutes.
# The gateways run at compose's trace ratio of 1. CI runs none of it: it
# waits on Tempo and needs a quiet machine.
verify-m4: dev
	@test/verify/m4.sh

# M5 exit checks (docs/design.md, ADR-0042): gateway-1 … gateway-5 in
# managed mode; (a) 60 publishes each reach all five in < 5 s p99 (NFR-08),
# (b) 20 revoked keys each rejected on all five in < 5 s (NFR-09), (c) a
# 10-minute control-plane and Postgres outage under 500 rps with gateway-3
# restarted from its snapshot: zero failed requests, config frozen, sync
# age growing, recovery within 60 s (NFR-12). About 18 minutes; the
# gateways run at compose's trace ratio of 1 and go back to static mode
# after. CI runs none of it: it takes the control plane down for 10 minutes.
verify-m5: dev
	@test/verify/m5.sh all

# M6 exit check (docs/design.md, ADR-0061): gateway-1 … gateway-5 in
# managed mode serve test/verify/m6.yaml while chaos m6 probe drives them;
# Playwright (web/e2e/m6-exit.spec.ts) takes a new policy shadow →
# reviewed → enforced → rolled back in the UI and times 23 publishes from
# the click to all five applied (NFR-08); the probe checks the gateways at
# each stage, and zero console errors and zero tokens in storage, cookies
# or URLs are PASS lines. About 3 minutes; needs pnpm install and
# Chromium (as make test-e2e); the gateways go back to static mode after.
# CI runs none of it: its e2e job runs the flows against static gateways.
verify-m6: dev
	@test/verify/m6.sh

# M7 exit (docs/design.md, ADR-0074, ADR-0088): every NFR row NFR-01 –
# NFR-15 and the chaos suite on one commit, PASS/FAIL per row, about 2¼
# hours. NFR-01 and NFR-05 come from a gated bench.yml run it dispatches on
# HEAD (pushed, clean tree, gh as the repository's owner); NFR-02 reads the
# newest scaling results and is an open known limit (ADR-0084 §4), printed
# but not counted; the compose checks run through their make targets; kind
# runs NFR-14's rolling restart with compose stopped, then make dev brings
# compose back. Results and logs go to test/verify/out/. README=1 rewrites
# the README's verify-m7 block from the run; PARTS="10 11 15" (or bench,
# compose, kind, chaos) runs a subset, which is never the exit evidence.
verify-m7:
	@README=$(README) test/verify/m7.sh $(PARTS)

# Accuracy harness v1 (NFR-06, NFR-07, ADR-0021, ADR-0069): gateway-1 …
# gateway-$(REPLICAS) (1 to 5) run test/accuracy/gateway.yaml, or with
# MODE=batched test/accuracy/batched.yaml (lease_k 10, over-admission up to
# REPLICAS x 10 allowed), and cmd/accuracy drives them from the compose
# network for about 3 minutes. Prints its JSON report.
REPLICAS ?= 3
MODE ?= strict
accuracy: dev
	@test/accuracy/run.sh $(REPLICAS) $(MODE)

# Redis chaos tests (NFR-13, ADR-0021): all six nodes paused under load, and
# the primary owning slot 0 killed under load. Each prints its checks with
# PASS/FAIL and always brings the cluster back. CI runs neither: each takes
# the shared cluster down for tens of seconds.
chaos-redis-outage: dev
	@test/chaos/redis-outage.sh

chaos-redis-primary: dev
	@test/chaos/redis-primary.sh

# NFR-12's control-plane outage alone, rule (c) of make verify-m5
# (ADR-0042): about 14 minutes with its setup.
chaos-cp-outage: dev
	@test/verify/m5.sh outage

# Redis latency +50 ms (ADR-0074 §D9): gateway-4 reaches Redis through
# toxiproxy and a 50 ms toxic sits on every node for 20 s of a 60 s run;
# each policy follows its failure mode within the budget, and Redis decides
# again within 5 s of the toxic's removal. About 2 minutes.
chaos-redis-latency: dev
	@test/chaos/redis-latency.sh

# Gateway kill -9 under load (ADR-0074 §D9): gateway-2 is killed while
# 300 rps go round robin to gateway-1 … gateway-3; only its in-flight
# requests may fail. About 1 minute.
chaos-gateway-kill: dev
	@test/chaos/gateway-kill.sh

# The compose chaos suite (docs/design.md, Chaos scenarios; ADR-0074 §D9),
# one scenario after another, each through its own target so it runs with
# that target's environment; a failing scenario does not stop the rest, and
# the suite fails if any did. Rolling restart under load is NFR-14's on kind
# (ADR-0075). About 40 minutes.
CHAOS_SUITE := chaos-redis-latency chaos-redis-outage chaos-redis-primary chaos-cp-outage chaos-gateway-kill verify-m4 nfr-noisy
chaos:
	@failed=""; for t in $(CHAOS_SUITE); do \
		$(MAKE) --no-print-directory $$t || failed="$$failed $$t"; \
	done; \
	echo; if [ -z "$$failed" ]; then echo "chaos suite: PASS ($(words $(CHAOS_SUITE)) scenarios)"; \
	else echo "chaos suite: FAIL:$$failed"; exit 1; fi

# NFR-10 (ADR-0074 §D9): 1,000,000 GCRA, then 1,000,000 sliding-window
# limiter keys made with the limiter script on the dev stack's Redis
# Cluster; <= 200 B of Redis memory per key (Σ Δused_memory ÷ Σ Δ DBSIZE of
# the primaries) for each kind. Under a minute here (budget 8); the keys are
# removed after.
nfr-keys: dev
	@test/load/keys.sh

# NFR-11 (ADR-0074 §D9): a 2,000-rps soak of gateway-1 by four tenants with
# keys made at run time, 30 s + 10 min; gateway-caused 5xx (reason not
# shed, circuit_open or fail_closed) <= 0.1% of requests, from gateway-1's
# /metrics deltas. About 12 minutes; config and identity go back after.
nfr-soak: dev
	@test/load/soak.sh

# NFR-15 (ADR-0074 §D9): quiet tenants at 100 rps each while a noisy one
# sends 2,000 rps (10x its limit) every other 10 s, 30 s + 10 min; each
# quiet tenant's median p99(on) / p99(off) over the 30 slot pairs < 1.10.
# About 12 minutes; config and identity go back after.
nfr-noisy: dev
	@test/load/noisy.sh

# Release (ADR-0076, ADR-0092). release-build builds the gateway and control
# plane images for linux/amd64 and linux/arm64 into dist/ as OCI layouts,
# with an SPDX SBOM per image and platform, the source SBOM, the release
# notes and SHA256SUMS; it pushes nothing (CI's image job and release.yml's
# build job run it, and so can you). release-publish pushes those layouts to
# ghcr, signs and attests them; only release.yml's publish job runs it.
RELEASE_VERSION ?= 0.0.0-dev
release-build: tool-syft
	@BIN=$(BIN) VERSION=$(RELEASE_VERSION) deploy/release/build.sh

release-publish: tool-crane tool-cosign
	@BIN=$(BIN) VERSION=$(RELEASE_VERSION) deploy/release/publish.sh
