# slice-map — repo review

RUN_ID: 2af4cc879761 · scope: project · mode: report

Slice/map boundary semantics over `internal/`, `cmd/`, `web/`. The codebase is
unusually disciplined here: `Registry.Repos` copies before returning under lock;
`boardColumns` does a full `append([]boardColumn(nil), field.Seeds...)` copy of the
package-var seeds; per-column `Beads` are built on fresh slices; capacity hints
(`make([]T, 0, n)`) are present at every loop with a known bound; the bd-client
`append(args, "--json")` patterns build fresh local literals consumed immediately
by `run`, never retained or shared; `squarify` sub-slices (`cells[i:i+n]`,
`cells[:n]`) are read-only views passed to pure functions with no append-aliasing;
no map-grow-during-iter. One borderline boundary remains.

---

### 1. [F1] `web/embed.go:43` — boundary-returns-internal-backing

**Diagnosis.** The `beadTypes` template helper returns the package-level slice var
directly, handing every caller a reference to the same backing array.

**Why.** A returned slice is a view. A consumer that re-slices and appends, or
writes an index, mutates the one shared `beadTypes` for the whole process. Here the
sole consumer is an `html/template` FuncMap, where execution treats the slice as
read-only, so the bug can't fire today — but the boundary is unguarded, and a future
non-template caller of the same helper (or a refactor that ranges-and-appends) would
silently corrupt the create-form's type list. Contrast `priorities()` two functions
below, which builds a fresh slice per call.

**Evidence.** `web/embed.go`:
```
43		"beadTypes":   func() []string { return beadTypes },
...
47	var beadTypes = []string{"task", "bug", "feature", "epic"}
```

**Fix.** Either return a clone — `func() []string { return slices.Clone(beadTypes) }`
— or, since the value never changes, document the read-only contract on the var
(`// beadTypes is read-only; callers MUST NOT mutate.`). The clone is cheap (4
strings, built once per template execution) and removes the footgun outright.

**Tier.** borderline
