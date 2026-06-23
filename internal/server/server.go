// Package server is strand's HTTP layer: it renders the embedded web UI as HTML
// (html/template) and swaps fragments over htmx. It reads beads through a small
// issue source so the bd CLI stays the only data path (spec D8).
package server

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	errNoRepo    = errors.New("no active repo")
	errNoIssue   = errors.New("no issue to display")
	errCrossSite = errors.New("cross-site request blocked")
)

// Cross-site guard header names and the one Sec-Fetch-Site value that is
// unambiguously hostile. Named constants keep the header strings out of the
// logic and out of goconst's sights.
const (
	headerSecFetchSite = "Sec-Fetch-Site"
	headerOrigin       = "Origin"
	secFetchCrossSite  = "cross-site"
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
	SetRank(ctx context.Context, id string, rank float64) (*bd.Issue, error)
	Comment(ctx context.Context, id, text string) error
	DepAdd(ctx context.Context, id, dependsOn string) error
	DepRemove(ctx context.Context, id, dependsOn string) error
	LabelAdd(ctx context.Context, id, label string) error
	LabelRemove(ctx context.Context, id, label string) error
	Create(ctx context.Context, opts bd.CreateOpts) (*bd.Issue, error)
	DeletePreview(ctx context.Context, id string) (string, error)
	Delete(ctx context.Context, id string) error
}

// readSource is the read-only slice of IssueSource the pure-read helpers need:
// the full-repo List (buildForest), the dependency edges (insightsModel,
// renderDrawer), and the comment thread (renderDrawer). Narrowing the read paths
// to this seam keeps the write methods (Update/Claim/SetRank/…) out of reach where
// only reads happen, so a read helper can't grow a stray write. Any IssueSource —
// the real *bd.Client, the caching wrapper, a test stub — satisfies it for free.
type readSource interface {
	List(ctx context.Context, args ...string) ([]bd.Issue, error)
	Deps(ctx context.Context, ids ...string) ([]bd.DepEdge, error)
	Comments(ctx context.Context, id string) ([]bd.Comment, error)
}

// Compile-time proof the fat source and its caching wrapper still satisfy the
// narrow read seam, so the read helpers accept whatever source() hands back.
var (
	_ readSource = (IssueSource)(nil)
	_ readSource = (*cachingSource)(nil)
)

// SourceFunc builds the bd-backed issue source for a repo. It is the seam the
// command wires (a real bd.Client scoped to the repo's path) and tests stub (an
// in-memory fake), so switching the active repo re-scopes every read and write
// without the server knowing how a source is made.
type SourceFunc func(registry.Repo) IssueSource

// writeFunc runs one bd write through the request's issue source and hands back
// the fresh issue (or nil) plus any error. Named so the write handlers and
// writeAndRefresh share one scannable contract instead of respelling it inline.
type writeFunc func(context.Context, IssueSource) (*bd.Issue, error)

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
	cache    *snapshotCache   // per-repo in-process read cache (strand-9s6)
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
	s := &Server{srcFor: srcFor, reg: reg, tmpl: tmpl, static: static, syn: syn, shutdown: defaultShutdown, now: time.Now}
	s.cache = newSnapshotCache(func() time.Time { return s.now() })
	return s
}

// source resolves the active repo's issue source. ok is false when no repo is
// active, which the landing renders as the empty state.
func (s *Server) source() (IssueSource, registry.Repo, bool) {
	repo, ok := s.reg.Active()
	if !ok {
		return nil, registry.Repo{}, false
	}
	// Wrap the bd source in the snapshot cache, keyed by the active repo's path.
	// Reads (List/Deps) are served from the per-repo snapshot; writes pass through
	// and drop that repo's snapshot. Keying on the path makes a repo switch a
	// natural miss — the new repo has no entry — so switching re-scopes for free.
	return &cachingSource{IssueSource: s.srcFor(repo), cache: s.cache, repo: repo.Path}, repo, true
}

// Routes returns the mux: the forest page, its htmx fragments, and static assets.
// Every mutating route (POST/PATCH/DELETE) is registered through mutate, which
// wraps the handler in the cross-site guard; the GET reads stay open so the same
// guard can never break a plain navigation (spec: gate writes, not reads).
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleForest)
	mux.HandleFunc("GET /list", s.handleList)
	mux.HandleFunc("GET /board", s.handleBoard)
	mux.HandleFunc("GET /insights", s.handleInsights)
	mux.HandleFunc("GET /bead/{id}", s.handleBead)
	s.mutate(mux, "PATCH /bead/{id}", s.handleEdit)
	s.mutate(mux, "POST /bead/{id}/move", s.handleMove)
	s.mutate(mux, "POST /bead/{id}/rank", s.handleRank)
	s.mutate(mux, "POST /bead/{id}/claim", s.handleClaim)
	s.mutate(mux, "POST /bead/{id}/close", s.handleClose)
	s.mutate(mux, "POST /bead/{id}/reopen", s.handleReopen)
	s.mutate(mux, "POST /bead/{id}/comment", s.handleComment)
	s.mutate(mux, "POST /bead/{id}/dep", s.handleDepAdd)
	s.mutate(mux, "POST /bead/{id}/dep/remove", s.handleDepRemove)
	s.mutate(mux, "POST /bead/{id}/label", s.handleLabelAdd)
	s.mutate(mux, "POST /bead/{id}/label/remove", s.handleLabelRemove)
	s.mutate(mux, "POST /bead/{id}/delete", s.handleDeletePreview)
	s.mutate(mux, "DELETE /bead/{id}", s.handleDelete)
	mux.HandleFunc("GET /new", s.handleNewForm)
	s.mutate(mux, "POST /new", s.handleCreate)
	mux.HandleFunc("GET /repos", s.handleRepos)
	s.mutate(mux, "POST /repos/rescan", s.handleRescan)
	s.mutate(mux, "POST /repo", s.handleSwitchRepo)
	s.mutate(mux, "POST /repo/add", s.handleAddRepo)
	s.mutate(mux, "POST /shutdown", s.handleShutdown)
	mux.Handle("GET /static/", s.static)
	return mux
}

// mutate registers a state-changing handler behind the cross-site guard. Every
// non-GET route goes through here so the check lives in one place — a new write
// route is guarded by construction, not by remembering to wrap it.
func (s *Server) mutate(mux *http.ServeMux, pattern string, h http.HandlerFunc) {
	mux.Handle(pattern, s.guardCrossSite(h))
}

// guardCrossSite rejects browser forms POSTing from another origin — the CSRF
// vector codex flagged on /shutdown, which applies to every write route. It is
// not token-based CSRF: this is a local single-user tool, so a header check is
// the right weight (no cookies, no tokens).
//
// Decision order:
//   - Sec-Fetch-Site (modern browsers send it on every request): allow
//     same-origin / same-site / none, reject cross-site. This is authoritative
//     when present because the browser sets it, not the page.
//   - else Origin (older browsers, or fetch without Sec-Fetch-Site): allow only
//     when its host matches the request Host; a mismatch is cross-site.
//   - neither header: allow. A request with no Origin and no Sec-Fetch-Site is a
//     CLI client (curl) or a same-origin htmx call on a client that omits both —
//     not a cross-site browser form, which is the only thing being blocked.
func (s *Server) guardCrossSite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameSite(r) {
			s.renderForbidden(w)
			return
		}
		next(w, r)
	}
}

// sameSite reports whether r is safe to mutate from — same-origin, or no
// cross-site browser signal at all. See guardCrossSite for the decision order.
func sameSite(r *http.Request) bool {
	if site := r.Header.Get(headerSecFetchSite); site != "" {
		return site != secFetchCrossSite
	}
	if origin := r.Header.Get(headerOrigin); origin != "" {
		return originMatchesHost(origin, r.Host)
	}
	return true
}

// originMatchesHost reports whether an Origin URL's host equals the request
// Host. A malformed Origin, or one with no host, is treated as a mismatch.
func originMatchesHost(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, host)
}

// renderForbidden answers a rejected cross-site write with the same error
// fragment the read errors use, at 403 — so htmx and a plain browser both show
// something legible rather than a bare status.
func (s *Server) renderForbidden(w http.ResponseWriter) {
	if rerr := s.renderStatus(w, "error", errCrossSite.Error(), http.StatusForbidden); rerr != nil {
		log.Printf("strand: render forbidden page: %v", rerr)
		http.Error(w, errCrossSite.Error(), http.StatusForbidden)
	}
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
	{"status", []boardColumn{{Key: string(bd.StatusOpen), Label: "open"}, {Key: string(bd.StatusInProgress), Label: "in progress"}, {Key: string(bd.StatusBlocked), Label: "blocked"}, {Key: string(bd.StatusClosed), Label: "closed"}}, func(b *forest.Bead) string { return string(b.Status) }},
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

// handleRank persists a drag-to-reorder of the V1 bead list. SortableJS posts only
// the post-drop order (comma-separated ids of the affected epic group); the server
// is authoritative — it re-reads ranks from bd, computes the minimal write, and
// stores it back as bd metadata (D5). The client keeps its optimistic DOM, so on
// success the response is 204 (no swap, no Sortable re-init churn); a write error
// renders the error fragment at a non-2xx status, the client's revert signal.
//
// Two write paths preserve the pure-rank-after-seed invariant (forest.sortBeads):
// a group with no manual rank yet is seeded with dense ranks 1..N over the new
// order; an already-ranked group moves one bead to the midpoint of its new
// neighbors (or just past an edge), renormalizing the whole group only when float
// space between neighbors is exhausted.
func (s *Server) handleRank(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	src, repo, ok := s.source()
	if !ok {
		s.renderError(w, errNoRepo)
		return
	}
	order := splitIDs(r.FormValue("order"))
	if len(order) <= 1 {
		w.WriteHeader(http.StatusNoContent) // nothing to order
		return
	}

	f, err := s.buildForest(ctx, src, repo)
	if err != nil {
		s.renderError(w, err)
		return
	}
	ranks, present, allRanked := groupRanks(f, order)

	if !allRanked {
		if err := seedRanks(ctx, src, order, present); err != nil {
			s.renderError(w, err) // seedRanks already wraps with wrapWrite
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	moved := movedID(order, ranks)
	newRank, renorm := rankFor(order, ranks, moved)
	if renorm {
		if err := seedRanks(ctx, src, order, present); err != nil {
			s.renderError(w, err) // seedRanks already wraps with wrapWrite
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if _, err := src.SetRank(ctx, moved, newRank); err != nil {
		s.renderError(w, wrapWrite("rank", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// splitIDs parses a comma-separated id list, dropping blanks so a trailing comma
// or empty field never yields a phantom "" id.
func splitIDs(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// walkBeads visits every bead in the forest. It flattens the region/epic/bead
// nesting so callers read as one loop, not three.
func walkBeads(f forest.Forest, fn func(forest.Bead)) {
	for ri := range f.Regions {
		for ei := range f.Regions[ri].Epics {
			for _, b := range f.Regions[ri].Epics[ei].Beads {
				fn(b)
			}
		}
	}
}

// groupRanks reads the current rank of every id in order from the forest and
// reports which ids the forest actually yielded (present) and whether the whole
// group is already manually ranked. A group that is not wholly ranked must be
// seeded, not midpoint-inserted (the sortBeads invariant). An id the forest never
// yielded (closed mid-drag, say) reads as not-ranked and not-present, so a partial
// group seeds over only its live members rather than mis-inserting.
func groupRanks(f forest.Forest, order []string) (ranks map[string]float64, present map[string]bool, allRanked bool) {
	want := make(map[string]bool, len(order))
	for _, id := range order {
		want[id] = true
	}
	ranks = make(map[string]float64, len(order))
	present = make(map[string]bool, len(order))
	unranked := false
	walkBeads(f, func(b forest.Bead) {
		if !want[b.ID] {
			return
		}
		present[b.ID] = true
		if b.HasRank {
			ranks[b.ID] = b.Rank
		} else {
			unranked = true
		}
	})
	return ranks, present, !unranked && len(present) == len(order)
}

// movedID finds the one bead a single drag relocated: deleting it from the posted
// order yields the same sequence as deleting it from the prior (rank-sorted) order.
// A pure swap matches on either element; the first is a fine, stable choice.
func movedID(order []string, ranks map[string]float64) string {
	prior := make([]string, len(order))
	copy(prior, order)
	slices.SortStableFunc(prior, func(a, b string) int {
		if c := cmp.Compare(ranks[a], ranks[b]); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	for _, m := range order {
		if slices.Equal(without(order, m), without(prior, m)) {
			return m
		}
	}
	return order[0] // unreachable for a real single move; safe default
}

// without returns ids with the first occurrence of m removed.
func without(ids []string, m string) []string {
	out := make([]string, 0, len(ids))
	dropped := false
	for _, id := range ids {
		if !dropped && id == m {
			dropped = true
			continue
		}
		out = append(out, id)
	}
	return out
}

// rankFor computes the new rank for the moved bead from its neighbors in the
// post-drop order: the midpoint of the two it now sits between, or one step past
// the edge it now leads or trails. It returns renorm=true when the neighbors leave
// no representable gap (float exhaustion), the signal to reseed the whole group.
func rankFor(order []string, ranks map[string]float64, moved string) (rank float64, renorm bool) {
	j := slices.Index(order, moved)
	switch {
	case j <= 0: // new head: just below the next bead
		next := ranks[order[1]]
		r := next - 1
		return r, r >= next
	case j >= len(order)-1: // new tail: just above the prior bead
		prev := ranks[order[j-1]]
		r := prev + 1
		return r, r <= prev
	default: // interior: midpoint of the two neighbors
		prev, next := ranks[order[j-1]], ranks[order[j+1]]
		r := prev + (next-prev)/2
		return r, r <= prev || r >= next
	}
}

// seedRanks writes dense ranks 1..M over the live ids in order, making the group
// wholly rank-ordered. Used to seed an untouched group on its first drag and to
// renormalize when midpoint space runs out. An id absent from present (closed
// mid-drag) is skipped so no rank lands on a bead the forest no longer shows; the
// counter only advances on a write, keeping the survivors' ranks dense.
func seedRanks(ctx context.Context, src IssueSource, order []string, present map[string]bool) error {
	rank := 1
	for _, id := range order {
		if !present[id] {
			continue
		}
		if _, err := src.SetRank(ctx, id, float64(rank)); err != nil {
			return wrapWrite("rank", err)
		}
		rank++
	}
	return nil
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

// blocksEdges keeps the in-scope "blocks" dependencies as gonum compute-edges.
// Edges of another type, or with an endpoint outside the scope, are dropped so the
// DAG stays closed over the visible nodes.
func blocksEdges(deps []bd.DepEdge, inScope map[string]bool) []graph.Edge {
	compute := make([]graph.Edge, 0, len(deps))
	for _, d := range deps {
		if d.Type != "blocks" || !inScope[d.IssueID] || !inScope[d.DependsOnID] {
			continue
		}
		compute = append(compute, graph.Edge{Dependent: d.IssueID, Dependency: d.DependsOnID})
	}
	return compute
}

// insightsView wraps the scope chrome (so the fragment reuses the list/board
// head) with the computed dashboard.
type insightsView struct {
	listView
	Insights insights
}

// insights is the V4 dashboard (spec §10): quick-ref counts plus the panels over
// strand's own in-process metrics. Structural panels (Influence, Bottlenecks,
// CritPath) read the in-scope closed graph; the triage counts read all blockers
// (a bead can be blocked from outside the scope).
type insights struct {
	Counts     triageCounts
	Ready      []rankedBead // ready beads ranked by influence — the dispatch queue
	Influence  []rankedBead // top PageRank — foundational beads
	Bottleneck []rankedBead // top betweenness — chokepoints
	CritPath   []forest.Bead
	Labels     []labelCount // label distribution over open beads, descending
	Untagged   int          // open beads carrying no label at all
}

// triageCounts is the quick-ref panel: the live shape of the scope's queue.
type triageCounts struct {
	Total, Open, InProgress, Ready, Blocked, Stale int
}

// rankedBead is one leaderboard row: a bead, its raw metric score, and a 0–100 bar
// width normalized to the panel's top score (computed in Go so the template is dumb).
// Blocked/Stale are the act-now cross-flags: a high-rank row that ALSO sits in the
// blocked or stale set is the one item worth acting on now (spec §3, cross-flag).
type rankedBead struct {
	forest.Bead
	Score   float64
	Width   int
	Blocked bool
	Stale   bool
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
// epic), mirroring handleBoard. No repo or an empty scope renders the same empty
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
	issues, err := src.List(ctx, allIssues...)
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
func (s *Server) insightsModel(ctx context.Context, src readSource, v *listView, issues []bd.Issue) (insights, error) {
	// Epics are containers, not actionable work: the dashboard is about the queue
	// and the structure of real tasks, so it drops them.
	beads := actionable(scopeBeads(v))
	ids, inScope := scopeIDs(beads)

	var deps []bd.DepEdge
	if len(ids) > 0 {
		var err error
		if deps, err = src.Deps(ctx, ids...); err != nil {
			return insights{}, fmt.Errorf("insights deps: %w", err)
		}
	}
	compEdges := blocksEdges(deps, inScope)
	m := graph.Compute(ids, compEdges)

	idx := indexIssues(issues)
	// One blocker scan per request: triage, the ready queue, and both leaderboards
	// all read the same open-blocker tallies, so compute once and share the map.
	openBlockers := blockerCounts(deps, idx)
	out := insights{
		Counts:   triage(beads, openBlockers, idx, s.now()),
		CritPath: beadPath(m.CriticalPath, beadByID(beads)),
		Labels:   labelHealth(beads, idx),
		Untagged: untaggedOpen(beads, idx),
	}
	// The dispatch queue: ready beads ranked by influence, so the count→actionable
	// gap closes (triage says "2 ready"; this says WHICH, most-impactful first). Ranks
	// even without edges — every ready bead is dispatchable, ordered by PageRank base.
	out.Ready = readyQueue(beads, openBlockers, idx, m.PageRank, s.now())
	// The leaderboards rank by graph position; with no dependencies every bead ties
	// at PageRank's base rank, so a ranking would be noise. Show them only with edges.
	// crossFlag marks the rows that ALSO sit in the blocked/stale sets — the act-now signal.
	if len(compEdges) > 0 {
		out.Influence = crossFlag(leaderboard(beads, m.PageRank), openBlockers, idx, s.now())
		out.Bottleneck = crossFlag(leaderboard(beads, m.Betweenness), openBlockers, idx, s.now())
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
func triage(beads []forest.Bead, openBlockers map[string]int, idx map[string]bd.Issue, now time.Time) triageCounts {
	var c triageCounts
	for i := range beads {
		b := &beads[i]
		c.Total++
		switch b.Status {
		case bd.StatusInProgress:
			c.InProgress++
		case bd.StatusOpen:
			c.Open++
			if openBlockers[b.ID] > 0 {
				c.Blocked++
			} else {
				c.Ready++
			}
		case bd.StatusBlocked:
			c.Blocked++
		case bd.StatusClosed, bd.StatusDeferred:
			// Not live work; the forest filter already drops these, so they
			// don't reach a count — listed to keep the status set exhaustive.
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
		if iss, ok := idx[d.DependsOnID]; ok && iss.Status != bd.StatusClosed {
			open[d.IssueID]++
		}
	}
	return open
}

// isStale reports whether live (open/in-progress) work has gone untouched past the
// cutoff. A zero timestamp (never recorded) is not flagged — absence isn't staleness.
func isStale(status bd.Status, updated, now time.Time) bool {
	if status == bd.StatusClosed || status == bd.StatusDeferred || updated.IsZero() {
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
	return rankBoard(ranked)
}

// rankBoard is the shared tail of every insights board: sort by score (descending,
// ID tiebreak), cap at leaderboardSize, and size each row's bar against the leader.
// The top>0 guard makes it safe for the ready queue, whose rows can score zero.
func rankBoard(board []rankedBead) []rankedBead {
	slices.SortFunc(board, func(a, b rankedBead) int {
		if a.Score != b.Score {
			return cmp.Compare(b.Score, a.Score) // descending
		}
		return cmp.Compare(a.ID, b.ID) // stable tiebreak
	})
	if len(board) > leaderboardSize {
		board = board[:leaderboardSize]
	}
	if len(board) > 0 && board[0].Score > 0 {
		top := board[0].Score
		for i := range board {
			board[i].Width = int(board[i].Score / top * 100)
		}
	}
	return board
}

// readyQueue is the dispatch queue: the scope's ready beads (open, no unmet blocker),
// ranked by influence (PageRank) descending so the most-impactful dispatch sits on top.
// It closes the count→actionable gap — triage says how many are ready, this says which.
// Rows carry the stale cross-flag (a ready bead can still have gone cold); ready beads
// are by definition not blocked, so Blocked stays false here.
func readyQueue(beads []forest.Bead, openBlockers map[string]int, idx map[string]bd.Issue, score map[string]float64, now time.Time) []rankedBead {
	ready := make([]rankedBead, 0, len(beads))
	for i := range beads {
		b := &beads[i]
		if b.Status != bd.StatusOpen || openBlockers[b.ID] > 0 {
			continue
		}
		ready = append(ready, rankedBead{
			Bead:  *b,
			Score: score[b.ID],
			Stale: isStale(b.Status, idx[b.ID].UpdatedAt, now),
		})
	}
	return rankBoard(ready)
}

// crossFlag marks each ranked row that ALSO sits in the blocked or stale set — the
// act-now signal (spec §3): a high-rank bottleneck that is itself blocked/stale is the
// one item worth acting on now. Blocked weighs unmet blocks-dependencies (a bd-reported
// "blocked" status also counts); stale reuses the triage cutoff.
func crossFlag(board []rankedBead, openBlockers map[string]int, idx map[string]bd.Issue, now time.Time) []rankedBead {
	for i := range board {
		id := board[i].ID
		board[i].Blocked = openBlockers[id] > 0 || board[i].Status == bd.StatusBlocked
		board[i].Stale = isStale(board[i].Status, idx[id].UpdatedAt, now)
	}
	return board
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
		if beads[i].Status != bd.StatusOpen {
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
		if beads[i].Status == bd.StatusOpen && len(idx[beads[i].ID].Labels) == 0 {
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
	Blockers []string // ids this bead depends on (its in-bd "blocks" blockers)
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
func (s *Server) renderDrawer(ctx context.Context, w http.ResponseWriter, src readSource, issue *bd.Issue, writeErr error) {
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
	if deps, err := src.Deps(ctx, issue.ID); err == nil {
		data.Blockers = blockerIDs(deps, issue.ID)
	}
	s.render(w, "drawer", data)
}

// blockerIDs pulls the ids that bead id depends on from its dependency edges,
// keeping only "blocks" (epic parent-child and soft links aren't blockers). The
// down-direction Deps query is id-centric, so every edge's IssueID is id; the
// guard is defensive in case bd folds in other rows.
func blockerIDs(deps []bd.DepEdge, id string) []string {
	var ids []string
	for _, d := range deps {
		if d.Type == "blocks" && d.IssueID == id {
			ids = append(ids, d.DependsOnID)
		}
	}
	return ids
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
		iss, err := src.Update(ctx, id, "status", string(bd.StatusOpen))
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
func (s *Server) writeAndRefresh(w http.ResponseWriter, r *http.Request, id string, write writeFunc) {
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

// handleDepAdd wires a "blocks" dependency from the drawer: the open bead depends
// on (is blocked by) the posted bead. Like handleComment it returns no issue, so
// the redraw re-reads — and renderDrawer reloads the blocker list with the new edge.
func (s *Server) handleDepAdd(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	on := strings.TrimSpace(r.FormValue("depends_on"))
	s.writeAndRefresh(w, r, id, func(ctx context.Context, src IssueSource) (*bd.Issue, error) {
		return nil, wrapWrite("dep add", src.DepAdd(ctx, id, on))
	})
}

// handleDepRemove drops a "blocks" dependency from the drawer's blocker list.
func (s *Server) handleDepRemove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	on := r.FormValue("depends_on")
	s.writeAndRefresh(w, r, id, func(ctx context.Context, src IssueSource) (*bd.Issue, error) {
		return nil, wrapWrite("dep remove", src.DepRemove(ctx, id, on))
	})
}

// handleLabelAdd attaches a label from the drawer. A key-value pair posts as a
// single `key=value` value; the form joins the two inputs, so this handler only
// trims and forwards. Like the dep writes it returns no issue, so the redraw
// re-reads — and renderDrawer reflows the chip list with the new label.
func (s *Server) handleLabelAdd(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	label := labelFromForm(r)
	s.writeAndRefresh(w, r, id, func(ctx context.Context, src IssueSource) (*bd.Issue, error) {
		return nil, wrapWrite("label add", src.LabelAdd(ctx, id, label))
	})
}

// handleLabelRemove detaches a label from the drawer's chip/pair list.
func (s *Server) handleLabelRemove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	label := strings.TrimSpace(r.FormValue("label"))
	s.writeAndRefresh(w, r, id, func(ctx context.Context, src IssueSource) (*bd.Issue, error) {
		return nil, wrapWrite("label remove", src.LabelRemove(ctx, id, label))
	})
}

// labelFromForm reads the add form's value. A plain chip posts `label`; a
// key-value pair posts `key` + `value`, which join into the `key=value` label
// bd stores (the encoding the view layer owns). A bare key (no value) stays a
// plain label so the field can't silently drop the `=`.
func labelFromForm(r *http.Request) string {
	if key := strings.TrimSpace(r.FormValue("key")); key != "" {
		if val := strings.TrimSpace(r.FormValue("value")); val != "" {
			return key + "=" + val
		}
		return key
	}
	return strings.TrimSpace(r.FormValue("label"))
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
func (s *Server) buildForest(ctx context.Context, src readSource, repo registry.Repo) (forest.Forest, error) {
	issues, err := src.List(ctx, allIssues...)
	if err != nil {
		return forest.Forest{}, fmt.Errorf("list issues: %w", err)
	}
	return forest.Build(issues, s.synFor(repo)), nil
}

// allIssues lifts bd list's default 50-row cap. The forest folds each issue into
// its trunk by walking the parent chain, so a truncated list breaks the laddering
// — a tile lands off-trunk the moment one ancestor row is missing. Fetch them all.
var allIssues = []string{"--limit", "0"}

// synFor labels the synthesis with the active repo's name; the project follows the
// active repo, so a switch re-labels every view. Shared by buildForest and the
// insights handler (which lists issues itself, so can't go through buildForest).
func (s *Server) synFor(repo registry.Repo) forest.Synthesis {
	syn := s.syn
	syn.Project = repo.Name
	return syn
}

// snapshotCache holds one in-process read snapshot per repo, keyed by the repo's
// path. Each view (forest/list/board/insights) used to shell out to `bd
// list --limit 0` (~0.5s, opens the Dolt store) and insights added a `dep
// list` call; the compute over the result is in-memory and negligible, so the bd
// subprocess spawn is the whole cost, paid back-to-back through execMu on a
// multi-fragment page. The snapshot folds the List result and the Deps result for
// one repo so every view after the first hits memory.
//
// strand is the SOLE writer to its repos, so the snapshot stays correct by
// invalidating on every successful write (cachingSource's write methods drop the
// repo's entry) and on repo switch (a switched-to path is a different key, hence a
// miss — no explicit hook needed). execMu in package bd is untouched: it still
// serializes the bd calls that do happen, but the cache removes the contention by
// removing the calls.
//
// Out-of-band staleness — a bd CLI run or another agent editing the same repo's
// store while strand holds a snapshot — is the one case writes can't catch. The
// documented mitigation is a TTL floor: an entry older than cacheTTL is treated as
// a miss, so out-of-band edits surface within a few seconds without any UI work.
// The manual-reload path is the browser refresh / re-navigation that re-issues the
// GET after the TTL lapses; no template change is needed for it.
type snapshotCache struct {
	mu      sync.Mutex
	now     func() time.Time
	gen     uint64
	entries map[string]*snapshot
}

// snapshot is one repo's cached reads: the full `list --limit 0` result and the
// repo-wide Deps result, stamped with the wall time they were fetched so the TTL
// floor can age them out. List and Deps share one entry (and the TTL stamp set at
// the List fetch) so forest/list/insights are one logical snapshot — and Deps is
// fetched once over the whole repo, not per scope, so the second structural view
// reuses the first's edges (depsOK distinguishes "no deps" from "not fetched yet").
//
// gen is the snapshot's identity: a monotonic stamp set at putList that lets a
// late Deps fetch tell whether the snapshot it read the ids from is still the one
// it's about to write deps into (see putDeps). It is clock-independent, so the
// fixed-clock tests still distinguish versions even when every `at` is equal.
type snapshot struct {
	at     time.Time
	gen    uint64
	list   []bd.Issue
	deps   []bd.DepEdge
	depsOK bool
}

// cacheTTL is the out-of-band staleness backstop: a snapshot older than this is a
// miss, so a bd CLI / fleet edit to the same repo surfaces on the next navigation
// within a few seconds even though strand didn't make the write. It is a floor,
// not a refresh interval — writes still invalidate immediately, so the steady
// single-writer path never waits on it. Kept short (the bead's ~2-5s window).
const cacheTTL = 3 * time.Second

func newSnapshotCache(now func() time.Time) *snapshotCache {
	return &snapshotCache{now: now, entries: map[string]*snapshot{}}
}

// entryLocked returns the repo's snapshot if present and within the TTL floor,
// else nil (a miss). The caller MUST hold c.mu: putDeps mutates an entry's
// deps/depsOK in place, so a reader that escapes the lock with the *snapshot
// races that write (strand-4sd). The public accessors below copy the fields they
// need out under the lock and never hand the pointer to a handler.
func (c *snapshotCache) entryLocked(repo string) *snapshot {
	e, ok := c.entries[repo]
	if !ok || c.now().Sub(e.at) >= cacheTTL {
		return nil
	}
	return e
}

// liveList returns the repo's cached list and true on a live snapshot. The slice
// header is copied out under the lock; its backing array is the shared read-only
// view (see the contract above putList), never mutated after publish.
func (c *snapshotCache) liveList(repo string) ([]bd.Issue, uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entryLocked(repo); e != nil {
		return e.list, e.gen, true
	}
	return nil, 0, false
}

// liveDeps returns the repo-wide dependency edges and true only when a live
// snapshot has them (depsOK). Read under the lock that putDeps writes under, so
// the deps/depsOK fields never race; the returned slice is the shared read-only
// view (see the contract above putList).
func (c *snapshotCache) liveDeps(repo string) ([]bd.DepEdge, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entryLocked(repo); e != nil && e.depsOK {
		return e.deps, true
	}
	return nil, false
}

// Shared-view contract (matches registry.Registry.Repos): a snapshot's list and
// deps slices are published once and never mutated in place. liveList/liveDeps
// hand the same backing array to every concurrent handler, which read it without
// copying and filter into fresh slices. putList replaces the whole *snapshot on a
// refresh rather than appending, so a handler holding an older slice keeps a
// valid, immutable view. Callers must treat the returned slices as read-only.

// putList records a fresh List result, opening the repo's snapshot and stamping it
// with the fetch time the TTL ages against.
func (c *snapshotCache) putList(repo string, list []bd.Issue) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	c.entries[repo] = &snapshot{at: c.now(), gen: c.gen, list: list}
}

// putDeps records the repo-wide Deps result into the snapshot it was fetched for,
// identified by gen (the value liveList returned alongside the ids). It writes only
// when the current snapshot is still that one: an invalidate or a fresh putList
// between the List read and here replaces the entry with a different gen, and the
// stale deps are dropped rather than stapled onto a newer list (the version-skew
// guard). The next read re-warms List, and Deps follows.
func (c *snapshotCache) putDeps(repo string, gen uint64, deps []bd.DepEdge) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[repo]; ok && e.gen == gen {
		e.deps, e.depsOK = deps, true
	}
}

// invalidate drops a repo's snapshot. Every successful write calls this so the
// next read re-fetches bd's truth.
func (c *snapshotCache) invalidate(repo string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, repo)
}

// cachingSource wraps a repo's bd issue source so reads are served from the
// snapshot and writes drop it. It embeds IssueSource, so every method strand
// doesn't override (Show, Comments, the writes' default pass-through) goes
// straight to bd; only List, Deps, and the mutating methods carry cache behavior.
type cachingSource struct {
	IssueSource
	cache *snapshotCache
	repo  string
}

// List serves the repo's cached `list --limit 0` snapshot, fetching once on a miss
// (or after the TTL lapses). The bead's reads pass no per-call filter that would
// change the result (every caller uses allIssues), so one cached list is correct.
func (c *cachingSource) List(ctx context.Context, args ...string) ([]bd.Issue, error) {
	if list, _, ok := c.cache.liveList(c.repo); ok {
		return list, nil
	}
	list, err := c.IssueSource.List(ctx, args...)
	if err != nil {
		return nil, err
	}
	c.cache.putList(c.repo, list)
	return list, nil
}

// Deps serves the repo-wide dependency edges, fetching once over the whole repo on
// a miss and caching the superset. Callers pass a scope's ids (graph/insights) or
// one id (drawer), but every caller already filters the result to its in-scope
// "blocks" edges (blocksEdges / blockerIDs), so a superset is correct and lets all
// structural views share one fetch. On a cold cache it fetches deps for the full
// cached list's ids; if List hasn't run yet it falls back to the requested ids.
func (c *cachingSource) Deps(ctx context.Context, ids ...string) ([]bd.DepEdge, error) {
	if deps, ok := c.cache.liveDeps(c.repo); ok {
		return deps, nil
	}
	fetchIDs, gen := c.repoIDs(ids)
	deps, err := c.IssueSource.Deps(ctx, fetchIDs...)
	if err != nil {
		return nil, err
	}
	c.cache.putDeps(c.repo, gen, deps)
	return deps, nil
}

// repoIDs returns the id set to fetch deps over plus the gen of the snapshot it
// read them from (so putDeps can bind the result to that exact snapshot): every id
// in the cached list (so one fetch covers all scopes), falling back to the caller's
// ids and gen 0 when the list isn't cached yet (a Deps before any List — not a path
// the views take, but safe; gen 0 never matches a live snapshot, so putDeps skips).
func (c *cachingSource) repoIDs(reqIDs []string) ([]string, uint64) {
	list, gen, ok := c.cache.liveList(c.repo)
	if !ok || len(list) == 0 {
		return reqIDs, 0
	}
	ids := make([]string, len(list))
	for i := range list {
		ids[i] = list[i].ID
	}
	return ids, gen
}

// The write methods pass through to bd, then drop the repo's snapshot on success so
// the next read reflects the change (strand is the sole writer — invalidate exactly
// on a successful write). A failed write leaves the snapshot, since nothing changed.

func (c *cachingSource) Update(ctx context.Context, id, field, value string) (*bd.Issue, error) {
	iss, err := c.IssueSource.Update(ctx, id, field, value)
	if err == nil {
		c.cache.invalidate(c.repo)
	}
	return iss, err
}

func (c *cachingSource) Claim(ctx context.Context, id string) (*bd.Issue, error) {
	iss, err := c.IssueSource.Claim(ctx, id)
	if err == nil {
		c.cache.invalidate(c.repo)
	}
	return iss, err
}

func (c *cachingSource) Close(ctx context.Context, id, reason string) (*bd.Issue, error) {
	iss, err := c.IssueSource.Close(ctx, id, reason)
	if err == nil {
		c.cache.invalidate(c.repo)
	}
	return iss, err
}

func (c *cachingSource) SetRank(ctx context.Context, id string, rank float64) (*bd.Issue, error) {
	iss, err := c.IssueSource.SetRank(ctx, id, rank)
	if err == nil {
		c.cache.invalidate(c.repo)
	}
	return iss, err
}

func (c *cachingSource) Comment(ctx context.Context, id, text string) error {
	err := c.IssueSource.Comment(ctx, id, text)
	if err == nil {
		c.cache.invalidate(c.repo)
	}
	return err
}

func (c *cachingSource) DepAdd(ctx context.Context, id, dependsOn string) error {
	err := c.IssueSource.DepAdd(ctx, id, dependsOn)
	if err == nil {
		c.cache.invalidate(c.repo)
	}
	return err
}

func (c *cachingSource) DepRemove(ctx context.Context, id, dependsOn string) error {
	err := c.IssueSource.DepRemove(ctx, id, dependsOn)
	if err == nil {
		c.cache.invalidate(c.repo)
	}
	return err
}

func (c *cachingSource) LabelAdd(ctx context.Context, id, label string) error {
	err := c.IssueSource.LabelAdd(ctx, id, label)
	if err == nil {
		c.cache.invalidate(c.repo)
	}
	return err
}

func (c *cachingSource) LabelRemove(ctx context.Context, id, label string) error {
	err := c.IssueSource.LabelRemove(ctx, id, label)
	if err == nil {
		c.cache.invalidate(c.repo)
	}
	return err
}

func (c *cachingSource) Create(ctx context.Context, opts bd.CreateOpts) (*bd.Issue, error) {
	iss, err := c.IssueSource.Create(ctx, opts)
	if err == nil {
		c.cache.invalidate(c.repo)
	}
	return iss, err
}

func (c *cachingSource) Delete(ctx context.Context, id string) error {
	err := c.IssueSource.Delete(ctx, id)
	if err == nil {
		c.cache.invalidate(c.repo)
	}
	return err
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
