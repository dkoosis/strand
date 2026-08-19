// Package strandmd resolves the layered STRAND.md grounding context that feeds
// strand's drawer-assist call. It mirrors the CLAUDE.md model: a shipped
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
	"strconv"
	"strings"

	"github.com/dkoosis/atomicfile"
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
// an assist proposal must draw its persona from.
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
		if wErr := atomicfile.WriteFile(path, []byte(def), 0o600); wErr != nil {
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
		// outside the repo — the global ~/.strand, a shared NORTH_STAR.md — is
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

// NorthStarFile is the repo-root file strand reads the masthead's North Star
// from — the same NORTH_STAR.md the wrap SessionStart hook greps its ★ line
// from (st-y0a: one destination doc, every reader on the same file; supersedes
// the north-star-mini.md of decision nug 952acad4aca2).
const NorthStarFile = "NORTH_STAR.md"

// NorthStar returns the masthead North Star for a repo: the first ★-marked line
// of NORTH_STAR.md (marker stripped) plus any immediately following lines up to
// a blank line or heading, newlines preserved. The ★ line is the doc's own
// TL;DR by convention, so a full north-star doc renders as its one-liner while
// a multi-line block under the ★ still comes through whole. A missing file or a
// doc with no ★ line yields "" so the masthead renders its seed hint instead of
// crashing (str-d2s).
func NorthStar(repoPath string) string {
	if repoPath == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(repoPath, NorthStarFile))
	if err != nil {
		return ""
	}
	return northStarBlock(string(b))
}

// RoadmapFile is the repo-root file holding the project's destination line and its
// ordered epic list — the sdlc standard (home/rules/sdlc.md §The standard,
// .claude/rules/vocabulary.md, ratified 2026-08-17). Resolved before the legacy
// NorthStarFile pointer.
const RoadmapFile = "ROADMAP.md"

// Roadmap returns the repo's roadmap-ordered epic ids — the Go twin of
// roadmap-epics.sh's roadmap_file/roadmap_parse contract
// (sdlc/plugins/wrap/scripts/lib/roadmap-epics.sh), reduced to that script's two
// non-kg branches (the kg face page is a shell-only migration rung; resolving it
// from Go is this bead's explicit non-goal, st-3wp.1). It is decision-owned order,
// not status; strand's station bar and pickNext walk it against the live epic DAG.
//
// Resolution: $repoPath/ROADMAP.md, else the legacy $repoPath/NORTH_STAR.md.
//
// ROADMAP.md format: numbered lines carrying an arrow inside a `## Epics` section
// (legacy `## Milestones`/`## Route` headings tolerated) —
// `N. [status] <title> → <id>[, <id>...]`. The epic id is the first
// comma/whitespace-delimited token after the first arrow (a legacy multi-id line:
// first wins). A numbered line with no arrow is a stray non-epic list item (the
// page's ## Architecture/## Lifecycle sections number their own rows) and is
// skipped, not read as a blank id. An indented continuation line folds into its
// preceding numbered line before parsing — the sd-r59 wrapped-line rule, so a title
// too long for one line still yields one epic, not a mangled arrow-less row.
//
// NORTH_STAR.md, resolved only when ROADMAP.md is absent, keeps its own original
// `## roadmap` section + id-first-token parse (roadmapIDs below) as the final
// fallback — no `## Epics` heading, no arrow, unchanged from before this bead.
//
// A missing file, missing section, or empty section yields nil.
func Roadmap(repoPath string) []string {
	if repoPath == "" {
		return nil
	}
	if b, err := os.ReadFile(filepath.Join(repoPath, RoadmapFile)); err == nil {
		return roadmapEpicIDs(string(b))
	}
	b, err := os.ReadFile(filepath.Join(repoPath, NorthStarFile))
	if err != nil {
		return nil
	}
	return roadmapIDs(string(b))
}

// epicSectionHeadings names the H2 headings that open ROADMAP.md's ordered epic
// list: the current name plus the two size-word names banned 2026-08-19
// (.claude/rules/vocabulary.md) that a not-yet-conformed page may still carry.
var epicSectionHeadings = map[string]bool{
	"epics":      true,
	"milestones": true,
	"route":      true,
}

// roadmapEpicIDs extracts ROADMAP.md's ordered epic ids: fold the epics section's
// wrapped continuation lines into their numbered row (foldEpicSection), then keep
// each row's id only if the row carries an arrow (epicLineID).
func roadmapEpicIDs(s string) []string {
	var ids []string
	for _, row := range foldEpicSection(s) {
		if id, ok := epicLineID(row); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// foldEpicSection returns the epics section's numbered lines as one row apiece,
// with any immediately following indented continuation lines folded in — the Go
// twin of roadmap-epics.sh's awk fold (sd-r59). A numbered line starts a new row;
// an indented line with content extends the current row; anything else (blank line,
// unindented prose, a heading) ends the current row without extending it. Rows
// outside the epics section are dropped.
func foldEpicSection(s string) []string {
	var rows []string
	var cur strings.Builder
	inSection := false
	flush := func() {
		if cur.Len() > 0 {
			rows = append(rows, cur.String())
			cur.Reset()
		}
	}
	for ln := range strings.SplitSeq(s, "\n") {
		line := strings.TrimRight(ln, "\r")
		trimmed := strings.TrimSpace(line)
		switch {
		case !inSection:
			if isH2(trimmed) && epicSectionHeadings[strings.ToLower(h2Name(trimmed))] {
				inSection = true
			}
		case isH2(trimmed):
			flush()
			return rows // next heading ends the section
		case isNumberedLine(trimmed):
			flush()
			cur.WriteString(trimmed)
		case isIndentedContinuation(line):
			if cur.Len() > 0 {
				cur.WriteByte(' ')
				cur.WriteString(trimmed)
			}
		default:
			flush() // blank line or unindented prose: ends the row, folds nothing
		}
	}
	flush()
	return rows
}

// isNumberedLine reports whether line starts with a leading integer and a dot
// (the epic-list row marker: "1.", "10.", …), independent of what follows.
func isNumberedLine(line string) bool {
	dot := strings.IndexByte(line, '.')
	if dot <= 0 {
		return false
	}
	_, err := strconv.Atoi(line[:dot])
	return err == nil
}

// isIndentedContinuation reports whether line is a wrapped-title continuation: it
// starts with leading whitespace AND has non-whitespace content after it. A
// whitespace-only line does not match (mirrors the awk fold's
// /^[[:space:]]+[^[:space:]]/, which requires real content), so it falls through
// to ending the row like any other blank line.
func isIndentedContinuation(line string) bool {
	if line == "" || (line[0] != ' ' && line[0] != '\t') {
		return false
	}
	return strings.TrimSpace(line) != ""
}

// epicLineID extracts a folded epic row's id: the row must carry an arrow (else a
// stray numbered line, e.g. from another section's own list, is not an epic), and
// the id is the first comma/whitespace-delimited token after the first arrow — a
// legacy line with several ids keeps only the first.
func epicLineID(row string) (string, bool) {
	_, after, ok := strings.Cut(row, "→")
	if !ok {
		return "", false
	}
	rest := strings.ReplaceAll(after, ",", " ")
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", false
	}
	return fields[0], true
}

// roadmapIDs scans the `## roadmap` section for numbered lines (`N. <epic-id> …`)
// and returns each line's epic id, in order. It reads only within the section: the
// scan starts after the `## roadmap` heading and stops at the next level-2 heading.
func roadmapIDs(s string) []string {
	var ids []string
	inSection := false
	for ln := range strings.SplitSeq(s, "\n") {
		t := strings.TrimSpace(ln)
		switch {
		case !inSection:
			if isH2(t) && strings.EqualFold(h2Name(t), "roadmap") {
				inSection = true
			}
		case isH2(t):
			return ids // next heading ends the section
		default:
			if id, ok := numberedEpicID(t); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// numberedEpicID pulls the epic id from a roadmap line of the form `N. <id> — …`:
// a leading number, a dot, then the id as the next whitespace-delimited token. Any
// line that isn't numbered (prose, blank) yields ok false.
func numberedEpicID(line string) (string, bool) {
	dot := strings.IndexByte(line, '.')
	if dot <= 0 {
		return "", false
	}
	if _, err := strconv.Atoi(line[:dot]); err != nil {
		return "", false
	}
	rest := strings.Fields(line[dot+1:])
	if len(rest) == 0 {
		return "", false
	}
	return rest[0], true
}

// northStarBlock extracts the ★ block: from the first line whose trimmed form
// starts with ★ (marker and surrounding whitespace stripped) through the last
// contiguous non-blank, non-heading line.
func northStarBlock(s string) string {
	lines := strings.Split(s, "\n")
	start := -1
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "★") {
			start = i
			break
		}
	}
	if start == -1 {
		return ""
	}
	first := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[start]), "★"))
	block := []string{first}
	for _, ln := range lines[start+1:] {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") {
			break
		}
		block = append(block, t)
	}
	return strings.Join(block, "\n")
}
