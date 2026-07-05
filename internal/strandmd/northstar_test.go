package strandmd

import (
	"os"
	"path/filepath"
	"testing"
)

// writeMini drops a north-star-mini.md into a fresh repo dir and returns the dir.
func writeMini(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, NorthStarMiniFile), []byte(body), 0o644); err != nil {
		t.Fatalf("write mini: %v", err)
	}
	return dir
}

// TestNorthStarMiniReadsLine: the masthead source is the first content line of
// north-star-mini.md, tolerating frontmatter and a heading marker, and blank when
// the file is absent or empty (str-d2s acceptance).
func TestNorthStarMiniReadsLine(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"bare line", "Make the invisible legible.\n", "Make the invisible legible."},
		{"heading", "# Make the invisible legible.\n", "Make the invisible legible."},
		{"frontmatter nug", "---\nid: abc\ntype: reference.decision\n---\nMake the invisible legible.\n", "Make the invisible legible."},
		{"leading blanks", "\n\n  spaced line  \n", "spaced line"},
		{"few lines preserved", "First line.\nSecond line.\nThird line.\n", "First line.\nSecond line.\nThird line."},
		{"few lines under frontmatter", "---\nid: abc\n---\nA reminder.\nAnother.\n", "A reminder.\nAnother."},
		{"empty file", "", ""},
		{"frontmatter only", "---\nid: abc\n---\n", ""},
		{"unclosed frontmatter", "---\nid: abc\nMake the invisible legible.\n", "---\nid: abc\nMake the invisible legible."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NorthStarMini(writeMini(t, tc.body)); got != tc.want {
				t.Errorf("NorthStarMini = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNorthStarMiniMissingFile: no file → blank, no crash.
func TestNorthStarMiniMissingFile(t *testing.T) {
	if got := NorthStarMini(t.TempDir()); got != "" {
		t.Errorf("NorthStarMini(no file) = %q, want blank", got)
	}
	if got := NorthStarMini(""); got != "" {
		t.Errorf("NorthStarMini(empty path) = %q, want blank", got)
	}
}
