package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	srv.openURL = func(_ context.Context, link string) error {
		*opened = append(*opened, link)
		return nil
	}
	return srv, opened
}

func TestNugLinkOpensObsidian(t *testing.T) {
	want := obsidianURL(testNugPath)
	tests := []struct {
		name   string
		target string
	}{
		{"by id", "/nug/cd3e1d8d4f05"},
		{"by path", "/nug?path=%2FUsers%2Fdk%2Fnugbases%2Fdk%2Freference%2Fa%20note.md"},
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
