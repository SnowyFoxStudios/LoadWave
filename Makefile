# LoadWave build tasks.
#
# `make help` lists everything. The common paths are:
#
#     make build      compile the binary with the dashboard embedded
#     make test       run the Go and TypeScript test suites
#     make check      everything CI runs, before you push
#     make dev        run the coordinator and the frontend dev server together

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

# Pinned so that every contributor and CI generate byte-identical code.
# Bumping one of these means committing the regenerated output alongside it.
BUF_VERSION              := v1.72.0
PROTOC_GEN_GO_VERSION    := v1.36.11
PROTOC_GEN_GRPC_VERSION  := v1.6.2
GOLANGCI_LINT_VERSION    := v2.7.1

TOOLS   := $(CURDIR)/.tools
BIN     := $(CURDIR)/bin
DIST    := $(CURDIR)/web/dist
BINARY  := $(BIN)/loadwave

# Version metadata stamped into the binary. A tagged build gets the tag; any
# other build is identified by its commit so a bug report can be traced back.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

MODULE  := github.com/SnowyFoxStudios/LoadWave
LDFLAGS := -s -w \
	-X $(MODULE)/internal/buildinfo.version=$(VERSION) \
	-X $(MODULE)/internal/buildinfo.commit=$(COMMIT) \
	-X $(MODULE)/internal/buildinfo.date=$(DATE)

export PATH := $(TOOLS):$(PATH)

# `go test ./...` would otherwise descend into web/node_modules, which ships
# stray Go files in some packages.
GO_PACKAGES := ./cmd/... ./internal/... ./pkg/... ./examples/...

.PHONY: help
help: ## List the available targets
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

.PHONY: build
build: ui ## Build the binary with the dashboard embedded
	@mkdir -p $(BIN)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/loadwave
	@echo "built $(BINARY) ($(VERSION))"

.PHONY: build-nodashboard
build-nodashboard: ## Build the binary without building the dashboard first
	@mkdir -p $(BIN)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/loadwave

.PHONY: install
install: ui ## Install the binary into GOBIN
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/loadwave

.PHONY: ui
ui: node_modules ## Build the dashboard
	npm --prefix web run build
	@# Vite empties web/dist before writing to it, taking the tracked
	@# .gitkeep placeholder with it. Restored so the tree stays clean —
	@# CI's release job depends on that, and it saves every other builder
	@# the same "why is git dirty" surprise.
	@git checkout -- web/dist/.gitkeep

node_modules: web/package-lock.json
	npm --prefix web ci
	@touch web/node_modules

# ---------------------------------------------------------------------------
# Develop
# ---------------------------------------------------------------------------

.PHONY: dev
dev: ## Run the coordinator and the frontend dev server together
	@echo "coordinator on :8088, dashboard with hot reload on :5173"
	@trap 'kill 0' EXIT INT TERM; \
	go run ./cmd/loadwave serve --ui-addr :8088 --allowed-origin localhost:5173 & \
	npm --prefix web run dev & \
	wait

.PHONY: run-example
run-example: build ## Run the bundled example against a local demo server
	$(BINARY) run examples/basic.yaml --ui

# ---------------------------------------------------------------------------
# Test
# ---------------------------------------------------------------------------

.PHONY: test
test: test-go test-web ## Run every test suite

.PHONY: test-go
test-go: ## Run the Go tests with the race detector
	go test -race -timeout 10m $(GO_PACKAGES)

.PHONY: test-short
test-short: ## Run the Go tests, skipping the slow integration ones
	go test -short -timeout 2m $(GO_PACKAGES)

.PHONY: test-web
test-web: node_modules ## Run the dashboard tests
	npm --prefix web test

.PHONY: cover
cover: ## Produce a Go coverage report at coverage.html
	go test -coverprofile=coverage.out -covermode=atomic $(GO_PACKAGES)
	go tool cover -html=coverage.out -o coverage.html
	@echo "wrote coverage.html"

.PHONY: bench
bench: ## Run the Go benchmarks
	go test -run '^$$' -bench . -benchmem $(GO_PACKAGES)

# ---------------------------------------------------------------------------
# Check
# ---------------------------------------------------------------------------

.PHONY: check
check: fmt-check vet lint lint-proto lint-web typecheck test ## Everything CI runs

.PHONY: fmt
fmt: ## Format Go and frontend sources
	gofmt -w cmd internal pkg examples web/embed.go
	npm --prefix web run format 2>/dev/null || true

.PHONY: fmt-check
fmt-check: ## Fail if anything is unformatted
	@unformatted=$$(gofmt -l cmd internal pkg examples web/embed.go); \
	if [[ -n "$$unformatted" ]]; then \
		echo "these files need gofmt:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: vet
vet: ## Run go vet
	go vet $(GO_PACKAGES)

.PHONY: lint
lint: $(TOOLS)/golangci-lint ## Lint the Go sources
	$(TOOLS)/golangci-lint run

.PHONY: lint-proto
lint-proto: $(TOOLS)/buf ## Lint the protobuf definitions
	$(TOOLS)/buf lint

.PHONY: lint-web
lint-web: node_modules ## Lint the dashboard sources
	npm --prefix web run lint

.PHONY: typecheck
typecheck: node_modules ## Type-check the dashboard
	npm --prefix web run typecheck

.PHONY: tidy
tidy: ## Tidy go.mod and verify nothing changed
	go mod tidy
	go mod verify

# ---------------------------------------------------------------------------
# Generate
# ---------------------------------------------------------------------------

.PHONY: generate
generate: $(TOOLS)/buf $(TOOLS)/protoc-gen-go $(TOOLS)/protoc-gen-go-grpc ## Regenerate protobuf code
	$(TOOLS)/buf generate
	@echo "regenerated gen/ — commit the result alongside the .proto change"

.PHONY: generate-check
generate-check: generate ## Fail if the generated code is out of date
	@if [[ -n "$$(git status --porcelain gen)" ]]; then \
		echo "generated code is out of date; run 'make generate' and commit the result"; \
		git --no-pager diff --stat gen; exit 1; \
	fi

# ---------------------------------------------------------------------------
# Tools
# ---------------------------------------------------------------------------

# Tools are built with the toolchain this repository targets, not with the one
# each tool's own go.mod asks for.
#
# `go install pkg@version` resolves the toolchain from the *target* module's go
# directive, so a tool pinned to an older Go gets built with that older Go —
# and which one you get depends on the machine, which is how this went
# unnoticed. For golangci-lint the difference is fatal rather than cosmetic: it
# refuses to run when the Go it was built with is older than the version go.mod
# targets, so a v2.7.1 release built with go1.25 cannot lint a module on 1.26.
#
# Naming the version explicitly makes the result identical everywhere. It is
# read from go.mod rather than written twice, so bumping Go cannot leave this
# behind. `local` would be wrong: contributors are not expected to have this
# exact Go installed already, only to let the toolchain fetch it.
GO_TOOLCHAIN := go$(shell awk '$$1 == "go" { print $$2; exit }' go.mod)
GO_INSTALL   := GOTOOLCHAIN=$(GO_TOOLCHAIN) GOBIN=$(TOOLS) go install

$(TOOLS)/buf:
	$(GO_INSTALL) github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)

$(TOOLS)/protoc-gen-go:
	$(GO_INSTALL) google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)

$(TOOLS)/protoc-gen-go-grpc:
	$(GO_INSTALL) google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GRPC_VERSION)

$(TOOLS)/golangci-lint:
	$(GO_INSTALL) github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: tools
tools: $(TOOLS)/buf $(TOOLS)/protoc-gen-go $(TOOLS)/protoc-gen-go-grpc $(TOOLS)/golangci-lint ## Install the pinned developer tools

# ---------------------------------------------------------------------------
# Clean
# ---------------------------------------------------------------------------

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN) $(DIST) coverage.out coverage.html
	@mkdir -p $(DIST) && touch $(DIST)/.gitkeep

.PHONY: clean-all
clean-all: clean ## Also remove tools and node_modules
	rm -rf $(TOOLS) web/node_modules
