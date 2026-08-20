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
	"os"
	"path/filepath"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/insight"
	"github.com/dkoosis/strand/internal/strandmd"
)

// Row is one repo's counts.json entry. The json tags + field order for root through
// ts match the object bd-counts-refresh.sh emitted, so every existing consumer (the
// status line, bdcounts.Reader) reads a strand-written file unchanged: bh=◆ waiting,
// bo=○ open, bw=◐ in_progress, bb=● blocked, bcl=✓ closed, bdf=❄ deferred.
// Epics/Next/Claimed (st-3wp.1) are additive, appended after ts.
//
// eid/epct are GONE (st-w9v). They carried the station bar's phase for wrap's
// statusline, whose last reader went away in wrap 0.28.0; nothing has read either
// key since. Dropping them is safe for the same reason adding epics/next was: every
// consumer decodes by name into its own struct (bdcounts.Reader, the statusline's
// jq), so an absent key it never mentions cannot affect it. A station derived from
// the roadmap could come back as a real feature — it would be new work, not this
// field.
type Row struct {
	Root   string `json:"root"`
	Prefix string `json:"prefix"`
	BH     int    `json:"bh"`
	BO     int    `json:"bo"`
	BW     int    `json:"bw"`
	BB     int    `json:"bb"`
	BCl    int    `json:"bcl"`
	BDf    int    `json:"bdf"`
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
	// Roadmap is the current epic's position among ALL roadmap ids, closed
	// included — the "epic 3 of 7" a banner states. nil when there is no roadmap
	// or no live epic (the same two conditions that null Epics).
	Roadmap *RoadmapPos `json:"roadmap"`
}

// RoadmapPos is Epics[0]'s 1-based position K among the roadmap's N ordered epic
// ids. N counts every id the roadmap names, including closed epics, so K/N reads
// as progress through the project — Epics[] indexes only the live ones and cannot
// express it.
type RoadmapPos struct {
	K int `json:"k"`
	N int `json:"n"`
}

// issueTypeEpic is bd's issue_type value for an epic — used to exclude epics
// themselves from the epic-buckets child count and from every pickNext rung (an
// epic never IS the next bead to work).
const issueTypeEpic = "epic"

// EpicRow is one live roadmap epic's ◆○◐● partition over its DIRECT children
// (Issue.Parent == the epic's id), excluding nested epics (issue_type=="epic"). bw
// is the raw in_progress status total, overlapping bh for a gated in-progress child
// — mirroring Row's own repo-level bw rule.
type EpicRow struct {
	ID string `json:"id"`
	// Title is bd's epic title, from the same EpicStatus read the buckets derive
	// from — so a consumer renders the epic by name without a second bd fork.
	// Empty when bd omitted it.
	Title string `json:"title"`
	BH    int    `json:"bh"`
	BO    int    `json:"bo"`
	BW    int    `json:"bw"`
	BB    int    `json:"bb"`
	// BCl and N are bd's OWN child roll-up for this epic — closed children and
	// total children, straight from the EpicStatus read. The four lane buckets
	// above partition only LIVE children (bd list omits closed), so they cannot
	// express "how far through this epic are we"; these two can, and a consumer
	// renders ✓/a progress bar from them without a second bd fork. They are bd's
	// count, not ours: a nested epic child counts here and not in the buckets.
	BCl int `json:"bcl"`
	N   int `json:"n"`
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
// insight.Lanes, plus the epic buckets and the what's-next pick. List/Deps/Stats are
// load-bearing — an error fails the row so the caller keeps the last-good entry
// rather than zeroing it. The epic-derived fields are best-effort: an EpicStatus read
// failure degrades epics to nil and runs the cascade with no epic info, and a
// transient blank heals next cycle.
func computeRow(ctx context.Context, src source, root string) (Row, error) {
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

	roadmap := strandmd.Roadmap(root)
	var epicRows []EpicRow
	var next *Next
	var claimed *Ref
	var pos *RoadmapPos
	if epics, err := src.EpicStatus(ctx); err == nil {
		liveEpics := liveRoadmapEpics(roadmap, epics)
		epicRows = epicBuckets(liveEpics, epicMeta(epics), issues, lanes)
		next, claimed = pickNext(issues, lanes, currentEpicID(liveEpics), liveEpics)
		pos = roadmapPos(roadmap, currentEpicID(liveEpics))
	} else {
		// EpicStatus read failed: degrade epics to nil (no epic data to bucket) and
		// run the cascade with no epic info — rungs 1 and 3 don't need it, so next
		// still resolves from those; a transient blank heals next cycle.
		next, claimed = pickNext(issues, lanes, "", nil)
	}
	return Row{
		Root: root, Prefix: prefix(root),
		BH: bh, BO: bo, BW: stats.InProgress, BB: bb,
		BCl: stats.Closed, BDf: stats.Deferred,
		Epics: epicRows, Next: next, Claimed: claimed,
		Roadmap: pos,
		TS:      lastTouched(root),
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
// currentEpic (its first element). Deliberately literal: "!= closed", so a deferred
// or blocked epic still counts as live and can be the current one. (The retired
// station() used a narrower open/in-progress test; when it went with eid/epct in
// st-w9v this became the only live-epic rule in the package.) A ghost id — present
// in the roadmap but absent from the EpicStatus/DAG set — is skipped, not an error.
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

// epicMeta maps epic id → the EpicStatus facts EpicRow carries beyond its own lane
// buckets: bd's title and bd's closed/total child roll-up. Built over the WHOLE
// EpicStatus set, not only the live epics, so a closed epic stays reachable for any
// consumer that wants to name it. An epic bd reported no title for maps to "",
// which renders as an absent title rather than an error.
func epicMeta(epics []bd.EpicStatus) map[string]bd.EpicStatus {
	meta := make(map[string]bd.EpicStatus, len(epics))
	for _, e := range epics {
		meta[e.Epic.ID] = e
	}
	return meta
}

// roadmapPos locates currentEpic among ALL roadmap ids — closed epics included, so
// K/N reads as progress through the project rather than through what happens to be
// live. nil when there is no roadmap, no current epic, or the current epic somehow
// is not on the roadmap (impossible today: liveRoadmapEpics only ever returns ids
// it walked the roadmap to find, but the guard keeps the two independent).
func roadmapPos(roadmap []string, currentEpic string) *RoadmapPos {
	if len(roadmap) == 0 || currentEpic == "" {
		return nil
	}
	for i, id := range roadmap {
		if id == currentEpic {
			return &RoadmapPos{K: i + 1, N: len(roadmap)}
		}
	}
	return nil
}

// epicBuckets builds one ◆○◐● bucket row per live roadmap epic, in liveEpics' order
// (roadmap order), over each epic's DIRECT children only (Issue.Parent == the
// epic's id) — a nested epic child (issue_type=="epic") never counts toward its
// parent's buckets. bw is the raw in_progress status total for that epic's
// children, overlapping bh for a gated in-progress child (mirrors laneCounts' own
// repo-level bw rule). nil when liveEpics is empty, matching the JSON schema's
// "epics: null" for a repo with no live roadmap epic.
func epicBuckets(liveEpics []string, meta map[string]bd.EpicStatus, issues []bd.Issue, lanes map[string]insight.Lane) []EpicRow {
	if len(liveEpics) == 0 {
		return nil
	}
	rows := make([]EpicRow, len(liveEpics))
	idx := make(map[string]int, len(liveEpics))
	for i, id := range liveEpics {
		m := meta[id]
		rows[i] = EpicRow{ID: id, Title: m.Epic.Title, BCl: m.ClosedChildren, N: m.TotalChildren}
		idx[id] = i
	}
	for i := range issues {
		iss := &issues[i]
		if iss.IssueType == issueTypeEpic {
			continue
		}
		ri, ok := idx[iss.Parent]
		if !ok {
			continue
		}
		if iss.Status == bd.StatusInProgress {
			rows[ri].BW++
		}
		switch lanes[iss.ID] {
		case insight.LaneWaiting:
			rows[ri].BH++
		case insight.LaneOpen:
			rows[ri].BO++
		case insight.LaneBlocked:
			rows[ri].BB++
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
	if top, ok := pickTop(filterIssues(issues, func(iss *bd.Issue) bool {
		return iss.IssueType != issueTypeEpic && iss.Status == bd.StatusInProgress
	})); ok {
		return &Next{ID: top.ID, Title: top.Title, Reason: "claimed"}, &Ref{ID: top.ID, Title: top.Title}
	}

	if currentEpic != "" {
		if top, ok := pickTop(filterIssues(issues, func(iss *bd.Issue) bool {
			return iss.IssueType != issueTypeEpic && iss.Parent == currentEpic && lanes[iss.ID] == insight.LaneOpen
		})); ok {
			return &Next{ID: top.ID, Title: top.Title, Reason: "epic"}, nil
		}
	}

	if top, ok := pickTop(filterIssues(issues, func(iss *bd.Issue) bool {
		return iss.IssueType != issueTypeEpic && lanes[iss.ID] == insight.LaneWaiting
	})); ok {
		return &Next{ID: top.ID, Title: top.Title, Reason: "waiting-on-dk"}, nil
	}

	if len(liveEpics) > 1 {
		for _, epicID := range liveEpics[1:] {
			if top, ok := pickTop(filterIssues(issues, func(iss *bd.Issue) bool {
				return iss.IssueType != issueTypeEpic && iss.Parent == epicID && lanes[iss.ID] == insight.LaneOpen
			})); ok {
				return &Next{ID: top.ID, Title: top.Title, Reason: "next-epic"}, nil
			}
		}
	}

	return nil, nil
}

// filterIssues returns pointers into issues for every element keep reports true for
// — a small local helper so pickNext's four rungs read as one predicate each, not a
// hand-unrolled loop apiece. Pointers (not copies) keep bd.Issue's ~240 bytes from
// being duplicated through the filter+pick chain.
func filterIssues(issues []bd.Issue, keep func(*bd.Issue) bool) []*bd.Issue {
	var out []*bd.Issue
	for i := range issues {
		if keep(&issues[i]) {
			out = append(out, &issues[i])
		}
	}
	return out
}

// pickTop returns cands' highest-priority issue per the cascade's tie-break rule:
// lowest priority number wins (nil priority ranks as P2, matching
// strand.NewBead's default so an omitted priority never sorts as falsely urgent
// or falsely low), ties broken by the lexicographically smallest id. ok is false
// for an empty cands.
func pickTop(cands []*bd.Issue) (*bd.Issue, bool) {
	if len(cands) == 0 {
		return nil, false
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
func candPriority(i *bd.Issue) int {
	if i.Priority != nil {
		return *i.Priority
	}
	return 2
}

// candLess reports whether a outranks b: lower priority number first, then the
// lexicographically smaller id.
func candLess(a, b *bd.Issue) bool {
	pa, pb := candPriority(a), candPriority(b)
	if pa != pb {
		return pa < pb
	}
	return a.ID < b.ID
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

// changeKey is the refresh gate: the Dolt store's CONTENT key (bd.StoreContentKey),
// not its mtime. It used to be the store mtime — the same signal the server's
// snapshot cache still evicts on via bd.StoreMTime — but every bd read (including
// the refresher's own four reads per repo) rewrites the manifest's mtime with
// byte-identical content, so a mtime-keyed gate never let a repo settle: each cycle
// re-armed the st-3p8 pending bit for every repo the refresher itself had just
// visited (st-3wp.1, ~25 repos x 4 execs x ~0.35s ≈ 35s warm). Content only moves on
// a genuine write — a local bd write, or an out-of-band bd dolt pull/sync/import —
// so st-nm5's out-of-band-change detection is preserved: last-touched moves only on
// a local bd write, a strict subset of what changes beads, and an out-of-band write
// advances the store manifest's content without bumping last-touched. Falls back to
// last-touched for a workspace with no embedded Dolt store (StoreContentKey
// ok=false), preserving the pre-fix behavior there. The row's ts stays lastTouched
// for DISPLAY — only this gate moved.
func changeKey(root string) int64 {
	if key, ok := bd.StoreContentKey(root); ok {
		return key
	}
	return lastTouched(root)
}
