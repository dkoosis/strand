# conversion-drift — strand (RUN_ID 2af4cc879761)

**Verdict: 0 findings (action=0 borderline=0)**

## Why zero

`conversion-drift` is **diff-scoped by design** — it reviews changed type-conversion
helpers near boundaries (encode/decode, marshal/unmarshal, DB/driver, JSON). Its own
spec: "Don't run this against full repo — it's diff-scoped."

For this run there is **no diff**:

- `git rev-parse HEAD origin/main` → both `163908bbe310618631771932f4b2878dd981782e`.
- `git diff --name-only origin/main...HEAD -- '*.go'` → empty.
- Working tree: only `M .beads/interactions.jsonl` (ledger) and untracked `docs/feedback/`
  (this review's own output). No `*.go` changes, no `go.mod` dep bump.

No changed conversion helper → nothing for this linter to flag.

## Surface check (for the record)

strand is a Go web app that shells out to the `bd` CLI; it has **no DB driver, no
`sql.Null*`, no custom `Scan`/`Value`/`MarshalJSON`/`UnmarshalJSON` methods**:

- `rg --type go 'func.*(MarshalJSON|UnmarshalJSON|ToSQL|FromSQL|\) Value\(|\) Scan\()'`
  over `internal/ cmd/ web/` → no matches.
- All JSON handling is plain `json.Unmarshal` into structs (`internal/bd/client.go`,
  `internal/registry/registry.go`, `internal/server/server.go`). No zero/empty/nil
  remapping at the boundary.
- The one persistence pair — `Registry.load`/`saveLocked`
  (`internal/registry/registry.go:215-252`) — is a symmetric
  `json.Unmarshal` / `json.MarshalIndent` round-trip with no zero-mapping logic.

Even if treated as whole-repo, the codebase has no helper of the class this linter
guards. Result stands at zero.
