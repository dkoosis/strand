package suggest

import (
	"strings"
	"testing"
)

// firstWord returns the leading word of a proposal, lowercased — the verb the
// rubric requires every proposal to lead with.
func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return strings.ToLower(f[0])
}

// TestTitleFlagsAndRewrites pins the Tier-1 namer over the fixture shapes: inert
// buckets and verbless titles get a Verb-object rewrite, a sharp title is left
// alone, and a thin/empty input yields a None suggestion without panicking.
func TestTitleFlagsAndRewrites(t *testing.T) {
	tests := []struct {
		name     string
		in       TitleInput
		wantNone bool
		wantHas  string // substring the proposal must contain (when not None)
	}{
		{
			name:    "phase slot rewrites from the body verb",
			in:      TitleInput{Title: "Phase 2", Type: "story", Body: "## Slice\nAdd a suggest preview slot to the drawer."},
			wantHas: "Add a suggest preview slot",
		},
		{
			name:    "bare bucket word rewrites from the body",
			in:      TitleInput{Title: "cleanup", Type: "task", Body: "The drawer leaves stale locks behind on close."},
			wantHas: "drawer leaves stale locks",
		},
		{
			name:    "verbless title takes the body's leading verb",
			in:      TitleInput{Title: "the board columns", Type: "task", Body: "Render the board columns from a status pivot."},
			wantHas: "Render the board columns",
		},
		{
			name:    "bug with verbless body defaults to Fix",
			in:      TitleInput{Title: "misc", Type: "bug", Body: "Nil issue panics the drawer redraw."},
			wantHas: "Fix",
		},
		{
			name:    "epic with verbless body defaults to Build",
			in:      TitleInput{Title: "Phase 1", Type: "epic", Body: "Suggest affordance across the bead drawer."},
			wantHas: "Build",
		},
		{
			name:     "sharp Verb-object title is left alone",
			in:       TitleInput{Title: "Add the waiting-on-you lane", Type: "story", Body: "Body text here."},
			wantNone: true,
		},
		{
			name:     "empty input yields None without panic",
			in:       TitleInput{},
			wantNone: true,
		},
		{
			name:     "inert title with no body to draw from yields None",
			in:       TitleInput{Title: "Misc enhancements", Type: "task"},
			wantNone: true,
		},
		{
			name:    "parent title is the last-resort object source",
			in:      TitleInput{Title: "Phase 3", Type: "task", Parent: "Drawer shaping surface"},
			wantHas: "Drawer shaping surface",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Title(tc.in)
			if tc.wantNone {
				if !got.None {
					t.Fatalf("Title(%q) = %+v, want None", tc.in.Title, got)
				}
				if got.Proposed != "" {
					t.Errorf("None suggestion carried a proposal: %q", got.Proposed)
				}
				return
			}
			if got.None {
				t.Fatalf("Title(%q) = None, want a proposal", tc.in.Title)
			}
			if got.Tier != 1 {
				t.Errorf("Tier = %d, want 1 (deterministic)", got.Tier)
			}
			if got.Anchored {
				t.Error("Tier-1 proposal must not claim an anchor")
			}
			if got.Why == "" {
				t.Error("proposal missing its Why line")
			}
			if !strings.Contains(got.Proposed, tc.wantHas) {
				t.Errorf("Proposed = %q, want substring %q", got.Proposed, tc.wantHas)
			}
			// Invariant: every proposal leads with a recognized action verb.
			if !actionVerbs[firstWord(got.Proposed)] {
				t.Errorf("Proposed %q does not lead with an action verb", got.Proposed)
			}
		})
	}
}

// TestTitleSkipsMarkdownHeadings: the body's first real content line is used, not
// a leading "## Section" heading, so the proposal is drawn from prose.
func TestTitleSkipsMarkdownHeadings(t *testing.T) {
	got := Title(TitleInput{
		Title: "Phase 4",
		Type:  "task",
		Body:  "## Acceptance Criteria\n- Render the pulse bar from the live counts.",
	})
	if got.None {
		t.Fatal("expected a proposal, got None")
	}
	if !strings.Contains(got.Proposed, "Render the pulse bar") {
		t.Errorf("Proposed = %q, want it drawn from the content line", got.Proposed)
	}
}

// TestTitleDropsLeadingArticle: when the verb comes from the type default, the
// object phrase sheds a leading article so it reads cleanly after the verb.
func TestTitleDropsLeadingArticle(t *testing.T) {
	got := Title(TitleInput{Title: "refactor the registry loader", Type: "task"})
	if got.None {
		t.Fatal("expected a proposal, got None")
	}
	if strings.HasPrefix(got.Proposed, "Add the ") {
		t.Errorf("Proposed kept a leading article: %q", got.Proposed)
	}
	if !strings.Contains(got.Proposed, "registry loader") {
		t.Errorf("Proposed = %q, want the residual object", got.Proposed)
	}
}
