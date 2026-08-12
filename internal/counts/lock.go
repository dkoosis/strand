package counts

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// lockName is the advisory lock file inside the counts cache dir. It carries no
// content — flock's kernel-side state is the lock, so a crashed holder releases it
// automatically when its fd closes (unlike a hand-rolled O_EXCL marker, which would
// strand the next run behind a stale file).
const lockName = ".refresh.lock"

// lockWait bounds how long a refresh waits for the holder. A wedged process must not
// let launchd's every-minute fire pile up blocked refreshers forever; past the wait we
// give up loudly instead of hanging.
const lockWait = 2 * time.Minute

// lockPoll is the retry interval while another process holds the lock.
const lockPoll = 50 * time.Millisecond

// errLockBusy is returned when the wait elapses with another refresh still holding the
// lock — a wedged or very slow holder, not a transient overlap.
var errLockBusy = errors.New("counts: refresh already running")

// withLock runs fn while holding an exclusive cross-process lock on dir, so the whole
// read-compute-write of counts.json + counts-mtimes is serialized (st-k6z). Atomic
// writes alone are not enough: two refreshes (launchd's `--all` run, a manual
// `strand counts`, and the server's per-write `strand counts <repo>` spawns) each read
// the same base, compute, and replace the file — the last writer wins wholesale and
// silently drops the other's rows. The lock closes that window; the atomic write still
// protects concurrent *readers* mid-rename.
//
// The lock is advisory and per-fd, so it serializes goroutines in one process as well
// as separate processes. It is taken non-blocking on a poll loop rather than with a
// blocking LOCK_EX so ctx cancellation and lockWait both stay honored.
func withLock(ctx context.Context, dir string, fn func() error) error {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("counts: open lock: %w", err)
	}
	defer f.Close()

	if err := acquire(ctx, f); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // closing the fd releases the lock regardless

	return fn()
}

// acquire polls for the exclusive lock until it lands, ctx ends, or lockWait elapses.
func acquire(ctx context.Context, f *os.File) error {
	deadline := time.Now().Add(lockWait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("counts: lock: %w", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w (waited %s for %s)", errLockBusy, lockWait, f.Name())
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("counts: waiting for refresh lock: %w", ctx.Err())
		case <-time.After(lockPoll):
		}
	}
}
