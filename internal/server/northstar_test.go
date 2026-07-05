package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dkoosis/strand/internal/registry"
	"github.com/dkoosis/strand/internal/strand"
	"github.com/dkoosis/strand/web"
)

// writeMini drops a north-star-mini.md into a fresh repo dir and returns the dir.
func writeMini(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "north-star-mini.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write mini: %v", err)
	}
	return dir
}

// The north-star-mini.md parse tests moved to internal/strandmd (st-w39) — the
// format rules now live in the file-format package. This file keeps only the
// server-level flag-wins-over-file precedence test below.

// TestMastheadReadsFileThenFlagOverrides: synFor reads the repo's mini when no
// --northstar flag is set, and the flag (a non-empty seeded NorthStar) wins.
func TestMastheadReadsFileThenFlagOverrides(t *testing.T) {
	dir := writeMini(t, "from the file\n")
	repo := registry.Repo{Name: "x", Path: dir}
	tmpl, err := web.Templates()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	srcFor := func(registry.Repo) IssueSource { return &stubBD{} }
	reg := registry.InMemory(repo)

	noFlag := New(srcFor, reg, tmpl, web.Static(), strand.Synthesis{})
	if got := noFlag.synFor(repo).NorthStar; got != "from the file" {
		t.Errorf("no-flag masthead = %q, want file content", got)
	}

	withFlag := New(srcFor, reg, tmpl, web.Static(), strand.Synthesis{NorthStar: "from the flag"})
	if got := withFlag.synFor(repo).NorthStar; got != "from the flag" {
		t.Errorf("flag did not override file: %q", got)
	}
}
