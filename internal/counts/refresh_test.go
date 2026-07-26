package counts

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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

// TestRefreshChangedSkipsUnchanged: in the default changed mode, a repo already
// cached with an unchanged last-touched mtime is not recomputed. A counting source
// proves the skip — its List is never called on the second run.
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
	if err := refresh(context.Background(), &cfg); err != nil { // second: unchanged mtime → skip
		t.Fatalf("refresh #2: %v", err)
	}
	if calls != 1 {
		t.Errorf("second run recomputed (calls=%d) — unchanged repo must be skipped", calls)
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
