SHELL := /bin/bash
MODULE := github.com/olehmushka/go-signalium
BIN_DIR := bin
BIN := $(BIN_DIR)/go-signalium
GO ?= go
PKGS := ./...

# Atlas + sqlc are introduced in M2 and M3; targets are wired now so the
# developer workflow stays stable as milestones land.
ATLAS ?= atlas
SQLC ?= sqlc
GOLANGCI_LINT ?= golangci-lint

.PHONY: help
help: ## List available targets
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-22s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: tidy
tidy: ## go mod tidy
	$(GO) mod tidy

.PHONY: fmt
fmt: ## gofumpt + goimports
	$(GO) run mvdan.cc/gofumpt@latest -w .

.PHONY: build
build: ## Compile the binary into ./bin/go-signalium
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN) ./cmd/go-signalium

.PHONY: run
run: ## Run the service from source
	$(GO) run ./cmd/go-signalium

.PHONY: test
test: ## Run unit tests (hermetic, no Docker required)
	$(GO) test -race -count=1 $(PKGS)

.PHONY: integration-test
integration-test: ## Run integration tests (spins testcontainers — Docker required)
	$(GO) test -tags integration -race -count=1 -timeout 5m $(PKGS)

.PHONY: lint
lint: ## golangci-lint
	$(GOLANGCI_LINT) run $(PKGS)

.PHONY: vet
vet: ## go vet
	$(GO) vet $(PKGS)

# ----- Atlas (wired up properly in M2) -----

.PHONY: migrate-status
migrate-status: ## Show pending migrations
	$(ATLAS) migrate status --env local

.PHONY: migrate-up
migrate-up: ## Apply pending migrations
	$(ATLAS) migrate apply --env local

.PHONY: migrate-down
migrate-down: ## Roll back the most recent migration
	$(ATLAS) migrate down --env local

.PHONY: migrate-diff
migrate-diff: ## Generate a new migration (NAME=<slug>)
	@test -n "$(NAME)" || (echo "NAME is required: make migrate-diff NAME=add_inbound_table"; exit 1)
	$(ATLAS) migrate diff $(NAME) --env local

.PHONY: migrate-hash
migrate-hash: ## Recompute atlas.sum
	$(ATLAS) migrate hash --env local

# ----- sqlc (wired up properly in M2) -----

.PHONY: sqlc-generate
sqlc-generate: ## Regenerate ./internal/repo/sqlc from queries + migrations
	$(SQLC) generate

.PHONY: sqlc-verify
sqlc-verify: ## Verify queries compile against the current schema
	$(SQLC) vet

# ----- Conjure -----
#
# `make conjure` runs the two-step pipeline:
#   1. conjure       compiles YAML -> IR JSON   (Java tool, palantir/conjure)
#   2. conjure-go    compiles IR  -> Go code    (palantir/conjure-go)
# Both tools are installed into $(GOBIN). Defaults below resolve to the Go bin
# dir to avoid clashing with /usr/bin/conjure (ImageMagick ships a binary with
# the same name on some distros).

GOBIN          ?= $(shell go env GOBIN)
ifeq ($(GOBIN),)
GOBIN          := $(shell go env GOPATH)/bin
endif
CONJURE        ?= $(GOBIN)/conjure
CONJURE_GO     ?= $(GOBIN)/conjure-go
CONJURE_IDL    := conjure/go-signalium-api.conjure.yml
CONJURE_IR     := conjure/go-signalium-api.conjure.ir.json
CONJURE_OUT    := internal/generated

.PHONY: conjure
conjure: $(CONJURE_OUT)/.stamp ## Regenerate ./internal/generated from the Conjure IDL

$(CONJURE_OUT)/.stamp: $(CONJURE_IDL)
	$(CONJURE) compile $(CONJURE_IDL) $(CONJURE_IR)
	$(CONJURE_GO) --server --output $(CONJURE_OUT) $(CONJURE_IR)
	@mkdir -p $(CONJURE_OUT) && touch $@

.PHONY: conjure-clean
conjure-clean: ## Remove generated conjure code and IR
	rm -rf $(CONJURE_OUT) $(CONJURE_IR)

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) cover.out coverage.html dist

# ----- Coverage / benchmarks / fuzz / release -----

.PHONY: cover
cover: ## Run unit tests with coverage and print the summary
	$(GO) test -race -count=1 -coverprofile=cover.out -covermode=atomic $(PKGS)
	$(GO) tool cover -func=cover.out | tail -1

.PHONY: cover-html
cover-html: cover ## Render an HTML coverage report
	$(GO) tool cover -html=cover.out -o coverage.html

.PHONY: bench
bench: ## Run hermetic benchmarks (no Docker)
	$(GO) test -bench=. -benchmem -run=^$$ $(PKGS)

.PHONY: bench-integration
bench-integration: ## Run benchmarks behind the integration build tag (Docker required)
	$(GO) test -tags integration -bench=. -benchmem -run=^$$ ./internal/repo/...

.PHONY: fuzz
fuzz: ## Run the multipart handler fuzz target for 30s
	$(GO) test -run=^$$ -fuzz=FuzzMultipart -fuzztime=30s ./internal/handler/

.PHONY: release-snapshot
release-snapshot: ## goreleaser snapshot — builds dist/ without signing or publishing
	goreleaser release --snapshot --clean --skip=sign,publish
