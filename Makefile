# Spillway developer entry points (CLAUDE.md, Commands).
# Keep this file compatible with GNU Make 3.81 (macOS) as well as 4.x (CI):
# no .ONESHELL, no `!=`, no $(file ...).

SHELL := /bin/bash
export GOTOOLCHAIN := go1.27.3

BIN := $(CURDIR)/bin
FUZZTIME ?= 5s

# Committed dev defaults first, then local overrides if present (ADR-0004).
COMPOSE := docker compose --env-file .env.example $(if $(wildcard .env.local),--env-file .env.local)

.PHONY: tools hooks certs dev test fuzz test-integration proto lint lint-go lint-web lint-proto bench verify-m0 verify-m0-stack

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
# gitignored deploy/certs; delete that directory to mint a new one. mkcert
# creates its local CA on first use; `mkcert -install` (docs/setup.md) only
# makes browsers trust it. The key is made readable to all, because the
# gateway containers run as uid 65532 and, on Linux, a bind mount keeps the
# host's owner and mode (ADR-0012).
certs: deploy/certs/gateway.pem

deploy/certs/gateway.pem: | tool-mkcert
	@mkdir -p deploy/certs
	$(BIN)/mkcert -cert-file $@ -key-file deploy/certs/gateway-key.pem \
		localhost 127.0.0.1 ::1 gateway-1 gateway-2 gateway-3 gateway-4 gateway-5
	chmod 644 deploy/certs/gateway-key.pem

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
proto: tool-buf tool-protoc-gen-go
	PATH="$(BIN):$$PATH" $(BIN)/buf generate

lint: lint-go lint-web lint-proto

lint-go: tool-golangci-lint
	$(BIN)/golangci-lint run ./...
	go mod tidy -diff

lint-web:
	pnpm -C web lint

# buf lint; internal/gen matches what api/proto generates; no breaking change
# against origin/main, skipped while main has no buf.yaml (the first M1 PR).
lint-proto: tool-buf tool-protoc-gen-go
	$(BIN)/buf lint
	@tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && \
	PATH="$(BIN):$$PATH" $(BIN)/buf generate -o "$$tmp" && \
	diff -r "$$tmp/internal/gen" internal/gen || { echo "internal/gen is stale: run make proto"; exit 1; }
	@if git cat-file -e origin/main:buf.yaml 2>/dev/null; then \
		$(BIN)/buf breaking --against '.git#ref=origin/main'; \
	else echo "buf breaking: origin/main has no buf.yaml yet, skipped"; fi

bench:
	go test -run '^$$' -bench . -benchmem ./...

# M0 exit checks (docs/design.md): the dev stack, then CI and the main ruleset
# on GitHub. CI runs only the stack part, since it can't check its own result.
verify-m0: dev
	@SPILLWAY_REDIS_ADDR_MAP=$(REDIS_ADDR_MAP) test/verify/m0.sh all

verify-m0-stack: dev
	@SPILLWAY_REDIS_ADDR_MAP=$(REDIS_ADDR_MAP) test/verify/m0.sh stack
