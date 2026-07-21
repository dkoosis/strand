package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkoosis/strand/internal/bdcounts"
)

// pointCounts writes a counts.json holding one row for demoRepo and points the
// server's reader at it, so the pulse resolves from the shared cache (st-p1f).
func pointCounts(t *testing.T, srv *Server, row string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "counts.json")
	body := `{"` + demoRepo.Path + `":` + row + `}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write counts.json: %v", err)
	}
	srv.counts = bdcounts.NewReaderAt(path)
}

// TestPulseReadsCountsCache pins the st-p1f contract: when counts.json has a row for
// the active repo, the masthead renders those six buckets verbatim — NOT the bd
// stub's spread. The stub carries the pulseIssues shape (open 2, in_progress 1, …);
// the cache row deliberately differs so a pass proves the cache won, not bd.
func TestPulseReadsCountsCache(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: pulseIssues})
	pointCounts(t, srv, `{"bh":4,"bo":7,"bw":2,"bb":3,"bcl":9,"bdf":5,"ts":1}`)

	body := do(t, srv, "/").Body.String()
	for _, want := range []string{
		`title="Waiting on you — decisions &amp; reviews: 4"`,
		`title="Open: 7"`,
		`title="In progress: 2"`,
		`title="Blocked: 3"`,
		`title="Closed: 9"`,
		`title="Deferred: 5"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("masthead did not read counts.json: missing %q", want)
		}
	}
	// The bd-stub numbers must NOT appear — proves the cache is the source, not bd.
	if strings.Contains(body, `title="Open: 2"`) {
		t.Error("pulse showed the bd-stub Open count; counts.json should have won")
	}
}

// TestPulseFallsBackToBD pins the fallback leg: a repo absent from counts.json
// resolves from bd exactly as before (the same numbers TestPulseStripRenders pins).
// newTestServer already points counts at a nonexistent file, so this is the default
// path; the explicit assertion documents that the miss degrades gracefully.
func TestPulseFallsBackToBD(t *testing.T) {
	srv := newTestServer(t, &stubBD{issues: pulseIssues})
	// A cache that exists but lists a DIFFERENT repo — demoRepo misses, falls to bd.
	pointCountsForOtherRepo(t, srv)

	body := do(t, srv, "/").Body.String()
	for _, want := range []string{
		`title="Open: 2"`, // bd-derived spread (pulseIssues), not a cache row
		`title="In progress: 1"`,
		`title="Blocked: 1"`,
		`title="Closed: 1"`,
		`title="Deferred: 1"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("fallback pulse missing bd-derived %q", want)
		}
	}
}

// pointCountsForOtherRepo writes a well-formed counts.json that has no row for
// demoRepo, so Lookup misses and the pulse falls back to bd.
func pointCountsForOtherRepo(t *testing.T, srv *Server) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "counts.json")
	body := `{"/some/other/repo":{"bh":1,"bo":1,"bw":1,"bb":1,"bcl":1,"bdf":1,"ts":1}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write counts.json: %v", err)
	}
	srv.counts = bdcounts.NewReaderAt(path)
}
