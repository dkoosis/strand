# pass-plan-review — strand-alg.2 (plan-reviewer, fresh subagent)

Verdict: **APPROVE-WITH-EDITS**. Sound approach, correct DepEdge→graph.Edge and Metrics→node-flag
mappings, faithful to the Table/Board pattern. Fold the three P1s before coding.

## P1 (fold before coding)
1. Name `scopeBeads(&view)` (server.go:268) as the in-scope flattener reuse point — the Board
   uses it; a second hand-rolled walk is quiet divergence.
2. `Deps` is dual-shape: single-ID query synthesizes `IssueID: ids[0]` (client.go:218-220). The
   "drop edges with an out-of-scope endpoint" rule neutralizes stray edges but is load-bearing and
   untested → add a test: an epic scope whose bead `blocks` something outside scope asserts that
   edge is dropped (DAG stays closed).
3. Pin the fixture's flagged IDs: assert the *specific* InCycle / OnPath node IDs (one cycle, one
   longest chain), not "some node" — same rigor as strand-alg.1's fixture tests.

## P2
- PageRank→node-size needs a normalization (min–max / sqrt to a px range) so the largest looms;
  raw PageRank floats (~sub-1) render near-identical. It's the feature, not an afterthought.
- Confirm the `//go:embed` pattern covers `static/vendor/*.js` or the libs 404 at runtime while
  httptest stays green.

## P3
- Scope question: **keep scoped per-epic/region** (matches R4; the three views share `#listPane`
  so they must share scope). Cross-epic-edge invisibility → V3.1 follow-up bead (surface dropped-
  edge count later). Plan already flags for dk — right call.
- Node-tap → drawer reuse, view-toggle third button, `window.cytoscape` guard: all confirmed
  consistent with existing code. No change.
- JSON injection: default to a `data-graph` attribute (dataset read, like Sortable) or a
  `template.JS` value; bare interpolation in a `<script type="application/json">` double-escapes.
