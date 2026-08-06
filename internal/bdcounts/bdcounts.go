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
	"time"
)

// MetaKey is the reserved top-level key in counts.json under which the refresher
// stamps its liveness meta (last-run time + binary version). It is not a repo root
// — every real key is an absolute path — so keyed readers (Lookup, the shell status
// line's `.[$repo]`) never collide with it, and a value shaped unlike an entry
// unmarshals into a zero entry there, harmless. It lets a dead or wedged refresher
// be seen (st-18l/B4) instead of freezing counts.json indistinguishably from "nothing
// changed."
const MetaKey = "_meta"

// RefreshInterval is the cadence the launchd agent com.trixi.bd-counts fires `strand
// counts` at — the ONE source of that number on the strand side, mirroring the plist's
// StartInterval (cc-plugins: plugins/wrap/scripts/bd-counts-install.sh, <integer>120).
// Staleness is judged against 2× this: a single missed tick is normal jitter, two is a
// refresher that stopped.
const RefreshInterval = 120 * time.Second

// Meta is the refresher's liveness stamp, written under MetaKey. LastRun is the unix
// second the last `strand counts` run finished; Version is the binary that wrote it,
// so a stale ~/go/bin/strand reintroducing predicate drift (B2) is legible in the file.
type Meta struct {
	LastRun int64  `json:"lastRun"`
	Version string `json:"version"`
}

// IsStale is the stale-detection rule, pure so it tests without a file: the counts
// cache is stale when its last run is older than 2× the refresh interval. A lastRun of
// 0 (no stamp) is treated as not-stale here — the absence of a stamp is handled by the
// ok return of Reader.Stale, not by flagging every unstamped cache as broken.
func IsStale(lastRun int64, now time.Time) bool {
	if lastRun == 0 {
		return false
	}
	return now.Sub(time.Unix(lastRun, 0)) > 2*RefreshInterval
}

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
	key := filepath.Clean(repoPath)
	if key == MetaKey {
		return Buckets{}, false // the reserved liveness stamp is not a repo row
	}
	e, ok := m[key]
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

// Meta returns the refresher's liveness stamp from counts.json. ok is false — the
// caller shows no stale flag rather than a false alarm — when the file is missing,
// unreadable, malformed, or carries no MetaKey stamp (a pre-st-18l cache). The dynamic
// repo-path keys are ignored: only MetaKey is decoded.
func (r *Reader) Meta() (Meta, bool) {
	data, err := os.ReadFile(r.path)
	if err != nil {
		return Meta{}, false
	}
	var wrap struct {
		Meta Meta `json:"_meta"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		return Meta{}, false
	}
	if wrap.Meta.LastRun == 0 {
		return Meta{}, false // no stamp — not a stamped-then-stale cache
	}
	return wrap.Meta, true
}

// Stale reports whether the counts cache is older than 2× the refresh interval — a
// dead or wedged refresher (st-18l). ok is false when there's no meta stamp to judge,
// so the caller renders no flag rather than a false alarm on an unstamped cache.
func (r *Reader) Stale(now time.Time) (stale, ok bool) {
	m, ok := r.Meta()
	if !ok {
		return false, false
	}
	return IsStale(m.LastRun, now), true
}
