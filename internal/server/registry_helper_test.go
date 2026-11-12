package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dkoosis/strand/internal/registry"
)

// testRoot is a scratch directory for the whole test binary. Every repo path the
// tests register lives under it, so registry.Open keeps them (it prunes a path that
// is not on disk) and no test touches the real home directory or registry.
var testRoot = mustMkdirTemp()

var (
	alphaRepo = repoAt("alpha")
	betaRepo  = repoAt("beta")
)

func mustMkdirTemp() string {
	dir, err := os.MkdirTemp("", "strand-server-test-")
	if err != nil {
		panic(err)
	}
	return dir
}

// repoAt is a repo named name whose directory exists under testRoot.
func repoAt(name string) registry.Repo {
	path := filepath.Join(testRoot, name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		panic(err)
	}
	return registry.Repo{Name: name, Path: path}
}

// TestMain points the home directory and XDG config at testRoot, so nothing in the
// suite reads or writes the real ~/.config/strand/repos.json or the real home.
func TestMain(m *testing.M) {
	os.Setenv("HOME", testRoot)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(testRoot, ".config"))
	code := m.Run()
	os.RemoveAll(testRoot)
	os.Exit(code)
}

// newRegistry builds a registry the way production does: it writes the repos to a
// registry file under t.TempDir and loads it with registry.Open. The first repo's
// directory must exist (repoAt, or a t.TempDir path); Open drops any that do not.
func newRegistry(t *testing.T, repos ...registry.Repo) *registry.Registry {
	t.Helper()
	file := filepath.Join(t.TempDir(), "repos.json")
	if repos == nil {
		repos = []registry.Repo{}
	}
	data, err := json.Marshal(repos)
	if err != nil {
		t.Fatalf("marshal repos: %v", err)
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatalf("write registry file: %v", err)
	}
	reg, err := registry.Open(file, "")
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	return reg
}
