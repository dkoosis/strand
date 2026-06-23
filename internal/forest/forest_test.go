package forest

import (
	"math"
	"testing"

	"github.com/dkoosis/strand/internal/bd"
)

// TestBuildGroupsByRootAndCountsOpen pins the core synthesis: live issues group
// under their top-level ancestor, closed/deferred drop out, and tile weight is
// the open count.
func TestBuildGroupsByRootAndCountsOpen(t *testing.T) {
	issues := []bd.Issue{
		{ID: "p-1", Title: "Epic one", IssueType: "epic", Status: "open"},
		{ID: "p-1.a", Parent: "p-1", Status: "open", Priority: 2},
		{ID: "p-1.b", Parent: "p-1", Status: "in_progress", Priority: 0},
		{ID: "p-1.c", Parent: "p-1", Status: "closed", Priority: 2}, // excluded
		{ID: "p-2", Title: "Lone feature", IssueType: "feature", Status: "open", Priority: 3},
		{ID: "p-3", Title: "Done epic", IssueType: "epic", Status: "closed"}, // no live work
	}
	f := Build(issues, Synthesis{Project: "demo", NorthStar: "north"})

	if f.NorthStar != "north" {
		t.Errorf("NorthStar = %q, want %q", f.NorthStar, "north")
	}
	if len(f.Regions) != 1 {
		t.Fatalf("Regions = %d, want 1", len(f.Regions))
	}
	if f.Regions[0].Name != "demo" {
		t.Errorf("region Name = %q, want demo", f.Regions[0].Name)
	}
	// p-1 (root + a + b = 3 open) and p-2 (1 open); p-3 has no live work.
	if got := len(f.Regions[0].Epics); got != 2 {
		t.Fatalf("epics = %d, want 2", got)
	}
	// Largest first: p-1 has 3 open, p-2 has 1.
	e := f.Regions[0].Epics
	if e[0].ID != "p-1" || e[0].Open != 3 {
		t.Errorf("epic[0] = %s/%d, want p-1/3", e[0].ID, e[0].Open)
	}
	if e[1].ID != "p-2" || e[1].Open != 1 {
		t.Errorf("epic[1] = %s/%d, want p-2/1", e[1].ID, e[1].Open)
	}
	if !e[0].Flag {
		t.Error("epic p-1 holds a P0 bead, want Flag=true")
	}
	if e[1].Flag {
		t.Error("epic p-2 has only P3 work, want Flag=false")
	}
	if f.Open != 4 {
		t.Errorf("Open = %d, want 4", f.Open)
	}
	if f.InProgress != 1 {
		t.Errorf("InProgress = %d, want 1", f.InProgress)
	}
}

// TestBuildBeadsSortPriorityThenID pins the in-tile bead order the list renders.
func TestBuildBeadsSortPriorityThenID(t *testing.T) {
	issues := []bd.Issue{
		{ID: "p-1", Title: "E", IssueType: "epic", Status: "open", Priority: 2},
		{ID: "p-1.hi", Parent: "p-1", Status: "open", Priority: 0},
		{ID: "p-1.lo", Parent: "p-1", Status: "open", Priority: 3},
	}
	f := Build(issues, Synthesis{Project: "demo"})
	beads := f.Regions[0].Epics[0].Beads
	want := []string{"p-1.hi", "p-1", "p-1.lo"} // P0, P2, P3
	for i, id := range want {
		if beads[i].ID != id {
			t.Errorf("bead[%d] = %s, want %s", i, beads[i].ID, id)
		}
	}
}

// A manually-ranked epic orders by rank (ascending), overriding priority. The
// rank lives in bd metadata; a P3 ranked ahead of a P0 leads the list.
func TestBuildRankedEpicSortsByRank(t *testing.T) {
	// The seed invariant ranks every member, including the epic root (a member of
	// its own list, P2 here), so the group is wholly rank-ordered.
	issues := []bd.Issue{
		{ID: "p-1", Title: "E", IssueType: "epic", Status: "open", Priority: 2, Metadata: map[string]any{"rank": 4.0}},
		{ID: "p-1.a", Parent: "p-1", Status: "open", Priority: 0, Metadata: map[string]any{"rank": 3.0}},
		{ID: "p-1.b", Parent: "p-1", Status: "open", Priority: 3, Metadata: map[string]any{"rank": 1.0}},
		{ID: "p-1.c", Parent: "p-1", Status: "open", Priority: 1, Metadata: map[string]any{"rank": 2.0}},
	}
	f := Build(issues, Synthesis{Project: "demo"})
	beads := f.Regions[0].Epics[0].Beads
	want := []string{"p-1.b", "p-1.c", "p-1.a", "p-1"} // rank 1,2,3,4 — not priority order
	for i, id := range want {
		if beads[i].ID != id {
			t.Errorf("bead[%d] = %s, want %s", i, beads[i].ID, id)
		}
	}
}

// Equal ranks tiebreak by id, deterministically.
func TestBuildRankTiebreaksByID(t *testing.T) {
	issues := []bd.Issue{
		{ID: "p-1", Title: "E", IssueType: "epic", Status: "open", Priority: 2, Metadata: map[string]any{"rank": 9.0}},
		{ID: "p-1.y", Parent: "p-1", Status: "open", Priority: 0, Metadata: map[string]any{"rank": 5.0}},
		{ID: "p-1.x", Parent: "p-1", Status: "open", Priority: 0, Metadata: map[string]any{"rank": 5.0}},
	}
	f := Build(issues, Synthesis{Project: "demo"})
	beads := f.Regions[0].Epics[0].Beads
	if beads[0].ID != "p-1.x" || beads[1].ID != "p-1.y" {
		t.Errorf("equal ranks should tiebreak by id: got %s, %s", beads[0].ID, beads[1].ID)
	}
}

// A bead created into an already-ranked epic carries no rank. Even when
// head-insert drags have minted negative ranks, the unranked newcomer sorts to
// the bottom — never mid-list on its zero default.
func TestBuildUnrankedBeadSortsLastInRankedGroup(t *testing.T) {
	issues := []bd.Issue{
		{ID: "p-1", Title: "E", IssueType: "epic", Status: "open", Priority: 2, Metadata: map[string]any{"rank": 1.0}},
		{ID: "p-1.head", Parent: "p-1", Status: "open", Priority: 0, Metadata: map[string]any{"rank": -2.0}},
		{ID: "p-1.new", Parent: "p-1", Status: "open", Priority: 0}, // no rank yet
	}
	f := Build(issues, Synthesis{Project: "demo"})
	beads := f.Regions[0].Epics[0].Beads
	want := []string{"p-1.head", "p-1", "p-1.new"} // ranked (-2, 1) then unranked
	for i, id := range want {
		if beads[i].ID != id {
			t.Errorf("bead[%d] = %s, want %s", i, beads[i].ID, id)
		}
	}
}

// TestBuildEmptyHasNoRegions: a workspace with no live work yields an empty
// forest, not a region with zero tiles.
func TestBuildEmptyHasNoRegions(t *testing.T) {
	f := Build([]bd.Issue{{ID: "x", Status: "closed"}}, Synthesis{Project: "demo"})
	if len(f.Regions) != 0 {
		t.Errorf("Regions = %d, want 0", len(f.Regions))
	}
}

// TestSquarifyAreaProportional: each tile's area tracks its weight's share, the
// rects tile the full 100×100 space, and output stays in input order.
func TestSquarifyAreaProportional(t *testing.T) {
	values := []float64{50, 30, 20}
	rects := squarify(values)
	if len(rects) != 3 {
		t.Fatalf("rects = %d, want 3", len(rects))
	}
	var total float64
	for i, r := range rects {
		area := r.W * r.H
		total += area
		wantFrac := values[i]                // values already sum to 100
		if math.Abs(area-wantFrac*100) > 5 { // area is in pct²; weight share ×100
			t.Errorf("tile %d area = %.1f, want ~%.1f", i, area, wantFrac*100)
		}
	}
	if math.Abs(total-10000) > 1 {
		t.Errorf("tiles cover %.1f%% , want 100%% of 100×100", total/100)
	}
}

// TestSquarifyEmptyAndZero: no values or all-zero weights return safe zero-area
// rects, never a panic or a divide-by-zero.
func TestSquarifyEmptyAndZero(t *testing.T) {
	if got := squarify(nil); len(got) != 0 {
		t.Errorf("squarify(nil) = %d rects, want 0", len(got))
	}
	rects := squarify([]float64{0, 0})
	if len(rects) != 2 {
		t.Fatalf("rects = %d, want 2", len(rects))
	}
	for i, r := range rects {
		if r.W != 0 || r.H != 0 {
			t.Errorf("zero-weight tile %d = %+v, want zero area", i, r)
		}
	}
}
