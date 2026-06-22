// Package graph computes dependency-graph metrics over a set of beads, in
// process. It replaces the dropped external `bv` CLI (spec D7 revised, O8
// dissolved): strand owns these algorithms via gonum/graph rather than shelling
// out. `bv` is attributed only as the inspiration for *which* metrics matter.
//
// The package is deliberately pure — it takes nodes and edges as plain data and
// returns metrics. It never touches bd; the caller fetches the graph (internal/bd
// Deps) and hands it in. That keeps the metric math unit-testable against fixture
// DAGs with known values, no shell required.
//
// Edge direction is canonical: an edge {Dependent, Dependency} means "Dependent
// is blocked by Dependency" — it points from the dependent to the thing it needs.
// So a foundational bead (many beads depend on it) collects many in-links and
// scores high on PageRank/authority, which is the importance reading we want.
package graph

import (
	"sort"

	"gonum.org/v1/gonum/graph/network"
	"gonum.org/v1/gonum/graph/simple"
	"gonum.org/v1/gonum/graph/topo"
)

// Edge is one directed dependency: Dependent is blocked by Dependency.
type Edge struct {
	Dependent  string
	Dependency string
}

// Metrics holds the per-node graph metrics, each keyed by bead ID, plus the
// graph-wide cycle and critical-path findings.
type Metrics struct {
	PageRank     map[string]float64 // importance: foundational beads rank high
	Betweenness  map[string]float64 // bottleneck: beads many chains route through
	Hub          map[string]float64 // HITS hub: depends on many important beads
	Authority    map[string]float64 // HITS authority: depended-upon by many hubs
	Depth        map[string]int     // longest dependency chain starting at the bead
	Cycles       [][]string         // dependency cycles (SCCs >1 node, or self-loops)
	CriticalPath []string           // the single longest dependency chain
}

// damping and tolerance for the iterative metrics. Standard PageRank damping;
// tolerances tight enough for a few-hundred-node bead graph to converge fast.
const (
	pageRankDamp = 0.85
	pageRankTol  = 1e-6
	hitsTol      = 1e-6
)

// Compute builds the DAG from nodes + edges and returns its metrics. Node IDs
// not present in nodes but referenced by an edge are added, so the graph is
// always closed over its edges. Empty input yields zeroed (non-nil) maps.
func Compute(nodes []string, edges []Edge) Metrics {
	g := simple.NewDirectedGraph()
	id := newIDMap()

	// Register every node first so isolated beads (no edges) still appear.
	for _, n := range nodes {
		g.AddNode(simple.Node(id.of(n)))
	}
	for _, e := range edges {
		f, t := id.of(e.Dependent), id.of(e.Dependency)
		ensure(g, f)
		ensure(g, t)
		if f == t {
			continue // self-loop: surfaced as a cycle below, never an edge
		}
		// Skip a duplicate edge; SetEdge would panic on a re-add of the same pair.
		if g.HasEdgeFromTo(f, t) {
			continue
		}
		g.SetEdge(simple.Edge{F: simple.Node(f), T: simple.Node(t)})
	}

	m := Metrics{
		PageRank:    map[string]float64{},
		Betweenness: map[string]float64{},
		Hub:         map[string]float64{},
		Authority:   map[string]float64{},
		Depth:       map[string]int{},
	}

	// gonum's matrix-based metrics panic on a node-less graph; nothing to compute.
	if g.Nodes().Len() == 0 {
		return m
	}

	for k, v := range network.PageRank(g, pageRankDamp, pageRankTol) {
		m.PageRank[id.name(k)] = v
	}
	for k, v := range network.Betweenness(g) {
		m.Betweenness[id.name(k)] = v
	}
	for k, ha := range network.HITS(g, hitsTol) {
		m.Hub[id.name(k)] = ha.Hub
		m.Authority[id.name(k)] = ha.Authority
	}

	m.Cycles = cyclesOf(g, id, selfLoops(edges))
	m.Depth, m.CriticalPath = depths(g, id)

	return m
}

// ensure adds a node to the graph if an edge referenced an ID not in the node
// list. Without this, SetEdge on an unknown endpoint would create a bare node
// with no entry in the ID map's reverse direction — but id.of already registers
// it, so this only guards the graph side.
func ensure(g *simple.DirectedGraph, n int64) {
	if g.Node(n) == nil {
		g.AddNode(simple.Node(n))
	}
}

// cyclesOf returns the dependency cycles: strongly-connected components with more
// than one node, plus any self-loop (a bead depending on itself), which Tarjan
// reports only as a singleton SCC. Each cycle's IDs are sorted for deterministic
// output.
func cyclesOf(g *simple.DirectedGraph, id *idMap, self map[string]bool) [][]string {
	var out [][]string
	for _, comp := range topo.TarjanSCC(g) {
		if len(comp) < 2 {
			continue
		}
		ids := make([]string, len(comp))
		for i, n := range comp {
			ids[i] = id.name(n.ID())
		}
		sort.Strings(ids)
		out = append(out, ids)
	}
	for s := range self {
		out = append(out, []string{s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// selfLoops collects beads that depend on themselves — a degenerate cycle Tarjan
// won't flag as a multi-node SCC.
func selfLoops(edges []Edge) map[string]bool {
	s := map[string]bool{}
	for _, e := range edges {
		if e.Dependent == e.Dependency {
			s[e.Dependent] = true
		}
	}
	return s
}

// depths computes, for each node, the length of the longest dependency chain
// starting at it (the bead itself counts as 1), and returns the single longest
// chain as the critical path. It is a memoized DFS with a visiting guard so a
// cyclic graph terminates (a back-edge contributes 0) rather than looping.
func depths(g *simple.DirectedGraph, id *idMap) (map[string]int, []string) {
	memo := map[int64]int{}
	next := map[int64]int64{}
	visiting := map[int64]bool{}

	var walk func(n int64) int
	walk = func(n int64) int {
		if d, ok := memo[n]; ok {
			return d
		}
		if visiting[n] {
			return 0 // back-edge: don't recurse into the cycle
		}
		visiting[n] = true
		best, bestNext := 0, int64(-1)
		it := g.From(n)
		for it.Next() {
			s := it.Node().ID()
			// Tie-break successors on ID name so the critical path is stable
			// across runs (gonum's From() iteration order is nondeterministic).
			if d := walk(s); d > best || (d == best && (bestNext < 0 || id.name(s) < id.name(bestNext))) {
				best, bestNext = d, s
			}
		}
		visiting[n] = false
		memo[n] = 1 + best
		next[n] = bestNext
		return memo[n]
	}

	depth := map[string]int{}
	bestStart, bestLen := int64(-1), 0
	nodes := g.Nodes()
	for nodes.Next() {
		n := nodes.Node().ID()
		d := walk(n)
		depth[id.name(n)] = d
		// Tie-break on ID name for a stable critical path across runs.
		if d > bestLen || (d == bestLen && bestStart >= 0 && id.name(n) < id.name(bestStart)) {
			bestStart, bestLen = n, d
		}
	}

	var path []string
	for n := bestStart; n >= 0; n = next[n] {
		path = append(path, id.name(n))
	}
	return depth, path
}

// idMap assigns each string bead ID a stable int64 (gonum nodes are int64) and
// maps back. IDs are handed out in first-seen order.
type idMap struct {
	fwd map[string]int64
	rev map[int64]string
	n   int64
}

func newIDMap() *idMap {
	return &idMap{fwd: map[string]int64{}, rev: map[int64]string{}}
}

func (m *idMap) of(s string) int64 {
	if v, ok := m.fwd[s]; ok {
		return v
	}
	v := m.n
	m.n++
	m.fwd[s] = v
	m.rev[v] = s
	return v
}

func (m *idMap) name(v int64) string { return m.rev[v] }
