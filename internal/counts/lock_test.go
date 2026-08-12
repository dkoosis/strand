package counts

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/bdcounts"
)

// gatedSource is a source whose first read parks until released, so a test can hold
// one refresh mid-compute while a second one starts against the same cache dir.
type gatedSource struct {
	fakeSource
	entered chan struct{} // closed when the read starts
	release chan struct{} // the read returns once this is closed
	once    sync.Once
}

func (g *gatedSource) List(ctx context.Context, opts bd.ListOpts) ([]bd.Issue, error) {
	g.once.Do(func() {
		close(g.entered)
		<-g.release
	})
	return g.fakeSource.List(ctx, opts)
}

// TestRefreshSerializesConcurrentRuns is the st-k6z regression: two refreshes over
// disjoint repos, overlapping in time, must both land their rows in counts.json and
// their gate entries in counts-mtimes. Atomic writes alone do not give this — before
// the cache-dir lock, the second run read the base while the first was still computing
// and then replaced the file, silently dropping the first run's repo.
func TestRefreshSerializesConcurrentRuns(t *testing.T) {
	projects := t.TempDir()
	cache := t.TempDir()
	a := mkRepo(t, projects, "repo-a")
	b := mkRepo(t, projects, "repo-b")

	gate := &gatedSource{
		fakeSource: fakeSource{issues: []bd.Issue{{ID: "x", Status: bd.StatusOpen}}},
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	slow := config{
		cacheDir: cache, projects: projects, mode: modeExplicit, targets: []string{a},
		newSource: func(string) source { return gate },
	}
	fast := config{
		cacheDir: cache, projects: projects, mode: modeExplicit, targets: []string{b},
		newSource: func(string) source { return oneOpenBead() },
	}

	errs := make(chan error, 2)
	go func() { errs <- refresh(context.Background(), &slow) }()

	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("slow refresh never reached its first read")
	}
	go func() { errs <- refresh(context.Background(), &fast) }()
	// Give the second run time to reach the lock (and, unlocked, to read the base and
	// race ahead) before the first is allowed to finish.
	time.Sleep(100 * time.Millisecond)
	close(gate.release)

	for range 2 {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("refresh: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("refresh did not return — lock not released?")
		}
	}

	r := bdcounts.NewReaderAt(filepath.Join(cache, "counts.json"))
	for _, root := range []string{a, b} {
		if _, ok := r.Lookup(root); !ok {
			t.Errorf("no counts.json row for %s — a concurrent refresh dropped it", root)
		}
	}
	state, err := os.ReadFile(filepath.Join(cache, "counts-mtimes"))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	for _, root := range []string{a, b} {
		if !strings.Contains(string(state), root+"\t") {
			t.Errorf("no counts-mtimes entry for %s — a concurrent refresh dropped it", root)
		}
	}
}

// TestRefreshLockTimesOutOnWedgedHolder: a holder that never releases must not hang a
// refresh forever — past the wait the run fails loudly so launchd's next fire does not
// pile up blocked refreshers behind it.
func TestRefreshLockTimesOutOnWedgedHolder(t *testing.T) {
	cache := t.TempDir()
	held := make(chan struct{})
	holding := make(chan struct{})
	go func() {
		_ = withLock(context.Background(), cache, func() error {
			close(holding)
			<-held
			return nil
		})
	}()
	<-holding
	defer close(held)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // stands in for the lockWait deadline, without a 2-minute test
	err := withLock(ctx, cache, func() error {
		t.Error("fn ran while another holder had the lock")
		return nil
	})
	if err == nil {
		t.Fatal("withLock returned nil while the lock was held")
	}
}
