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
	"github.com/dkoosis/strand/internal/llm"
	"github.com/dkoosis/strand/internal/registry"
	"github.com/dkoosis/strand/internal/strand"
	"github.com/dkoosis/strand/internal/strandmd"
	"github.com/dkoosis/strand/internal/suggest"
)

// errNoRepo means no beads workspace is active. The landing turns it into an
// actionable empty state (spec R1); other handlers shouldn't reach it, since the
// UI offers no bead links until a repo is active.
var (
	errNoRepo  = errors.New("no active repo")
	errNoIssue = errors.New("no issue to display")
	// errNoParent rejects a create with no deliberate parent choice — the
	// forced-parent contract (str-6k0.6.2): pick an existing parent, choose
	// no epic, or mint one inline, but never create a bead parentless by
	// omission.
	errNoParent = errors.New("pick a parent, choose no epic, or create a new parent")
	// errNoParentTitle rejects the inline new-parent path with an empty title.
	errNoParentTitle = errors.New("new parent needs a title")
	// errEpicMint covers a Create that returns neither an epic nor an error (a
	// bd-contract violation): the inline mint produced nothing to hang the child or
	// story under. Shared by the create and attach inline-new-epic paths.
	errEpicMint = errors.New("could not create the new epic")
)

// IssueSource is the slice of bd.Client the server needs: reads plus the V1
// writes. An interface keeps the handlers testable with a stub and the bd CLI
// behind one seam (spec Q0). Writes go through the write-client (spec D6) so the
// bare-`bd update` footgun stays impossible. It is exported because the active
// repo is resolved per request through a SourceFunc the command wires.
type IssueSource interface {
	List(ctx context.Context, args ...string) ([]bd.Issue, error)
	Stats(ctx context.Context) (bd.Stats, error)
	Deps(ctx context.Context, ids ...string) ([]bd.DepEdge, error)
	Show(ctx context.Context, id string) (*bd.Issue, error)
	Comments(ctx context.Context, id string) ([]bd.Comment, error)
	Update(ctx context.Context, id, field, value string) (*bd.Issue, error)
	Claim(ctx context.Context, id string) (*bd.Issue, error)
	Close(ctx context.Context, id, reason string) (*bd.Issue, error)
	SetRank(ctx context.Context, id string, rank float64) (*bd.Issue, error)
	SetParent(ctx context.Context, id, parent bd.ID) (*bd.Issue, error)
	Comment(ctx context.Context, id, text string) error
	DepAdd(ctx context.Context, id, dependsOn bd.ID) error
	DepRemove(ctx context.Context, id, dependsOn bd.ID) error
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

	// Tier-2 Suggest seams (st-suggest.3.3). suggestLLM builds the key-gated model
	// client (default defaultSuggestLLM over llm.New); tests swap it for a stub so
	// the namer never touches the network. homeDir roots the global STRAND.md load
	// (default os.UserHomeDir); tests point it at a temp dir so a Suggest call never
	// initializes the real ~/.strand. Both are read only inside the keyed Tier-2
	// branch, so the unkeyed Tier-1 path touches neither.
	suggestLLM func() (suggest.Completer, bool)
	homeDir    string
	// noKeyOnce logs the "Tier-2 disabled — no API key" warning a single time, the
	// first time an unkeyed Suggest falls to Tier-1. Without it the incident this seam
	// exists to surface (server running with no ANTHROPIC_API_KEY, so every title comes
	// from the deterministic floor) stays as silent as a thin bead — but logging it per
	// request would spam every drawer open, so it fires once per process.
	noKeyOnce sync.Once

	// Lifecycle for detached background work (the Insights deps prefetch). bgCtx
	// roots every goBackground context so Stop can cancel them; bgWG tracks the
	// goroutines so Stop can wait them out. The HTTP drain covers request handlers
	// only — these outlive their request and would otherwise escape it (str-47z).
	bgCtx    context.Context //nolint:containedctx // the server owns the lifecycle of its detached goroutines; bgCtx IS that lifecycle, cancelled by Stop.
	bgCancel context.CancelFunc
	bgWG     sync.WaitGroup
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
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	s.suggestLLM = defaultSuggestLLM
	s.homeDir, _ = os.UserHomeDir()
	return s
}

// defaultSuggestLLM is the production Tier-2 model gate: it builds the key-gated
// llm client, reporting unavailable (so Suggest stays Tier-1) when no API key is
// set. It adapts *llm.Client to the suggest.Completer seam and returns a nil
// interface — not a typed-nil — when unavailable, so the caller's ok-check is the
// only gate (mirrors `bd find-duplicates --ai`).
func defaultSuggestLLM() (suggest.Completer, bool) {
	c, ok := llm.New()
	if !ok {
		return nil, false
	}
	return c, true
}

// goBackground runs fn in a tracked goroutine on a server-owned context, so work
// that must outlive its request — the Insights deps prefetch (str-udl) — dies with
// the server instead of escaping the shutdown drain (str-47z). The context is
// detached from the request (the request ctx dies the moment the handler returns)
// but rooted in bgCtx, which Stop cancels; the WaitGroup lets Stop block until the
// goroutine lands, so no bd spawn or snapshot-cache write outlives shutdown.
func (s *Server) goBackground(timeout time.Duration, fn func(context.Context)) {
	s.bgWG.Go(func() {
		ctx, cancel := context.WithTimeout(s.bgCtx, timeout)
		defer cancel()
		fn(ctx)
	})
}

// Stop cancels in-flight background work (goBackground) and waits for it to land.
// httpSrv.Shutdown drains request handlers only; the detached prefetch goroutines
// outlive it, so main calls Stop AFTER the HTTP drain to guarantee nothing is
// still spawning bd or writing the snapshot cache once the process exits (str-47z).
// Cancellation makes the wait short — an in-flight `bd dep list` is killed via its
// context rather than run to its 10s timeout. Idempotent.
func (s *Server) Stop() {
	s.bgCancel()
	s.bgWG.Wait()
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
	mux.HandleFunc("GET /pulse", s.handlePulse)
	mux.HandleFunc("GET /board", s.handleBoard)
	mux.HandleFunc("GET /insights", s.handleInsights)
	mux.HandleFunc("GET /bead/{id}", s.handleBead)
	mux.HandleFunc("GET /bead/{id}/suggest", s.handleSuggest)
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
	// Flat marks a masthead-pulse status cut: a flat, repo-wide bead list with no
	// epic/story structure (the beads come straight from a status fetch). The list
	// template renders FlatTitle as the head and Story.Beads as one table — no
	// breadcrumb, no per-story groups. Story.Open carries the count.
	Flat      bool
	FlatTitle string
}

// pageData is the full landing render: the strand, the list pane, and the repo
// selector chrome (the active repo's name and the known repos). Empty is true
// when no repo is active, switching the landing to its actionable empty state.
type pageData struct {
	Strand strand.Model
	List   listView
	Pulse  Pulse
	Repos  repoMenu
	Empty  bool
	AsOf   string // "data as of HH:MM" for the refresh readout; empty when no snapshot
}

// Pulse is the masthead's repo-wide bead-status spread — the bead half of the
// Claude Code status line, mirrored into the page. Each field is a glyph cell:
// ◆ Waiting, ○ Open, ◐ InProgress, ● Blocked, ✓ Closed, ❄ Deferred. The status
// counts come from `bd stats` (the same source the status line uses); Waiting is
// the human-gated count the strand drops. A nonzero cell is a click-to-list
// filter (handleList's pulse cuts).
type Pulse struct {
	Waiting, Open, InProgress, Blocked, Closed, Deferred int
}

// computePulse builds the masthead spread for the active repo. The five status
// counts come from one `bd stats` call (closed/deferred are invisible to `bd
// list`); Waiting is read off the cached open snapshot so it costs no extra
// spawn. A failed stats read degrades to zero status counts rather than failing
// the whole page — the pulse is ambient, not load-bearing.
func (s *Server) computePulse(ctx context.Context, src IssueSource, repo registry.Repo) Pulse {
	var p Pulse
	if st, err := src.Stats(ctx); err == nil {
		p.Open, p.InProgress, p.Blocked = st.Open, st.InProgress, st.Blocked
		p.Closed, p.Deferred = st.Closed, st.Deferred
	}
	if issues, err := src.List(ctx, allIssues...); err == nil {
		p.Waiting = insight.WaitingCount(issues)
		// The masthead counts must agree with the cuts they click through to, so the
		// live lanes derive from insight.Classify — the same truth the ● cut, the ○
		// cut, and the board share. bd stats' blocked_issues counts an in-progress
		// bead with an unmet blocker as blocked, but Classify (and so the ● cut) keeps
		// it in ◐, not ●; taking the count from stats made ● click through to fewer
		// beads, or none (st-x66). Deps are read from the warm cache only — the
		// masthead never pays a deps spawn on the landing path (str-47z). On a cold
		// first paint we keep the bd-stats figures for one render; the background
		// prefetch warms deps and the next /pulse render (refreshList) is exact.
		blocked, exact := s.cachedBlockedSet(repo, issues)
		// ○ is actionable-now: open, not parked on dk (◆), not blocked (●). Subtract
		// both diversions so a bead sits in one lane, not two — mirroring how ◆ and ○
		// were already made disjoint. Only open-status beads leave ○: an in-progress
		// review or a stored-blocked bead never entered the open count.
		open := 0
		for i := range issues {
			iss := &issues[i]
			if iss.Status != bd.StatusOpen || insight.IsHumanGated(iss) {
				continue
			}
			if blocked[iss.ID] { // nil map on a cold cache → parked-only subtraction
				continue
			}
			open++
		}
		p.Open = open
		if exact {
			p.Blocked = len(blocked)
		}
	}
	return p
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	src, repo, ok := s.source()
	if !ok {
		s.render(w, "page", pageData{Empty: true, Repos: s.repoMenu("")})
		return
	}
	f, _, err := s.buildStrand(ctx, src, repo)
	if err != nil {
		s.renderError(w, err)
		return
	}
	// computePulse runs before the deps prefetch launches: its Stats call is a bd
	// spawn that serializes on package bd's execMu, so kicking off the background
	// `dep list` first would queue the masthead's stats read behind it and stall the
	// landing. Compute the pulse (List is already warm from buildStrand, so this is
	// one Stats spawn), then warm deps in the background.
	pulse := s.computePulse(ctx, src, repo)
	// Open warms every view, not just this one: buildStrand warmed the List
	// snapshot, so prefetch the repo-wide Deps too — that makes the first Insights
	// click (the one view needing edges) hit memory instead of paying a cold `dep
	// list` spawn (str-udl). Warm it in the background, detached from the request
	// (the landing must not block on a ~0.5s spawn the user may never need, and the
	// request ctx dies the moment handleHome returns) but bound to the server via
	// goBackground, so shutdown drains it instead of letting it escape (str-47z). A
	// prefetch failure is non-fatal — the landing renders and Insights fetches its
	// own deps on demand.
	s.goBackground(10*time.Second, func(ctx context.Context) {
		_, _ = src.Deps(ctx)
	})
	s.render(w, "page", pageData{
		Strand: f,
		List:   listViewFor(f, "", "", ""),
		Pulse:  pulse,
		Repos:  s.repoMenu(""),
		AsOf:   s.asOf(repo),
	})
}

// handlePulse re-renders just the masthead spread. The page wires it to the
// refreshList event so an in-app create/edit/close keeps the counts honest
// without a full reload; with no active repo it renders nothing.
func (s *Server) handlePulse(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	src, repo, ok := s.source()
	if !ok {
		return
	}
	s.render(w, "pulse", s.computePulse(ctx, src, repo))
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
	// A masthead-pulse cut (filter=open|in_progress|blocked|closed|deferred|waiting)
	// is a repo-wide status slice, not a strand scope — it can include closed and
	// deferred beads the strand drops, so it renders from its own fetch, before the
	// strand is built.
	q := r.URL.Query()
	if cut, ok := pulseCutFor(q.Get("filter")); ok {
		view, err := s.pulseListView(ctx, src, cut)
		if err != nil {
			s.renderError(w, err)
			return
		}
		s.render(w, "list", view)
		return
	}
	f, _, err := s.buildStrand(ctx, src, repo)
	if err != nil {
		s.renderError(w, err)
		return
	}
	// story=<id> narrows the pane to a single story; absent means the whole epic.
	s.render(w, "list", listViewFor(f, q.Get("story"), q.Get("epic"), q.Get("filter")))
}

// pulseCut is one masthead-pulse drill-down: the pane title and the predicate
// selecting its beads. external marks the cuts whose beads live outside the live
// snapshot (closed/deferred) — they need an uncached `--status` fetch because the
// cachingSource serves only the open-list snapshot and the strand drops them.
type pulseCut struct {
	title    string
	status   bd.Status // the --status to fetch for an external cut; "" for live cuts
	external bool
	// blockerAware marks the ● cut, whose set is derived (stored-blocked OR open
	// with an unmet blocker), not a plain status match — pulseListView classifies
	// it via insight so the list agrees with the masthead count (st-88o).
	blockerAware bool
	// excludeBlocked marks the ○ cut: after the match predicate, pulseListView drops
	// any bead the ● cut would claim, so an open-but-blocked bead sits in ● alone —
	// the disjoint-lane rule the count now enforces too (st-x66).
	excludeBlocked bool
	match          func(*bd.Issue) bool
}

// pulseCutFor maps a filter token to its cut, or reports false for any other
// filter (so "bugs" and the empty/scope filters fall through to listViewFor).
func pulseCutFor(filter string) (pulseCut, bool) {
	statusIs := func(st bd.Status) func(*bd.Issue) bool {
		return func(i *bd.Issue) bool { return i.Status == st }
	}
	switch filter {
	case "waiting":
		return pulseCut{title: "Waiting on you", match: func(i *bd.Issue) bool {
			return i.Status != bd.StatusClosed && i.Status != bd.StatusDeferred && insight.IsHumanGated(i)
		}}, true
	case "open":
		// ○ lists open work the agent can act on — open beads parked on dk live in
		// the ◆ cut, and open-but-blocked beads in the ● cut, not here. Both are
		// excluded so the lanes stay disjoint and the list matches the ○ count.
		return pulseCut{title: "Open", excludeBlocked: true, match: func(i *bd.Issue) bool {
			return i.Status == bd.StatusOpen && !insight.IsHumanGated(i)
		}}, true
	case "in_progress":
		return pulseCut{title: "In progress", match: statusIs(bd.StatusInProgress)}, true
	case "blocked":
		// ● counts bd stats' blocked_issues, which includes dependency-derived
		// blocks; a literal status match would miss those (they carry status
		// "open"), so the cut classifies effective-blocked instead (st-88o).
		return pulseCut{title: "Blocked", blockerAware: true, match: statusIs(bd.StatusBlocked)}, true
	case "closed":
		return pulseCut{title: "Closed", status: bd.StatusClosed, external: true, match: statusIs(bd.StatusClosed)}, true
	case "deferred":
		return pulseCut{title: "Deferred", status: bd.StatusDeferred, external: true, match: statusIs(bd.StatusDeferred)}, true
	}
	return pulseCut{}, false
}

// pulseListView gathers a status cut's beads into a flat list scope. Live cuts
// (and waiting) read the cached open snapshot; an external cut fetches its status
// through the same source — the caching wrapper passes a filtered read (any args
// other than allIssues) straight through to bd uncached, so the status slice never
// serves or poisons the open snapshot. The match predicate is applied in both cases,
// so the cut is correct even when the source returns an unfiltered list.
func (s *Server) pulseListView(ctx context.Context, src IssueSource, cut pulseCut) (listView, error) {
	var (
		issues []bd.Issue
		err    error
	)
	if cut.external {
		issues, err = src.List(ctx, "--status", string(cut.status), "--limit", "0")
	} else {
		issues, err = src.List(ctx, allIssues...)
	}
	if err != nil {
		return listView{}, err
	}
	beads, err := s.pulseBeads(ctx, src, cut, issues)
	if err != nil {
		return listView{}, err
	}
	slices.SortStableFunc(beads, func(a, b strand.Bead) int {
		if a.Priority != b.Priority {
			return cmp.Compare(a.Priority, b.Priority)
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return listView{Flat: true, FlatTitle: cut.title, Story: strand.Story{Beads: beads, Open: len(beads)}}, nil
}

// pulseBeads selects a cut's matching beads from the snapshot. The ● cut
// (blockerAware) lists the effective-blocked set straight from blockedBeads;
// every other cut runs the match predicate, dropping ●-claimed beads when the
// cut asks (excludeBlocked) so the lanes stay disjoint and match the ○ count
// (st-x66).
func (s *Server) pulseBeads(ctx context.Context, src IssueSource, cut pulseCut, issues []bd.Issue) ([]strand.Bead, error) {
	if cut.blockerAware {
		return s.blockedBeads(ctx, src, issues)
	}
	// The ○ cut drops beads the ● cut would claim, so an open-but-blocked bead
	// lists under ● alone (st-x66). Only fetched when the cut asks — a nil map
	// leaves every match in place.
	var blocked map[string]bool
	if cut.excludeBlocked {
		var err error
		if blocked, err = s.blockedSet(ctx, src, issues); err != nil {
			return nil, err
		}
	}
	var beads []strand.Bead
	for i := range issues {
		if cut.match(&issues[i]) && !blocked[issues[i].ID] {
			beads = append(beads, strand.NewBead(&issues[i]))
		}
	}
	return beads, nil
}

// blockedBeads is the ● cut's bead set: every live bead the board would bucket
// blocked — stored-status "blocked" OR open with an unmet blocker. It runs the
// open snapshot through insight.Classify (the same truth the board column and
// triage use, the same set bd stats' blocked_issues counts), so the masthead ●
// count and this list can't diverge. A literal status match found none when the
// blocks were all dependency-derived (those beads carry status "open"), leaving
// the pane empty under a nonzero count (st-88o). deps are fetched repo-wide: a
// blocker can sit outside any one scope, and the count is repo-wide too.
func (s *Server) blockedBeads(ctx context.Context, src IssueSource, issues []bd.Issue) ([]strand.Bead, error) {
	blocked, err := s.blockedSet(ctx, src, issues)
	if err != nil {
		return nil, err
	}
	out := make([]strand.Bead, 0, len(blocked))
	for i := range issues {
		if blocked[issues[i].ID] {
			out = append(out, strand.NewBead(&issues[i]))
		}
	}
	return out, nil
}

// cachedBlockedSet classifies the effective-blocked set from ALREADY-WARM deps,
// reporting ok=false when the repo's deps aren't cached yet. computePulse uses it
// so the masthead never triggers a deps spawn on the landing path (str-47z): a cold
// cache falls back to bd stats for one render, and the background deps prefetch plus
// the next /pulse render (on refreshList) bring the count in line with the cut.
func (s *Server) cachedBlockedSet(repo registry.Repo, issues []bd.Issue) (map[string]bool, bool) {
	if len(issues) == 0 {
		return nil, true // nothing to classify — Blocked is exactly zero, no deps needed
	}
	// Read the repo's warm deps straight from the snapshot cache (never fetching),
	// so the masthead classifies from cached edges when they're warm and falls back
	// to bd stats when they aren't — no deps spawn on the landing path (str-47z).
	deps, warm := s.cache.liveDeps(repo.Path)
	if !warm {
		return nil, false
	}
	return classifyBlocked(issues, deps), true
}

// classifyBlocked projects the snapshot issues into beads and returns
// insight.Classify's effective-blocked map (stored "blocked" OR open with an unmet
// blocker) — the one truth the ● cut lists and the ● count reports. cachedBlockedSet
// (warm-deps peek) and blockedSet (fetching path) share it, differing only in how
// they acquire deps.
func classifyBlocked(issues []bd.Issue, deps []bd.DepEdge) map[string]bool {
	all := make([]strand.Bead, len(issues))
	for i := range issues {
		all[i] = strand.NewBead(&issues[i])
	}
	blocked, _ := insight.Classify(all, issues, deps)
	return blocked
}

// blockedSet classifies the snapshot's effective-blocked beads (stored "blocked"
// OR open with an unmet blocker) — insight.Classify's blocked map. It is the one
// truth the ● cut lists, the ● count reports (len), and the ○ cut/count subtract,
// so the masthead count can never click through to a different set (st-x66). Shares
// the repo-wide deps fetch, warm after the first structural view. Returns an empty
// map for an empty snapshot; an error only when the deps fetch itself fails.
func (s *Server) blockedSet(ctx context.Context, src IssueSource, issues []bd.Issue) (map[string]bool, error) {
	if len(issues) == 0 {
		return map[string]bool{}, nil
	}
	ids := make([]string, len(issues))
	for i := range issues {
		ids[i] = issues[i].ID
	}
	deps, err := src.Deps(ctx, ids...)
	if err != nil {
		return nil, fmt.Errorf("blocked deps: %w", err)
	}
	return classifyBlocked(issues, deps), nil
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
	{"status", []boardColumn{{Key: string(bd.StatusOpen), Label: "open"}, {Key: string(bd.StatusInProgress), Label: "in progress"}, {Key: string(bd.StatusBlocked), Label: "blocked"}, {Key: string(bd.StatusClosed), Label: "closed"}}, func(b *strand.Bead) string {
		// Effective status: an open bead held by an unmet blocker shows in the blocked
		// column though bd still stores it "open" (a dependency-blocked bead is never
		// auto-promoted to status=blocked). Without this the blocked column only ever
		// catches hand-set beads and reads as empty. Human-gated beads stay in their
		// real column and carry a ◆ badge instead — "waiting" is no bd status, so it
		// can't be a drop target that handleMove writes.
		if b.Blocked {
			return string(bd.StatusBlocked)
		}
		return string(b.Status)
	}},
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
	// List once: strand.Build folds the issues into the scope, and the attention
	// classifier reuses the same list for its blocker/gate index (a blocker can live
	// outside the visible scope), so the board and the dashboard read one truth.
	f, issues, err := s.buildStrand(ctx, src, repo)
	if err != nil {
		s.renderError(w, err)
		return
	}
	q := r.URL.Query()
	view := listViewFor(f, q.Get("story"), q.Get("epic"), q.Get("filter"))
	scope := scopeBeads(&view)
	if err := s.markAttention(ctx, src, scope, issues); err != nil {
		s.renderError(w, err)
		return
	}
	s.render(w, "board", buildBoard(&view, q.Get("pivot"), scope))
}

// markAttention flags each scope bead Blocked or Waiting for the board, fetching the
// scope's dependency edges and classifying via insight (the precedence the dashboard
// triage already uses). A deps-read error fails the render rather than degrading
// silently — a blocked bead shown as plain open is exactly the bug this fixes. The
// scope slice is mutated in place; it is the same backing the board buckets.
func (s *Server) markAttention(ctx context.Context, src readSource, scope []strand.Bead, issues []bd.Issue) error {
	if len(scope) == 0 {
		return nil
	}
	ids := make([]string, len(scope))
	for i := range scope {
		ids[i] = scope[i].ID
	}
	deps, err := src.Deps(ctx, ids...)
	if err != nil {
		return fmt.Errorf("board deps: %w", err)
	}
	blocked, waiting := insight.Classify(scope, issues, deps)
	for i := range scope {
		scope[i].Blocked = blocked[scope[i].ID]
		scope[i].Waiting = waiting[scope[i].ID]
	}
	return nil
}

// buildBoard pivots the scope's (attention-marked) beads into columns. Both the
// whole-epic and single-story scopes flow through here, so the board can't diverge
// from the table.
func buildBoard(v *listView, pivot string, beads []strand.Bead) boardView {
	pivot = pivotOrDefault(pivot)
	return boardView{
		listView: *v,
		Pivot:    pivot,
		Pivots:   boardPivots,
		Columns:  boardColumns(pivot, beads),
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
	// Recompute the moved card's attention flags (st-03i). markAttention runs only on
	// the full /board render, so this single-card refresh would otherwise drop the ◆
	// waiting badge and Blocked state until the next full board load. Best-effort: a
	// read error degrades to the unflagged card rather than failing the move — the
	// write already landed, and a non-2xx here would wrongly tell the client to revert
	// a change bd has already stored.
	if issues, lerr := src.List(ctx, allIssues...); lerr != nil {
		log.Printf("strand: move %s attention recompute skipped: %v", id, lerr)
	} else {
		scope := []strand.Bead{b}
		if aerr := s.markAttention(ctx, src, scope, issues); aerr != nil {
			log.Printf("strand: move %s attention recompute skipped: %v", id, aerr)
		} else {
			b = scope[0]
		}
	}
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

	f, _, err := s.buildStrand(ctx, src, repo)
	if err != nil {
		s.renderError(w, err)
		return
	}
	ranks, present, allRanked := groupRanks(f, order)

	// A wholly-ranked group with a representable gap moves the one dragged bead with a
	// single SetRank and returns. Every other case — an unseeded group, or a move that
	// exhausts the midpoint gap (renorm) — falls through to the shared seed-and-204
	// tail, which reseeds dense ranks 1..M over the live ids.
	if allRanked {
		moved := movedID(order, ranks)
		newRank, renorm := rankFor(order, ranks, moved)
		if !renorm {
			if _, err := src.SetRank(ctx, moved, newRank); err != nil {
				s.renderError(w, wrapWrite("rank", err))
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	if err := seedRanks(ctx, src, order, present); err != nil {
		s.renderError(w, err) // seedRanks already wraps with wrapWrite
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	f, issues, err := s.buildStrand(ctx, src, repo)
	if err != nil {
		s.renderError(w, err)
		return
	}
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

// suggestPreview is the title Suggest slot's data: the bead id (so Apply/Dismiss
// act on the right drawer), the current title, and the deterministic proposal.
// S.None drives the "nothing to suggest" branch; otherwise the template renders
// current-vs-proposed with Apply/Dismiss controls.
type suggestPreview struct {
	ID      string
	Current string
	S       suggest.Suggestion
}

// sectionPreview is the body Suggest slot's data: the bead id and the deterministic
// section-gap suggestion. S.None drives the "nothing to suggest" branch; otherwise
// the template lists each missing section's draft with an Apply that copies the
// augmented description into the editor's change→PATCH path.
type sectionPreview struct {
	ID string
	S  suggest.SectionSuggestion
}

// handleSuggest renders a deterministic suggestion for the bead as a preview slot.
// Read-only by design: it loads the bead, runs the namer, and renders the preview
// — no bd write. Apply is the user's gesture in the browser, which copies the
// proposal into the matching editor input and fires its change→PATCH path; Suggest
// itself writes nothing. ?kind=body proposes the missing description sections
// (st-suggest.2); any other kind serves the Tier-1 title namer (st-suggest.1).
func (s *Server) handleSuggest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	src, repo, ok := s.source()
	if !ok {
		s.renderError(w, errNoRepo)
		return
	}
	issue, err := src.Show(ctx, r.PathValue("id"))
	if err != nil {
		s.renderError(w, err)
		return
	}
	if issue == nil {
		s.renderError(w, errNoIssue)
		return
	}
	if r.URL.Query().Get("kind") == "body" {
		sg := suggest.Sections(suggest.SectionInput{Body: issue.Description, Type: issue.IssueType})
		s.render(w, "sectionPreview", sectionPreview{ID: issue.ID, S: sg})
		return
	}
	// Tier-2: when a model key is present, ground the title in the resolved
	// STRAND.md + north star + graph. Any failure (strand load or model call) falls
	// through to the Tier-1 deterministic namer below — Suggest is advisory and never
	// errors on the model path; an absent key keeps the whole branch dark, so the
	// unkeyed path costs no STRAND.md load and no network (st-suggest.3.3).
	if c, keyed := s.suggestLLM(); keyed {
		sg, terr := s.tier2Title(ctx, src, repo, issue, c)
		if terr == nil {
			s.render(w, "suggestPreview", suggestPreview{ID: issue.ID, Current: issue.Title, S: sg})
			return
		}
		// Tier-2 is best-effort; fall through to the Tier-1 floor below. Log the cause
		// so a dead/invalid key or model error surfaces instead of silently degrading to
		// worse titles (a 401 from a wrong-typed key looks identical to a thin bead). A
		// cancelled request (drawer closed, navigated away) is expected, not an incident
		// — don't log it, or the noise drowns the real failures (PR #61, gemini).
		if !errors.Is(terr, context.Canceled) {
			log.Printf("strand: suggest tier-2 fell back to tier-1 for %s: %v", issue.ID, terr)
		}
	} else {
		// No API key: Tier-2 is never attempted, so the keyed branch above can't log it.
		// Surface the disabled state once — this is the actual incident dk hit (PR #61,
		// codex), otherwise indistinguishable from a run of thin beads.
		s.noKeyOnce.Do(func() {
			log.Printf("strand: Tier-2 Suggest disabled (ANTHROPIC_API_KEY not set); titles use the deterministic Tier-1 floor")
		})
	}
	sg := suggest.Title(suggest.TitleInput{
		Title:  issue.Title,
		Body:   issue.Description,
		Type:   issue.IssueType,
		Parent: parentTitle(ctx, src, issue.Parent),
	})
	s.render(w, "suggestPreview", suggestPreview{ID: issue.ID, Current: issue.Title, S: sg})
}

// tier2Title builds the model-grounded title suggestion: it resolves the layered
// STRAND.md, gathers the per-call grounding (north star, parent, children, and an
// inline-cited job only when the page already references one), and runs the Tier-2
// namer. A strand-load or model error is returned so handleSuggest falls back to
// Tier-1 — the model path never breaks Suggest (st-suggest.3.3).
func (s *Server) tier2Title(ctx context.Context, src IssueSource, repo registry.Repo, issue *bd.Issue, c suggest.Completer) (suggest.Suggestion, error) {
	sc, err := strandmd.Load(s.homeDir, repo.Path)
	if err != nil {
		return suggest.Suggestion{}, fmt.Errorf("tier2: load strand: %w", err)
	}
	sg, err := suggest.Tier2(ctx, c, &suggest.Tier2Input{
		Strand:    sc.Text,
		Actors:    sc.Actors,
		NorthStar: s.northStarFor(repo),
		Title:     issue.Title,
		Body:      issue.Description,
		Type:      issue.IssueType,
		Parent:    parentTitle(ctx, src, issue.Parent),
		Children:  childTitles(ctx, src, issue.ID),
		Job:       citedJob(repo.Path, issue.Description),
	})
	if err != nil {
		return suggest.Suggestion{}, fmt.Errorf("tier2: %w", err)
	}
	return sg, nil
}

// parentTitle returns the parent bead's title, or "" on no parent or a read miss.
// Best-effort graph context: Suggest stays advisory and must not fail on a thin
// parent link. The parent is almost always in the repo snapshot List already serves
// from memory, so it reads there first (like childTitles) and pays a bd show spawn
// only on a miss — a parent outside the list.
func parentTitle(ctx context.Context, src IssueSource, parentID string) string {
	if parentID == "" {
		return ""
	}
	if issues, err := src.List(ctx, allIssues...); err == nil {
		for i := range issues {
			if issues[i].ID == parentID {
				return issues[i].Title
			}
		}
	}
	if p, err := src.Show(ctx, parentID); err == nil && p != nil {
		return p.Title
	}
	return ""
}

// childTitles returns the titles of the bead's direct children, read from the
// (cached) repo list. A list error yields no children rather than failing Suggest.
func childTitles(ctx context.Context, src IssueSource, id string) []string {
	issues, err := src.List(ctx, allIssues...)
	if err != nil {
		return nil
	}
	var out []string
	for i := range issues {
		if issues[i].Parent == id {
			out = append(out, issues[i].Title)
		}
	}
	return out
}

// citedJob resolves the JTBD job a bead's description already cites, or "" when it
// cites none — the job reaches the model ONLY when the page references it, never
// fetched and never required (st-suggest.3.3). A missing registry or an unresolved
// id is "", not an error.
func citedJob(repoPath, description string) string {
	id, ok := jtbd.Cite(description)
	if !ok {
		return ""
	}
	if job, ok := jtbd.Load(repoPath).Resolve(id); ok {
		return job
	}
	return ""
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
		if d.Type == bd.DepBlocks && d.IssueID == id {
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
		return nil, wrapWrite("dep add", src.DepAdd(ctx, bd.ID(id), bd.ID(on)))
	})
}

// handleDepRemove drops a "blocks" dependency from the drawer's blocker list.
func (s *Server) handleDepRemove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	on := strings.TrimSpace(r.FormValue("depends_on"))
	s.writeAndRefresh(w, r, id, func(ctx context.Context, src IssueSource) (*bd.Issue, error) {
		return nil, wrapWrite("dep remove", src.DepRemove(ctx, bd.ID(id), bd.ID(on)))
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
	// sentinelNew is the picker value that means "mint a fresh epic inline" — the
	// create form's new-parent path (from ParentNewTitle) and the attach form's
	// new-epic path (from EpicTitle) both post it, so one const serves both.
	sentinelNew = "__new__"
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
	Parent         string      // the picked value: a bead id, parentOffEpic, or sentinelNew
	ParentNewTitle string      // title for the inline new-parent path (Parent == sentinelNew)
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

	parent, minted, err := s.resolveParent(ctx, src, &form)
	if err != nil {
		reject(err)
		return
	}

	opts := bd.CreateOpts{Title: form.Title, Type: form.Type, Description: form.Description, Parent: bd.ID(parent)}
	if p, err := strconv.Atoi(form.Priority); err == nil {
		opts.Priority = &p
	}
	issue, err := src.Create(ctx, &opts)
	if err != nil {
		// The child create failed; if we just minted its parent, undo it so the
		// failed pair leaves no orphan epic (str-qig).
		reject(compensateOrphan(src, minted, err))
		return
	}
	w.Header().Set("HX-Trigger", "refreshList")
	s.renderDrawer(ctx, w, src, issue, nil)
}

// resolveParent turns the picker choice into the --parent id the create should
// carry, enforcing the forced-parent contract. The empty choice is rejected (no
// accidental parentless bead); off-epic resolves to "" (a deliberate root); the
// inline path mints a new epic and returns its id; anything else is taken as an
// existing parent id. The minted issue is returned (non-nil only on the inline
// path) so the caller can compensate it away if the child create then fails: bd
// has no transaction, so a parent-mint that succeeds before a failed child would
// otherwise orphan the epic (str-qig).
func (s *Server) resolveParent(ctx context.Context, src IssueSource, form *createForm) (parent string, minted *bd.Issue, err error) {
	switch form.Parent {
	case "":
		return "", nil, errNoParent
	case parentOffEpic:
		return "", nil, nil
	case sentinelNew:
		if form.ParentNewTitle == "" {
			return "", nil, errNoParentTitle
		}
		epic, err := mintEpic(ctx, src, form.ParentNewTitle)
		if err != nil {
			return "", nil, err
		}
		return epic.ID, epic, nil
	default:
		return form.Parent, nil, nil
	}
}

// mintEpic creates a fresh epic with the given title — the inline new-parent path on
// create and the inline new-epic path on attach both call it. It returns the epic so
// the caller can bind it and compensate it away if a later write fails: bd has no
// transaction, so a mint that succeeds before a failed child create or story reparent
// would otherwise orphan the epic (str-qig). A nil issue with no error is a bd-contract
// violation, surfaced as errEpicMint rather than a silent success.
func mintEpic(ctx context.Context, src IssueSource, title string) (*bd.Issue, error) {
	epic, err := src.Create(ctx, &bd.CreateOpts{Title: title, Type: "epic"})
	if err != nil {
		return nil, err
	}
	if epic == nil {
		return nil, errEpicMint
	}
	return epic, nil
}

// compensateOrphan cleans up a parent epic that was just minted for a create /
// reparent that then failed, so a failed two-call pair leaves no orphan behind.
// bd has no transaction (one serialized write path through the subprocess), so
// this is best-effort compensation, not a rollback. minted is nil whenever the
// parent already existed (an existing pick, or off-epic) — nothing to undo, and
// the cause is returned untouched. The delete runs on a fresh bounded context
// because the request ctx may be the very thing that failed (timeout/cancel) and
// would refuse the compensating write too. If the delete also fails the orphan
// can't be hidden, so the returned error names it for the user to remove by hand.
func compensateOrphan(src IssueSource, minted *bd.Issue, cause error) error {
	if minted == nil {
		return cause
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := src.Delete(ctx, minted.ID); err != nil {
		return fmt.Errorf("%w — and its new parent %s could not be cleaned up; delete it by hand", cause, minted.ID)
	}
	return cause
}

// attachForm is the "No epic" → attach-to-epic drawer's state: the orphan story
// being laddered up, the candidate epics to pick from, the sticky choice (an epic
// id or sentinelNew with a title), and a bd error to show inline on a failed submit.
type attachForm struct {
	StoryID    string
	StoryTitle string
	Epics      []parentOpt // candidate existing epics for the picker
	Epic       string      // picked value: an epic id or sentinelNew
	EpicTitle  string      // title for the inline new-epic path (Epic == sentinelNew)
	Err        string
}

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
		// Only a parentless epic is a real epic root (matches strand.isEpic). An
		// epic-typed bead with a parent is a nested node, not a trunk; attaching an
		// orphan under it would ladder up through a non-root, so leave it out.
		if is.IssueType != "epic" || is.Parent != "" || is.Status == bd.StatusClosed || is.Status == bd.StatusDeferred {
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
	epicID, minted, err := s.resolveAttachEpic(ctx, src, &form)
	if err != nil {
		reject(err)
		return
	}
	if _, err := src.SetParent(ctx, bd.ID(form.StoryID), bd.ID(epicID)); err != nil {
		// The reparent failed; if we just minted the target epic, undo it so the
		// failed attach leaves no orphan epic (str-qig).
		reject(compensateOrphan(src, minted, err))
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
// as the epic id; an empty choice is rejected (no accidental no-op move). The
// minted issue is returned (non-nil only on the inline path) so the caller can
// compensate it away if the reparent then fails — bd has no transaction, so a
// mint-then-failed-SetParent would otherwise orphan the epic (str-qig).
func (s *Server) resolveAttachEpic(ctx context.Context, src IssueSource, form *attachForm) (epicID string, minted *bd.Issue, err error) {
	switch form.Epic {
	case sentinelNew:
		if form.EpicTitle == "" {
			return "", nil, errNoEpicChoice
		}
		epic, err := mintEpic(ctx, src, form.EpicTitle)
		if err != nil {
			return "", nil, err
		}
		return epic.ID, epic, nil
	case "":
		return "", nil, errNoEpicChoice
	default:
		return form.Epic, nil, nil
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
// synthesis project follows the active repo, so a switch re-labels every view). The
// raw list is returned alongside the model so a caller needing both — the board and
// insights reuse the issues for their attention/metric index — lists once through
// here rather than re-spelling the List + Build inline.
func (s *Server) buildStrand(ctx context.Context, src readSource, repo registry.Repo) (strand.Model, []bd.Issue, error) {
	issues, err := src.List(ctx, allIssues...)
	if err != nil {
		return strand.Model{}, nil, fmt.Errorf("list issues: %w", err)
	}
	return strand.Build(issues, s.synFor(repo)), issues, nil
}

// allIssues lifts bd list's default 50-row cap. The strand folds each issue into
// its top-level epic by walking the parent chain, so a truncated list breaks the
// laddering — a story lands off-epic the moment one ancestor row is missing.
// Fetch them all.
var allIssues = []string{"--limit", "0"}

// synFor labels the synthesis with the active repo's name; the project follows the
// active repo, so a switch re-labels every view. buildStrand calls it, so every view
// built through there (landing, board, insights) shares one label.
func (s *Server) synFor(repo registry.Repo) strand.Synthesis {
	syn := s.syn
	syn.Project = repo.Name
	syn.NorthStar = s.northStarFor(repo)
	syn.NorthStarPath = filepath.Join(repo.Path, northStarMiniFile)
	syn.JTBD = jtbd.Load(repo.Path)
	return syn
}

// northStarFor resolves the project's one-line North Star without the JTBD load
// synFor does — the keyed Suggest path wants the masthead line only, not a
// docs/jtbd.md read on every call (JTBD stays inline-cited, never fetched). A
// non-empty s.syn.NorthStar is the --northstar flag and wins; with no flag the
// active repo's canonical north-star-mini.md is read (decision nug 952acad4aca2).
// Missing/empty → "".
// northStarMiniFile is the repo-root file strand reads the masthead line from.
const northStarMiniFile = "north-star-mini.md"

func (s *Server) northStarFor(repo registry.Repo) string {
	if s.syn.NorthStar != "" {
		return s.syn.NorthStar
	}
	return northStarMini(repo.Path)
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
	b, err := os.ReadFile(filepath.Join(repoPath, northStarMiniFile))
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
// issue (404) from bad input (400) from a real upstream failure (502). A blocked
// cross-site write is 403. An error from no known sentinel (e.g. a template failure)
// is ours: 500.
func statusForError(err error) int {
	switch {
	case errors.Is(err, errCrossSite):
		return http.StatusForbidden
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
