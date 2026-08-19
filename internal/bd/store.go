package bd

import (
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// StoreMTime reports the newest mtime of the repo's Dolt noms manifest, the file
// Dolt rewrites on every commit — strand's out-of-band change signal. The glob
// covers the (single) embedded database under the workspace without strand having
// to know its name. ok is false when nothing matches or every stat fails (a non-bd
// path, a permissions error): a missing signal degrades to the caller's pre-gate
// behavior rather than forcing churn. Read-only — it stats, never opens, the store,
// so it stays inside bd's "never touch Dolt directly" contract.
//
// This was the ONE freshness signal both strand surfaces gated on; the server's
// snapshot cache (freshEntryLocked/checkStale) still does. The counts refresher
// (internal/counts) has since switched to StoreContentKey below — StoreMTime moves on
// every bd read, including the refresher's own, so gating on it never lets a repo
// settle (st-3wp.1). The server cache self-churns the same way today; fixing it is a
// follow-up bead, not this one. Both signals still glob the one shared path — a second
// re-implementation of the glob is exactly the drift the shared home exists to kill
// (st-nm5; the st-p1f failure mode).
func StoreMTime(repoPath string) (time.Time, bool) {
	matches, _ := filepath.Glob(filepath.Join(repoPath, ".beads", "embeddeddolt", "*", ".dolt", "noms", "manifest"))
	var newest time.Time
	found := false
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil {
			continue
		}
		found = true
		if fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	return newest, found
}

// StoreContentKey reports a content-derived key for the repo's Dolt noms manifest(s):
// FNV-1a 64 folded over each manifest's (path, content) pair, sorted by path for a
// deterministic key regardless of glob order. Same glob and same read-only posture as
// StoreMTime, but unlike StoreMTime it does NOT move when a pure read rewrites the
// manifest's mtime with identical bytes — the counts refresher's self-churn bug
// (st-3wp.1): every bd read touches the manifest's mtime, so gating on mtime alone
// makes the refresher perpetually think every repo just changed. Content still moves
// on any genuine write — a local bd commit or an out-of-band `bd dolt pull/sync`,
// `bd import` — so st-nm5's out-of-band-change detection is preserved.
//
// Read-only in the same sense StoreMTime's doc comment claims: it opens the tiny
// (~183B) manifest metadata file to hash its bytes, one step past a bare stat, but
// still never opens the Dolt store's data tables — inside bd's "never touch Dolt
// directly" contract. ok is false when nothing matches or every read fails, the same
// degrade-to-caller-behavior signal StoreMTime returns.
func StoreContentKey(repoPath string) (int64, bool) {
	matches, _ := filepath.Glob(filepath.Join(repoPath, ".beads", "embeddeddolt", "*", ".dolt", "noms", "manifest"))
	sort.Strings(matches)
	h := fnv.New64a()
	found := false
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		found = true
		_, _ = h.Write([]byte(m))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(b)
		_, _ = h.Write([]byte{0})
	}
	if !found {
		return 0, false
	}
	return int64(h.Sum64()), true //nolint:gosec // G115: opaque hash key, sign/truncation is not meaningful
}
