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
