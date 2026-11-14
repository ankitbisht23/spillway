# Spillway developer entry points (CLAUDE.md, Commands).
# Keep this file compatible with GNU Make 3.81 (macOS) as well as 4.x (CI):
# no .ONESHELL, no `!=`, no $(file ...).

SHELL := /bin/bash
export GOTOOLCHAIN := go1.27.4

BIN := $(CURDIR)/bin
FUZZTIME ?= 5s

# Committed dev defaults first, then local overrides if present (ADR-0004).
COMPOSE := docker compose --env-file .env.example $(if $(wildcard .env.local),--env-file .env.local)

.PHONY: tools hooks certs dev test fuzz test-integration proto api sqlc lint lint-go lint-web lint-proto lint-api lint-sql bench verify-m0 verify-m0-stack verify-m1 verify-m1-reload verify-m2 verify-m2-shadow verify-m3 verify-m4 accuracy chaos-redis-outage chaos-redis-primary

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

certs: deploy/certs/gateway.pem $(STREAM_CA) $(STREAM)/controlplane/cert.pem $(STREAM_GATEWAYS:%=$(STREAM)/%/cert.pem)

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

# Boots the dev stack and returns once every service is healthy and every
# one-shot service has exited 0.
dev: certs
	$(COMPOSE) up -d --build --wait --wait-timeout 300

# Unit and property tests, then a short run of every fuzz target. No Docker.
test:
	go test -race -count=1 ./...
	@$(MAKE) --no-print-directory fuzz

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

# The control plane's sqlc queries into internal/controlplane/store/db
# (ADR-0036); committed.
sqlc: tool-sqlc
	cd internal/controlplane/store && $(BIN)/sqlc generate

lint: lint-go lint-web lint-proto lint-api lint-sql

lint-go: tool-golangci-lint
	$(BIN)/golangci-lint run ./...
	go mod tidy -diff

lint-web:
	pnpm -C web lint

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

# Checks that measure latency run the gateways at the benchmarks' fixed 1%
# trace head ratio (OB-02, D6, ADR-0030): their dev prerequisite recreates
# the gateways with it. make dev alone, and make verify-m4, recreate them at
# compose's default of 1.
verify-m1 verify-m2 verify-m3 accuracy chaos-redis-outage chaos-redis-primary: export SPILLWAY_GATEWAY_TRACE_SAMPLE_RATIO = 0.01

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

# Accuracy harness v1 (NFR-06, ADR-0021): gateway-1 … gateway-$(REPLICAS)
# (1 to 5) run test/accuracy/gateway.yaml and cmd/accuracy drives them from
# the compose network for about 3 minutes. Prints its JSON report.
REPLICAS ?= 3
accuracy: dev
	@test/accuracy/run.sh $(REPLICAS)

# Redis chaos tests (NFR-13, ADR-0021): all six nodes paused under load, and
# the primary owning slot 0 killed under load. Each prints its checks with
# PASS/FAIL and always brings the cluster back. CI runs neither: each takes
# the shared cluster down for tens of seconds.
chaos-redis-outage: dev
	@test/chaos/redis-outage.sh

chaos-redis-primary: dev
	@test/chaos/redis-primary.sh
