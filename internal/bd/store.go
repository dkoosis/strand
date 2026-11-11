package bd

import (
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
)

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
	matches, _ := filepath.Glob(filepath.Join(repoPath, ".beads", "embeddeddolt", "*", ".dolt", "noms", "manifest")) //nolint:errcheck // the pattern is a constant; Glob errs only on a malformed pattern
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
