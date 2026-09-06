package strandmd

import (
	"os"
	"path/filepath"
	"testing"
)

// layout writes each name→body pair into a fresh repo dir, creating docs/ when a
// name needs it, and returns the dir. A path is relative to the repo root, so
// "docs/ROADMAP.md" and "ROADMAP.md" name the two locations under test.
func layout(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return dir
}

// TestNorthStarResolvesDocsDirFirst: the masthead ★ follows the same two
// locations as the roadmap, so a swept repo keeps its destination line.
func TestNorthStarResolvesDocsDirFirst(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			"swept repo: only docs/NORTH_STAR.md",
			map[string]string{"docs/NORTH_STAR.md": "★ from docs\n"},
			"from docs",
		},
		{
			"unswept repo: only root NORTH_STAR.md",
			map[string]string{"NORTH_STAR.md": "★ from the root\n"},
			"from the root",
		},
		{
			"both copies: docs wins",
			map[string]string{
				"docs/NORTH_STAR.md": "★ from docs\n",
				"NORTH_STAR.md":      "★ from the root\n",
			},
			"from docs",
		},
		{"neither copy", map[string]string{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NorthStar(layout(t, tc.files)); got != tc.want {
				t.Fatalf("NorthStar() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNorthStarPathNamesWhereTheFileBelongs: the masthead's empty state renders
// this path as "add <path>", so with neither copy present it must name the
// standard location — docs/ — not the root the sweep is emptying.
func TestNorthStarPathNamesWhereTheFileBelongs(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"neither copy: the standard location", map[string]string{}, filepath.Join("docs", NorthStarFile)},
		{"swept repo", map[string]string{"docs/NORTH_STAR.md": "★ x\n"}, filepath.Join("docs", NorthStarFile)},
		{"unswept repo", map[string]string{"NORTH_STAR.md": "★ x\n"}, NorthStarFile},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := layout(t, tc.files)
			want := filepath.Join(dir, tc.want)
			if got := NorthStarPath(dir); got != want {
				t.Fatalf("NorthStarPath() = %q, want %q", got, want)
			}
		})
	}
	if got := NorthStarPath(""); got != "" {
		t.Fatalf("NorthStarPath(\"\") = %q, want \"\"", got)
	}
}
