package bd

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestStoreMTime exercises the real glob path both surfaces gate on (st-69h,
// CodeRabbit catch): it builds the on-disk layout StoreMTime globs for and asserts it
// finds the manifest's mtime, so a drift in the .beads/embeddeddolt/*/.dolt/noms/manifest
// convention fails here instead of silently degrading the whole gate to a no-op. A bare
// directory with no store returns ok=false — the degrade-to-old-behavior signal.
func TestStoreMTime(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, ".beads", "embeddeddolt", "demo", ".dolt", "noms", "manifest")
	if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("root-chunk"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(manifest, want, want); err != nil {
		t.Fatal(err)
	}

	got, ok := StoreMTime(root)
	if !ok {
		t.Fatal("StoreMTime returned ok=false for a valid store layout — the glob path has drifted")
	}
	if !got.Equal(want) {
		t.Errorf("StoreMTime = %v, want %v", got, want)
	}

	if _, ok := StoreMTime(t.TempDir()); ok {
		t.Error("StoreMTime returned ok=true for a directory with no store — want false")
	}
}

// writeManifest is setStoreMTime's twin for the content-key tests: it builds the
// on-disk store layout StoreContentKey globs for and writes content at path, without
// touching mtime (the content tests don't care about it).
func writeManifest(t *testing.T, root, content string) string {
	t.Helper()
	manifest := filepath.Join(root, ".beads", "embeddeddolt", "demo", ".dolt", "noms", "manifest")
	if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return manifest
}

// TestStoreContentKeyStableAcrossRewriteWithSameContent is the counts-refresher's
// whole reason for switching off StoreMTime: a pure bd read rewrites the manifest's
// mtime with byte-identical content, and the content key must not move when that
// happens — only StoreMTime should react to that self-churn.
func TestStoreContentKeyStableAcrossRewriteWithSameContent(t *testing.T) {
	root := t.TempDir()
	manifest := writeManifest(t, root, "root-chunk")

	key1, ok := StoreContentKey(root)
	if !ok {
		t.Fatal("StoreContentKey ok=false for a valid store layout")
	}

	// Rewrite with the SAME bytes but a different mtime — simulating the self-churn
	// a pure `bd stats`/`bd list` read causes on the noms manifest.
	future := time.Now().Add(time.Hour)
	if err := os.WriteFile(manifest, []byte("root-chunk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(manifest, future, future); err != nil {
		t.Fatal(err)
	}

	key2, ok := StoreContentKey(root)
	if !ok {
		t.Fatal("StoreContentKey ok=false after rewrite")
	}
	if key1 != key2 {
		t.Errorf("StoreContentKey moved on a content-identical rewrite: %d -> %d", key1, key2)
	}
}

// TestStoreContentKeyMovesOnContentChange: a genuine content change (an out-of-band
// write — pull/sync/import — or a local commit) must move the key, or st-nm5's
// out-of-band-change detection breaks under the new key.
func TestStoreContentKeyMovesOnContentChange(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "root-chunk-v1")
	key1, ok := StoreContentKey(root)
	if !ok {
		t.Fatal("StoreContentKey ok=false for a valid store layout")
	}

	writeManifest(t, root, "root-chunk-v2")
	key2, ok := StoreContentKey(root)
	if !ok {
		t.Fatal("StoreContentKey ok=false after content change")
	}
	if key1 == key2 {
		t.Error("StoreContentKey did not move on a genuine content change")
	}
}

// TestStoreContentKeyNoStore: ok=false with no store — the degrade-to-caller signal,
// matching StoreMTime.
func TestStoreContentKeyNoStore(t *testing.T) {
	if _, ok := StoreContentKey(t.TempDir()); ok {
		t.Error("StoreContentKey returned ok=true for a directory with no store — want false")
	}
}

// StoreMTime lives in the test file: only tests call it, and deadcode fails an
// unreachable function in the binary. The comment below is its original doc.
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
