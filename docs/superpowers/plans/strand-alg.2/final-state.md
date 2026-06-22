# final-state — strand-alg.2 (V3 dependency-graph view)

Profile: craft · Shape: standard · Authority: stop-at-green · Date: 2026-06-22

## Outcome

`make check` green (0 lint, all `-race` tests pass). Graph-determinism loop 20/20.
Feature complete and reviewed. Branch pushed; **no PR, no `bd close`** (stop-at-green —
dk opens the PR).

## What shipped

A third view beside Table and Board: the bead dependency DAG, drawn layered top-down
(dagre), nodes sized by normalized PageRank, cycles outlined red, critical path outlined
yellow, node tap → existing detail drawer. Read-only. Metrics computed in-process via
`internal/graph` (gonum) — no `bv`, only the `bd` CLI data path (D7-revised / O8-dissolved).

Server computes + serializes a JSON node/edge model into a `data-graph` attribute (D8:
server renders, client only draws). Cytoscape + dagre (O3, dk-pinned) hydrate it client-side.

| File | Change |
|---|---|
| `internal/server/server.go` | `graphData`/`graphNode`/`graphEdge`, `graphView`, `handleGraph`, `graphModel`, `scopeIDs`, `blocksEdges`, `metricNodes`, `GET /graph` route |
| `internal/server/server_test.go` | 5 graph tests (fixture DAG: chain + cycle + out-of-scope noise) |
| `web/templates/partials.html` | `{{define "graph"}}` + Graph button in list & board toggles |
| `web/templates/page.html` | 3 vendored `<script defer>` tags |
| `web/static/app.js` | `initGraph()` — Cytoscape+dagre hydration, node-tap→drawer |
| `web/static/app.css` | `#cy` + `.graph-legend` |
| `web/static/vendor/{cytoscape,dagre,cytoscape-dagre}.min.js` | vendored libs (new) |
| `internal/graph/graph.go` | **PLAN-DELTA** (see below): `betterSucc()` critical-path determinism fix |

## Plan-vs-actual

Fully plan-adherent (R-A: every numbered step 1–8, Tests, Acceptance, Don't-build verified;
north-star = yes). The scoped-vs-whole-repo open question resolved as planned (scoped, drops
cross-scope `blocks` edges; whole-repo toggle is a clean follow-up bead if needed).

**PLAN-DELTA (dk-approved):** the diff spans two beads' code. Besides the strand-alg.2 view,
it carries a fix to **strand-alg.1's** `internal/graph/graph.go` `depths()`: the critical-path
branch selection used bare `d > best`, so two equal-depth successors resolved by gonum's
map-iteration order — a ~1/8 flaky nondeterministic critical path. Added `betterSucc()`: deeper
chain wins; on a tie the lower-ID branch wins; a `d==0` guard prevents a back-edge from being
chosen as the path's next hop (that guard also fixed an infinite loop in path reconstruction
for cyclic graphs, caught by `TestCycleDetected`). Verified deterministic 20/20.

## Review trail

- **R-self:** clean. Verified data-graph escape safety (html/template escapes `"`→`&#34;`,
  regex-safe), empty/single-node size guards, `cy?.destroy()` re-init (no leak), determinism 20×.
- **R-A (plan-adherence):** fully adherent, no deviations, north-star yes.
- **R-B (feature-dev:code-reviewer):** 0 critical; 2 "important", both self-flagged as
  not-runtime-bugs.
  - *Accepted:* `betterSucc` comment said "back-edge or leaf" — misleading (leaves return
    depth 1, never hit `d==0`). Reworded.
  - *Declined:* double-`cytoscape.use` guard — reviewer confirms defer-order + module-once make
    it unreachable in current architecture. Speculative; declined under craft/minimal-diff.
- **/simplify (4 agents):** reuse/altitude/simplification all clean (graph view mirrors
  handleBoard, reuses scopeBeads/listViewFor/render). 2 free efficiency wins applied:
  preallocate the two edge slices with `len(deps)` cap; single-pass min/max in `initGraph`
  (also dodges spread-arg limit on large scopes). Pre-existing view-toggle triplication noted,
  left out of scope (a future consolidation bead).

## north_star_answer

> Does this diff render the bead dependency DAG (scoped, sized by PageRank, cycles + critical
> path flagged, tap→drawer, read-only) and nothing more?

**Yes.** Every element is present and scoped exactly as specified; the only out-of-scope code is
the one acknowledged, dk-approved strand-alg.1 determinism fix.

## Watch-items (non-blocking)

- `window.cytoscapeDagre` UMD global name is runtime-only (not Go-test-covered); `initGraph`
  falls back to the `breadthfirst` layout if the registration is absent, so a wrong global
  degrades gracefully rather than throwing.
