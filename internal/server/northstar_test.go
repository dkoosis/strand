package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dkoosis/strand/internal/registry"
	"github.com/dkoosis/strand/internal/strand"
	"github.com/dkoosis/strand/web"
)

// writeNorthStar drops a NORTH_STAR.md into a fresh repo dir and returns the dir.
func writeNorthStar(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "NORTH_STAR.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write north star: %v", err)
	}
	return dir
}

// The NORTH_STAR.md parse tests live in internal/strandmd (st-w39, st-y0a) — the
// format rules now live in the file-format package. This file keeps only the
// server-level flag-wins-over-file precedence test below.

// TestMastheadReadsFileThenFlagOverrides: synFor reads the repo's NORTH_STAR.md
// ★ line when no --northstar flag is set, and the flag (a non-empty seeded
// NorthStar) wins.
func TestMastheadReadsFileThenFlagOverrides(t *testing.T) {
	dir := writeNorthStar(t, "★ from the file\n")
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
