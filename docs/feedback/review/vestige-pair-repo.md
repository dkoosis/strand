# vestige-pair — strand (project scope)

RUN_ID: 2af4cc879761
Mode: report (tree unmodified)
Commit: 163908b

## Summary

**0 findings.** No vestige pairs in the workspace.

A vestige pair is an empty (or ≤1-field, zero-method) struct paired with a lone
constructor where both have zero non-test references. Strand has none.

## What was checked

Swept `internal/`, `cmd/`, `web/` for the two halves of the pattern.

**Empty structs (`struct{}`):** zero, anywhere — including `_test.go`.

```
$ rg -n 'struct\{\}|struct \{\}' internal/ cmd/ web/
(no output)
```

**Near-empty structs (≤1 field):** none qualify. Every struct in the tree is
either a multi-field data carrier or a real layout/view type:

- `internal/forest/squarify.go:9` `Rect` — 4 fields (`X, Y, W, H`), the
  treemap geometry used across forest + server views.
- `internal/forest/squarify.go:15` `cell`, `:21` `box` — 2/4-field layout
  working types, both consumed inside `squarify.go`.
- `internal/forest/forest.go:75` `Synthesis` — 2 fields, exported and held by
  `server.Server` (`syn forest.Synthesis`, server.go:90).
- `internal/graph/graph.go:235` `idMap` — 3 fields, used by `Compute`.

No single-field-struct + lone-constructor pair exists.

**Constructors (`func New*` / `func new*`):** all four have live, non-test
callers, so none is orphaned:

- `internal/server/server.go:110` `New` — the package entry point; `IssueSource`
  carries ref_count 23, server wired from `cmd`.
- `internal/forest/forest.go:30` `NewBead` — invoked while `Build`
  (forest.go:87, ref_count 6) assembles the bead list.
- `internal/graph/graph.go:241` `newIDMap` — called at graph.go:56 inside
  `Compute` (ref_count 11).

  ```
  $ rg -n 'newIDMap' internal/graph/
  internal/graph/graph.go:56:	id := newIDMap()
  internal/graph/graph.go:241:func newIDMap() *idMap {
  ```

- `internal/forest/squarify.go:60` `newCells` — called at squarify.go:38 inside
  the squarify layout pass.

  ```
  $ rg -n 'newCells' internal/forest/
  internal/forest/squarify.go:38:	cells, total := newCells(values)
  internal/forest/squarify.go:60:func newCells(values []float64) ([]cell, float64) {
  ```

## Verdict

Healthy workspace. Spec's expected outcome for this case: zero vestiges, no
padding. The reader has nothing to delete here.
