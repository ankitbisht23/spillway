# Spillway developer entry points (CLAUDE.md, Commands).
# Keep this file compatible with GNU Make 3.81 (macOS) as well as 4.x (CI):
# no .ONESHELL, no `!=`, no $(file ...).

SHELL := /bin/bash
export GOTOOLCHAIN := go1.27.3

BIN := $(CURDIR)/bin
FUZZTIME ?= 5s

.PHONY: tools hooks test fuzz lint bench

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

tools: $(TOOL_STAMPS)

# Pre-commit hook: gitleaks scans staged changes with .gitleaks.toml.
hooks: tool-gitleaks
	@hook="$$(git rev-parse --git-path hooks)/pre-commit"; \
	printf '%s\n' '#!/bin/sh' '# Installed by make hooks: block commits that contain secrets.' \
		'exec "$$(git rev-parse --show-toplevel)/bin/gitleaks" git --pre-commit --staged --redact --no-banner' > "$$hook"; \
	chmod +x "$$hook"; echo "installed $$hook"

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

lint: tool-golangci-lint
	$(BIN)/golangci-lint run ./...
	go mod tidy -diff
	pnpm -C web lint

bench:
	go test -run '^$$' -bench . -benchmem ./...
