package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/forest"
	"github.com/dkoosis/strand/web"
)

// errMarshal stands in for a non-bd error (e.g. a template/marshal failure) —
// the kind statusForError must treat as our own 500.
var errMarshal = errors.New("json: unsupported type")

// stubBD is an in-memory issueSource so the handlers run without the bd CLI
// (spec Q0: fake the bd boundary, assert on the rendered HTML).
type stubBD struct {
	issues  []bd.Issue
	show    map[string]*bd.Issue
	listErr error
	showErr error
}

func (s stubBD) List(context.Context, ...string) ([]bd.Issue, error) {
	return s.issues, s.listErr
}

func (s stubBD) Show(_ context.Context, id string) (*bd.Issue, error) {
	if s.showErr != nil {
		return nil, s.showErr
	}
	return s.show[id], nil
}

func newTestServer(t *testing.T, src issueSource) *Server {
	t.Helper()
	tmpl, err := web.Templates()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	return New(src, tmpl, web.Static(), forest.Synthesis{Project: "demo", NorthStar: "remember across sessions"})
}

func do(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

var sampleIssues = []bd.Issue{
	{ID: "demo-e1", Title: "Forest epic", IssueType: "epic", Status: "open", Priority: 1},
	{ID: "demo-e1.a", Parent: "demo-e1", Title: "Wire the thing", Status: "open", Priority: 0},
	{ID: "demo-e1.b", Parent: "demo-e1", Title: "Test the thing", Status: "in_progress", Priority: 2},
	{ID: "demo-e2", Title: "Lone task", IssueType: "task", Status: "open", Priority: 3},
}

// TestForestPageRenders pins the landing: the page renders the north star, the
// treemap, and a sized tile per epic with htmx wiring to the list pane.
func TestForestPageRenders(t *testing.T) {
	srv := newTestServer(t, stubBD{issues: sampleIssues})
	rec := do(t, srv, "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"remember across sessions", // north star
		`class="treemap"`,
		`hx-get="/list?epic=demo-e1"`, // tile drills into its epic
		`hx-get="/list?epic=demo-e2"`,
		`class="flag"`, // demo-e1 holds P0/P1 work
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

// TestListFragmentNarrowsToEpic: the epic param scopes the bead-list pane to one
// tile's beads and excludes others.
func TestListFragmentNarrowsToEpic(t *testing.T) {
	srv := newTestServer(t, stubBD{issues: sampleIssues})
	rec := do(t, srv, "/list?epic=demo-e1")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /list = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Wire the thing") || !strings.Contains(body, "Test the thing") {
		t.Errorf("epic list missing its beads:\n%s", body)
	}
	if strings.Contains(body, "Lone task") {
		t.Error("epic list leaked a bead from another epic")
	}
	if !strings.Contains(body, `hx-get="/bead/demo-e1.a"`) {
		t.Error("bead row missing drawer wiring")
	}
}

// TestBeadDrawerRendersDetail: a bead drill renders the drawer with its title and
// description.
func TestBeadDrawerRendersDetail(t *testing.T) {
	srv := newTestServer(t, stubBD{show: map[string]*bd.Issue{
		"demo-e1.a": {ID: "demo-e1.a", Title: "Wire the thing", Status: "open", Priority: 0, IssueType: "task", Description: "do the wiring"},
	}})
	rec := do(t, srv, "/bead/demo-e1.a")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /bead = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Wire the thing") || !strings.Contains(body, "do the wiring") {
		t.Errorf("drawer missing detail:\n%s", body)
	}
}

// TestBeadNotFoundIs404: a missing bead surfaces the bd sentinel as a 404 with an
// error fragment, not a 200 with an empty drawer.
func TestBeadNotFoundIs404(t *testing.T) {
	srv := newTestServer(t, stubBD{showErr: fmt.Errorf("bd show x: %w", bd.ErrNotFound)})
	rec := do(t, srv, "/bead/x")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /bead missing = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "error-fragment") {
		t.Error("404 missing error fragment")
	}
}

// TestStatusForError pins the bd-error -> HTTP-status mapping. Wrapped errors must
// classify through errors.Is, as the bd client wraps them in the field.
func TestStatusForError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"not found", fmt.Errorf("bd show x-9: %w", bd.ErrNotFound), http.StatusNotFound},
		{"invalid arg", fmt.Errorf("bd list: %w", bd.ErrInvalidArg), http.StatusBadRequest},
		{"bd failure", fmt.Errorf("bd ready: %w", bd.ErrBD), http.StatusBadGateway},
		{"unclassified is internal", errMarshal, http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusForError(tt.err); got != tt.want {
				t.Errorf("statusForError(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}
