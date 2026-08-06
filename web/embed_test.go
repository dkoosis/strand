package web

import (
	"slices"
	"strings"
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

// TestAppJSGuardsSSEWithVisibility pins the st-58m fix: the SSE EventSource in
// app.js must be visibility-managed, not held open unconditionally. Browsers cap
// HTTP/1.1 connections at 6 per host; one always-open /events stream per tab
// exhausts that pool, after which every request (GET /repos, POST /repo, the
// HX-Refresh reload) queues forever and the repo dropdown silently dies.
// Reproduced with playwright: 6 open tabs stall the 6th tab's own dropdown.
// The guard: hidden tabs close their stream (visibilitychange) so only visible
// tabs hold connections. A regression to `const changes = new EventSource(...)`
// at top level fails here.
func TestAppJSGuardsSSEWithVisibility(t *testing.T) {
	js, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("read embedded app.js: %v", err)
	}
	src := string(js)

	if !strings.Contains(src, "EventSource") {
		t.Skip("app.js no longer uses SSE — retire this test with the stream")
	}
	if !strings.Contains(src, "visibilitychange") {
		t.Fatal("app.js opens an EventSource with no visibilitychange lifecycle — " +
			"hidden tabs will hold /events connections and exhaust the browser's " +
			"6-per-host pool (st-58m)")
	}
}
