// Package insight is strand's bead-analytics domain: the dashboard math the
// server used to carry inline. It takes a scope's beads, the full-repo issue
// list, and the dependency edges as plain data and returns a Model — never
// touching bd or HTTP. That keeps the analytics unit-testable in isolation
// (fixture beads + a fixed clock) and leaves package server as pure transport.
//
// internal/insight is the sole importer of internal/graph: the structural
// metrics (PageRank/betweenness/critical path) are computed here, behind the
// Compute seam, so the server no longer depends on the graph package directly.
package insight

import (
	"cmp"
	"slices"
	"time"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/graph"
	"github.com/dkoosis/strand/internal/strand"
)

// Model is the V4 dashboard (spec §10): quick-ref counts plus the panels over
// strand's own in-process metrics. Structural panels (Influence, Bottleneck,
// CritPath) read the in-scope closed graph; the triage counts read all blockers
// (a bead can be blocked from outside the scope). The field names are the
// view-facing contract: the insights template binds .Counts/.Ready/… directly.
type Model struct {
	Counts       Counts
	Ready        []RankedBead // ready beads ranked by influence — the dispatch queue
	WaitingOnYou Waiting      // beads parked on a human, excluded from Ready (str-xdy)
	Influence    []RankedBead // top PageRank — foundational beads
	Bottleneck   []RankedBead // top betweenness — chokepoints
	CritPath     []strand.Bead
	Labels       []LabelCount // label distribution over open beads, descending
	Untagged     int          // open beads carrying no label at all
}

// Counts is the quick-ref panel: the live shape of the scope's queue. WaitingOnYou
// counts the open beads diverted out of Ready by the human-gate (str-xdy), so
// Ready means genuinely-claimable work and Open still totals every open bead.
type Counts struct {
	Total, Open, InProgress, Ready, WaitingOnYou, Blocked, Stale int
}

// Waiting is the "Waiting on you" lane: open beads kept out of the ready queue
// because they're parked on a human (str-xdy), sub-grouped by why — a DECISION the
// human must make (the "human" label, bdx's decision queue) vs. a REVIEW of done
// work (metadata.review_needed=="true"). A bead carrying both is a decision.
type Waiting struct {
	Decision []strand.Bead // label "human": a call only the human can make
	Review   []strand.Bead // review_needed=="true": done work awaiting the human's review
}

// Any reports whether the lane holds any bead, so the template can skip the panel
// when nothing is parked on the human.
func (w Waiting) Any() bool {
	return len(w.Decision) > 0 || len(w.Review) > 0
}

// RankedBead is one leaderboard row: a bead, its raw metric score, and a 0–100
// bar width normalized to the panel's top score (computed in Go so the template
// is dumb). Blocked/Stale are the act-now cross-flags: a high-rank row that ALSO
// sits in the blocked or stale set is the one item worth acting on now (spec §3,
// cross-flag).
type RankedBead struct {
	strand.Bead
	Score      float64
	Width      int
	Blocked    bool
	Stale      bool
	Frees      int           // in-scope beads this one transitively unblocks
	Downstream []strand.Bead // those beads, carried so the row can link to each
}

// LabelCount is one row of the label-health distribution.
type LabelCount struct {
	Label string
	Count int
}

// staleAfter is how long an open bead can sit untouched before triage flags it.
const staleAfter = 14 * 24 * time.Hour

// leaderboardSize caps the Influence and Bottleneck panels.
const leaderboardSize = 5

// humanLabel is the bd label marking a bead as parked on a human decision — bdx's
// DECISION queue convention (the human-gate model, cc-plugins bdx/human-gate.md).
const humanLabel = "human"

// reviewNeededKey is the bd metadata key marking a bead as awaiting human review —
// bdx's REVIEW queue. bd emits it as the string "true"; bool is tolerated defensively.
const reviewNeededKey = "review_needed"

// humanGate classifies a bead's human-gate state from its full issue record: a
// DECISION (carries the "human" label) or a REVIEW (review_needed=="true"). A bead
// carrying both is a decision — the stronger "needs a human call" signal — so the
// waiting lane never double-counts it. A bead with neither is neither (claimable).
func humanGate(iss *bd.Issue) (decision, review bool) {
	if iss == nil {
		return false, false
	}
	if slices.Contains(iss.Labels, humanLabel) {
		return true, false
	}
	if reviewNeeded(iss.Metadata) {
		return false, true
	}
	return false, false
}

// isHumanGated reports whether a bead is parked on a human (decision or review) —
// the test that keeps it out of the ready queue and triage's Ready count.
func isHumanGated(iss *bd.Issue) bool {
	d, r := humanGate(iss)
	return d || r
}

// IsHumanGated reports whether an issue is parked on a human (a decision or a
// review). Exported for the masthead pulse's "waiting on you" drill-down, which
// lists the same beads the per-scope waiting lane surfaces.
func IsHumanGated(iss *bd.Issue) bool { return isHumanGated(iss) }

// WaitingCount reports how many of the given issues are parked on a human — the
// masthead pulse's ◆ (and the status line's human segment): a decision (label
// "human") or a review (review_needed). Closed and deferred issues are excluded;
// the pulse counts what is in motion.
func WaitingCount(issues []bd.Issue) int {
	n := 0
	for i := range issues {
		s := issues[i].Status
		if s == bd.StatusClosed || s == bd.StatusDeferred {
			continue
		}
		if isHumanGated(&issues[i]) {
			n++
		}
	}
	return n
}

// reviewNeeded reads metadata.review_needed. bd emits the flag as the string "true";
// a bool true is tolerated in case a future bd quotes it differently (mirrors the
// defensive number/string handling in bd.Issue.Rank). Anything else is "not flagged".
func reviewNeeded(m map[string]any) bool {
	switch v := m[reviewNeededKey].(type) {
	case string:
		return v == "true"
	case bool:
		return v
	default:
		return false
	}
}

// Compute builds the dashboard for a scope. beads is the scope's actionable work
// (callers drop epic containers first); issues is the full repo list (for the
// labels and timestamps the strand drops); deps drives both the in-scope
// structural graph and the all-blockers triage. now is the clock the stale
// cutoff reads — a parameter so the caller's test seam (Server.now) flows in.
//
// The structural metrics run over the in-scope closed "blocks" graph; with no
// such edges every bead ties at PageRank's base, so the leaderboards stay empty
// (a ranking would be noise) and only the ready queue — every ready bead is
// dispatchable — is populated.
func Compute(beads []strand.Bead, issues []bd.Issue, deps []bd.DepEdge, now time.Time) Model {
	ids, inScope := scopeIDs(beads)
	compEdges := blocksEdges(deps, inScope)
	m := graph.Compute(ids, compEdges)

	idx := indexIssues(issues)
	// One blocker scan per request: triage, the ready queue, and both leaderboards
	// all read the same open-blocker tallies, so compute once and share the map.
	openBlockers := blockerCounts(deps, idx)
	frees, down := downstreamReach(compEdges, beads)
	out := Model{
		Counts:   triage(beads, openBlockers, idx, now),
		CritPath: beadPath(m.CriticalPath, beadByID(beads)),
		Labels:   labelHealth(beads, idx),
		Untagged: untaggedOpen(beads, idx),
	}
	// The dispatch queue: ready beads ranked by influence, so the count→actionable
	// gap closes (triage says "2 ready"; this says WHICH, most-impactful first). Ranks
	// even without edges — every ready bead is dispatchable, ordered by PageRank base.
	out.Ready = readyQueue(beads, openBlockers, idx, m.PageRank, frees, down, now)
	// The "Waiting on you" lane: the human-gated beads readyQueue just excluded,
	// grouped decision-vs-review so the parked work has a distinct home (str-xdy).
	// Shares openBlockers so a blocked+gated bead stays out of the lane — it's blocked,
	// not yet waiting on the human (codex P2).
	out.WaitingOnYou = waitingLane(beads, openBlockers, idx)
	// The leaderboards rank by graph position; with no dependencies every bead ties
	// at PageRank's base rank, so a ranking would be noise. Show them only with edges.
	// crossFlag marks the rows that ALSO sit in the blocked/stale sets — the act-now signal.
	if len(compEdges) > 0 {
		out.Influence = withReach(crossFlag(leaderboard(beads, m.PageRank), openBlockers, idx, now), frees, down)
		out.Bottleneck = crossFlag(leaderboard(beads, m.Betweenness), openBlockers, idx, now)
	}
	return out
}

// Actionable drops epic-type beads (containers) from a scope, leaving the real
// work the dashboard reasons about. The caller narrows the scope before Compute
// so the structural graph and triage run over tasks, not the epic shells.
func Actionable(beads []strand.Bead) []strand.Bead {
	out := make([]strand.Bead, 0, len(beads))
	for i := range beads {
		if beads[i].Type != "epic" {
			out = append(out, beads[i])
		}
	}
	return out
}

// scopeIDs lists the scope's bead IDs and a membership set for the edge filter.
func scopeIDs(beads []strand.Bead) ([]string, map[string]bool) {
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
		if d.Type != bd.DepBlocks || !inScope[d.IssueID] || !inScope[d.DependsOnID] {
			continue
		}
		compute = append(compute, graph.Edge{Dependent: d.IssueID, Dependency: d.DependsOnID})
	}
	return compute
}

// indexIssues maps every repo bead by id, so triage and label-health can read the
// fields strand.Bead drops (status of an out-of-scope blocker, timestamps, labels).
func indexIssues(issues []bd.Issue) map[string]bd.Issue {
	m := make(map[string]bd.Issue, len(issues))
	for i := range issues {
		m[issues[i].ID] = issues[i]
	}
	return m
}

// beadByID indexes the scope's beads for the critical-path title lookup.
func beadByID(beads []strand.Bead) map[string]strand.Bead {
	m := make(map[string]strand.Bead, len(beads))
	for i := range beads {
		m[beads[i].ID] = beads[i]
	}
	return m
}

// triage counts the scope's queue shape. ready/blocked weigh ALL of a bead's
// blocks-dependencies (resolved against the full-repo index), since a blocker can
// live outside the visible scope; stale flags live work untouched past the cut.
func triage(beads []strand.Bead, openBlockers map[string]int, idx map[string]bd.Issue, now time.Time) Counts {
	var c Counts
	for i := range beads {
		b := &beads[i]
		c.Total++
		switch b.Status {
		case bd.StatusInProgress:
			c.InProgress++
		case bd.StatusOpen:
			c.Open++
			iss := idx[b.ID]
			switch {
			case openBlockers[b.ID] > 0:
				// Blocked beats the human-gate: a dependency must clear before the
				// human can act, so a blocked+gated bead is Blocked, not waiting — the
				// same precedence readyQueue already applies (blocker check first).
				c.Blocked++
			case isHumanGated(&iss):
				// Parked on a human (decision/review) — not claimable, so it leaves
				// the Ready count and joins the "Waiting on you" lane (str-xdy).
				c.WaitingOnYou++
			default:
				c.Ready++
			}
		case bd.StatusBlocked:
			c.Blocked++
		case bd.StatusClosed, bd.StatusDeferred:
			// Not live work; the strand filter already drops these, so they
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
		if d.Type != bd.DepBlocks {
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
func leaderboard(beads []strand.Bead, score map[string]float64) []RankedBead {
	ranked := make([]RankedBead, 0, len(beads))
	for i := range beads {
		if s := score[beads[i].ID]; s > 0 {
			ranked = append(ranked, RankedBead{Bead: beads[i], Score: s})
		}
	}
	return rankBoard(ranked)
}

// rankBoard is the shared tail of every insights board: sort by score (descending,
// ID tiebreak), cap at leaderboardSize, and size each row's bar against the leader.
// The top>0 guard makes it safe for the ready queue, whose rows can score zero.
func rankBoard(board []RankedBead) []RankedBead {
	slices.SortFunc(board, func(a, b RankedBead) int {
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
func readyQueue(beads []strand.Bead, openBlockers map[string]int, idx map[string]bd.Issue, pr map[string]float64, frees map[string]int, down map[string][]strand.Bead, now time.Time) []RankedBead {
	ready := make([]RankedBead, 0, len(beads))
	for i := range beads {
		b := &beads[i]
		if b.Status != bd.StatusOpen || openBlockers[b.ID] > 0 {
			continue
		}
		// A bead parked on a human (decision/review) isn't claimable work — it
		// belongs to the "Waiting on you" lane, not the dispatch queue (str-xdy).
		iss := idx[b.ID]
		if isHumanGated(&iss) {
			continue
		}
		ready = append(ready, RankedBead{
			Bead:       *b,
			Score:      pr[b.ID],
			Frees:      frees[b.ID],
			Downstream: down[b.ID],
			Stale:      isStale(b.Status, idx[b.ID].UpdatedAt, now),
		})
	}
	// Rank by what each bead frees (the dispatch payoff), PageRank then id breaking
	// ties, and size the bar against the bead that frees the most. With no edges every
	// bead frees zero, so the queue stays ordered by influence and shows no bars.
	slices.SortFunc(ready, func(a, b RankedBead) int {
		if a.Frees != b.Frees {
			return cmp.Compare(b.Frees, a.Frees)
		}
		if a.Score != b.Score {
			return cmp.Compare(b.Score, a.Score)
		}
		return cmp.Compare(a.ID, b.ID)
	})
	if len(ready) > leaderboardSize {
		ready = ready[:leaderboardSize]
	}
	if len(ready) > 0 && ready[0].Frees > 0 {
		top := ready[0].Frees
		for i := range ready {
			ready[i].Width = ready[i].Frees * 100 / top
		}
	}
	return ready
}

// waitingLane gathers the scope's live beads that are parked on a human and groups
// them by why (str-xdy): a DECISION (label "human") vs. a REVIEW (review_needed).
// These are the beads readyQueue/triage divert out of the ready view, so the "ready
// column" means genuinely-claimable work and parked beads still have a home. Only
// open beads are considered — the same live, not-yet-claimed work the ready view
// reasons about; closed/deferred work has already left the strand. Order follows the
// scope's bead order (priority-then-id, as the strand sorts), so the lane reads stably.
// A bead with an unmet blocker is left out — it's blocked, not yet waiting on the
// human (the dependency must clear first), matching readyQueue's precedence (codex P2).
func waitingLane(beads []strand.Bead, openBlockers map[string]int, idx map[string]bd.Issue) Waiting {
	var w Waiting
	for i := range beads {
		if beads[i].Status != bd.StatusOpen || openBlockers[beads[i].ID] > 0 {
			continue
		}
		iss := idx[beads[i].ID]
		switch decision, review := humanGate(&iss); {
		case decision:
			w.Decision = append(w.Decision, beads[i])
		case review:
			w.Review = append(w.Review, beads[i])
		}
	}
	return w
}

// downstreamReach computes, for each bead, the in-scope beads it transitively unblocks
// — its "frees" set — over the closed blocks-graph. Edge{Dependent, Dependency} means
// Dependency blocks Dependent, so reachability follows Dependency→Dependent.
func downstreamReach(edges []graph.Edge, beads []strand.Bead) (map[string]int, map[string][]strand.Bead) {
	adj := map[string][]string{}
	for _, e := range edges {
		adj[e.Dependency] = append(adj[e.Dependency], e.Dependent)
	}
	by := beadByID(beads)
	frees := make(map[string]int, len(beads))
	down := make(map[string][]strand.Bead, len(beads))
	for i := range beads {
		seen := map[string]bool{}
		stack := append([]string(nil), adj[beads[i].ID]...)
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[n] {
				continue
			}
			seen[n] = true
			stack = append(stack, adj[n]...)
		}
		if len(seen) == 0 {
			continue
		}
		ds := make([]strand.Bead, 0, len(seen))
		for id := range seen {
			if b, ok := by[id]; ok {
				ds = append(ds, b)
			}
		}
		slices.SortFunc(ds, func(a, b strand.Bead) int { return cmp.Compare(a.ID, b.ID) })
		frees[beads[i].ID] = len(ds)
		down[beads[i].ID] = ds
	}
	return frees, down
}

// withReach attaches each row's frees count and downstream beads, so a leaderboard
// row can show "frees N" and link straight to the beads it would unblock.
func withReach(board []RankedBead, frees map[string]int, down map[string][]strand.Bead) []RankedBead {
	for i := range board {
		board[i].Frees = frees[board[i].ID]
		board[i].Downstream = down[board[i].ID]
	}
	return board
}

// crossFlag marks each ranked row that ALSO sits in the blocked or stale set — the
// act-now signal (spec §3): a high-rank bottleneck that is itself blocked/stale is the
// one item worth acting on now. Blocked weighs unmet blocks-dependencies (a bd-reported
// "blocked" status also counts); stale reuses the triage cutoff.
func crossFlag(board []RankedBead, openBlockers map[string]int, idx map[string]bd.Issue, now time.Time) []RankedBead {
	for i := range board {
		id := board[i].ID
		board[i].Blocked = openBlockers[id] > 0 || board[i].Status == bd.StatusBlocked
		board[i].Stale = isStale(board[i].Status, idx[id].UpdatedAt, now)
	}
	return board
}

// beadPath resolves the critical-path ids to scope beads (for their titles),
// dropping any id not in scope so the panel can't render a blank row.
func beadPath(path []string, byID map[string]strand.Bead) []strand.Bead {
	out := make([]strand.Bead, 0, len(path))
	for _, id := range path {
		if b, ok := byID[id]; ok {
			out = append(out, b)
		}
	}
	return out
}

// labelHealth tallies labels across the scope's open beads, descending by count
// then name, so the panel surfaces what the live work is tagged with.
func labelHealth(beads []strand.Bead, idx map[string]bd.Issue) []LabelCount {
	count := map[string]int{}
	for i := range beads {
		if beads[i].Status != bd.StatusOpen {
			continue
		}
		for _, l := range idx[beads[i].ID].Labels {
			count[l]++
		}
	}
	out := make([]LabelCount, 0, len(count))
	for l, n := range count {
		out = append(out, LabelCount{Label: l, Count: n})
	}
	slices.SortFunc(out, func(a, b LabelCount) int {
		if a.Count != b.Count {
			return cmp.Compare(b.Count, a.Count)
		}
		return cmp.Compare(a.Label, b.Label)
	})
	return out
}

// untaggedOpen counts open beads carrying no label — the hygiene warning that pairs
// with the distribution.
func untaggedOpen(beads []strand.Bead, idx map[string]bd.Issue) int {
	n := 0
	for i := range beads {
		if beads[i].Status == bd.StatusOpen && len(idx[beads[i].ID].Labels) == 0 {
			n++
		}
	}
	return n
}
