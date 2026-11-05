package bd_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dkoosis/strand/internal/bd"
)

// A cancelled bd whose own child still holds stdout must not hang List: the
// kill reaches bd only, so without WaitDelay Wait blocks on the pipe until the
// grandchild exits, and every bd call queued behind execMu waits with it.
func TestListReturnsWhenGrandchildHoldsStdout(t *testing.T) {
	path := fakeBDWithGrandchild(t, "sleep 20")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := (&bd.Client{Bin: path}).List(ctx, bd.ListOpts{})
	if err == nil {
		t.Fatal("List succeeded; want an error from the killed bd")
	}
	if elapsed := time.Since(start); elapsed > bd.ExecWaitDelay+3*time.Second {
		t.Fatalf("List took %v after cancel; WaitDelay should bound it near %v", elapsed, bd.ExecWaitDelay)
	}
}

// A bd that exits 0 while its child still holds stdout is a success: List
// returns bd's output instead of the ErrWaitDelay Go reports for it.
func TestListSucceedsWhenGrandchildOutlivesCleanExit(t *testing.T) {
	path := fakeBDWithGrandchild(t, "echo '[]'")

	if _, err := (&bd.Client{Bin: path}).List(context.Background(), bd.ListOpts{}); err != nil {
		t.Fatalf("List: %v; want success after bd exited 0", err)
	}
}

// fakeBDWithGrandchild writes a fake bd that starts a 20s sleep holding its
// stdout, then runs tail. The sleep is killed when the test ends.
func fakeBDWithGrandchild(t *testing.T, tail string) string {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	path := filepath.Join(dir, "bd")
	script := "#!/bin/sh\nsleep 20 &\necho $! > " + pidFile + "\n" + tail + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return path
}
