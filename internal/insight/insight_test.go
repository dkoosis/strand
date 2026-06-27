package insight

import (
	"slices"
	"testing"
	"time"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/graph"
	"github.com/dkoosis/strand/internal/strand"
)

// --- V4 insights fixtures (carved from internal/server, strand-hh4) ---

// insightsNow is the fixed clock the insights tests pin Compute's `now` to, so the
// stale cutoff is deterministic regardless of when the suite runs.
var insightsNow = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

var (
	insFresh = insightsNow.Add(-time.Hour)
	insStale = insightsNow.Add(-30 * 24 * time.Hour)
)

// insightsIssues is the fixture for the dashboard: story demo-i with a 3-bead
// dependency chain (i.3→i.2→i.1, so i.1 is foundational), one in-progress bead, and
// one stale untagged bead. bd list omits closed, so no closed beads appear.
var insightsIssues = []bd.Issue{
	{ID: "demo-root", Title: "DEMO", IssueType: "epic", Status: "open"}, // top-level epic; demo-i is the story
	{ID: "demo-i", Parent: "demo-root", Title: "Insights story", IssueType: "epic", Status: "open", Priority: new(1), UpdatedAt: insFresh},
	{ID: "demo-i.1", Parent: "demo-i", Title: "Foundation", Status: "open", Priority: new(1), Labels: []string{"core"}, UpdatedAt: insFresh},
	{ID: "demo-i.2", Parent: "demo-i", Title: "Mid", Status: "open", Priority: new(2), Labels: []string{"core", "ui"}, UpdatedAt: insFresh},
	{ID: "demo-i.3", Parent: "demo-i", Title: "Leaf", Status: "open", Priority: new(2), Labels: []string{"ui"}, UpdatedAt: insFresh},
	{ID: "demo-i.4", Parent: "demo-i", Title: "Active", Status: "in_progress", Priority: new(2), Labels: []string{"core"}, UpdatedAt: insFresh},
	{ID: "demo-i.5", Parent: "demo-i", Title: "Stale", Status: "open", Priority: new(3), UpdatedAt: insStale},
}

var insightsDeps = []bd.DepEdge{
	{IssueID: "demo-i.2", DependsOnID: "demo-i.1", Type: "blocks"},
	{IssueID: "demo-i.3", DependsOnID: "demo-i.2", Type: "blocks"},
}

// insScope returns the demo-i story's actionable beads and the full-repo issue
// index, the two inputs the pure insight helpers take. It mirrors how the server
// narrows a scope before Compute: build the strand, pick the story, drop the epic
// container.
func insScope(t *testing.T) ([]strand.Bead, map[string]bd.Issue) {
	t.Helper()
	f := strand.Build(insightsIssues, strand.Synthesis{Project: "demo"})
	if len(f.Epics) == 0 {
		t.Fatal("fixture strand has no epics")
	}
	var beads []strand.Bead
	for _, e := range f.Epics[0].Stories {
		if e.ID == "demo-i" {
			beads = Actionable(e.Beads)
			break
		}
	}
	if beads == nil {
		t.Fatal("fixture story demo-i not found in strand")
	}
	return beads, indexIssues(insightsIssues)
}

// TestTriageCounts pins the queue-shape math: ready/blocked weigh all blockers,
// in-progress and stale are split out, and Total counts only live beads.
func TestTriageCounts(t *testing.T) {
	beads, idx := insScope(t)
	got := triage(beads, blockerCounts(insightsDeps, idx), idx, insightsNow)
	want := Counts{Total: 5, Open: 4, InProgress: 1, Ready: 2, Blocked: 2, Stale: 1}
	if got != want {
		t.Errorf("triage = %+v, want %+v", got, want)
	}
}

// TestTriageAbsentBlockerIsResolved: a blocks-dep whose target isn't in the live
// list (bd omits closed) must not keep the bead out of ready.
func TestTriageAbsentBlockerIsResolved(t *testing.T) {
	beads, idx := insScope(t)
	deps := append(append([]bd.DepEdge(nil), insightsDeps...),
		bd.DepEdge{IssueID: "demo-i.1", DependsOnID: "demo-gone", Type: "blocks"})
	got := triage(beads, blockerCounts(deps, idx), idx, insightsNow)
	if got.Ready != 2 || got.Blocked != 2 {
		t.Errorf("absent blocker changed triage: ready=%d blocked=%d, want 2/2", got.Ready, got.Blocked)
	}
}

// TestTriageExplicitlyBlocked: a bead bd reports with status "blocked" (not just
// dependency-blocked) lands in Blocked, not lost between the open/in-progress cases.
func TestTriageExplicitlyBlocked(t *testing.T) {
	beads := []strand.Bead{{ID: "b1", Status: bd.StatusBlocked}}
	idx := map[string]bd.Issue{"b1": {ID: "b1", Status: bd.StatusBlocked}}
	got := triage(beads, blockerCounts(nil, idx), idx, insightsNow)
	if got.Total != 1 || got.Blocked != 1 {
		t.Errorf("explicitly blocked bead: got %+v, want Total=1 Blocked=1", got)
	}
}

// TestIsStale: only live work past the cutoff is stale; a zero timestamp isn't.
func TestIsStale(t *testing.T) {
	cases := []struct {
		name    string
		status  bd.Status
		updated time.Time
		want    bool
	}{
		{"old open", "open", insStale, true},
		{"fresh open", "open", insFresh, false},
		{"old closed", "closed", insStale, false},
		{"zero time", "open", time.Time{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isStale(c.status, c.updated, insightsNow); got != c.want {
				t.Errorf("isStale = %v, want %v", got, c.want)
			}
		})
	}
}

// TestLeaderboard: ranks by score descending, caps the list, and sizes the leader's
// bar at 100%. The foundational bead (most depended-on) tops PageRank.
func TestLeaderboard(t *testing.T) {
	beads, _ := insScope(t)
	compEdges := []graph.Edge{
		{Dependent: "demo-i.2", Dependency: "demo-i.1"},
		{Dependent: "demo-i.3", Dependency: "demo-i.2"},
	}
	m := graph.Compute([]string{"demo-i.1", "demo-i.2", "demo-i.3", "demo-i.4", "demo-i.5"}, compEdges)
	board := leaderboard(beads, m.PageRank)
	if len(board) == 0 {
		t.Fatal("leaderboard empty; expected ranked beads")
	}
	if board[0].ID != "demo-i.1" {
		t.Errorf("top influence = %s, want demo-i.1 (foundational)", board[0].ID)
	}
	if board[0].Width != 100 {
		t.Errorf("leader bar = %d%%, want 100%%", board[0].Width)
	}
	for i := 1; i < len(board); i++ {
		if board[i-1].Score < board[i].Score {
			t.Errorf("leaderboard not descending at %d: %v < %v", i, board[i-1].Score, board[i].Score)
		}
	}
}

// TestLeaderboardEmptyWithoutEdges: an all-zero metric (no deps) yields no rows.
func TestLeaderboardEmptyWithoutEdges(t *testing.T) {
	beads, _ := insScope(t)
	if board := leaderboard(beads, map[string]float64{}); len(board) != 0 {
		t.Errorf("leaderboard over zero scores = %d rows, want 0", len(board))
	}
}

// TestReadyQueue: the dispatch queue lists only ready beads (open, no open blocker),
// ranked by influence (PageRank) descending, sized against the leader. In the fixture
// demo-i.1 (foundational) and demo-i.5 (stale, no deps) are ready; the chained i.2/i.3
// are blocked and the in-progress i.4 is not ready.
func TestReadyQueue(t *testing.T) {
	beads, idx := insScope(t)
	edges := []graph.Edge{
		{Dependent: "demo-i.2", Dependency: "demo-i.1"},
		{Dependent: "demo-i.3", Dependency: "demo-i.2"},
	}
	m := graph.Compute([]string{"demo-i.1", "demo-i.2", "demo-i.3", "demo-i.4", "demo-i.5"}, edges)
	frees, down := downstreamReach(edges, beads)
	q := readyQueue(beads, blockerCounts(insightsDeps, idx), idx, m.PageRank, frees, down, insightsNow)
	ids := make([]string, len(q))
	for i := range q {
		ids[i] = q[i].ID
	}
	if !slices.Contains(ids, "demo-i.1") || !slices.Contains(ids, "demo-i.5") {
		t.Fatalf("ready queue = %v, want demo-i.1 and demo-i.5", ids)
	}
	if slices.Contains(ids, "demo-i.2") || slices.Contains(ids, "demo-i.3") || slices.Contains(ids, "demo-i.4") {
		t.Errorf("ready queue leaked a non-ready bead: %v", ids)
	}
	if q[0].ID != "demo-i.1" {
		t.Errorf("ready queue top = %s, want demo-i.1 (most influence)", q[0].ID)
	}
	if q[0].Width != 100 {
		t.Errorf("ready leader bar = %d%%, want 100%%", q[0].Width)
	}
	// The stale ready bead carries the stale cross-flag.
	for _, b := range q {
		if b.ID == "demo-i.5" && !b.Stale {
			t.Error("ready bead demo-i.5 should carry the stale cross-flag")
		}
	}
}

// TestCrossFlag: a leaderboard row whose bead is also blocked/stale gets marked —
// the one act-now signal. demo-i.2 tops betweenness and is blocked by demo-i.1, so
// it carries the Blocked flag.
func TestCrossFlag(t *testing.T) {
	beads, idx := insScope(t)
	m := graph.Compute(
		[]string{"demo-i.1", "demo-i.2", "demo-i.3", "demo-i.4", "demo-i.5"},
		[]graph.Edge{
			{Dependent: "demo-i.2", Dependency: "demo-i.1"},
			{Dependent: "demo-i.3", Dependency: "demo-i.2"},
		})
	board := crossFlag(leaderboard(beads, m.Betweenness), blockerCounts(insightsDeps, idx), idx, insightsNow)
	var mid *RankedBead
	for i := range board {
		if board[i].ID == "demo-i.2" {
			mid = &board[i]
		}
	}
	if mid == nil {
		t.Fatal("demo-i.2 not in bottleneck board")
	}
	if !mid.Blocked {
		t.Error("demo-i.2 is dependency-blocked; want Blocked cross-flag set")
	}
}

// TestLabelHealth: counts labels over open beads (in-progress excluded), descending
// by count then name, and flags untagged open beads.
func TestLabelHealth(t *testing.T) {
	beads, idx := insScope(t)
	labels := labelHealth(beads, idx)
	want := []LabelCount{{Label: "core", Count: 2}, {Label: "ui", Count: 2}}
	if len(labels) != len(want) {
		t.Fatalf("labelHealth = %+v, want %+v", labels, want)
	}
	for i := range want {
		if labels[i] != want[i] {
			t.Errorf("label[%d] = %+v, want %+v", i, labels[i], want[i])
		}
	}
	if n := untaggedOpen(beads, idx); n != 1 {
		t.Errorf("untaggedOpen = %d, want 1 (demo-i.5)", n)
	}
}

// TestBeadPath resolves the critical-path ids to scope beads, dropping unknowns.
func TestBeadPath(t *testing.T) {
	beads, _ := insScope(t)
	path := beadPath([]string{"demo-i.3", "demo-i.2", "demo-i.1", "demo-gone"}, beadByID(beads))
	if len(path) != 3 {
		t.Fatalf("beadPath len = %d, want 3 (unknown dropped)", len(path))
	}
	if path[0].ID != "demo-i.3" || path[2].ID != "demo-i.1" {
		t.Errorf("beadPath order wrong: %s..%s", path[0].ID, path[2].ID)
	}
}

// TestComputeWiresTheDashboard drives the package's public seam end to end: the same
// scope and edges the server hands in must yield the full model — triage counts, the
// ready queue (with the stale cross-flag), the populated leaderboards (edges present),
// the critical path, and the label distribution. It pins that Compute composes the
// helpers above into the view-facing shape the insights template binds.
func TestComputeWiresTheDashboard(t *testing.T) {
	beads, _ := insScope(t)
	got := Compute(beads, insightsIssues, insightsDeps, insightsNow)

	if got.Counts != (Counts{Total: 5, Open: 4, InProgress: 1, Ready: 2, Blocked: 2, Stale: 1}) {
		t.Errorf("Compute counts = %+v", got.Counts)
	}
	// With blocks-edges in scope, both leaderboards rank; the foundational bead leads.
	if len(got.Influence) == 0 || got.Influence[0].ID != "demo-i.1" {
		t.Errorf("influence leader = %v, want demo-i.1 on top", got.Influence)
	}
	if len(got.Bottleneck) == 0 {
		t.Error("bottleneck board empty; edges present should rank it")
	}
	// The bottleneck leader demo-i.2 is dependency-blocked: cross-flag must be set.
	var midBlocked bool
	for _, r := range got.Bottleneck {
		if r.ID == "demo-i.2" {
			midBlocked = r.Blocked
		}
	}
	if !midBlocked {
		t.Error("Compute did not cross-flag the blocked bottleneck leader")
	}
	// Ready queue carries the foundational+stale ready beads; the stale one is flagged.
	readyIDs := make([]string, len(got.Ready))
	for i := range got.Ready {
		readyIDs[i] = got.Ready[i].ID
	}
	if !slices.Contains(readyIDs, "demo-i.1") || !slices.Contains(readyIDs, "demo-i.5") {
		t.Errorf("ready queue = %v, want demo-i.1 and demo-i.5", readyIDs)
	}
	// Critical path runs the full chain i.1→i.2→i.3 (order may be source→sink).
	if len(got.CritPath) != 3 {
		t.Errorf("critical path len = %d, want 3", len(got.CritPath))
	}
	if got.Untagged != 1 {
		t.Errorf("untagged = %d, want 1", got.Untagged)
	}
	if len(got.Labels) != 2 {
		t.Errorf("labels = %+v, want 2 rows", got.Labels)
	}
}

// TestComputeNoEdgesSkipsLeaderboards: with no blocks-edges every bead ties at the
// PageRank base, so a ranking would be noise — the leaderboards stay empty, but the
// ready queue still lists the dispatchable beads.
func TestComputeNoEdgesSkipsLeaderboards(t *testing.T) {
	beads, _ := insScope(t)
	got := Compute(beads, insightsIssues, nil, insightsNow)
	if len(got.Influence) != 0 || len(got.Bottleneck) != 0 {
		t.Errorf("no edges should leave leaderboards empty: infl=%d bot=%d", len(got.Influence), len(got.Bottleneck))
	}
	// Every open, unblocked bead is now ready (no edges → no blockers).
	if got.Counts.Ready == 0 {
		t.Error("no-edge scope should report ready beads")
	}
	if len(got.Ready) == 0 {
		t.Error("no-edge scope should still populate the dispatch queue")
	}
}

// --- str-xdy: "Waiting on you" lane fixtures + tests ---

// gateScope returns three open beads — a plain one, a decision bead (label
// "human"), and a review bead (metadata.review_needed=="true") — plus the issue
// index that carries the labels/metadata strand.Bead drops. It is the minimal
// fixture for the human-gate split: the queue helpers iterate the beads; the gate
// classification reads idx.
func gateScope() ([]strand.Bead, map[string]bd.Issue) {
	issues := []bd.Issue{
		{ID: "plain", Status: bd.StatusOpen, UpdatedAt: insFresh},
		{ID: "decision", Status: bd.StatusOpen, Labels: []string{"human"}, UpdatedAt: insFresh},
		{ID: "review", Status: bd.StatusOpen, Metadata: map[string]any{"review_needed": "true"}, UpdatedAt: insFresh},
	}
	beads := []strand.Bead{
		{ID: "plain", Status: bd.StatusOpen},
		{ID: "decision", Status: bd.StatusOpen},
		{ID: "review", Status: bd.StatusOpen},
	}
	return beads, indexIssues(issues)
}

func rankedIDs(rs []RankedBead) []string {
	out := make([]string, len(rs))
	for i := range rs {
		out[i] = rs[i].ID
	}
	return out
}

func beadIDs(bs []strand.Bead) []string {
	out := make([]string, len(bs))
	for i := range bs {
		out[i] = bs[i].ID
	}
	return out
}

// TestHumanGate pins the classifier: a "human" label is a decision, a string
// "true" (or bool true) review_needed is a review, and a bead carrying both is a
// decision (the stronger "needs a human call" signal) so the lane never double-counts.
func TestHumanGate(t *testing.T) {
	cases := []struct {
		name                     string
		iss                      bd.Issue
		wantDecision, wantReview bool
	}{
		{"plain", bd.Issue{ID: "a"}, false, false},
		{"label human", bd.Issue{Labels: []string{"core", "human"}}, true, false},
		{"review string true", bd.Issue{Metadata: map[string]any{"review_needed": "true"}}, false, true},
		{"review bool true", bd.Issue{Metadata: map[string]any{"review_needed": true}}, false, true},
		{"review false", bd.Issue{Metadata: map[string]any{"review_needed": "false"}}, false, false},
		{"both decision wins", bd.Issue{Labels: []string{"human"}, Metadata: map[string]any{"review_needed": "true"}}, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, r := humanGate(&c.iss)
			if d != c.wantDecision || r != c.wantReview {
				t.Errorf("humanGate = (%v,%v), want (%v,%v)", d, r, c.wantDecision, c.wantReview)
			}
		})
	}
}

// TestReadyQueueExcludesHumanGated: a human-gated bead (decision or review) is not
// genuinely claimable, so it must not appear in the dispatch queue; the plain bead does.
func TestReadyQueueExcludesHumanGated(t *testing.T) {
	beads, idx := gateScope()
	q := readyQueue(beads, blockerCounts(nil, idx), idx, map[string]float64{}, nil, nil, insightsNow)
	if got := rankedIDs(q); !slices.Equal(got, []string{"plain"}) {
		t.Errorf("ready queue = %v, want [plain] (human-gated excluded)", got)
	}
}

// TestWaitingLane: the lane surfaces the excluded beads, sub-grouped decision-vs-review.
func TestWaitingLane(t *testing.T) {
	beads, idx := gateScope()
	w := waitingLane(beads, nil, idx)
	if got := beadIDs(w.Decision); !slices.Equal(got, []string{"decision"}) {
		t.Errorf("waiting.Decision = %v, want [decision]", got)
	}
	if got := beadIDs(w.Review); !slices.Equal(got, []string{"review"}) {
		t.Errorf("waiting.Review = %v, want [review]", got)
	}
	if !w.Any() {
		t.Error("waitingLane.Any() = false, want true when beads are parked")
	}
}

// TestBlockedHumanGatedStaysBlocked: a bead that is BOTH blocked and human-gated is
// not yet waiting on the human — the dependency must clear first. Blockers are checked
// before the human-gate, so it counts as Blocked (not WaitingOnYou) and stays out of
// the waiting lane, matching readyQueue, which already excludes it as blocked (codex P2).
func TestBlockedHumanGatedStaysBlocked(t *testing.T) {
	beads := []strand.Bead{{ID: "bg", Status: bd.StatusOpen}}
	idx := map[string]bd.Issue{"bg": {ID: "bg", Status: bd.StatusOpen, Labels: []string{"human"}}}
	openBlockers := map[string]int{"bg": 1}

	if c := triage(beads, openBlockers, idx, insightsNow); c.Blocked != 1 || c.WaitingOnYou != 0 || c.Ready != 0 {
		t.Errorf("triage = %+v, want Blocked=1 WaitingOnYou=0 Ready=0", c)
	}
	if w := waitingLane(beads, openBlockers, idx); w.Any() {
		t.Errorf("waitingLane surfaced a blocked bead: %+v", w)
	}
}

// TestTriageDivertsToWaiting: an open, unblocked human-gated bead leaves the Ready
// count and lands in WaitingOnYou instead — the ready column means claimable work.
func TestTriageDivertsToWaiting(t *testing.T) {
	beads, idx := gateScope()
	got := triage(beads, blockerCounts(nil, idx), idx, insightsNow)
	if got.Ready != 1 || got.WaitingOnYou != 2 {
		t.Errorf("triage Ready=%d WaitingOnYou=%d, want 1/2", got.Ready, got.WaitingOnYou)
	}
}

// TestComputeSurfacesWaitingLane drives the public seam: human-gated beads are kept
// out of the ready queue and surfaced in the waiting lane, with the count to match.
func TestComputeSurfacesWaitingLane(t *testing.T) {
	beads, idx := gateScope()
	issues := make([]bd.Issue, 0, len(idx))
	for _, v := range idx {
		issues = append(issues, v)
	}
	got := Compute(beads, issues, nil, insightsNow)
	if ids := rankedIDs(got.Ready); slices.Contains(ids, "decision") || slices.Contains(ids, "review") {
		t.Errorf("Compute ready leaked a human-gated bead: %v", ids)
	}
	if len(got.WaitingOnYou.Decision) != 1 || len(got.WaitingOnYou.Review) != 1 {
		t.Errorf("Compute waiting lane = %+v, want 1 decision + 1 review", got.WaitingOnYou)
	}
	if got.Counts.WaitingOnYou != 2 {
		t.Errorf("Compute WaitingOnYou count = %d, want 2", got.Counts.WaitingOnYou)
	}
}

// TestActionableDropsEpics confirms the scope-narrowing the server does before Compute:
// epic containers are not actionable work and must not reach the dashboard math.
func TestActionableDropsEpics(t *testing.T) {
	in := []strand.Bead{
		{ID: "e", Type: "epic"},
		{ID: "t1"},
		{ID: "t2", Type: "task"},
	}
	out := Actionable(in)
	if len(out) != 2 {
		t.Fatalf("Actionable kept %d beads, want 2 (epic dropped)", len(out))
	}
	for _, b := range out {
		if b.Type == "epic" {
			t.Errorf("Actionable leaked an epic: %s", b.ID)
		}
	}
}
