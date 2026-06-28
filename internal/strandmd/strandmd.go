// Package strandmd resolves the layered STRAND.md grounding context that feeds
// strand's Tier-2 suggestion call. It mirrors the CLAUDE.md model: a shipped
// default at ~/.strand/STRAND.md (the bead-quality rubric + an ## Actors stub,
// user-managed after first init) overlaid by an optional repo-local
// ./.strand/STRAND.md (the project's real ## Actors and direction).
//
// The loader is deliberately small — read, one-level @-import expansion, and a
// line-based ## Actors split to compose the layers. No markdown AST, no
// templating engine, no config framework.
package strandmd

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// errHomeDirRequired is returned by Load when homeDir is empty, so a default is
// never written to a path resolved against the current directory.
var errHomeDirRequired = errors.New("strandmd: homeDir required")

// defaultTemplate is the shipped default written to an absent global STRAND.md.
// It carries the bead-quality rubric (seeded from bdx:bead-fmt) and a default
// ## Actors stub.
//
//go:embed default.md
var defaultTemplate string

// strandRelPath is the location of a STRAND.md under a home or repo root.
var strandRelPath = filepath.Join(".strand", "STRAND.md")

// Context is the resolved STRAND.md grounding blob. Text is the composed,
// @-expanded context (rubric from global, the resolved Actors, and the layered
// direction). Actors is the resolved ## Actors section on its own — the registry
// a Tier-2 suggestion must draw its persona from.
type Context struct {
	Text   string
	Actors string
}

// Default returns the shipped STRAND.md template embedded in the binary.
func Default() string { return defaultTemplate }

// Load resolves the layered STRAND.md context.
//
// homeDir is the root that holds ~/.strand/STRAND.md; it is injected (never read
// from os.UserHomeDir) so callers and tests choose the root. An absent global
// file is initialized from the embedded default, claude-init style, with no
// error. homeDir must be non-empty so a default is never written to a path
// resolved against the current directory.
//
// repoDir is the repo root that may hold an optional ./.strand/STRAND.md overlay;
// an empty repoDir, or a repo with no such file, means global-only.
//
// Composition: the rubric comes from global; the ## Actors section is the local
// one if the local file declares it, else the global default; direction is the
// two layers concatenated, local last. Each file's @<path> lines are expanded one
// level — the referenced file is inlined, but @-lines inside it are left literal.
func Load(homeDir, repoDir string) (Context, error) {
	if homeDir == "" {
		return Context{}, errHomeDirRequired
	}

	globalPath := filepath.Join(homeDir, strandRelPath)
	globalRaw, err := readOrInit(globalPath, defaultTemplate)
	if err != nil {
		return Context{}, err
	}
	globalText := expandImports(globalRaw, filepath.Dir(globalPath))

	var localText string
	haveLocal := false
	if repoDir != "" {
		localPath := filepath.Join(repoDir, strandRelPath)
		raw, rerr := os.ReadFile(localPath)
		switch {
		case rerr == nil:
			haveLocal = true
			localText = expandImports(string(raw), filepath.Dir(localPath))
		case errors.Is(rerr, os.ErrNotExist):
			// no overlay — global-only
		default:
			return Context{}, fmt.Errorf("strandmd: read local STRAND.md: %w", rerr)
		}
	}

	globalActors, globalBody := splitActors(globalText)
	localActors, localBody := splitActors(localText)

	// ## Actors = local-if-present-else-global.
	actors := globalActors
	if haveLocal && strings.TrimSpace(localActors) != "" {
		actors = localActors
	}

	// rubric + global direction, then Actors, then local direction (local last).
	var parts []string
	if s := strings.TrimSpace(globalBody); s != "" {
		parts = append(parts, s)
	}
	if s := strings.TrimSpace(actors); s != "" {
		parts = append(parts, s)
	}
	if s := strings.TrimSpace(localBody); haveLocal && s != "" {
		parts = append(parts, s)
	}

	return Context{
		Text:   strings.Join(parts, "\n\n") + "\n",
		Actors: strings.TrimSpace(actors),
	}, nil
}

// readOrInit reads path, or writes def there (creating the dir) and returns it
// when the file is absent. Any other read/write error is surfaced.
func readOrInit(path, def string) (string, error) {
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		return string(b), nil
	case errors.Is(err, os.ErrNotExist):
		if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
			return "", fmt.Errorf("strandmd: mkdir .strand: %w", mkErr)
		}
		if wErr := os.WriteFile(path, []byte(def), 0o600); wErr != nil {
			return "", fmt.Errorf("strandmd: write default STRAND.md: %w", wErr)
		}
		return def, nil
	default:
		return "", fmt.Errorf("strandmd: read STRAND.md: %w", err)
	}
}

// expandImports inlines one level of @<path> imports. A line whose trimmed form
// is @<path> is replaced by the referenced file's contents; paths are resolved
// relative to baseDir (absolute paths used as-is). Expansion is one level only —
// @-lines inside an inlined file are not followed. A missing or unreadable target
// leaves the @-line intact.
func expandImports(text, baseDir string) string {
	if !strings.Contains(text, "@") {
		return text
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		p, ok := importPath(ln)
		if !ok {
			out = append(out, ln)
			continue
		}
		target := p
		if !filepath.IsAbs(target) {
			target = filepath.Join(baseDir, target)
		}
		// G703: the @-import target is a path written in the user's own STRAND.md
		// (user-managed config, like CLAUDE.md @-imports). Pointing at a file
		// outside the repo — the global ~/.strand, a shared north-star-mini — is
		// the intended use, not untrusted input.
		b, err := os.ReadFile(target) //nolint:gosec // G703: user-managed config path, see comment above

		if err != nil {
			out = append(out, ln) // missing import: leave the line as-is
			continue
		}
		out = append(out, strings.TrimRight(string(b), "\r\n"))
	}
	return strings.Join(out, "\n")
}

// importPath returns the path of an @-import line and whether ln is one. An
// import line's trimmed form starts with '@' followed by a non-empty,
// whitespace-free path. The whitespace guard keeps social-style mentions
// (@Human, "@Agent and @Reviewer") in an ## Actors section from being read as
// file paths.
func importPath(ln string) (string, bool) {
	t := strings.TrimSpace(ln)
	if !strings.HasPrefix(t, "@") {
		return "", false
	}
	p := strings.TrimSpace(t[1:])
	if p == "" || strings.ContainsAny(p, " \t") {
		return "", false
	}
	return p, true
}

// splitActors separates a STRAND.md's ## Actors section from the rest. actors is
// the Actors heading and its body (through the next H2 or EOF), or "" when no
// such section exists; rest is the text with that section removed. The match is
// case-insensitive on the heading text.
func splitActors(text string) (actors, rest string) {
	if text == "" {
		return "", ""
	}
	lines := strings.Split(text, "\n")
	start := -1
	for i, ln := range lines {
		if isH2(ln) && strings.EqualFold(h2Name(ln), "actors") {
			start = i
			break
		}
	}
	if start == -1 {
		return "", text
	}
	end := len(lines)
	for j := start + 1; j < len(lines); j++ {
		if isH2(lines[j]) {
			end = j
			break
		}
	}
	actors = strings.Join(lines[start:end], "\n")
	kept := make([]string, 0, len(lines)-(end-start))
	kept = append(kept, lines[:start]...)
	kept = append(kept, lines[end:]...)
	return actors, strings.Join(kept, "\n")
}

// isH2 reports whether ln is a level-2 ATX heading (## ...), not a deeper level.
func isH2(ln string) bool {
	t := strings.TrimSpace(ln)
	return strings.HasPrefix(t, "## ") && !strings.HasPrefix(t, "### ")
}

// h2Name returns an H2 heading's text, stripped of leading #s and surrounding
// whitespace.
func h2Name(ln string) string {
	return strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(ln), "#"))
}
