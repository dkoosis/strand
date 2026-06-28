package suggest

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubCompleter is the model seam under test: it records the system+user prompt it
// is handed and returns a canned reply (or error), so Tier-2 is exercised with no
// network. It satisfies suggest.Completer.
type stubCompleter struct {
	system string
	user   string
	calls  int
	reply  string
	err    error
}

func (s *stubCompleter) Complete(_ context.Context, system, user string) (string, error) {
	s.calls++
	s.system, s.user = system, user
	return s.reply, s.err
}

// fullInput is a Tier2Input carrying every layer, for the assembly tests.
func fullInput() Tier2Input {
	return Tier2Input{
		Strand:    "# Bead rubric\nName work as Verb object.\n\n## Actors\n- Maintainer: ships strand.",
		Actors:    "## Actors\n- Maintainer: ships strand.",
		NorthStar: "a strand that remembers across sessions",
		Title:     "Phase 2",
		Body:      "Add a suggest preview slot to the drawer.",
		Type:      "story",
		Parent:    "Suggest affordance across the drawer",
		Children:  []string{"Wire the preview slot", "Render the proposal"},
		Job:       "",
	}
}

// TestBuildTier2Prompt_AssemblesAllLayers pins context assembly: the system prompt
// carries the resolved STRAND.md (rubric + ## Actors) plus the naming instruction;
// the user turn carries the north-star line, the bead, its parent, and each child.
func TestBuildTier2Prompt_AssemblesAllLayers(t *testing.T) {
	in := fullInput()
	system, user := buildTier2Prompt(in)

	for _, want := range []string{"Bead rubric", "## Actors", "Maintainer"} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt missing STRAND.md fragment %q:\n%s", want, system)
		}
	}
	// The instruction must reach the model so it names an outcome and never stamps a key.
	for _, want := range []string{"Verb object", "Actors", "jtbd"} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt missing instruction fragment %q:\n%s", want, system)
		}
	}
	// Every per-call layer must reach the user turn: north star, current title,
	// type, parent, each child, and the body.
	for _, want := range []string{
		"a strand that remembers across sessions",
		"Phase 2",
		"story",
		"Suggest affordance across the drawer",
		"Wire the preview slot",
		"Render the proposal",
		"Add a suggest preview slot to the drawer.",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing layer %q:\n%s", want, user)
		}
	}
}

// TestBuildTier2Prompt_OmitsJTBDWhenAbsent pins the JTBD gate: with no inline job
// the prompt carries no job line — a JTBD reaches the model only when the page
// already references one.
func TestBuildTier2Prompt_OmitsJTBDWhenAbsent(t *testing.T) {
	_, user := buildTier2Prompt(fullInput()) // Job == ""
	if strings.Contains(strings.ToLower(user), "job to be done") {
		t.Errorf("absent JTBD leaked into the prompt:\n%s", user)
	}
}

// TestBuildTier2Prompt_IncludesJTBDWhenPresent: an inline-referenced job is folded
// into the user turn when present.
func TestBuildTier2Prompt_IncludesJTBDWhenPresent(t *testing.T) {
	in := fullInput()
	in.Job = "Triage what to work on next"
	_, user := buildTier2Prompt(in)
	if !strings.Contains(user, "Triage what to work on next") {
		t.Errorf("present JTBD missing from the prompt:\n%s", user)
	}
}

// TestParseTier2_GroundedSuggestion: a model reply parses into a Tier-2 suggestion
// that is anchored, names the proposed title, and cites the north-star line.
func TestParseTier2_GroundedSuggestion(t *testing.T) {
	in := fullInput()
	got := parseTier2("Add a drawer suggest preview slot\n", in)
	if got.None {
		t.Fatal("expected a proposal, got None")
	}
	if got.Proposed != "Add a drawer suggest preview slot" {
		t.Errorf("Proposed = %q, want the cleaned model title", got.Proposed)
	}
	if !got.Anchored {
		t.Error("Tier-2 proposal must set Anchored")
	}
	if got.Tier != 2 {
		t.Errorf("Tier = %d, want 2", got.Tier)
	}
	if !strings.Contains(got.Why, in.NorthStar) {
		t.Errorf("Why = %q, want it to cite the north-star line", got.Why)
	}
}

// TestParseTier2_WhyCitesJobWhenPresent: when the page cited a job, the Why cites
// the job rather than the north-star line.
func TestParseTier2_WhyCitesJobWhenPresent(t *testing.T) {
	in := fullInput()
	in.Job = "Triage what to work on next"
	got := parseTier2("Add the triage lane", in)
	if !strings.Contains(got.Why, in.Job) {
		t.Errorf("Why = %q, want it to cite the job", got.Why)
	}
}

// TestParseTier2_CleansReply: surrounding quotes, a Title: label, and trailing
// prose lines are stripped so Proposed is a bare title.
func TestParseTier2_CleansReply(t *testing.T) {
	got := parseTier2("Title: \"Add the suggest preview slot\"\nThis names the outcome.", fullInput())
	if got.Proposed != "Add the suggest preview slot" {
		t.Errorf("Proposed = %q, want the cleaned title", got.Proposed)
	}
}

// TestParseTier2_EmptyReplyIsNone: a blank model reply yields a None suggestion
// rather than an empty proposal.
func TestParseTier2_EmptyReplyIsNone(t *testing.T) {
	got := parseTier2("   \n\n", fullInput())
	if !got.None {
		t.Errorf("blank reply = %+v, want None", got)
	}
}

// TestTier2_UsesCompleterSeam: Tier2 sends the assembled prompt through the
// injected completer (no network), parses the reply, and returns an anchored
// suggestion — proving the model seam, not a live client.
func TestTier2_UsesCompleterSeam(t *testing.T) {
	c := &stubCompleter{reply: "Add the grounded drawer slot"}
	got, err := Tier2(context.Background(), c, fullInput())
	if err != nil {
		t.Fatalf("Tier2 error = %v", err)
	}
	if c.calls != 1 {
		t.Errorf("completer called %d times, want exactly 1", c.calls)
	}
	if !strings.Contains(c.system, "## Actors") || !strings.Contains(c.user, "Phase 2") {
		t.Errorf("completer did not receive the assembled prompt: system=%q user=%q", c.system, c.user)
	}
	if got.Proposed != "Add the grounded drawer slot" || !got.Anchored || got.Tier != 2 {
		t.Errorf("Tier2 = %+v, want the anchored model title at Tier 2", got)
	}
}

// TestTier2_PropagatesCompleterError: a model failure surfaces as an error so the
// caller can fall back to Tier-1; it never fabricates a suggestion.
func TestTier2_PropagatesCompleterError(t *testing.T) {
	c := &stubCompleter{err: errors.New("model down")}
	if _, err := Tier2(context.Background(), c, fullInput()); err == nil {
		t.Error("Tier2 swallowed a completer error; want it propagated for Tier-1 fallback")
	}
}
