.PHONY: help build run test race lint vet tidy check standard audit fuzz dupe vuln nilcheck deadcode omitempty install deploy uninstall clean cross

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

# repo-standard is the fleet checker, pinned by version (go run <pkg>@$(VER)).
# v0.4.0 is the first tag that holds the module under this name and the omitempty command.
REPO_STANDARD_VER := v0.4.0
OMITEMPTYSCALAR_VER := v0.4.0
NILAWAY_VER := v0.0.0-20260918162853-acb8859b9031
GOVULNCHECK_VER := v1.9.0
DEADCODE_VER := v0.52.0
JSCPD_VER := 5.1.2

standard: ## Run repo-standard (fleet checker, pinned) against this repo
	go run github.com/dkoosis/repo-standard/cmd/repo-standard@$(REPO_STANDARD_VER)

# check is the fast local gate; CI runs the same verb. standard runs last so it
# grades the tree the rest of the gate passed.
check: vet lint test build standard ## Full repo: vet + lint + test + build + repo-standard
	@echo "=== check pass ==="

# audit is the exhaustive gate: check + race + fuzz + dupe + nilcheck + deadcode
# + omitempty + vuln. Slower; run before a release or a risky merge.
audit: check race fuzz dupe nilcheck deadcode omitempty vuln ## Exhaustive: check + race + fuzz + dupe + nilcheck + deadcode + omitempty + vuln
	@echo "=== audit pass ==="

# Runs each Fuzz function it finds, FUZZTIME apiece.
FUZZTIME ?= 10s
fuzz: ## Fuzz every Fuzz test (FUZZTIME=10s each)
	@set -e; \
	for pkg in $$(go list ./...); do \
		list=$$(go test -list '^Fuzz' $$pkg) || exit $$?; \
		for f in $$(printf '%s\n' "$$list" | sed -n '/^Fuzz/p'); do \
			echo "fuzz $$pkg $$f"; \
			go test -run '^$$' -fuzz "^$$f$$" -fuzztime=$(FUZZTIME) $$pkg; \
		done; \
	done

# dupe flags copy-paste duplication (jscpd, config in .jscpd.json).
dupe: ## Flag copy-paste duplication (jscpd, pinned)
	npx --yes jscpd@$(JSCPD_VER) . --silent --reporters console

vuln: ## Scan dependencies for known vulnerabilities (govulncheck, pinned)
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VER) ./...

# nilcheck runs nilaway over the whole module.
# -test=false: tests deliberately pass nil to exercise error paths.
nilcheck: ## Run nilaway nil-safety analysis (pinned)
	go run go.uber.org/nilaway/cmd/nilaway@$(NILAWAY_VER) -test=false -include-pkgs=github.com/dkoosis/strand ./...
	@echo "=== nilcheck pass ==="

# deadcode lists each function no path from main reaches, and exits 0 either way,
# so the target fails on any output.
deadcode: ## Fail on any function unreachable from main (deadcode, pinned)
	@out=$$(go run golang.org/x/tools/cmd/deadcode@$(DEADCODE_VER) ./cmd/strand) || { echo "deadcode: could not run"; exit 1; }; \
	if [ -n "$$out" ]; then echo "$$out"; echo "deadcode: the functions above are unreachable; cut them"; exit 1; fi

# omitemptyscalar exits 3 on a ,omitempty tag on a bool (a false lost on write).
omitempty: ## Fail on a bool field tagged omitempty (omitemptyscalar, pinned)
	go run github.com/dkoosis/repo-standard/cmd/omitemptyscalar@$(OMITEMPTYSCALAR_VER) ./...

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

cross: ## Cross-compile linux-amd64 and linux-arm64 into .sandbox/bin/
	for a in amd64 arm64; do mkdir -p .sandbox/bin/linux-$$a && CGO_ENABLED=0 GOOS=linux GOARCH=$$a go build -o .sandbox/bin/linux-$$a/ ./cmd/strand; done
