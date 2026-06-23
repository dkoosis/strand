# n-plus-one — strand (repo scope)

RUN_ID: 2af4cc879761 · mode: report · 0 findings (action=0 borderline=0)

## Verdict

No N+1 over `bd` exec. The codebase is structurally immune to the pattern this linter hunts: every `bd` query call sits outside any loop, and the one fan-out shape (dependency edges) is served by a batched, variadic API.

## What was checked

Treating each `bd` invocation as the query/RPC (`internal/bd/client.go:108` `run` → `exec.CommandContext`), the query surface is the `IssueSource` interface (`internal/server/server.go:62`): `List`, `Deps`, `Show`, `Comments`, plus the write methods. I cross-referenced every non-test call site against every `for … range` in the same files:

- `internal/server/server.go` — all 14 `src.*` call sites (lines 457, 463, 547, 678, 705, 909, 932, 943, 952, 962, 972, 1002, 1020, 1061, 1083, 1107, 1119) are outside any range loop. Loop bodies (e.g. lines 567, 581, 745, 795, 863) iterate in-memory slices/maps already fetched — no `bd` exec inside.
- `internal/registry/registry.go` — loops at 170, 179, 263 are pure in-memory / `os.Stat` / `filepath.Glob`; `discover` does not probe each repo with `bd`. The repo selector (`internal/server/repo.go:32`) renders registry data only — it shows no per-repo bead count, so it avoids the classic "list repos, count beads per repo" N+1.

## Why it's clean (by design, not accident)

- The fan-out case — fetching dependency edges for N beads — is batched at the API: `Deps(ctx context.Context, ids ...string)` (`internal/bd/client.go:184`) takes the whole ID set in one `bd dep list --json` call. Both callers pass `ids...` whole:
  - `internal/server/server.go:547` — `deps, err := src.Deps(ctx, ids...)` (graph view)
  - `internal/server/server.go:705` — `if deps, err = src.Deps(ctx, ids...); err != nil {` (insights view)
- Each view handler issues exactly one `List` per request (`internal/server/server.go:678`, `:1119`) then computes everything in memory.
- `Show` / `Comments` appear only in single-bead handlers (`handleBead` :909, `renderDrawer` :932, `writeAndRefresh` :1002) — one ID per request, never iterated.

No preload-candidate gap either: the one repo with a fan-out shape (`bd.Client`) already exposes the batched `Deps` method, and its callers use it.
