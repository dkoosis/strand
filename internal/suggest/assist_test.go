package suggest

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubCompleter is the model seam under test: it records the system+user prompt it
// is handed and returns a canned reply (or error), so Assist is exercised with no
// network. It satisfies Completer.
var errModelDown = errors.New("model down")

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

// fullInput is an Input carrying every layer, for the assembly tests.
func fullInput() *Input {
	return &Input{
		Prompts:   Prompts{Title: "TITLE RULES: name the done state.", Body: "BODY RULES: first line is a plain sentence.", Source: sourceEmbed},
		Strand:    "# Bead rubric\nName work as a done state.\n\n## Actors\n- Maintainer: ships strand.",
		Actors:    "## Actors\n- Maintainer: ships strand.",
		NorthStar: "a strand that remembers across sessions",
		Title:     "Phase 2",
		Body:      "Old body text.",
		Type:      "story",
		Parent:    "Assist affordance across the drawer",
		Children:  []string{"Wire the preview slot", "Render the proposal"},
		Job:       "",
	}
}

// TestAssistPromptCarriesEveryLayer: the assembled system prompt carries the reply
// contract and both canonical element prompts, and the user prompt carries every
// per-call grounding layer. The prompts are the whole rule source — a layer that
// silently drops is a proposal grounded in less than the caller assembled.
func TestAssistPromptCarriesEveryLayer(t *testing.T) {
	in := fullInput()
	c := &stubCompleter{reply: "TITLE: Render the drawer preview slot"}
	if _, err := Assist(context.Background(), c, in); err != nil {
		t.Fatalf("Assist: %v", err)
	}
	for _, want := range []string{"TITLE PROMPT", "TITLE RULES", "BODY PROMPT", "BODY RULES", "PROJECT CONTEXT", "## Actors"} {
		if !strings.Contains(c.system, want) {
			t.Errorf("system prompt missing %q:\n%s", want, c.system)
		}
	}
	for _, want := range []string{
		"North star: a strand that remembers across sessions",
		"Current title: Phase 2",
		"Type: story",
		"Parent: Assist affordance across the drawer",
		"Wire the preview slot",
		"Old body text.",
	} {
		if !strings.Contains(c.user, want) {
			t.Errorf("user prompt missing %q:\n%s", want, c.user)
		}
	}
}

// TestAssistOutputProtocolComesLast: each canonical prompt is written for a consumer
// that asks for ONE element and closes by telling the model to return that element
// alone, unlabelled. Read last, either would suppress the TITLE:/BODY: markers
// splitReply needs, and a good proposal would parse as no proposal at all. So the
// output protocol has to sit after both element prompts and after PROJECT CONTEXT.
func TestAssistOutputProtocolComesLast(t *testing.T) {
	in := fullInput()
	in.Prompts.Title += "\n\nReturn only the title, with no label."
	in.Prompts.Body += "\n\nReturn only the body as markdown."
	c := &stubCompleter{reply: "TITLE: Render the drawer preview slot"}
	if _, err := Assist(context.Background(), c, in); err != nil {
		t.Fatalf("Assist: %v", err)
	}
	out := strings.Index(c.system, "===== OUTPUT =====")
	if out < 0 {
		t.Fatalf("system prompt states no closing output protocol:\n%s", c.system)
	}
	for _, earlier := range []string{"Return only the title", "Return only the body", "PROJECT CONTEXT"} {
		if i := strings.LastIndex(c.system, earlier); i > out {
			t.Errorf("%q appears after the output protocol — it would be the model's last word", earlier)
		}
	}
	// The closing section has to restate the form, not just cite it.
	for _, want := range []string{"TITLE:", "BODY:", "ignore its output instructions"} {
		if !strings.Contains(c.system[out:], want) {
			t.Errorf("output protocol missing %q:\n%s", want, c.system[out:])
		}
	}
}

// TestAssistOmitsBlankLayers: a blank layer leaves no trace in the prompt — most
// pointedly an uncited JTBD, which must never appear as an empty label the model
// could try to fill.
func TestAssistOmitsBlankLayers(t *testing.T) {
	in := fullInput()
	in.Strand, in.Parent, in.Children, in.Body = "", "", nil, ""
	c := &stubCompleter{reply: "TITLE: Render the drawer preview slot"}
	if _, err := Assist(context.Background(), c, in); err != nil {
		t.Fatalf("Assist: %v", err)
	}
	for _, gone := range []string{"PROJECT CONTEXT", "Job to be done"} {
		if strings.Contains(c.system+c.user, gone) {
			t.Errorf("prompt carries the blank layer %q:\n%s\n%s", gone, c.system, c.user)
		}
	}
	for _, gone := range []string{"Parent:", "Children:", "Current body:"} {
		if strings.Contains(c.user, gone) {
			t.Errorf("user prompt carries the blank layer %q:\n%s", gone, c.user)
		}
	}
}

// TestAssistParsesReply covers the reply contract: both elements, one element, a
// decorated title, and a reply proposing nothing.
func TestAssistParsesReply(t *testing.T) {
	tests := []struct {
		name      string
		reply     string
		wantTitle string
		wantBody  string
		wantNone  bool
	}{
		{
			name:      "both elements",
			reply:     "TITLE: Render the drawer preview slot\nBODY:\nThe drawer renders one preview.\n\n## Acceptance Criteria\n- one slot",
			wantTitle: "Render the drawer preview slot",
			wantBody:  "The drawer renders one preview.\n\n## Acceptance Criteria\n- one slot",
		},
		{
			name:      "title only",
			reply:     "TITLE: Render the drawer preview slot",
			wantTitle: "Render the drawer preview slot",
		},
		{
			name:     "body only",
			reply:    "BODY:\nThe drawer renders one preview.",
			wantBody: "The drawer renders one preview.",
		},
		{
			name:      "decorated title",
			reply:     "  **Title:** \"Render the drawer preview slot\"  ",
			wantTitle: "Render the drawer preview slot",
		},
		{
			name:      "body marker carries its first line inline",
			reply:     "TITLE: Render the slot\nBODY: The drawer renders one preview.\nMore detail.",
			wantTitle: "Render the slot",
			wantBody:  "The drawer renders one preview.\nMore detail.",
		},
		{name: "nothing proposed", reply: "NONE", wantNone: true},
		{name: "blank reply", reply: "   \n  ", wantNone: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &stubCompleter{reply: tt.reply}
			got, err := Assist(context.Background(), c, fullInput())
			if err != nil {
				t.Fatalf("Assist: %v", err)
			}
			if got.None != tt.wantNone {
				t.Errorf("None = %v, want %v", got.None, tt.wantNone)
			}
			if got.Title != tt.wantTitle {
				t.Errorf("Title = %q, want %q", got.Title, tt.wantTitle)
			}
			if got.Body != tt.wantBody {
				t.Errorf("Body = %q, want %q", got.Body, tt.wantBody)
			}
		})
	}
}

// TestAssistDropsUnchangedProposal: an element the model echoed back verbatim is
// not a proposal — applying it would be a PATCH that writes the value already
// there. Echoing both elements means nothing to suggest.
func TestAssistDropsUnchangedProposal(t *testing.T) {
	in := fullInput()
	c := &stubCompleter{reply: "TITLE: " + in.Title + "\nBODY:\n" + in.Body}
	got, err := Assist(context.Background(), c, in)
	if err != nil {
		t.Fatalf("Assist: %v", err)
	}
	if !got.None {
		t.Errorf("echoed reply yielded a proposal: title=%q body=%q", got.Title, got.Body)
	}
}

// TestAssistWhyNamesTheAnchor: the Why cites the inline job when the page cited
// one, else the north star.
func TestAssistWhyNamesTheAnchor(t *testing.T) {
	in := fullInput()
	c := &stubCompleter{reply: "TITLE: Render the drawer preview slot"}
	got, _ := Assist(context.Background(), c, in)
	if !strings.Contains(got.Why, "north star") {
		t.Errorf("Why = %q, want the north-star anchor", got.Why)
	}

	in.Job = "Triage what to work on next"
	got, _ = Assist(context.Background(), c, in)
	if !strings.Contains(got.Why, "Triage what to work on next") {
		t.Errorf("Why = %q, want the cited job", got.Why)
	}
}

// TestAssistReturnsModelError: a completer failure is returned, never swallowed into
// a fabricated proposal. There is no deterministic tier beneath to fall back to, so
// the caller must be able to say the assist failed.
func TestAssistReturnsModelError(t *testing.T) {
	c := &stubCompleter{err: errModelDown}
	got, err := Assist(context.Background(), c, fullInput())
	if !errors.Is(err, errModelDown) {
		t.Fatalf("err = %v, want the model error wrapped", err)
	}
	if got.Title != "" || got.Body != "" {
		t.Errorf("a failed call still produced a proposal: %+v", got)
	}
}
