// Package suggest is strand's bead-assist domain. One call proposes both elements
// of a bead — a sharper title and a conformant description body — grounded in the
// canonical prompts bdx:bead-fmt ships (loaded by LoadPrompts) plus the resolved
// STRAND.md, the north star, and the bead's own graph neighbors.
//
// There is deliberately no deterministic tier beneath it. A rule-based namer can
// only restate bead-fmt in Go, which is the drift this package exists to kill, so
// an unkeyed strand shows no assist affordance at all rather than a worse one. The
// required-section rules ride in the body prompt; enforcing them is bd's job
// (`bd lint`, `bd create --validate`), never strand scaffolding.
//
// The package holds no llm import: the model arrives through the Completer seam,
// so this stays the pure assist domain and tests run with no network.
package suggest

import (
	"context"
	"fmt"
	"strings"
)

// Completer is the model seam Assist calls: one text-in, text-out completion. The
// key-gated *llm.Client satisfies it; tests inject a stub.
type Completer interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// Input is the assembled grounding for one assist call. Prompts are the canonical
// bead-fmt element prompts. The standing layers (Strand, Actors) come from the
// resolved STRAND.md; the per-call layers ground the specific bead. Job is the
// inline-cited JTBD job title — populated ONLY when the bead's page already
// references one, never fetched and never required (it stays "" otherwise).
type Input struct {
	Prompts   Prompts  // canonical title + body prompts from bead-fmt
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

// Result is one assist proposal. Title and Body are the proposed replacements —
// either may be empty when the model judged that element already well-formed, and
// the drawer offers Apply only for the ones it filled. Why names the anchor in one
// line for the preview. None is true when the model proposed neither element.
type Result struct {
	Title string
	Body  string
	Why   string
	None  bool
}

// The reply contract strand imposes on top of the two canonical prompts: one call
// returns both elements, delimited, so the drawer's single button costs one model
// round-trip. The prompts themselves stay consumer-agnostic — the framing below is
// strand's, the rules are bead-fmt's.
const replyContract = `You are shaping one bead for strand's drawer. Two prompts follow, one per element: the TITLE PROMPT and the BODY PROMPT. Apply each to the bead described in the user message.

Reply in exactly this form, with no preamble, no commentary, and no code fence around the whole reply:

TITLE: <the proposed title on one line>
BODY:
<the proposed description body, markdown, to the end of the reply>

Omit an element entirely — its whole line, or the BODY: marker and everything after — when the bead's current version of it already satisfies that element's prompt. Propose only what you would change. If both already satisfy their prompts, reply with the single word NONE.`

// The two canonical prompts are written for a consumer that asks for ONE element
// and each closes by telling the model to return only that element, unlabelled.
// Read last, either would suppress the markers splitReply needs — a well-formed
// proposal would parse as no proposal at all. This closing section restates the
// reply form after them and names the conflict, so the markers win.
const outputProtocol = `Each element prompt above ends with instructions for a consumer that asks for that element alone, unlabelled. Those do not apply here: take the RULES from each prompt and ignore its output instructions. The reply form is the TITLE:/BODY: one stated at the top, and nothing else.

TITLE: <one line>
BODY:
<markdown to the end of the reply>

Omit either marker to leave that element alone; reply NONE to leave both.`

// Assist proposes a title and body for the bead: it assembles the prompt from in,
// sends it through the completer, and parses the reply. A completer error is
// returned rather than swallowed — the caller surfaces it, since with no
// deterministic tier beneath there is nothing to fall back to.
func Assist(ctx context.Context, c Completer, in *Input) (Result, error) {
	system, user := buildPrompt(in)
	raw, err := c.Complete(ctx, system, user)
	if err != nil {
		return Result{}, fmt.Errorf("suggest: assist complete: %w", err)
	}
	return parseReply(raw, in), nil
}

// buildPrompt assembles the plain-text model input. system is the reply contract,
// the two canonical element prompts, the resolved STRAND.md (rubric + ## Actors),
// and the output protocol last — it has to outrank the single-element output
// instruction each canonical prompt closes with. user is the per-call grounding —
// north star, an inline job WHEN present, the bead, its parent, and its children.
// Read-and-concat, no templating.
func buildPrompt(in *Input) (system, user string) {
	var sys strings.Builder
	sys.WriteString(replyContract)
	writeSection(&sys, "TITLE PROMPT", in.Prompts.Title)
	writeSection(&sys, "BODY PROMPT", in.Prompts.Body)
	writeSection(&sys, "PROJECT CONTEXT", in.Strand)
	writeSection(&sys, "OUTPUT", outputProtocol) // last word, over each prompt's own

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
		u.WriteString("Current body:\n")
		u.WriteString(s)
		u.WriteString("\n")
	}
	return sys.String(), u.String()
}

// writeSection appends a titled block when body is non-blank, skipping it entirely
// when blank so an absent layer (a repo with no STRAND.md overlay) leaves no trace.
func writeSection(b *strings.Builder, title, body string) {
	if body = strings.TrimSpace(body); body == "" {
		return
	}
	b.WriteString("\n\n===== ")
	b.WriteString(title)
	b.WriteString(" =====\n\n")
	b.WriteString(body)
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

// parseReply turns a model reply into a Result: the TITLE: line and the BODY:
// block, each optional. A proposal identical to the bead's current text is dropped
// — the model was asked to propose only what it would change, and an Apply that
// writes the same value is a wasted PATCH. Nothing left means None, so the drawer
// shows "nothing to suggest" rather than an empty preview.
func parseReply(raw string, in *Input) Result {
	title, body := splitReply(raw)
	if title == strings.TrimSpace(in.Title) {
		title = ""
	}
	if body == strings.TrimSpace(in.Body) {
		body = ""
	}
	if title == "" && body == "" {
		return Result{None: true}
	}
	return Result{Title: title, Body: body, Why: why(in)}
}

// splitReply pulls the two elements out of the reply: the text after the TITLE:
// marker, cleaned to a bare line, and everything after the BODY: marker. Markers
// are matched case-insensitively at the head of a line, and a reply that carries
// neither yields two empty strings.
func splitReply(raw string) (title, body string) {
	lines := strings.Split(raw, "\n")
	for i, ln := range lines {
		// Strip leading markdown decoration so a bolded or headed marker
		// ("**TITLE:**", "## BODY:") still reads as the marker it is.
		trimmed := strings.TrimLeft(strings.TrimSpace(ln), "*_#> ")
		low := strings.ToLower(trimmed)
		switch {
		case strings.HasPrefix(low, "title:"):
			title = cleanTitle(trimmed[len("title:"):])
		case strings.HasPrefix(low, "body:"):
			// The marker may carry the first body line inline ("BODY: text"); keep it.
			rest := strings.TrimSpace(trimmed[len("body:"):])
			tail := strings.Join(lines[i+1:], "\n")
			body = strings.TrimSpace(strings.TrimSpace(rest + "\n" + tail))
			return title, body // BODY: runs to the end — nothing after it to scan
		}
	}
	return title, body
}

// cleanTitle reduces a proposed title to one bare line, stripping surrounding
// quotes and markdown emphasis or heading marks. The model is asked for a bare
// line; this tolerates the common decorations when it doesn't comply.
func cleanTitle(raw string) string {
	for ln := range strings.SplitSeq(raw, "\n") {
		if ln = strings.Trim(strings.TrimSpace(ln), "\"'`*# "); ln != "" {
			return ln
		}
	}
	return ""
}

// why names the anchor the proposal cites: the inline job when the page referenced
// one, else the north-star line, else a generic intent note.
func why(in *Input) string {
	if job := strings.TrimSpace(in.Job); job != "" {
		return "Anchored to the job: " + job
	}
	if ns := strings.TrimSpace(in.NorthStar); ns != "" {
		return "Anchored to the north star: " + ns
	}
	return "Anchored to the project's intent."
}
