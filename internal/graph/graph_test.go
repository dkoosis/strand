package graph

import (
	"reflect"
	"testing"
	"time"
)

// diamond is a DAG with a single chokepoint D and a shared leaf E:
//
//	A → B ┐
//	A → C ┘→ D → E
//
// Edges point dependent → dependency. Longest chains start at A (length 4); D is
// the chokepoint every chain routes through; A is depended-upon by nothing.
func diamond() ([]string, []Edge) {
	nodes := []string{"A", "B", "C", "D", "E"}
	edges := []Edge{
		{"A", "B"}, {"A", "C"},
		{"B", "D"}, {"C", "D"},
		{"D", "E"},
	}
	return nodes, edges
}

func TestCriticalPath(t *testing.T) {
	m := Compute(diamond())

	// Both A→B→D→E and A→C→D→E are length 4; the walk takes the lower-ID branch.
	wantPath := []string{"A", "B", "D", "E"}
	if !reflect.DeepEqual(m.CriticalPath, wantPath) {
		t.Errorf("CriticalPath = %v, want %v", m.CriticalPath, wantPath)
	}
}

func TestNoCyclesInDAG(t *testing.T) {
	m := Compute(diamond())
	if len(m.Cycles) != 0 {
		t.Errorf("Cycles = %v, want none", m.Cycles)
	}
}

func TestPageRankRanksFoundationOverLeaf(t *testing.T) {
	m := Compute(diamond())
	if len(m.PageRank) != 5 {
		t.Fatalf("PageRank has %d nodes, want 5", len(m.PageRank))
	}
	// A is depended on by nothing → lowest importance.
	for n, pr := range m.PageRank {
		if n != "A" && pr < m.PageRank["A"] {
			t.Errorf("PageRank[%s]=%g < PageRank[A]=%g; A should be the minimum", n, pr, m.PageRank["A"])
		}
	}
	// E is the shared foundation → it should outrank A.
	if m.PageRank["E"] <= m.PageRank["A"] {
		t.Errorf("PageRank[E]=%g should exceed PageRank[A]=%g", m.PageRank["E"], m.PageRank["A"])
	}
}

func TestBetweennessPeaksAtChokepoint(t *testing.T) {
	m := Compute(diamond())
	top, arg := -1.0, ""
	for n, b := range m.Betweenness {
		if b > top {
			top, arg = b, n
		}
	}
	if arg != "D" {
		t.Errorf("max betweenness at %s (=%g), want D", arg, top)
	}
}

func TestCycleDetected(t *testing.T) {
	// A 3-cycle plus a self-loop; depth must still terminate.
	m := Compute(
		[]string{"X", "Y", "Z", "S"},
		[]Edge{{"X", "Y"}, {"Y", "Z"}, {"Z", "X"}, {"S", "S"}},
	)
	wantCycles := [][]string{{"S"}, {"X", "Y", "Z"}}
	if !reflect.DeepEqual(m.Cycles, wantCycles) {
		t.Errorf("Cycles = %v, want %v", m.Cycles, wantCycles)
	}
}

func TestEmptyGraph(t *testing.T) {
	m := Compute(nil, nil)
	if m.PageRank == nil || m.Betweenness == nil {
		t.Error("metric maps must be non-nil even when empty")
	}
	if len(m.PageRank) != 0 || len(m.CriticalPath) != 0 || len(m.Cycles) != 0 {
		t.Errorf("empty graph should yield empty metrics, got %+v", m)
	}
}

// TestNodesNoEdgesTerminates is the regression for strand-d6f: a scope with
// nodes but zero in-scope edges (every dependency filtered out) once drove
// gonum's HITS to divide by a zero link norm and spin forever, hanging /graph
// and /insights past their request deadline. HITS is gone (api-surface F1), but
// the edgeless-scope path stays pinned: Compute must return promptly and cover
// every node in PageRank.
func TestNodesNoEdgesTerminates(t *testing.T) {
	done := make(chan Metrics, 1)
	go func() { done <- Compute([]string{"A", "B", "C"}, nil) }()

	select {
	case m := <-done:
		if len(m.PageRank) != 3 {
			t.Errorf("PageRank should cover all 3 nodes, got %d", len(m.PageRank))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Compute hung on an edgeless graph (non-convergence regression)")
	}
}

func TestEdgeAddsMissingNode(t *testing.T) {
	// "F" appears only as an edge endpoint, never in the node list.
	m := Compute([]string{"A"}, []Edge{{"A", "F"}})
	if _, ok := m.PageRank["F"]; !ok {
		t.Error("node referenced only by an edge should appear in metrics")
	}
	// A depends on F, so the longest chain is A→F.
	if want := []string{"A", "F"}; !reflect.DeepEqual(m.CriticalPath, want) {
		t.Errorf("CriticalPath = %v, want %v", m.CriticalPath, want)
	}
}
