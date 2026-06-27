package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/dkoosis/strand/internal/bd"
)

// pulseIssues spans every status the masthead pulse counts, plus a human-gated
// open bead (the ◆ waiting cut) and a closed + a deferred bead the strand drops
// — so the cut tests prove the pulse reaches work the normal views never show.
var pulseIssues = []bd.Issue{
	{ID: "demo-root", Title: "DEMO", IssueType: "epic", Status: "open"},
	{ID: "demo-1", Parent: "demo-root", Title: "Open task", Status: "open", Priority: new(2)},
	{ID: "demo-2", Parent: "demo-root", Title: "Active task", Status: "in_progress", Priority: new(1)},
	{ID: "demo-3", Parent: "demo-root", Title: "Stuck task", Status: "blocked", Priority: new(0)},
	{ID: "demo-4", Parent: "demo-root", Title: "Needs a call", Status: "open", Priority: new(2), Labels: []string{"human"}},
	{ID: "demo-5", Parent: "demo-root", Title: "Done task", Status: "closed", Priority: new(2)},
	{ID: "demo-6", Parent: "demo-root", Title: "Parked task", Status: "deferred", Priority: new(3)},
}

// TestPulseStripRenders pins the masthead spread: every glyph cell, the
// status-line counts (open counts the epic root too, matching bd stats), and the
// waiting ◆ off the human label.
func TestPulseStripRenders(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: pulseIssues})
	body := do(t, srv, "/").Body.String()

	for _, want := range []string{
		`class="pulse"`,
		`data-filter="waiting"`, `data-filter="open"`, `data-filter="in_progress"`,
		`data-filter="blocked"`, `data-filter="closed"`, `data-filter="deferred"`,
		"◆", "○", "◐", "●", "✓", "❄",
		`title="Waiting on you — decisions &amp; reviews: 1"`,
		`title="Open: 3"`,
		`title="In progress: 1"`,
		`title="Blocked: 1"`,
		`title="Closed: 1"`,
		`title="Deferred: 1"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("masthead pulse missing %q", want)
		}
	}
}

// TestPulseZeroCellDisabled checks an empty status renders a dimmed, non-clickable
// cell (no closed/deferred beads in this strand → those cells disable).
func TestPulseZeroCellDisabled(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: sampleIssues})
	body := do(t, srv, "/").Body.String()
	for _, want := range []string{
		`title="Closed: 0" aria-label="Closed: 0" disabled`,
		`title="Deferred: 0" aria-label="Deferred: 0" disabled`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("zero pulse cell not disabled: missing %q", want)
		}
	}
}

// TestPulseCutsListBeads checks each cut lists exactly its status. The closed and
// deferred cuts prove the uncached --status path surfaces beads the strand drops.
func TestPulseCutsListBeads(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: pulseIssues})

	cases := []struct {
		filter, title, want string
		absent              []string
	}{
		{"blocked", "Blocked", "Stuck task", []string{"Open task", "Done task", "Active task"}},
		{"in_progress", "In progress", "Active task", []string{"Stuck task", "Done task"}},
		{"waiting", "Waiting on you", "Needs a call", []string{"Open task", "Active task"}},
		{"closed", "Closed", "Done task", []string{"Open task", "Stuck task"}},
		{"deferred", "Deferred", "Parked task", []string{"Open task", "Done task"}},
	}
	for _, c := range cases {
		t.Run(c.filter, func(t *testing.T) {
			rec := do(t, srv, "/list?filter="+c.filter)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /list?filter=%s = %d, want 200", c.filter, rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `class="lp-name">`+c.title+`<`) {
				t.Errorf("filter %s: pane head not titled %q", c.filter, c.title)
			}
			// A flat cut clears story/epic scope so afterSwap keeps the filter.
			if !strings.Contains(body, `data-view="list" data-story="" data-epic=""`) {
				t.Errorf("filter %s: flat pane head missing cleared scope", c.filter)
			}
			if !strings.Contains(body, c.want) {
				t.Errorf("filter %s: want bead %q in list", c.filter, c.want)
			}
			for _, a := range c.absent {
				if strings.Contains(body, a) {
					t.Errorf("filter %s: %q leaked into the cut", c.filter, a)
				}
			}
		})
	}
}

// TestPulseFragment checks the /pulse fragment re-renders just the cells (the
// refreshList swap target), with no surrounding page chrome.
func TestPulseFragment(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: pulseIssues})
	rec := do(t, srv, "/pulse")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /pulse = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-filter="blocked"`) || !strings.Contains(body, `title="Blocked: 1"`) {
		t.Errorf("/pulse fragment missing the blocked cell:\n%s", body)
	}
	if strings.Contains(body, "<header") || strings.Contains(body, `class="masthead"`) {
		t.Error("/pulse leaked page chrome — should be the cells only")
	}
}

// TestPulseEmptyCut checks a cut with no matching beads renders the empty note,
// not a broken table.
func TestPulseEmptyCut(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: sampleIssues}) // no closed beads
	body := do(t, srv, "/list?filter=closed").Body.String()
	if !strings.Contains(body, "No beads in this status.") {
		t.Errorf("empty closed cut: want empty note, got:\n%s", body)
	}
}
