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

// TestHomeRepoDeepLinkScopesPerRequest pins st-ga4: a `/?repo=<path>` deep-link
// (the status line's OSC 8 link carrying its own repo) scopes THAT request's
// landing to the named repo without touching the persistent active repo — no
// registry write, and a follow-on request with no `?repo=` reads the untouched
// default, not whatever the deep-link named (the old, now-superseded, sticky
// st-vai behavior). A combined `?repo=&filter=` still applies the pulse cut,
// and every fragment carries its own scope the same way the page's hx-vals
// would (a bare fragment request with no param falls back to the default).
func TestHomeRepoDeepLinkScopesPerRequest(t *testing.T) {
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
	if active, ok := reg.Active(); !ok || active.Path != "/a" {
		t.Errorf("registry active = %+v, ok=%v; a scoping GET must not re-point the default", active, ok)
	}

	// Not sticky: a follow-on fragment with no ?repo= reads the untouched
	// default (alpha), not the deep-link's beta.
	if lb := do(t, srv, "/list").Body.String(); !strings.Contains(lb, "Alpha work") || strings.Contains(lb, "Beta work") {
		t.Errorf("unscoped fragment should read the default (alpha), got:\n%s", lb)
	}

	// A fragment that DOES carry the scope (mirroring the page's inherited
	// hx-vals) resolves to the named repo.
	if lb := do(t, srv, "/list?repo=/b").Body.String(); !strings.Contains(lb, "Beta work") || strings.Contains(lb, "Alpha work") {
		t.Errorf("fragment carrying ?repo=/b not scoped to beta:\n%s", lb)
	}

	// An unknown, unregistered, no-.beads path is unresolved — the landing
	// renders the explicit empty state rather than silently falling back to
	// some other repo (no silent fallback, st-ga4).
	if eb := do(t, srv, "/?repo=/nope").Body.String(); !strings.Contains(eb, "No active beads workspace") {
		t.Errorf("unresolvable ?repo= should render the empty state, got:\n%s", eb)
	}

	// A combined ?repo=&filter= applies both: scope repo, then the pulse cut. A
	// trailing slash still resolves (filepath.Clean) — a hand-typed/bookmarked link.
	cb := do(t, srv, "/?repo=/a/&filter=open").Body.String()
	if !strings.Contains(cb, "Alpha work") || strings.Contains(cb, "Beta work") {
		t.Errorf("combined repo+filter deep-link (trailing slash) not scoped to alpha:\n%s", cb)
	}
	if !strings.Contains(cb, `data-filter="open"`) {
		t.Errorf("combined deep-link did not apply the pulse cut:\n%s", cb)
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
