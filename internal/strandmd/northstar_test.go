package strandmd

import (
	"os"
	"path/filepath"
	"testing"
)

// writeNorthStar drops a NORTH_STAR.md into a fresh repo dir and returns the dir.
func writeNorthStar(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, NorthStarFile), []byte(body), 0o644); err != nil {
		t.Fatalf("write north star: %v", err)
	}
	return dir
}

// TestNorthStarReadsStarBlock: the masthead source is the first ★-marked line of
// NORTH_STAR.md (marker stripped) plus contiguous following lines up to a blank
// or heading, and blank when the file or ★ line is absent (st-y0a acceptance).
func TestNorthStarReadsStarBlock(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			"real doc shape",
			"# north star — repo\n\n★ the one-liner destination\n\n*Seeded 2026-07-19.*\n\nProse body.\n\n## section\n",
			"the one-liner destination",
		},
		{"bare star line", "★ make the invisible legible\n", "make the invisible legible"},
		{"no marker space", "★make the invisible legible\n", "make the invisible legible"},
		{
			"multi-line block",
			"★ first line\nsecond line\nthird line\n\nnot this\n",
			"first line\nsecond line\nthird line",
		},
		{
			"block stops at heading",
			"★ first line\nsecond line\n## section\nnot this\n",
			"first line\nsecond line",
		},
		{"indented star", "  ★ spaced line  \n", "spaced line"},
		{"no star line", "# heading\n\nProse only, no marker.\n", ""},
		{"empty file", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NorthStar(writeNorthStar(t, tc.body)); got != tc.want {
				t.Errorf("NorthStar = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNorthStarMissingFile: no file → blank, no crash.
func TestNorthStarMissingFile(t *testing.T) {
	if got := NorthStar(t.TempDir()); got != "" {
		t.Errorf("NorthStar(no file) = %q, want blank", got)
	}
	if got := NorthStar(""); got != "" {
		t.Errorf("NorthStar(empty path) = %q, want blank", got)
	}
}
