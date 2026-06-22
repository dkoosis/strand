package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/forest"
	"github.com/dkoosis/strand/internal/registry"
	"github.com/dkoosis/strand/web"
)

// errMarshal stands in for a non-bd error (e.g. a template/marshal failure) —
// the kind statusForError must treat as our own 500.
var errMarshal = errors.New("json: unsupported type")

// stubBD is an in-memory issueSource so the handlers run without the bd CLI
// (spec Q0: fake the bd boundary, assert on the rendered HTML).
type stubBD struct {
	issues   []bd.Issue
	show     map[string]*bd.Issue
	comments map[string][]bd.Comment
	listErr  error
	showErr  error
	writeErr error // when set, every write fails with it; the show map stays put
}

func (s *stubBD) List(context.Context, ...string) ([]bd.Issue, error) {
	return s.issues, s.listErr
}

func (s *stubBD) Show(_ context.Context, id string) (*bd.Issue, error) {
	if s.showErr != nil {
		return nil, s.showErr
	}
	return s.show[id], nil
}

func (s *stubBD) Comments(_ context.Context, id string) ([]bd.Comment, error) {
	return s.comments[id], nil
}

// Comment appends to the in-memory thread so the drawer's re-read shows it; an
// empty text fails like bd's validation does, and writeErr models a bd outage.
func (s *stubBD) Comment(_ context.Context, id, text string) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	if text == "" {
		return bd.ErrEmptyText
	}
	if s.comments == nil {
		s.comments = map[string][]bd.Comment{}
	}
	s.comments[id] = append(s.comments[id], bd.Comment{IssueID: id, Author: "me", Text: text})
	return nil
}

// Create mints a bead with a fixed id and stores it so the post-create drawer
// re-read finds it; an empty title fails like bd's validation does.
func (s *stubBD) Create(_ context.Context, opts bd.CreateOpts) (*bd.Issue, error) {
	if s.writeErr != nil {
		return nil, s.writeErr
	}
	if opts.Title == "" {
		return nil, bd.ErrEmptyTitle
	}
	iss := &bd.Issue{ID: "demo-new", Title: opts.Title, IssueType: opts.Type, Status: "open"}
	if opts.Priority != nil {
		iss.Priority = *opts.Priority
	}
	if s.show == nil {
		s.show = map[string]*bd.Issue{}
	}
	s.show[iss.ID] = iss
	return iss, nil
}

func (s *stubBD) DeletePreview(_ context.Context, id string) (string, error) {
	if s.showErr != nil {
		return "", s.showErr
	}
	return "DELETE PREVIEW\n  " + id, nil
}

func (s *stubBD) Delete(_ context.Context, id string) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	delete(s.show, id)
	return nil
}

// The write methods mutate the in-memory show map so the handler's re-read
// reflects the change — exercising the honest-write contract (Q2). When writeErr
// is set the map is left untouched, modelling a bd failure: the re-read still
// returns the old value and the handler must surface the error, not the change.

func (s *stubBD) Update(_ context.Context, id, field, value string) (*bd.Issue, error) {
	if s.writeErr != nil {
		return nil, s.writeErr
	}
	iss := s.show[id]
	switch field {
	case "title":
		iss.Title = value
	case "priority":
		iss.Priority, _ = strconv.Atoi(value)
	case "assignee":
		iss.Assignee = value
	case "description":
		iss.Description = value
	case "status":
		iss.Status = value
	}
	return iss, nil
}

func (s *stubBD) Claim(_ context.Context, id string) (*bd.Issue, error) {
	if s.writeErr != nil {
		return nil, s.writeErr
	}
	s.show[id].Assignee = "me"
	return s.show[id], nil
}

func (s *stubBD) Close(_ context.Context, id, _ string) (*bd.Issue, error) {
	if s.writeErr != nil {
		return nil, s.writeErr
	}
	s.show[id].Status = "closed"
	return s.show[id], nil
}

// demoRepo is the active repo every test server scopes to; its name labels the
// forest region the way the active repo's name does in production.
var demoRepo = registry.Repo{Name: "demo", Path: "/demo"}

// newTestServer wires a server whose only repo is demoRepo, so srcFor always
// hands back the one stub regardless of the (single) active repo.
func newTestServer(t *testing.T, src IssueSource) *Server {
	t.Helper()
	tmpl, err := web.Templates()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	reg := registry.InMemory(demoRepo)
	srcFor := func(registry.Repo) IssueSource { return src }
	return New(srcFor, reg, tmpl, web.Static(), forest.Synthesis{NorthStar: "remember across sessions"})
}

func do(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// send issues a write request with a form body, like htmx submits one.
func send(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.Routes().ServeHTTP(rec, req)
	return rec
}

// oneBead is a stub holding a single editable bead for the write-path tests.
func oneBead(iss *bd.Issue) *stubBD {
	return &stubBD{show: map[string]*bd.Issue{iss.ID: iss}}
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
	srv := newTestServer(t, &stubBD{issues: sampleIssues})
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
	srv := newTestServer(t, &stubBD{issues: sampleIssues})
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
	srv := newTestServer(t, &stubBD{show: map[string]*bd.Issue{
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
	srv := newTestServer(t, &stubBD{showErr: fmt.Errorf("bd show x: %w", bd.ErrNotFound)})
	rec := do(t, srv, "/bead/x")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /bead missing = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "error-fragment") {
		t.Error("404 missing error fragment")
	}
}

// TestEditFieldReflects: a field edit re-renders the drawer from a fresh read, so
// the new value shows because bd confirmed it — not because the UI guessed.
func TestEditFieldReflects(t *testing.T) {
	srv := newTestServer(t, oneBead(&bd.Issue{ID: "demo-x", Title: "Old name", Status: "open", IssueType: "task"}))
	rec := send(t, srv, http.MethodPatch, "/bead/demo-x", "field=title&value=New+name")

	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH /bead = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "New name") {
		t.Errorf("drawer missing the edited title:\n%s", body)
	}
	if strings.Contains(body, "Old name") {
		t.Error("drawer still shows the stale title")
	}
}

// TestClaimReflects: the claim button assigns the bead and the redrawn drawer
// shows the new assignee.
func TestClaimReflects(t *testing.T) {
	srv := newTestServer(t, oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task"}))
	rec := send(t, srv, http.MethodPost, "/bead/demo-x/claim", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("POST claim = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "me") {
		t.Errorf("drawer missing the assignee after claim:\n%s", rec.Body.String())
	}
}

// TestCloseReflects: closing flips the bead's status and the drawer shows it.
func TestCloseReflects(t *testing.T) {
	srv := newTestServer(t, oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task"}))
	rec := send(t, srv, http.MethodPost, "/bead/demo-x/close", "reason=done")

	if rec.Code != http.StatusOK {
		t.Fatalf("POST close = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "dot closed") {
		t.Errorf("drawer does not show the closed status:\n%s", rec.Body.String())
	}
}

// TestReopenReflects: reopening a closed bead routes through Update(status,open)
// and the drawer shows it open again.
func TestReopenReflects(t *testing.T) {
	srv := newTestServer(t, oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "closed", IssueType: "task"}))
	rec := send(t, srv, http.MethodPost, "/bead/demo-x/reopen", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("POST reopen = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "dot open") {
		t.Errorf("drawer does not show the reopened status:\n%s", rec.Body.String())
	}
}

// TestWriteErrorIsHonest: when bd rejects a write, the UI shows bd's message and
// keeps the unchanged value — it never claims a change that didn't land (Q2).
func TestWriteErrorIsHonest(t *testing.T) {
	stub := oneBead(&bd.Issue{ID: "demo-x", Title: "Keep me", Status: "open", IssueType: "task"})
	stub.writeErr = fmt.Errorf("bd update demo-x: %w", bd.ErrBD)
	srv := newTestServer(t, stub)
	rec := send(t, srv, http.MethodPatch, "/bead/demo-x", "field=title&value=Lost+edit")

	body := rec.Body.String()
	if !strings.Contains(body, "Keep me") {
		t.Errorf("drawer dropped the unchanged value on a failed write:\n%s", body)
	}
	if strings.Contains(body, "Lost edit") {
		t.Error("drawer shows an edit bd never accepted")
	}
	if !strings.Contains(body, bd.ErrBD.Error()) {
		t.Errorf("drawer hides bd's error message:\n%s", body)
	}
}

// TestDrawerShowsComments: the drawer renders the issue's comment thread.
func TestDrawerShowsComments(t *testing.T) {
	stub := oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task"})
	stub.comments = map[string][]bd.Comment{"demo-x": {{Author: "ada", Text: "first note"}}}
	srv := newTestServer(t, stub)
	rec := do(t, srv, "/bead/demo-x")

	body := rec.Body.String()
	if !strings.Contains(body, "ada") || !strings.Contains(body, "first note") {
		t.Errorf("drawer missing the comment:\n%s", body)
	}
}

// TestAddCommentReflects: a posted comment shows on the redrawn drawer, because
// the re-read picked it up — the honest-write contract, applied to comments.
func TestAddCommentReflects(t *testing.T) {
	srv := newTestServer(t, oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task"}))
	rec := send(t, srv, http.MethodPost, "/bead/demo-x/comment", "text=looks+good")

	if rec.Code != http.StatusOK {
		t.Fatalf("POST comment = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "looks good") {
		t.Errorf("drawer missing the new comment:\n%s", rec.Body.String())
	}
}

// TestAddEmptyCommentIsHonest: an empty comment surfaces bd's error and the
// drawer still shows the bead, never claiming a comment that wasn't recorded.
func TestAddEmptyCommentIsHonest(t *testing.T) {
	srv := newTestServer(t, oneBead(&bd.Issue{ID: "demo-x", Title: "Keep me", Status: "open", IssueType: "task"}))
	rec := send(t, srv, http.MethodPost, "/bead/demo-x/comment", "text=")

	body := rec.Body.String()
	if !strings.Contains(body, "Keep me") {
		t.Errorf("drawer dropped the bead on a failed comment:\n%s", body)
	}
	if !strings.Contains(body, bd.ErrEmptyText.Error()) {
		t.Errorf("drawer hides the empty-comment error:\n%s", body)
	}
}

// TestCreateFormRenders: the new-bead form opens with the type options.
func TestCreateFormRenders(t *testing.T) {
	srv := newTestServer(t, &stubBD{})
	rec := do(t, srv, "/new")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /new = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `hx-post="/new"`) {
		t.Errorf("create form missing its post target:\n%s", rec.Body.String())
	}
}

// TestCreateReflects: a valid create shows the new bead's drawer and fires
// refreshList so the list pane picks up the addition.
func TestCreateReflects(t *testing.T) {
	srv := newTestServer(t, &stubBD{show: map[string]*bd.Issue{}})
	rec := send(t, srv, http.MethodPost, "/new", "title=Fresh+bead&type=task&priority=2")

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /new = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Fresh bead") {
		t.Errorf("drawer missing the created bead:\n%s", rec.Body.String())
	}
	if rec.Header().Get("HX-Trigger") != "refreshList" {
		t.Errorf("create did not fire refreshList, got %q", rec.Header().Get("HX-Trigger"))
	}
}

// TestCreateEmptyTitleIsHonest: a titleless create re-renders the form with bd's
// error, not a half-made bead.
func TestCreateEmptyTitleIsHonest(t *testing.T) {
	srv := newTestServer(t, &stubBD{})
	rec := send(t, srv, http.MethodPost, "/new", "title=&type=task&priority=2")

	body := rec.Body.String()
	if !strings.Contains(body, `hx-post="/new"`) {
		t.Errorf("failed create did not re-render the form:\n%s", body)
	}
	if !strings.Contains(body, bd.ErrEmptyTitle.Error()) {
		t.Errorf("form hides the empty-title error:\n%s", body)
	}
}

// TestDeletePreviewConfirms: the delete button shows bd's preview and a confirm
// control, and destroys nothing yet.
func TestDeletePreviewConfirms(t *testing.T) {
	srv := newTestServer(t, oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task"}))
	rec := send(t, srv, http.MethodPost, "/bead/demo-x/delete", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("POST delete preview = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "DELETE PREVIEW") {
		t.Errorf("confirm panel missing bd's preview:\n%s", body)
	}
	if !strings.Contains(body, `hx-delete="/bead/demo-x"`) {
		t.Errorf("confirm panel missing the commit control:\n%s", body)
	}
}

// TestDeleteRemoves: confirming the delete commits it and fires refreshList so
// the gone bead leaves the list.
func TestDeleteRemoves(t *testing.T) {
	stub := oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task"})
	srv := newTestServer(t, stub)
	rec := send(t, srv, http.MethodDelete, "/bead/demo-x", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /bead = %d, want 200", rec.Code)
	}
	if rec.Header().Get("HX-Trigger") != "refreshList" {
		t.Errorf("delete did not fire refreshList, got %q", rec.Header().Get("HX-Trigger"))
	}
	if _, ok := stub.show["demo-x"]; ok {
		t.Error("bead survived a confirmed delete")
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
