package registry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mkRepo creates dir/.beads under root so discovery and Add see a workspace.
func mkRepo(t *testing.T, root, name string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(path, ".beads"), 0o755); err != nil {
		t.Fatalf("mkRepo %s: %v", name, err)
	}
	return path
}

// TestDiscoverFindsWorkspaces: a *.beads scan of the root surfaces every child
// repo and ignores plain directories without a workspace.
func TestDiscoverFindsWorkspaces(t *testing.T) {
	root := t.TempDir()
	mkRepo(t, root, "alpha")
	mkRepo(t, root, "beta")
	if err := os.MkdirAll(filepath.Join(root, "plain"), 0o755); err != nil {
		t.Fatal(err)
	}

	found := discover(root)
	if len(found) != 2 {
		t.Fatalf("discover found %d repos, want 2: %+v", len(found), found)
	}
	names := map[string]bool{found[0].Name: true, found[1].Name: true}
	if !names["alpha"] || !names["beta"] {
		t.Errorf("discover missed a repo: %+v", found)
	}
}

// TestMRUDefaultActive: the most-recently-used repo is active by default and
// leads the selector order.
func TestMRUDefaultActive(t *testing.T) {
	now := time.Now()
	reg := InMemory(
		Repo{Name: "old", Path: "/old", LastUsed: now.Add(-time.Hour)},
		Repo{Name: "fresh", Path: "/fresh", LastUsed: now},
	)
	active, ok := reg.Active()
	if !ok || active.Path != "/fresh" {
		t.Fatalf("active = %+v ok=%v, want /fresh", active, ok)
	}
	if reg.Repos()[0].Path != "/fresh" {
		t.Errorf("MRU repo not first: %+v", reg.Repos())
	}
}

// TestNoReposNoActive: an empty registry has no active repo — the signal the UI
// turns into its empty state.
func TestNoReposNoActive(t *testing.T) {
	if _, ok := InMemory().Active(); ok {
		t.Error("empty registry reports an active repo")
	}
}

// TestSwitchReScopesAndPersists: switching makes the picked repo active, stamps
// it most-recent, and writes through to disk so the choice survives a reload.
func TestSwitchReScopesAndPersists(t *testing.T) {
	root := t.TempDir()
	a := mkRepo(t, root, "alpha")
	b := mkRepo(t, root, "beta")
	file := filepath.Join(t.TempDir(), "repos.json")

	reg, err := Open(file, root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := reg.Switch(b); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if active, _ := reg.Active(); active.Path != b {
		t.Fatalf("active = %s, want %s", active.Path, b)
	}

	// Reload from the same file: the switch persisted, beta stays MRU/active.
	reloaded, err := Open(file, root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if active, ok := reloaded.Active(); !ok || active.Path != b {
		t.Errorf("reloaded active = %+v, want %s", active, b)
	}
	_ = a
}

// TestSwitchUnknownErrors: switching to an unregistered path is a typed error, not
// a silent scope-to-nothing.
func TestSwitchUnknownErrors(t *testing.T) {
	reg := InMemory(Repo{Name: "alpha", Path: "/alpha", LastUsed: time.Now()})
	if _, err := reg.Switch("/nope"); !errors.Is(err, ErrUnknownRepo) {
		t.Errorf("switch unknown = %v, want ErrUnknownRepo", err)
	}
}

// TestAddRequiresBeads: a path without a .beads workspace is rejected; one with a
// workspace registers and becomes active.
func TestAddRequiresBeads(t *testing.T) {
	root := t.TempDir()
	good := mkRepo(t, root, "good")
	bare := filepath.Join(root, "bare")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}

	reg := InMemory()
	if _, err := reg.Add(bare); !errors.Is(err, ErrNoBeads) {
		t.Errorf("add bare dir = %v, want ErrNoBeads", err)
	}
	repo, err := reg.Add(good)
	if err != nil {
		t.Fatalf("add good: %v", err)
	}
	if repo.Name != "good" {
		t.Errorf("added repo name = %q, want good", repo.Name)
	}
	if active, ok := reg.Active(); !ok || active.Path != good {
		t.Errorf("added repo not active: %+v ok=%v", active, ok)
	}
}

// TestResolve pins st-ga4: a per-request `?repo=` param resolves without any
// registry mutation — a known path returns its entry, an unknown-but-valid
// path (has .beads) returns an ephemeral unregistered Repo, an unresolvable
// path reports ok=false, and an empty param defers to Active(). None of these
// touch r.repos or r.active or trigger a disk write.
func TestResolve(t *testing.T) {
	root := t.TempDir()
	known := mkRepo(t, root, "known")
	unregistered := mkRepo(t, root, "unregistered")
	bare := filepath.Join(root, "bare")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}

	reg := InMemory(Repo{Name: "known", Path: known, LastUsed: time.Now()})

	t.Run("empty param defers to Active", func(t *testing.T) {
		repo, ok := reg.Resolve("")
		if !ok || repo.Path != known {
			t.Errorf("Resolve(\"\") = %+v, %v; want the active repo", repo, ok)
		}
	})

	t.Run("known path returns its registered entry", func(t *testing.T) {
		repo, ok := reg.Resolve(known)
		if !ok || repo.Name != "known" {
			t.Errorf("Resolve(known) = %+v, %v; want the registered entry", repo, ok)
		}
	})

	t.Run("unregistered but valid path resolves ephemerally", func(t *testing.T) {
		repo, ok := reg.Resolve(unregistered)
		if !ok || repo.Path != unregistered {
			t.Errorf("Resolve(unregistered) = %+v, %v; want an ephemeral match", repo, ok)
		}
		if len(reg.Repos()) != 1 {
			t.Errorf("Resolve must not register: known repos = %v", reg.Repos())
		}
		if active, ok := reg.Active(); !ok || active.Path != known {
			t.Errorf("Resolve must not re-point active: got %+v, %v", active, ok)
		}
	})

	t.Run("no .beads is unresolved", func(t *testing.T) {
		if _, ok := reg.Resolve(bare); ok {
			t.Error("Resolve(bare dir with no .beads) = ok, want false")
		}
	})

	t.Run("nonexistent path is unresolved", func(t *testing.T) {
		if _, ok := reg.Resolve(filepath.Join(root, "nope")); ok {
			t.Error("Resolve(nonexistent path) = ok, want false")
		}
	})
}

// TestOpenPersistsDiscovered: first run discovers repos under the root and writes
// them to the registry file.
func TestOpenPersistsDiscovered(t *testing.T) {
	root := t.TempDir()
	mkRepo(t, root, "alpha")
	file := filepath.Join(t.TempDir(), "repos.json")

	if _, err := Open(file, root); err != nil {
		t.Fatalf("open: %v", err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("registry file not written: %v", err)
	}
	var repos []Repo
	if err := json.Unmarshal(data, &repos); err != nil {
		t.Fatalf("registry file unparseable: %v", err)
	}
	if len(repos) != 1 || repos[0].Name != "alpha" {
		t.Errorf("persisted repos = %+v, want [alpha]", repos)
	}
}

// TestOpenPrunesDeadEntry: a registry entry whose dir was deleted (a removed
// worktree) is the most-recently-used, so it would win the active pick and then
// 502 every view + spam the reconciler (st-f9o). Open drops it, activates the
// live repo instead, and persists the pruned list.
func TestOpenPrunesDeadEntry(t *testing.T) {
	root := t.TempDir()
	live := mkRepo(t, root, "live")
	dead := filepath.Join(root, "gone") // never created on disk
	file := filepath.Join(t.TempDir(), "repos.json")

	now := time.Now()
	seed := []Repo{
		{Name: "gone", Path: dead, LastUsed: now}, // MRU — would be active
		{Name: "live", Path: live, LastUsed: now.Add(-time.Hour)},
	}
	data, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}

	reg, err := Open(file, root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	active, ok := reg.Active()
	if !ok || active.Path != live {
		t.Fatalf("active = %+v ok=%v, want the live repo", active, ok)
	}
	for _, repo := range reg.Repos() {
		if repo.Path == dead {
			t.Errorf("dead entry survived prune: %+v", reg.Repos())
		}
	}
	persisted, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var out []Repo
	if err := json.Unmarshal(persisted, &out); err != nil {
		t.Fatal(err)
	}
	for _, repo := range out {
		if repo.Path == dead {
			t.Errorf("dead entry persisted to disk: %+v", out)
		}
	}
}

// TestOpenMissingFileIsEmpty: a registry pointed at a non-existent file with an
// empty scan root opens clean, with no repos and no active selection.
func TestOpenMissingFileIsEmpty(t *testing.T) {
	file := filepath.Join(t.TempDir(), "absent.json")
	reg, err := Open(file, t.TempDir())
	if err != nil {
		t.Fatalf("open missing: %v", err)
	}
	if _, ok := reg.Active(); ok {
		t.Error("missing-file registry reports an active repo")
	}
}
