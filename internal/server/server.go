// Package server is strand's HTTP layer: it renders the embedded web UI as HTML
// (html/template) and swaps fragments over htmx. It reads beads through a small
// issue source so the bd CLI stays the only data path (spec D8).
package server

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/forest"
	"github.com/dkoosis/strand/internal/graph"
	"github.com/dkoosis/strand/internal/registry"
)

// errNoRepo means no beads workspace is active. The landing turns it into an
// actionable empty state (spec R1); other handlers shouldn't reach it, since the
// UI offers no bead links until a repo is active.
var (
	errNoRepo  = errors.New("no active repo")
	errNoIssue = errors.New("no issue to display")
)

// bd status values strand reasons about (triage, the kanban status pivot). The
// kanban column captions stay separate display strings.
const (
	statusOpen       = "open"
	statusInProgress = "in_progress"
	statusBlocked    = "blocked"
	statusClosed     = "closed"
	statusDeferred   = "deferred"
)

// IssueSource is the slice of bd.Client the server needs: reads plus the V1
// writes. An interface keeps the handlers testable with a stub and the bd CLI
// behind one seam (spec Q0). Writes go through the write-client (spec D6) so the
// bare-`bd update` footgun stays impossible. It is exported because the active
// repo is resolved per request through a SourceFunc the command wires.
type IssueSource interface {
	List(ctx context.Context, args ...string) ([]bd.Issue, error)
	Deps(ctx context.Context, ids ...string) ([]bd.DepEdge, error)
	Show(ctx context.Context, id string) (*bd.Issue, error)
	Comments(ctx context.Context, id string) ([]bd.Comment, error)
	Update(ctx context.Context, id, field, value string) (*bd.Issue, error)
	Claim(ctx context.Context, id string) (*bd.Issue, error)
	Close(ctx context.Context, id, reason string) (*bd.Issue, error)
	Comment(ctx context.Context, id, text string) error
	Create(ctx context.Context, opts bd.CreateOpts) (*bd.Issue, error)
	DeletePreview(ctx context.Context, id string) (string, error)
	Delete(ctx context.Context, id string) error
}

// SourceFunc builds the bd-backed issue source for a repo. It is the seam the
// command wires (a real bd.Client scoped to the repo's path) and tests stub (an
// in-memory fake), so switching the active repo re-scopes every read and write
// without the server knowing how a source is made.
type SourceFunc func(registry.Repo) IssueSource

// Server renders the forest landing and its htmx fragments over the active repo's
// issue source. The active repo (and the known-repo registry) live in reg;
// srcFor turns the active repo into the source each request reads through.
type Server struct {
	srcFor   SourceFunc
	reg      *registry.Registry
	tmpl     *template.Template
	static   http.Handler
	syn      forest.Synthesis
	shutdown func()           // raised by POST /shutdown; a test seam over the interrupt hook
	now      func() time.Time // clock for the stale cutoff; a test seam (default time.Now)
}

// defaultShutdown raises os.Interrupt at strand's own process, so the Quit button
// flows through the same graceful path Ctrl-C does (signal.NotifyContext →
// httpSrv.Shutdown in main) rather than a hard exit. os.Interrupt keeps this
// portable (no syscall import); main already listens for it. Tests replace it.
func defaultShutdown() {
	if p, err := os.FindProcess(os.Getpid()); err == nil {
		_ = p.Signal(os.Interrupt)
	}
}

// New builds a Server. srcFor resolves the active repo to its bd source and reg
// holds the registry + active selection. tmpl holds the parsed UI templates and
// static serves the embedded assets, both wired in by the caller so package
// server stays free of embed. syn is the human-shaped synthesis layer (north
// star); the project label follows the active repo.
func New(srcFor SourceFunc, reg *registry.Registry, tmpl *template.Template, static http.Handler, syn forest.Synthesis) *Server {
	return &Server{srcFor: srcFor, reg: reg, tmpl: tmpl, static: static, syn: syn, shutdown: defaultShutdown, now: time.Now}
}

// source resolves the active repo's issue source. ok is false when no repo is
// active, which the landing renders as the empty state.
func (s *Server) source() (IssueSource, registry.Repo, bool) {
	repo, ok := s.reg.Active()
	if !ok {
		return nil, registry.Repo{}, false
	}
	return s.srcFor(repo), repo, true
}

// Routes returns the mux: the forest page, its htmx fragments, and static assets.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleForest)
	mux.HandleFunc("GET /list", s.handleList)
	mux.HandleFunc("GET /board", s.handleBoard)
	mux.HandleFunc("GET /graph", s.handleGraph)
	mux.HandleFunc("GET /insights", s.handleInsights)
	mux.HandleFunc("GET /bead/{id}", s.handleBead)
	mux.HandleFunc("PATCH /bead/{id}", s.handleEdit)
	mux.HandleFunc("POST /bead/{id}/move", s.handleMove)
	mux.HandleFunc("POST /bead/{id}/claim", s.handleClaim)
	mux.HandleFunc("POST /bead/{id}/close", s.handleClose)
	mux.HandleFunc("POST /bead/{id}/reopen", s.handleReopen)
	mux.HandleFunc("POST /bead/{id}/comment", s.handleComment)
	mux.HandleFunc("POST /bead/{id}/delete", s.handleDeletePreview)
	mux.HandleFunc("DELETE /bead/{id}", s.handleDelete)
	mux.HandleFunc("GET /new", s.handleNewForm)
	mux.HandleFunc("POST /new", s.handleCreate)
	mux.HandleFunc("GET /repos", s.handleRepos)
	mux.HandleFunc("POST /repos/rescan", s.handleRescan)
	mux.HandleFunc("POST /repo", s.handleSwitchRepo)
	mux.HandleFunc("POST /repo/add", s.handleAddRepo)
	mux.HandleFunc("POST /shutdown", s.handleShutdown)
	mux.Handle("GET /static/", s.static)
	return mux
}

// reqContext bounds every bd shell-out so a hung CLI can't wedge a request.
func reqContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 10*time.Second)
}

// handleShutdown stops strand from the UI: it answers with the stopped fragment,
// then raises the shutdown hook. The answer is written first and the real hook
// only signals (non-blocking) — the graceful drain in main lets this response
// flush before the listener closes. Local-only tool, so no confirm or auth guard.
func (s *Server) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	s.render(w, "shutdown", nil)
	s.shutdown()
}

// listView is the bead-list pane: a region, optionally narrowed to one epic.
type listView struct {
	Region  forest.Region
	Epic    forest.Epic
	HasEpic bool // false = show the whole region
}

// pageData is the full landing render: the forest, the list pane, and the repo
// selector chrome (the active repo's name and the known repos). Empty is true
// when no repo is active, switching the landing to its actionable empty state.
type pageData struct {
	Forest forest.Forest
	List   listView
	Repos  repoMenu
	Empty  bool
}

func (s *Server) handleForest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	src, repo, ok := s.source()
	if !ok {
		s.render(w, "page", pageData{Empty: true, Repos: s.repoMenu("")})
		return
	}
	f, err := s.buildForest(ctx, src, repo)
	if err != nil {
		s.renderError(w, err)
		return
	}
	s.render(w, "page", pageData{Forest: f, List: listViewFor(f, ""), Repos: s.repoMenu("")})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	src, repo, ok := s.source()
	if !ok {
		s.render(w, "list", listView{})
		return
	}
	f, err := s.buildForest(ctx, src, repo)
	if err != nil {
		s.renderError(w, err)
		return
	}
	// epic=<id> narrows the pane to a single tile; absent means the whole region.
	s.render(w, "list", listViewFor(f, r.URL.Query().Get("epic")))
}

// listViewFor builds the bead-list pane from the forest: its first region,
// optionally narrowed to one epic by id. An empty forest yields an empty view.
// Both the full-page render and the htmx list swap go through here, so the two
// panes can't diverge.
func listViewFor(f forest.Forest, epicID string) listView {
	if len(f.Regions) == 0 {
		return listView{}
	}
	view := listView{Region: f.Regions[0]}
	if epicID != "" {
		for _, e := range view.Region.Epics {
			if e.ID == epicID {
				view.Epic, view.HasEpic = e, true
				break
			}
		}
	}
	return view
}

// pivotField is one kanban axis: its name (the bd field a drop writes), the
// canonical ordered columns that always exist as drop targets, and how to read a
// bead's value for it. Co-locating all three means a new pivot is added in one
// place, not spread across a name list, a seed map, and a value switch.
type pivotField struct {
	Name  string
	Seeds []boardColumn
	value func(*forest.Bead) string
}

// pivotFields are the fields the kanban can pivot on, in pivot-bar order (the
// first is the default). Type is omitted: bd has no `update --type`, so a type
// column couldn't accept a drag. The seeds give canonical, ordered columns so
// empty drop targets exist (e.g. an empty "in progress" to drag into); values bd
// reports that aren't seeded (a named assignee) are appended. The assignee
// "unassigned" column writes an empty value — dropping there clears the field.
var pivotFields = []pivotField{
	{"status", []boardColumn{{Key: statusOpen, Label: "open"}, {Key: statusInProgress, Label: "in progress"}, {Key: statusBlocked, Label: "blocked"}, {Key: statusClosed, Label: "closed"}}, func(b *forest.Bead) string { return b.Status }},
	{"priority", []boardColumn{{Key: "0", Label: "P0"}, {Key: "1", Label: "P1"}, {Key: "2", Label: "P2"}, {Key: "3", Label: "P3"}, {Key: "4", Label: "P4"}}, func(b *forest.Bead) string { return strconv.Itoa(b.Priority) }},
	{"assignee", []boardColumn{{Key: "", Label: "unassigned"}}, func(b *forest.Bead) string { return b.Assignee }},
}

// boardPivots is the pivot bar's name order, derived from pivotFields so the two
// can't drift. The first name is the default pivot.
var boardPivots = pivotNames()

func pivotNames() []string {
	names := make([]string, len(pivotFields))
	for i := range pivotFields {
		names[i] = pivotFields[i].Name
	}
	return names
}

// boardView is the kanban render: the same scope the table shows (a region or one
// epic, embedded so the head reuses the list's scope markup), pivoted into columns
// by Pivot. Pivots drives the pivot bar.
type boardView struct {
	listView
	Pivot   string
	Pivots  []string
	Columns []boardColumn
}

// boardColumn is one kanban column: Key is the field value a drop writes (e.g.
// "in_progress", "2", or "" to clear an assignee); Label is its caption.
type boardColumn struct {
	Key   string
	Label string
	Beads []forest.Bead
}

func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	src, repo, ok := s.source()
	if !ok {
		s.render(w, "board", boardView{Pivots: boardPivots, Pivot: boardPivots[0]})
		return
	}
	f, err := s.buildForest(ctx, src, repo)
	if err != nil {
		s.renderError(w, err)
		return
	}
	view := listViewFor(f, r.URL.Query().Get("epic"))
	s.render(w, "board", buildBoard(&view, r.URL.Query().Get("pivot")))
}

// buildBoard pivots the scope's beads into columns. Both the whole-region and
// single-epic scopes flow through here, so the board can't diverge from the table.
func buildBoard(v *listView, pivot string) boardView {
	pivot = pivotOrDefault(pivot)
	return boardView{
		listView: *v,
		Pivot:    pivot,
		Pivots:   boardPivots,
		Columns:  boardColumns(pivot, scopeBeads(v)),
	}
}

// scopeBeads flattens the view's beads: one epic's, or every epic's in the region.
func scopeBeads(v *listView) []forest.Bead {
	if v.HasEpic {
		return v.Epic.Beads
	}
	total := 0
	for i := range v.Region.Epics {
		total += len(v.Region.Epics[i].Beads)
	}
	all := make([]forest.Bead, 0, total)
	for i := range v.Region.Epics {
		all = append(all, v.Region.Epics[i].Beads...)
	}
	return all
}

// pivotOrDefault clamps an arbitrary query value to a known pivot, defaulting to
// the first so a bad ?pivot= never yields an empty board.
func pivotOrDefault(p string) string {
	if slices.Contains(boardPivots, p) {
		return p
	}
	return boardPivots[0]
}

// boardColumns buckets beads into the pivot's columns: the seeded columns first
// (in order), then any unseeded value bd reports, appended. Every bead lands in
// exactly one column. pivot must be a known field (callers pass pivotOrDefault).
func boardColumns(pivot string, beads []forest.Bead) []boardColumn {
	field := pivotByName(pivot)
	cols := append([]boardColumn(nil), field.Seeds...)
	idx := make(map[string]int, len(cols))
	for i := range cols {
		idx[cols[i].Key] = i
	}
	for i := range beads {
		key := field.value(&beads[i])
		col, ok := idx[key]
		if !ok {
			col = len(cols)
			cols = append(cols, boardColumn{Key: key, Label: key})
			idx[key] = col
		}
		cols[col].Beads = append(cols[col].Beads, beads[i])
	}
	return cols
}

// pivotByName returns the field for a known pivot name. Callers gate on
// pivotOrDefault, so an unknown name (only from a bad caller) falls back to the
// first field rather than a zero value with a nil reader.
func pivotByName(name string) pivotField {
	for i := range pivotFields {
		if pivotFields[i].Name == name {
			return pivotFields[i]
		}
	}
	return pivotFields[0]
}

// handleMove writes the pivot field of a dragged card (SortableJS onEnd posts
// field+value) through the same Update path the drawer edits use, then returns the
// refreshed card so the board shows bd's truth. A write error renders the error
// fragment at a non-2xx status, which the client reads as the signal to revert the
// card to its old column (spec R0 V2: optimistic move, revert on error).
func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	src, _, ok := s.source()
	if !ok {
		s.renderError(w, errNoRepo)
		return
	}
	id := r.PathValue("id")
	issue, err := src.Update(ctx, id, r.FormValue("field"), r.FormValue("value"))
	if err != nil {
		s.renderError(w, wrapWrite("move", err))
		return
	}
	if issue == nil { // bd wrote silently; re-read so the card reflects the change
		if issue, err = src.Show(ctx, id); err != nil {
			s.renderError(w, err)
			return
		}
	}
	s.render(w, "boardCard", forest.NewBead(issue))
}

// graphData is the serialized dependency-graph model the fragment hands to
// Cytoscape: a node per in-scope bead, an edge per kept "blocks" dependency, each
// node carrying the gonum-computed metrics that drive size and highlight. The
// client only lays out and draws (D8) — it computes nothing.
type graphData struct {
	Nodes []graphNode `json:"nodes"`
	Edges []graphEdge `json:"edges"`
}

// graphNode is one bead: Score is its PageRank (node size — foundational beads
// loom), InCycle marks a bead caught in a dependency cycle, OnPath a bead on the
// single longest dependency chain (the critical path).
type graphNode struct {
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	Status   string  `json:"status"`
	Priority int     `json:"priority"`
	Score    float64 `json:"score"`
	InCycle  bool    `json:"inCycle"`
	OnPath   bool    `json:"onPath"`
}

// graphEdge is one blocks dependency: Source (the dependent) is blocked by Target
// (the dependency) — the DAG's "points at what it needs" direction.
type graphEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// graphView wraps the scope chrome (so the fragment reuses the list/board head)
// with the marshaled model the client reads off a data-graph attribute. JSON is a
// plain string so html/template escapes it for the attribute; the test reverses
// that with html.UnescapeString.
type graphView struct {
	listView
	JSON string
}

// handleGraph renders the dependency-graph view for the active scope (a region or
// one epic), mirroring handleBoard. No repo or empty scope renders the same empty
// pane the other views use.
func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	src, repo, ok := s.source()
	if !ok {
		s.render(w, "graph", graphView{})
		return
	}
	f, err := s.buildForest(ctx, src, repo)
	if err != nil {
		s.renderError(w, err)
		return
	}
	view := listViewFor(f, r.URL.Query().Get("epic"))
	model, err := s.graphModel(ctx, src, &view)
	if err != nil {
		s.renderError(w, err)
		return
	}
	s.render(w, "graph", graphView{listView: view, JSON: model})
}

// graphModel builds the serialized dependency graph for a scope. It uses the same
// beads the table and board show (scopeBeads), fetches their dependencies, keeps
// only the in-scope "blocks" edges, and folds in the gonum metrics. The edge
// filter is load-bearing: bd's Deps can report an edge to a bead outside the scope
// (its single-ID query synthesizes one), and dropping it keeps the DAG closed over
// the visible nodes.
func (s *Server) graphModel(ctx context.Context, src IssueSource, v *listView) (string, error) {
	beads := scopeBeads(v)
	ids, inScope := scopeIDs(beads)

	gd := graphData{Edges: []graphEdge{}}
	var compEdges []graph.Edge
	if len(ids) > 0 {
		deps, err := src.Deps(ctx, ids...)
		if err != nil {
			return "", fmt.Errorf("graph deps: %w", err)
		}
		compEdges, gd.Edges = blocksEdges(deps, inScope)
	}
	computed := graph.Compute(ids, compEdges)
	gd.Nodes = metricNodes(beads, &computed)

	raw, err := json.Marshal(gd)
	if err != nil {
		return "", fmt.Errorf("marshal graph: %w", err)
	}
	return string(raw), nil
}

// scopeIDs lists the scope's bead IDs and a membership set for the edge filter.
func scopeIDs(beads []forest.Bead) ([]string, map[string]bool) {
	ids := make([]string, len(beads))
	in := make(map[string]bool, len(beads))
	for i := range beads {
		ids[i] = beads[i].ID
		in[beads[i].ID] = true
	}
	return ids, in
}

// blocksEdges keeps the in-scope "blocks" dependencies, returning them as both
// gonum compute-edges and serialized model edges. Edges of another type, or with
// an endpoint outside the scope, are dropped so the DAG stays closed over the
// visible nodes.
func blocksEdges(deps []bd.DepEdge, inScope map[string]bool) ([]graph.Edge, []graphEdge) {
	compute := make([]graph.Edge, 0, len(deps))
	model := make([]graphEdge, 0, len(deps))
	for _, d := range deps {
		if d.Type != "blocks" || !inScope[d.IssueID] || !inScope[d.DependsOnID] {
			continue
		}
		compute = append(compute, graph.Edge{Dependent: d.IssueID, Dependency: d.DependsOnID})
		model = append(model, graphEdge{Source: d.IssueID, Target: d.DependsOnID})
	}
	return compute, model
}

// metricNodes projects each scope bead to a graph node, folding in the metrics
// that drive node size (PageRank) and highlight (cycle membership, critical path).
func metricNodes(beads []forest.Bead, m *graph.Metrics) []graphNode {
	inCycle := map[string]bool{}
	for _, c := range m.Cycles {
		for _, id := range c {
			inCycle[id] = true
		}
	}
	onPath := map[string]bool{}
	for _, id := range m.CriticalPath {
		onPath[id] = true
	}
	nodes := make([]graphNode, len(beads))
	for i := range beads {
		b := &beads[i]
		nodes[i] = graphNode{
			ID:       b.ID,
			Label:    b.Title,
			Status:   b.Status,
			Priority: b.Priority,
			Score:    m.PageRank[b.ID],
			InCycle:  inCycle[b.ID],
			OnPath:   onPath[b.ID],
		}
	}
	return nodes
}

// insightsView wraps the scope chrome (so the fragment reuses the list/board/graph
// head) with the computed dashboard.
type insightsView struct {
	listView
	Insights insights
}

// insights is the V4 dashboard (spec §10): quick-ref counts plus five panels over
// strand's own in-process metrics. Structural panels (Influence, Bottlenecks,
// CritPath, Cycles) read the in-scope closed graph, mirroring V3; the triage counts
// read all blockers (a bead can be blocked from outside the scope).
type insights struct {
	Counts     triageCounts
	Influence  []rankedBead // top PageRank — foundational beads
	Bottleneck []rankedBead // top betweenness — chokepoints
	CritPath   []forest.Bead
	Cycles     [][]string
	Labels     []labelCount // label distribution over open beads, descending
	Untagged   int          // open beads carrying no label at all
}

// triageCounts is the quick-ref panel: the live shape of the scope's queue.
type triageCounts struct {
	Total, Open, InProgress, Ready, Blocked, Stale int
}

// rankedBead is one leaderboard row: a bead, its raw metric score, and a 0–100 bar
// width normalized to the panel's top score (computed in Go so the template is dumb).
type rankedBead struct {
	forest.Bead
	Score float64
	Width int
}

// labelCount is one row of the label-health distribution.
type labelCount struct {
	Label string
	Count int
}

// staleAfter is how long an open bead can sit untouched before triage flags it.
const staleAfter = 14 * 24 * time.Hour

// leaderboardSize caps the Influence and Bottleneck panels.
const leaderboardSize = 5

// handleInsights renders the V4 dashboard for the active scope (a region or one
// epic), mirroring handleGraph. No repo or an empty scope renders the same empty
// pane the other views use. It lists once and reuses the issues for both the forest
// (scope) and the metric/triage computation.
func (s *Server) handleInsights(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	src, repo, ok := s.source()
	if !ok {
		s.render(w, "insights", insightsView{})
		return
	}
	issues, err := src.List(ctx)
	if err != nil {
		s.renderError(w, fmt.Errorf("list issues: %w", err))
		return
	}
	f := forest.Build(issues, s.synFor(repo))
	view := listViewFor(f, r.URL.Query().Get("epic"))
	model, err := s.insightsModel(ctx, src, &view, issues)
	if err != nil {
		s.renderError(w, err)
		return
	}
	s.render(w, "insights", insightsView{listView: view, Insights: model})
}

// insightsModel computes the dashboard for a scope. issues is the full repo list
// (for labels and timestamps the forest drops); the scope's beads come from the
// view. Deps drives both the in-scope structural graph and the all-blockers triage.
func (s *Server) insightsModel(ctx context.Context, src IssueSource, v *listView, issues []bd.Issue) (insights, error) {
	// Epics are containers, not actionable work: the dashboard is about the queue
	// and the structure of real tasks, so it drops them (the graph view keeps them).
	beads := actionable(scopeBeads(v))
	ids, inScope := scopeIDs(beads)

	var deps []bd.DepEdge
	if len(ids) > 0 {
		var err error
		if deps, err = src.Deps(ctx, ids...); err != nil {
			return insights{}, fmt.Errorf("insights deps: %w", err)
		}
	}
	compEdges, _ := blocksEdges(deps, inScope)
	m := graph.Compute(ids, compEdges)

	idx := indexIssues(issues)
	out := insights{
		Counts:   triage(beads, deps, idx, s.now()),
		CritPath: beadPath(m.CriticalPath, beadByID(beads)),
		Cycles:   m.Cycles,
		Labels:   labelHealth(beads, idx),
		Untagged: untaggedOpen(beads, idx),
	}
	// The leaderboards rank by graph position; with no dependencies every bead ties
	// at PageRank's base rank, so a ranking would be noise. Show them only with edges.
	if len(compEdges) > 0 {
		out.Influence = leaderboard(beads, m.PageRank)
		out.Bottleneck = leaderboard(beads, m.Betweenness)
	}
	return out, nil
}

// actionable drops epic-type beads (containers) from a scope, leaving the real work
// the dashboard reasons about.
func actionable(beads []forest.Bead) []forest.Bead {
	out := make([]forest.Bead, 0, len(beads))
	for i := range beads {
		if beads[i].Type != "epic" {
			out = append(out, beads[i])
		}
	}
	return out
}

// indexIssues maps every repo bead by id, so triage and label-health can read the
// fields forest.Bead drops (status of an out-of-scope blocker, timestamps, labels).
func indexIssues(issues []bd.Issue) map[string]bd.Issue {
	m := make(map[string]bd.Issue, len(issues))
	for i := range issues {
		m[issues[i].ID] = issues[i]
	}
	return m
}

// beadByID indexes the scope's beads for the critical-path title lookup.
func beadByID(beads []forest.Bead) map[string]forest.Bead {
	m := make(map[string]forest.Bead, len(beads))
	for i := range beads {
		m[beads[i].ID] = beads[i]
	}
	return m
}

// triage counts the scope's queue shape. ready/blocked weigh ALL of a bead's
// blocks-dependencies (resolved against the full-repo index), since a blocker can
// live outside the visible scope; stale flags live work untouched past the cut.
func triage(beads []forest.Bead, deps []bd.DepEdge, idx map[string]bd.Issue, now time.Time) triageCounts {
	openBlockers := blockerCounts(deps, idx)
	var c triageCounts
	for i := range beads {
		b := &beads[i]
		c.Total++
		switch b.Status {
		case statusInProgress:
			c.InProgress++
		case statusOpen:
			c.Open++
			if openBlockers[b.ID] > 0 {
				c.Blocked++
			} else {
				c.Ready++
			}
		case statusBlocked:
			c.Blocked++
		}
		if isStale(b.Status, idx[b.ID].UpdatedAt, now) {
			c.Stale++
		}
	}
	return c
}

// blockerCounts tallies, per bead, how many of its blocks-dependencies are still
// unmet — the ones keeping it out of the ready queue. A blocker counts only if it's
// present in the live index AND not closed; an absent target is treated as resolved,
// since `bd list` omits closed beads (a done dependency simply isn't in the list).
func blockerCounts(deps []bd.DepEdge, idx map[string]bd.Issue) map[string]int {
	open := map[string]int{}
	for _, d := range deps {
		if d.Type != "blocks" {
			continue
		}
		if iss, ok := idx[d.DependsOnID]; ok && iss.Status != statusClosed {
			open[d.IssueID]++
		}
	}
	return open
}

// isStale reports whether live (open/in-progress) work has gone untouched past the
// cutoff. A zero timestamp (never recorded) is not flagged — absence isn't staleness.
func isStale(status string, updated, now time.Time) bool {
	if status == statusClosed || status == statusDeferred || updated.IsZero() {
		return false
	}
	return now.Sub(updated) > staleAfter
}

// leaderboard ranks the scope's beads by a metric, descending, keeping the top few
// with a positive score and sizing each row's bar against the leader. An all-zero
// metric (no edges) yields no rows — there's nothing to lead.
func leaderboard(beads []forest.Bead, score map[string]float64) []rankedBead {
	ranked := make([]rankedBead, 0, len(beads))
	for i := range beads {
		if s := score[beads[i].ID]; s > 0 {
			ranked = append(ranked, rankedBead{Bead: beads[i], Score: s})
		}
	}
	slices.SortFunc(ranked, func(a, b rankedBead) int {
		if a.Score != b.Score {
			return cmp.Compare(b.Score, a.Score) // descending
		}
		return cmp.Compare(a.ID, b.ID) // stable tiebreak
	})
	if len(ranked) > leaderboardSize {
		ranked = ranked[:leaderboardSize]
	}
	if len(ranked) > 0 {
		top := ranked[0].Score
		for i := range ranked {
			ranked[i].Width = int(ranked[i].Score / top * 100)
		}
	}
	return ranked
}

// beadPath resolves the critical-path ids to scope beads (for their titles),
// dropping any id not in scope so the panel can't render a blank row.
func beadPath(path []string, byID map[string]forest.Bead) []forest.Bead {
	out := make([]forest.Bead, 0, len(path))
	for _, id := range path {
		if b, ok := byID[id]; ok {
			out = append(out, b)
		}
	}
	return out
}

// labelHealth tallies labels across the scope's open beads, descending by count
// then name, so the panel surfaces what the live work is tagged with.
func labelHealth(beads []forest.Bead, idx map[string]bd.Issue) []labelCount {
	count := map[string]int{}
	for i := range beads {
		if beads[i].Status != statusOpen {
			continue
		}
		for _, l := range idx[beads[i].ID].Labels {
			count[l]++
		}
	}
	out := make([]labelCount, 0, len(count))
	for l, n := range count {
		out = append(out, labelCount{Label: l, Count: n})
	}
	slices.SortFunc(out, func(a, b labelCount) int {
		if a.Count != b.Count {
			return cmp.Compare(b.Count, a.Count)
		}
		return cmp.Compare(a.Label, b.Label)
	})
	return out
}

// untaggedOpen counts open beads carrying no label — the hygiene warning that pairs
// with the distribution.
func untaggedOpen(beads []forest.Bead, idx map[string]bd.Issue) int {
	n := 0
	for i := range beads {
		if beads[i].Status == statusOpen && len(idx[beads[i].ID].Labels) == 0 {
			n++
		}
	}
	return n
}

// drawerData is the detail panel: a bead, its comments, and an optional write
// error. Embedding promotes the issue's fields, so the template reads .Title etc.
// directly; .Err carries a bd write failure to show inline (spec Q2).
type drawerData struct {
	*bd.Issue
	Comments []bd.Comment
	Err      string
}

func (s *Server) handleBead(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	src, _, ok := s.source()
	if !ok {
		s.renderError(w, errNoRepo)
		return
	}
	issue, err := src.Show(ctx, r.PathValue("id"))
	if err != nil {
		s.renderError(w, err)
		return
	}
	s.renderDrawer(ctx, w, src, issue, nil)
}

// renderDrawer redraws the detail panel from bd's truth: the issue plus its
// comments, with an optional write error shown inline. A comments-read failure is
// non-fatal — the panel still shows the issue, just without the thread.
func (s *Server) renderDrawer(ctx context.Context, w http.ResponseWriter, src IssueSource, issue *bd.Issue, writeErr error) {
	if issue == nil {
		// bd can return (nil, nil) — a silent write or an empty Show (firstIssue's
		// documented contract). No bead to draw, so fall back to the error panel
		// rather than panic dereferencing issue.ID below.
		s.renderError(w, errNoIssue)
		return
	}
	data := drawerData{Issue: issue}
	if writeErr != nil {
		data.Err = writeErr.Error()
	}
	if cs, err := src.Comments(ctx, issue.ID); err == nil {
		data.Comments = cs
	}
	s.render(w, "drawer", data)
}

// handleEdit writes one field from the detail panel (hx-patch with field+value).
func (s *Server) handleEdit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	field, value := r.FormValue("field"), r.FormValue("value")
	s.writeAndRefresh(w, r, id, func(ctx context.Context, src IssueSource) (*bd.Issue, error) {
		iss, err := src.Update(ctx, id, field, value)
		return iss, wrapWrite("edit", err)
	})
}

// handleClaim assigns the bead to the current user.
func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.writeAndRefresh(w, r, id, func(ctx context.Context, src IssueSource) (*bd.Issue, error) {
		iss, err := src.Claim(ctx, id)
		return iss, wrapWrite("claim", err)
	})
}

// handleClose closes the bead, with an optional reason.
func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	reason := r.FormValue("reason")
	s.writeAndRefresh(w, r, id, func(ctx context.Context, src IssueSource) (*bd.Issue, error) {
		iss, err := src.Close(ctx, id, reason)
		return iss, wrapWrite("close", err)
	})
}

// handleReopen flips a closed bead back to open. There is no bd `reopen` in the
// write-client; a status write does it (O7: status goes through update -s).
func (s *Server) handleReopen(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.writeAndRefresh(w, r, id, func(ctx context.Context, src IssueSource) (*bd.Issue, error) {
		iss, err := src.Update(ctx, id, "status", statusOpen)
		return iss, wrapWrite("reopen", err)
	})
}

// wrapWrite tags a write failure with the action so the surfaced message names
// what the user tried; nil stays nil. bd's own message is preserved via %w.
func wrapWrite(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", action, err)
}

// writeAndRefresh runs a write and redraws the drawer from bd's truth — never an
// optimistic guess (spec Q2). A successful write hands back the fresh issue, so
// the common path needs no second read. On failure (or a silent write that
// returned no issue) it re-reads, so the drawer shows the unchanged value next
// to bd's error message; the UI never claims a change that didn't land. If that
// re-read also fails, fall back to the hard error page.
func (s *Server) writeAndRefresh(w http.ResponseWriter, r *http.Request, id string, write func(context.Context, IssueSource) (*bd.Issue, error)) {
	ctx, cancel := reqContext(r)
	defer cancel()
	src, _, ok := s.source()
	if !ok {
		s.renderError(w, errNoRepo)
		return
	}
	issue, writeErr := write(ctx, src)
	if writeErr != nil || issue == nil {
		fresh, showErr := src.Show(ctx, id)
		if showErr != nil {
			s.renderError(w, showErr)
			return
		}
		issue = fresh
	}
	s.renderDrawer(ctx, w, src, issue, writeErr)
}

// handleComment adds a comment from the drawer's compose box. It routes through
// writeAndRefresh like the other writes: a successful add returns no issue, so
// the redraw re-reads the bead — and renderDrawer reloads the thread, now with
// the new comment. An empty comment surfaces bd's error inline.
func (s *Server) handleComment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	text := r.FormValue("text")
	s.writeAndRefresh(w, r, id, func(ctx context.Context, src IssueSource) (*bd.Issue, error) {
		return nil, wrapWrite("comment", src.Comment(ctx, id, text))
	})
}

// handleNewForm renders the empty create form into the drawer. New beads default
// to a task at P2 so the common case is one field (a title) away from created.
func (s *Server) handleNewForm(w http.ResponseWriter, _ *http.Request) {
	s.render(w, "createForm", createForm{Type: "task", Priority: "2"})
}

// createForm is the new-bead form's state: the raw field values (so a failed
// submit re-renders with what the user typed) plus a bd error to show inline.
type createForm struct {
	Title       string
	Type        string
	Priority    string
	Description string
	Err         string
}

// handleCreate runs bd create from the form. On success it shows the new bead's
// drawer and fires refreshList so the list pane picks up the addition; on failure
// it re-renders the form with bd's message and the user's input intact.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	src, _, ok := s.source()
	if !ok {
		s.renderError(w, errNoRepo)
		return
	}
	form := createForm{
		Title:       r.FormValue("title"),
		Type:        r.FormValue("type"),
		Priority:    r.FormValue("priority"),
		Description: r.FormValue("description"),
	}
	opts := bd.CreateOpts{Title: form.Title, Type: form.Type, Description: form.Description}
	if p, err := strconv.Atoi(form.Priority); err == nil {
		opts.Priority = &p
	}
	issue, err := src.Create(ctx, opts)
	if err != nil {
		form.Err = err.Error()
		s.render(w, "createForm", form)
		return
	}
	w.Header().Set("HX-Trigger", "refreshList")
	s.renderDrawer(ctx, w, src, issue, nil)
}

// handleDeletePreview runs bd's bare delete — the free confirm step (O5). It
// shows what would be removed and a button to commit the deletion; nothing is
// destroyed yet.
func (s *Server) handleDeletePreview(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	src, _, ok := s.source()
	if !ok {
		s.renderError(w, errNoRepo)
		return
	}
	id := r.PathValue("id")
	preview, err := src.DeletePreview(ctx, id)
	if err != nil {
		s.renderError(w, err)
		return
	}
	s.render(w, "deleteConfirm", deleteData{ID: id, Preview: preview})
}

// deleteData drives the confirm panel: the id to delete and bd's preview text.
type deleteData struct {
	ID      string
	Preview string
}

// handleDelete commits the deletion (bd delete --force) after the confirm step,
// then shows a tombstone and fires refreshList so the gone bead leaves the list.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	src, _, ok := s.source()
	if !ok {
		s.renderError(w, errNoRepo)
		return
	}
	if err := src.Delete(ctx, r.PathValue("id")); err != nil {
		s.renderError(w, err)
		return
	}
	w.Header().Set("HX-Trigger", "refreshList")
	s.render(w, "deleted", nil)
}

// buildForest pulls the active repo's live issue list once and folds it into the
// landing model, labeling the region with the active repo's name (the synthesis
// project follows the active repo, so a switch re-labels every view).
func (s *Server) buildForest(ctx context.Context, src IssueSource, repo registry.Repo) (forest.Forest, error) {
	issues, err := src.List(ctx)
	if err != nil {
		return forest.Forest{}, fmt.Errorf("list issues: %w", err)
	}
	return forest.Build(issues, s.synFor(repo)), nil
}

// synFor labels the synthesis with the active repo's name; the project follows the
// active repo, so a switch re-labels every view. Shared by buildForest and the
// insights handler (which lists issues itself, so can't go through buildForest).
func (s *Server) synFor(repo registry.Repo) forest.Synthesis {
	syn := s.syn
	syn.Project = repo.Name
	return syn
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	if err := s.renderStatus(w, name, data, http.StatusOK); err != nil {
		log.Printf("strand: %v", err)
		s.renderError(w, err)
	}
}

// renderStatus renders a template into a buffer first, then writes it with the
// given status — so a template failure becomes a clean error instead of a 200
// with a half-written body. On failure it writes nothing and returns the error.
func (s *Server) renderStatus(w http.ResponseWriter, name string, data any, code int) error {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		return fmt.Errorf("render %q: %w", name, err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_, _ = buf.WriteTo(w)
	return nil
}

// renderError sends an HTML error fragment with the status mapped from the bd
// error, so htmx and a plain browser both show something legible. If the error
// template itself fails, it falls back to a plaintext error.
func (s *Server) renderError(w http.ResponseWriter, err error) {
	code := statusForError(err)
	if rerr := s.renderStatus(w, "error", err.Error(), code); rerr != nil {
		log.Printf("strand: render error page: %v", rerr)
		http.Error(w, err.Error(), code)
	}
}

// statusForError maps a bd error to an HTTP status so the UI can tell a missing
// issue (404) from bad input (400) from a real upstream failure (502). An error
// from no bd sentinel (e.g. a template failure) is ours: 500.
func statusForError(err error) int {
	switch {
	case errors.Is(err, bd.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, bd.ErrInvalidArg):
		return http.StatusBadRequest
	case errors.Is(err, bd.ErrBD):
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}
