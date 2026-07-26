package strandmd

import "testing"

// TestRoadmapReadsOrderedEpicIDs: Roadmap returns the epic ids from the
// `## roadmap` section's numbered lines, in order, epic id = the first token
// after the number. Empty when the section or file is absent.
func TestRoadmapReadsOrderedEpicIDs(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			"real doc shape",
			"# north star\n\n★ line\n\n## roadmap\n\n*Route order.*\n\n1. tx-4535e5be — trixi assembles turn context\n2. tx-02a231d1 — trixi uses current user state\n\n## next section\n\n9. not-this — after the section\n",
			[]string{"tx-4535e5be", "tx-02a231d1"},
		},
		{
			"stops at next h2",
			"## roadmap\n1. a-1 — first\n## other\n2. a-2 — should not read\n",
			[]string{"a-1"},
		},
		{"no roadmap section", "# h\n\n★ line\n\nprose\n", nil},
		{"empty roadmap section", "## roadmap\n\n*just prose, no numbered lines*\n", nil},
		{"empty file", "", nil},
		{
			"tolerates extra whitespace and multi-digit numbers",
			"## roadmap\n 1.  a-1 — first\n10. a-10 — tenth\n",
			[]string{"a-1", "a-10"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Roadmap(writeNorthStar(t, tc.body))
			if len(got) != len(tc.want) {
				t.Fatalf("Roadmap = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("Roadmap[%d] = %q, want %q (full %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

// TestRoadmapMissingFile: no file → nil, no crash.
func TestRoadmapMissingFile(t *testing.T) {
	if got := Roadmap(t.TempDir()); got != nil {
		t.Errorf("Roadmap(no file) = %v, want nil", got)
	}
	if got := Roadmap(""); got != nil {
		t.Errorf("Roadmap(empty path) = %v, want nil", got)
	}
}
