// Package counts computes the shared bead-count cache (counts.json) that the
// Claude Code status line and strand's masthead both read. It is the write side of
// the contract internal/bdcounts reads: one process derives every bucket, so the
// two surfaces can never disagree (st-p1f).
//
// It exists to kill a predicate-drift class. The buckets used to be derived in jq
// by bd-counts-refresh.sh, which re-implemented strand's human-gate + lane rules and
// drifted from them (the badge showed 5 while the Waiting pane listed 7). Here the
// ◆/○/● counts come straight from internal/insight.Lanes — the SAME partition the
// pane renders — so the badge is definitionally the set the pane lists (st-2fy.2).
package counts

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/insight"
	"github.com/dkoosis/strand/internal/strandmd"
)

// Row is one repo's counts.json entry. The json tags + field order for root through
// ts match the object bd-counts-refresh.sh emitted, so every existing consumer (the
// status line, bdcounts.Reader) reads a strand-written file unchanged: bh=◆ waiting,
// bo=○ open, bw=◐ in_progress, bb=● blocked, bcl=✓ closed, bdf=❄ deferred; eid/epct =
// the station bar's phase. Epics/Next/Claimed (st-3wp.1) are additive, appended after
// ts — no existing key renamed, retyped, or reordered.
type Row struct {
	Root   string `json:"root"`
	Prefix string `json:"prefix"`
	BH     int    `json:"bh"`
	BO     int    `json:"bo"`
	BW     int    `json:"bw"`
	BB     int    `json:"bb"`
	BCl    int    `json:"bcl"`
	BDf    int    `json:"bdf"`
	EID    string `json:"eid"`
	EPct   *int   `json:"epct"` // nil → JSON null: no roadmap id maps to a live epic
	TS     int64  `json:"ts"`
	// Epics is one ◆○◐● bucket row per live (non-closed) roadmap epic, roadmap
	// order, epics[0] = the current epic. nil → JSON null when the repo has no
	// roadmap or no live epic (no ROADMAP.md/NORTH_STAR.md, all-ghost ids, or every
	// roadmap epic closed) — never an error.
	Epics []EpicRow `json:"epics"`
	// Next is the what's-next cascade's pick (pickNext), or nil when every rung is
	// empty.
	Next *Next `json:"next"`
	// Claimed is the repo-wide in_progress bead pickNext's rung 1 found (the same
	// bead Next names when Next.Reason == "claimed"), or nil when nothing is
	// in_progress.
	Claimed *Ref `json:"claimed"`
}

// EpicRow is one live roadmap epic's ◆○◐● partition over its DIRECT children
// (Issue.Parent == the epic's id), excluding nested epics (issue_type=="epic"). bw
// is the raw in_progress status total, overlapping bh for a gated in-progress child
// — mirroring Row's own repo-level bw rule.
type EpicRow struct {
	ID string `json:"id"`
	BH int    `json:"bh"`
	BO int    `json:"bo"`
	BW int    `json:"bw"`
	BB int    `json:"bb"`
}

// Next is the what's-next cascade's pick: the bead id/title to work next, and which
// rung picked it (one of "claimed", "epic", "waiting-on-dk", "next-epic" — see
// pickNext).
type Next struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Reason string `json:"reason"`
}

// Ref is a bare bead id/title reference — Row.Claimed's shape.
type Ref struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// source is the bd read surface a row derives from. *bd.Client satisfies it; tests
// stub it. The four reads mirror exactly what strand's masthead pulse and station
// bar compute from, so the cache and the live views share one derivation.
type source interface {
	List(ctx context.Context, opts bd.ListOpts) ([]bd.Issue, error)
	Deps(ctx context.Context, ids ...string) ([]bd.DepEdge, error)
	Stats(ctx context.Context) (bd.Stats, error)
	EpicStatus(ctx context.Context) ([]bd.EpicStatus, error)
}

// computeRow derives one repo's row: the six buckets from bd reads folded through
// insight.Lanes, plus the station phase. List/Deps/Stats are load-bearing — an error
// fails the row so the caller keeps the last-good entry rather than zeroing it. The
// station is best-effort: a repo with no roadmap epic still open/in-progress degrades
// to an empty station (eid "", epct null) — that's a real answer, not a failure. An
// EpicStatus *read* failure is different: it carries prevEID/prevEPct forward (st-2fy.7,
// the previous row's station) rather than blanking a station a prior successful run
// already computed — matching the last-good semantics the buckets get from the caller.
// Only the two station fields are threaded through (not the whole prior Row) to keep
// the parameter light.
func computeRow(ctx context.Context, src source, root string, prevEID string, prevEPct *int) (Row, error) {
	issues, err := src.List(ctx, bd.ListOpts{})
	if err != nil {
		return Row{}, fmt.Errorf("list: %w", err)
	}
	ids := make([]string, len(issues))
	for i := range issues {
		ids[i] = issues[i].ID
	}
	deps, err := src.Deps(ctx, ids...)
	if err != nil {
		return Row{}, fmt.Errorf("deps: %w", err)
	}
	stats, err := src.Stats(ctx)
	if err != nil {
		return Row{}, fmt.Errorf("stats: %w", err)
	}
	// Computed once and shared by the repo-level buckets, the epic buckets, and the
	// what's-next cascade — zero new bd execs beyond what this function already
	// fetched (st-3wp.1 §Design/2).
	lanes := insight.Lanes(issues, deps)
	bh, bo, bb := laneCounts(lanes)

	var eid string
	var epct *int
	var epicRows []EpicRow
	var next *Next
	var claimed *Ref
	if epics, err := src.EpicStatus(ctx); err == nil {
		roadmap := strandmd.Roadmap(root)
		eid, epct = station(roadmap, epics)
		liveEpics := liveRoadmapEpics(roadmap, epics)
		epicRows = epicBuckets(liveEpics, issues, lanes)
		next, claimed = pickNext(issues, lanes, currentEpicID(liveEpics), liveEpics)
	} else {
		// EpicStatus read failed: carry the last-good station forward (st-2fy.7,
		// unchanged), degrade epics to nil (no epic data to bucket), and run the
		// cascade with no epic info — rungs 1 and 3 don't need it, so next still
		// resolves from those; a transient blank heals next cycle.
		eid, epct = prevEID, prevEPct
		next, claimed = pickNext(issues, lanes, "", nil)
	}
	return Row{
		Root: root, Prefix: prefix(root),
		BH: bh, BO: bo, BW: stats.InProgress, BB: bb,
		BCl: stats.Closed, BDf: stats.Deferred,
		EID: eid, EPct: epct,
		Epics: epicRows, Next: next, Claimed: claimed,
		TS: lastTouched(root),
	}, nil
}

// laneCounts tallies the three human-facing lanes from an already-computed
// insight.Lanes partition — the same partition strand's Waiting pane lists — so the
// badge ◆ count can't drift from it. ◐ (bw) is NOT taken here: it is the raw
// in_progress status total (stats.InProgress), which overlaps ◆ for a gated
// in-progress bead, matching the masthead.
func laneCounts(lanes map[string]insight.Lane) (bh, bo, bb int) {
	for _, l := range lanes {
		switch l {
		case insight.LaneWaiting:
			bh++
		case insight.LaneOpen:
			bo++
		case insight.LaneBlocked:
			bb++
		case insight.LaneNone:
			// ◐ ungated in_progress / closed / deferred — not a human-facing lane;
			// bw/bcl/bdf come from Stats, not here.
		}
	}
	return bh, bo, bb
}

// liveRoadmapEpics returns the roadmap-ordered epic ids that map to a non-closed
// epic in the EpicStatus set — the "epics" array's own filter, and the source of
// currentEpic (its first element). Deliberately literal ("!= closed", not
// station()'s open/in-progress test): a deferred or blocked epic still counts as
// live/current here; station()'s own eid/epct rule is untouched (it is NOT
// redefined by this bead). A ghost id — present in the roadmap but absent from the
// EpicStatus/DAG set — is skipped, not an error.
func liveRoadmapEpics(roadmap []string, epics []bd.EpicStatus) []string {
	byID := make(map[string]bd.EpicStatus, len(epics))
	for _, e := range epics {
		byID[e.Epic.ID] = e
	}
	var live []string
	for _, id := range roadmap {
		if e, ok := byID[id]; ok && e.Epic.Status != bd.StatusClosed {
			live = append(live, id)
		}
	}
	return live
}

// currentEpicID is the first (roadmap-ordered) live epic, or "" when liveEpics is
// empty — no roadmap, an all-ghost roadmap, or every roadmap epic closed.
func currentEpicID(liveEpics []string) string {
	if len(liveEpics) == 0 {
		return ""
	}
	return liveEpics[0]
}

// epicBuckets builds one ◆○◐● bucket row per live roadmap epic, in liveEpics' order
// (roadmap order), over each epic's DIRECT children only (Issue.Parent == the
// epic's id) — a nested epic child (issue_type=="epic") never counts toward its
// parent's buckets. bw is the raw in_progress status total for that epic's
// children, overlapping bh for a gated in-progress child (mirrors laneCounts' own
// repo-level bw rule). nil when liveEpics is empty, matching the JSON schema's
// "epics: null" for a repo with no live roadmap epic.
func epicBuckets(liveEpics []string, issues []bd.Issue, lanes map[string]insight.Lane) []EpicRow {
	if len(liveEpics) == 0 {
		return nil
	}
	rows := make([]EpicRow, len(liveEpics))
	idx := make(map[string]int, len(liveEpics))
	for i, id := range liveEpics {
		rows[i] = EpicRow{ID: id}
		idx[id] = i
	}
	for _, iss := range issues {
		if iss.IssueType == "epic" {
			continue
		}
		i, ok := idx[iss.Parent]
		if !ok {
			continue
		}
		if iss.Status == bd.StatusInProgress {
			rows[i].BW++
		}
		switch lanes[iss.ID] {
		case insight.LaneWaiting:
			rows[i].BH++
		case insight.LaneOpen:
			rows[i].BO++
		case insight.LaneBlocked:
			rows[i].BB++
		case insight.LaneNone:
		}
	}
	return rows
}

// pickNext runs the four-rung what's-next cascade, epics excluded from every rung
// (an epic never IS the next bead to work). Each rung is tried in order; the first
// non-empty candidate set wins via pickTop (lowest priority number, nil priority
// ranking as P2 to match strand.NewBead's default, ties broken by the
// lexicographically smallest id):
//
//  1. Any in_progress bead, REPO-WIDE — claimed work may live outside currentEpic,
//     so next/claimed can name an epic that isn't currentEpic (intended). claimed
//     mirrors this exact pick; it is nil for every other rung.
//  2. Else currentEpic's direct children with a LaneOpen lane — a human-gated
//     child must NOT win here, it belongs to rung 3.
//  3. Else any LaneWaiting bead repo-wide — insight's ◆ is the human label UNION
//     review_needed (the ratified human-gate), broader than a bare "labeled
//     human" filter would be; using anything narrower would reopen the 5-vs-7
//     drift class laneCounts already fixed at the repo level.
//  4. Else walk liveEpics[1:] (liveEpics is roadmap-ordered with liveEpics[0] ==
//     currentEpic, so this IS "the live epics after currentEpic" — when
//     currentEpic is "" liveEpics is empty too, so this rung naturally yields
//     nothing rather than needing a separate empty-anchor case): the first epic
//     with a LaneOpen direct child wins.
//
// There is deliberately no repo-wide "any ready bead" rung (sdlc plan's
// §Non-goals, bead-verbatim) — a LaneOpen bead outside currentEpic and outside
// every liveEpics[1:] epic is never picked, at any rung.
func pickNext(issues []bd.Issue, lanes map[string]insight.Lane, currentEpic string, liveEpics []string) (next *Next, claimed *Ref) {
	if top, ok := pickTop(filterIssues(issues, func(iss bd.Issue) bool {
		return iss.IssueType != "epic" && iss.Status == bd.StatusInProgress
	})); ok {
		return &Next{ID: top.ID, Title: top.Title, Reason: "claimed"}, &Ref{ID: top.ID, Title: top.Title}
	}

	if currentEpic != "" {
		if top, ok := pickTop(filterIssues(issues, func(iss bd.Issue) bool {
			return iss.IssueType != "epic" && iss.Parent == currentEpic && lanes[iss.ID] == insight.LaneOpen
		})); ok {
			return &Next{ID: top.ID, Title: top.Title, Reason: "epic"}, nil
		}
	}

	if top, ok := pickTop(filterIssues(issues, func(iss bd.Issue) bool {
		return iss.IssueType != "epic" && lanes[iss.ID] == insight.LaneWaiting
	})); ok {
		return &Next{ID: top.ID, Title: top.Title, Reason: "waiting-on-dk"}, nil
	}

	if len(liveEpics) > 1 {
		for _, epicID := range liveEpics[1:] {
			if top, ok := pickTop(filterIssues(issues, func(iss bd.Issue) bool {
				return iss.IssueType != "epic" && iss.Parent == epicID && lanes[iss.ID] == insight.LaneOpen
			})); ok {
				return &Next{ID: top.ID, Title: top.Title, Reason: "next-epic"}, nil
			}
		}
	}

	return nil, nil
}

// filterIssues returns the issues keep reports true for — a small local helper so
// pickNext's four rungs read as one predicate each, not a hand-unrolled loop apiece.
func filterIssues(issues []bd.Issue, keep func(bd.Issue) bool) []bd.Issue {
	var out []bd.Issue
	for _, iss := range issues {
		if keep(iss) {
			out = append(out, iss)
		}
	}
	return out
}

// pickTop returns cands' highest-priority issue per the cascade's tie-break rule:
// lowest priority number wins (nil priority ranks as P2, matching
// strand.NewBead's default so an omitted priority never sorts as falsely urgent
// or falsely low), ties broken by the lexicographically smallest id. ok is false
// for an empty cands.
func pickTop(cands []bd.Issue) (bd.Issue, bool) {
	if len(cands) == 0 {
		return bd.Issue{}, false
	}
	best := cands[0]
	for _, c := range cands[1:] {
		if candLess(c, best) {
			best = c
		}
	}
	return best, true
}

// candPriority is i's cascade priority: its own value, or 2 (P2) when bd omitted
// the field — the same default strand.NewBead applies.
func candPriority(i bd.Issue) int {
	if i.Priority != nil {
		return *i.Priority
	}
	return 2
}

// candLess reports whether a outranks b: lower priority number first, then the
// lexicographically smaller id.
func candLess(a, b bd.Issue) bool {
	pa, pb := candPriority(a), candPriority(b)
	if pa != pb {
		return pa < pb
	}
	return a.ID < b.ID
}

// station walks the roadmap ids in route order and returns the first that is still an
// open/in-progress epic, with its percent-done (closed/total children, rounded). eid
// "" and epct nil when no roadmap id maps to a live epic — the empty station.
func station(roadmap []string, epics []bd.EpicStatus) (string, *int) {
	byID := make(map[string]bd.EpicStatus, len(epics))
	for _, e := range epics {
		byID[e.Epic.ID] = e
	}
	for _, id := range roadmap {
		e, ok := byID[id]
		if !ok || (e.Epic.Status != bd.StatusOpen && e.Epic.Status != bd.StatusInProgress) {
			continue
		}
		pct := 0
		if e.TotalChildren > 0 {
			pct = int(math.Round(float64(e.ClosedChildren) / float64(e.TotalChildren) * 100))
		}
		return id, &pct
	}
	return "", nil
}

// prefix is the repo's short name: the Dolt database from .beads/metadata.json, or
// the directory basename when that's absent — matching the shell's derivation.
func prefix(root string) string {
	data, err := os.ReadFile(filepath.Join(root, ".beads", "metadata.json"))
	if err == nil {
		var m struct {
			DoltDatabase string `json:"dolt_database"`
		}
		if json.Unmarshal(data, &m) == nil && m.DoltDatabase != "" {
			return m.DoltDatabase
		}
	}
	return filepath.Base(root)
}

// lastTouched is the mtime (unix seconds) of the repo's .beads/last-touched, the
// change stamp the shell wrote as ts; 0 when the file is absent.
func lastTouched(root string) int64 {
	fi, err := os.Stat(filepath.Join(root, ".beads", "last-touched"))
	if err != nil {
		return 0
	}
	return fi.ModTime().Unix()
}

// changeKey is the refresh gate: the Dolt store mtime (unix-nanos) — the SAME signal
// the server's snapshot cache evicts on, via the one shared helper bd.StoreMTime.
// last-touched moves only on a local bd write, a strict subset of what changes beads;
// an out-of-band write (bd dolt pull/sync, a direct Dolt commit, bd import) advances
// the store manifest without bumping last-touched, so gating counts on the store mtime
// is what stops the badge stalling while the board stays fresh (st-nm5). Falls back to
// last-touched for a workspace with no embedded Dolt store (StoreMTime ok=false),
// preserving the pre-fix behavior there. The row's ts stays lastTouched for DISPLAY —
// only this gate moved.
func changeKey(root string) int64 {
	if mt, ok := bd.StoreMTime(root); ok {
		return mt.UnixNano()
	}
	return lastTouched(root)
}
