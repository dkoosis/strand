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
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/insight"
	"github.com/dkoosis/strand/internal/jtbd"
	"github.com/dkoosis/strand/internal/registry"
	"github.com/dkoosis/strand/internal/strand"
)

// errNoRepo means no beads workspace is active. The landing turns it into an
// actionable empty state (spec R1); other handlers shouldn't reach it, since the
// UI offers no bead links until a repo is active.
var (
	errNoRepo    = errors.New("no active repo")
	errNoIssue   = errors.New("no issue to display")
	errCrossSite = errors.New("cross-site request blocked")
	// errNoParent rejects a create with no deliberate parent choice — the
	// forced-parent contract (str-6k0.6.2): pick an existing parent, choose
	// no epic, or mint one inline, but never create a bead parentless by
	// omission.
	errNoParent = errors.New("pick a parent, choose no epic, or create a new parent")
	// errNoParentTitle rejects the inline new-parent path with an empty title.
	errNoParentTitle = errors.New("new parent needs a title")
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
	SetParent(ctx context.Context, id, parent string) (*bd.Issue, error)
	Comment(ctx context.Context, id, text string) error
	DepAdd(ctx context.Context, id, dependsOn string) error
	DepRemove(ctx context.Context, id, dependsOn string) error
	LabelAdd(ctx context.Context, id, label string) error
	LabelRemove(ctx context.Context, id, label string) error
	Create(ctx context.Context, opts *bd.CreateOpts) (*bd.Issue, error)
	DeletePreview(ctx context.Context, id string) (string, error)
	Delete(ctx context.Context, id string) error
}

// readSource is the read-only slice of IssueSource the pure-read helpers need:
// the full-repo List (buildStrand), the dependency edges (insightsModel,
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

// Server renders the strand landing and its htmx fragments over the active repo's
// issue source. The active repo (and the known-repo registry) live in reg;
// srcFor turns the active repo into the source each request reads through.
type Server struct {
	srcFor   SourceFunc
	reg      *registry.Registry
	tmpl     *template.Template
	static   http.Handler
	syn      strand.Synthesis
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
func New(srcFor SourceFunc, reg *registry.Registry, tmpl *template.Template, static http.Handler, syn strand.Synthesis) *Server {
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

// Routes returns the mux: the strand page, its htmx fragments, and static assets.
// Every mutating route (POST/PATCH/DELETE) is registered through mutate, which
// wraps the handler in the cross-site guard; the GET reads stay open so the same
// guard can never break a plain navigation (spec: gate writes, not reads).
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleHome)
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
	mux.HandleFunc("GET /attach-epic", s.handleAttachForm)
	s.mutate(mux, "POST /attach-epic", s.handleAttachEpic)
	s.mutate(mux, "POST /refresh", s.handleRefresh)
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

// listView is the bead-list pane: an epic, optionally narrowed to one story.
type listView struct {
	Epic     strand.Epic
	Story    strand.Story
	HasStory bool // false = show the whole epic
	HasEpic  bool // true only for a genuine single-epic scope (not "everything"/bugs/story)
	// ScopeID is the bd id of the bead this scope names — a story (Story.ID) or a
	// real epic (Epic.Key). Empty when the scope has no editable bead behind it
	// ("everything", "bugs", the off-epic catch-all). The pane-head title's
	// open-drawer affordance keys off this, so an epic or story edits through the
	// same gesture the leaf rows already use (str-scn).
	ScopeID string
	// EpicBeadID is the bd id of the story's epic, or "" when the story is off-epic.
	// The story-scoped head's breadcrumb keys off it: a real id renders a crumb that
	// navigates up to the epic; "" renders the "No epic" attach affordance (str-o74).
	// Precomputed here because BeadID is a pointer method the template can't call on
	// the non-addressable Epic value.
	EpicBeadID string
}

// pageData is the full landing render: the strand, the list pane, and the repo
// selector chrome (the active repo's name and the known repos). Empty is true
// when no repo is active, switching the landing to its actionable empty state.
type pageData struct {
	Strand strand.Model
	List   listView
	Repos  repoMenu
	Empty  bool
	AsOf   string // "data as of HH:MM" for the refresh readout; empty when no snapshot
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	src, repo, ok := s.source()
	if !ok {
		s.render(w, "page", pageData{Empty: true, Repos: s.repoMenu("")})
		return
	}
	f, err := s.buildStrand(ctx, src, repo)
	if err != nil {
		s.renderError(w, err)
		return
	}
	// Open warms every view, not just this one: buildStrand warmed the List
	// snapshot, so prefetch the repo-wide Deps too — that makes the first Insights
	// click (the one view needing edges) hit memory instead of paying a cold `dep
	// list` spawn (str-udl). Warm it in the background on a detached context: the
	// landing must not block on a ~0.5s spawn the user may never need, and the
	// request ctx dies the moment handleHome returns. A prefetch failure is
	// non-fatal — the landing renders and Insights fetches its own deps on demand.
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = src.Deps(bgCtx)
	}()
	s.render(w, "page", pageData{
		Strand: f,
		List:   listViewFor(f, "", "", ""),
		Repos:  s.repoMenu(""),
		AsOf:   s.asOf(repo),
	})
}

// handleRefresh drops the active repo's snapshot and tells htmx to reload the
// page. With no time-based expiry (str-udl), a plain reload would serve the warm
// snapshot — so seeing an out-of-band edit (a fleet agent or bd CLI touching the
// same store) needs this deliberate invalidate. The HX-Refresh response header
// makes htmx do a full document reload, which re-runs handleHome and re-warms List
// + Deps from bd's truth. No active repo is a no-op reload.
func (s *Server) handleRefresh(w http.ResponseWriter, _ *http.Request) {
	if _, repo, ok := s.source(); ok {
		s.cache.invalidate(repo.Path)
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// asOf formats the active snapshot's fetch time as "HH:MM" for the refresh
// readout, or "" when no snapshot is warm (the readout then shows nothing rather
// than a misleading zero time).
func (s *Server) asOf(repo registry.Repo) string {
	if at, ok := s.cache.stampedAt(repo.Path); ok {
		return at.Format("15:04")
	}
	return ""
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	src, repo, ok := s.source()
	if !ok {
		s.render(w, "list", listView{})
		return
	}
	f, err := s.buildStrand(ctx, src, repo)
	if err != nil {
		s.renderError(w, err)
		return
	}
	// story=<id> narrows the pane to a single story; absent means the whole epic.
	q := r.URL.Query()
	s.render(w, "list", listViewFor(f, q.Get("story"), q.Get("epic"), q.Get("filter")))
}

// listViewFor builds the bead-list pane from the strand. Scope precedence:
// filter=="bugs" gathers every bug across all epics; else storyID narrows to one
// story; else epicKey narrows to one epic; else the default is the whole strand
// (every epic flattened — "everything"). Story and epic are both searched across
// all epics. An empty strand yields an empty view. The full-page render and the
// htmx swaps all go through here, so the panes, board, and insights can't
// diverge.
func listViewFor(f strand.Model, storyID, epicKey, filter string) listView {
	if len(f.Epics) == 0 {
		return listView{}
	}
	if filter == "bugs" {
		return listView{Epic: bugEpic(f)}
	}
	if storyID != "" {
		if v, ok := findStory(f, storyID); ok {
			return v
		}
	}
	if epicKey != "" {
		if v, ok := findEpic(f, epicKey); ok {
			return v
		}
	}
	return listView{Epic: everythingEpic(f)}
}

// findStory locates the story with the given id and returns the view scoped to it.
func findStory(f strand.Model, storyID string) (listView, bool) {
	for _, e := range f.Epics {
		for _, st := range e.Stories {
			if st.ID == storyID {
				return listView{Epic: e, Story: st, HasStory: true, ScopeID: st.ID, EpicBeadID: e.BeadID()}, true
			}
		}
	}
	return listView{}, false
}

// findEpic locates the epic with the given key and returns the view scoped to it.
func findEpic(f strand.Model, epicKey string) (listView, bool) {
	for _, e := range f.Epics {
		if e.Key == epicKey {
			// The off-epic catch-all has no bead behind it, so its title stays
			// inert; a real epic carries its bd id as the editable scope (str-scn).
			return listView{Epic: e, HasEpic: true, ScopeID: e.BeadID()}, true
		}
	}
	return listView{}, false
}

// everythingEpic flattens every top-level epic into one synthetic scope named
// "Everything", so the unscoped list/board/insights show all live work, not just
// the largest epic. A single-epic strand is returned as-is (it keeps its own
// name — a no-epic project reads as the project, not "Everything").
func everythingEpic(f strand.Model) strand.Epic {
	if len(f.Epics) == 1 {
		return f.Epics[0]
	}
	total := 0
	for _, e := range f.Epics {
		total += len(e.Stories)
	}
	out := strand.Epic{Name: "Everything", Color: f.Epics[0].Color, Stories: make([]strand.Story, 0, total)}
	for _, e := range f.Epics {
		out.Stories = append(out.Stories, e.Stories...)
		out.Open += e.Open
	}
	return out
}

// bugEpic gathers every bug-type bead across all epics into one synthetic scope
// named "Bugs", keeping each bug under its own story (stories with no bug drop
// out). It is the list-side companion to the map's bug dot.
func bugEpic(f strand.Model) strand.Epic {
	out := strand.Epic{Name: "Bugs", Color: f.Epics[0].Color}
	for _, e := range f.Epics {
		for _, st := range e.Stories {
			var bugs []strand.Bead
			for bi := range st.Beads {
				if st.Beads[bi].Type == "bug" {
					bugs = append(bugs, st.Beads[bi])
				}
			}
			if len(bugs) == 0 {
				continue
			}
			scoped := st
			scoped.Beads = bugs
			scoped.Open = len(bugs)
			out.Stories = append(out.Stories, scoped)
			out.Open += len(bugs)
		}
	}
	return out
}

// pivotField is one kanban axis: its name (the bd field a drop writes), the
// canonical ordered columns that always exist as drop targets, and how to read a
// bead's value for it. Co-locating all three means a new pivot is added in one
// place, not spread across a name list, a seed map, and a value switch.
type pivotField struct {
	Name  string
	Seeds []boardColumn
	value func(*strand.Bead) string
}

// pivotFields are the fields the kanban can pivot on, in pivot-bar order (the
// first is the default). Type is omitted: bd has no `update --type`, so a type
// column couldn't accept a drag. The seeds give canonical, ordered columns so
// empty drop targets exist (e.g. an empty "in progress" to drag into); values bd
// reports that aren't seeded (a named assignee) are appended. The assignee
// "unassigned" column writes an empty value — dropping there clears the field.
var pivotFields = []pivotField{
	{"status", []boardColumn{{Key: string(bd.StatusOpen), Label: "open"}, {Key: string(bd.StatusInProgress), Label: "in progress"}, {Key: string(bd.StatusBlocked), Label: "blocked"}, {Key: string(bd.StatusClosed), Label: "closed"}}, func(b *strand.Bead) string { return string(b.Status) }},
	{"priority", []boardColumn{{Key: "0", Label: "P0"}, {Key: "1", Label: "P1"}, {Key: "2", Label: "P2"}, {Key: "3", Label: "P3"}, {Key: "4", Label: "P4"}}, func(b *strand.Bead) string { return strconv.Itoa(b.Priority) }},
	{"assignee", []boardColumn{{Key: "", Label: "unassigned"}}, func(b *strand.Bead) string { return b.Assignee }},
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

// boardView is the kanban render: the same scope the table shows (an epic or one
// story, embedded so the head reuses the list's scope markup), pivoted into
// columns by Pivot. Pivots drives the pivot bar.
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
	Beads []strand.Bead
}

func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	src, repo, ok := s.source()
	if !ok {
		s.render(w, "board", boardView{Pivots: boardPivots, Pivot: boardPivots[0]})
		return
	}
	f, err := s.buildStrand(ctx, src, repo)
	if err != nil {
		s.renderError(w, err)
		return
	}
	q := r.URL.Query()
	view := listViewFor(f, q.Get("story"), q.Get("epic"), q.Get("filter"))
	s.render(w, "board", buildBoard(&view, q.Get("pivot")))
}

// buildBoard pivots the scope's beads into columns. Both the whole-epic and
// single-story scopes flow through here, so the board can't diverge from the table.
func buildBoard(v *listView, pivot string) boardView {
	pivot = pivotOrDefault(pivot)
	return boardView{
		listView: *v,
		Pivot:    pivot,
		Pivots:   boardPivots,
		Columns:  boardColumns(pivot, scopeBeads(v)),
	}
}

// scopeBeads flattens the view's beads: one story's, or every story's in the epic.
func scopeBeads(v *listView) []strand.Bead {
	if v.HasStory {
		return v.Story.Beads
	}
	total := 0
	for i := range v.Epic.Stories {
		total += len(v.Epic.Stories[i].Beads)
	}
	all := make([]strand.Bead, 0, total)
	for i := range v.Epic.Stories {
		all = append(all, v.Epic.Stories[i].Beads...)
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
func boardColumns(pivot string, beads []strand.Bead) []boardColumn {
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
	src, repo, ok := s.source()
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
	b := strand.NewBead(issue)
	b.ResolveJTBD(issue.Description, jtbd.Load(repo.Path))
	s.render(w, "boardCard", b)
}

// handleRank persists a drag-to-reorder of the V1 bead list. SortableJS posts only
// the post-drop order (comma-separated ids of the affected story group); the server
// is authoritative — it re-reads ranks from bd, computes the minimal write, and
// stores it back as bd metadata (D5). The client keeps its optimistic DOM, so on
// success the response is 204 (no swap, no Sortable re-init churn); a write error
// renders the error fragment at a non-2xx status, the client's revert signal.
//
// Two write paths preserve the pure-rank-after-seed invariant (strand.sortBeads):
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

	f, err := s.buildStrand(ctx, src, repo)
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

// walkBeads visits every bead in the strand. It flattens the epic/story/bead
// nesting so callers read as one loop, not three.
func walkBeads(f strand.Model, fn func(strand.Bead)) {
	for ei := range f.Epics {
		for si := range f.Epics[ei].Stories {
			beads := f.Epics[ei].Stories[si].Beads
			for bi := range beads {
				fn(beads[bi])
			}
		}
	}
}

// groupRanks reads the current rank of every id in order from the strand and
// reports which ids the strand actually yielded (present) and whether the whole
// group is already manually ranked. A group that is not wholly ranked must be
// seeded, not midpoint-inserted (the sortBeads invariant). An id the strand never
// yielded (closed mid-drag, say) reads as not-ranked and not-present, so a partial
// group seeds over only its live members rather than mis-inserting.
func groupRanks(f strand.Model, order []string) (ranks map[string]float64, present map[string]bool, allRanked bool) {
	want := make(map[string]bool, len(order))
	for _, id := range order {
		want[id] = true
	}
	ranks = make(map[string]float64, len(order))
	present = make(map[string]bool, len(order))
	unranked := false
	walkBeads(f, func(b strand.Bead) {
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
// mid-drag) is skipped so no rank lands on a bead the strand no longer shows; the
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

// insightsView wraps the scope chrome (so the fragment reuses the list/board
// head) with the computed dashboard. The analytics math lives in internal/insight;
// this view-DTO pairs the scope chrome with the domain model the template binds.
type insightsView struct {
	listView
	Insights insight.Model
}

// handleInsights renders the V4 dashboard for the active scope (an epic or one
// story), mirroring handleBoard. No repo or an empty scope renders the same empty
// pane the other views use. It lists once and reuses the issues for both the strand
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
	f := strand.Build(issues, s.synFor(repo))
	q := r.URL.Query()
	view := listViewFor(f, q.Get("story"), q.Get("epic"), q.Get("filter"))
	model, err := s.insightsModel(ctx, src, &view, issues)
	if err != nil {
		s.renderError(w, err)
		return
	}
	s.render(w, "insights", insightsView{listView: view, Insights: model})
}

// insightsModel computes the dashboard for a scope. issues is the full repo list
// (for labels and timestamps the strand drops); the scope's beads come from the
// view. It fetches the scope's dependency edges, then hands the plain data to
// internal/insight — the analytics seam that owns the triage/leaderboard/graph
// math, so the server stays transport-only and no longer depends on the graph
// package directly (insight is its sole importer now).
func (s *Server) insightsModel(ctx context.Context, src readSource, v *listView, issues []bd.Issue) (insight.Model, error) {
	// Epics are containers, not actionable work: the dashboard is about the queue
	// and the structure of real tasks, so it drops them.
	beads := insight.Actionable(scopeBeads(v))

	var deps []bd.DepEdge
	if len(beads) > 0 {
		ids := make([]string, len(beads))
		for i := range beads {
			ids[i] = beads[i].ID
		}
		var err error
		if deps, err = src.Deps(ctx, ids...); err != nil {
			return insight.Model{}, fmt.Errorf("insights deps: %w", err)
		}
	}
	return insight.Compute(beads, issues, deps, s.now()), nil
}

// drawerData is the detail panel: a bead, its comments, and an optional write
// error. Embedding promotes the issue's fields, so the template reads .Title etc.
// directly; .Err carries a bd write failure to show inline (spec Q2).
//
// Priority shadows the embedded *bd.Issue.Priority (now *int) with the render-side
// int the priority <select> compares against: it routes through strand.NewBead so
// an absent priority maps to the P2 default — never a false P0 — exactly like the
// list and board (str-zvh).
type drawerData struct {
	*bd.Issue
	Priority int
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
	data := drawerData{Issue: issue, Priority: strand.NewBead(issue).Priority}
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
		iss, err := src.Update(ctx, id, bd.FieldStatus, string(bd.StatusOpen))
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

// Parent-picker sentinels. The create form forces a deliberate parent choice:
// the bead lands under an existing parent, off-epic by explicit opt-out, or
// under a parent minted inline. An empty value is no choice and is rejected, so
// no bead is ever created parentless by accident (the strand's parent axis holds
// for near-zero cost).
const (
	parentOffEpic = "__off_epic__" // deliberate "no parent" choice
	parentNew     = "__new__"      // mint a new parent inline from parentNewTitle
)

// parentOpt is one selectable parent in the create form's picker: the bead id to
// pass to --parent and its raw title (the template formats the label with the
// shared shortID/cleanName helpers).
type parentOpt struct {
	ID    string
	Title string
}

// handleNewForm renders the empty create form into the drawer. New beads default
// to a task at P2 so the common case is a title plus a parent choice away from
// created. It loads the candidate parents (every open bead) so the picker can
// offer them; a List failure still renders the form (off-epic / new-inline both
// work without a candidate list).
func (s *Server) handleNewForm(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	form := createForm{Type: "task", Priority: "2"}
	if src, _, ok := s.source(); ok {
		form.Parents = candidateParents(ctx, src)
	}
	s.render(w, "createForm", form)
}

// candidateParents lists the beads a new bead may hang under: every issue the
// source returns, newest-relevant first as bd orders them, labelled for the
// picker. A List error yields no candidates (the off-epic / new-inline paths
// still let the user proceed) rather than blocking the form.
func candidateParents(ctx context.Context, src readSource) []parentOpt {
	issues, err := src.List(ctx, allIssues...)
	if err != nil {
		return nil
	}
	opts := make([]parentOpt, 0, len(issues))
	for i := range issues {
		opts = append(opts, parentOpt{ID: issues[i].ID, Title: issues[i].Title})
	}
	return opts
}

// createForm is the new-bead form's state: the raw field values (so a failed
// submit re-renders with what the user typed), the candidate parents for the
// forced-parent picker, the sticky parent choice, and a bd error to show inline.
type createForm struct {
	Title          string
	Type           string
	Priority       string
	Description    string
	Parent         string      // the picked value: a bead id, parentOffEpic, or parentNew
	ParentNewTitle string      // title for the inline new-parent path (Parent == parentNew)
	Parents        []parentOpt // candidate existing parents for the picker
	Err            string
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
		Title:          strings.TrimSpace(r.FormValue("title")),
		Type:           r.FormValue("type"),
		Priority:       r.FormValue("priority"),
		Description:    r.FormValue("description"),
		Parent:         r.FormValue("parent"),
		ParentNewTitle: strings.TrimSpace(r.FormValue("parent_new")),
	}
	// Re-render with the candidate list so a rejected submit keeps the picker.
	reject := func(err error) {
		form.Err = err.Error()
		form.Parents = candidateParents(ctx, src)
		s.render(w, "createForm", form)
	}

	parent, err := s.resolveParent(ctx, src, &form)
	if err != nil {
		reject(err)
		return
	}

	opts := bd.CreateOpts{Title: form.Title, Type: form.Type, Description: form.Description, Parent: parent}
	if p, err := strconv.Atoi(form.Priority); err == nil {
		opts.Priority = &p
	}
	issue, err := src.Create(ctx, &opts)
	if err != nil {
		reject(err)
		return
	}
	w.Header().Set("HX-Trigger", "refreshList")
	s.renderDrawer(ctx, w, src, issue, nil)
}

// resolveParent turns the picker choice into the --parent id the create should
// carry, enforcing the forced-parent contract. The empty choice is rejected
// (no accidental parentless bead); off-epic resolves to "" (a deliberate root);
// the inline path mints a new epic and returns its id; anything else is taken as
// an existing parent id. Minting the parent first means a failure there surfaces
// before the child is created, so a half-made pair can't result.
func (s *Server) resolveParent(ctx context.Context, src IssueSource, form *createForm) (string, error) {
	switch form.Parent {
	case "":
		return "", errNoParent
	case parentOffEpic:
		return "", nil
	case parentNew:
		if form.ParentNewTitle == "" {
			return "", errNoParentTitle
		}
		parent, err := src.Create(ctx, &bd.CreateOpts{Title: form.ParentNewTitle, Type: "epic"})
		if err != nil {
			return "", err
		}
		if parent == nil {
			return "", errNoParent
		}
		return parent.ID, nil
	default:
		return form.Parent, nil
	}
}

// attachForm is the "No epic" → attach-to-epic drawer's state: the orphan story
// being laddered up, the candidate epics to pick from, the sticky choice (an epic
// id or attachNew with a title), and a bd error to show inline on a failed submit.
type attachForm struct {
	StoryID    string
	StoryTitle string
	Epics      []parentOpt // candidate existing epics for the picker
	Epic       string      // picked value: an epic id or attachNew
	EpicTitle  string      // title for the inline new-epic path (Epic == attachNew)
	Err        string
}

// attachNew is the picker sentinel for minting a fresh epic to attach the story
// to, mirroring parentNew on the create form.
const attachNew = "__new__"

// errNoAttachStory rejects an attach with no story to reparent.
var errNoAttachStory = errors.New("no story to attach")

// errNoEpicChoice rejects an attach that names neither an existing epic nor a new
// epic title — the same no-accidental-move guard resolveParent enforces on create.
var errNoEpicChoice = errors.New("pick an epic or name a new one")

// handleAttachForm renders the attach-to-epic form into the drawer for an orphan
// story (the "No epic" crumb's target). It loads the candidate epics so the picker
// can offer them; a List failure still renders the form (the new-epic path works
// without a candidate list).
func (s *Server) handleAttachForm(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	form := attachForm{StoryID: r.URL.Query().Get("story")}
	if src, _, ok := s.source(); ok {
		form.Epics = candidateEpics(ctx, src)
		if iss, err := src.Show(ctx, form.StoryID); err == nil && iss != nil {
			form.StoryTitle = iss.Title
		}
	}
	s.render(w, "attachEpic", form)
}

// candidateEpics lists the open top-level epics a story may attach to, labelled
// for the picker. A List error yields no candidates (the new-epic path still
// lets the user proceed) rather than blocking the form.
func candidateEpics(ctx context.Context, src readSource) []parentOpt {
	issues, err := src.List(ctx, allIssues...)
	if err != nil {
		return nil
	}
	opts := make([]parentOpt, 0)
	for i := range issues {
		is := &issues[i]
		if is.IssueType != "epic" || is.Status == bd.StatusClosed || is.Status == bd.StatusDeferred {
			continue
		}
		opts = append(opts, parentOpt{ID: is.ID, Title: is.Title})
	}
	return opts
}

// handleAttachEpic reparents an orphan story onto an epic — existing or minted
// inline — then refreshes the list and shows the now-laddered story's drawer. It
// mints the epic before the reparent so a failure there surfaces before the story
// is moved, and re-renders the form with bd's message on any failure.
func (s *Server) handleAttachEpic(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	src, _, ok := s.source()
	if !ok {
		s.renderError(w, errNoRepo)
		return
	}
	form := attachForm{
		StoryID:   strings.TrimSpace(r.FormValue("story")),
		Epic:      r.FormValue("epic"),
		EpicTitle: strings.TrimSpace(r.FormValue("epic_new")),
	}
	reject := func(err error) {
		form.Err = err.Error()
		form.Epics = candidateEpics(ctx, src)
		s.render(w, "attachEpic", form)
	}
	if form.StoryID == "" {
		reject(errNoAttachStory)
		return
	}
	epicID, err := s.resolveAttachEpic(ctx, src, &form)
	if err != nil {
		reject(err)
		return
	}
	if _, err := src.SetParent(ctx, form.StoryID, epicID); err != nil {
		reject(err)
		return
	}
	w.Header().Set("HX-Trigger", "refreshList")
	issue, err := src.Show(ctx, form.StoryID)
	if err != nil {
		s.renderError(w, err)
		return
	}
	s.renderDrawer(ctx, w, src, issue, nil)
}

// resolveAttachEpic turns the picker choice into the epic id the reparent targets.
// The inline path mints a new epic and returns its id; an existing choice is taken
// as the epic id; an empty choice is rejected (no accidental no-op move).
func (s *Server) resolveAttachEpic(ctx context.Context, src IssueSource, form *attachForm) (string, error) {
	switch form.Epic {
	case attachNew:
		if form.EpicTitle == "" {
			return "", errNoEpicChoice
		}
		epic, err := src.Create(ctx, &bd.CreateOpts{Title: form.EpicTitle, Type: "epic"})
		if err != nil {
			return "", err
		}
		if epic == nil {
			return "", errNoEpicChoice
		}
		return epic.ID, nil
	case "":
		return "", errNoEpicChoice
	default:
		return form.Epic, nil
	}
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

// buildStrand pulls the active repo's live issue list once and folds it into the
// landing model, labeling the catch-all epic with the active repo's name (the
// synthesis project follows the active repo, so a switch re-labels every view).
func (s *Server) buildStrand(ctx context.Context, src readSource, repo registry.Repo) (strand.Model, error) {
	issues, err := src.List(ctx, allIssues...)
	if err != nil {
		return strand.Model{}, fmt.Errorf("list issues: %w", err)
	}
	return strand.Build(issues, s.synFor(repo)), nil
}

// allIssues lifts bd list's default 50-row cap. The strand folds each issue into
// its top-level epic by walking the parent chain, so a truncated list breaks the
// laddering — a story lands off-epic the moment one ancestor row is missing.
// Fetch them all.
var allIssues = []string{"--limit", "0"}

// synFor labels the synthesis with the active repo's name; the project follows the
// active repo, so a switch re-labels every view. Shared by buildStrand and the
// insights handler (which lists issues itself, so can't go through buildStrand).
func (s *Server) synFor(repo registry.Repo) strand.Synthesis {
	syn := s.syn
	syn.Project = repo.Name
	// A non-empty s.syn.NorthStar is the --northstar flag and wins; with no flag
	// the masthead reads the active repo's canonical one-line North Star from
	// north-star-mini.md (decision nug 952acad4aca2). Missing/empty → blank.
	if syn.NorthStar == "" {
		syn.NorthStar = northStarMini(repo.Path)
	}
	syn.JTBD = jtbd.Load(repo.Path)
	return syn
}

// northStarMini returns the North Star reminder for a repo, read from
// north-star-mini.md at the repo root (decision nug 952acad4aca2). The file is a
// few curated lines; the masthead renders them verbatim (newlines preserved). It
// tolerates a leading YAML frontmatter block (the file may be a vault nug) and a
// Markdown heading marker on the first line. A missing or empty file yields "" so
// the masthead renders blank instead of crashing (str-d2s).
func northStarMini(repoPath string) string {
	if repoPath == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(repoPath, "north-star-mini.md"))
	if err != nil {
		return ""
	}
	return northStarBody(string(b))
}

// northStarBody skips a leading --- frontmatter block and returns the remaining
// body trimmed, with any Markdown heading marker stripped from the first line.
// Internal newlines are preserved so a few-line reminder renders as a few lines.
func northStarBody(s string) string {
	lines := strings.Split(s, "\n")
	i := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i = 1; i < len(lines) && strings.TrimSpace(lines[i]) != "---"; i++ {
		}
		i++ // skip the closing ---
	}
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" { // drop leading blanks
		i++
	}
	body := strings.TrimRight(strings.Join(lines[i:], "\n"), " \t\n")
	return strings.TrimSpace(strings.TrimLeft(body, "#")) // strip a leading heading marker
}

// snapshotCache holds one in-process read snapshot per repo, keyed by the repo's
// path. Each view (strand/list/board/insights) used to shell out to `bd
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
// A snapshot has no time-based expiry: it lives until a write invalidates it or
// the repo switches. The model is "open a beadbase, look at each view in turn", so
// a snapshot must outlast a long look without re-paying the ~0.4s bd spawn on the
// next tab (str-udl supersedes the original 3s TTL, which punished lingering).
//
// Out-of-band staleness — a bd CLI run or another agent editing the same repo's
// store while strand holds a snapshot — is the one case writes can't catch. With
// no clock to age the snapshot out, a plain browser reload would serve the stale
// view, so the mitigation is the explicit refresh control (POST /refresh →
// invalidate → reload): out-of-band edits surface on a deliberate click, with a
// "data as of HH:MM" readout so the staleness window is visible.
type snapshotCache struct {
	mu      sync.Mutex
	now     func() time.Time
	gen     uint64
	entries map[string]*snapshot
}

// snapshot is one repo's cached reads: the full `list --limit 0` result and the
// repo-wide Deps result, stamped with the wall time they were fetched so the TTL
// floor can age them out. List and Deps share one entry (and the TTL stamp set at
// the List fetch) so strand/list/insights are one logical snapshot — and Deps is
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

func newSnapshotCache(now func() time.Time) *snapshotCache {
	return &snapshotCache{now: now, entries: map[string]*snapshot{}}
}

// entryLocked returns the repo's snapshot if present, else nil (a miss). A
// snapshot lives until a write invalidates it or the repo switches — there is no
// time-based expiry. The usage model is "open a beadbase, look at each view", so
// a snapshot must survive a long look without re-paying the bd spawn; out-of-band
// edits surface through the explicit refresh control (str-udl), not a silent
// clock. The caller MUST hold c.mu: putDeps mutates an entry's deps/depsOK in
// place, so a reader that escapes the lock with the *snapshot races that write
// (strand-4sd). The public accessors below copy the fields they need out under
// the lock and never hand the pointer to a handler.
func (c *snapshotCache) entryLocked(repo string) *snapshot {
	return c.entries[repo]
}

// stampedAt reports when the repo's snapshot was fetched, for the "data as of …"
// readout. ok is false when no snapshot is warm yet.
func (c *snapshotCache) stampedAt(repo string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[repo]; ok {
		return e.at, true
	}
	return time.Time{}, false
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

func (c *cachingSource) SetParent(ctx context.Context, id, parent string) (*bd.Issue, error) {
	iss, err := c.IssueSource.SetParent(ctx, id, parent)
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

func (c *cachingSource) Create(ctx context.Context, opts *bd.CreateOpts) (*bd.Issue, error) {
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
