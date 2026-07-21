// Package bdcounts reads the shared bead-count cache that the off-render launchd
// agent com.trixi.bd-counts writes (cc-plugins: plugins/wrap/scripts/bd-counts-refresh.sh).
// That agent is the ONE place bead counts are derived; the Claude Code status line
// and strand's masthead pulse are both dumb readers of the same file, so the two
// surfaces can never disagree (st-p1f).
//
// The file is a JSON object keyed by each repo's absolute root path:
//
//	{"<repo_root>": {"prefix":..,"bh":..,"bo":..,"bw":..,"bb":..,"bcl":..,"bdf":..,"ts":..}}
//
// where the buckets map onto strand's Pulse glyphs: bh=◆ ready∧human, bo=○ ready∧
// ¬human, bw=◐ in_progress, bb=● blocked, bcl=✓ closed, bdf=❄ deferred. strand
// reads only these six aggregates; the board/graph/detail views stay on bd.
package bdcounts

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Buckets is one repo's aggregate counts, mapped to strand's masthead glyphs. The
// field order matches the Pulse strip so a caller can copy them across by name.
type Buckets struct {
	Waiting    int // bh — ◆ ready ∧ human-gated
	Open       int // bo — ○ ready ∧ not human-gated
	InProgress int // bw — ◐ in_progress
	Blocked    int // bb — ● blocked
	Closed     int // bcl — ✓ closed
	Deferred   int // bdf — ❄ deferred
}

// entry is one repo's row as the agent writes it. Only the six buckets are read;
// prefix/root/ts are ignored (strand keys by the caller's repo path, not the row).
type entry struct {
	BH  int `json:"bh"`
	BO  int `json:"bo"`
	BW  int `json:"bw"`
	BB  int `json:"bb"`
	BCl int `json:"bcl"`
	BDf int `json:"bdf"`
}

// Reader loads the counts cache from a fixed path. It holds no state beyond that
// path — every Lookup re-reads the file, so strand always reflects the agent's
// latest write (the file is a few KB; the read is off any hot loop).
type Reader struct {
	path string
}

// NewReader points at the production cache: $BD_COUNTS_CACHE_DIR/counts.json, or
// ~/.cache/cc-dashboard/counts.json when the env var is unset — the same location
// bd-counts-refresh.sh writes.
func NewReader() *Reader {
	return &Reader{path: defaultPath()}
}

// NewReaderAt points at an explicit file — the test seam.
func NewReaderAt(path string) *Reader {
	return &Reader{path: path}
}

// defaultPath resolves the cache file, honoring BD_COUNTS_CACHE_DIR so a caller can
// redirect it the way the writer does. A missing home dir falls back to a relative
// path, which simply won't exist — Lookup then reports absent and the caller uses bd.
func defaultPath() string {
	dir := os.Getenv("BD_COUNTS_CACHE_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = ""
		}
		dir = filepath.Join(home, ".cache", "cc-dashboard")
	}
	return filepath.Join(dir, "counts.json")
}

// Lookup returns the buckets for repoPath. ok is false — the caller falls back to
// its own bd snapshot — when the file is missing, unreadable, malformed, or has no
// entry for the repo. repoPath is cleaned so a trailing slash or redundant segment
// still matches the agent's canonical absolute keys.
func (r *Reader) Lookup(repoPath string) (Buckets, bool) {
	data, err := os.ReadFile(r.path)
	if err != nil {
		return Buckets{}, false
	}
	var m map[string]entry
	if err := json.Unmarshal(data, &m); err != nil {
		return Buckets{}, false
	}
	e, ok := m[filepath.Clean(repoPath)]
	if !ok {
		return Buckets{}, false
	}
	return Buckets{
		Waiting:    e.BH,
		Open:       e.BO,
		InProgress: e.BW,
		Blocked:    e.BB,
		Closed:     e.BCl,
		Deferred:   e.BDf,
	}, true
}
