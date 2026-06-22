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
