.PHONY: build run test race lint vet tidy check audit dupe vuln nilcheck install deploy uninstall clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
STRAND_ADDR ?= 127.0.0.1:7777

# Per-worktree golangci-lint cache. Concurrent worktrees (dispatch/team runs)
# otherwise share one cache (~/.cache/golangci-lint); one worktree's cached
# analysis leaks stale file paths into another's run, so a clean worktree goes
# false-RED citing a sibling's files (st-afw). Keying off $(CURDIR) gives each
# worktree its own cache, so contention can't happen. (GOCACHE stays shared —
# it's content-addressed and doesn't leak paths.)
GOLANGCI_LINT_CACHE := $(CURDIR)/.golangci-cache

build:
	go build -o bin/strand ./cmd/strand

run:
	go run ./cmd/strand

test:
	go test ./...

# race runs the suite under the race detector.
race:
	go test -race -count=1 ./...

# lint runs the strict golangci-lint set (.golangci.yml).
# --allow-parallel-runners: golangci-lint's global single-instance lock exists to
# stop concurrent runs corrupting a shared cache. With per-worktree caches (above)
# that risk is gone, so we let dispatch/team waves lint concurrently.
lint:
	GOLANGCI_LINT_CACHE=$(GOLANGCI_LINT_CACHE) golangci-lint run --allow-parallel-runners ./...

vet:
	go vet ./...

tidy:
	go mod tidy

# check is the full local gate: vet + strict lint + race.
check: vet lint race

# audit is the exhaustive gate (modeled on ../trixi): check + dupe + vuln +
# nilcheck. Slower; run before a release or a risky merge.
audit: check dupe vuln nilcheck
	@echo "=== audit pass ==="

# dupe flags copy-paste duplication (jscpd, config in .jscpd.json).
dupe:
	@TMP=$$(mktemp -d); jscpd . --output $$TMP; rm -rf $$TMP

# vuln scans for known vulnerabilities in dependencies.
vuln:
	govulncheck ./...

# nilcheck runs nilaway over the whole module (skips if nilaway missing).
# -test=false: tests deliberately pass nil to exercise error paths.
nilcheck:
	@if ! command -v nilaway >/dev/null 2>&1; then \
		echo "nilcheck: nilaway not installed — skipping (go install go.uber.org/nilaway/cmd/nilaway@latest)"; \
		exit 0; \
	fi
	nilaway -test=false -include-pkgs=github.com/dkoosis/strand ./...
	@echo "=== nilcheck pass ==="

# install builds + installs the strand binary into $GOBIN/$GOPATH/bin.
install:
	go install -ldflags='-X main.Version=$(VERSION)' ./cmd/strand

# deploy builds, installs, and (re)loads the launchd web agent (macOS only).
deploy: install
	@[ "$$(uname -s)" = "Darwin" ] || { echo "deploy: macOS-only (uses launchctl)"; exit 1; }
	@bash "$(CURDIR)/deploy/launchd/install.sh" "$(STRAND_ADDR)"
	@echo "=== deployed (http://$(STRAND_ADDR)) ==="

# uninstall stops + removes the launchd web agent (logs preserved).
uninstall:
	@bash "$(CURDIR)/deploy/launchd/uninstall.sh"

clean:
	rm -rf bin .golangci-cache
