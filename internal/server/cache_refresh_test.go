package server

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/dkoosis/strand/internal/bd"
)

var errTornRead = errors.New("torn read")

type refreshStub struct {
	stubBD
	mu      sync.Mutex
	calls   int
	block   chan struct{}
	listErr error
	started chan struct{} // closed once, on the first List call
}

func (s *refreshStub) List(ctx context.Context, opts bd.ListOpts) ([]bd.Issue, error) {
	s.mu.Lock()
	s.calls++
	if s.calls == 1 && s.started != nil {
		close(s.started)
	}
	block, err := s.block, s.listErr
	issues := slices.Clone(s.issues)
	s.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return issues, err
}

func (s *refreshStub) callCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }

func TestRefreshCoalescesConcurrentReads(t *testing.T) {
	src := &refreshStub{stubBD: stubBD{issues: sampleIssues}, block: make(chan struct{}), started: make(chan struct{})}
	cache := newSnapshotCache(func() time.Time { return cacheNow })
	const readers = 20
	var wg sync.WaitGroup
	wg.Add(readers)
	for range readers {
		go func() {
			defer wg.Done()
			if _, err := cache.refreshList(context.Background(), "repo", src, false); err != nil {
				t.Errorf("refresh: %v", err)
			}
		}()
	}
	select {
	case <-src.started:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh goroutine never started")
	}
	close(src.block)
	wg.Wait()
	if got := src.callCount(); got != 1 {
		t.Fatalf("bd list calls = %d, want 1", got)
	}
}

func TestFailedRefreshRetainsLastGood(t *testing.T) {
	src := &refreshStub{stubBD: stubBD{issues: sampleIssues}}
	cache := newSnapshotCache(func() time.Time { return cacheNow })
	if _, err := cache.refreshList(context.Background(), "repo", src, false); err != nil {
		t.Fatal(err)
	}
	src.mu.Lock()
	src.issues = []bd.Issue{{ID: "partial"}}
	src.listErr = errTornRead
	src.mu.Unlock()
	if _, err := cache.refreshList(context.Background(), "repo", src, true); err == nil {
		t.Fatal("failed refresh returned nil error")
	}
	got, _, ok := cache.liveList("repo")
	if !ok || len(got) != len(sampleIssues) {
		t.Fatalf("last good snapshot replaced: %#v", got)
	}
}

func TestExternalRefreshPublishesChange(t *testing.T) {
	src := &refreshStub{stubBD: stubBD{issues: sampleIssues}}
	cache := newSnapshotCache(func() time.Time { return cacheNow })
	if _, err := cache.refreshList(context.Background(), "repo", src, false); err != nil {
		t.Fatal(err)
	}
	src.mu.Lock()
	src.issues = slices.Concat(sampleIssues, []bd.Issue{{ID: "external"}})
	src.mu.Unlock()
	if _, err := cache.refreshList(context.Background(), "repo", src, true); err != nil {
		t.Fatal(err)
	}
	got, _, _ := cache.liveList("repo")
	if got[len(got)-1].ID != "external" {
		t.Fatalf("external change absent: %#v", got)
	}
}

func TestWriteFollowedByReadUsesPublishedIssue(t *testing.T) {
	cache := newSnapshotCache(func() time.Time { return cacheNow })
	cache.putList("repo", []bd.Issue{{ID: "a", Title: "old"}})
	cache.upsert("repo", &bd.Issue{ID: "a", Title: "new"})
	got, _, _ := cache.liveList("repo")
	if got[0].Title != "new" {
		t.Fatalf("title = %q, want new", got[0].Title)
	}
}

// putList is a test seam: production publishes through publishList, so deadcode
// would fail it in the binary.
// putList records a fresh List result, opening the repo's snapshot and stamping it
// with both the wall time (the "data as of" readout) and the Dolt store key the
// out-of-band gate compares against. A storeKey that can't read now leaves
// storeKeyOK false, which disables the gate for this entry (freshEntryLocked) — the
// same degrade-to-old-behavior a missing signal gets everywhere else. Kept in step
// with publishList, which stamps the same pair on the production path.
func (c *snapshotCache) putList(repo string, list []bd.Issue) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	key, keyOK := c.readStoreKey(repo)
	c.entries[repo] = &snapshot{at: c.now(), storeKey: key, storeKeyOK: keyOK, gen: c.gen, list: list}
}
