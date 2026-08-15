.PHONY: help build run test race lint vet tidy check selfcheck audit dupe vuln nilcheck install deploy uninstall clean

.DEFAULT_GOAL := check

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
STRAND_ADDR ?= 127.0.0.1:7777

# Per-worktree golangci-lint cache. Concurrent worktrees (dispatch/team runs)
# otherwise share one cache (~/.cache/golangci-lint); one worktree's cached
# analysis leaks stale file paths into another's run, so a clean worktree goes
# false-RED citing a sibling's files (st-afw). Keying off $(CURDIR) gives each
# worktree its own cache, so contention can't happen. (GOCACHE stays shared —
# it's content-addressed and doesn't leak paths.)
GOLANGCI_LINT_CACHE := $(CURDIR)/.golangci-cache

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

build: ## Build the strand binary into bin/
	go build -o bin/strand ./cmd/strand

run: ## Run the strand server from source
	go run ./cmd/strand

test: ## Run the test suite
	go test ./...

# race runs the suite under the race detector. -count=1 bypasses the test
# cache so race runs on every invocation.
race: ## Run the suite under the race detector (uncached)
	go test -race -count=1 ./...

# lint runs the strict golangci-lint set (.golangci.yml).
# --allow-parallel-runners: golangci-lint's global single-instance lock exists to
# stop concurrent runs corrupting a shared cache. With per-worktree caches (above)
# that risk is gone, so we let dispatch/team waves lint concurrently.
lint: ## Run the strict golangci-lint set (.golangci.yml)
	GOLANGCI_LINT_CACHE="$(GOLANGCI_LINT_CACHE)" golangci-lint run --allow-parallel-runners ./...

vet: ## Run go vet
	go vet ./...

tidy: ## Tidy go.mod/go.sum
	go mod tidy

# Dogfood the fleet gate (sd-th5.16): conform is pinned as a go.mod tool
# directive, so `go tool conform` runs the go.sum-verified pinned version.
selfcheck: ## Run conform (fleet SDLC checker) against this repo
	go tool conform

# check is the fast local gate; CI runs the same verb (conform ci-gate rule).
check: vet lint test build selfcheck ## Full repo: vet + lint + test + build + conform
	@echo "=== check pass ==="

# audit is the exhaustive gate (modeled on ../trixi): check + race + dupe +
# vuln + nilcheck. Slower; run before a release or a risky merge.
audit: check race dupe vuln nilcheck ## Exhaustive: check + race + dupe + vuln + nilcheck
	@echo "=== audit pass ==="

# dupe flags copy-paste duplication (jscpd, config in .jscpd.json).
dupe: ## Flag copy-paste duplication (jscpd)
	@command -v jscpd >/dev/null 2>&1 || { echo "dupe: jscpd missing — npm i -g jscpd@latest"; exit 1; }
	@TMP=$$(mktemp -d); jscpd . --output $$TMP; rm -rf $$TMP

vuln: ## Scan dependencies for known vulnerabilities
	govulncheck ./...

# nilcheck runs nilaway over the whole module (skips if nilaway missing).
# -test=false: tests deliberately pass nil to exercise error paths.
nilcheck: ## Run nilaway nil-safety analysis (skips if missing)
	@if ! command -v nilaway >/dev/null 2>&1; then \
		echo "nilcheck: nilaway not installed — skipping (go install go.uber.org/nilaway/cmd/nilaway@latest)"; \
		exit 0; \
	fi
	nilaway -test=false -include-pkgs=github.com/dkoosis/strand ./...
	@echo "=== nilcheck pass ==="

install: ## Build + install the strand binary into GOBIN
	go install -ldflags='-X main.Version=$(VERSION)' ./cmd/strand

# deploy builds, installs, and (re)loads the launchd web agent (macOS only).
deploy: install ## Install + (re)load the launchd web agent (macOS only)
	@[ "$$(uname -s)" = "Darwin" ] || { echo "deploy: macOS-only (uses launchctl)"; exit 1; }
	@bash "$(CURDIR)/deploy/launchd/install.sh" "$(STRAND_ADDR)"
	@echo "=== deployed (http://$(STRAND_ADDR)) ==="

# uninstall stops + removes the launchd web agent (logs preserved).
uninstall: ## Stop + remove the launchd web agent (logs preserved)
	@bash "$(CURDIR)/deploy/launchd/uninstall.sh"

clean: ## Remove build artifacts and lint cache
	rm -rf bin .golangci-cache
