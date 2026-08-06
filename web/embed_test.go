package web

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/strand"
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

// TestLooseOnlyMapCarriesShadeSteps pins the st-ps1 fix: a repo with no epics
// renders one gray catch-all epic, so app.css needs a per-cell --si shade index
// on every story to make cells distinguishable. The map template must emit
// data-epic="__loose__" on the epic and cycling --si:0..3 on the story cells;
// losing either regresses the loose-only map to a featureless block.
func TestLooseOnlyMapCarriesShadeSteps(t *testing.T) {
	tmpl, err := Templates()
	if err != nil {
		t.Fatalf("Templates() error: %v", err)
	}

	stories := make([]strand.Story, 5)
	for i := range stories {
		stories[i] = strand.Story{ID: fmt.Sprintf("st-%d", i), Name: fmt.Sprintf("story %d", i), Open: 1}
	}
	m := strand.Model{Epics: []strand.Epic{{
		Key:     "__loose__",
		Name:    "strand",
		Color:   "#7a8290",
		Open:    5,
		Stories: stories,
	}}}

	var b strings.Builder
	if err := tmpl.ExecuteTemplate(&b, "map", m); err != nil {
		t.Fatalf("execute map template: %v", err)
	}
	out := b.String()

	if !strings.Contains(out, `data-epic="__loose__"`) {
		t.Error("loose-only map lost its data-epic=\"__loose__\" hook — CSS shade steps won't apply")
	}
	// Steps cycle 0..3; the fifth story wraps back to 0.
	for step, want := range map[string]int{"--si:0": 2, "--si:1": 1, "--si:2": 1, "--si:3": 1} {
		if got := strings.Count(out, step+";"); got != want {
			t.Errorf("story shade var %s appears %d times, want %d", step, got, want)
		}
	}
}

// TestShadeStepCyclesAndClamps pins the helper: steps repeat 0..3 and a
// negative index (defensive; ranges never produce one) clamps to 0.
func TestShadeStepCyclesAndClamps(t *testing.T) {
	for i, want := range []int{0, 1, 2, 3, 0, 1} {
		if got := shadeStep(i); got != want {
			t.Errorf("shadeStep(%d) = %d, want %d", i, got, want)
		}
	}
	if got := shadeStep(-1); got != 0 {
		t.Errorf("shadeStep(-1) = %d, want 0", got)
	}
}
