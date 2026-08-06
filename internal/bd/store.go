package bd

import (
	"os"
	"path/filepath"
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
// This is the ONE freshness signal both strand surfaces gate on: the server's
// snapshot cache evicts on it (freshEntryLocked/checkStale) and the counts refresher
// uses it as its change key (internal/counts). They MUST call this single helper —
// a second re-implementation is exactly the drift the shared home exists to kill
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
