package web

import (
	"slices"
	"testing"

	"github.com/dkoosis/strand/internal/bd"
)

// TestBeadTypesDerivesFromIssueTypes pins the create-form dropdown to bd's closed
// set: the helper must stringify bd.IssueTypes in order, with no hand-maintained
// list to drift (the st-w2r / F1 hazard). A regression to a literal slice that
// drops a kind — as the original {task,bug,feature,epic} dropped story+chore —
// fails here.
func TestBeadTypesDerivesFromIssueTypes(t *testing.T) {
	got := beadTypes()

	want := make([]string, len(bd.IssueTypes))
	for i, it := range bd.IssueTypes {
		want[i] = string(it)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("beadTypes() = %v, want %v (must mirror bd.IssueTypes)", got, want)
	}

	// The drift that prompted the bead: story and chore were missing.
	for _, kind := range []string{"story", "chore"} {
		if !slices.Contains(got, kind) {
			t.Errorf("dropdown is missing %q — drifted from bd.IssueTypes", kind)
		}
	}
}
