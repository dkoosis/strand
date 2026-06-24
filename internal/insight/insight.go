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
	Counts     Counts
	Ready      []RankedBead // ready beads ranked by influence — the dispatch queue
	Influence  []RankedBead // top PageRank — foundational beads
	Bottleneck []RankedBead // top betweenness — chokepoints
	CritPath   []strand.Bead
	Labels     []LabelCount // label distribution over open beads, descending
	Untagged   int          // open beads carrying no label at all
}

// Counts is the quick-ref panel: the live shape of the scope's queue.
type Counts struct {
	Total, Open, InProgress, Ready, Blocked, Stale int
}

// RankedBead is one leaderboard row: a bead, its raw metric score, and a 0–100
// bar width normalized to the panel's top score (computed in Go so the template
// is dumb). Blocked/Stale are the act-now cross-flags: a high-rank row that ALSO
// sits in the blocked or stale set is the one item worth acting on now (spec §3,
// cross-flag).
type RankedBead struct {
	strand.Bead
	Score   float64
	Width   int
	Blocked bool
	Stale   bool
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
	out := Model{
		Counts:   triage(beads, openBlockers, idx, now),
		CritPath: beadPath(m.CriticalPath, beadByID(beads)),
		Labels:   labelHealth(beads, idx),
		Untagged: untaggedOpen(beads, idx),
	}
	// The dispatch queue: ready beads ranked by influence, so the count→actionable
	// gap closes (triage says "2 ready"; this says WHICH, most-impactful first). Ranks
	// even without edges — every ready bead is dispatchable, ordered by PageRank base.
	out.Ready = readyQueue(beads, openBlockers, idx, m.PageRank, now)
	// The leaderboards rank by graph position; with no dependencies every bead ties
	// at PageRank's base rank, so a ranking would be noise. Show them only with edges.
	// crossFlag marks the rows that ALSO sit in the blocked/stale sets — the act-now signal.
	if len(compEdges) > 0 {
		out.Influence = crossFlag(leaderboard(beads, m.PageRank), openBlockers, idx, now)
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
		if d.Type != "blocks" || !inScope[d.IssueID] || !inScope[d.DependsOnID] {
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
			if openBlockers[b.ID] > 0 {
				c.Blocked++
			} else {
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
func readyQueue(beads []strand.Bead, openBlockers map[string]int, idx map[string]bd.Issue, score map[string]float64, now time.Time) []RankedBead {
	ready := make([]RankedBead, 0, len(beads))
	for i := range beads {
		b := &beads[i]
		if b.Status != bd.StatusOpen || openBlockers[b.ID] > 0 {
			continue
		}
		ready = append(ready, RankedBead{
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
