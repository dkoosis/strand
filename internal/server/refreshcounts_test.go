package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkoosis/strand/internal/registry"
)

// TestDefaultRefreshCountsExecsBeadwatchWithRepoPath is the regression for
// bw-onx: strand no longer recomputes counts.json in-process — it execs the
// beadwatch binary and hands it the repo path as a bare, explicit root (never
// --all). The stub is a scratch script on PATH that records its own argv, so
// the assertion is on exactly what the real beadwatch binary would receive.
func TestDefaultRefreshCountsExecsBeadwatchWithRepoPath(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv.txt")
	script := "#!/bin/sh\necho \"$@\" > " + argvFile + "\n"
	if err := os.WriteFile(filepath.Join(dir, "beadwatch"), []byte(script), 0o755); err != nil { //nolint:gosec // test-only executable stub
		t.Fatalf("write beadwatch stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	s := &Server{now: time.Now}
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	defer s.bgCancel()

	s.defaultRefreshCounts(registry.Repo{Path: "/tmp/some-repo"})
	s.bgWG.Wait() // the exec runs in a tracked background goroutine — wait for it to land

	got, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("beadwatch stub never ran (argv file missing): %v", err)
	}
	if want, got := "/tmp/some-repo", strings.TrimSpace(string(got)); got != want {
		t.Errorf("beadwatch argv = %q, want %q", got, want)
	}
}

// TestDefaultRefreshCountsSkipsWhenBeadwatchMissing pins the absent-binary path:
// no beadwatch on PATH or at $HOME/go/bin means the refresh is a no-op, not a
// crash or a hang.
func TestDefaultRefreshCountsSkipsWhenBeadwatchMissing(t *testing.T) {
	emptyDir := t.TempDir()
	t.Setenv("PATH", emptyDir)
	t.Setenv("HOME", emptyDir) // so $HOME/go/bin/beadwatch resolves to nothing either

	s := &Server{now: time.Now}
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	defer s.bgCancel()

	s.defaultRefreshCounts(registry.Repo{Path: "/tmp/some-repo"})
	s.bgWG.Wait()
	// No assertion beyond "did not hang/panic" — beadwatchPath's ok=false path
	// logs once and returns before spawning anything.
}
