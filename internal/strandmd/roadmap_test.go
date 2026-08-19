package strandmd

import (
	"os"
	"path/filepath"
	"testing"
)

// writeRoadmap drops a ROADMAP.md into a fresh repo dir and returns the dir.
func writeRoadmap(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, RoadmapFile), []byte(body), 0o644); err != nil {
		t.Fatalf("write roadmap: %v", err)
	}
	return dir
}

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

// TestRoadmapReadsROADMAPEpicsSection is the sdlc-standard format (st-3wp.1, the Go
// twin of roadmap-epics.sh's roadmap_parse): `N. [status] <title> → <id>[, <id>...]`
// numbered lines inside a `## Epics` section (legacy `## Milestones`/`## Route`
// headings tolerated), id = the first token after the first arrow.
func TestRoadmapReadsROADMAPEpicsSection(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			"real doc shape, [status] tag stripped",
			"## Epics\n\n1. [done] SDLC machinery ships → sd-jhxe\n2. [active] The SDLC works as one system → sd-ev2\n",
			[]string{"sd-jhxe", "sd-ev2"},
		},
		{"no status tag", "## Epics\n1. Title → sd-x\n", []string{"sd-x"}},
		{"multi-id line: first wins", "## Epics\n1. Title → sd-x, sd-y\n", []string{"sd-x"}},
		{"multi-id line, whitespace-only separator", "## Epics\n1. Title → sd-x sd-y\n", []string{"sd-x"}},
		{
			"numbered line without an arrow is skipped, not a blank id",
			"## Epics\n1. Not an epic line, no arrow\n2. Real epic → sd-y\n",
			[]string{"sd-y"},
		},
		{"legacy ## Milestones heading", "## Milestones\n1. Title → sd-x\n", []string{"sd-x"}},
		{"legacy ## Route heading", "## Route\n1. Title → sd-x\n", []string{"sd-x"}},
		{
			"stops at next H2",
			"## Epics\n1. a → sd-a\n## Other\n2. b → sd-b\n",
			[]string{"sd-a"},
		},
		{
			"a numbered list in a later section is not read as epics",
			"## Epics\n1. a → sd-a\n## Architecture\n1. some module\n2. another module\n",
			[]string{"sd-a"},
		},
		{"order preserved", "## Epics\n1. A → sd-a\n2. B → sd-b\n3. C → sd-c\n", []string{"sd-a", "sd-b", "sd-c"}},
		{"multi-digit numbers", "## Epics\n9. A → sd-a\n10. B → sd-b\n", []string{"sd-a", "sd-b"}},
		{"empty section", "## Epics\n\n*nothing yet*\n", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Roadmap(writeRoadmap(t, tc.body))
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

// TestRoadmapFoldsWrappedLine is the sd-r59 repro (observed live on sdlc's own
// ROADMAP.md 2026-08-15, pinned in bead-journey.test.sh's parse_epics case): a title
// too long for one line wraps onto an indented continuation carrying the arrow and
// ids. The continuation must fold into its numbered line before parsing, not be
// mis-read as a separate, arrow-less numbered-list item.
func TestRoadmapFoldsWrappedLine(t *testing.T) {
	body := "## Epics\n\n" +
		"1. [active] Proven in practice, then spread: the gates pass\n" +
		"   and conform carries the SDLC to the Go fleet → sd-ev2, sd-nbs\n"
	got := Roadmap(writeRoadmap(t, body))
	want := []string{"sd-ev2"}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("Roadmap = %v, want %v — the wrapped continuation must fold into epic 1, not parse as its own arrow-less item", got, want)
	}
}

// TestRoadmapPrefersROADMAPFileOverNorthStar: when both files exist, ROADMAP.md wins
// — it is the sdlc standard; NORTH_STAR.md is the legacy pointer, resolved only when
// ROADMAP.md is absent (roadmap-epics.sh's roadmap_file, reduced to two branches —
// the kg face page is a shell-only migration rung, out of scope in Go per st-3wp.1's
// non-goals).
func TestRoadmapPrefersROADMAPFileOverNorthStar(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, RoadmapFile), []byte("## Epics\n1. Title → sd-new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, NorthStarFile), []byte("## roadmap\n1. sd-old — old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Roadmap(dir)
	if len(got) != 1 || got[0] != "sd-new" {
		t.Errorf("Roadmap = %v, want [sd-new] — ROADMAP.md must win over NORTH_STAR.md", got)
	}
}

// TestRoadmapFallsBackToNorthStarLegacyFormat: no ROADMAP.md → NORTH_STAR.md's
// original `## roadmap` + id-first-token parse is the final fallback, unchanged
// (TestRoadmapReadsOrderedEpicIDs already pins that format's own edge cases; this
// just proves the fallback actually fires from Roadmap's new resolution order).
func TestRoadmapFallsBackToNorthStarLegacyFormat(t *testing.T) {
	got := Roadmap(writeNorthStar(t, "## roadmap\n1. sd-legacy — still works\n"))
	if len(got) != 1 || got[0] != "sd-legacy" {
		t.Errorf("Roadmap = %v, want [sd-legacy] — no ROADMAP.md must fall back to NORTH_STAR.md", got)
	}
}
