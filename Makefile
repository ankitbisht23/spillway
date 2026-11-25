# Spillway developer entry points (CLAUDE.md, Commands).
# Keep this file compatible with GNU Make 3.81 (macOS) as well as 4.x (CI):
# no .ONESHELL, no `!=`, no $(file ...).

SHELL := /bin/bash
export GOTOOLCHAIN := go1.27.4

BIN := $(CURDIR)/bin
FUZZTIME ?= 5s

# Committed dev defaults first, then local overrides if present (ADR-0004).
COMPOSE := docker compose --env-file .env.example $(if $(wildcard .env.local),--env-file .env.local)

.PHONY: tools hooks certs dev test test-go test-web test-e2e fuzz test-integration proto api sqlc web-gen web-build lint lint-go lint-web lint-proto lint-api lint-sql bench bench-gate verify-m0 verify-m0-stack verify-m1 verify-m1-reload verify-m2 verify-m2-shadow verify-m3 verify-m4 verify-m5 verify-m6 accuracy chaos-redis-outage chaos-redis-primary chaos-cp-outage nfr-keys nfr-soak nfr-noisy chaos-redis-latency chaos-gateway-kill chaos

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

lint: lint-go lint-web lint-proto lint-api lint-sql

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
