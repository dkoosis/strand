package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/forest"
	"github.com/dkoosis/strand/internal/graph"
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
	deps     []bd.DepEdge
	show     map[string]*bd.Issue
	comments map[string][]bd.Comment
	listErr  error
	showErr  error
	writeErr error // when set, every write fails with it; the show map stays put

	lastField string // the field/value of the most recent Update — lets the board
	lastValue string // move test assert it issued the right bd update.

	rankWrites []rankWrite // ordered log of SetRank calls, for the reorder tests.
}

// rankWrite records one SetRank call so a test can assert the handler issued the
// minimal write (one midpoint) or the full reseed (dense 1..N).
type rankWrite struct {
	id   string
	rank float64
}

func (s *stubBD) List(context.Context, ...string) ([]bd.Issue, error) {
	return s.issues, s.listErr
}

func (s *stubBD) Deps(context.Context, ...string) ([]bd.DepEdge, error) {
	return s.deps, s.listErr
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
	s.lastField, s.lastValue = field, value
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

// SetRank logs the write and reflects it in the issue list so a follow-up
// buildForest re-read sees the new rank — mirroring bd's metadata round-trip.
// writeErr models a bd outage: the list is left untouched and the handler must
// surface the error rather than report a phantom reorder.
func (s *stubBD) SetRank(_ context.Context, id string, rank float64) (*bd.Issue, error) {
	if s.writeErr != nil {
		return nil, s.writeErr
	}
	s.rankWrites = append(s.rankWrites, rankWrite{id, rank})
	for i := range s.issues {
		if s.issues[i].ID == id {
			if s.issues[i].Metadata == nil {
				s.issues[i].Metadata = map[string]any{}
			}
			s.issues[i].Metadata["rank"] = rank
		}
	}
	return nil, nil //nolint:nilnil // bd may answer silently; the handler re-reads, not this value.
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

// TestBoardRendersColumns: the kanban defaults to a status pivot, drawing the
// canonical columns as drop targets and a draggable card per bead, wired for the
// move POST and the drawer drill.
func TestBoardRendersColumns(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: sampleIssues})
	rec := do(t, srv, "/board")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /board = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`class="board"`,
		`data-pivot="status"`,
		`data-value="open"`,        // seeded column
		`data-value="in_progress"`, // demo-e1.b lives here
		`data-value="closed"`,      // empty drop target still rendered
		`class="bcard" data-id="demo-e1.a"`,
		"Wire the thing",
		`hx-get="/board?pivot=priority"`, // pivot bar switches the field
	} {
		if !strings.Contains(body, want) {
			t.Errorf("board missing %q", want)
		}
	}
}

// TestBoardPivotPriority: switching the pivot to priority lays the beads out in
// P0..P4 columns whose drop values are the bare priority numbers.
func TestBoardPivotPriority(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: sampleIssues})
	rec := do(t, srv, "/board?pivot=priority")

	body := rec.Body.String()
	if !strings.Contains(body, `data-pivot="priority"`) {
		t.Errorf("board did not pivot on priority:\n%s", body)
	}
	for _, want := range []string{`data-value="0"`, `data-value="2"`, `data-value="4"`} {
		if !strings.Contains(body, want) {
			t.Errorf("priority board missing column %q", want)
		}
	}
}

// TestBoardScopedToEpic: the epic param narrows the board to one epic's beads,
// like the table view does.
func TestBoardScopedToEpic(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: sampleIssues})
	rec := do(t, srv, "/board?epic=demo-e1")

	body := rec.Body.String()
	if !strings.Contains(body, "Wire the thing") || !strings.Contains(body, "Test the thing") {
		t.Errorf("epic board missing its beads:\n%s", body)
	}
	if strings.Contains(body, "Lone task") {
		t.Error("epic board leaked a bead from another epic")
	}
}

// TestBoardMoveUpdates: a column move issues the matching bd update and returns
// the refreshed card showing bd's truth (spec Q0).
func TestBoardMoveUpdates(t *testing.T) {
	stub := oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task"})
	srv := newTestServer(t, stub)
	rec := send(t, srv, http.MethodPost, "/bead/demo-x/move", "field=status&value=in_progress")

	if rec.Code != http.StatusOK {
		t.Fatalf("POST move = %d, want 200", rec.Code)
	}
	if stub.lastField != "status" || stub.lastValue != "in_progress" {
		t.Errorf("move issued update(%q,%q), want (status,in_progress)", stub.lastField, stub.lastValue)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="bcard"`) {
		t.Errorf("move did not return a card:\n%s", body)
	}
	if !strings.Contains(body, "dot in_progress") {
		t.Errorf("returned card does not show the new status:\n%s", body)
	}
}

// TestBoardMoveErrorReverts: when bd rejects the move, the handler returns a
// non-2xx so the client reverts the optimistic drop, with bd's message in the
// error fragment.
func TestBoardMoveErrorReverts(t *testing.T) {
	stub := oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task"})
	stub.writeErr = fmt.Errorf("bd update demo-x: %w", bd.ErrBD)
	srv := newTestServer(t, stub)
	rec := send(t, srv, http.MethodPost, "/bead/demo-x/move", "field=status&value=in_progress")

	if rec.Code == http.StatusOK {
		t.Fatalf("rejected move returned 200; client would not revert")
	}
	if !strings.Contains(rec.Body.String(), "error-fragment") {
		t.Errorf("rejected move missing the error fragment:\n%s", rec.Body.String())
	}
}

// rankedEpic is a fully-ranked epic group (root included — it's a member of its
// own list), the post-seed state where midpoint inserts apply. Ranks are 1..4 in
// id order so the rank order and the obvious reading order line up.
func rankedEpic() []bd.Issue {
	return []bd.Issue{
		{ID: "r-1", Title: "Ranked epic", IssueType: "epic", Status: "open", Priority: 1, Metadata: map[string]any{"rank": 1.0}},
		{ID: "r-1.a", Parent: "r-1", Title: "A", Status: "open", Priority: 0, Metadata: map[string]any{"rank": 2.0}},
		{ID: "r-1.b", Parent: "r-1", Title: "B", Status: "open", Priority: 2, Metadata: map[string]any{"rank": 3.0}},
		{ID: "r-1.c", Parent: "r-1", Title: "C", Status: "open", Priority: 2, Metadata: map[string]any{"rank": 4.0}},
	}
}

// rankOf returns the rank a SetRank call wrote for id, or NaN if the handler never
// touched it — so a test can assert both the value and that nothing else moved.
func rankOf(writes []rankWrite, id string) float64 {
	for _, w := range writes {
		if w.id == id {
			return w.rank
		}
	}
	return math.NaN()
}

// TestRankSeedsUntouchedGroup: the first drag on an epic with no manual rank yet
// seeds dense ranks 1..N over the post-drop order, turning the whole group ranked
// in one pass (the sortBeads invariant). Success is 204 — the client keeps its
// optimistic DOM.
func TestRankSeedsUntouchedGroup(t *testing.T) {
	stub := &stubBD{issues: append([]bd.Issue(nil), sampleIssues...)}
	srv := newTestServer(t, stub)
	// demo-e1 group is {demo-e1, demo-e1.a, demo-e1.b}; drop b to the front.
	rec := send(t, srv, http.MethodPost, "/bead/demo-e1.b/rank",
		"order=demo-e1.b,demo-e1.a,demo-e1")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("seed reorder = %d, want 204", rec.Code)
	}
	want := []rankWrite{{"demo-e1.b", 1}, {"demo-e1.a", 2}, {"demo-e1", 3}}
	if len(stub.rankWrites) != len(want) {
		t.Fatalf("seed wrote %v, want dense 1..3 over the order", stub.rankWrites)
	}
	for i, w := range want {
		if stub.rankWrites[i] != w {
			t.Errorf("seed write %d = %+v, want %+v", i, stub.rankWrites[i], w)
		}
	}
}

// TestRankSeedSkipsAbsentID: an id in the posted order that the forest no longer
// yields (closed mid-drag) gets no rank write — only the live survivors are seeded,
// and they stay dense.
func TestRankSeedSkipsAbsentID(t *testing.T) {
	stub := &stubBD{issues: append([]bd.Issue(nil), sampleIssues...)}
	srv := newTestServer(t, stub)
	// "ghost" is not in the forest; the live demo-e1 group is the other three.
	rec := send(t, srv, http.MethodPost, "/bead/demo-e1.b/rank",
		"order=demo-e1.b,ghost,demo-e1.a,demo-e1")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("seed-with-absent reorder = %d, want 204", rec.Code)
	}
	if !math.IsNaN(rankOf(stub.rankWrites, "ghost")) {
		t.Errorf("wrote a rank to the absent id: %v", stub.rankWrites)
	}
	want := []rankWrite{{"demo-e1.b", 1}, {"demo-e1.a", 2}, {"demo-e1", 3}}
	if len(stub.rankWrites) != len(want) {
		t.Fatalf("seed wrote %v, want dense 1..3 over live ids only", stub.rankWrites)
	}
	for i, w := range want {
		if stub.rankWrites[i] != w {
			t.Errorf("seed write %d = %+v, want %+v", i, stub.rankWrites[i], w)
		}
	}
}

// TestRankMidpointInsert: a single move inside an already-ranked group writes one
// bead to the midpoint of its new neighbors — not a full reseed.
func TestRankMidpointInsert(t *testing.T) {
	stub := &stubBD{issues: rankedEpic()}
	srv := newTestServer(t, stub)
	// Move r-1.c between r-1 (rank 1) and r-1.a (rank 2): midpoint 1.5.
	rec := send(t, srv, http.MethodPost, "/bead/r-1.c/rank",
		"order=r-1,r-1.c,r-1.a,r-1.b")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("midpoint reorder = %d, want 204", rec.Code)
	}
	if len(stub.rankWrites) != 1 {
		t.Fatalf("midpoint wrote %v, want exactly one", stub.rankWrites)
	}
	if got := rankOf(stub.rankWrites, "r-1.c"); got != 1.5 {
		t.Errorf("moved bead rank = %v, want 1.5", got)
	}
}

// TestRankHeadAndTailEdges: dropping a bead at either end ranks it one step past
// the edge it now leads or trails, with no priority floor to collide with.
func TestRankHeadAndTailEdges(t *testing.T) {
	t.Run("head", func(t *testing.T) {
		stub := &stubBD{issues: rankedEpic()}
		srv := newTestServer(t, stub)
		// r-1.c to the front: just below r-1 (rank 1) → 0.
		rec := send(t, srv, http.MethodPost, "/bead/r-1.c/rank",
			"order=r-1.c,r-1,r-1.a,r-1.b")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("head reorder = %d, want 204", rec.Code)
		}
		if len(stub.rankWrites) != 1 || rankOf(stub.rankWrites, "r-1.c") != 0 {
			t.Errorf("head writes = %v, want one {r-1.c 0}", stub.rankWrites)
		}
	})
	t.Run("tail", func(t *testing.T) {
		stub := &stubBD{issues: rankedEpic()}
		srv := newTestServer(t, stub)
		// r-1 to the back: just above r-1.c (rank 4) → 5.
		rec := send(t, srv, http.MethodPost, "/bead/r-1/rank",
			"order=r-1.a,r-1.b,r-1.c,r-1")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("tail reorder = %d, want 204", rec.Code)
		}
		if len(stub.rankWrites) != 1 || rankOf(stub.rankWrites, "r-1") != 5 {
			t.Errorf("tail writes = %v, want one {r-1 5}", stub.rankWrites)
		}
	})
}

// TestRankRenormalizesOnExhaustion: when the new neighbors sit on adjacent floats,
// no midpoint exists, so the handler reseeds the whole group dense instead of
// writing a colliding rank.
func TestRankRenormalizesOnExhaustion(t *testing.T) {
	tight := rankedEpic()
	tight[1].Metadata["rank"] = math.Nextafter(1, 2) // r-1.a one ulp above r-1
	stub := &stubBD{issues: tight}
	srv := newTestServer(t, stub)
	// Drop r-1.c between r-1 (1) and r-1.a (1+ulp): the midpoint rounds back to 1.
	rec := send(t, srv, http.MethodPost, "/bead/r-1.c/rank",
		"order=r-1,r-1.c,r-1.a,r-1.b")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("renorm reorder = %d, want 204", rec.Code)
	}
	if len(stub.rankWrites) != 4 {
		t.Fatalf("exhausted midpoint wrote %v, want a full 4-bead reseed", stub.rankWrites)
	}
	for i, id := range []string{"r-1", "r-1.c", "r-1.a", "r-1.b"} {
		if stub.rankWrites[i] != (rankWrite{id, float64(i + 1)}) {
			t.Errorf("reseed %d = %+v, want {%s %d}", i, stub.rankWrites[i], id, i+1)
		}
	}
}

// TestRankErrorReverts: a bd write failure returns non-2xx with the error
// fragment, the client's signal to revert the optimistic drag.
func TestRankErrorReverts(t *testing.T) {
	stub := &stubBD{issues: rankedEpic()}
	stub.writeErr = fmt.Errorf("bd update r-1.c: %w", bd.ErrBD)
	srv := newTestServer(t, stub)
	rec := send(t, srv, http.MethodPost, "/bead/r-1.c/rank",
		"order=r-1,r-1.c,r-1.a,r-1.b")

	if rec.Code == http.StatusNoContent {
		t.Fatal("rejected reorder returned 204; client would not revert")
	}
	if !strings.Contains(rec.Body.String(), "error-fragment") {
		t.Errorf("rejected reorder missing the error fragment:\n%s", rec.Body.String())
	}
}

// TestRankSingleIDNoOp: an order of one id has nothing to reorder, so the handler
// short-circuits to 204 without touching bd.
func TestRankSingleIDNoOp(t *testing.T) {
	stub := &stubBD{issues: rankedEpic()}
	srv := newTestServer(t, stub)
	rec := send(t, srv, http.MethodPost, "/bead/r-1.a/rank", "order=r-1.a")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("single-id reorder = %d, want 204", rec.Code)
	}
	if len(stub.rankWrites) != 0 {
		t.Errorf("single-id reorder wrote %v, want no bd calls", stub.rankWrites)
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

// --- exit button / graceful shutdown (strand-4fj) ---

// TestShutdownRoute: POST /shutdown answers 200 with the stopped fragment and
// fires the shutdown hook exactly once. The hook is stubbed so the real SIGTERM
// never reaches the test process.
func TestShutdownRoute(t *testing.T) {
	srv := newTestServer(t, &stubBD{})
	called := 0
	srv.shutdown = func() { called++ }

	rec := send(t, srv, http.MethodPost, "/shutdown", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /shutdown = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "strand stopped") {
		t.Errorf("shutdown fragment missing stopped message:\n%s", body)
	}
	if called != 1 {
		t.Errorf("shutdown hook fired %d times, want 1", called)
	}
}

// --- cross-site write guard (strand-a7w) ---

// sendWithHeaders issues a write request with extra headers, modelling what a
// browser (Sec-Fetch-Site / Origin) versus a CLI client sends.
func sendWithHeaders(t *testing.T, srv *Server, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	srv.Routes().ServeHTTP(rec, req)
	return rec
}

// TestGuardRejectsCrossSitePost: a browser form POSTing from another site
// (Sec-Fetch-Site: cross-site) is rejected at 403 with the error fragment — the
// write handler never runs, so no bead changes.
func TestGuardRejectsCrossSitePost(t *testing.T) {
	stub := oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task"})
	srv := newTestServer(t, stub)
	rec := sendWithHeaders(t, srv, http.MethodPost, "/bead/demo-x/claim",
		map[string]string{"Sec-Fetch-Site": "cross-site"})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "error-fragment") {
		t.Errorf("rejected request missing the error fragment:\n%s", rec.Body.String())
	}
	if stub.show["demo-x"].Assignee != "" {
		t.Error("cross-site POST reached the write handler")
	}
}

// TestGuardRejectsCrossSiteDelete: delete is the worst vector (data loss), so it
// must be guarded too — a cross-site DELETE is rejected and the bead survives.
func TestGuardRejectsCrossSiteDelete(t *testing.T) {
	stub := oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task"})
	srv := newTestServer(t, stub)
	rec := sendWithHeaders(t, srv, http.MethodDelete, "/bead/demo-x",
		map[string]string{"Sec-Fetch-Site": "cross-site"})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site DELETE = %d, want 403", rec.Code)
	}
	if _, ok := stub.show["demo-x"]; !ok {
		t.Error("cross-site DELETE destroyed the bead")
	}
}

// TestGuardRejectsForeignOrigin: an older browser (no Sec-Fetch-Site) sending an
// Origin whose host differs from the request Host is cross-site, so rejected.
func TestGuardRejectsForeignOrigin(t *testing.T) {
	srv := newTestServer(t, oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task"}))
	rec := sendWithHeaders(t, srv, http.MethodPost, "/bead/demo-x/claim",
		map[string]string{"Origin": "http://evil.example"})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign-origin POST = %d, want 403", rec.Code)
	}
}

// TestGuardAllowsSameOrigin: a same-origin browser request (Sec-Fetch-Site:
// same-origin) passes through to the handler.
func TestGuardAllowsSameOrigin(t *testing.T) {
	srv := newTestServer(t, oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task"}))
	rec := sendWithHeaders(t, srv, http.MethodPost, "/bead/demo-x/claim",
		map[string]string{"Sec-Fetch-Site": "same-origin"})

	if rec.Code != http.StatusOK {
		t.Fatalf("same-origin POST = %d, want 200", rec.Code)
	}
}

// TestGuardAllowsMatchingOrigin: an Origin whose host equals the request Host
// (httptest defaults Host to example.com) is same-origin, so it passes.
func TestGuardAllowsMatchingOrigin(t *testing.T) {
	srv := newTestServer(t, oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task"}))
	rec := sendWithHeaders(t, srv, http.MethodPost, "/bead/demo-x/claim",
		map[string]string{"Origin": "http://example.com"})

	if rec.Code != http.StatusOK {
		t.Fatalf("matching-origin POST = %d, want 200", rec.Code)
	}
}

// TestGuardAllowsCaseDifferingOrigin: hostnames are case-insensitive (RFC 3986),
// so an Origin host differing only in case still counts as same-origin.
func TestGuardAllowsCaseDifferingOrigin(t *testing.T) {
	srv := newTestServer(t, oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task"}))
	rec := sendWithHeaders(t, srv, http.MethodPost, "/bead/demo-x/claim",
		map[string]string{"Origin": "http://EXAMPLE.com"})

	if rec.Code != http.StatusOK {
		t.Fatalf("case-differing-origin POST = %d, want 200", rec.Code)
	}
}

// TestGuardAllowsNoHeaders: a request with neither Sec-Fetch-Site nor Origin (a
// CLI client like curl, or same-origin htmx that omits both) is allowed — the
// guard blocks cross-site browser forms, not local tooling.
func TestGuardAllowsNoHeaders(t *testing.T) {
	srv := newTestServer(t, oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task"}))
	rec := send(t, srv, http.MethodPost, "/bead/demo-x/claim", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("no-header POST = %d, want 200", rec.Code)
	}
}

// TestGuardNeverGatesGET: read routes must never be gated — a GET carrying a
// cross-site signal still renders, so the guard can't break navigation.
func TestGuardNeverGatesGET(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: sampleIssues})
	rec := sendWithHeaders(t, srv, http.MethodGet, "/list",
		map[string]string{"Sec-Fetch-Site": "cross-site"})

	if rec.Code != http.StatusOK {
		t.Fatalf("cross-site GET /list = %d, want 200 (reads are never gated)", rec.Code)
	}
}

// --- V3 dependency-graph view (strand-alg.2) ---

// graphIssues is the fixture DAG for the graph view: epic demo-g holds a chain
// (g.4→g.2→g.1, the critical path) and a 2-node cycle (g.5↔g.6); epic demo-h
// holds one unrelated bead so the epic-scoping test has something to exclude.
var graphIssues = []bd.Issue{
	{ID: "demo-g", Title: "Graph epic", IssueType: "epic", Status: "open", Priority: 1},
	{ID: "demo-g.1", Parent: "demo-g", Title: "Foundation", Status: "open", Priority: 1},
	{ID: "demo-g.2", Parent: "demo-g", Title: "Mid", Status: "open", Priority: 2},
	{ID: "demo-g.4", Parent: "demo-g", Title: "Leaf", Status: "in_progress", Priority: 2},
	{ID: "demo-g.5", Parent: "demo-g", Title: "CycleA", Status: "open", Priority: 2},
	{ID: "demo-g.6", Parent: "demo-g", Title: "CycleB", Status: "open", Priority: 2},
	{ID: "demo-h", Title: "Other epic", IssueType: "epic", Status: "open", Priority: 2},
	{ID: "demo-h.1", Parent: "demo-h", Title: "Elsewhere", Status: "open", Priority: 2},
}

// graphDeps mixes the kept "blocks" edges with noise the model must drop: a
// parent-child edge (wrong type) and a blocks edge to an out-of-scope bead.
var graphDeps = []bd.DepEdge{
	{IssueID: "demo-g.2", DependsOnID: "demo-g.1", Type: "blocks"},
	{IssueID: "demo-g.4", DependsOnID: "demo-g.2", Type: "blocks"},
	{IssueID: "demo-g.5", DependsOnID: "demo-g.6", Type: "blocks"},
	{IssueID: "demo-g.6", DependsOnID: "demo-g.5", Type: "blocks"},
	{IssueID: "demo-g.1", DependsOnID: "demo-g", Type: "parent-child"}, // dropped: not a blocks edge
	{IssueID: "demo-g.1", DependsOnID: "demo-out", Type: "blocks"},     // dropped: endpoint out of scope
}

var dataGraphRe = regexp.MustCompile(`data-graph="([^"]*)"`)

// graphModelFromHTML pulls the serialized graph model back out of the rendered
// fragment's data-graph attribute, so the test asserts on the exact node/edge
// data the client will draw — not on pixels (spec Q0).
func graphModelFromHTML(t *testing.T, body string) graphData {
	t.Helper()
	m := dataGraphRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no data-graph attribute in fragment:\n%s", body)
	}
	var gd graphData
	if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &gd); err != nil {
		t.Fatalf("data-graph is not valid JSON (%v): %s", err, m[1])
	}
	return gd
}

func nodeIDs(gd graphData) map[string]graphNode {
	m := make(map[string]graphNode, len(gd.Nodes))
	for _, n := range gd.Nodes {
		m[n.ID] = n
	}
	return m
}

// TestGraphFragmentRenders: the whole-region graph view returns a node per
// in-scope bead and one edge per kept "blocks" dependency, with the view-toggle
// and a Cytoscape mount point.
func TestGraphFragmentRenders(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: graphIssues, deps: graphDeps})
	rec := do(t, srv, "/graph")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /graph = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="cy"`,         // Cytoscape mounts here
		`hx-get="/list"`,  // view-toggle back to Table
		`hx-get="/board"`, // and Board
	} {
		if !strings.Contains(body, want) {
			t.Errorf("graph fragment missing %q", want)
		}
	}

	gd := graphModelFromHTML(t, body)
	nodes := nodeIDs(gd)
	for _, id := range []string{"demo-g.1", "demo-g.2", "demo-g.4", "demo-g.5", "demo-g.6", "demo-h.1"} {
		if _, ok := nodes[id]; !ok {
			t.Errorf("graph model missing node %q", id)
		}
	}
	// Four kept blocks edges; the parent-child and out-of-scope edges are gone.
	if len(gd.Edges) != 4 {
		t.Errorf("got %d edges, want 4 (blocks only, in-scope): %+v", len(gd.Edges), gd.Edges)
	}
	for _, e := range gd.Edges {
		if e.Source == "demo-g.1" { // its only out-edges were both droppable
			t.Errorf("kept a droppable edge from demo-g.1: %+v", e)
		}
	}
}

// TestGraphDropsOutOfScopeEdge: the load-bearing filter — a blocks edge to a bead
// outside the scope must not smuggle that bead in as a node (Deps' single-ID
// branch can synthesize such an edge).
func TestGraphDropsOutOfScopeEdge(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: graphIssues, deps: graphDeps})
	gd := graphModelFromHTML(t, do(t, srv, "/graph").Body.String())
	if _, leaked := nodeIDs(gd)["demo-out"]; leaked {
		t.Error("out-of-scope bead leaked into the graph as a node")
	}
}

// TestGraphScopedToEpic: the epic param narrows the graph to one epic's beads,
// like Table and Board.
func TestGraphScopedToEpic(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: graphIssues, deps: graphDeps})
	gd := graphModelFromHTML(t, do(t, srv, "/graph?epic=demo-g").Body.String())
	nodes := nodeIDs(gd)
	if _, ok := nodes["demo-g.1"]; !ok {
		t.Error("epic graph missing its own bead demo-g.1")
	}
	if _, leaked := nodes["demo-h.1"]; leaked {
		t.Error("epic graph leaked a bead from another epic")
	}
}

// TestGraphMetricFlags: the server-side metric wiring — the critical path is the
// g.4→g.2→g.1 chain, the cycle is {g.5,g.6}, and the foundational bead outranks a
// leaf on PageRank.
func TestGraphMetricFlags(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: graphIssues, deps: graphDeps})
	nodes := nodeIDs(graphModelFromHTML(t, do(t, srv, "/graph?epic=demo-g").Body.String()))

	onPath := map[string]bool{"demo-g.1": true, "demo-g.2": true, "demo-g.4": true}
	inCycle := map[string]bool{"demo-g.5": true, "demo-g.6": true}
	for id, n := range nodes {
		if n.OnPath != onPath[id] {
			t.Errorf("%s OnPath = %v, want %v", id, n.OnPath, onPath[id])
		}
		if n.InCycle != inCycle[id] {
			t.Errorf("%s InCycle = %v, want %v", id, n.InCycle, inCycle[id])
		}
	}
	if nodes["demo-g.1"].Score <= nodes["demo-g.4"].Score {
		t.Errorf("foundational g.1 (%v) should outrank leaf g.4 (%v) on PageRank",
			nodes["demo-g.1"].Score, nodes["demo-g.4"].Score)
	}
}

// TestGraphNoRepo: with no active repo the graph view degrades to the empty pane
// at 200, like Board.
func TestGraphNoRepo(t *testing.T) {
	tmpl, err := web.Templates()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	srv := New(func(registry.Repo) IssueSource { return &stubBD{} },
		registry.InMemory(), tmpl, web.Static(), forest.Synthesis{})
	rec := do(t, srv, "/graph")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /graph (no repo) = %d, want 200", rec.Code)
	}
}

// --- V4 insights (strand-alg.3) ---

// insightsNow is the fixed clock the insights tests pin Server.now to, so the stale
// cutoff is deterministic regardless of when the suite runs.
var insightsNow = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

var (
	insFresh = insightsNow.Add(-time.Hour)
	insStale = insightsNow.Add(-30 * 24 * time.Hour)
)

// insightsIssues is the fixture for the dashboard: epic demo-i with a 3-bead
// dependency chain (i.3→i.2→i.1, so i.1 is foundational), one in-progress bead, and
// one stale untagged bead. bd list omits closed, so no closed beads appear.
var insightsIssues = []bd.Issue{
	{ID: "demo-i", Title: "Insights epic", IssueType: "epic", Status: "open", Priority: 1, UpdatedAt: insFresh},
	{ID: "demo-i.1", Parent: "demo-i", Title: "Foundation", Status: "open", Priority: 1, Labels: []string{"core"}, UpdatedAt: insFresh},
	{ID: "demo-i.2", Parent: "demo-i", Title: "Mid", Status: "open", Priority: 2, Labels: []string{"core", "ui"}, UpdatedAt: insFresh},
	{ID: "demo-i.3", Parent: "demo-i", Title: "Leaf", Status: "open", Priority: 2, Labels: []string{"ui"}, UpdatedAt: insFresh},
	{ID: "demo-i.4", Parent: "demo-i", Title: "Active", Status: "in_progress", Priority: 2, Labels: []string{"core"}, UpdatedAt: insFresh},
	{ID: "demo-i.5", Parent: "demo-i", Title: "Stale", Status: "open", Priority: 3, UpdatedAt: insStale},
}

var insightsDeps = []bd.DepEdge{
	{IssueID: "demo-i.2", DependsOnID: "demo-i.1", Type: "blocks"},
	{IssueID: "demo-i.3", DependsOnID: "demo-i.2", Type: "blocks"},
}

// insScope returns the demo-i epic's beads and the full-repo issue index, the two
// inputs the pure insight helpers take.
func insScope(t *testing.T) ([]forest.Bead, map[string]bd.Issue) {
	t.Helper()
	f := forest.Build(insightsIssues, forest.Synthesis{Project: "demo"})
	view := listViewFor(f, "demo-i")
	if !view.HasEpic {
		t.Fatal("fixture epic demo-i not found in forest")
	}
	// mirror insightsModel: the dashboard reasons over actionable work, not the
	// epic container the forest folds into the scope.
	return actionable(view.Epic.Beads), indexIssues(insightsIssues)
}

// TestTriageCounts pins the queue-shape math: ready/blocked weigh all blockers,
// in-progress and stale are split out, and Total counts only live beads.
func TestTriageCounts(t *testing.T) {
	beads, idx := insScope(t)
	got := triage(beads, insightsDeps, idx, insightsNow)
	want := triageCounts{Total: 5, Open: 4, InProgress: 1, Ready: 2, Blocked: 2, Stale: 1}
	if got != want {
		t.Errorf("triage = %+v, want %+v", got, want)
	}
}

// TestTriageAbsentBlockerIsResolved: a blocks-dep whose target isn't in the live
// list (bd omits closed) must not keep the bead out of ready.
func TestTriageAbsentBlockerIsResolved(t *testing.T) {
	beads, idx := insScope(t)
	deps := append(append([]bd.DepEdge(nil), insightsDeps...),
		bd.DepEdge{IssueID: "demo-i.1", DependsOnID: "demo-gone", Type: "blocks"})
	got := triage(beads, deps, idx, insightsNow)
	if got.Ready != 2 || got.Blocked != 2 {
		t.Errorf("absent blocker changed triage: ready=%d blocked=%d, want 2/2", got.Ready, got.Blocked)
	}
}

// TestTriageExplicitlyBlocked: a bead bd reports with status "blocked" (not just
// dependency-blocked) lands in Blocked, not lost between the open/in-progress cases.
func TestTriageExplicitlyBlocked(t *testing.T) {
	beads := []forest.Bead{{ID: "b1", Status: statusBlocked}}
	idx := map[string]bd.Issue{"b1": {ID: "b1", Status: statusBlocked}}
	got := triage(beads, nil, idx, insightsNow)
	if got.Total != 1 || got.Blocked != 1 {
		t.Errorf("explicitly blocked bead: got %+v, want Total=1 Blocked=1", got)
	}
}

// TestIsStale: only live work past the cutoff is stale; a zero timestamp isn't.
func TestIsStale(t *testing.T) {
	cases := []struct {
		name    string
		status  string
		updated time.Time
		want    bool
	}{
		{"old open", "open", insStale, true},
		{"fresh open", "open", insFresh, false},
		{"old closed", "closed", insStale, false},
		{"zero time", "open", time.Time{}, false},
	}
	for _, c := range cases {
		if got := isStale(c.status, c.updated, insightsNow); got != c.want {
			t.Errorf("isStale(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestLeaderboard: ranks by score descending, caps the list, and sizes the leader's
// bar at 100%. The foundational bead (most depended-on) tops PageRank.
func TestLeaderboard(t *testing.T) {
	beads, _ := insScope(t)
	compEdges := []graph.Edge{
		{Dependent: "demo-i.2", Dependency: "demo-i.1"},
		{Dependent: "demo-i.3", Dependency: "demo-i.2"},
	}
	m := graph.Compute([]string{"demo-i.1", "demo-i.2", "demo-i.3", "demo-i.4", "demo-i.5"}, compEdges)
	board := leaderboard(beads, m.PageRank)
	if len(board) == 0 {
		t.Fatal("leaderboard empty; expected ranked beads")
	}
	if board[0].ID != "demo-i.1" {
		t.Errorf("top influence = %s, want demo-i.1 (foundational)", board[0].ID)
	}
	if board[0].Width != 100 {
		t.Errorf("leader bar = %d%%, want 100%%", board[0].Width)
	}
	for i := 1; i < len(board); i++ {
		if board[i-1].Score < board[i].Score {
			t.Errorf("leaderboard not descending at %d: %v < %v", i, board[i-1].Score, board[i].Score)
		}
	}
}

// TestLeaderboardEmptyWithoutEdges: an all-zero metric (no deps) yields no rows.
func TestLeaderboardEmptyWithoutEdges(t *testing.T) {
	beads, _ := insScope(t)
	if board := leaderboard(beads, map[string]float64{}); len(board) != 0 {
		t.Errorf("leaderboard over zero scores = %d rows, want 0", len(board))
	}
}

// TestLabelHealth: counts labels over open beads (in-progress excluded), descending
// by count then name, and flags untagged open beads.
func TestLabelHealth(t *testing.T) {
	beads, idx := insScope(t)
	labels := labelHealth(beads, idx)
	want := []labelCount{{Label: "core", Count: 2}, {Label: "ui", Count: 2}}
	if len(labels) != len(want) {
		t.Fatalf("labelHealth = %+v, want %+v", labels, want)
	}
	for i := range want {
		if labels[i] != want[i] {
			t.Errorf("label[%d] = %+v, want %+v", i, labels[i], want[i])
		}
	}
	if n := untaggedOpen(beads, idx); n != 1 {
		t.Errorf("untaggedOpen = %d, want 1 (demo-i.5)", n)
	}
}

// TestBeadPath resolves the critical-path ids to scope beads, dropping unknowns.
func TestBeadPath(t *testing.T) {
	beads, _ := insScope(t)
	path := beadPath([]string{"demo-i.3", "demo-i.2", "demo-i.1", "demo-gone"}, beadByID(beads))
	if len(path) != 3 {
		t.Fatalf("beadPath len = %d, want 3 (unknown dropped)", len(path))
	}
	if path[0].ID != "demo-i.3" || path[2].ID != "demo-i.1" {
		t.Errorf("beadPath order wrong: %s..%s", path[0].ID, path[2].ID)
	}
}

// TestInsightsFragmentRenders: the dashboard returns the six panels, the view-toggle
// (with Insights active and links back to the other views), and the computed values.
func TestInsightsFragmentRenders(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: insightsIssues, deps: insightsDeps})
	srv.now = func() time.Time { return insightsNow }
	rec := do(t, srv, "/insights?epic=demo-i")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /insights = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Triage", "Influence", "Bottlenecks", "Critical path", "Cycles", "Label health",
		`hx-get="/list?epic=demo-i"`,  // toggle back to Table
		`hx-get="/graph?epic=demo-i"`, // and Graph
		"Foundation",                  // top influence bead title
		"No cycles",                   // acyclic fixture
		"untagged",                    // hygiene warning (demo-i.5)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("insights fragment missing %q", want)
		}
	}
}

// TestInsightsScopedToEpic: the epic param narrows the dashboard to one epic; a bead
// from another epic must not appear in the critical path or leaderboards.
func TestInsightsScopedToEpic(t *testing.T) {
	mixed := append(append([]bd.Issue(nil), insightsIssues...),
		bd.Issue{ID: "demo-z", Title: "Other epic", IssueType: "epic", Status: "open", Priority: 2, UpdatedAt: insFresh},
		bd.Issue{ID: "demo-z.1", Parent: "demo-z", Title: "Elsewhere", Status: "open", Priority: 2, UpdatedAt: insFresh})
	srv := newTestServer(t, &stubBD{issues: mixed, deps: insightsDeps})
	srv.now = func() time.Time { return insightsNow }
	body := do(t, srv, "/insights?epic=demo-i").Body.String()
	if strings.Contains(body, "Elsewhere") {
		t.Error("epic-scoped insights leaked a bead from another epic")
	}
}

// TestInsightsCycleWarning: a scope with a dependency cycle surfaces it instead of
// the all-clear (reuses the graph fixture's {g.5,g.6} cycle).
func TestInsightsCycleWarning(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: graphIssues, deps: graphDeps})
	srv.now = func() time.Time { return insightsNow }
	body := do(t, srv, "/insights?epic=demo-g").Body.String()
	if strings.Contains(body, "No cycles") {
		t.Error("cyclic scope reported as acyclic")
	}
	if !strings.Contains(body, `class="cycles"`) {
		t.Error("cycle panel missing the cycle list")
	}
}

// TestInsightsNoRepo: with no active repo the dashboard degrades to the empty pane
// at 200, like the other views.
func TestInsightsNoRepo(t *testing.T) {
	tmpl, err := web.Templates()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	srv := New(func(registry.Repo) IssueSource { return &stubBD{} },
		registry.InMemory(), tmpl, web.Static(), forest.Synthesis{})
	rec := do(t, srv, "/insights")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /insights (no repo) = %d, want 200", rec.Code)
	}
}
