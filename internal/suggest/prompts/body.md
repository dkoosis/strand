# Body prompt

Canonical prompt for proposing a bead **description body**. Consumers (strand's
drawer assist, `/conform-beads`, the wrap conform pass, dispatch shaping) feed
the text below to a model verbatim and use the reply as-is — the rules are stated
inline so the output arrives already conformant and no consumer post-processes
it or appends section scaffolds of its own.

`bd lint` / `bd create --validate` stay the enforcement backstop.

---

You propose the description body for the bead described below. Return only the
body as markdown — no fenced wrapper around the whole reply, no preamble, no
commentary about what you changed.

## Who reads it

The primary reader is a **future agent**: the `bd ready` worker who opens this
bead cold, with none of the conversation that produced it. Humans triage on the
title; they reach the body only when they need the detail. Write for the agent.

## Two hard rules

1. **First line is one plain-English sentence** naming what the bead delivers.
   Complete sentence, no markdown heading above it, no jargon that needs the
   thread to decode. A reader who stops after that line still knows what this is.
   For a bead labeled `human`, that sentence is the ask, stated as an instruction:
   `Human must choose between design A and design B.`
2. **Every name resolves by search.** Use only vocabulary a reader can find in
   the repo, the docs, or the bead graph — exact symbols, files, commands, bead
   ids, decision-nug ids. Never a term coined in the session that produced the
   bead. If a concept has no findable name, describe it literally instead of
   naming it.

## Shape after the first line

Claudish: fragments, short bullets, tables, `‡ → ✗ φ`. No prose paragraphs, no
restating the title, no narration of how the bead came to exist. Include only
what the worker needs: the current behavior, the intended behavior, the
constraints that bound the change, and what is explicitly out of scope.

## Required sections by type

Emit the section the type demands, with real content — never an empty scaffold
and never a placeholder heading for the human to fill:

| Type | Required |
|---|---|
| epic | `## Success Criteria` |
| story | acceptance criteria |
| task | acceptance criteria |
| bug | `## Steps to Reproduce` + acceptance criteria |
| spike | `## Goal` (the question) + `## Findings` (`_pending_` at mint) |
| chore | none |

Acceptance criteria belong in bd's native `--acceptance` field, so propose them
as a body section **only** when the consumer asks for the body alone. Write them
measurably — an observable behavior at one test seam, not a feeling. Prefer the
highest seam that still proves the change; a bead needing three seams to verify
is usually three beads.

## Stay out of the body

- **Open questions** → they are a `spike` bead, not a paragraph here.
- **Categorical facts** → a label.
- **Justification for the approach** → `--context`.
- **Metadata** → the sanctioned `metadata` keys. Never echo `write_set`,
  `risk_markers`, or any other key into prose; every consumer reads the key and
  only the key.
- **A changelog.** The body states current truth. bd comments are append-only and
  cannot be deleted, so a body that narrates its own revisions is permanent noise.

## Register

Orwell's six rules: short word over long · cut every cuttable word · active voice ·
plain English over jargon · no dead metaphor · break a rule sooner than write
something unclear. Keep technical terms that carry real meaning (mutex, PATCH,
gradient).

**No clickbait** (decision nug `8452821b92b3`) — this governs headings as much as
titles. A heading states its content, never its significance or emotion. Banned
shapes: importance asserted with the noun withheld (`The distinction that
matters`), `Why X, not Y` framing, slogan cadence (`Lapse is data, not failure`).
A bead body is a form, not an essay: the same boring sections in the same order
across every instance of a type.

## When there is nothing to propose

If the body already carries its required sections and passes both hard rules,
return it unchanged.
