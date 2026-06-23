# truthful-names — strand (repo)

RUN_ID: 2af4cc879761 · commit 163908b · scope: project · mode: report

## Verdict

Names are honest. Across all five packages (`bd`, `forest`, `graph`, `registry`, `server`) every exported symbol carries a doc-comment contract and the body matches it. No receiver-mismatch, no imprecise-function-name, no generic catch-all package, no terminology drift, no file-basename dumping ground. The module path `github.com/dkoosis/strand` names the product. Test names trace to the code they exercise.

Two borderline items below — neither rises to action tier. No findings at action tier.

A note on the `Issue`/`Bead` pair: `bd.Issue` is bd's JSON record; `forest.Bead` is the render-facing projection, and `NewBead` (forest.go:30) documents the boundary explicitly ("the one place that maps bd's field names … so the epic roll-up and the board's single-card refresh can't drift"). This is a deliberate bounded-context split, not `terminology-drift`. Not flagged.

---

### 1. [F1] `internal/forest/forest.go:75` — terminology-drift

**Diagnosis.** The package's domain vocabulary is `Forest → Region → Epic → Bead`. `Synthesis` names a fifth, differently-shaped concept that today carries only two scalar strings.

**Why.** `Synthesis` predicts a process or a computed result ("the synthesis of X and Y"). The body is a plain config carrier — `Project` label + `NorthStar` line — that the human supplies, not something Build synthesizes. A reader meeting `Synthesis` in `Build(issues, syn Synthesis)` expects derived structure and finds two passthrough fields.

**Evidence.** forest.go:75-78:
```go
type Synthesis struct {
	Project   string
	NorthStar string
}
```
The doc-comment (forest.go:71-74) concedes the name is aspirational: "Today it carries the project label and north-star line; when module derivation lands it grows the region-mapping." The name is sized for the future struct, not the present one.

**Fix.** None now — the comment documents the migration intent, which is the rules-file escape hatch ("add a `// Alias` if needed during migration"). Re-judge if the module-derivation layer lands without `Synthesis` growing real synthesis behavior. Until then this is a watch item, not a rename.

**Tier.** borderline

---

### 2. [F2] `internal/graph/graph.go:125` — imprecise-function-name

**Diagnosis.** `ensure` is a verb with no object — it promises nothing checkable. The body ensures one specific thing: a node exists in the graph.

**Why.** A bare `ensure` forces the reader to the body to learn what is ensured. At the two call sites (graph.go:64-65, `ensure(g, f)` / `ensure(g, t)`) the intent — "make sure this node is in the graph" — isn't readable from the name alone. `ensureNode` would make the call sites self-documenting.

**Evidence.** graph.go:125-129:
```go
func ensure(g *simple.DirectedGraph, n int64) {
	if g.Node(n) == nil {
		g.AddNode(simple.Node(n))
	}
}
```
The doc-comment names the real contract ("ensure adds a node to the graph if an edge referenced an ID not in the node list"); the function name drops the object.

**Fix.** Rename `ensure` → `ensureNode` (unexported, two call sites in the same file — grep-clean, no external break).

**Tier.** borderline

---

## Checked and cleared (not flagged)

- `bd.Client.Update/Claim/Close/Create/Comment/Delete` — receiver is the bd client; verbs target bd. Honest controller pattern, each documents its bd subcommand.
- `forest.Build`, `graph.Compute` — pure transforms, names match. `Compute`'s edgeless-HITS guard (graph.go:101) is documented and the name still holds.
- `registry.Open/InMemory/Add/Switch/Rescan` — each does exactly what it says; lock discipline named with `…Locked` suffixes.
- `server` handlers — `handleMove`, `handleDeletePreview` (preview destroys nothing, doc-comment says so), `handleReopen` (status write, documented) all match their bodies. `sameSite`/`guardCrossSite`/`originMatchesHost` are precise.
- `squarify` internals (`orient`, `rowLen`, `placeRow`, `advance`, `worst`) — terse but each names its slice of the squarified-treemap algorithm accurately.
- Test names (`TestDecodeEdgesBatchShape`, `TestNodesNoEdgesTerminates`, etc.) exercise the named function; subtest cases match their descriptions.
