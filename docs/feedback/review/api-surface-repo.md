# api-surface — strand (repo)

RUN_ID: 2af4cc879761 · scope: project · mode: report

2 findings (action=1 borderline=1). Surface is small and intentional: receivers are uniformly pointer across every type (`*Client`, `*Registry`, `*Server`, `*idMap`); the only embeddings (`drawerData`, `rankedBead`, the `*View` structs) are on *unexported* render-data types, so no exported method-set leaks. `bd.Client` is exported with no premature interface in its own package. The two notes below are the only real surface calls.

---

### 1. [F1] `internal/graph/graph.go:33` — exported-but-unreferenced

**Diagnosis.** `Metrics` exports `Hub`, `Authority`, and `Depth`, but no production code outside package `graph` reads them. The only consumer (`internal/server`) reads `PageRank`, `Betweenness`, `Cycles`, `CriticalPath` and nothing else; `Hub`/`Authority`/`Depth` are referenced only by `graph_test.go`.

**Why.** Three exported fields are dead public surface. Worse, `Compute` pays to populate them on every call — `Depth` drives the memoized DFS (which also yields the used `CriticalPath`, so that pass stays), but `Hub`/`Authority` exist *only* to fill these fields: the whole `network.HITS` branch (and its edgeless-graph zero-fill workaround) runs to produce numbers nothing renders. Each `/graph` and `/insights` request computes HITS for the wastebasket.

**Evidence.** `internal/graph/graph.go:33-41`:

```go
type Metrics struct {
	PageRank     map[string]float64 // importance: foundational beads rank high
	Betweenness  map[string]float64 // bottleneck: beads many chains route through
	Hub          map[string]float64 // HITS hub: depends on many important beads
	Authority    map[string]float64 // HITS authority: depended-upon by many hubs
	Depth        map[string]int     // longest dependency chain starting at the bead
	Cycles       [][]string         // dependency cycles (SCCs >1 node, or self-loops)
	CriticalPath []string           // the single longest dependency chain
}
```

Server reads (every `m.` access in `internal/server/server.go`): `m.Cycles` (595), `m.CriticalPath` (601, 715), `m.PageRank` (612, 723), `m.Betweenness` (724), `m.Cycles` (716). No `m.Hub`, `m.Authority`, or `m.Depth`. The only `Hub`/`Authority`/`Depth` reads in the tree are in `internal/graph/graph_test.go` (lines 30-31, 91-92, 112-117, 127-131).

**Fix.** Drop `Hub`, `Authority`, `Depth` from `Metrics` and delete the `network.HITS` branch in `Compute` (lines ~95-113) along with its edgeless zero-fill. `Depth` is the per-node longest-chain map; if you want to keep computing the critical path, keep the internal `depths()` pass but stop exposing its per-node map. Trim the tests that pin the removed fields. If any field is a deliberate "future dashboard panel" hook, say so in a `// reserved for …` godoc line and accept the cost — but today it's unused surface plus a wasted matrix solve.

**Tier.** action

---

### 2. [F2] `internal/server/server.go:62` — single-impl-interface

**Diagnosis.** `IssueSource` has exactly one production implementer, `*bd.Client` (its eleven methods live in `internal/bd/client.go` + `write.go`). The only other "impl" is the `stubBD` test fake. The interface is exported.

**Why.** A single-prod-impl interface is the textbook premature-abstraction smell — extra indirection, harder navigation, the eleven-method set duplicated between the interface decl and `bd.Client`. The rules carve out an exception for interfaces that test isolation "truly demands," and this is close to it: the server resolves a *fresh* source per request through `SourceFunc`, and the tests swap an in-memory `stubBD` per repo path (`repo_test.go:22,43,62`). That swap is real and the godoc documents the seam (spec Q0/D6). So this stays borderline, not action — but it's worth a conscious "yes, keep it" rather than drift.

**Evidence.** `internal/server/server.go:62-74`:

```go
type IssueSource interface {
	List(ctx context.Context, args ...string) ([]bd.Issue, error)
	Deps(ctx context.Context, ids ...string) ([]bd.DepEdge, error)
	Show(ctx context.Context, id string) (*bd.Issue, error)
	Comments(ctx context.Context, id string) ([]bd.Comment, error)
	Update(ctx context.Context, id, field, value string) (*bd.Issue, error)
	Claim(ctx context.Context, id string) (*bd.Issue, error)
	Close(ctx context.Context, id, reason string) (*bd.Issue, error)
	Comment(ctx context.Context, id, text string) error
	Create(ctx context.Context, opts bd.CreateOpts) (*bd.Issue, error)
	DeletePreview(ctx context.Context, id string) (string, error)
	Delete(ctx context.Context, id string) error
}
```

Sole prod wiring, `cmd/strand/main.go:54-56`:

```go
	srcFor := func(repo registry.Repo) server.IssueSource {
		return &bd.Client{Dir: repo.Path, Bin: bdBin}
	}
```

**Fix.** Keep the interface — the per-request `SourceFunc` swap and the in-memory test stub justify it under the rules' test-isolation carve-out, and it's already documented. No change needed beyond awareness. *If* you ever want to shrink it: the read-only handlers (`graphModel`, `insightsModel`, `renderDrawer`, `buildForest`) take the full eleven-method `IssueSource` but call only `List`/`Deps`/`Show`/`Comments` — a narrower read-side interface would shrink what those signatures promise. That's an ISP split, deferred to `/review solid`.

**Tier.** borderline
