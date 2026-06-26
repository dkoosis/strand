package strand

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/jtbd"
)

// TestBuildEpicsFromTopLevel pins the core synthesis: top-level bd epics are the
// strand's epics, the issues beneath them are stories, deeper work rolls into its
// story, closed/deferred drop out, and work off any epic lands in the catch-all.
func TestBuildEpicsFromTopLevel(t *testing.T) {
	issues := []bd.Issue{
		{ID: "t-sub", Title: "SUBSTRATE — code health", IssueType: "epic", Status: "open"}, // epic
		{ID: "e-1", Title: "Epic one", IssueType: "epic", Parent: "t-sub", Status: "open"}, // story
		{ID: "e-1.a", Parent: "e-1", Status: "open", Priority: new(2)},
		{ID: "e-1.b", Parent: "e-1", IssueType: "bug", Status: "in_progress", Priority: new(0)},
		{ID: "e-1.c", Parent: "e-1", Status: "closed", Priority: new(2)},                                              // excluded
		{ID: "e-2", Title: "Lone feature epic", IssueType: "epic", Parent: "t-sub", Status: "open", Priority: new(3)}, // story, 1 open
		{ID: "loose-1", Title: "Standalone", IssueType: "task", Status: "open", Priority: new(2)},                     // catch-all
		{ID: "t-done", Title: "Done epic", IssueType: "epic", Status: "closed"},                                       // no live work
	}
	f := Build(issues, Synthesis{Project: "demo", NorthStar: "north"})

	if f.NorthStar != "north" {
		t.Errorf("NorthStar = %q, want %q", f.NorthStar, "north")
	}
	// SUBSTRATE (4 open) and the catch-all (1 open); t-done has no live work.
	if len(f.Epics) != 2 {
		t.Fatalf("Epics = %d, want 2", len(f.Epics))
	}
	// Largest epic first.
	sub := f.Epics[0]
	if sub.Name != "SUBSTRATE" || sub.Open != 4 {
		t.Errorf("epic[0] = %q/%d, want SUBSTRATE/4", sub.Name, sub.Open)
	}
	if got := len(sub.Stories); got != 2 {
		t.Fatalf("SUBSTRATE stories = %d, want 2", got)
	}
	// Largest story first: e-1 (e-1 + a + b = 3 open), e-2 (1 open).
	st := sub.Stories
	if st[0].ID != "e-1" || st[0].Open != 3 {
		t.Errorf("story[0] = %s/%d, want e-1/3", st[0].ID, st[0].Open)
	}
	if st[1].ID != "e-2" || st[1].Open != 1 {
		t.Errorf("story[1] = %s/%d, want e-2/1", st[1].ID, st[1].Open)
	}
	if !st[0].Flag {
		t.Error("story e-1 holds a bug bead, want Flag=true")
	}
	if st[1].Flag {
		t.Error("story e-2 has no bug, want Flag=false")
	}
	loose := f.Epics[1]
	if loose.Name != "No epic" || loose.Open != 1 {
		t.Errorf("epic[1] = %q/%d, want No epic/1", loose.Name, loose.Open)
	}
	if loose.Stories[0].ID != "loose-1" {
		t.Errorf("catch-all story = %s, want loose-1", loose.Stories[0].ID)
	}
	if f.Open != 5 {
		t.Errorf("Open = %d, want 5", f.Open)
	}
	if f.InProgress != 1 {
		t.Errorf("InProgress = %d, want 1", f.InProgress)
	}
}

// TestEpicBeadID pins the editable-scope id: a real epic exposes its bd id so the
// pane-head title can open its drawer, while the off-epic catch-all has no bead
// behind it and reports "" — its title stays inert (str-scn).
func TestEpicBeadID(t *testing.T) {
	issues := []bd.Issue{
		{ID: "t-sub", Title: "SUBSTRATE — code health", IssueType: "epic", Status: "open"},
		{ID: "e-1", Parent: "t-sub", Status: "open", Priority: new(2)},
		{ID: "loose-1", Title: "Standalone", IssueType: "task", Status: "open", Priority: new(2)},
	}
	f := Build(issues, Synthesis{Project: "demo"})
	seenReal, seenLoose := false, false
	for _, e := range f.Epics {
		switch e.Key {
		case "t-sub":
			seenReal = true
			if e.BeadID() != "t-sub" {
				t.Errorf("real epic BeadID = %q, want t-sub", e.BeadID())
			}
		case looseKey:
			seenLoose = true
			if e.BeadID() != "" {
				t.Errorf("catch-all BeadID = %q, want empty", e.BeadID())
			}
		}
	}
	if !seenReal || !seenLoose {
		t.Fatalf("expected both a real epic and the catch-all; got %d epics", len(f.Epics))
	}
}

// TestBuildNoEpicNamesForProject: a workspace whose live work hangs off no
// top-level epic is one catch-all epic, named for the project rather than "Off-epic".
func TestBuildNoEpicNamesForProject(t *testing.T) {
	issues := []bd.Issue{
		{ID: "a", Title: "Task a", IssueType: "task", Status: "open"},
		{ID: "b", Title: "Task b", IssueType: "task", Status: "open"},
	}
	f := Build(issues, Synthesis{Project: "demo"})
	if len(f.Epics) != 1 {
		t.Fatalf("Epics = %d, want 1", len(f.Epics))
	}
	if f.Epics[0].Name != "demo" {
		t.Errorf("epic Name = %q, want demo", f.Epics[0].Name)
	}
}

// TestBuildBeadsSortPriorityThenID pins the in-story bead order the list renders.
func TestBuildBeadsSortPriorityThenID(t *testing.T) {
	issues := []bd.Issue{
		{ID: "t", IssueType: "epic", Status: "open"}, // top-level epic; p-1 is the story
		{ID: "p-1", Title: "E", IssueType: "epic", Parent: "t", Status: "open", Priority: new(2)},
		{ID: "p-1.hi", Parent: "p-1", Status: "open", Priority: new(0)},
		{ID: "p-1.lo", Parent: "p-1", Status: "open", Priority: new(3)},
	}
	f := Build(issues, Synthesis{Project: "demo"})
	beads := f.Epics[0].Stories[0].Beads
	want := []string{"p-1.hi", "p-1", "p-1.lo"} // P0, P2, P3
	for i, id := range want {
		if beads[i].ID != id {
			t.Errorf("bead[%d] = %s, want %s", i, beads[i].ID, id)
		}
	}
}

// A manually-ranked story orders by rank (ascending), overriding priority. The
// rank lives in bd metadata; a P3 ranked ahead of a P0 leads the list.
func TestBuildRankedStorySortsByRank(t *testing.T) {
	// The seed invariant ranks every member, including the story root (a member of
	// its own list, P2 here), so the group is wholly rank-ordered.
	issues := []bd.Issue{
		{ID: "t", IssueType: "epic", Status: "open"}, // top-level epic; p-1 is the story
		{ID: "p-1", Title: "E", IssueType: "epic", Parent: "t", Status: "open", Priority: new(2), Metadata: map[string]any{"rank": 4.0}},
		{ID: "p-1.a", Parent: "p-1", Status: "open", Priority: new(0), Metadata: map[string]any{"rank": 3.0}},
		{ID: "p-1.b", Parent: "p-1", Status: "open", Priority: new(3), Metadata: map[string]any{"rank": 1.0}},
		{ID: "p-1.c", Parent: "p-1", Status: "open", Priority: new(1), Metadata: map[string]any{"rank": 2.0}},
	}
	f := Build(issues, Synthesis{Project: "demo"})
	beads := f.Epics[0].Stories[0].Beads
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
		{ID: "t", IssueType: "epic", Status: "open"}, // top-level epic; p-1 is the story
		{ID: "p-1", Title: "E", IssueType: "epic", Parent: "t", Status: "open", Priority: new(2), Metadata: map[string]any{"rank": 9.0}},
		{ID: "p-1.y", Parent: "p-1", Status: "open", Priority: new(0), Metadata: map[string]any{"rank": 5.0}},
		{ID: "p-1.x", Parent: "p-1", Status: "open", Priority: new(0), Metadata: map[string]any{"rank": 5.0}},
	}
	f := Build(issues, Synthesis{Project: "demo"})
	beads := f.Epics[0].Stories[0].Beads
	if beads[0].ID != "p-1.x" || beads[1].ID != "p-1.y" {
		t.Errorf("equal ranks should tiebreak by id: got %s, %s", beads[0].ID, beads[1].ID)
	}
}

// A bead created into an already-ranked story carries no rank. Even when
// head-insert drags have minted negative ranks, the unranked newcomer sorts to
// the bottom — never mid-list on its zero default.
func TestBuildUnrankedBeadSortsLastInRankedGroup(t *testing.T) {
	issues := []bd.Issue{
		{ID: "t", IssueType: "epic", Status: "open"}, // top-level epic; p-1 is the story
		{ID: "p-1", Title: "E", IssueType: "epic", Parent: "t", Status: "open", Priority: new(2), Metadata: map[string]any{"rank": 1.0}},
		{ID: "p-1.head", Parent: "p-1", Status: "open", Priority: new(0), Metadata: map[string]any{"rank": -2.0}},
		{ID: "p-1.new", Parent: "p-1", Status: "open", Priority: new(0)}, // no rank yet
	}
	f := Build(issues, Synthesis{Project: "demo"})
	beads := f.Epics[0].Stories[0].Beads
	want := []string{"p-1.head", "p-1", "p-1.new"} // ranked (-2, 1) then unranked
	for i, id := range want {
		if beads[i].ID != id {
			t.Errorf("bead[%d] = %s, want %s", i, beads[i].ID, id)
		}
	}
}

// TestBuildEmptyHasNoEpics: a workspace with no live work yields an empty strand,
// not an epic with zero stories.
func TestBuildEmptyHasNoEpics(t *testing.T) {
	f := Build([]bd.Issue{{ID: "x", Status: "closed"}}, Synthesis{Project: "demo"})
	if len(f.Epics) != 0 {
		t.Errorf("Epics = %d, want 0", len(f.Epics))
	}
}

// TestSquarifyAreaProportional: each cell's area tracks its weight's share, the
// rects cover the full 100×100 space, and output stays in input order.
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
			t.Errorf("cell %d area = %.1f, want ~%.1f", i, area, wantFrac*100)
		}
	}
	if math.Abs(total-10000) > 1 {
		t.Errorf("cells cover %.1f%% , want 100%% of 100×100", total/100)
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
			t.Errorf("zero-weight cell %d = %+v, want zero area", i, r)
		}
	}
}

func TestResolveJTBD(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "| id | job |\n|----|-----|\n| j-001 | Triage what to work on next |\n"
	if err := os.WriteFile(filepath.Join(dir, "docs", "jtbd.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := jtbd.Load(dir)

	// Cited id with a registry row → the job title resolves, id retained.
	resolved := NewBead(&bd.Issue{ID: "a", Description: "why\nJTBD: j-001\n"})
	resolved.ResolveJTBD("why\nJTBD: j-001\n", reg)
	if resolved.JTBDID != "j-001" || resolved.JTBDJob != "Triage what to work on next" {
		t.Errorf("resolved = (%q, %q), want (j-001, Triage what to work on next)", resolved.JTBDID, resolved.JTBDJob)
	}

	// Cited id with no row → id retained, job empty (the unresolved state).
	unres := NewBead(&bd.Issue{ID: "b"})
	unres.ResolveJTBD("JTBD: j-999", reg)
	if unres.JTBDID != "j-999" || unres.JTBDJob != "" {
		t.Errorf("unresolved = (%q, %q), want (j-999, \"\")", unres.JTBDID, unres.JTBDJob)
	}

	// No citation → both fields empty, bead unchanged.
	none := NewBead(&bd.Issue{ID: "c"})
	none.ResolveJTBD("no job here", reg)
	if none.JTBDID != "" || none.JTBDJob != "" {
		t.Errorf("no-citation = (%q, %q), want empty", none.JTBDID, none.JTBDJob)
	}
}

func TestNewBeadAbsentPriorityDefaultsToP2(t *testing.T) {
	// Absent priority (nil) must NOT render as P0 — it defaults to P2 so it
	// does not sort to the top of priority-asc.
	got := NewBead(&bd.Issue{ID: "a", Priority: nil})
	if got.Priority != 2 {
		t.Errorf("absent priority -> %d, want 2 (P2 default)", got.Priority)
	}

	// A present P0 still renders as 0 — the default must not swallow a real P0.
	zero := 0
	gotZero := NewBead(&bd.Issue{ID: "b", Priority: &zero})
	if gotZero.Priority != 0 {
		t.Errorf("present P0 -> %d, want 0", gotZero.Priority)
	}

	// A present non-zero round-trips.
	three := 3
	gotThree := NewBead(&bd.Issue{ID: "c", Priority: &three})
	if gotThree.Priority != 3 {
		t.Errorf("present P3 -> %d, want 3", gotThree.Priority)
	}
}
