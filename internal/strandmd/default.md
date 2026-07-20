# STRAND context

User-managed grounding for `strand` Tier-2 suggestions, modeled on `~/.claude/CLAUDE.md`.
strand ships this default once, then leaves it to you — edit freely. A repo-local
`./.strand/STRAND.md` overlays this file: your real `## Actors` and project direction win.

This is NOT the north star — the destination doc is `NORTH_STAR.md` at the repo
root (its `★` line is the masthead one-liner). This file is suggestion grounding:
rubric + actors.

## Bead Quality Rubric

Work is **epic > story > task**. Each type has a job; a suggestion must match the bead's type.

- **epic** — a capability too big for one session, delivered as a DAG of stories/tasks. Title names the done-state capability the arc delivers.
- **story** — one persona's shippable outcome. Title: `<actor> can <outcome>`, where `<actor>` is a declared Actor below.
- **task** — one concrete change in one session. Title passes four tests: names the done-state (result, not activity); literal & searchable (exact `pkg.Symbol` / file / command names); self-contained; one outcome.

Title rules (every bead):

- Name the **done-state**, not the activity. ✗ vague verbs: fix · update · improve · refactor · handle · cleanup · tweak.
- Literal & searchable — real symbol / file / command names; titles feed search and dedup.
- ✗ a `[type]` prefix (bd renders the type) · ✗ phase / version labels (Phase 2 · vNext · V3).
- One outcome per bead — two deliverables are two beads.

## Actors

The personas a story can serve. A story's `<actor> can <outcome>` must name one of these.
Replace this stub with your project's real registry in `./.strand/STRAND.md`.

- Agent — the autonomous `bd ready` worker that owns work below the story line.
- Human — the person who shapes epics, stories, and priority above the story line.
