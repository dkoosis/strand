# strand-alg.2 — V3 dependency-graph view

Shape: standard · Profile: craft · Branch: strand-alg.2 · O3 pinned: **Cytoscape + dagre**

## Direction

A third view beside Table and Board: the bead dependency DAG, drawn layered (top-down)
so blocks→blocked-by direction reads at a glance. Nodes sized by PageRank (foundational
beads loom), cycles flagged, the critical path highlighted. Read-only — a node tap opens
the existing detail drawer. The metrics come **in-process** from `internal/graph` (gonum),
not from `bv` (O8 dissolved, D7 revised); the only data path stays the bd CLI via
`IssueSource.List` + `IssueSource.Deps`.

Server computes everything and ships a JSON node/edge model into the fragment; Cytoscape
(client) only lays out and draws. Matches D8 (server renders, client enhances) exactly as
the Board does with SortableJS.

## Plan

1. **Scope → graph model (server).** New unexported `graphModel` builder in `server.go`:
   - reuse `listViewFor(f, epic)` then `scopeBeads(&view)` (server.go:268) for the in-scope beads
     — the **same** flattener the Board uses, so the graph can't diverge from Table/Board.
   - collect the scope's bead IDs; `src.Deps(ctx, ids...)` for edges; keep only `Type=="blocks"`
     (drop parent-child / relates_to — the DepEdge doc already says the graph keeps only blocks).
   - drop edges whose endpoints aren't both in-scope, so the DAG is closed over visible nodes.
     ‡ Load-bearing: `Deps` is dual-shape (single-ID query synthesizes `IssueID: ids[0]`,
     client.go:218-220), so a one-bead epic scope can yield a stray out-of-scope edge — this
     filter neutralizes it. Tested below.
   - `graph.Compute(ids, edges)` for metrics: map each `bd.DepEdge{IssueID, DependsOnID}` →
     `graph.Edge{Dependent: IssueID, Dependency: DependsOnID}` (direction confirmed: IssueID is
     blocked by DependsOnID).
2. **Serialize.** `graphData{Nodes []graphNode, Edges []graphEdge}`:
   - `graphNode{ID, Label, Status string; Priority int; Score float64; InCycle, OnPath bool}`
     — Score = PageRank (drives node size), InCycle from `Metrics.Cycles`, OnPath from
     `Metrics.CriticalPath`.
   - `graphEdge{Source, Target string}` — Source=Dependent, Target=Dependency.
   - marshal with `json.Marshal` in the handler. Inject via a **`data-graph` attribute** read by
     JS (`el.dataset.graph`, the path Sortable already uses) — foolproof default; bare
     interpolation in a `<script type="application/json">` block double-escapes. The fragment
     test parses the JSON back out of the rendered HTML, so a broken escape still fails red.
3. **Route + handler.** `GET /graph` (optional `?epic=`), mirroring `handleBoard`:
   build forest → `listViewFor` → `graphModel` → render `graph` fragment. No-repo / empty
   scope render the same empty-pane shape Board uses.
4. **`graph` fragment** (`partials.html`, new `{{define "graph"}}`): the pane-head with the
   view-toggle (Table | Board | **Graph** active), a legend (size=influence, red=cycle,
   bold=critical path), and `<div id="cy" data-graph="{{.JSON}}">` carrying the serialized model.
   Empty scope → same "forest is quiet" empty state.
5. **View-toggle third button.** Add a `Graph` button to the toggle in the `list` and `board`
   fragments (`hx-get="/graph"{{if .HasEpic}}?epic=…`, `hx-target="#listPane"`), so all three
   views switch the same pane.
6. **Cytoscape hydration (app.js).** On `htmx:afterSwap`, if `#cy` landed: read its `data-graph`
   attribute (`el.dataset.graph`), build elements, init Cytoscape with the `dagre` layout
   (rankDir TB). **Normalize** Score across the node set (min–max to a px range, or sqrt) so the
   largest node visibly looms — raw PageRank floats (~sub-1) render near-identical and the
   "foundational beads loom" read would silently fail. status→node color (reuse the status dot
   palette via CSS classes/data-attrs),
   InCycle→red border, OnPath→thicker edge/border. `cy.on('tap','node',…)` →
   `htmx.ajax('GET','/bead/'+id,{target:'#drawer'})` — the same `#drawer` swap a card click does,
   so the existing `htmx:afterSwap` drawer-open handler fires unchanged (no new open path).
   Guard on `window.cytoscape` like the board guards on `window.Sortable`.
7. **Vendor libs** into `web/static/vendor/`: `cytoscape.min.js`, `dagre.min.js`,
   `cytoscape-dagre.min.js`; three `<script defer>` tags in `page.html` after sortable.
8. **CSS** (`app.css`): `#cy` sizing (fills the list-pane body), legend chips. Minimal — the
   drawing is canvas, owned by Cytoscape.

## Files

| File | Change |
|---|---|
| `internal/server/server.go` | `graphModel`, `graphData`/`graphNode`/`graphEdge`, `handleGraph`, route |
| `web/templates/partials.html` | `{{define "graph"}}`; Graph button in `list` + `board` toggles |
| `web/templates/page.html` | three vendored `<script defer>` tags |
| `web/static/app.js` | Cytoscape init on afterSwap; node-tap → drawer |
| `web/static/app.css` | `#cy` + legend styling |
| `web/static/vendor/*` | cytoscape, dagre, cytoscape-dagre (vendored; confirm `//go:embed static` glob covers `vendor/*.js` or they 404 at runtime while httptest stays green) |
| `internal/server/server_test.go` | stub `Deps` returns fixture edges; `/graph` fragment test |

## Tests (requires_test)

`server_test.go`, httptest, stub mutates nothing (read-only view):
- `GET /graph` (whole region) → 200; body's `graph-data` JSON contains every in-scope bead as a
  node and each `blocks` edge as a `{source,target}`; parent-child edges excluded.
- `GET /graph?epic=<id>` → nodes limited to that epic's scope.
- **closed-over-scope:** an epic scope whose bead `blocks` something *outside* the scope asserts
  that edge is dropped (the stray edge from `Deps`' single-ID branch never reaches the model).
- node carries PageRank Score and the cycle/critical-path flags for a fixture DAG with a known
  shape — assert the **specific** InCycle and OnPath node IDs (one cycle, one longest chain), not
  "some node"; hard-code the fixture edge list (same rigor as strand-alg.1's tests). Server-side
  metric wiring only, not Cytoscape.
- no-repo → empty pane, 200 (matches Board).
- Assert on the serialized JSON model (parse the script block), not on rendered pixels — the
  Q0 HTML-fragment strategy; Cytoscape itself is not exercised in Go tests.

## Acceptance

`make check` green; `/graph` renders a layered DAG of the active scope with nodes sized by
PageRank, cycles flagged, critical path highlighted; a node tap opens the existing drawer;
Table↔Board↔Graph switch the same pane; metrics are computed in-process (no bv).

## Don't build

- ✗ server-computed layout (positions) — Cytoscape/dagre owns layout (O3 = client lib).
- ✗ editing from the graph — read-only view; writes stay in the drawer (R4 "navigate only").
- ✗ a new data path — only `bd` via `IssueSource`. No bv, no extra CLI.
- ✗ V4 insights/stats — that's strand-alg.3. Graph view shows the DAG, not the dashboards.

## Open question for review

**Scope-consistent vs whole-repo graph.** The plan scopes the DAG to one region/epic, matching
Table/Board, and drops `blocks` edges that cross the scope boundary. That keeps the three views
consistent and matches R4 ("render the dependency tree beneath an epic/chain root"). The cost:
a bead that blocks across epics shows no cross-scope edge — the dependency is invisible at this
zoom. Deliberate for V3.1; a whole-repo graph toggle is a clean follow-up bead if the scoped
view proves too narrow. Flagging for the plan-reviewer + dk rather than silently scoping.
