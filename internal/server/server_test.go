package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/registry"
	"github.com/dkoosis/strand/internal/strand"
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

	rankWrites  []rankWrite  // ordered log of SetRank calls, for the reorder tests.
	depWrites   []depWrite   // ordered log of DepAdd/DepRemove calls, for the dep tests.
	labelWrites []labelWrite // ordered log of LabelAdd/LabelRemove calls, for the label tests.

	createOpts []bd.CreateOpts // ordered log of Create calls, for the create-path tests.
}

// labelWrite records one LabelAdd/LabelRemove call so a test can assert the
// handler forwarded the right label (including an encoded key=value pair).
type labelWrite struct {
	op    string // "add" | "remove"
	id    string
	label string
}

// depWrite records one DepAdd/DepRemove call so a test can assert the handler
// wired the edge in the right direction (id depends on `on`).
type depWrite struct {
	op string // "add" | "remove"
	id string
	on string
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

// DepAdd records a "blocks" edge so the drawer re-read lists it; writeErr models
// a bd outage. depWrites logs the call so a test can assert the wiring direction.
func (s *stubBD) DepAdd(_ context.Context, id, dependsOn string) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	s.depWrites = append(s.depWrites, depWrite{op: "add", id: id, on: dependsOn})
	s.deps = append(s.deps, bd.DepEdge{IssueID: id, DependsOnID: dependsOn, Type: "blocks"})
	return nil
}

// DepRemove drops the matching edge from the in-memory set so the re-read reflects it.
func (s *stubBD) DepRemove(_ context.Context, id, dependsOn string) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	s.depWrites = append(s.depWrites, depWrite{op: "remove", id: id, on: dependsOn})
	kept := s.deps[:0]
	for _, d := range s.deps {
		if d.IssueID == id && d.DependsOnID == dependsOn {
			continue
		}
		kept = append(kept, d)
	}
	s.deps = kept
	return nil
}

// LabelAdd appends a label to the shown bead so the drawer re-read renders the new
// chip; writeErr models a bd outage. labelWrites logs the call so a test can assert
// the forwarded value (a key=value pair arrives already encoded).
func (s *stubBD) LabelAdd(_ context.Context, id, label string) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	s.labelWrites = append(s.labelWrites, labelWrite{op: "add", id: id, label: label})
	if iss := s.show[id]; iss != nil {
		iss.Labels = append(iss.Labels, label)
	}
	return nil
}

// LabelRemove drops the matching label so the re-read no longer renders its chip.
func (s *stubBD) LabelRemove(_ context.Context, id, label string) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	s.labelWrites = append(s.labelWrites, labelWrite{op: "remove", id: id, label: label})
	if iss := s.show[id]; iss != nil {
		kept := iss.Labels[:0]
		for _, l := range iss.Labels {
			if l == label {
				continue
			}
			kept = append(kept, l)
		}
		iss.Labels = kept
	}
	return nil
}

// Create mints a bead with a fixed id and stores it so the post-create drawer
// re-read finds it; an empty title fails like bd's validation does.
func (s *stubBD) Create(_ context.Context, opts *bd.CreateOpts) (*bd.Issue, error) {
	if s.writeErr != nil {
		return nil, s.writeErr
	}
	if opts.Title == "" {
		return nil, bd.ErrEmptyTitle
	}
	// The first Create of a request keeps the legacy demo-new id (the common
	// single create, and the new-parent mint which always runs first). A second
	// Create in the same request — the child under a freshly-minted parent — gets
	// a distinct id so both land in the show map.
	id := "demo-new"
	if len(s.createOpts) > 0 {
		id = "demo-child"
	}
	s.createOpts = append(s.createOpts, *opts)
	iss := &bd.Issue{ID: id, Title: opts.Title, IssueType: opts.Type, Status: "open"}
	iss.Priority = opts.Priority
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
		if p, err := strconv.Atoi(value); err == nil {
			iss.Priority = &p
		}
	case "assignee":
		iss.Assignee = value
	case "description":
		iss.Description = value
	case "status":
		iss.Status = bd.Status(value)
	}
	return iss, nil
}

// SetRank logs the write and reflects it in the issue list so a follow-up
// buildStrand re-read sees the new rank — mirroring bd's metadata round-trip.
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
// strand region the way the active repo's name does in production.
var demoRepo = registry.Repo{Name: "demo", Path: "/demo"}

// readOnlyStub implements ONLY the readSource methods (List/Deps/Comments) — no
// writes. It pins the narrowed seam (strand-8zg): the read helpers must depend on
// readSource, not the fat IssueSource, so a source with no write methods drives
// them. If a read helper grew a write call, buildStrand/insightsModel/renderDrawer
// would no longer accept this type and the package would not compile.
type readOnlyStub struct{ inner *stubBD }

func (r *readOnlyStub) List(ctx context.Context, args ...string) ([]bd.Issue, error) {
	return r.inner.List(ctx, args...)
}

func (r *readOnlyStub) Deps(ctx context.Context, ids ...string) ([]bd.DepEdge, error) {
	return r.inner.Deps(ctx, ids...)
}

func (r *readOnlyStub) Comments(ctx context.Context, id string) ([]bd.Comment, error) {
	return r.inner.Comments(ctx, id)
}

// readOnlyStub satisfies the narrow seam but NOT the fat IssueSource.
var _ readSource = (*readOnlyStub)(nil)

// TestReadHelpersTakeReadSource drives the three read helpers through a source
// that has no write methods, proving they bind to readSource (strand-8zg).
func TestReadHelpersTakeReadSource(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: sampleIssues, deps: sampleDeps})
	src := &readOnlyStub{inner: &stubBD{issues: sampleIssues, deps: sampleDeps}}
	ctx := context.Background()

	f, err := srv.buildStrand(ctx, src, demoRepo)
	if err != nil {
		t.Fatalf("buildStrand: %v", err)
	}
	view := listViewFor(f, "")
	if _, err := srv.insightsModel(ctx, src, &view, sampleIssues); err != nil {
		t.Fatalf("insightsModel: %v", err)
	}
	rec := httptest.NewRecorder()
	srv.renderDrawer(ctx, rec, src, &sampleIssues[0], nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("renderDrawer status = %d, want 200", rec.Code)
	}
}

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
	return New(srcFor, reg, tmpl, web.Static(), strand.Synthesis{NorthStar: "remember across sessions"})
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
	{ID: "demo-root", Title: "DEMO trunk", IssueType: "epic", Status: "open"}, // region; epics below are tiles
	{ID: "demo-e1", Parent: "demo-root", Title: "Strand epic", IssueType: "epic", Status: "open", Priority: new(1)},
	{ID: "demo-e1.a", Parent: "demo-e1", Title: "Wire the thing", Status: "open", Priority: new(0)},
	{ID: "demo-e1.b", Parent: "demo-e1", Title: "Test the thing", Status: "in_progress", Priority: new(2)},
	{ID: "demo-e2", Parent: "demo-root", Title: "Lone task", IssueType: "task", Status: "open", Priority: new(3)},
}

// TestStrandPageRenders pins the view-centric landing: the page renders the north
// star, the loud primary view-switcher (Table/Board/Insights tabs), the minimap
// treemap with a tile per epic carrying its filter identity (data-epic, routed to
// the active view by app.js), and the centerpiece list.
func TestStrandPageRenders(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: sampleIssues})
	rec := do(t, srv, "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"remember across sessions", // north star
		`class="viewbar"`,          // loud primary view switcher
		`class="viewtab active" type="button" data-view="list"`, // Table is the default loud tab
		`data-view="board"`,    // Board tab present
		`data-view="insights"`, // Insights tab present
		`class="minimap"`,      // treemap demoted to ambient minimap rail
		`class="treemap"`,
		`data-epic="demo-e1"`, // tile carries its filter identity (app.js routes the click)
		`data-epic="demo-e2"`,
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

// TestEpicGroupHeadBranches pins both epic-group renders after the epicArgs
// bool-trap split: the region view (every epic) draws a group header per epic,
// while the epic-scoped view drops the header (the pane already names the epic).
func TestEpicGroupHeadBranches(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: sampleIssues})

	// Region view: no epic param ranges over Region.Epics with a head per group.
	recRegion := do(t, srv, "/list")
	if recRegion.Code != http.StatusOK {
		t.Fatalf("GET /list = %d, want 200", recRegion.Code)
	}
	region := recRegion.Body.String()
	if !strings.Contains(region, `class="eg-head"`) {
		t.Error("region list missing per-epic group header")
	}
	if !strings.Contains(region, `hx-get="/list?epic=demo-e1"`) {
		t.Error("group header missing drill-in wiring")
	}

	// Epic-scoped view: the header is redundant (the pane head already names it).
	recEpic := do(t, srv, "/list?epic=demo-e1")
	if recEpic.Code != http.StatusOK {
		t.Fatalf("GET /list?epic=demo-e1 = %d, want 200", recEpic.Code)
	}
	epic := recEpic.Body.String()
	if strings.Contains(epic, `class="eg-head"`) {
		t.Error("epic-scoped list drew a redundant group header")
	}
	// The body (bead rows) must still render in both branches.
	if !strings.Contains(epic, `class="bead-rows" data-epic="demo-e1"`) {
		t.Error("epic-scoped list missing its bead rows")
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
	// The board head carries the scope marker app.js reads to keep the top tab strip
	// on Board at this epic, so a minimap click filters the active (board) view.
	if !strings.Contains(body, `data-view="board"`) || !strings.Contains(body, `data-epic="demo-e1"`) {
		t.Error("board fragment missing data-view/data-epic scope marker")
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
		{ID: "r-root", Title: "RANK trunk", IssueType: "epic", Status: "open"}, // region; r-1 is the tile
		{ID: "r-1", Parent: "r-root", Title: "Ranked epic", IssueType: "epic", Status: "open", Priority: new(1), Metadata: map[string]any{"rank": 1.0}},
		{ID: "r-1.a", Parent: "r-1", Title: "A", Status: "open", Priority: new(0), Metadata: map[string]any{"rank": 2.0}},
		{ID: "r-1.b", Parent: "r-1", Title: "B", Status: "open", Priority: new(2), Metadata: map[string]any{"rank": 3.0}},
		{ID: "r-1.c", Parent: "r-1", Title: "C", Status: "open", Priority: new(2), Metadata: map[string]any{"rank": 4.0}},
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

// TestRankSeedSkipsAbsentID: an id in the posted order that the strand no longer
// yields (closed mid-drag) gets no rank write — only the live survivors are seeded,
// and they stay dense.
func TestRankSeedSkipsAbsentID(t *testing.T) {
	stub := &stubBD{issues: append([]bd.Issue(nil), sampleIssues...)}
	srv := newTestServer(t, stub)
	// "ghost" is not in the strand; the live demo-e1 group is the other three.
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
	tight[2].Metadata["rank"] = math.Nextafter(1, 2) // r-1.a one ulp above r-1 (index 2: after r-root, r-1)
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
		"demo-e1.a": {ID: "demo-e1.a", Title: "Wire the thing", Status: "open", Priority: new(0), IssueType: "task", Description: "do the wiring"},
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

// TestDrawerShapingLeadsAgentOpsDemoted: the drawer is the human shaping surface, so
// the Description editor must render before the agent-ops zone (claim/close/delete).
// Byte-offset ordering guards the zoning — a re-inversion regresses this red.
func TestDrawerShapingLeadsAgentOpsDemoted(t *testing.T) {
	srv := newTestServer(t, &stubBD{show: map[string]*bd.Issue{
		"demo-z": {ID: "demo-z", Title: "Shape me", Status: "open", IssueType: "task", Description: "the body"},
	}})
	body := do(t, srv, "/bead/demo-z").Body.String()

	desc := strings.Index(body, `value="description"`) // hidden field marks the Description editor
	ops := strings.Index(body, "dr-overflow")          // demoted agent-ops disclosure
	if desc < 0 {
		t.Fatalf("drawer missing the Description editor:\n%s", body)
	}
	if ops < 0 {
		t.Fatalf("drawer missing the demoted agent-ops zone:\n%s", body)
	}
	if desc > ops {
		t.Errorf("agent ops render before Description — shaping is not leading (desc=%d, ops=%d)", desc, ops)
	}
	// Claim/Close/Delete all live inside the demoted zone, not the head — guard each
	// so a partial re-inversion of any one button fails red.
	for _, op := range []string{"/claim", "/close", "/delete"} {
		if i := strings.Index(body, op); i < ops {
			t.Errorf("%s sits above the agent-ops zone (%s=%d, ops=%d)", op, op, i, ops)
		}
	}
}

// TestDrawerSystemMetadataReadOnly: the system-metadata block shows the agent/
// rubric-owned fields (rank, kg_project, requires_test, difficulty, est_cost_usd)
// for orientation, but exposes NO edit affordance — clobber-protection is the
// whole point (str-6k0.6.4). The block must render the values and contain no
// input/textarea/select/hx-patch that would let a human stomp what agents depend on.
func TestDrawerSystemMetadataReadOnly(t *testing.T) {
	srv := newTestServer(t, &stubBD{show: map[string]*bd.Issue{
		"demo-m": {
			ID: "demo-m", Title: "Meta bead", Status: "open", IssueType: "task",
			Metadata: map[string]any{
				"rank":          7.0,
				"kg_project":    "strand",
				"requires_test": true,
				"difficulty":    "medium",
				"est_cost_usd":  4.5,
			},
		},
	}})
	body := do(t, srv, "/bead/demo-m").Body.String()

	// The block renders, below the shaping zone (after the demoted agent-ops zone).
	sys := strings.Index(body, `class="dr-sys"`)
	if sys < 0 {
		t.Fatalf("drawer missing the system-metadata block:\n%s", body)
	}
	if ops := strings.Index(body, "dr-overflow"); ops < 0 || ops > sys {
		t.Errorf("system metadata not below the shaping zone (ops=%d, sys=%d)", ops, sys)
	}

	// Every field's value renders.
	for _, want := range []string{"strand", "yes", "medium", "4.5", "7"} {
		if !strings.Contains(body[sys:], want) {
			t.Errorf("system-metadata block missing value %q:\n%s", want, body[sys:])
		}
	}

	// No edit affordance inside the block: scope the scan to dr-sys..</dl> so the
	// drawer's other (legitimately editable) controls don't mask a leak here.
	end := strings.Index(body[sys:], "</dl>")
	if end < 0 {
		t.Fatalf("system-metadata block not closed with </dl>:\n%s", body[sys:])
	}
	block := body[sys : sys+end]
	for _, banned := range []string{"<input", "<textarea", "<select", "hx-patch", "hx-post"} {
		if strings.Contains(block, banned) {
			t.Errorf("system-metadata block exposes a write control %q (clobber-protection breached):\n%s", banned, block)
		}
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

// TestCreateFormRenders: the new-bead form opens with the type options, the
// forced-parent picker (off-trunk + new-inline choices), and the candidate
// parents loaded from the source.
func TestCreateFormRenders(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: sampleIssues})
	rec := do(t, srv, "/new")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /new = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `hx-post="/new"`) {
		t.Errorf("create form missing its post target:\n%s", body)
	}
	for _, want := range []string{`name="parent"`, `value="__off_trunk__"`, `value="__new__"`} {
		if !strings.Contains(body, want) {
			t.Errorf("create form missing forced-parent picker part %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, sampleIssues[0].ID) {
		t.Errorf("create form did not offer candidate parent %q:\n%s", sampleIssues[0].ID, body)
	}
}

// TestCreateReflects: a valid create under an existing parent shows the new
// bead's drawer, fires refreshList, and threads the parent id through to bd.
func TestCreateReflects(t *testing.T) {
	stub := &stubBD{show: map[string]*bd.Issue{}}
	srv := newTestServer(t, stub)
	rec := send(t, srv, http.MethodPost, "/new", "title=Fresh+bead&type=task&priority=2&parent=epic-1")

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /new = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Fresh bead") {
		t.Errorf("drawer missing the created bead:\n%s", rec.Body.String())
	}
	if rec.Header().Get("HX-Trigger") != "refreshList" {
		t.Errorf("create did not fire refreshList, got %q", rec.Header().Get("HX-Trigger"))
	}
	if len(stub.createOpts) != 1 || stub.createOpts[0].Parent != "epic-1" {
		t.Errorf("create did not thread the parent id, got %+v", stub.createOpts)
	}
}

// TestCreateForcesParent: a create with no parent choice is rejected — the form
// re-renders with the forced-parent error and no bead is created. The strand's
// tree axis can't be skipped by omission (str-6k0.6.2).
func TestCreateForcesParent(t *testing.T) {
	stub := &stubBD{show: map[string]*bd.Issue{}}
	srv := newTestServer(t, stub)
	rec := send(t, srv, http.MethodPost, "/new", "title=Orphan&type=task&priority=2")

	body := rec.Body.String()
	if !strings.Contains(body, `hx-post="/new"`) {
		t.Errorf("rejected create did not re-render the form:\n%s", body)
	}
	if !strings.Contains(body, errNoParent.Error()) {
		t.Errorf("form hides the forced-parent error:\n%s", body)
	}
	if len(stub.createOpts) != 0 {
		t.Errorf("a parentless create reached bd: %+v", stub.createOpts)
	}
}

// TestCreateOffTrunk: the off-trunk choice is a deliberate root — the bead is
// created with no parent id and no error.
func TestCreateOffTrunk(t *testing.T) {
	stub := &stubBD{show: map[string]*bd.Issue{}}
	srv := newTestServer(t, stub)
	rec := send(t, srv, http.MethodPost, "/new", "title=Root&type=task&priority=2&parent=__off_trunk__")

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /new = %d, want 200", rec.Code)
	}
	if len(stub.createOpts) != 1 || stub.createOpts[0].Parent != "" {
		t.Errorf("off-trunk create should carry no parent, got %+v", stub.createOpts)
	}
}

// TestCreateNewParentInline: the create-new-inline path mints the parent epic
// first, then creates the child under it — two creates, the child carrying the
// minted parent's id.
func TestCreateNewParentInline(t *testing.T) {
	stub := &stubBD{show: map[string]*bd.Issue{}}
	srv := newTestServer(t, stub)
	rec := send(t, srv, http.MethodPost, "/new",
		"title=Child&type=task&priority=2&parent=__new__&parent_new=New+Epic")

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /new = %d, want 200", rec.Code)
	}
	if len(stub.createOpts) != 2 {
		t.Fatalf("inline new-parent should create parent then child, got %+v", stub.createOpts)
	}
	if stub.createOpts[0].Title != "New Epic" || stub.createOpts[0].Type != "epic" {
		t.Errorf("first create should mint the parent epic, got %+v", stub.createOpts[0])
	}
	if stub.createOpts[1].Title != "Child" || stub.createOpts[1].Parent != "demo-new" {
		t.Errorf("child should hang under the minted parent, got %+v", stub.createOpts[1])
	}
}

// TestCreateNewParentNeedsTitle: choosing create-new-inline without a title is
// rejected; nothing is created.
func TestCreateNewParentNeedsTitle(t *testing.T) {
	stub := &stubBD{show: map[string]*bd.Issue{}}
	srv := newTestServer(t, stub)
	rec := send(t, srv, http.MethodPost, "/new", "title=Child&type=task&priority=2&parent=__new__&parent_new=")

	body := rec.Body.String()
	if !strings.Contains(body, errNoParentTitle.Error()) {
		t.Errorf("form hides the new-parent-title error:\n%s", body)
	}
	if len(stub.createOpts) != 0 {
		t.Errorf("a titleless new-parent reached bd: %+v", stub.createOpts)
	}
}

// TestCreateEmptyTitleIsHonest: a titleless create (with a valid parent choice)
// re-renders the form with bd's error, not a half-made bead.
func TestCreateEmptyTitleIsHonest(t *testing.T) {
	srv := newTestServer(t, &stubBD{})
	rec := send(t, srv, http.MethodPost, "/new", "title=&type=task&priority=2&parent=__off_trunk__")

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
	{ID: "demo-root", Title: "DEMO trunk", IssueType: "epic", Status: "open"}, // region; demo-i is the tile
	{ID: "demo-i", Parent: "demo-root", Title: "Insights epic", IssueType: "epic", Status: "open", Priority: new(1), UpdatedAt: insFresh},
	{ID: "demo-i.1", Parent: "demo-i", Title: "Foundation", Status: "open", Priority: new(1), Labels: []string{"core"}, UpdatedAt: insFresh},
	{ID: "demo-i.2", Parent: "demo-i", Title: "Mid", Status: "open", Priority: new(2), Labels: []string{"core", "ui"}, UpdatedAt: insFresh},
	{ID: "demo-i.3", Parent: "demo-i", Title: "Leaf", Status: "open", Priority: new(2), Labels: []string{"ui"}, UpdatedAt: insFresh},
	{ID: "demo-i.4", Parent: "demo-i", Title: "Active", Status: "in_progress", Priority: new(2), Labels: []string{"core"}, UpdatedAt: insFresh},
	{ID: "demo-i.5", Parent: "demo-i", Title: "Stale", Status: "open", Priority: new(3), UpdatedAt: insStale},
}

var insightsDeps = []bd.DepEdge{
	{IssueID: "demo-i.2", DependsOnID: "demo-i.1", Type: "blocks"},
	{IssueID: "demo-i.3", DependsOnID: "demo-i.2", Type: "blocks"},
}

// The pure analytics tests (triage/leaderboard/rank math/label health) moved to
// internal/insight with the domain logic they cover (str-hh4). The fixtures above
// stay because the handler tests below still drive the /insights render path.

// TestInsightsFragmentRenders: the dashboard returns its panels, the scope marker
// app.js reads to keep the top tab strip on Insights at this epic, and the computed
// values. The view switcher itself is now top-level page chrome, not per-fragment.
func TestInsightsFragmentRenders(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: insightsIssues, deps: insightsDeps})
	srv.now = func() time.Time { return insightsNow }
	rec := do(t, srv, "/insights?epic=demo-i")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /insights = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Shape of the work", "Critical path", "Label health",
		"Do next",                 // the frees-ranked dispatch list
		"Hotspots",                // load-bearing lens (was PageRank column)
		"Bottlenecks",             // bridge lens (was betweenness column)
		"frees",                   // do-next consequence framing
		"lean on this",            // hotspot structural read
		`data-view="insights"`,    // scope marker: app.js syncs the Insights tab as active
		`data-epic="demo-i"`,      // ...scoped to this epic, so a tab switch keeps the scope
		`hx-get="/bead/demo-i.1"`, // referenced rows click → drawer
		`hx-target="#drawer"`,     // ...into the detail panel
		"Foundation",              // top load-bearing bead title
		"untagged",                // hygiene warning (demo-i.5)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("insights fragment missing %q", want)
		}
	}
	// The bottleneck leader demo-i.2 is dependency-blocked: its row carries the
	// act-now cross-flag marker.
	if !strings.Contains(body, "lb-flag") {
		t.Error("insights fragment missing cross-flag marker on a blocked ranked row")
	}
}

// TestInsightsScopedToEpic: the epic param narrows the dashboard to one epic; a bead
// from another epic must not appear in the critical path or leaderboards.
func TestInsightsScopedToEpic(t *testing.T) {
	mixed := append(append([]bd.Issue(nil), insightsIssues...),
		bd.Issue{ID: "demo-z", Parent: "demo-root", Title: "Other epic", IssueType: "epic", Status: "open", Priority: new(2), UpdatedAt: insFresh},
		bd.Issue{ID: "demo-z.1", Parent: "demo-z", Title: "Elsewhere", Status: "open", Priority: new(2), UpdatedAt: insFresh})
	srv := newTestServer(t, &stubBD{issues: mixed, deps: insightsDeps})
	srv.now = func() time.Time { return insightsNow }
	body := do(t, srv, "/insights?epic=demo-i").Body.String()
	if strings.Contains(body, "Elsewhere") {
		t.Error("epic-scoped insights leaked a bead from another epic")
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
		registry.InMemory(), tmpl, web.Static(), strand.Synthesis{})
	rec := do(t, srv, "/insights")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /insights (no repo) = %d, want 200", rec.Code)
	}
}

// TestDepAddWiresBlocker: adding a blocker from the drawer issues DepAdd in the
// "id depends on target" direction and redraws the panel listing the new blocker.
func TestDepAddWiresBlocker(t *testing.T) {
	stub := oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task"})
	srv := newTestServer(t, stub)
	rec := send(t, srv, http.MethodPost, "/bead/demo-x/dep", "depends_on=demo-y")

	if rec.Code != http.StatusOK {
		t.Fatalf("POST dep = %d, want 200", rec.Code)
	}
	if len(stub.depWrites) != 1 || stub.depWrites[0] != (depWrite{op: "add", id: "demo-x", on: "demo-y"}) {
		t.Errorf("dep add issued %+v, want one add(demo-x, demo-y)", stub.depWrites)
	}
	if !strings.Contains(rec.Body.String(), "demo-y") {
		t.Errorf("redrawn drawer missing the new blocker:\n%s", rec.Body.String())
	}
}

// TestDepAddTrimsAndErrors: surrounding whitespace is trimmed before the write,
// and a bd rejection surfaces in the drawer rather than 200-with-no-change.
func TestDepAddError(t *testing.T) {
	stub := oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task"})
	stub.writeErr = fmt.Errorf("bd dep add: %w", bd.ErrBD)
	srv := newTestServer(t, stub)
	rec := send(t, srv, http.MethodPost, "/bead/demo-x/dep", "depends_on=demo-y")

	if !strings.Contains(rec.Body.String(), "dep add") {
		t.Errorf("rejected dep add missing bd's message:\n%s", rec.Body.String())
	}
}

// TestDepRemoveDropsBlocker: the remove form issues DepRemove and the redrawn
// drawer no longer lists the dropped blocker.
func TestDepRemoveDropsBlocker(t *testing.T) {
	stub := oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task"})
	stub.deps = []bd.DepEdge{{IssueID: "demo-x", DependsOnID: "demo-y", Type: "blocks"}}
	srv := newTestServer(t, stub)
	rec := send(t, srv, http.MethodPost, "/bead/demo-x/dep/remove", "depends_on=demo-y")

	if rec.Code != http.StatusOK {
		t.Fatalf("POST dep remove = %d, want 200", rec.Code)
	}
	if len(stub.depWrites) != 1 || stub.depWrites[0].op != "remove" {
		t.Errorf("dep remove issued %+v, want one remove", stub.depWrites)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No blockers") {
		t.Errorf("drawer still lists a blocker after remove:\n%s", body)
	}
}

// TestLabelAddChipReflects: adding a plain chip forwards the bare label and the
// redrawn drawer renders it as a removable chip.
func TestLabelAddChipReflects(t *testing.T) {
	stub := oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task"})
	srv := newTestServer(t, stub)
	rec := send(t, srv, http.MethodPost, "/bead/demo-x/label", "key=ui")

	if rec.Code != http.StatusOK {
		t.Fatalf("POST label = %d, want 200", rec.Code)
	}
	if len(stub.labelWrites) != 1 || stub.labelWrites[0] != (labelWrite{op: "add", id: "demo-x", label: "ui"}) {
		t.Errorf("label add issued %+v, want one add(demo-x, ui)", stub.labelWrites)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "dr-chip") || !strings.Contains(body, "ui") {
		t.Errorf("redrawn drawer missing the new chip:\n%s", body)
	}
}

// TestLabelAddKeyValueEncodes: a key + value pair joins into the `key=value`
// label bd stores, and the redraw renders it as a key-value chip.
func TestLabelAddKeyValueEncodes(t *testing.T) {
	stub := oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task"})
	srv := newTestServer(t, stub)
	rec := send(t, srv, http.MethodPost, "/bead/demo-x/label", "key=owner&value=dk")

	if len(stub.labelWrites) != 1 || stub.labelWrites[0].label != "owner=dk" {
		t.Errorf("label add issued %+v, want encoded owner=dk", stub.labelWrites)
	}
	if body := rec.Body.String(); !strings.Contains(body, "dr-chip-kv") {
		t.Errorf("redrawn drawer missing the key-value chip:\n%s", body)
	}
}

// TestLabelRemoveReflects: the chip's remove form forwards the raw label and the
// redrawn drawer no longer renders it.
func TestLabelRemoveReflects(t *testing.T) {
	stub := oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task", Labels: []string{"ui", "owner=dk"}})
	srv := newTestServer(t, stub)
	rec := send(t, srv, http.MethodPost, "/bead/demo-x/label/remove", "label=owner%3Ddk")

	if rec.Code != http.StatusOK {
		t.Fatalf("POST label remove = %d, want 200", rec.Code)
	}
	if len(stub.labelWrites) != 1 || stub.labelWrites[0] != (labelWrite{op: "remove", id: "demo-x", label: "owner=dk"}) {
		t.Errorf("label remove issued %+v, want one remove(demo-x, owner=dk)", stub.labelWrites)
	}
	if body := rec.Body.String(); strings.Contains(body, "owner") {
		t.Errorf("drawer still renders the removed pair:\n%s", body)
	}
}

// TestLabelAddError: a bd rejection surfaces in the drawer rather than a silent
// 200-with-no-change.
func TestLabelAddError(t *testing.T) {
	stub := oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task"})
	stub.writeErr = fmt.Errorf("bd label add: %w", bd.ErrBD)
	srv := newTestServer(t, stub)
	rec := send(t, srv, http.MethodPost, "/bead/demo-x/label", "key=ui")

	if !strings.Contains(rec.Body.String(), "label add") {
		t.Errorf("rejected label add missing bd's message:\n%s", rec.Body.String())
	}
}

// countingBD wraps stubBD to tally List/Deps invocations, so the snapshot-cache
// tests can prove a second view hit memory (no second bd spawn) rather than the
// source again. Writes delegate to the embedded stub.
type countingBD struct {
	stubBD
	listCalls atomic.Int64
	depsCalls atomic.Int64
}

func (c *countingBD) List(ctx context.Context, args ...string) ([]bd.Issue, error) {
	c.listCalls.Add(1)
	return c.stubBD.List(ctx, args...)
}

func (c *countingBD) Deps(ctx context.Context, ids ...string) ([]bd.DepEdge, error) {
	c.depsCalls.Add(1)
	return c.stubBD.Deps(ctx, ids...)
}

// TestSnapshotCacheCrossView: navigating strand→list→board→insights on one repo
// fetches List and Deps once each; every later view is served from the in-process
// snapshot (acceptance: cross-view nav hits memory after first load).
func TestSnapshotCacheCrossView(t *testing.T) {
	src := &countingBD{stubBD: stubBD{issues: sampleIssues, deps: sampleDeps}}
	srv := newTestServer(t, src)
	srv.now = func() time.Time { return cacheNow }

	// /insights is the only view that fetches Deps; request it twice so the
	// depsCalls==1 assertion proves the second hit is served from the snapshot,
	// not just that one view fetched once.
	for _, p := range []string{"/", "/list", "/board", "/insights", "/insights"} {
		if rec := do(t, srv, p); rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", p, rec.Code)
		}
	}
	if src.listCalls.Load() != 1 {
		t.Errorf("List spawned %d times across the views, want 1 (cache miss only on first load)", src.listCalls.Load())
	}
	if src.depsCalls.Load() != 1 {
		t.Errorf("Deps spawned %d times, want 1 (insights' fetch is cached)", src.depsCalls.Load())
	}
}

// TestSnapshotCacheInvalidatesOnWrite: a successful write drops the repo's
// snapshot, so the next view re-reads bd's truth (acceptance: writes invalidate
// the cache so the UI still shows bd truth).
func TestSnapshotCacheInvalidatesOnWrite(t *testing.T) {
	src := &countingBD{stubBD: stubBD{
		issues: sampleIssues,
		deps:   sampleDeps,
		show:   map[string]*bd.Issue{"demo-e1.a": {ID: "demo-e1.a", Title: "Wire the thing", Status: "open"}},
	}}
	srv := newTestServer(t, src)
	srv.now = func() time.Time { return cacheNow }

	do(t, srv, "/")     // warms the snapshot (List #1)
	do(t, srv, "/list") // served from cache (still List #1)
	if src.listCalls.Load() != 1 {
		t.Fatalf("List spawned %d times before write, want 1", src.listCalls.Load())
	}

	// A successful edit must invalidate the snapshot.
	if rec := send(t, srv, http.MethodPatch, "/bead/demo-e1.a", "field=title&value=Renamed"); rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200", rec.Code)
	}

	do(t, srv, "/list") // must re-read bd's truth (List #2)
	if src.listCalls.Load() != 2 {
		t.Errorf("List spawned %d times, want 2 — a write must invalidate the snapshot", src.listCalls.Load())
	}
}

// TestHandleHomePrefetchesDeps: opening the landing warms Deps too, not just List,
// so the first Insights click hits memory instead of a cold `dep list` spawn
// (str-udl). Asserting depsCalls==1 after only GET / — with no Insights visit —
// proves the prefetch ran.
func TestHandleHomePrefetchesDeps(t *testing.T) {
	src := &countingBD{stubBD: stubBD{issues: sampleIssues, deps: sampleDeps}}
	srv := newTestServer(t, src)
	srv.now = func() time.Time { return cacheNow }

	if rec := do(t, srv, "/"); rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	// The prefetch runs in a background goroutine (so the landing never blocks on
	// the spawn), so poll for it rather than asserting immediately.
	deadline := time.Now().Add(2 * time.Second)
	for src.depsCalls.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if src.depsCalls.Load() != 1 {
		t.Errorf("Deps spawned %d times on open, want 1 — the landing must prefetch deps", src.depsCalls.Load())
	}
	if rec := do(t, srv, "/insights"); src.depsCalls.Load() != 1 {
		t.Errorf("Deps spawned %d times after insights, want 1 — insights must reuse the prefetch (code %d)", src.depsCalls.Load(), rec.Code)
	}
}

// TestHandleRefreshInvalidates: POST /refresh drops the active snapshot and tells
// htmx to reload (HX-Refresh: true). With no time expiry, a write-less reload
// would otherwise serve the warm snapshot, so refresh is the only way to surface an
// out-of-band edit (str-udl).
func TestHandleRefreshInvalidates(t *testing.T) {
	src := &countingBD{stubBD: stubBD{issues: sampleIssues, deps: sampleDeps}}
	srv := newTestServer(t, src)
	srv.now = func() time.Time { return cacheNow }

	do(t, srv, "/") // warms the snapshot (List #1)
	if src.listCalls.Load() != 1 {
		t.Fatalf("List spawned %d times before refresh, want 1", src.listCalls.Load())
	}

	rec := send(t, srv, http.MethodPost, "/refresh", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /refresh = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("HX-Refresh"); got != "true" {
		t.Errorf("HX-Refresh = %q, want \"true\"", got)
	}

	do(t, srv, "/list") // must re-read bd's truth (List #2)
	if src.listCalls.Load() != 2 {
		t.Errorf("List spawned %d times after refresh, want 2 — refresh must invalidate", src.listCalls.Load())
	}
}

// TestHandleHomeShowsAsOf: the landing carries the "as of HH:MM" refresh readout,
// stamped from the snapshot's fetch time (str-udl).
func TestHandleHomeShowsAsOf(t *testing.T) {
	src := &countingBD{stubBD: stubBD{issues: sampleIssues, deps: sampleDeps}}
	srv := newTestServer(t, src)
	srv.now = func() time.Time { return cacheNow }

	rec := do(t, srv, "/")
	if want := "as of " + cacheNow.Format("15:04"); !strings.Contains(rec.Body.String(), want) {
		t.Errorf("landing missing the refresh readout %q", want)
	}
}

// TestSnapshotCacheRepoSwitchRescopes: switching the active repo serves the new
// repo's source, not the prior repo's cached snapshot (acceptance: repo switch
// re-scopes). Two repos back two distinct counting sources.
func TestSnapshotCacheRepoSwitchRescopes(t *testing.T) {
	tmpl, err := web.Templates()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	srcA := &countingBD{stubBD: stubBD{issues: sampleIssues, deps: sampleDeps}}
	srcB := &countingBD{stubBD: stubBD{issues: sampleIssues, deps: sampleDeps}}
	repoA := registry.Repo{Name: "alpha", Path: "/alpha"}
	repoB := registry.Repo{Name: "bravo", Path: "/bravo"}
	reg := registry.InMemory(repoA, repoB)
	srcFor := func(r registry.Repo) IssueSource {
		if r.Path == "/bravo" {
			return srcB
		}
		return srcA
	}
	srv := New(srcFor, reg, tmpl, web.Static(), strand.Synthesis{NorthStar: "x"})
	srv.now = func() time.Time { return cacheNow }

	// Warm repoB's snapshot (InMemory makes the last-added repo active).
	do(t, srv, "/")
	bBefore := srcB.listCalls.Load()

	// Switch to repoA, then load a view: it must fetch repoA's source, not serve
	// repoB's snapshot.
	if rec := send(t, srv, http.MethodPost, "/repo", "path=/alpha"); rec.Code != http.StatusNoContent {
		t.Fatalf("POST /repo = %d, want 204", rec.Code)
	}
	do(t, srv, "/")
	if srcA.listCalls.Load() != 1 {
		t.Errorf("repoA List spawned %d times after switch, want 1 — switch must re-scope", srcA.listCalls.Load())
	}
	if srcB.listCalls.Load() != bBefore {
		t.Errorf("repoB List spawned again (%d→%d) after switching away", bBefore, srcB.listCalls.Load())
	}
}

// TestSnapshotCacheConcurrentListDeps drives List and Deps on one repo's caching
// source from many goroutines at once. Run under -race it trips if liveList /
// liveDeps leak the shared *snapshot and a reader touches deps/depsOK while
// putDeps writes them in place (strand-4sd). The first goroutines miss and fetch;
// the rest are served from the snapshot — the point is the concurrent reads, not
// the call count.
func TestSnapshotCacheConcurrentListDeps(t *testing.T) {
	src := &cachingSource{
		IssueSource: &countingBD{stubBD: stubBD{issues: sampleIssues, deps: sampleDeps}},
		cache:       newSnapshotCache(func() time.Time { return cacheNow }),
		repo:        "demo",
	}
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := src.List(ctx); err != nil {
				t.Errorf("List: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := src.Deps(ctx, "demo-e1.a"); err != nil {
				t.Errorf("Deps: %v", err)
			}
		}()
	}
	wg.Wait()
}

// TestSnapshotCachePutDepsVersionSkew proves putDeps binds deps to the snapshot
// generation they were fetched for: a Deps fetch that reads ids from snapshot V1,
// then races a putList that publishes V2, must NOT staple V1's deps onto V2. The
// cache reads the gen at the List fetch and only writes deps when the live snapshot
// still carries that gen (gemini review, strand-4sd).
func TestSnapshotCachePutDepsVersionSkew(t *testing.T) {
	c := newSnapshotCache(func() time.Time { return cacheNow })

	c.putList("demo", []bd.Issue{{ID: "a"}}) // V1
	_, genV1, ok := c.liveList("demo")
	if !ok {
		t.Fatal("V1 not live after putList")
	}

	// Simulate a concurrent invalidate+refresh: V1 is replaced by V2 (depsOK false)
	// before the in-flight Deps fetch returns.
	c.putList("demo", []bd.Issue{{ID: "a"}, {ID: "b"}}) // V2

	// The late putDeps carries V1's gen — it must be dropped, not written onto V2.
	c.putDeps("demo", genV1, []bd.DepEdge{{IssueID: "a", DependsOnID: "stale"}})
	if _, ok := c.liveDeps("demo"); ok {
		t.Error("putDeps stapled stale V1 deps onto V2 — version-skew guard failed")
	}

	// A putDeps for the current snapshot (V2's gen) still writes.
	_, genV2, _ := c.liveList("demo")
	c.putDeps("demo", genV2, []bd.DepEdge{{IssueID: "a", DependsOnID: "b"}})
	if _, ok := c.liveDeps("demo"); !ok {
		t.Error("putDeps for the live snapshot was wrongly dropped")
	}
}

// TestSnapshotCacheNoTimeExpiry proves str-udl's core contract: a snapshot has no
// time-based expiry — advancing the clock by any amount keeps it warm, so a long
// look at one view never re-pays the bd spawn on the next tab. Only an explicit
// invalidate (a write, or POST /refresh) forces the re-fetch. The clock is a
// mutable fake; advancing it stands in for wall-clock passing during a long look.
func TestSnapshotCacheNoTimeExpiry(t *testing.T) {
	src := &countingBD{stubBD: stubBD{issues: sampleIssues, deps: sampleDeps}}
	clock := cacheNow
	cache := newSnapshotCache(func() time.Time { return clock })
	cs := &cachingSource{IssueSource: src, cache: cache, repo: "demo"}
	ctx := context.Background()

	if _, err := cs.List(ctx); err != nil { // miss → fetch #1
		t.Fatalf("List: %v", err)
	}
	clock = clock.Add(time.Hour) // a long look — far past the old 3s TTL
	if _, err := cs.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}
	if src.listCalls.Load() != 1 {
		t.Fatalf("List spawned %d times after an hour, want 1 — snapshot must not age out", src.listCalls.Load())
	}

	cache.invalidate("demo") // explicit refresh / a write drops the snapshot

	if _, err := cs.List(ctx); err != nil { // miss → fetch #2
		t.Fatalf("List: %v", err)
	}
	if src.listCalls.Load() != 2 {
		t.Errorf("List spawned %d times after invalidate, want 2 — invalidate must re-fetch", src.listCalls.Load())
	}
}

var cacheNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// sampleDeps gives the graph/insights views one in-scope blocks edge so a Deps
// fetch has something to return — the cache tests only count the call, not the
// payload.
var sampleDeps = []bd.DepEdge{{IssueID: "demo-e1.a", DependsOnID: "demo-e1.b", Type: "blocks"}}

// TestDrawerListsBlockers: the detail panel renders the bead's existing "blocks"
// dependencies and ignores non-blocks edges (epic parent-child et al).
func TestDrawerListsBlockers(t *testing.T) {
	stub := oneBead(&bd.Issue{ID: "demo-x", Title: "Task", Status: "open", IssueType: "task"})
	stub.deps = []bd.DepEdge{
		{IssueID: "demo-x", DependsOnID: "demo-y", Type: "blocks"},
		{IssueID: "demo-x", DependsOnID: "demo-epic", Type: "parent-child"},
	}
	srv := newTestServer(t, stub)
	body := do(t, srv, "/bead/demo-x").Body.String()
	if !strings.Contains(body, "demo-y") {
		t.Errorf("drawer missing the blocks dependency:\n%s", body)
	}
	if strings.Contains(body, "demo-epic") {
		t.Error("drawer leaked a non-blocks (parent-child) edge into the blocker list")
	}
}
