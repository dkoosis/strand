package strandmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeStrand writes content to dir/.strand/STRAND.md, creating the dir.
func writeStrand(t *testing.T, dir, content string) {
	t.Helper()
	writeAt(t, filepath.Join(dir, ".strand", "STRAND.md"), content)
}

// writeAt writes content to an arbitrary path, creating parent dirs.
func writeAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestLoadInitsGlobalFromEmbedWhenAbsent pins the claude-init behavior: an absent
// global ~/.strand/STRAND.md is created from the shipped embedded default, no
// error, and the on-disk file matches Default() byte-for-byte.
func TestLoadInitsGlobalFromEmbedWhenAbsent(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()

	ctx, err := Load(home, repo)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	globalPath := filepath.Join(home, ".strand", "STRAND.md")
	got, err := os.ReadFile(globalPath)
	if err != nil {
		t.Fatalf("global file not initialized: %v", err)
	}
	if string(got) != Default() {
		t.Errorf("initialized global does not match Default()")
	}
	if !strings.Contains(ctx.Text, "## Actors") {
		t.Errorf("context missing default ## Actors stub:\n%s", ctx.Text)
	}
	if !strings.Contains(ctx.Text, "epic") {
		t.Errorf("context missing rubric (epic/story/task):\n%s", ctx.Text)
	}
}

// TestLoadBothAbsentReturnsDefaultOnly: with neither file present, the result is
// composed from the embedded default alone and is stable across calls.
func TestLoadBothAbsentReturnsDefaultOnly(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()

	first, err := Load(home, repo)
	if err != nil {
		t.Fatalf("Load #1: %v", err)
	}
	if strings.TrimSpace(first.Text) == "" {
		t.Fatal("default-only context is empty")
	}
	second, err := Load(home, repo)
	if err != nil {
		t.Fatalf("Load #2: %v", err)
	}
	if first.Text != second.Text {
		t.Errorf("default-only context not stable across calls:\n#1: %q\n#2: %q", first.Text, second.Text)
	}
}

// TestLocalActorsOverrideGlobal: when the repo-local file declares ## Actors, the
// local registry replaces the global default stub.
func TestLocalActorsOverrideGlobal(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	writeStrand(t, home, "## Bead Quality Rubric\nRUBRIC\n\n## Actors\n- GlobalOnlyActor\n")
	writeStrand(t, repo, "## Actors\n- LocalProjectManager\n")

	ctx, err := Load(home, repo)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(ctx.Text, "LocalProjectManager") {
		t.Errorf("local Actors not used:\n%s", ctx.Text)
	}
	if strings.Contains(ctx.Text, "GlobalOnlyActor") {
		t.Errorf("global Actors stub leaked past local override:\n%s", ctx.Text)
	}
	if !strings.Contains(ctx.Actors, "LocalProjectManager") {
		t.Errorf("ctx.Actors not resolved to local: %q", ctx.Actors)
	}
}

// TestActorsFromGlobalWhenLocalAbsent: no local file -> Actors come from global.
func TestActorsFromGlobalWhenLocalAbsent(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir() // empty, no .strand
	writeStrand(t, home, "## Bead Quality Rubric\nRUBRIC\n\n## Actors\n- GlobalOnlyActor\n")

	ctx, err := Load(home, repo)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(ctx.Text, "GlobalOnlyActor") {
		t.Errorf("global Actors not used when local absent:\n%s", ctx.Text)
	}
	if !strings.Contains(ctx.Actors, "GlobalOnlyActor") {
		t.Errorf("ctx.Actors not from global: %q", ctx.Actors)
	}
}

// TestActorsFromGlobalWhenLocalHasNoActors: a local file that overlays direction
// but omits ## Actors still falls back to the global registry.
func TestActorsFromGlobalWhenLocalHasNoActors(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	writeStrand(t, home, "## Bead Quality Rubric\nRUBRIC\n\n## Actors\n- GlobalOnlyActor\n")
	writeStrand(t, repo, "## Direction\nLOCAL-DIRECTION\n")

	ctx, err := Load(home, repo)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(ctx.Actors, "GlobalOnlyActor") {
		t.Errorf("Actors should fall back to global when local omits the section: %q", ctx.Actors)
	}
	if !strings.Contains(ctx.Text, "LOCAL-DIRECTION") {
		t.Errorf("local direction missing:\n%s", ctx.Text)
	}
}

// TestDirectionConcatLocalLast: direction text from both layers is present, with
// the local layer appearing after the global one (local last, wins on conflict).
func TestDirectionConcatLocalLast(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	writeStrand(t, home, "## Bead Quality Rubric\nRUBRIC\n\n## Direction\nGLOBAL-DIRECTION\n\n## Actors\n- G\n")
	writeStrand(t, repo, "## Direction\nLOCAL-DIRECTION\n")

	ctx, err := Load(home, repo)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	gi := strings.Index(ctx.Text, "GLOBAL-DIRECTION")
	li := strings.Index(ctx.Text, "LOCAL-DIRECTION")
	if gi < 0 || li < 0 {
		t.Fatalf("both directions must be present; global=%d local=%d\n%s", gi, li, ctx.Text)
	}
	if gi >= li {
		t.Errorf("local direction must come last: global@%d local@%d\n%s", gi, li, ctx.Text)
	}
}

// TestRubricFromGlobalSurvivesLocalActorsOverride: the rubric always comes from
// global, even when the local layer overrides Actors.
func TestRubricFromGlobalSurvivesLocalActorsOverride(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	writeStrand(t, home, "## Bead Quality Rubric\nRUBRIC-MARKER-XYZ\n\n## Actors\n- GlobalOnlyActor\n")
	writeStrand(t, repo, "## Actors\n- LocalProjectManager\n")

	ctx, err := Load(home, repo)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(ctx.Text, "RUBRIC-MARKER-XYZ") {
		t.Errorf("rubric from global missing after local Actors override:\n%s", ctx.Text)
	}
	if !strings.Contains(ctx.Text, "LocalProjectManager") {
		t.Errorf("local Actors override missing:\n%s", ctx.Text)
	}
}

// TestImportExpandsOneLevel: a line `@<path>` inlines the referenced file so the
// north-star line reaches the blob whether melded inline or pointed at by import.
func TestImportExpandsOneLevel(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	writeStrand(t, home, "## Actors\n- G\n")
	writeStrand(t, repo, "## North Star\n@notes.md\n")
	// import resolves relative to the STRAND.md that holds the @-line (repo/.strand).
	writeAt(t, filepath.Join(repo, ".strand", "notes.md"), "NORTH-STAR-LINE\n")

	ctx, err := Load(home, repo)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(ctx.Text, "NORTH-STAR-LINE") {
		t.Errorf("@-import not inlined:\n%s", ctx.Text)
	}
	if strings.Contains(ctx.Text, "@notes.md") {
		t.Errorf("literal @-line survived expansion:\n%s", ctx.Text)
	}
}

// TestImportDoesNotRecurse: expansion is exactly one level — an @-line inside an
// imported file stays literal, never followed.
func TestImportDoesNotRecurse(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	writeStrand(t, home, "## Actors\n- G\n")
	writeStrand(t, repo, "## North Star\n@a.md\n")
	writeAt(t, filepath.Join(repo, ".strand", "a.md"), "A-CONTENT\n@b.md\n")
	writeAt(t, filepath.Join(repo, ".strand", "b.md"), "B-CONTENT\n")

	ctx, err := Load(home, repo)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(ctx.Text, "A-CONTENT") {
		t.Errorf("first-level import missing:\n%s", ctx.Text)
	}
	if !strings.Contains(ctx.Text, "@b.md") {
		t.Errorf("second-level @-line should stay literal:\n%s", ctx.Text)
	}
	if strings.Contains(ctx.Text, "B-CONTENT") {
		t.Errorf("import recursed past one level:\n%s", ctx.Text)
	}
}

// TestMeldedInlineNorthStarReachesBlob: a north-star melded inline under a heading
// reaches the blob via plain concatenation (the meld arm of meld-or-point).
func TestMeldedInlineNorthStarReachesBlob(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	writeStrand(t, home, "## Actors\n- G\n")
	writeStrand(t, repo, "## North Star\nMELDED-NORTH-STAR\n\n## Actors\n- P\n")

	ctx, err := Load(home, repo)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(ctx.Text, "MELDED-NORTH-STAR") {
		t.Errorf("inline north star missing from blob:\n%s", ctx.Text)
	}
}

// TestMissingImportLeftAsIs: an @-line that points at a missing file is not an
// error — the line is left intact rather than dropped or panicking.
func TestMissingImportLeftAsIs(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	writeStrand(t, home, "## Actors\n- G\n")
	writeStrand(t, repo, "## North Star\n@nope.md\n")

	ctx, err := Load(home, repo)
	if err != nil {
		t.Fatalf("Load should not error on missing import: %v", err)
	}
	if !strings.Contains(ctx.Text, "@nope.md") {
		t.Errorf("missing-import line should be preserved verbatim:\n%s", ctx.Text)
	}
}

// TestMentionLineNotImported: a social-style mention in an ## Actors section
// (@Human, or a line with several @-mentions) is not a single whitespace-free
// path, so it is left verbatim rather than read as a file.
func TestMentionLineNotImported(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	writeStrand(t, home, "## Actors\n- G\n")
	writeStrand(t, repo, "## Actors\n@Human and @Agent collaborate\n")

	ctx, err := Load(home, repo)
	if err != nil {
		t.Fatalf("Load should not error on a mention line: %v", err)
	}
	if !strings.Contains(ctx.Text, "@Human and @Agent collaborate") {
		t.Errorf("mention line should survive verbatim:\n%s", ctx.Text)
	}
}

// TestEmptyHomeDirErrors guards the real ~/.strand: an empty homeDir is refused so
// the loader never writes a default into a path resolved against the cwd.
func TestEmptyHomeDirErrors(t *testing.T) {
	if _, err := Load("", t.TempDir()); err == nil {
		t.Errorf("Load(\"\", repo) must error to protect the real filesystem")
	}
}
