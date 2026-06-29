// Package suggest is strand's deterministic naming domain. It takes a bead's
// plain text — current title, body, type, and one cheap graph signal (the parent
// title) — and proposes a sharper, Verb-object title. It never touches bd or
// HTTP, so the namer stays unit-testable on fixtures and the drawer's Suggest
// affordance works on every bead with no model call, no network, and no API key
// (Tier-1).
//
// The rubric is "a title names the work as `Verb object`". A title that already
// leads with an action verb is left alone; a verbless or inert-bucket title
// (Phase N, cleanup, misc, refactor X, a bare noun) gets a rewrite drawn from the
// body and type. When there is nothing to draw from, Title returns a None
// suggestion so the caller shows "nothing to suggest" rather than inventing a
// name. A later tier (behind a separate seam) adds the model-grounded namer; this
// package is the deterministic floor it falls back to.
package suggest

import (
	"strings"
	"unicode"
)

// Word-count ceilings so a proposal stays title-length rather than swallowing a
// whole sentence of body.
const (
	maxTitleWords  = 9
	maxObjectWords = 7
)

// TitleInput is the plain data the Tier-1 namer reasons over. Body is the bead's
// description; Type is bd's issue_type (epic/story/task/bug/…); Parent is the
// parent bead's title — a cheap graph signal used only as a last-resort object
// hint. Every field is optional; an empty input yields a None suggestion.
type TitleInput struct {
	Title  string
	Body   string
	Type   string
	Parent string
}

// Suggestion is one proposed rewrite. Proposed is the new title; Why names the
// reason in one line for the preview. Tier is 1 for the deterministic namer.
// Anchored reports whether the proposal cites an intent anchor (always false at
// Tier-1; the model tier sets it). None is true when there is nothing to propose
// — the title already reads as Verb-object, or it is too thin to rewrite.
type Suggestion struct {
	Proposed string
	Why      string
	Tier     int
	Anchored bool
	None     bool
}

// Title proposes a sharper Verb-object title, or returns None when nothing can be
// proposed: the bead is a story or epic (the model tier owns those shapes), the
// current title already leads with an action verb, or nothing can be drawn from
// the body. Deterministic: no model, no network.
func Title(in TitleInput) Suggestion {
	t := normalize(in.Title)
	// The deterministic floor only writes task-shaped "Verb object" titles. A story
	// ("<actor> can <outcome>") and an epic (a done-state capability arc) need the
	// model tier's judgment; coercing them into "Add <noun>" reads as a developer
	// instruction, not the work. Stay quiet and let Tier-2 own them.
	if isModelOnlyType(in.Type) {
		return none()
	}
	if isSharp(t) {
		return none()
	}
	proposed := propose(in)
	if proposed == "" {
		return none()
	}
	return Suggestion{Proposed: proposed, Why: why(t), Tier: 1}
}

// none is the "nothing to suggest" answer; a Suggestion with None set and no text.
func none() Suggestion { return Suggestion{None: true} }

// normalize lowercases and trims a title for the inert/sharp checks; the proposal
// itself keeps the original casing.
func normalize(title string) string {
	return strings.ToLower(strings.TrimSpace(title))
}

// isSharp reports whether a title already names the work as Verb-object: at least
// two words, not an inert bucket, leading with a recognized action verb. title is
// the normalized (lowercased) form.
func isSharp(title string) bool {
	words := strings.Fields(title)
	if len(words) < 2 {
		return false
	}
	if isInertBucket(title) {
		return false
	}
	return actionVerbs[words[0]]
}

// isInertBucket reports whether a title is one of the inert placeholder shapes —
// a "Phase N" slot, or a leading bucket word (cleanup, misc, refactor, …) — that
// names a container rather than the work. title is the normalized form.
func isInertBucket(title string) bool {
	words := strings.Fields(title)
	if len(words) == 0 {
		return false
	}
	return words[0] == "phase" || bucketWords[words[0]]
}

// propose builds a Verb-object rewrite: prefer the body's own opening action
// phrase when it already leads with a verb; otherwise pair a type-default verb
// with the best object phrase drawn from the title, body, or parent. Empty when
// there is nothing to draw from.
func propose(in TitleInput) string {
	if p := actionPhrase(in.Body); p != "" {
		return p
	}
	obj := objectPhrase(in.Title, in.Body, in.Parent)
	if obj == "" {
		return ""
	}
	return verbForType(in.Type) + " " + obj
}

// actionPhrase returns the body's opening clause when it already leads with an
// action verb — that clause is itself a Verb-object title — trimmed to a title
// length. Empty when the body is missing or its opener is not an action verb.
func actionPhrase(body string) string {
	words := trimEmphasisWords(strings.Fields(firstClause(body)))
	if len(words) == 0 || !actionVerbs[strings.ToLower(words[0])] {
		return ""
	}
	return capitalize(clamp(words, maxTitleWords))
}

// objectPhrase finds the noun phrase the rewrite acts on: the title with its
// bucket word(s) stripped, else the body's opening clause, else the parent title.
func objectPhrase(title, body, parent string) string {
	if residual := stripBucket(title); residual != "" {
		return cleanObject(residual)
	}
	if clause := firstClause(body); clause != "" {
		return cleanObject(clause)
	}
	return cleanObject(parent)
}

// firstClause returns the first meaningful clause of a body: the first non-blank,
// non-heading line outside a fenced code block, cut at the first sentence
// terminator and stripped of leading markdown bullet/quote and ordered-list marks.
func firstClause(body string) string {
	inFence := false
	for raw := range strings.SplitSeq(body, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue // a code-fence delimiter — toggle, never content
		}
		if inFence || line == "" || strings.HasPrefix(line, "#") {
			continue // fenced code, blank, or markdown heading — not body content
		}
		line = strings.TrimSpace(strings.TrimLeft(line, "-*>•\t "))
		line = stripListMarker(line) // drop a leading "1." before cutSentence sees its dot
		if line == "" {
			continue
		}
		return cutSentence(line)
	}
	return ""
}

// stripListMarker drops a leading ordered-list marker ("1.", "2)") so a numbered
// step reads as its content. Without it, cutSentence would stop at the marker's
// own period and return just the digit.
func stripListMarker(line string) string {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i > 0 && i < len(line) && (line[i] == '.' || line[i] == ')') {
		return strings.TrimSpace(line[i+1:])
	}
	return line
}

// cutSentence returns line up to its first sentence terminator (. ! ?), dropping
// the terminator. A clause without one is returned whole.
func cutSentence(line string) string {
	if i := strings.IndexAny(line, ".!?"); i >= 0 {
		return strings.TrimSpace(line[:i])
	}
	return line
}

// stripBucket removes the leading bucket word(s) — and a "Phase N" number — from
// the title, returning the residual object words. Empty when the title was
// nothing but bucket words; the whole title when it opens with none.
func stripBucket(title string) string {
	words := strings.Fields(title)
	out := words
	for len(out) > 0 {
		// Trim trailing list/label punctuation so "Cleanup:" or "Refactor-" still
		// matches a bucket word.
		head := strings.TrimRight(strings.ToLower(out[0]), ":-")
		if head == "phase" {
			out = stripPhaseHead(out)
			continue
		}
		if bucketWords[head] {
			out = out[1:]
			continue
		}
		break
	}
	if len(out) == len(words) {
		return strings.Join(words, " ") // opened with no bucket word — keep it whole
	}
	return strings.Join(out, " ")
}

// stripPhaseHead drops a leading "Phase" and the numeric slot label that follows
// it (Phase 2, Phase 2:) when present.
func stripPhaseHead(words []string) []string {
	rest := words[1:]
	if len(rest) > 0 && isNumberish(rest[0]) {
		rest = rest[1:]
	}
	return rest
}

// cleanObject trims a phrase to a tidy object: cut at a sentence, drop a leading
// article, and clamp the word count. Empty when nothing is left.
func cleanObject(phrase string) string {
	words := trimEmphasisWords(strings.Fields(cutSentence(strings.TrimSpace(phrase))))
	if len(words) > 0 && articles[strings.ToLower(words[0])] {
		words = words[1:]
	}
	if len(words) == 0 {
		return ""
	}
	return clamp(words, maxObjectWords)
}

// trimEmphasisWords strips surrounding markdown emphasis (*, _, ~, `) from each
// word and drops any that reduce to nothing, so a bolded lead verb like **Add**
// is recognized and a proposal never carries stray markup.
func trimEmphasisWords(words []string) []string {
	out := words[:0]
	for _, w := range words {
		if w = strings.Trim(w, "*_~`"); w != "" {
			out = append(out, w)
		}
	}
	return out
}

// verbForType picks the default leading verb when the body offers none, keyed to
// the bead's kind: a bug is fixed, every other task-shaped kind is added. Story and
// epic never reach here — Title returns None for them, since the deterministic floor
// can't write an "<actor> can <outcome>" story or a done-state epic title.
func verbForType(t string) string {
	if strings.EqualFold(strings.TrimSpace(t), "bug") {
		return "Fix"
	}
	return "Add"
}

// isModelOnlyType reports whether a bead's kind needs the model tier to title well —
// a story ("<actor> can <outcome>") or an epic (a done-state capability arc). The
// deterministic floor can't shape either, so Title stays quiet rather than coercing
// them into a task-shaped "Add <noun>".
func isModelOnlyType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "story", "epic":
		return true
	default:
		return false
	}
}

// why explains in one line why the title was flagged, for the preview. title is
// the normalized form.
func why(title string) string {
	switch {
	case title == "":
		return "Empty title — name the work as Verb + object."
	case isInertBucket(title):
		return "Inert bucket title — name the work as Verb + object."
	default:
		return "Verbless title — lead with the action (Verb + object)."
	}
}

// clamp joins at most n words, so a proposal stays title-length.
func clamp(words []string, n int) string {
	if len(words) > n {
		words = words[:n]
	}
	return strings.Join(words, " ")
}

// capitalize upper-cases the first rune so the proposal reads as a title.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// isNumberish reports whether w is a bare number once surrounding punctuation is
// stripped — the slot label after "Phase" (2, 2:, [3]).
func isNumberish(w string) bool {
	w = strings.TrimFunc(w, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if w == "" {
		return false
	}
	for _, r := range w {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// actionVerbs is the curated set of imperative verbs a sharp engineering title
// leads with. Vague verbs (update, improve, enhance, tweak, refactor, change) are
// deliberately absent — a title built on them reads as a bucket, so the namer
// flags it for a sharper rewrite.
var actionVerbs = map[string]bool{
	"add": true, "build": true, "create": true, "wire": true, "render": true,
	"fix": true, "guard": true, "gate": true, "remove": true, "drop": true,
	"split": true, "extract": true, "rename": true, "move": true, "surface": true,
	"show": true, "expose": true, "return": true, "parse": true, "validate": true,
	"cache": true, "load": true, "store": true, "write": true, "read": true,
	"compute": true, "detect": true, "propose": true, "apply": true, "register": true,
	"handle": true, "support": true, "enable": true, "disable": true, "harden": true,
	"simplify": true, "document": true, "test": true, "cover": true, "audit": true,
	"implement": true, "introduce": true, "replace": true, "migrate": true,
	"normalize": true, "sanitize": true, "scope": true, "narrow": true, "route": true,
	"dispatch": true, "emit": true, "log": true, "track": true, "scan": true,
	"draft": true, "attach": true, "append": true, "insert": true, "delete": true,
	"flag": true, "pin": true,
}

// bucketWords are the inert placeholder heads — a title that opens with one names
// a container, not the work.
var bucketWords = map[string]bool{
	"cleanup": true, "enhancements": true, "enhancement": true, "misc": true,
	"miscellaneous": true, "refactor": true, "refactoring": true, "tweaks": true,
	"tweak": true, "fixes": true, "updates": true, "update": true,
	"improvements": true, "improvement": true, "chore": true, "chores": true,
	"wip": true, "todo": true, "stuff": true, "various": true, "general": true,
	"polish": true, "housekeeping": true,
}

// articles are dropped from the head of an object phrase so "Add the lane" reads
// as "Add lane".
var articles = map[string]bool{"a": true, "an": true, "the": true}
