package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dkoosis/strand/internal/registry"
)

// repoA and repoB are two distinct registered repos used to exercise the
// per-repo SSE scoping fixed by st-6i1: broadcastChange must reach only the
// channels watching the repo that changed, and watchedRepos must report the
// distinct set reconcileLoop needs to poll.
var (
	repoA = registry.Repo{Name: "a", Path: "/repo-a"}
	repoB = registry.Repo{Name: "b", Path: "/repo-b"}
)

func TestBroadcastChangeScopesToItsRepo(t *testing.T) {
	srv := newTestServer(t, &stubBD{})
	chA := make(chan struct{}, 1)
	chB := make(chan struct{}, 1)
	srv.events[chA] = repoA
	srv.events[chB] = repoB

	srv.broadcastChange(repoA.Path)

	select {
	case <-chA:
	default:
		t.Error("repo A's channel got nothing after repo A changed")
	}
	select {
	case <-chB:
		t.Error("repo B's channel fired after repo A changed, want no delivery")
	default:
	}
}

func TestBroadcastChangeUnknownRepoNotifiesNoOne(t *testing.T) {
	srv := newTestServer(t, &stubBD{})
	ch := make(chan struct{}, 1)
	srv.events[ch] = repoA

	srv.broadcastChange("/repo-nobody-watches")

	select {
	case <-ch:
		t.Error("channel fired for a repo it isn't scoped to")
	default:
	}
}

func TestWatchedReposDedupesByPath(t *testing.T) {
	srv := newTestServer(t, &stubBD{})
	ch1 := make(chan struct{}, 1)
	ch2 := make(chan struct{}, 1)
	ch3 := make(chan struct{}, 1)
	// Two tabs on repo A, one on repo B: reconcileLoop should poll each repo
	// once, not once per open channel.
	srv.events[ch1] = repoA
	srv.events[ch2] = repoA
	srv.events[ch3] = repoB

	got := srv.watchedRepos()

	seen := map[string]int{}
	for _, r := range got {
		seen[r.Path]++
	}
	if seen[repoA.Path] != 1 {
		t.Errorf("repo A watched %d times, want 1 (deduped)", seen[repoA.Path])
	}
	if seen[repoB.Path] != 1 {
		t.Errorf("repo B watched %d times, want 1", seen[repoB.Path])
	}
	if len(got) != 2 {
		t.Errorf("watchedRepos returned %d entries, want 2", len(got))
	}
}

func TestWatchedReposEmptyWithNoOpenChannels(t *testing.T) {
	srv := newTestServer(t, &stubBD{})
	if got := srv.watchedRepos(); len(got) != 0 {
		t.Errorf("watchedRepos on an idle server returned %d entries, want 0", len(got))
	}
}

// TestHandleEventsRegistersRequestedRepo covers the other half of st-6i1:
// handleEvents must resolve the connection's ?repo= the same way a page/
// fragment request does (registry.Resolve), and record that repo against its
// channel — the write side watchedRepos/broadcastChange read from.
func TestHandleEventsRegistersRequestedRepo(t *testing.T) {
	srv := newTestServer(t, &stubBD{})
	// demoRepo (the test registry's only repo) is what an empty/omitted
	// ?repo= resolves to via Active().
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.handleEvents(rec, req)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for {
		srv.eventsMu.Lock()
		n := len(srv.events)
		var got registry.Repo
		for _, r := range srv.events {
			got = r
		}
		srv.eventsMu.Unlock()
		if n == 1 {
			if got.Path != demoRepo.Path {
				t.Errorf("handleEvents registered repo %q, want %q", got.Path, demoRepo.Path)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("handleEvents never registered a channel")
		default:
		}
	}

	cancel()
	<-done
}
