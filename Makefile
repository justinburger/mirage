# Mirage: service virtualization and traffic replay.
#
# Run `make` (or `make help`) to list the targets. Variables can be overridden
# on the command line, e.g. `make build VERSION=v1.2.3` or
# `make bench PKGS=./internal/store BENCHTIME=2s`.
#
# Works with GNU Make 3.81 (the macOS default) and newer.

MODULE    := github.com/justinburger/mirage
BINARY    := mirage
BIN_DIR   := bin
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X $(MODULE)/internal/version.Version=$(VERSION)

GO        ?= go
PKGS      ?= ./...
GOFMT_DIRS := cmd internal examples web
BENCH     ?= .
BENCHTIME ?= 1s
COVERFILE ?= coverage.out

# Arguments for `make run`, e.g. make run ARGS="serve --admin 127.0.0.1:9090".
ARGS      ?= serve --admin 127.0.0.1:8484 --data ./mirage.db

# Demo settings.
DEMO_CONFIG  ?= examples/mirage.yaml
DEMO_API_ADDR ?= 127.0.0.1:7001
DEMO_RPS     ?= 20
DEMO_TIME    ?= 60s

# staticcheck: from PATH, else from GOPATH/bin, else skipped with a hint.
STATICCHECK ?= $(shell command -v staticcheck 2>/dev/null || { test -x "$$($(GO) env GOPATH)/bin/staticcheck" && echo "$$($(GO) env GOPATH)/bin/staticcheck"; })

IMAGE     ?= mirage
DOCKER    ?= docker

.DEFAULT_GOAL := help

.PHONY: help build test race bench lint fmt cover cover-html run demo traffic e2e docker clean

help: ## List the available targets
	@echo "Mirage $(VERSION)"
	@echo
	@echo "Usage: make <target>"
	@echo
	@awk 'BEGIN { FS = ":.*## " } /^[a-zA-Z0-9_-]+:.*## / { printf "  %-10s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## Build the static binary into bin/mirage (CGO off, version from git describe)
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) ./cmd/mirage
	@echo "built $(BIN_DIR)/$(BINARY) $(VERSION)"

test: ## Run all tests
	$(GO) test -count=1 $(PKGS)

race: ## Run all tests with the race detector
	$(GO) test -race -count=1 $(PKGS)

bench: ## Run the benchmarks once (PKGS, BENCH, BENCHTIME override)
	$(GO) test -run='^$$' -bench='$(BENCH)' -benchmem -benchtime=$(BENCHTIME) $(PKGS)

lint: ## gofmt check, go vet and staticcheck (when installed)
	@echo "gofmt"; \
	unformatted="$$(gofmt -l $(GOFMT_DIRS))"; \
	if [ -n "$$unformatted" ]; then echo "needs gofmt:"; echo "$$unformatted"; exit 1; fi
	@echo "go vet"
	$(GO) vet $(PKGS)
	@if [ -n "$(STATICCHECK)" ]; then \
		echo "staticcheck"; \
		$(STATICCHECK) $(PKGS); \
	else \
		echo "staticcheck not installed, skipping (go install honnef.co/go/tools/cmd/staticcheck@latest)"; \
	fi

fmt: ## Format the code with gofmt
	gofmt -w $(GOFMT_DIRS)

cover: ## Run the tests with coverage and print the total
	$(GO) test -race -count=1 -coverprofile=$(COVERFILE) -covermode=atomic $(PKGS)
	@$(GO) tool cover -func=$(COVERFILE) | tail -n 1

cover-html: cover ## Open the coverage report in a browser
	$(GO) tool cover -html=$(COVERFILE)

run: build ## Build and run the server (override ARGS)
	./$(BIN_DIR)/$(BINARY) $(ARGS)

demo: build ## Run the demo API and Mirage with examples/mirage.yaml (Ctrl-C stops both)
	$(GO) build -o $(BIN_DIR)/demo-api ./examples/demo-api
	@echo
	@echo "demo API   http://$(DEMO_API_ADDR)"
	@echo "service    http://127.0.0.1:9001   (records the demo API)"
	@echo "admin UI   http://127.0.0.1:8484"
	@echo "traffic    make traffic            (in another terminal)"
	@echo
	@set -e; \
	./$(BIN_DIR)/demo-api -addr $(DEMO_API_ADDR) -v & api=$$!; \
	trap 'kill $$api 2>/dev/null || true' EXIT INT TERM; \
	./$(BIN_DIR)/$(BINARY) serve --config $(DEMO_CONFIG)

traffic: ## Send simulated users to the demo service on :9001 (DEMO_RPS, DEMO_TIME)
	$(GO) run ./examples/trafficgen -target http://127.0.0.1:9001 -rps $(DEMO_RPS) -duration $(DEMO_TIME) -users 10

e2e: ## Build the binaries and run the end-to-end test (scripts/e2e.sh; E2E_KEEP=1 keeps the temp dir)
	bash scripts/e2e.sh

docker: ## Build the container image (IMAGE, VERSION)
	$(DOCKER) build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

clean: ## Remove build output and coverage files
	rm -rf $(BIN_DIR) dist $(COVERFILE) coverage.html
