package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dkoosis/strand/internal/bd"
)

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
	issues := append([]bd.Issue(nil), s.issues...)
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
	src.listErr = errors.New("torn read")
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
	src.issues = append(append([]bd.Issue(nil), sampleIssues...), bd.Issue{ID: "external"})
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
