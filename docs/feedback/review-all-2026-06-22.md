# /review all — strand

Run id: `2af4cc879761` · 2026-06-22 · 21 project-scope linters (skipped `sqlite`, `tx-boundary` — no DB/transactions)

**23 findings: 4 action · 19 borderline. 7 linters clean.**

Each row links its per-linter report under `docs/feedback/review/<linter>-repo.md`.

| linter | findings | a/b | top finding |
|---|---|---|---|
| **zero-sentinel** | 3 | 1/2 | `forest.go:169` optional-value-without-pointer **(action)** |
| **api-surface** | 2 | 1/1 | `graph.go:33` exported-but-unreferenced **(action)** |
| **change-smells** | 2 | 1/1 | `forest.go:83` primitive-obsession (bead status = bare string) **(action)** |
| **domain-vocab** | 2 | 1/1 | `server.go:992` inline-func-type-repeated **(action)** |
| errors-design | 2 | 0/2 | `server.go:1163` boundary-leak-to-client |
| json-shape | 2 | 0/2 | `bd/client.go:64` time-format-drift |
| solid | 2 | 0/2 | `server.go:992` isp-caller-uses-subset |
| truthful-names | 2 | 0/2 | `forest.go:75` terminology-drift |
| arch | 1 | 0/1 | `server.go:540` coupling-hotspot |
| concurrency-safety | 1 | 0/1 | `registry.go:124` lock-held-during-io |
| goroutine-lifecycle | 1 | 0/1 | `main.go:74` goroutine-no-owner |
| pointer-value | 1 | 0/1 | `server.go:309` small-struct-by-pointer |
| slice-map | 1 | 0/1 | `web/embed.go:43` boundary-returns-internal-backing |
| test-effectiveness | 1 | 0/1 | `server_test.go:887` test-without-assertion |
| alloc-bounds | 0 | — | clean |
| conversion-drift | 0 | — | clean |
| ctx-value | 0 | — | clean |
| io-parallel | 0 | — | clean (bd calls serialized by design — single-writer Dolt) |
| n-plus-one | 0 | — | clean |
| test-tables | 0 | — | clean |
| vestige-pair | 0 | — | clean |

## The 4 action findings

1. **`internal/forest/forest.go:83` — primitive-obsession (change-smells).** Bead status is a bare-string vocabulary with literals duplicated across `forest.go` and `server.go`; no named type, no single owner. Highest-leverage fix.
2. **`internal/forest/forest.go:169` — zero-sentinel.** Optional value carried as a plain zero instead of a pointer/ok — a real value and "absent" collapse.
3. **`internal/graph/graph.go:33` — api-surface.** Exported symbol with no references — trim the surface or document intent.
4. **`internal/server/server.go:992` — domain-vocab.** Repeated inline func type that wants a named domain type (also surfaced by `solid` — see hotspot).

## Cross-linter hotspots

_Locations cited by 2+ linters. One fix may close multiple findings._

| # linters | location | linter:finding:rule |
|-----------|----------|---------------------|
| 2 | `internal/server/server.go:992` | domain-vocab:F1:inline-func-type-repeated, solid:F1:isp-caller-uses-subset |
| 2 | `internal/server/server.go:62` | api-surface:F2:single-impl-interface, solid:F2:interface-with-one-impl |

Both hotspots sit on `internal/server/server.go` — the same file `arch` (coupling-hotspot), `change-smells` (divergent-change), `pointer-value`, and `errors-design` also touch. server.go is the project's gravity well: it absorbs every V1–V4 view axis. The borderline `divergent-change` finding proposes the eventual cut (carve per-view model+build helpers into `board.go`/`graph.go`/`insights.go` when V5 lands).

## Next

`/assess-feedback <linter> --run-id=2af4cc879761` rates a linter's findings (six outcomes/finding, accept-ratio logged).
