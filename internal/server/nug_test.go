package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkoosis/strand/internal/bd"
)

const testNugPath = "/Users/dk/nugbases/dk/reference/a note.md"

// nugServer is a test server whose /nug seams record what would have opened and
// resolve one known id, so no test shells out to mnemd or open.
func nugServer(t *testing.T) (srv *Server, opened *[]string) {
	t.Helper()
	srv = newTestServer(t, &stubBD{})
	opened = &[]string{}
	srv.nugPath = func(_ context.Context, id string) (string, error) {
		if id == "cd3e1d8d4f05" {
			return testNugPath, nil
		}
		return "", fmt.Errorf("%w: no nug has id %q", bd.ErrNotFound, id)
	}
	srv.nugVaults = func() []string { return []string{"/Users/dk/nugbases/dk"} }
	srv.nugFind = func(context.Context, string) (string, error) {
		return "", fmt.Errorf("%w: no nug named", bd.ErrNotFound)
	}
	srv.openURL = func(_ context.Context, link string) error {
		*opened = append(*opened, link)
		return nil
	}
	return srv, opened
}

// writeNug writes a note with the frontmatter id and returns its path.
func writeNug(t *testing.T, dir, name, id string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("---\nid: "+id+"\nkind: reference\n---\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// openPath serves target and returns the status, body and the one URL opened.
func openPath(t *testing.T, srv *Server, opened *[]string, target string) (code int, body, link string) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if len(*opened) > 0 {
		link = (*opened)[0]
	}
	return rec.Code, rec.Body.String(), link
}

func TestNugLinkOpensObsidian(t *testing.T) {
	want := obsidianURL(testNugPath)
	tests := []struct {
		name   string
		target string
	}{
		{"by id", "/nug/cd3e1d8d4f05"},
		{"by path", "/nug?path=" + url.QueryEscape(writeNug(t, t.TempDir(), "a note.md", "cd3e1d8d4f05"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, opened := nugServer(t)
			rec := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.target, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
			}
			if len(*opened) != 1 || (*opened)[0] != want {
				t.Fatalf("opened = %v, want [%s]", *opened, want)
			}
			body := rec.Body.String()
			if !strings.Contains(body, "window.close()") {
				t.Errorf("page does not close its tab: %s", body)
			}
			if !strings.Contains(body, `href="obsidian://open?path=`) {
				t.Errorf("page has no obsidian link: %s", body)
			}
		})
	}
}

func TestNugLinkCrossSiteOpensNothing(t *testing.T) {
	srv, opened := nugServer(t)
	req := httptest.NewRequest(http.MethodGet, "/nug/cd3e1d8d4f05", nil)
	req.Header.Set(headerSecFetchSite, secFetchCrossSite)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(*opened) != 0 {
		t.Fatalf("cross-site request opened %v", *opened)
	}
	body := rec.Body.String()
	if strings.Contains(body, "window.close()") {
		t.Errorf("cross-site page closes its tab: %s", body)
	}
	if !strings.Contains(body, `href="obsidian://open?path=`) {
		t.Errorf("cross-site page has no link: %s", body)
	}
}

func TestNugLinkRejectsBadInput(t *testing.T) {
	tests := []struct {
		target string
		want   int
	}{
		{"/nug/not-an-id", http.StatusBadRequest},
		{"/nug/000000000000", http.StatusNotFound},
		{"/nug?path=relative.md", http.StatusBadRequest},
		{"/nug", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			srv, opened := nugServer(t)
			rec := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.target, nil))
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
			if len(*opened) != 0 {
				t.Errorf("opened %v on bad input", *opened)
			}
		})
	}
}

func TestObsidianURLEscapesSpaceAsPercent20(t *testing.T) {
	got := obsidianURL("/a b+c.md")
	want := "obsidian://open?path=%2Fa%20b%2Bc.md"
	if got != want {
		t.Errorf("obsidianURL = %q, want %q", got, want)
	}
}

// Obsidian matches its registered vault path case-sensitively; a link that
// spells the vault otherwise must still open (the "Vault not found" bug).
func TestNugLinkUsesVaultAsObsidianRegistersIt(t *testing.T) {
	base := t.TempDir()
	note := writeNug(t, filepath.Join(base, "Nugbases", "dk"), "reference/n.md", "aaaaaaaaaaaa")
	registered := filepath.Join(base, "nugbases", "dk")
	srv, opened := nugServer(t)
	srv.nugVaults = func() []string { return []string{registered} }
	srv.nugPath = func(context.Context, string) (string, error) { return note, nil }
	code, _, link := openPath(t, srv, opened, "/nug?path="+url.QueryEscape(note))
	want := obsidianURL(registered + "/reference/n.md")
	if code != http.StatusOK || link != want {
		t.Fatalf("status %d, opened %q, want %q", code, link, want)
	}
}

func TestRegisteredPathMatchesVaultWithoutCase(t *testing.T) {
	vaults := []string{"/Users/dk/nugbases/dk", "/tmp/other"}
	tests := []struct{ in, want string }{
		{"/Users/dk/Nugbases/dk/a.md", "/Users/dk/nugbases/dk/a.md"},
		{"/USERS/DK/NUGBASES/DK/x/a.md", "/Users/dk/nugbases/dk/x/a.md"},
		{"/Users/dk/nugbases/dkx/a.md", "/Users/dk/nugbases/dkx/a.md"},
		{"/elsewhere/a.md", "/elsewhere/a.md"},
	}
	for _, tt := range tests {
		if got := registeredPath(tt.in, vaults); got != tt.want {
			t.Errorf("registeredPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// A link to a path mnemd move vacated opens the nug at its new path.
func TestNugLinkStalePathFollowsMove(t *testing.T) {
	vault := t.TempDir()
	moved := writeNug(t, vault, "projects/new/n.md", "bbbbbbbbbbbb")
	ledger := `{"verb":"correct","id":"bbbbbbbbbbbb","path":"old/n.md"}
{"verb":"move","id":"bbbbbbbbbbbb","path":"projects/new/n.md","from":"old/n.md"}
`
	if err := os.MkdirAll(filepath.Join(vault, ".mnemd"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vault, ".mnemd", "mnemd-write-ledger.jsonl"), []byte(ledger), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, opened := nugServer(t)
	srv.nugVaults = func() []string { return []string{vault} }
	srv.nugPath = func(_ context.Context, id string) (string, error) {
		if id == "bbbbbbbbbbbb" {
			return moved, nil
		}
		return "", fmt.Errorf("%w: no nug has id %q", bd.ErrNotFound, id)
	}
	code, _, link := openPath(t, srv, opened, "/nug?path="+url.QueryEscape(filepath.Join(vault, "old", "n.md")))
	if want := obsidianURL(moved); code != http.StatusOK || link != want {
		t.Fatalf("status %d, opened %q, want %q", code, link, want)
	}
}

// With no ledger line, the file name finds the nug.
func TestNugLinkStalePathFindsByName(t *testing.T) {
	srv, opened := nugServer(t)
	srv.nugFind = func(_ context.Context, name string) (string, error) {
		if name == "gone.md" {
			return testNugPath, nil
		}
		return "", fmt.Errorf("%w: no nug named %q", bd.ErrNotFound, name)
	}
	code, _, link := openPath(t, srv, opened, "/nug?path="+url.QueryEscape("/Users/dk/nugbases/dk/old/gone.md"))
	if want := obsidianURL(testNugPath); code != http.StatusOK || link != want {
		t.Fatalf("status %d, opened %q, want %q", code, link, want)
	}
}

// A path no nug answers to gets a page saying so, never an obsidian:// link.
func TestNugLinkNotFoundIsAPageNotARedirect(t *testing.T) {
	srv, opened := nugServer(t)
	for _, target := range []string{
		"/nug?path=" + url.QueryEscape("/Users/dk/nugbases/dk/nowhere.md"),
		"/nug/000000000000",
	} {
		code, body, _ := openPath(t, srv, opened, target)
		if code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", target, code)
		}
		if !strings.Contains(body, "Nug not found") || strings.Contains(body, "obsidian://") {
			t.Errorf("%s: body = %s", target, body)
		}
	}
	if len(*opened) != 0 {
		t.Errorf("opened %v for a missing nug", *opened)
	}
}

func TestUniqueByName(t *testing.T) {
	tests := []struct {
		name  string
		paths []string
		file  string
		want  string
	}{
		{"one", []string{"/v/a/x.md", "/v/b/y.md"}, "x.md", "/v/a/x.md"},
		{"case", []string{"/v/a/X.md"}, "x.md", "/v/a/X.md"},
		{"two share a name", []string{"/v/a/x.md", "/v/b/x.md"}, "x.md", ""},
		{"none", []string{"/v/b/y.md"}, "x.md", ""},
		{"face page", []string{"/v/a/index.md"}, "index.md", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := uniqueByName(tt.paths, tt.file)
			if got != tt.want || ok != (tt.want != "") {
				t.Errorf("uniqueByName = %q, %v; want %q", got, ok, tt.want)
			}
		})
	}
}
