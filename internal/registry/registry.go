// Package registry tracks the beads workspaces strand knows about and which one
// is active. The active repo scopes every view (spec D3); the registry persists
// to ~/.config/strand/repos.json so the choice survives a restart (spec O9). One
// repo source of truth, one active selection — the rest of strand reads through
// it and re-scopes when it changes.
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrUnknownRepo means a switch targeted a path the registry doesn't hold.
var ErrUnknownRepo = errors.New("repo not registered")

// ErrNoBeads means a path can't be registered because it has no .beads workspace.
var ErrNoBeads = errors.New("no .beads workspace")

// Repo is one registered beads workspace. Path is the directory bd runs in; Name
// is its label in the selector; Prefix is the bead-id prefix (cosmetic, filled
// best-effort); LastUsed drives the most-recently-used ordering and default.
type Repo struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Prefix   string    `json:"prefix,omitempty"`
	LastUsed time.Time `json:"last_used"`
}

// Registry holds the known repos and the active selection behind one mutex, so
// concurrent HTTP requests can read and switch safely. file is the persistence
// path; an empty file means in-memory (tests), with no disk reads or writes.
type Registry struct {
	mu     sync.Mutex
	file   string
	repos  []Repo
	active string // active repo path; empty = none
}

// ConfigPath is the registry file location, honoring $XDG_CONFIG_HOME and falling
// back to ~/.config (spec O9). It deliberately avoids os.UserConfigDir, which
// resolves to ~/Library/Application Support on macOS and defeats the XDG intent.
func ConfigPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "strand", "repos.json")
}

// ScanRoot is the directory tree discovery walks: ~/Projects (spec O6).
func ScanRoot() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Projects")
}

// Open loads the registry from file (a missing file is an empty registry, not an
// error), folds in any workspaces discovered under scanRoot, picks the
// most-recently-used as active, and persists the merged result. An empty file
// path yields an in-memory registry that never touches disk.
func Open(file, scanRoot string) (*Registry, error) {
	r := &Registry{file: file}
	if err := r.load(); err != nil {
		return nil, err
	}
	r.merge(discover(scanRoot))
	r.sortLocked()
	r.pickActiveLocked()
	if err := r.saveLocked(); err != nil {
		return nil, err
	}
	return r, nil
}

// InMemory builds a registry with no persistence, for tests and callers that
// supply their own repos. The most-recently-used repo is active by default.
func InMemory(repos ...Repo) *Registry {
	r := &Registry{repos: repos}
	r.sortLocked()
	r.pickActiveLocked()
	return r
}

// Repos returns the known repos in most-recently-used order. The slice is a copy;
// callers can read it without holding the lock.
func (r *Registry) Repos() []Repo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Repo, len(r.repos))
	copy(out, r.repos)
	return out
}

// Active returns the active repo and whether one is selected. No repos (or a
// stale active path) yields ok=false, which the UI shows as the empty state.
func (r *Registry) Active() (Repo, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i := r.indexOf(r.active); i >= 0 {
		return r.repos[i], true
	}
	return Repo{}, false
}

// Switch makes path the active repo, stamps it most-recently-used, and persists.
// An unknown path is ErrUnknownRepo — the caller never silently scopes to nothing.
func (r *Registry) Switch(path string) (Repo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := r.indexOf(path)
	if i < 0 {
		return Repo{}, fmt.Errorf("%w: %s", ErrUnknownRepo, path)
	}
	r.repos[i].LastUsed = time.Now()
	r.active = path
	r.sortLocked()
	if err := r.saveLocked(); err != nil {
		return Repo{}, err
	}
	return r.repos[r.indexOf(path)], nil
}

// Add registers the repo at path (validating it has a .beads workspace) and makes
// it active. A leading ~ expands to the home directory so the add field accepts
// the path the user would type. A path with no .beads is ErrNoBeads — the empty
// state's whole point is to keep an un-initialized directory out of the registry.
func (r *Registry) Add(path string) (Repo, error) {
	path, err := filepath.Abs(expandHome(filepath.Clean(path)))
	if err != nil {
		return Repo{}, fmt.Errorf("resolve absolute path: %w", err)
	}
	if !hasBeads(path) {
		return Repo{}, fmt.Errorf("%w: %s", ErrNoBeads, path)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if i := r.indexOf(path); i >= 0 {
		r.repos[i].LastUsed = time.Now()
	} else {
		r.repos = append(r.repos, Repo{Name: filepath.Base(path), Path: path, LastUsed: time.Now()})
	}
	r.active = path
	r.sortLocked()
	if err := r.saveLocked(); err != nil {
		return Repo{}, err
	}
	return r.repos[r.indexOf(path)], nil
}

// Rescan folds in any workspaces newly present under scanRoot, keeping existing
// entries' history, then persists. The active selection is untouched.
func (r *Registry) Rescan(scanRoot string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.merge(discover(scanRoot))
	r.sortLocked()
	return r.saveLocked()
}

// merge adds discovered repos whose path is not already known, preserving the
// history (last_used, prefix) of repos already in the registry.
func (r *Registry) merge(found []Repo) {
	for _, f := range found {
		if r.indexOf(f.Path) < 0 {
			r.repos = append(r.repos, f)
		}
	}
}

// indexOf returns the position of path in repos, or -1. Callers hold the lock.
func (r *Registry) indexOf(path string) int {
	for i := range r.repos {
		if r.repos[i].Path == path {
			return i
		}
	}
	return -1
}

// sortLocked orders repos most-recently-used first, with never-used repos after
// and ties broken by name, so the selector reads stably. Callers hold the lock.
func (r *Registry) sortLocked() {
	sort.SliceStable(r.repos, func(i, j int) bool {
		a, b := r.repos[i], r.repos[j]
		if !a.LastUsed.Equal(b.LastUsed) {
			return a.LastUsed.After(b.LastUsed)
		}
		return a.Name < b.Name
	})
}

// pickActiveLocked keeps a still-valid active selection, else defaults to the
// most-recently-used repo (repos[0] after the sort). Callers hold the lock.
func (r *Registry) pickActiveLocked() {
	if r.indexOf(r.active) >= 0 {
		return
	}
	if len(r.repos) > 0 {
		r.active = r.repos[0].Path
	} else {
		r.active = ""
	}
}

// load reads the registry file. A missing file is an empty registry, not an
// error; an empty file path skips disk entirely. Callers hold the lock (Open
// constructs before any other goroutine can reach the registry).
func (r *Registry) load() error {
	if r.file == "" {
		return nil
	}
	data, err := os.ReadFile(r.file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read registry: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, &r.repos); err != nil {
		return fmt.Errorf("parse registry %s: %w", r.file, err)
	}
	return nil
}

// saveLocked writes the registry file, creating its directory. An empty file path
// skips disk. Callers hold the lock.
func (r *Registry) saveLocked() error {
	if r.file == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(r.file), 0o755); err != nil {
		return fmt.Errorf("create registry dir: %w", err)
	}
	data, err := json.MarshalIndent(r.repos, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal registry: %w", err)
	}
	if err := os.WriteFile(r.file, data, 0o600); err != nil {
		return fmt.Errorf("write registry: %w", err)
	}
	return nil
}

// discover finds beads workspaces directly under root by globbing root/*/.beads.
// A missing or unreadable root yields nothing, not an error — discovery is a
// best-effort convenience over the registry's explicit adds.
func discover(root string) []Repo {
	if root == "" {
		return nil
	}
	matches, _ := filepath.Glob(filepath.Join(root, "*", ".beads"))
	out := make([]Repo, 0, len(matches))
	for _, m := range matches {
		path := filepath.Dir(m)
		out = append(out, Repo{Name: filepath.Base(path), Path: path})
	}
	return out
}

// hasBeads reports whether path holds a .beads workspace (file or directory).
func hasBeads(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".beads"))
	return err == nil
}

// expandHome turns a leading ~ into the home directory, so the add field accepts
// the shorthand a user would type. Anything else is returned unchanged.
func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path[1:], "/"))
		}
	}
	return path
}
