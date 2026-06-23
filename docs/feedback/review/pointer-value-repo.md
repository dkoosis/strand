# pointer-value — strand (project scope)

RUN_ID: 2af4cc879761 · mode: report (no tree changes)

## Summary

strand is clean on pointer-vs-value. The pointer choices that exist are all justified by the linter's own gates:

- `*bd.Issue` returns (`Show`, `Update`, `Claim`, `Close`, `Create`, `firstIssue`) — `Issue` is a large struct (15 fields: 7 strings, a `[]string`, two `time.Time`, several ints — well over 64 bytes), and the `(nil, nil)` outcome is a **documented nil-meaningful contract** ("bd succeeded but emitted no issue", write.go:166-172). Both gates (large struct, meaningful nil) say drop.
- `*registry.Registry` / `*server.Server` — carry a `sync.Mutex` and have identity; correctly pointer.
- `CreateOpts.Priority *int` (write.go:93) — nil explicitly means "leaves bd's default" (the absent-vs-zero distinction the spec says *not* to flag).
- `*forest.Bead` is the only debatable case (below). No `[]*T` slices, no `NewT() *T` small-value constructors, no `*Point`-style primitive-struct params exist in the tree.

One borderline finding follows. No action-tier findings.

---

### 1. [F1] `internal/server/server.go:309` — small-struct-by-pointer

**Diagnosis.** `pivotField.value` is `func(*forest.Bead) string` — an accessor that only reads one field of a `Bead`, never mutating, never nil-checking. Each accessor (server.go:319-321) returns one field; the sole call site passes `&beads[i]` while iterating a slice (server.go:419).

**Why.** `forest.Bead` is a plain value type (forest.go:18-25): six fields, five strings + one int, no methods, no pointer-receiver constraint, no nil semantics. A function that reads a single field has no reason to take `*Bead` over `Bead` — the pointer adds an indirection and a notional nil hazard for an accessor that can't handle nil. Whether to flag turns on size: at ~88 bytes (5×16-byte string headers + 8-byte int) `Bead` sits above the linter's 64-byte by-value comfort line, which is why this is borderline rather than action — passing the pointer dodges an 88-byte copy per bead during column bucketing.

**Evidence.** `internal/server/server.go:309`:
```go
	value func(*forest.Bead) string
```
`internal/server/server.go:319` (representative accessor — reads one field, no mutation, no nil use):
```go
	{"status", []boardColumn{{Key: statusOpen, Label: "open"}, {Key: statusInProgress, Label: "in progress"}, {Key: statusBlocked, Label: "blocked"}, {Key: statusClosed, Label: "closed"}}, func(b *forest.Bead) string { return b.Status }},
```
`internal/server/server.go:419` (call site, slice iteration):
```go
		key := field.value(&beads[i])
```
`internal/forest/forest.go:18-25` (the value type — no methods, no nilable fields):
```go
type Bead struct {
	ID       string
	Title    string
	Status   string
	Priority int
	Type     string
	Assignee string
}
```

**Fix.** Optional. Changing the signature to `func(forest.Bead) string` and the call to `field.value(beads[i])` reads cleaner and removes the nil hazard from an accessor that can't honor it. But `Bead` is over the 64-byte line, so the current pointer also has a defensible copy-avoidance rationale; leaving it is fine. If `Bead` later shrinks (e.g. fields become enums/ints), revisit — then it's a clear value.

**Tier.** borderline
