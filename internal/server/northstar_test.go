package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dkoosis/strand/internal/registry"
	"github.com/dkoosis/strand/internal/strand"
	"github.com/dkoosis/strand/web"
)

// writeMini drops a north-star-mini.md into a fresh repo dir and returns the dir.
func writeMini(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "north-star-mini.md"), []byte(body), 0o644); err != nil {
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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := northStarMini(writeMini(t, tc.body)); got != tc.want {
				t.Errorf("northStarMini = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNorthStarMiniMissingFile: no file → blank, no crash.
func TestNorthStarMiniMissingFile(t *testing.T) {
	if got := northStarMini(t.TempDir()); got != "" {
		t.Errorf("northStarMini(no file) = %q, want blank", got)
	}
	if got := northStarMini(""); got != "" {
		t.Errorf("northStarMini(empty path) = %q, want blank", got)
	}
}

// TestMastheadReadsFileThenFlagOverrides: synFor reads the repo's mini when no
// --northstar flag is set, and the flag (a non-empty seeded NorthStar) wins.
func TestMastheadReadsFileThenFlagOverrides(t *testing.T) {
	dir := writeMini(t, "from the file\n")
	repo := registry.Repo{Name: "x", Path: dir}
	tmpl, err := web.Templates()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	srcFor := func(registry.Repo) IssueSource { return &stubBD{} }
	reg := registry.InMemory(repo)

	noFlag := New(srcFor, reg, tmpl, web.Static(), strand.Synthesis{})
	if got := noFlag.synFor(repo).NorthStar; got != "from the file" {
		t.Errorf("no-flag masthead = %q, want file content", got)
	}

	withFlag := New(srcFor, reg, tmpl, web.Static(), strand.Synthesis{NorthStar: "from the flag"})
	if got := withFlag.synFor(repo).NorthStar; got != "from the flag" {
		t.Errorf("flag did not override file: %q", got)
	}
}
