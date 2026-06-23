# arch — strand (project scope)

RUN_ID: 2af4cc879761 · commit 163908b · linter: arch (report-only)

## Summary

strand is a small, clean, **acyclic** 7-package Go web app over the `bd` CLI. The
dependency topology is healthy on every axis the catalog checks:

- **Conformance** — no `.go-arch-lint.yml` exists, so there are no declared
  layering rules to violate (noted as a gap below, not a violation).
- **Cycles** — `metrics-cycles.txt`: *0 nontrivial SCCs*; import DAG is clean.
- **Coupling** — no package is in the danger zone. Top Ca=3 (`internal/bd`),
  top Ce=5 (`cmd/strand`, the composition root). Heaviest pair is
  `server → registry` at 4 import-files. Nothing approaches the >50-call or
  `Ca≥10 ∧ I≥0.5` thresholds.
- **Surface** — every package exports ≤8 symbols. No pkg-surface-bloat.
- **Structural** — no orphans (every `internal/*` pkg has an importer), no
  god-package, no lazy-package, single module (no reverse-DAG risk).

The dependency arrows all point the right way: `cmd` → everything (instability
1.0), `server` orchestrates (I=0.80), and `bd`/`graph`/`registry` are stable
leaves (I=0.0). This is textbook stable-dependencies shape for an app this size.

One borderline boundary observation follows. The oversized `server.go` (1181 LOC)
is intentionally **not** raised here — `oversized-file` belongs to `/review
clarity` per the arch spec.

Scorecard: Conformance green · Coupling green · Surface green · Pkg-health green ·
Structural green. **Overall: green.**

---

### 1. [F1] `internal/server/server.go:540` — coupling-hotspot

**Diagnosis.** The `internal/server` package has absorbed a whole
bead/dependency **analytics domain** that has no package home of its own. Beyond
HTTP transport (routing, the cross-site guard, render helpers), `server.go`
carries the graph-model projection and the insights computation:
`graphModel`, `blocksEdges`, `metricNodes`, `triage`, `blockerCounts`,
`leaderboard`, `labelHealth`, `untaggedOpen`, `isStale`, `actionable`,
`beadPath`. These are pure functions over `[]forest.Bead` + `[]bd.DepEdge` +
`graph.Metrics` — domain logic, not request handling.

**Why.** This is the reason `server` is both the most efferent internal package
(Ce=4, importing `bd` + `forest` + `graph` + `registry` + `web`) and the
1181-LOC file. The transport layer drags a coupling to `internal/graph` and to
the dep-edge shape that exists only to compute insights. An `internal/insight`
(or `internal/analytics`) package taking the same inputs and returning view
models would let `server` import one focused producer instead of reaching into
`graph` and `bd.DepEdge` directly, and would make the analytics independently
testable without an `httptest` round-trip. The coupling is real but modest
(Ce=4, no cycle, well under danger-zone), and the app is small enough that
co-location is a legitimate choice — hence borderline, not action.

**Evidence.** `internal/server/server.go`:

```
540:func (s *Server) graphModel(ctx context.Context, src IssueSource, v *listView) (string, error) {
...
553:	computed := graph.Compute(ids, compEdges)
554:	gd.Nodes = metricNodes(beads, &computed)
```

```
593:func metricNodes(beads []forest.Bead, m *graph.Metrics) []graphNode {
```

```
578:func blocksEdges(deps []bd.DepEdge, inScope map[string]bool) ([]graph.Edge, []graphEdge) {
```

`deps-tree.txt`: `internal/server -> internal/graph (2 files)` — the only importer
of `internal/graph` in the app, an edge that exists solely for this analytics code.

**Fix.** If `server.go` keeps growing, lift the pure analytics functions
(`graphModel` minus the `http`/`json` shell, `triage`, `leaderboard`,
`labelHealth`, `blockerCounts`, `metricNodes`, `blocksEdges`) into
`internal/insight`, returning plain view-model structs. `server` then imports
`insight` and renders; the `server → graph` and `server → bd.DepEdge` couplings
move behind that seam. No change needed today — flag this when the next insights
feature lands so the package boundary is drawn before, not after, the next
300 LOC accrete.

**Tier.** borderline

---

## Gap (not a finding)

No `.go-arch-lint.yml` is present. With seven packages and a clean DAG today,
nothing is being violated — but there is also nothing pinning the current
direction. The single rule worth codifying is the leaf invariant: `internal/bd`,
`internal/graph`, `internal/registry` must not import `internal/server` or
`internal/forest`. A three-line arch-lint config would turn a future accidental
back-edge from a silent coupling into a CI failure. Optional; the app is small
enough to police by eye for now.
