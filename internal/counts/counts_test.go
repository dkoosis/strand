package counts

import (
	"context"
	"errors"
	"testing"

	"github.com/dkoosis/strand/internal/bd"
)

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
	bh, bo, bb := laneCounts(issues, nil)
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

func TestStation(t *testing.T) {
	epics := []bd.EpicStatus{
		{Epic: bd.EpicRef{ID: "e1", Status: bd.StatusClosed}, TotalChildren: 4, ClosedChildren: 4},
		{Epic: bd.EpicRef{ID: "e2", Status: bd.StatusOpen}, TotalChildren: 8, ClosedChildren: 2},
		{Epic: bd.EpicRef{ID: "e3", Status: bd.StatusInProgress}, TotalChildren: 3, ClosedChildren: 1},
	}
	tests := []struct {
		name     string
		roadmap  []string
		epics    []bd.EpicStatus
		wantEID  string
		wantPct  int
		wantNull bool
	}{
		{"first live epic wins, rounds pct", []string{"e1", "e2", "e3"}, epics, "e2", 25, false},
		{"skips closed to next live", []string{"e1", "e3"}, epics, "e3", 33, false},
		{"skips ids absent from the DAG", []string{"ghost", "e2"}, epics, "e2", 25, false},
		{"no live epic → empty station", []string{"e1"}, epics, "", 0, true},
		{"empty roadmap → empty station", nil, epics, "", 0, true},
		{"zero children → 0 pct, not divide-by-zero", []string{"z"}, []bd.EpicStatus{{Epic: bd.EpicRef{ID: "z", Status: bd.StatusOpen}}}, "z", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eid, epct := station(tc.roadmap, tc.epics)
			if eid != tc.wantEID {
				t.Errorf("eid = %q, want %q", eid, tc.wantEID)
			}
			if tc.wantNull {
				if epct != nil {
					t.Errorf("epct = %d, want nil", *epct)
				}
				return
			}
			if epct == nil || *epct != tc.wantPct {
				t.Errorf("epct = %v, want %d", epct, tc.wantPct)
			}
		})
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
// builds (a station with no roadmap file is empty, not an error).
func TestComputeRowAssemblesBuckets(t *testing.T) {
	src := &fakeSource{
		issues: []bd.Issue{
			{ID: "o", Status: bd.StatusOpen},
			{ID: "h", Status: bd.StatusOpen, Labels: []string{"human"}},
			{ID: "ip", Status: bd.StatusInProgress},
		},
		stats: bd.Stats{InProgress: 1, Closed: 42, Deferred: 3},
	}
	row, err := computeRow(context.Background(), src, "/tmp/not-a-repo-xyz", "", nil)
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
	if row.EID != "" || row.EPct != nil {
		t.Errorf("station should be empty for a repo with no roadmap: eid=%q epct=%v", row.EID, row.EPct)
	}
}

// TestComputeRowStationLastGoodOnEpicStatusFailure is the st-2fy.7 fix: a transient
// EpicStatus error must NOT blank a station that a prior successful run computed. The
// buckets already get this last-good treatment from refresh() (a repo whose reads fail
// keeps its previous row); computeRow now extends the same guarantee to its one
// best-effort sub-read by taking the prior row and carrying its eid/epct forward on
// EpicStatus failure, rather than degrading to the empty station.
func TestComputeRowStationLastGoodOnEpicStatusFailure(t *testing.T) {
	src := &fakeSource{epicsErr: errEpicStatusBoom}
	prevPct := 42

	row, err := computeRow(context.Background(), src, "/tmp/not-a-repo-xyz", "st-2fy", &prevPct)
	if err != nil {
		t.Fatalf("computeRow: %v", err)
	}
	if row.EID != "st-2fy" {
		t.Errorf("eid = %q, want carried-forward %q", row.EID, "st-2fy")
	}
	if row.EPct == nil || *row.EPct != prevPct {
		t.Errorf("epct = %v, want carried-forward %d", row.EPct, prevPct)
	}
}

// TestComputeRowStationClearsOnGenuineEmpty proves the carry-forward is scoped to the
// EpicStatus *failure* path only: a successful EpicStatus call that legitimately finds
// no live roadmap epic must still clear a stale prior station, not stick forever.
func TestComputeRowStationClearsOnGenuineEmpty(t *testing.T) {
	src := &fakeSource{epics: nil} // succeeds, no epics → station() returns "", nil
	prevPct := 42

	row, err := computeRow(context.Background(), src, "/tmp/not-a-repo-xyz", "st-2fy", &prevPct)
	if err != nil {
		t.Fatalf("computeRow: %v", err)
	}
	if row.EID != "" || row.EPct != nil {
		t.Errorf("eid=%q epct=%v, want empty station — a real result must overwrite a stale prior one", row.EID, row.EPct)
	}
}
