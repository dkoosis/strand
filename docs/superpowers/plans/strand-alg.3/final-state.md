# strand-alg.3 — final state

**Shape:** standard · **Profile:** craft · **Authority:** ship (dk: "just do it")
**Classification:** defaulted-no-metadata → standard.

## Delivered

V4 insights/stats view — a fourth scope view (Table/Board/Graph → **Insights**) rendering
six Tufte panels over strand's own in-process gonum metrics (`internal/graph`), never bv:

1. Triage counts — total / ready / blocked / in-progress / open / stale
2. Influence — top-N PageRank
3. Bottlenecks — top-N betweenness
4. Critical path — longest dependency chain, ordered
5. Cycles — dependency cycles or all-clear
6. Label health — label distribution over open beads + untagged warning

## Plan vs actual

Followed plan.md. Additions surfaced during build:
- **`actionable()`** — drops epic-type beads from the dashboard (containers, not work).
  The forest folds the epic node into its own scope; the other views show it, but a
  triage dashboard counting an epic as "ready" is misleading. Documented divergence.
- **`blockerCounts` absent-blocker rule** — `bd list` omits closed beads (verified), so a
  blocks-dep whose target isn't in the live list is treated as resolved, not unmet.
- **Leaderboards gated on edge presence** — PageRank gives every node a positive base
  rank, so with no deps a ranking is noise; show Influence/Bottleneck only with edges.
- **Status constants** (statusOpen/InProgress/Closed/Deferred) — to satisfy goconst once
  the new code pushed "open" past the literal threshold.

## Files

- `internal/server/server.go` — `now` seam, `synFor`, route, `handleInsights`,
  `insightsModel` + helpers (triage/leaderboard/labelHealth/beadPath/actionable), consts
- `web/templates/partials.html` — `insights` + `leaderboard` templates; Insights button in 3 toggles
- `web/static/app.css` — panel grid + score bars
- `internal/server/server_test.go` — 11 tests (unit helpers + httptest fragment)

## Verification

- `make audit` GREEN (vet, golangci-lint, race tests, jscpd, govulncheck, nilaway) → `stage-0-audit-clean.log`
- Tests: TestTriageCounts, TestTriageAbsentBlockerIsResolved, TestIsStale, TestLeaderboard,
  TestLeaderboardEmptyWithoutEdges, TestLabelHealth, TestBeadPath, TestInsightsFragmentRenders,
  TestInsightsScopedToEpic, TestInsightsCycleWarning, TestInsightsNoRepo — all pass.

## Escalated / caveats

- **Live /insights hangs on whole-region scope** — filed **strand-d6f (P1)**. This is a
  PRE-EXISTING defect: `/graph` hangs identically on pristine main (verified with a fresh
  main-branch binary). Root cause is `bd.Client.Deps` passing multiple positional ids to
  `bd dep list` (which rejects them) plus the request deadline not aborting. V4 inherits the
  exact V3 path; it is not a regression. The view itself is proven correct by the httptest
  suite (stubbed source). dk decides whether to merge before or after d6f lands.

## north_star_answer
(stop-at-PR; recenter deferred to dk at review — the view serves "see structure, decide
what's next" directly; the live-hang is the gating risk, tracked as d6f.)
