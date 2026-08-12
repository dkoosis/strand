# Title prompt

Canonical prompt for proposing a bead **title**. Consumers (strand's drawer
assist, `/conform-beads`, the wrap conform pass, dispatch shaping) feed the text
below to a model verbatim and use the reply as-is — the rules are stated inline
so the output arrives already conformant and no consumer post-processes it.

`bd lint` / `bd create --validate` stay the enforcement backstop.

---

You propose ONE title for the bead described below. Return only the title: a
single line, no quotes, no `Title:` label, no preamble, no trailing period.

## What a title is

A title names the **done state** — the result that exists when the bead closes.
It is the one element both readers share: a human triaging a list, and a future
agent searching for prior art. Write it literal and searchable for both.

## Shape by type

- **epic** — the capability the arc delivers once every child is closed.
  Test: prepend "so that" and it still parses.
  ✓ `Forest view ships over the bead graph` · `CLI migrated from cobra to kong`
  ✗ `outer loop epic` (insider slug) · `loops plugin: hill-climbing optimization`
  (noun-pile) · `Give every skill a post-run loop` (activity, not done-state)
- **story** — `<persona> can <outcome>`. One persona, one outcome. The persona
  must be a role the project already names; never invent one.
  ✓ `operator can see why a bead is blocked` · `planner can reorder the backlog by drag`
  ✗ `blocked-reason badge` (widget) · `add drag-to-reorder` (activity)
- **task / chore** — one concrete outcome, `Verb object` or a done-state clause.
- **bug** — the wrong behavior plus its trigger: `<thing> <misbehaves> when <condition>`.
  ✓ `Reconciler drops orphaned query rows when AppendRun fails`
- **spike** — the question answered, inside its timebox.

## The four tests — a task title passes all four

1. **Names the done state.** A stranger reads it and knows what "closed" means.
2. **Literal and searchable.** Exact `pkg.Symbol`, file, command, and API names —
   dedup and search run over titles.
3. **Self-contained.** It groks with no parent and no description.
4. **One outcome.** Two deliverables mean two beads; propose the title for the
   one this bead is actually about.

## Banned outright

- **Vague verbs**: fix · update · improve · refactor · handle · cleanup · tweak ·
  address · enhance · optimize · support. They name activity, not outcome, and
  never say when it's done. (`fix` is fine in a bug title that also names the
  wrong behavior and its trigger — `Fix` alone as the verb is not.)
- **Phase and version labels**: Phase 2 · vNext · V3 · R6 · Milestone 4. They mark
  a roadmap slot, not a done state, and rot the moment the plan shifts. Name the
  capability instead.
- **`[type]` prefixes** — `[epic]`, `[bug]`. bd renders the type itself.
- **Bucket heads** — cleanup, misc, chores, WIP, various, general, polish,
  housekeeping, enhancements.
- **Metadata keys** — never mention or propose `jtbd`, `alignment`,
  `blast_radius`, or any other key. You propose a title, nothing else.

## Register

Orwell's six rules: short word over long · cut every cuttable word · active voice ·
plain English over jargon · no dead metaphor · break a rule sooner than write
something unclear. Keep technical terms that carry real meaning (mutex, PATCH,
gradient) — they are the literal names test 2 asks for.

**No clickbait** (decision nug `8452821b92b3`). A title states its content, never
its significance or emotion. Banned shapes:

- Importance asserted with the noun withheld — `The distinction that matters`
- `Why X, not Y` framing
- Slogan cadence — `Lapse is data, not failure`

Punctuation (`→`, `/`, en-dash) is fine; just never let a glyph be the only
carrier of a word.

## When there is nothing to propose

If the current title already passes every test above, return it unchanged.
