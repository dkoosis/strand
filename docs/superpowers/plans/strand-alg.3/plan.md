# strand-alg.3 — V4 insights/stats view

Shape: standard · Profile: craft · Branch: strand-alg.3

## Direction

Phase 5 (spec §10). A fourth scope view (Table/Board/Graph → **Insights**) rendering
Tufte panels over strand's own in-process metrics — the `internal/graph` gonum lib
shipped in .1, never `bv` (D7 revised). Same scope contract as the other views: a
region, or one epic via `?epic=`.

Six panels, every value computed in-process from the issue list + `blocks` deps:

1. **Quick-ref counts** — total / open / in-progress / ready / blocked / stale.
2. **Influence** — top-N PageRank (foundational beads).
3. **Bottlenecks** — top-N betweenness (beads many chains route through).
4. **Critical path** — the single longest dependency chain, in order.
5. **Cycles** — dependency cycles, or an all-clear.
6. **Label health** — label distribution over open beads + an untagged warning.

## Design decisions

- **Triage truth (ready/blocked) uses ALL blockers, not just in-scope.** A bead can be
  blocked by one outside the epic. The structural metrics (PageRank/betweenness/critical
  path/cycles) stay on the in-scope closed graph — consistent with V3.
- **`now` seam** on `Server` (`now func() time.Time`, defaults `time.Now`) so the stale
  threshold (14d) is deterministic in tests — mirrors the existing `shutdown` seam.
- **Bars sized in Go** (`Width` 0–100 on each ranked bead), so the template stays dumb.
- Insights computes from raw `[]bd.Issue` (labels, timestamps) — not `forest.Bead`, which
  drops both. `handleInsights` lists once, builds the forest for scope, reuses the issues.
- Extract `synFor(repo)` (the `syn.Project = repo.Name` line) so `buildForest` and
  `handleInsights` share it without a double List.

## Plan

1. **`synFor` helper** — pull the two-line synthesis-labeling out of `buildForest`.
2. **`now` seam** — add field to `Server`, default in `New`.
3. **Model + `insightsModel`** in `server.go`: `insightsView{listView; Insights}`,
   `insights`, `triageCounts`, `rankedBead{forest.Bead; Score; Width}`, `labelCount`.
4. **`GET /insights`** route + `handleInsights` (mirrors `handleGraph`).
5. **Triage classify helpers**: `ready`/`blocked`/`stale` over scoped beads using a
   full-repo status map + the bead's blocks deps.
6. **`insights` template** — six panels, reusing `.vt` view-toggle + pane-head markup.
7. **Insights button** added to list/board/graph view-toggles.
8. **CSS** — minimal panel grid + score bars, token-based.
9. **Tests** (`server_test.go`, httptest): renders+toggle, triage counts (with `now`
   seam), influence leaderboard order, critical path, cycle warning, label health +
   untagged, epic-scoped, no-repo empty pane.

## Files

| File | Change |
|---|---|
| `internal/server/server.go` | `synFor`, `now` seam, insights model + handler + route + triage helpers |
| `web/templates/partials.html` | `insights` template; Insights button in 3 toggles |
| `web/static/app.css` | panel grid + bars (minimal) |
| `internal/server/server_test.go` | stub UpdatedAt/Labels already on bd.Issue; insights tests |

## Acceptance

`make audit` green; `/insights` (and `?epic=`) renders the six panels with correct
computed values for a fixture repo; triage counts, leaderboard order, critical path,
cycle warning, and label health all asserted from rendered HTML.
