# zero-sentinel — strand (project scope)

RUN_ID: 2af4cc879761 · mode: report

3 findings (action=1 borderline=2). The codebase is small (2830 LOC non-test) and mostly disciplined about the absent-vs-zero line: the write path uses `*int` / `nil` for optional priority, `isStale` guards `updated.IsZero()`, and map-index reads (`idx[b.ID].UpdatedAt`, `idx[…].Labels`) all feed a tolerant guard (IsZero / range-over-nil / len==0), so they don't conflate. The one real hazard is decode-side bead **priority**: `bd.Issue.Priority` is a plain `int`, so a bead bd omits the field for is indistinguishable from a genuine P0.

---

### 1. [F1] `internal/forest/forest.go:169` — optional-value-without-pointer

**Diagnosis.** `buildEpic` flags an epic as carrying active P0/P1 work via `if members[i].Priority <= 1`. `Priority` is a value `int` (`bd.Issue.Priority int`, `internal/bd/client.go:57`), and the struct's own doc says "Fields bd omits stay at their zero value" (`client.go:51-52`). A bead bd emits with no priority decodes to `Priority == 0`, which satisfies `<= 1` and silently raises the epic's P0/P1 flag.

**Why.** P0 is a real, highest-priority domain value, so zero cannot mean both "P0" and "unset". The write side already distinguishes them — `CreateOpts.Priority *int // nil leaves bd's default` (`internal/bd/write.go:93`) — so the conflation is purely a read-side asymmetry. The day bd changes its JSON to omit a default priority (or a `show` payload drops it), every such bead becomes a false P0/P1 and turns the epic flag on.

**Evidence.**
```go
	for i := range members {
		if members[i].Priority <= 1 {
			e.Flag = true
		}
```
and the field that can't express absence:
```go
	Priority        int       `json:"priority"`
```
(`internal/bd/client.go:57`)

**Fix.** Decode into a field that can express absence on the read path too — `*int` on `bd.Issue.Priority` (nil = bd omitted), or a companion `PriorityValid bool`, and treat only an explicitly-set 0 as P0. If bd's contract guarantees it always emits `priority`, document that guarantee at the struct so the `<= 1` test is provably safe; absent that guarantee, the flag is one dependency bump from lying.

**Tier:** action

---

### 2. [F2] `internal/server/server.go:320` — optional-value-without-pointer

**Diagnosis.** The priority board pivot keys columns by `strconv.Itoa(b.Priority)`, with the `"0"` column labeled `P0`. The same omitted-priority bead that decodes to `Priority == 0` (F1) lands in the P0 column, presented to the user as deliberately top-priority.

**Why.** Same root as F1, separate observable defect at a separate site: here the conflation surfaces as a wrong board placement rather than an epic flag. A bead the operator never prioritized renders in P0 indistinguishably from one they set to P0, and a drag from that column would write `priority=0` back as if confirmed.

**Evidence.**
```go
	{"priority", []boardColumn{{Key: "0", Label: "P0"}, {Key: "1", Label: "P1"}, {Key: "2", Label: "P2"}, {Key: "3", Label: "P3"}, {Key: "4", Label: "P4"}}, func(b *forest.Bead) string { return strconv.Itoa(b.Priority) }},
```

**Fix.** Resolve at the source (F1): once `bd.Issue.Priority` can express absence, route unset beads to an explicit "unprioritized" column (mirroring the `assignee` field's `"unassigned"` column at line 321) rather than folding them into P0.

**Tier:** borderline

---

### 3. [F3] `internal/registry/registry.go:33` — optional-value-without-pointer

**Diagnosis.** `Repo.LastUsed time.Time` is the most-recently-used sort key, but `discover` mints repos with no `LastUsed` (`internal/registry/registry.go:265`: `Repo{Name: filepath.Base(path), Path: path}`), so a discovered-but-never-switched repo carries `LastUsed == time.Time{}`. The zero time means "never used" and the value type can't say so explicitly.

**Why.** Lower severity than F1/F2 because zero here is a coherent domain value — "never used" legitimately sorts oldest, and `sortLocked` relies on real timestamps' `.After(zero)` being true to place never-used repos last (matching the comment at line 187). It's still the optional-value-as-zero shape, and it crosses a persistence boundary: the zero marshals to `"0001-01-01T00:00:00Z"` in `repos.json`. JSON time roundtrips that faithfully, so it's lossless today — but it's the exact `time.Time{}`-on-the-wire footprint the rule guards, and a future store/format change (or a consumer that treats `0001-01-01` as a real date) would expose it.

**Evidence.**
```go
	LastUsed time.Time `json:"last_used"`
```
and the never-used construction:
```go
		out = append(out, Repo{Name: filepath.Base(path), Path: path})
```
(`internal/registry/registry.go:265`)

**Fix.** Use `*time.Time` (nil = never used) so absence is explicit and never serializes a sentinel date, or keep the value type and add a doc note that `discover` intentionally leaves the zero as the "never used" marker that sorts last. The first is the safer boundary contract.

**Tier:** borderline
