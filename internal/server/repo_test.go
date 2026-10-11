package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if rec.Header().Get("HX-Redirect") != "/" {
		t.Errorf("switch did not redirect to the unscoped landing, got %q", rec.Header().Get("HX-Redirect"))
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

// TestSelectorNamesTheScopedRepo pins the deep-link's chrome: a `/?repo=<path>`
// landing renders the SCOPED repo's name in the header caption (and marks it
// active in the dropdown), not whatever the registry's persistent active repo
// happens to be. The body was already scoped (st-ga4); the caption lagged, so a
// status-line click landed on the right beads under the wrong project name —
// which reads as "the URL doesn't match what strand displays".
func TestSelectorNamesTheScopedRepo(t *testing.T) {
	reg := registry.InMemory(
		registry.Repo{Name: "alpha", Path: "/a"},
		registry.Repo{Name: "beta", Path: "/b"},
	)
	srv := serverFor(t, reg, map[string]IssueSource{"/a": &stubBD{}, "/b": &stubBD{}})

	head := headerCaption(t, do(t, srv, "/?repo=/b").Body.String())
	if head != "beta" {
		t.Errorf("header caption on a ?repo=/b landing = %q, want beta", head)
	}

	// The dropdown fragment carries the same scope (the page's inherited
	// hx-vals) and must mark the scoped row, not the registry default.
	menu := do(t, srv, "/repos?repo=/b").Body.String()
	i := strings.Index(menu, `class="rm-item active"`)
	if i < 0 {
		t.Fatalf("menu marked no row active:\n%s", menu)
	}
	activeRow := menu[i:]
	if !strings.Contains(activeRow[:min(len(activeRow), 200)], "beta") {
		t.Errorf("menu marked the wrong row active:\n%s", menu)
	}

	// An unscoped landing still names the registry default.
	if head := headerCaption(t, do(t, srv, "/").Body.String()); head != "alpha" {
		t.Errorf("unscoped header caption = %q, want alpha", head)
	}
}

// headerCaption pulls the repo-selector button's caption out of a rendered page.
func headerCaption(t *testing.T, body string) string {
	t.Helper()
	const anchor = `<button class="repo"`
	i := strings.Index(body, anchor)
	if i < 0 {
		t.Fatalf("no repo selector button in page:\n%s", body)
	}
	rest := body[i:]
	start := strings.Index(rest, "<span>")
	end := strings.Index(rest, "</span>")
	if start < 0 || end < start {
		t.Fatalf("malformed repo selector caption:\n%s", rest[:min(len(rest), 300)])
	}
	return rest[start+len("<span>") : end]
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
	if rec.Header().Get("HX-Redirect") != "" {
		t.Error("a failed switch still requested a reload")
	}
	if !strings.Contains(rec.Body.String(), "rm-err") {
		t.Errorf("failed switch did not surface the error:\n%s", rec.Body.String())
	}
}

// mkPrefixedRepo creates a beads workspace under root whose config.yaml sets
// issue-prefix, the shape bd init leaves behind.
func mkPrefixedRepo(t *testing.T, root, name, prefix string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(path, ".beads", "embeddeddolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("# Issue prefix for this repository\n# issue-prefix: \"\"\nissue-prefix: %q\n", prefix)
	if err := os.WriteFile(filepath.Join(path, ".beads", "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestBeadWithNoRepoOpensFromPrefixRepo: the registry's most recently used repo
// has lost its folder, and GET /bead/<id> with no ?repo= answers 200 from the
// repo whose issue-prefix matches the id, not from the default (st-mqj).
func TestBeadWithNoRepoOpensFromPrefixRepo(t *testing.T) {
	root := t.TempDir()
	sdloop := mkPrefixedRepo(t, root, "sdloop", "sd")
	strandDir := mkPrefixedRepo(t, root, "strand", "st")
	mnemd := mkPrefixedRepo(t, root, "mnemd", "mn")
	now := time.Now()
	file := filepath.Join(root, "repos.json")
	seed := fmt.Sprintf(`[{"name":"sdloop","path":%q,"last_used":%q},{"name":"strand","path":%q,"last_used":%q},{"name":"mnemd","path":%q,"last_used":%q}]`,
		sdloop, now.Format(time.RFC3339Nano),
		strandDir, now.Add(-time.Hour).Format(time.RFC3339Nano),
		mnemd, now.Add(-48*time.Hour).Format(time.RFC3339Nano))
	if err := os.WriteFile(file, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := registry.Open(file, "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// The folder goes away while strand runs, as sdloop's did.
	if err := os.RemoveAll(sdloop); err != nil {
		t.Fatal(err)
	}
	srv := serverFor(t, reg, map[string]IssueSource{
		sdloop:    &stubBD{showErr: fmt.Errorf("chdir %s: %w", sdloop, os.ErrNotExist)},
		strandDir: &stubBD{showErr: errNoIssue},
		mnemd:     &stubBD{show: map[string]*bd.Issue{"mn-x305": {ID: "mn-x305", Title: "Mnemd bead title", Status: "open"}}},
	})
	rec := do(t, srv, "/bead/mn-x305")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /bead/mn-x305 = %d, want 200:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Mnemd bead title") {
		t.Errorf("drawer missing mn-x305's title:\n%s", rec.Body.String())
	}

	// A browser visit lands on the board scoped to that repo, so the page's
	// drawer fetch (which carries the page's repo) reads the right workspace.
	req := httptest.NewRequest(http.MethodGet, "/bead/mn-x305", nil)
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	rec = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	want := "/?" + url.Values{"bead": {"mn-x305"}, "repo": {mnemd}}.Encode()
	if got := rec.Header().Get("Location"); got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
}
