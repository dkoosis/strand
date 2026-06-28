package suggest

import (
	"context"
	"fmt"
	"strings"
)

// Completer is the model seam Tier-2 calls: one text-in, text-out completion. The
// key-gated *llm.Client satisfies it; tests inject a stub so the namer never hits
// the network. Keeping the seam local keeps package suggest free of an llm import —
// it stays the pure naming domain, with the model wired in by the caller.
type Completer interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// Tier2Input is the assembled grounding for one model-grounded title proposal. The
// standing layers (Strand, Actors) come from the resolved STRAND.md; the per-call
// layers (NorthStar, the bead, Parent, Children) ground the specific bead. Job is
// the inline-cited JTBD job title — populated ONLY when the bead's page already
// references one, never fetched and never required (it stays "" otherwise).
type Tier2Input struct {
	Strand    string   // resolved STRAND.md: rubric + ## Actors + direction
	Actors    string   // the ## Actors section alone — the persona registry
	NorthStar string   // the project's one-line north star
	Title     string   // current bead title
	Body      string   // current bead description
	Type      string   // bd issue_type
	Parent    string   // parent bead title (a cheap graph signal)
	Children  []string // child bead titles
	Job       string   // inline-cited JTBD job title; "" when the page cites none
}

// tier2Instruction is the standing naming directive appended to the STRAND.md
// rubric in the system prompt. It pins the bead-fmt shape, sources the actor from
// the ## Actors section, and bans a jtbd key — the model proposes a title only.
const tier2Instruction = `You are strand's bead namer. Propose ONE sharper title for the bead described below.

Rules:
- Name the work's own concrete outcome as "Verb object" (bead-fmt style), grounded in the north star.
- Draw any actor reference from the ## Actors section above; never invent an actor.
- Propose a title only. Never add or mention a "jtbd" key or any metadata key.
- Return ONLY the proposed title on a single line — no quotes, no label, no preamble.`

// Tier2 grounds a title proposal in the model: it assembles the prompt from in,
// sends it through the completer, and parses the reply into a Suggestion with
// Anchored set. A completer error is returned so the caller falls back to Tier-1;
// Tier2 never fabricates a suggestion on failure.
func Tier2(ctx context.Context, c Completer, in *Tier2Input) (Suggestion, error) {
	system, user := buildTier2Prompt(in)
	raw, err := c.Complete(ctx, system, user)
	if err != nil {
		return Suggestion{}, fmt.Errorf("suggest: tier-2 complete: %w", err)
	}
	return parseTier2(raw, in), nil
}

// buildTier2Prompt assembles the plain-text model input. system is the resolved
// STRAND.md (rubric + ## Actors) followed by the naming instruction; user is the
// per-call grounding — north star, an inline job WHEN present, the bead, its
// parent, and its children. No templating: read-and-concat, the SIMPLICITY the
// st-suggest.3 design note calls for.
func buildTier2Prompt(in *Tier2Input) (system, user string) {
	var sys strings.Builder
	if s := strings.TrimSpace(in.Strand); s != "" {
		sys.WriteString(s)
		sys.WriteString("\n\n")
	}
	sys.WriteString(tier2Instruction)

	var u strings.Builder
	writeField(&u, "North star", in.NorthStar)
	writeField(&u, "Job to be done", in.Job) // present only when the page cited one
	writeField(&u, "Current title", in.Title)
	writeField(&u, "Type", in.Type)
	writeField(&u, "Parent", in.Parent)
	if kids := nonEmpty(in.Children); len(kids) > 0 {
		u.WriteString("Children:\n")
		for _, ch := range kids {
			u.WriteString("- ")
			u.WriteString(ch)
			u.WriteString("\n")
		}
	}
	if s := strings.TrimSpace(in.Body); s != "" {
		u.WriteString("Body:\n")
		u.WriteString(s)
		u.WriteString("\n")
	}
	return sys.String(), u.String()
}

// writeField appends a "Label: value" line when value is non-blank, skipping it
// entirely when blank so an absent layer (notably an uncited JTBD) leaves no trace.
func writeField(b *strings.Builder, label, value string) {
	if value = strings.TrimSpace(value); value == "" {
		return
	}
	b.WriteString(label)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteString("\n")
}

// nonEmpty returns the trimmed, non-blank entries of in, preserving order.
func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// parseTier2 turns a model reply into a grounded Suggestion: the cleaned title, an
// anchored Why citing the job (when the page cited one) else the north star, Tier
// 2, and Anchored. A blank reply yields None so the caller shows "nothing to
// suggest" rather than an empty proposal.
func parseTier2(raw string, in *Tier2Input) Suggestion {
	proposed := cleanTitle(raw)
	if proposed == "" {
		return none()
	}
	return Suggestion{
		Proposed: proposed,
		Why:      tier2Why(in),
		Tier:     2,
		Anchored: true,
	}
}

// tier2Why names the anchor the proposal cites: the inline job when the page
// referenced one, else the north-star line, else a generic intent note.
func tier2Why(in *Tier2Input) string {
	if job := strings.TrimSpace(in.Job); job != "" {
		return "Anchored to the job: " + job
	}
	if ns := strings.TrimSpace(in.NorthStar); ns != "" {
		return "Anchored to the north star: " + ns
	}
	return "Anchored to the project's intent."
}

// cleanTitle pulls a bare title from a model reply: the first non-blank line, with
// a leading "Title:" label and surrounding quotes/markdown/heading marks stripped.
// The model is asked for a single bare line; this tolerates the common decorations
// when it doesn't comply.
func cleanTitle(raw string) string {
	for ln := range strings.SplitSeq(raw, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if low := strings.ToLower(ln); strings.HasPrefix(low, "title:") {
			ln = strings.TrimSpace(ln[len("title:"):])
		}
		ln = strings.Trim(ln, "\"'`*# ")
		if ln != "" {
			return ln
		}
	}
	return ""
}
