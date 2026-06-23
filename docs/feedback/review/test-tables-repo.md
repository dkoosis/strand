# test-tables — strand (RUN_ID 2af4cc879761)

Reviewed Go table-tests for structure: one-row tables, per-case branching, unused fields, missing name fields, stale `tt := tt` rescope.

Scope: canonical `internal/**` tree. The `strand-alg.4/internal/**` subtree is a checked-in feature-branch snapshot (out of scope; not double-flagged).

## Inventory

Three real table literals, all in the canonical tree:

- `internal/bd/classify_test.go:13` — `TestClassifyMapsBDMessages`, 6 rows, fields `{name, msg, want}`.
- `internal/server/server_test.go:579` — `TestStatusForError`, 4 rows, fields `{name, err, want}`.
- `internal/server/server_test.go:978` — `TestIsStale`, 4 rows, fields `{name, status, updated, want}`.

All three: ≥3 rows, uniform shape, every field read, a `name` field present, no per-case `if/switch` branching. `go.mod` is `go 1.26.4` and no test uses `tt := tt`, so the 1.22-rescope rule has nothing to flag.

The remaining `*_test.go` loops (`for _, want := range []string{...}`) are substring/membership assertions over literal slices, not struct table-tests — out of rule scope.

## Findings

None.

The table-tests here are textbook: shared shape, named cases, no dead fields, no branching. The one stylistic nit — `TestIsStale` ranges with a bare loop instead of `t.Run(c.name, ...)` while the other two wrap subtests — is below the bar: `c.name` is interpolated into the failure message (`isStale(%s)`), so diagnosability is preserved, and the spec's don't-list explicitly excludes this. Not a finding.
