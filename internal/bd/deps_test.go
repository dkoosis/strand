package bd

import (
	"reflect"
	"testing"
)

// TestDecodeEdgesBatchShape covers the multi-ID flat record shape — the form
// strand uses in practice when it fetches the whole graph.
func TestDecodeEdgesBatchShape(t *testing.T) {
	out := []byte(`[
		{"issue_id":"a-2","depends_on_id":"a-1","type":"blocks"},
		{"issue_id":"a-2","depends_on_id":"a","type":"parent-child"}
	]`)
	got, err := decodeEdges(out, []string{"a-2", "a-3"})
	if err != nil {
		t.Fatal(err)
	}
	want := []DepEdge{
		{IssueID: "a-2", DependsOnID: "a-1", Type: "blocks"},
		{IssueID: "a-2", DependsOnID: "a", Type: "parent-child"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestDecodeEdgesSingleShape covers bd's other shape: a one-ID query returns the
// dependency *issues*, which we turn into edges from the queried ID.
func TestDecodeEdgesSingleShape(t *testing.T) {
	out := []byte(`[
		{"id":"a-1","title":"foo","dependency_type":"blocks"},
		{"id":"a","title":"epic","dependency_type":"parent-child"}
	]`)
	got, err := decodeEdges(out, []string{"a-2"})
	if err != nil {
		t.Fatal(err)
	}
	want := []DepEdge{
		{IssueID: "a-2", DependsOnID: "a-1", Type: "blocks"},
		{IssueID: "a-2", DependsOnID: "a", Type: "parent-child"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestDecodeEdgesEmpty(t *testing.T) {
	for _, in := range []string{"[]", "  ", ""} {
		got, err := decodeEdges([]byte(in), []string{"a-1", "a-2"})
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != nil {
			t.Errorf("%q: got %+v, want nil", in, got)
		}
	}
}

func TestDecodeEdgesError(t *testing.T) {
	_, err := decodeEdges([]byte(`{"error":"no issues found matching the provided IDs"}`), []string{"x"})
	if err == nil {
		t.Fatal("want error from bd error object")
	}
}
