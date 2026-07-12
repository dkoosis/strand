package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/registry"
	"github.com/dkoosis/strand/internal/strand"
	"github.com/dkoosis/strand/web"
)

// serverFor wires a server over an explicit registry and a per-repo source map,
// so a switch can be observed re-scoping the views to a different stub.
func serverFor(t *testing.T, reg *registry.Registry, byPath map[string]IssueSource) *Server {
	t.Helper()
	tmpl, err := web.Templates()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	srcFor := func(r registry.Repo) IssueSource { return byPath[r.Path] }
	return New(srcFor, reg, tmpl, web.Static(), strand.Synthesis{NorthStar: "ns"})
}

// TestHeaderShowsActiveRepo: the landing's repo selector is captioned with the
// active repo's name (R1: list known repos, MRU active).
func TestHeaderShowsActiveRepo(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: sampleIssues})
	body := do(t, srv, "/").Body.String()
	if !strings.Contains(body, "demo") {
		t.Errorf("header missing the active repo name:\n%s", body)
	}
}

// TestReposMenuListsRegistered: GET /repos renders a selector row per registered
// repo, each wired to POST /repo with its path.
func TestReposMenuListsRegistered(t *testing.T) {
	reg := registry.InMemory(
		registry.Repo{Name: "alpha", Path: "/a"},
		registry.Repo{Name: "beta", Path: "/b"},
	)
	srv := serverFor(t, reg, map[string]IssueSource{"/a": &stubBD{}, "/b": &stubBD{}})
	body := do(t, srv, "/repos").Body.String()
	for _, want := range []string{"alpha", "beta", `hx-post="/repo"`, `"path":"/a"`} {
		if !strings.Contains(body, want) {
			t.Errorf("repo menu missing %q:\n%s", want, body)
		}
	}
}

// TestSwitchRepoReScopes: picking a different repo re-scopes the views. The list
// pane then shows the new repo's beads and not the old one's, and the switch tells
// htmx to reload so every view re-scopes (R1: switch active repo).
func TestSwitchRepoReScopes(t *testing.T) {
	reg := registry.InMemory(
		registry.Repo{Name: "alpha", Path: "/a"},
		registry.Repo{Name: "beta", Path: "/b"},
	)
	stubA := &stubBD{issues: []bd.Issue{{ID: "a-1", Title: "Alpha work", Status: "open"}}}
	stubB := &stubBD{issues: []bd.Issue{{ID: "b-1", Title: "Beta work", Status: "open"}}}
	srv := serverFor(t, reg, map[string]IssueSource{"/a": stubA, "/b": stubB})

	// alpha is active by default (ties broken by name); the list shows its bead.
	if body := do(t, srv, "/list").Body.String(); !strings.Contains(body, "Alpha work") {
		t.Fatalf("default list not scoped to alpha:\n%s", body)
	}

	rec := send(t, srv, http.MethodPost, "/repo", "path=/b")
	if rec.Header().Get("HX-Refresh") != "true" {
		t.Errorf("switch did not request a reload, got %q", rec.Header().Get("HX-Refresh"))
	}

	body := do(t, srv, "/list").Body.String()
	if !strings.Contains(body, "Beta work") {
		t.Errorf("list did not re-scope to beta:\n%s", body)
	}
	if strings.Contains(body, "Alpha work") {
		t.Error("list still shows the old repo's beads after a switch")
	}
}

// TestHomeRepoDeepLinkScopes: a `/?repo=<path>` deep-link (the status line's OSC 8
// link carrying its own repo) switches the active repo before rendering, so the
// landing — and every follow-on fragment — scopes to the named repo, not whatever
// was last active (st-vai). A combined `?repo=&filter=` still applies the pulse cut.
func TestHomeRepoDeepLinkScopes(t *testing.T) {
	reg := registry.InMemory(
		registry.Repo{Name: "alpha", Path: "/a"},
		registry.Repo{Name: "beta", Path: "/b"},
	)
	stubA := &stubBD{issues: []bd.Issue{{ID: "a-1", Title: "Alpha work", Status: "open"}}}
	stubB := &stubBD{issues: []bd.Issue{{ID: "b-1", Title: "Beta work", Status: "open"}}}
	srv := serverFor(t, reg, map[string]IssueSource{"/a": stubA, "/b": stubB})

	// alpha is active by default (ties broken by name); the deep-link names beta.
	body := do(t, srv, "/?repo=/b").Body.String()
	if !strings.Contains(body, "Beta work") {
		t.Errorf("deep-link did not scope the landing to beta:\n%s", body)
	}
	if strings.Contains(body, "Alpha work") {
		t.Error("deep-link landing still shows the old active repo's beads")
	}

	// The switch is sticky: a follow-on fragment reads the now-active repo.
	if lb := do(t, srv, "/list").Body.String(); !strings.Contains(lb, "Beta work") || strings.Contains(lb, "Alpha work") {
		t.Errorf("fragment after deep-link not scoped to beta:\n%s", lb)
	}

	// An unknown path is ignored — the landing keeps the active repo, no error.
	if eb := do(t, srv, "/?repo=/nope").Body.String(); !strings.Contains(eb, "Beta work") {
		t.Errorf("unknown ?repo should fall back to the active repo, got:\n%s", eb)
	}
}

// TestEmptyStateWhenNoRepo: with no registered repo the landing renders the
// actionable empty state, not an error dump (R1: no repos / empty).
func TestEmptyStateWhenNoRepo(t *testing.T) {
	srv := serverFor(t, registry.InMemory(), map[string]IssueSource{})
	rec := do(t, srv, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / empty = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "empty-state") || !strings.Contains(body, "repo selector") {
		t.Errorf("empty landing missing the actionable empty state:\n%s", body)
	}
	if strings.Contains(body, "error-fragment") {
		t.Error("empty landing rendered an error dump")
	}
}

// TestSwitchUnknownRepoSurfacesError: switching to an unregistered path re-renders
// the menu with the error instead of scoping to nothing.
func TestSwitchUnknownRepoSurfacesError(t *testing.T) {
	reg := registry.InMemory(registry.Repo{Name: "alpha", Path: "/a"})
	srv := serverFor(t, reg, map[string]IssueSource{"/a": &stubBD{}})
	rec := send(t, srv, http.MethodPost, "/repo", "path=/nope")
	if rec.Header().Get("HX-Refresh") == "true" {
		t.Error("a failed switch still requested a reload")
	}
	if !strings.Contains(rec.Body.String(), "rm-err") {
		t.Errorf("failed switch did not surface the error:\n%s", rec.Body.String())
	}
}
