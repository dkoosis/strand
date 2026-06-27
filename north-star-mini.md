# strand — north-star (mini)

*One screen. The recenter the dispatch SR reads verbatim. Drift-check, not a spec.*

**strand renders synthesis, not the catalog.** It shows where the mass,
the bugs, the stuck work sit — so a human can orient → decide → shape.

## The line

> The human shapes the top. Agents own below the story line.

- **Shape (human):** epics, stories, priority, clarity. The bead drawer is the shaping
  surface — Description leads; agent ops (claim/close/delete) stay demoted.
- **Execute (agents):** tasks below the story line. Their fields
  (rank, requires_test, difficulty, est_cost) are agent/rubric-owned — viewable, not
  stomped from the UI.

## Drift smells (fail the recenter)

- Catalog/CRUD polish that makes strand a nicer bug list instead of a sharper lens.
- Loud agent ops crowding the human's shaping act.
- Editable system metadata that lets a human clobber what agents depend on.
- Graph/insights that decorate rather than drive a decision.

## Pass test

A change is on-star if it sharpens **orient → decide → shape** for the human at the top,
or gets out of the agents' way below the line. Otherwise it drifted.
