package counts

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/insight"
	"github.com/dkoosis/strand/internal/strandmd"
)

// writeRoadmapDir drops a ROADMAP.md into a fresh repo dir and returns the dir —
// computeRow's strandmd.Roadmap(root) call needs a real file to resolve.
func writeRoadmapDir(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, strandmd.RoadmapFile), []byte(body), 0o644); err != nil {
		t.Fatalf("write roadmap: %v", err)
	}
	return dir
}

// errEpicStatusBoom is a static sentinel for the EpicStatus-failure fixture (err113).
var errEpicStatusBoom = errors.New("bd epic status: boom")

// TestLaneCountsMatchesInsightPartition is the 5-vs-7 repro: the shell's
// `ready ∧ label=human` missed two ◆ beads — a review_needed open bead and a
// human-labelled in-progress bead. laneCounts routes both to bh via insight.Lanes,
// so the badge count becomes the exact set the Waiting pane lists.
func TestLaneCountsMatchesInsightPartition(t *testing.T) {
	issues := []bd.Issue{
		{ID: "open-plain", Status: bd.StatusOpen},                                                     // ○
		{ID: "open-human", Status: bd.StatusOpen, Labels: []string{"human"}},                          // ◆ decision
		{ID: "open-review", Status: bd.StatusOpen, Metadata: map[string]any{"review_needed": "true"}}, // ◆ review
		{ID: "ip-plain", Status: bd.StatusInProgress},                                                 // ◐ only (LaneNone)
		{ID: "ip-human", Status: bd.StatusInProgress, Labels: []string{"human"}},                      // ◆ overlays ◐
		{ID: "blk", Status: bd.StatusBlocked},                                                         // ●
	}
	bh, bo, bb := laneCounts(insight.Lanes(issues, nil))
	if bh != 3 {
		t.Errorf("bh (◆ Waiting) = %d, want 3 — human + review_needed + in_progress-gated", bh)
	}
	if bo != 1 {
		t.Errorf("bo (○ Open) = %d, want 1 — the review_needed bead must NOT count here", bo)
	}
	if bb != 1 {
		t.Errorf("bb (● Blocked) = %d, want 1", bb)
	}
}

// fakeSource is a canned bd read surface for computeRow — no subprocess, no repo.
type fakeSource struct {
	issues   []bd.Issue
	deps     []bd.DepEdge
	stats    bd.Stats
	epics    []bd.EpicStatus
	err      error
	epicsErr error
}

func (f *fakeSource) List(context.Context, bd.ListOpts) ([]bd.Issue, error) {
	return f.issues, f.err
}
func (f *fakeSource) Deps(context.Context, ...string) ([]bd.DepEdge, error) { return f.deps, nil }
func (f *fakeSource) Stats(context.Context) (bd.Stats, error)               { return f.stats, nil }
func (f *fakeSource) EpicStatus(context.Context) ([]bd.EpicStatus, error) {
	return f.epics, f.epicsErr
}

// TestComputeRowAssemblesBuckets pins the full row: lane counts from insight, bw/bcl/
// bdf straight from Stats (bw is the raw in_progress total, overlapping ◆). The repo
// path has no .beads, so prefix falls back to the basename and ts is 0 — the row still
// builds (no roadmap file means no epic buckets, not an error).
func TestComputeRowAssemblesBuckets(t *testing.T) {
	src := &fakeSource{
		issues: []bd.Issue{
			{ID: "o", Status: bd.StatusOpen},
			{ID: "h", Status: bd.StatusOpen, Labels: []string{"human"}},
			{ID: "ip", Status: bd.StatusInProgress},
		},
		stats: bd.Stats{InProgress: 1, Closed: 42, Deferred: 3},
	}
	row, err := computeRow(context.Background(), src, "/tmp/not-a-repo-xyz")
	if err != nil {
		t.Fatalf("computeRow: %v", err)
	}
	if row.BH != 1 || row.BO != 1 {
		t.Errorf("lanes: bh=%d bo=%d, want 1/1", row.BH, row.BO)
	}
	if row.BW != 1 || row.BCl != 42 || row.BDf != 3 {
		t.Errorf("stats buckets: bw=%d bcl=%d bdf=%d, want 1/42/3", row.BW, row.BCl, row.BDf)
	}
	if row.Prefix != "not-a-repo-xyz" {
		t.Errorf("prefix = %q, want the basename fallback", row.Prefix)
	}
	if row.Epics != nil {
		t.Errorf("epics = %v, want nil for a repo with no roadmap file", row.Epics)
	}
}

// --- pickNext: the four-rung "what's next" cascade (st-3wp.1) ---

// TestPickNextRung1ClaimedInProgressWinsRepoWide: rung 1 is repo-wide (claimed work
// may live outside currentEpic) — the lowest-priority in_progress bead wins,
// regardless of which epic it belongs to. An in_progress EPIC-type issue is excluded
// (epics never win any rung). claimed mirrors the same pick.
func TestPickNextRung1ClaimedInProgressWinsRepoWide(t *testing.T) {
	issues := []bd.Issue{
		{ID: "epic-ip", IssueType: "epic", Status: bd.StatusInProgress, Priority: new(0)}, // excluded: it's an epic
		{ID: "e2.a", Parent: "e2", Status: bd.StatusInProgress, Priority: new(2)},
		{ID: "e3.b", Parent: "e3", Status: bd.StatusInProgress, Priority: new(1)}, // lower number wins
		{ID: "e2.c", Parent: "e2", Status: bd.StatusOpen, Priority: new(0)},       // open, not in_progress: not a rung-1 candidate
	}
	lanes := insight.Lanes(issues, nil)
	next, claimed := pickNext(issues, lanes, "e2", []string{"e2"})
	if next == nil || next.ID != "e3.b" || next.Reason != "claimed" {
		t.Fatalf("next = %+v, want {e3.b claimed}", next)
	}
	if claimed == nil || claimed.ID != "e3.b" {
		t.Fatalf("claimed = %+v, want e3.b", claimed)
	}
}

// TestPickNextRung2CurrentEpicOpenChildWins: with nothing in progress, rung 2 is
// scoped to currentEpic's DIRECT children — a human-gated child must NOT win here
// (it belongs to rung 3), and a ready child of a DIFFERENT epic must not be picked
// (proving the scoping, unlike rung 1's repo-wide reach).
func TestPickNextRung2CurrentEpicOpenChildWins(t *testing.T) {
	issues := []bd.Issue{
		{ID: "e2.gated", Parent: "e2", Status: bd.StatusOpen, Labels: []string{"human"}, Priority: new(0)},
		{ID: "e2.open-lo", Parent: "e2", Status: bd.StatusOpen, Priority: new(3)},
		{ID: "e2.open-hi", Parent: "e2", Status: bd.StatusOpen, Priority: new(1)}, // wins: lowest priority open child
		{ID: "e3.open", Parent: "e3", Status: bd.StatusOpen, Priority: new(0)},    // different epic: must not be picked
	}
	lanes := insight.Lanes(issues, nil)
	next, claimed := pickNext(issues, lanes, "e2", []string{"e2", "e3"})
	if next == nil || next.ID != "e2.open-hi" || next.Reason != "epic" {
		t.Fatalf("next = %+v, want {e2.open-hi epic}", next)
	}
	if claimed != nil {
		t.Errorf("claimed = %+v, want nil (rung 2 has no in-progress pick)", claimed)
	}
}

// TestPickNextRung3WaitingRepoWide: with no in-progress bead and no open child in
// currentEpic, rung 3 widens to any human-gated bead repo-wide — and "human-gated"
// is insight's broadened ◆ (label ∪ review_needed), so a review_needed-only bead
// with no "human" label still qualifies on its own (the deliberate broadening
// documented in the plan vs. the bead's literal "labeled human").
func TestPickNextRung3WaitingRepoWide(t *testing.T) {
	tests := []struct {
		name   string
		issues []bd.Issue
		wantID string
	}{
		{
			"human-labeled beats a lower-priority review-needed bead",
			[]bd.Issue{
				{ID: "review", Parent: "e3", Status: bd.StatusOpen, Metadata: map[string]any{"review_needed": "true"}, Priority: new(2)},
				{ID: "human", Parent: "e3", Status: bd.StatusOpen, Labels: []string{"human"}, Priority: new(1)},
			},
			"human",
		},
		{
			"review_needed-only bead, no human label, still qualifies",
			[]bd.Issue{
				{ID: "review-only", Parent: "e3", Status: bd.StatusOpen, Metadata: map[string]any{"review_needed": "true"}, Priority: new(2)},
			},
			"review-only",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lanes := insight.Lanes(tc.issues, nil)
			next, claimed := pickNext(tc.issues, lanes, "e2", []string{"e2", "e3"})
			if next == nil || next.ID != tc.wantID || next.Reason != "waiting-on-dk" {
				t.Fatalf("next = %+v, want {%s waiting-on-dk}", next, tc.wantID)
			}
			if claimed != nil {
				t.Errorf("claimed = %+v, want nil", claimed)
			}
		})
	}
}

// TestPickNextRung4WalksLiveEpicsAfterCurrent: with rungs 1-3 empty, rung 4 walks
// liveEpics[1:] (the roadmap-ordered live epics after currentEpic) and picks the
// first one that HAS a LaneOpen child — skipping an intervening live epic whose only
// child is blocked, not stopping there.
func TestPickNextRung4WalksLiveEpicsAfterCurrent(t *testing.T) {
	issues := []bd.Issue{
		{ID: "e5.blocked", Parent: "e5", Status: bd.StatusBlocked, Priority: new(0)}, // e5 has no open child: skipped
		{ID: "e4.open", Parent: "e4", Status: bd.StatusOpen, Priority: new(1)},       // e4 is the first later epic WITH an open child
	}
	lanes := insight.Lanes(issues, nil)
	next, claimed := pickNext(issues, lanes, "e2", []string{"e2", "e5", "e4"})
	if next == nil || next.ID != "e4.open" || next.Reason != "next-epic" {
		t.Fatalf("next = %+v, want {e4.open next-epic}", next)
	}
	if claimed != nil {
		t.Errorf("claimed = %+v, want nil", claimed)
	}
}

// TestPickNextRung4EmptyWhenNoCurrentEpic: currentEpic == "" (empty roadmap, all-ghost
// ids, or no live epic at all) leaves liveEpics empty too, so epics[1:] has no anchor
// and rung 4 yields nothing rather than re-walking the roadmap from the top.
func TestPickNextRung4EmptyWhenNoCurrentEpic(t *testing.T) {
	issues := []bd.Issue{
		{ID: "e9.open", Parent: "e9", Status: bd.StatusOpen, Priority: new(0)},
	}
	lanes := insight.Lanes(issues, nil)
	next, claimed := pickNext(issues, lanes, "", nil)
	if next != nil {
		t.Errorf("next = %+v, want nil — no current epic means no rung-4 anchor", next)
	}
	if claimed != nil {
		t.Errorf("claimed = %+v, want nil", claimed)
	}
}

// TestPickNextAllRungsEmpty: no candidate at any rung → next=nil, claimed=nil.
func TestPickNextAllRungsEmpty(t *testing.T) {
	next, claimed := pickNext(nil, nil, "", nil)
	if next != nil {
		t.Errorf("next = %+v, want nil", next)
	}
	if claimed != nil {
		t.Errorf("claimed = %+v, want nil", claimed)
	}
}

// TestPickNextTieBreaks pins the tie-break rule: lowest priority number wins, a nil
// priority ranks as P2 (mirroring strand.NewBead's default, so it ties with an
// explicit P2 rather than sorting as highest or lowest), and an exact tie breaks on
// the lexicographically smallest id.
func TestPickNextTieBreaks(t *testing.T) {
	t.Run("nil priority ties with explicit P2, id breaks the tie", func(t *testing.T) {
		issues := []bd.Issue{
			{ID: "z-nil", Status: bd.StatusInProgress}, // Priority nil -> P2
			{ID: "a-p2", Status: bd.StatusInProgress, Priority: new(2)},
		}
		lanes := insight.Lanes(issues, nil)
		next, _ := pickNext(issues, lanes, "", nil)
		if next == nil || next.ID != "a-p2" {
			t.Fatalf("next = %+v, want a-p2 (lexicographically smallest on a P2/P2 tie)", next)
		}
	})
	t.Run("equal explicit priority breaks by id", func(t *testing.T) {
		issues := []bd.Issue{
			{ID: "b", Status: bd.StatusInProgress, Priority: new(1)},
			{ID: "a", Status: bd.StatusInProgress, Priority: new(1)},
		}
		lanes := insight.Lanes(issues, nil)
		next, _ := pickNext(issues, lanes, "", nil)
		if next == nil || next.ID != "a" {
			t.Fatalf("next = %+v, want a", next)
		}
	})
}

// TestPickNextExcludesRepoWideReadyOutsideEveryRung is the bead-verbatim exclusion
// pinned (sdlc plan §Non-goals: "No repo-wide bd-ready pick anywhere in the
// cascade"): a LaneOpen bead exists, but its parent epic is neither currentEpic nor
// reachable via liveEpics[1:], so it sits outside every rung's scope. next must stay
// nil — proving there is no catch-all "any ready bead repo-wide" rung.
func TestPickNextExcludesRepoWideReadyOutsideEveryRung(t *testing.T) {
	issues := []bd.Issue{
		{ID: "orphan.open", Parent: "orphan-epic", Status: bd.StatusOpen, Priority: new(0)},
	}
	lanes := insight.Lanes(issues, nil)
	next, claimed := pickNext(issues, lanes, "e2", []string{"e2"}) // no epic after e2 that could reach orphan-epic
	if next != nil {
		t.Errorf("next = %+v, want nil — a ready bead outside every rung's scope must not be picked", next)
	}
	if claimed != nil {
		t.Errorf("claimed = %+v, want nil", claimed)
	}
}

// --- epicBuckets: the ◆○◐● partition per live roadmap epic ---

// TestEpicBuckets pins the full per-epic partition: bh/bo/bb come from lanes, bw is
// the raw in_progress status total (overlapping bh for a gated in-progress child,
// mirroring the repo-level bw rule), a nested epic child (issue_type=="epic") is
// excluded from every bucket, array order matches the input (roadmap) order, and an
// epic present in the issues but absent from liveEpics contributes nothing.
func TestEpicBuckets(t *testing.T) {
	issues := []bd.Issue{
		{ID: "e1.nested-epic", Parent: "e1", IssueType: "epic", Status: bd.StatusOpen, Priority: new(0)}, // excluded: nested epic
		{ID: "e1.waiting", Parent: "e1", Status: bd.StatusOpen, Labels: []string{"human"}},               // bh
		{ID: "e1.open", Parent: "e1", Status: bd.StatusOpen},                                             // bo
		{ID: "e1.ip-plain", Parent: "e1", Status: bd.StatusInProgress},                                   // bw only
		{ID: "e1.ip-gated", Parent: "e1", Status: bd.StatusInProgress, Labels: []string{"human"}},        // bw AND bh (overlap)
		{ID: "e1.blocked", Parent: "e1", Status: bd.StatusBlocked},                                       // bb
		{ID: "e2.open", Parent: "e2", Status: bd.StatusOpen},                                             // e2's own bucket
		{ID: "e3.open", Parent: "e3", Status: bd.StatusOpen},                                             // e3 not in liveEpics: must be absent
	}
	lanes := insight.Lanes(issues, nil)
	titles := map[string]string{"e1": "First epic", "e3": "Not live"}
	got := epicBuckets([]string{"e1", "e2"}, titles, issues, lanes)
	if len(got) != 2 {
		t.Fatalf("epicBuckets returned %d rows, want 2 (roadmap order, e3 excluded): %+v", len(got), got)
	}
	if got[0].ID != "e1" || got[1].ID != "e2" {
		t.Fatalf("order = [%s %s], want [e1 e2] — roadmap order", got[0].ID, got[1].ID)
	}
	if e1 := got[0]; e1.BH != 2 || e1.BO != 1 || e1.BW != 2 || e1.BB != 1 {
		t.Errorf("e1 buckets = %+v, want bh=2 bo=1 bw=2 bb=1 (ip-gated overlaps bw+bh)", e1)
	}
	if e2 := got[1]; e2.BH != 0 || e2.BO != 1 || e2.BW != 0 || e2.BB != 0 {
		t.Errorf("e2 buckets = %+v, want bo=1 only", e2)
	}
	// Title comes from the EpicStatus lookup, and an epic bd gave no title for
	// renders as empty — not as a missing row.
	if got[0].Title != "First epic" {
		t.Errorf("e1 title = %q, want %q", got[0].Title, "First epic")
	}
	if got[1].Title != "" {
		t.Errorf("e2 title = %q, want empty — no title in the lookup", got[1].Title)
	}
}

// TestEpicBucketsNilForNoLiveEpics: nil roadmap (or an all-ghost/all-closed one) →
// nil, matching the JSON schema's "epics: null" for a repo with no roadmap.
func TestEpicBucketsNilForNoLiveEpics(t *testing.T) {
	if got := epicBuckets(nil, nil, nil, nil); got != nil {
		t.Errorf("epicBuckets(nil roadmap) = %v, want nil", got)
	}
}

// --- liveRoadmapEpics / currentEpicID: the roadmap-ordered live-epic list ---

// TestLiveRoadmapEpics pins the epics-array filter: closed excluded, a deferred (or
// any other non-closed) epic still counts as live — deliberately literal, matching
// currentEpic's "!= closed" rule, which is deliberately looser than the retired station()'s —
// a ghost id (absent from the EpicStatus/DAG set) is skipped, and roadmap order is
// preserved throughout.
func TestLiveRoadmapEpics(t *testing.T) {
	epics := []bd.EpicStatus{
		{Epic: bd.EpicRef{ID: "e1", Status: bd.StatusClosed}},
		{Epic: bd.EpicRef{ID: "e2", Status: bd.StatusDeferred}}, // deferred: still live/current under the "!= closed" rule
		{Epic: bd.EpicRef{ID: "e3", Status: bd.StatusOpen}},
	}
	tests := []struct {
		name    string
		roadmap []string
		want    []string
	}{
		{"closed epic excluded, order preserved", []string{"e1", "e2", "e3"}, []string{"e2", "e3"}},
		{"ghost id (absent from the DAG) skipped", []string{"ghost", "e2"}, []string{"e2"}},
		{"deferred epic counts as live", []string{"e2"}, []string{"e2"}},
		{"empty roadmap", nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := liveRoadmapEpics(tc.roadmap, epics)
			if len(got) != len(tc.want) {
				t.Fatalf("liveRoadmapEpics = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestCurrentEpicID: the first element of the (already-filtered, roadmap-ordered)
// live-epic list, or "" when that list is empty.
func TestCurrentEpicID(t *testing.T) {
	if got := currentEpicID([]string{"e2", "e3"}); got != "e2" {
		t.Errorf("currentEpicID = %q, want e2", got)
	}
	if got := currentEpicID(nil); got != "" {
		t.Errorf("currentEpicID(nil) = %q, want empty", got)
	}
}

// --- computeRow: the new fields assembled end-to-end ---

// TestComputeRowAssemblesEpicsNextAndClaimed: a real ROADMAP.md resolves through
// strandmd.Roadmap, the epic buckets and the cascade both derive from the SAME
// issues/lanes computeRow already fetched (zero new bd execs).
// TestRoadmapPos pins K/N against ALL roadmap ids — a closed epic still occupies a
// slot, which is the whole reason this field exists rather than an epics[] index.
func TestRoadmapPos(t *testing.T) {
	roadmap := []string{"e1", "e2", "e3"} // e1 closed: still counted, still shifts e2 to slot 2
	tests := []struct {
		name        string
		roadmap     []string
		currentEpic string
		want        *RoadmapPos
	}{
		{"current epic mid-roadmap", roadmap, "e2", &RoadmapPos{K: 2, N: 3}},
		{"first slot", roadmap, "e1", &RoadmapPos{K: 1, N: 3}},
		{"last slot", roadmap, "e3", &RoadmapPos{K: 3, N: 3}},
		{"no roadmap", nil, "e2", nil},
		{"no current epic", roadmap, "", nil},
		{"current epic off the roadmap", roadmap, "e9", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := roadmapPos(tt.roadmap, tt.currentEpic)
			switch {
			case tt.want == nil && got != nil:
				t.Fatalf("roadmapPos = %+v, want nil", got)
			case tt.want != nil && got == nil:
				t.Fatalf("roadmapPos = nil, want %+v", tt.want)
			case tt.want != nil && (got.K != tt.want.K || got.N != tt.want.N):
				t.Errorf("roadmapPos = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestEpicTitlesCoversWholeSet: the lookup carries every epic bd reported, not only
// the live ones — a closed epic's title stays reachable for any consumer that wants
// to name it.
func TestEpicTitlesCoversWholeSet(t *testing.T) {
	got := epicTitles([]bd.EpicStatus{
		{Epic: bd.EpicRef{ID: "e1", Title: "Closed one", Status: bd.StatusClosed}},
		{Epic: bd.EpicRef{ID: "e2", Title: "Live one", Status: bd.StatusOpen}},
		{Epic: bd.EpicRef{ID: "e3", Status: bd.StatusOpen}},
	})
	if len(got) != 3 || got["e1"] != "Closed one" || got["e2"] != "Live one" || got["e3"] != "" {
		t.Errorf("epicTitles = %+v, want all three ids with e3 empty", got)
	}
}

func TestComputeRowAssemblesEpicsNextAndClaimed(t *testing.T) {
	src := &fakeSource{
		issues: []bd.Issue{
			{ID: "e2.open", Parent: "e2", Status: bd.StatusOpen, Priority: new(1)},
			{ID: "e2.ip", Parent: "e2", Status: bd.StatusInProgress, Priority: new(0)}, // rung 1 winner
		},
		epics: []bd.EpicStatus{
			{Epic: bd.EpicRef{ID: "e2", Title: "Second epic", Status: bd.StatusOpen}, TotalChildren: 2, ClosedChildren: 0},
		},
	}
	// e1 is on the roadmap but absent from the EpicStatus set (a ghost id), so the
	// live list starts at e2 while the roadmap position still counts e1: 2 of 2.
	root := writeRoadmapDir(t, "## Epics\n1. First → e1\n2. Title → e2\n")
	row, err := computeRow(context.Background(), src, root)
	if err != nil {
		t.Fatalf("computeRow: %v", err)
	}
	if len(row.Epics) != 1 || row.Epics[0].ID != "e2" || row.Epics[0].BO != 1 {
		t.Errorf("Epics = %+v, want one e2 row with bo=1", row.Epics)
	}
	if len(row.Epics) == 1 && row.Epics[0].Title != "Second epic" {
		t.Errorf("Epics[0].Title = %q, want %q — carried from the EpicStatus read", row.Epics[0].Title, "Second epic")
	}
	if row.Roadmap == nil || row.Roadmap.K != 2 || row.Roadmap.N != 2 {
		t.Errorf("Roadmap = %+v, want k=2 n=2 — the current epic's slot among ALL roadmap ids", row.Roadmap)
	}
	if row.Next == nil || row.Next.ID != "e2.ip" || row.Next.Reason != "claimed" {
		t.Errorf("Next = %+v, want e2.ip/claimed", row.Next)
	}
	if row.Claimed == nil || row.Claimed.ID != "e2.ip" {
		t.Errorf("Claimed = %+v, want e2.ip", row.Claimed)
	}
}

// TestComputeRowEpicStatusFailureDegradesEpicsAndNext pins the whole degrade path:
// an EpicStatus read failure yields Epics=nil (no epic data to bucket), while Next
// still derives from the epic-independent rungs (1 and 3) — a transient blank heals
// next cycle, matching the plan's "no new last-good threading" call. Since st-w9v
// dropped eid/epct, this is the only last-good behavior computeRow still owns.
func TestComputeRowEpicStatusFailureDegradesEpicsAndNext(t *testing.T) {
	src := &fakeSource{
		issues: []bd.Issue{
			{ID: "b1", Status: bd.StatusOpen, Labels: []string{"human"}, Priority: new(1)},
		},
		epicsErr: errEpicStatusBoom,
	}
	row, err := computeRow(context.Background(), src, "/tmp/not-a-repo-xyz")
	if err != nil {
		t.Fatalf("computeRow: %v", err)
	}
	if row.Epics != nil {
		t.Errorf("Epics = %v, want nil on EpicStatus failure", row.Epics)
	}
	if row.Next == nil || row.Next.ID != "b1" || row.Next.Reason != "waiting-on-dk" {
		t.Errorf("Next = %+v, want b1/waiting-on-dk — rung 3 is still reachable without epic info", row.Next)
	}
	if row.Roadmap != nil {
		t.Errorf("Roadmap = %+v, want nil on EpicStatus failure — no current epic to place", row.Roadmap)
	}
}

// --- JSON shape: additive fields, explicit null, existing keys untouched ---

// TestRowJSONShapeExplicitNulls: a zero-value Row's nullable fields marshal as
// explicit JSON null (no omitempty) — and every pre-existing key is still present,
// unrenamed.
func TestRowJSONShapeExplicitNulls(t *testing.T) {
	row := Row{Root: "/r", Prefix: "r"}
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"epics", "next", "claimed", "roadmap"} {
		raw, ok := m[key]
		if !ok {
			t.Fatalf("key %q missing from JSON, want present as explicit null", key)
		}
		if string(raw) != "null" {
			t.Errorf("%s = %s, want null for a zero-value Row", key, raw)
		}
	}
	for _, key := range []string{"root", "prefix", "bh", "bo", "bw", "bb", "bcl", "bdf", "ts"} {
		if _, ok := m[key]; !ok {
			t.Errorf("existing key %q missing from JSON — a key was renamed or dropped", key)
		}
	}
	// eid/epct are deliberately gone (st-w9v): they fed a statusline reader that was
	// removed in wrap 0.28.0. Assert their ABSENCE so a revert has to be deliberate.
	for _, key := range []string{"eid", "epct"} {
		if _, ok := m[key]; ok {
			t.Errorf("key %q is back in the JSON — st-w9v dropped it as reader-less", key)
		}
	}
}

// TestRowJSONShapePopulated: a populated Row round-trips epics/next/claimed intact.
func TestRowJSONShapePopulated(t *testing.T) {
	row := Row{
		Epics:   []EpicRow{{ID: "e1", Title: "Epic one", BH: 1}},
		Next:    &Next{ID: "n1", Title: "t", Reason: "claimed"},
		Claimed: &Ref{ID: "n1", Title: "t"},
		Roadmap: &RoadmapPos{K: 2, N: 7},
	}
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Row
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Epics) != 1 || got.Epics[0].ID != "e1" || got.Epics[0].Title != "Epic one" {
		t.Errorf("Epics round-trip = %+v", got.Epics)
	}
	if got.Roadmap == nil || got.Roadmap.K != 2 || got.Roadmap.N != 7 {
		t.Errorf("Roadmap round-trip = %+v, want k=2 n=7", got.Roadmap)
	}
	if got.Next == nil || got.Next.Reason != "claimed" {
		t.Errorf("Next round-trip = %+v", got.Next)
	}
	if got.Claimed == nil || got.Claimed.ID != "n1" {
		t.Errorf("Claimed round-trip = %+v", got.Claimed)
	}
}
