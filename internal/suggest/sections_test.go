package suggest

import (
	"strings"
	"testing"
)

// TestSectionsFlagsGaps pins the section-gap detector over the per-type rules:
// each type is held to bead-fmt's required sections, a present heading is matched
// case-insensitively, a complete bead yields None, and a type with no rules yields
// None without inventing a section.
func TestSectionsFlagsGaps(t *testing.T) {
	tests := []struct {
		name     string
		in       SectionInput
		wantNone bool
		wantGaps []string // headings the suggestion must flag (when not None)
	}{
		{
			name:     "story missing acceptance criteria",
			in:       SectionInput{Type: "story", Body: "## Slice\nBody quality via the section gaps."},
			wantGaps: []string{"Acceptance Criteria"},
		},
		{
			name:     "task missing acceptance criteria",
			in:       SectionInput{Type: "task", Body: "Render the board columns."},
			wantGaps: []string{"Acceptance Criteria"},
		},
		{
			name:     "epic missing success criteria",
			in:       SectionInput{Type: "epic", Body: "Suggest affordance across the drawer."},
			wantGaps: []string{"Success Criteria"},
		},
		{
			name:     "bug missing both repro and acceptance",
			in:       SectionInput{Type: "bug", Body: "Nil issue panics the drawer redraw."},
			wantGaps: []string{"Steps to Reproduce", "Acceptance Criteria"},
		},
		{
			name:     "bug missing only acceptance when repro present",
			in:       SectionInput{Type: "bug", Body: "## Steps to Reproduce\n1. Open the drawer on a nil bead."},
			wantGaps: []string{"Acceptance Criteria"},
		},
		{
			name:     "complete story yields None",
			in:       SectionInput{Type: "story", Body: "## Slice\nText.\n## Acceptance Criteria\n- It works."},
			wantNone: true,
		},
		{
			name:     "heading match is case-insensitive",
			in:       SectionInput{Type: "task", Body: "## acceptance criteria\n- done"},
			wantNone: true,
		},
		{
			name:     "chore has no required sections",
			in:       SectionInput{Type: "chore", Body: "Bump the linter."},
			wantNone: true,
		},
		{
			name:     "spike has no required sections",
			in:       SectionInput{Type: "spike", Body: "Probe the gonum graph API."},
			wantNone: true,
		},
		{
			name:     "a heading inside a code fence is not counted",
			in:       SectionInput{Type: "task", Body: "```md\n## Acceptance Criteria\n```\nReal body."},
			wantGaps: []string{"Acceptance Criteria"},
		},
		{
			name:     "no space after hash is not a heading (CommonMark)",
			in:       SectionInput{Type: "task", Body: "#Acceptance Criteria\n- done"},
			wantGaps: []string{"Acceptance Criteria"},
		},
		{
			name:     "seven hashes exceed heading levels and do not count",
			in:       SectionInput{Type: "task", Body: "####### Acceptance Criteria\n- done"},
			wantGaps: []string{"Acceptance Criteria"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Sections(tc.in)
			if tc.wantNone {
				if !got.None {
					t.Fatalf("Sections(%+v) = %+v, want None", tc.in, got)
				}
				if len(got.Missing) != 0 {
					t.Errorf("None suggestion carried gaps: %+v", got.Missing)
				}
				if got.Proposed != "" {
					t.Errorf("None suggestion carried a proposal: %q", got.Proposed)
				}
				if got.Why == "" {
					t.Error("None suggestion missing its Why line")
				}
				return
			}
			if got.None {
				t.Fatalf("Sections(%+v) = None, want gaps %v", tc.in, tc.wantGaps)
			}
			if got.Why == "" {
				t.Error("suggestion missing its Why line")
			}
			gotHeadings := make([]string, len(got.Missing))
			for i, g := range got.Missing {
				gotHeadings[i] = g.Heading
				if g.Draft == "" {
					t.Errorf("gap %q carried no draft scaffold", g.Heading)
				}
			}
			if strings.Join(gotHeadings, "|") != strings.Join(tc.wantGaps, "|") {
				t.Errorf("Missing = %v, want %v", gotHeadings, tc.wantGaps)
			}
		})
	}
}

// TestSectionsProposedAppendsScaffold: the proposed description keeps the current
// body intact and appends every missing section's heading after it, so Apply
// augments rather than replaces.
func TestSectionsProposedAppendsScaffold(t *testing.T) {
	body := "## Slice\nBody quality via section gaps."
	got := Sections(SectionInput{Type: "story", Body: body})
	if got.None {
		t.Fatal("expected gaps, got None")
	}
	if !strings.HasPrefix(got.Proposed, body) {
		t.Errorf("Proposed dropped the current body:\n%q", got.Proposed)
	}
	if !strings.Contains(got.Proposed, "## Acceptance Criteria") {
		t.Errorf("Proposed missing the appended section heading:\n%q", got.Proposed)
	}
}

// TestSectionsProposedFromEmptyBody: an empty body proposes just the scaffold, no
// stray leading blank lines.
func TestSectionsProposedFromEmptyBody(t *testing.T) {
	got := Sections(SectionInput{Type: "bug", Body: ""})
	if got.None {
		t.Fatal("expected gaps for an empty bug body, got None")
	}
	if !strings.HasPrefix(got.Proposed, "## Steps to Reproduce") {
		t.Errorf("Proposed should lead with the first scaffold:\n%q", got.Proposed)
	}
	if strings.Contains(got.Proposed, "\n\n\n") {
		t.Errorf("Proposed has a triple blank line:\n%q", got.Proposed)
	}
}
