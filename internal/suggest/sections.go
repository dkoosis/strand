package suggest

import "strings"

// SectionInput is the plain data the section-gap detector reasons over: the bead's
// description Body and its Type (bd issue_type). Both optional — an empty input
// yields a None suggestion.
type SectionInput struct {
	Body string
	Type string
}

// SectionGap is one required section the body is missing, paired with a draft
// scaffold — the heading plus an empty placeholder marker — for the human to fill.
type SectionGap struct {
	Heading string
	Draft   string
}

// SectionSuggestion is the section-gap answer. Missing lists the absent required
// sections (empty when None). Proposed is the full description Apply would write:
// the current body with each missing section's scaffold appended. Why names the
// reason in one line for the preview. None is true when there is nothing to
// propose — the type has no required sections, or the body already carries them
// all. The detector never pads a complete bead.
type SectionSuggestion struct {
	Missing  []SectionGap
	Proposed string
	Why      string
	None     bool
}

// The required-section headings, named once so the rules, the draft scaffolds, and
// the present-heading match all reference the same string.
const (
	secAcceptance = "Acceptance Criteria"
	secSuccess    = "Success Criteria"
	secRepro      = "Steps to Reproduce"
)

// requiredSections maps a bead type to the section headings a well-formed bead of
// that type records. Drawn from the bdx:bead-fmt standard, which is a superset of
// `bd lint`: bd lint has no story rule, so shelling out to it would silently skip
// every strand story — the most common bead type. Here a story is held to the
// same Acceptance Criteria bar as a task. Types absent from the map — chore,
// spike — carry no required section.
var requiredSections = map[string][]string{
	"epic":    {secSuccess},
	"story":   {secAcceptance},
	"task":    {secAcceptance},
	"feature": {secAcceptance},
	"bug":     {secRepro, secAcceptance},
}

// sectionDrafts maps a section heading to the scaffold Apply appends: the H2
// heading and one empty placeholder marker. A skeleton, never invented content —
// Suggest scaffolds the section, the human fills it.
var sectionDrafts = map[string]string{
	secAcceptance: "## " + secAcceptance + "\n- ",
	secSuccess:    "## " + secSuccess + "\n- ",
	secRepro:      "## " + secRepro + "\n1. ",
}

// Sections reports which required sections a bead's body is missing and proposes a
// draft to fill each. It returns None when the type has no required sections or
// the body already carries them all — never padding a complete bead. Deterministic:
// no model, no network, no bd.
func Sections(in SectionInput) SectionSuggestion {
	want := requiredSections[normalizeType(in.Type)]
	if len(want) == 0 {
		return SectionSuggestion{None: true, Why: noRequirementWhy(in.Type)}
	}
	have := headingsIn(in.Body)
	var missing []SectionGap
	for _, h := range want {
		if have[strings.ToLower(h)] {
			continue
		}
		missing = append(missing, SectionGap{Heading: h, Draft: sectionDrafts[h]})
	}
	if len(missing) == 0 {
		return SectionSuggestion{None: true, Why: "All required sections present — nothing to suggest."}
	}
	return SectionSuggestion{
		Missing:  missing,
		Proposed: appendSections(in.Body, missing),
		Why:      sectionWhy(in.Type, missing),
	}
}

// normalizeType lowercases and trims a bead type for the requiredSections lookup.
func normalizeType(t string) string {
	return strings.ToLower(strings.TrimSpace(t))
}

// headingsIn collects the markdown headings present in a body as a lowercased set,
// so a present section matches case-insensitively at any heading depth. A heading
// is a line whose first non-space run is '#'; the marker and a trailing colon are
// stripped to the bare title. Fenced code blocks are skipped so a '#' comment
// inside a snippet is never read as a section.
func headingsIn(body string) map[string]bool {
	out := map[string]bool{}
	inFence := false
	for raw := range strings.SplitSeq(body, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		rest := strings.TrimLeft(line, "#")
		level := len(line) - len(rest)
		// CommonMark: 1–6 leading '#' then a space/tab. A bare '#Foo' is a
		// paragraph, not a heading, so it must not count as a present section.
		if inFence || level == 0 || level > 6 {
			continue
		}
		if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
			continue
		}
		title := strings.TrimRight(strings.TrimSpace(rest), ":")
		if title != "" {
			out[strings.ToLower(title)] = true
		}
	}
	return out
}

// appendSections builds the proposed description: the trimmed current body, a
// blank line, then each missing section's draft joined by blank lines. An empty
// body yields just the draft block.
func appendSections(body string, missing []SectionGap) string {
	drafts := make([]string, len(missing))
	for i, m := range missing {
		drafts[i] = m.Draft
	}
	block := strings.Join(drafts, "\n\n")
	if body = strings.TrimSpace(body); body == "" {
		return block
	}
	return body + "\n\n" + block
}

// sectionWhy names, in one line, the sections a bead of this type should record.
func sectionWhy(t string, missing []SectionGap) string {
	names := make([]string, len(missing))
	for i, m := range missing {
		names[i] = m.Heading
	}
	return "A " + displayType(t) + " should record its " + joinAnd(names) + "."
}

// noRequirementWhy is the None message when a type has no required sections.
func noRequirementWhy(t string) string {
	return "A " + displayType(t) + " has no required sections — nothing to suggest."
}

// displayType is the bead type for prose, falling back to "bead" when unset.
func displayType(t string) string {
	if n := normalizeType(t); n != "" {
		return n
	}
	return "bead"
}

// joinAnd joins names as "A", "A and B", or "A, B and C".
func joinAnd(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}
