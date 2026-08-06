package counts

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/bdcounts"
)

// mkRepo creates projects/<name>/.beads/last-touched so discover() finds it.
func mkRepo(t *testing.T, projects, name string) string {
	t.Helper()
	beads := filepath.Join(projects, name, ".beads")
	if err := os.MkdirAll(beads, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beads, "last-touched"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write last-touched: %v", err)
	}
	return filepath.Join(projects, name)
}

// oneOpenBead is a canned source with a single ready-open bead — bo=1, everything
// else zero — enough to prove a row was computed for a repo.
func oneOpenBead() source {
	return &fakeSource{issues: []bd.Issue{{ID: "x", Status: bd.StatusOpen}}}
}

// TestRefreshAllComputesEveryRepo: --all visits every discovered repo and writes a
// row per repo, readable back through bdcounts.Reader (the cross-package schema
// guard — producer and consumer agree on the wire, or this breaks).
func TestRefreshAllComputesEveryRepo(t *testing.T) {
	projects := t.TempDir()
	cache := t.TempDir()
	a := mkRepo(t, projects, "repo-a")
	b := mkRepo(t, projects, "repo-b")

	cfg := config{
		cacheDir:  cache,
		projects:  projects,
		mode:      modeAll,
		newSource: func(string) source { return oneOpenBead() },
	}
	if err := refresh(context.Background(), &cfg); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	r := bdcounts.NewReaderAt(filepath.Join(cache, "counts.json"))
	for _, root := range []string{a, b} {
		bk, ok := r.Lookup(root)
		if !ok {
			t.Fatalf("no row for %s", root)
		}
		if bk.Open != 1 {
			t.Errorf("%s: bo(open) = %d, want 1 — reader/writer schema drift", root, bk.Open)
		}
	}
}

// TestRefreshChangedSkipsUnchanged: in the default changed mode, a repo settles into
// being skipped once its mtime has been stable across a full derive-twice cycle (the
// st-3p8 guaranteed-follow-up: run 1 is cold/change-triggered so it re-arms pending for
// exactly one more look; run 2 is that guaranteed follow-up and clears pending; only
// run 3, with the mtime still unchanged and pending now clear, is actually skipped). A
// counting source proves the skip — its List is not called on the third run.
func TestRefreshChangedSkipsUnchanged(t *testing.T) {
	projects := t.TempDir()
	cache := t.TempDir()
	mkRepo(t, projects, "repo-a") // discovery finds it; the path isn't needed here

	var calls int
	countingNew := func(string) source {
		calls++
		return oneOpenBead()
	}
	cfg := config{cacheDir: cache, projects: projects, mode: modeChanged, newSource: countingNew}

	if err := refresh(context.Background(), &cfg); err != nil { // first: cold, computes
		t.Fatalf("refresh #1: %v", err)
	}
	if calls != 1 {
		t.Fatalf("first run computed %d repos, want 1", calls)
	}
	if err := refresh(context.Background(), &cfg); err != nil { // second: guaranteed follow-up, still computes
		t.Fatalf("refresh #2: %v", err)
	}
	if calls != 2 {
		t.Fatalf("second run (guaranteed follow-up) calls=%d, want 2", calls)
	}
	if err := refresh(context.Background(), &cfg); err != nil { // third: settled → skip
		t.Fatalf("refresh #3: %v", err)
	}
	if calls != 2 {
		t.Errorf("third run recomputed (calls=%d) — a settled, non-pending repo must be skipped", calls)
	}
}

// TestRefreshLastGoodOnReadFailure: a repo whose bd reads fail keeps its previous
// row rather than being zeroed or dropped.
func TestRefreshLastGoodOnReadFailure(t *testing.T) {
	projects := t.TempDir()
	cache := t.TempDir()
	root := mkRepo(t, projects, "repo-a")

	good := config{cacheDir: cache, projects: projects, mode: modeAll,
		newSource: func(string) source { return oneOpenBead() }}
	if err := refresh(context.Background(), &good); err != nil {
		t.Fatalf("seed refresh: %v", err)
	}

	// Now the source errors; --all still visits, but the row must survive.
	broken := config{cacheDir: cache, projects: projects, mode: modeAll,
		newSource: func(string) source { return &fakeSource{err: os.ErrPermission} }}
	if err := refresh(context.Background(), &broken); err != nil {
		t.Fatalf("broken refresh: %v", err)
	}

	bk, ok := bdcounts.NewReaderAt(filepath.Join(cache, "counts.json")).Lookup(root)
	if !ok {
		t.Fatal("row dropped on read failure — last-good not preserved")
	}
	if bk.Open != 1 {
		t.Errorf("row zeroed on read failure: bo=%d, want the prior 1", bk.Open)
	}
}

// callCountingSource returns a caller-supplied row per call index (clamped to the last
// row once calls exceed len(rows)) — lets a test prove which of several sequential
// refresh() invocations actually recomputed vs skipped.
type callCountingSource struct {
	calls *int
	rows  []bd.Issue // one issue slice per call; List returns rows[min(*calls, len-1)]
}

func (s *callCountingSource) List(context.Context, bd.ListOpts) ([]bd.Issue, error) {
	i := *s.calls
	if i >= len(s.rows) {
		i = len(s.rows) - 1
	}
	*s.calls++
	return []bd.Issue{s.rows[i]}, nil
}
func (s *callCountingSource) Deps(context.Context, ...string) ([]bd.DepEdge, error) {
	return nil, nil
}
func (s *callCountingSource) Stats(context.Context) (bd.Stats, error) { return bd.Stats{}, nil }
func (s *callCountingSource) EpicStatus(context.Context) ([]bd.EpicStatus, error) {
	return nil, nil
}

// TestRefreshConvergesAfterTornRead is the st-3p8 repro: a bd write touches
// last-touched, but the strand-counts derive that fires on that same watch-cycle reads
// bd mid-commit and computes a stale row (here: an open bead, bo=1 — standing in for
// the pre-close snapshot). Without the pending-retry fix, that stale row would latch
// forever, because the recorded mtime never changes again after the single touch.
//
// The fix: a change-triggered derive schedules exactly one guaranteed follow-up derive
// next cycle. This test drives three `refresh()` calls against ONE repo whose
// last-touched mtime is set once (simulating the single `bd close` touch) and never
// touched again:
//
//  1. cold run — mtime "changed" (no prior state) → derives, gets the stale row (call
//     0 of the fake), and must schedule a pending re-check.
//  2. same mtime, no further touch — must STILL recompute (the scheduled pending
//     re-check), gets call 1's row, and must clear pending afterward.
//  3. same mtime again — must NOT recompute a third time (pending cleared, mtime
//     unchanged) — proving the retry is bounded, not a permanent hot-loop.
func TestRefreshConvergesAfterTornRead(t *testing.T) {
	projects := t.TempDir()
	cache := t.TempDir()
	mkRepo(t, projects, "repo-a")

	calls := 0
	src := &callCountingSource{
		calls: &calls,
		rows: []bd.Issue{
			{ID: "stale", Status: bd.StatusOpen},   // call 0: torn-read snapshot
			{ID: "settled", Status: bd.StatusOpen}, // call 1: settled snapshot
		},
	}
	cfg := config{cacheDir: cache, projects: projects, mode: modeChanged,
		newSource: func(string) source { return src }}

	if err := refresh(context.Background(), &cfg); err != nil { // run 1: change-triggered
		t.Fatalf("refresh #1: %v", err)
	}
	if calls != 1 {
		t.Fatalf("run 1: calls=%d, want 1 (change-triggered derive)", calls)
	}

	if err := refresh(context.Background(), &cfg); err != nil { // run 2: pending-triggered
		t.Fatalf("refresh #2: %v", err)
	}
	if calls != 2 {
		t.Fatalf("run 2: calls=%d, want 2 — the guaranteed follow-up derive must fire even though mtime did not change", calls)
	}

	if err := refresh(context.Background(), &cfg); err != nil { // run 3: must be quiet
		t.Fatalf("refresh #3: %v", err)
	}
	if calls != 2 {
		t.Errorf("run 3: calls=%d, want 2 — pending must have cleared after run 2's successful derive, no permanent hot-loop", calls)
	}
}

// TestRefreshPendingClearedOnAllMode: modeAll always derives unconditionally; a repo
// left with a stale pending flag from a prior modeChanged run must have it cleared by
// a successful --all visit, so a later modeChanged run doesn't force a needless extra
// recompute.
func TestRefreshPendingClearedOnAllMode(t *testing.T) {
	projects := t.TempDir()
	cache := t.TempDir()
	mkRepo(t, projects, "repo-a")

	calls := 0
	src := &callCountingSource{calls: &calls, rows: []bd.Issue{{ID: "a", Status: bd.StatusOpen}}}
	changedCfg := config{cacheDir: cache, projects: projects, mode: modeChanged,
		newSource: func(string) source { return src }}
	if err := refresh(context.Background(), &changedCfg); err != nil { // arms pending
		t.Fatalf("seed refresh: %v", err)
	}
	if calls != 1 {
		t.Fatalf("seed: calls=%d, want 1", calls)
	}

	allCfg := changedCfg
	allCfg.mode = modeAll
	if err := refresh(context.Background(), &allCfg); err != nil { // clears pending
		t.Fatalf("all refresh: %v", err)
	}
	if calls != 2 {
		t.Fatalf("--all run: calls=%d, want 2", calls)
	}

	if err := refresh(context.Background(), &changedCfg); err != nil { // must be quiet now
		t.Fatalf("final changed refresh: %v", err)
	}
	if calls != 2 {
		t.Errorf("final: calls=%d, want 2 — --all must have cleared the pending flag", calls)
	}
}

// TestRefreshChangeTriggeredFailureReArmsPending covers the one nextPending branch the
// other new tests don't reach directly: modeChanged, mtimeChanged=true, err!=nil. A
// change-triggered derive that itself fails must still re-arm pending (not just record
// failure and go quiet) — otherwise a failed torn-read-window derive would never get
// its guaranteed follow-up, reintroducing the exact latch st-3p8 fixes. The repo's
// mtime never changes again after the initial touch, so only the pending re-arm can
// trigger a second attempt.
func TestRefreshChangeTriggeredFailureReArmsPending(t *testing.T) {
	projects := t.TempDir()
	cache := t.TempDir()
	mkRepo(t, projects, "repo-a")

	var calls int
	failingNew := func(string) source {
		calls++
		return &fakeSource{err: os.ErrPermission}
	}
	cfg := config{cacheDir: cache, projects: projects, mode: modeChanged, newSource: failingNew}

	if err := refresh(context.Background(), &cfg); err != nil { // run 1: change-triggered, fails
		t.Fatalf("refresh #1: %v", err)
	}
	if calls != 1 {
		t.Fatalf("run 1: calls=%d, want 1", calls)
	}

	if err := refresh(context.Background(), &cfg); err != nil { // run 2: mtime unchanged, but must retry — pending was re-armed despite the failure
		t.Fatalf("refresh #2: %v", err)
	}
	if calls != 2 {
		t.Errorf("run 2: calls=%d, want 2 — a change-triggered derive that failed must still re-arm pending for a retry", calls)
	}
}

// TestRefreshExplicitPreservesOtherReposGateState is the st-dd9 repro: an explicit-mode
// run for one repo must not truncate the mtime-gate state of the repos it did not visit.
// Two repos are first settled (a full --all derive records gate state for both). An
// explicit `strand counts <repo-a>` then visits only repo-a. If writeState replaced the
// state file wholesale, repo-b's gate entry would vanish, so the next changed-mode run
// would find no prior mtime for repo-b and cold-recompute it — the ~60s thrash this fix
// removes. The final changed run must recompute NOTHING: both repos stay settled.
func TestRefreshExplicitPreservesOtherReposGateState(t *testing.T) {
	projects := t.TempDir()
	cache := t.TempDir()
	a := mkRepo(t, projects, "repo-a")
	mkRepo(t, projects, "repo-b")

	// Seed: --all derives and settles both repos (modeAll records gate state, pending clear).
	seed := config{cacheDir: cache, projects: projects, mode: modeAll,
		newSource: func(string) source { return oneOpenBead() }}
	if err := refresh(context.Background(), &seed); err != nil {
		t.Fatalf("seed refresh: %v", err)
	}

	// Explicit run for repo-a only — must not drop repo-b's gate entry.
	explicit := config{cacheDir: cache, projects: projects, mode: modeExplicit,
		targets: []string{a}, newSource: func(string) source { return oneOpenBead() }}
	if err := refresh(context.Background(), &explicit); err != nil {
		t.Fatalf("explicit refresh: %v", err)
	}

	// Final changed-mode run: with repo-b's gate state preserved, both repos are settled
	// and unchanged, so nothing recomputes. Truncation would force repo-b's recompute.
	var calls int
	final := config{cacheDir: cache, projects: projects, mode: modeChanged,
		newSource: func(string) source { calls++; return oneOpenBead() }}
	if err := refresh(context.Background(), &final); err != nil {
		t.Fatalf("final refresh: %v", err)
	}
	if calls != 0 {
		t.Errorf("final changed run recomputed %d repo(s), want 0 — explicit run truncated another repo's gate state", calls)
	}
}

// TestRefreshExplicitDirs: named roots are visited directly, no discovery scan.
func TestRefreshExplicitDirs(t *testing.T) {
	projects := t.TempDir() // deliberately empty — explicit mode must ignore discovery
	cache := t.TempDir()
	root := mkRepo(t, t.TempDir(), "somewhere-else")

	cfg := config{cacheDir: cache, projects: projects, mode: modeExplicit,
		targets: []string{root}, newSource: func(string) source { return oneOpenBead() }}
	if err := refresh(context.Background(), &cfg); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, ok := bdcounts.NewReaderAt(filepath.Join(cache, "counts.json")).Lookup(root); !ok {
		t.Errorf("explicit dir %s not in output", root)
	}
}

// setStoreMTime builds the embedded-Dolt manifest layout bd.StoreMTime globs for under
// root and stamps it with mt, so the change key sees a store the way an out-of-band
// write (a Dolt pull/sync, a direct commit) would leave one. Returns the manifest path.
func setStoreMTime(t *testing.T, root string, mt time.Time) string {
	t.Helper()
	manifest := filepath.Join(root, ".beads", "embeddeddolt", "demo", ".dolt", "noms", "manifest")
	if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
		t.Fatalf("mkdir store: %v", err)
	}
	if err := os.WriteFile(manifest, []byte("root-chunk"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.Chtimes(manifest, mt, mt); err != nil {
		t.Fatalf("chtimes manifest: %v", err)
	}
	return manifest
}

// TestRefreshOutOfBandStoreChangeRefreshes is the st-nm5 guard: the refresh gate is the
// Dolt store mtime (bd.StoreMTime), not .beads/last-touched. An out-of-band write — a bd
// dolt pull/sync, a direct Dolt commit, a bd import — moves the store manifest WITHOUT
// bumping last-touched. Before st-nm5 that repo latched skipped (last-touched unchanged →
// counts.json stalled while the board stayed fresh). Here we settle a repo, advance ONLY
// the manifest mtime, and assert the next changed-mode run recomputes it.
func TestRefreshOutOfBandStoreChangeRefreshes(t *testing.T) {
	projects := t.TempDir()
	cache := t.TempDir()
	root := mkRepo(t, projects, "repo-a")
	setStoreMTime(t, root, time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC))

	lastTouchedPath := filepath.Join(root, ".beads", "last-touched")
	fi, err := os.Stat(lastTouchedPath)
	if err != nil {
		t.Fatalf("stat last-touched: %v", err)
	}
	ltBefore := fi.ModTime()

	var calls int
	countingNew := func(string) source {
		calls++
		return oneOpenBead()
	}
	cfg := config{cacheDir: cache, projects: projects, mode: modeChanged, newSource: countingNew}

	// Runs 1 (cold) + 2 (st-3p8 guaranteed follow-up) settle the repo: after this a
	// third unchanged run would be skipped (see TestRefreshChangedSkipsUnchanged).
	if err := refresh(context.Background(), &cfg); err != nil {
		t.Fatalf("refresh #1: %v", err)
	}
	if err := refresh(context.Background(), &cfg); err != nil {
		t.Fatalf("refresh #2: %v", err)
	}
	if calls != 2 {
		t.Fatalf("after settle calls=%d, want 2", calls)
	}

	// Out-of-band write: advance ONLY the store manifest; last-touched must not move.
	setStoreMTime(t, root, time.Date(2026, 7, 25, 13, 0, 0, 0, time.UTC))

	if err := refresh(context.Background(), &cfg); err != nil {
		t.Fatalf("refresh #3: %v", err)
	}
	if calls != 3 {
		t.Errorf("out-of-band store change was skipped (calls=%d, want 3) — the gate is not on the Dolt store mtime", calls)
	}

	if fi, err := os.Stat(lastTouchedPath); err != nil {
		t.Fatalf("re-stat last-touched: %v", err)
	} else if !fi.ModTime().Equal(ltBefore) {
		t.Fatalf("last-touched mtime moved (%v → %v) — the test no longer isolates the store signal", ltBefore, fi.ModTime())
	}
}
