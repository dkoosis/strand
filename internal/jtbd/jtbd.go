// Package jtbd resolves a story's job-to-be-done id to its human-readable job
// title. bead-fmt (bdx) stores a story's JTBD as an id pointer into the project
// registry, never the title — a title denormalized onto a bead drifts. The id
// alone is unreadable to a human triaging in strand, so the render layer looks
// the id up here and shows the job inline (str-r1q).
//
// The registry is docs/jtbd.md at the repo root, a Markdown pipe table:
//
//	| id    | job                          |
//	|-------|------------------------------|
//	| j-001 | Triage what to work on next  |
//
// A story cites its id with a `JTBD: <id>` line in its description (bead-fmt
// bans a `jtbd` metadata key, so the citation rides in the description). A
// missing registry, a missing file, or an id with no row are all non-errors: a
// repo without JTBD is the common case, and an unresolved id renders with a
// visible marker rather than crashing or hiding the bead.
package jtbd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Registry maps a project's JTBD ids to their job titles. The zero value (nil
// map) resolves nothing — the safe state for a repo with no docs/jtbd.md.
type Registry struct {
	jobs map[string]string
}

// citeRE matches a `JTBD: <id>` citation line, case-insensitive. The agreed
// storage format is a dedicated line, so the match is anchored to line-start
// (`(?m)^`) with only horizontal whitespace before the id — this keeps a prose
// `JTBD:` heading (with the value on the next line) from capturing the wrong
// token, and the line anchor alone rejects `NOTJTBD:`-style false positives.
// The id is the bead-fmt id-token charset (letters, digits, dot, dash,
// underscore); the first matching line wins.
var citeRE = regexp.MustCompile(`(?im)^[ \t]*JTBD:[ \t]*([A-Za-z0-9._-]+)`)

// Load reads docs/jtbd.md under repoPath and parses its pipe table into an
// id→job map. A missing or unreadable file yields an empty Registry, not an
// error — a repo without a JTBD registry is expected, not a failure.
func Load(repoPath string) Registry {
	if repoPath == "" {
		return Registry{}
	}
	b, err := os.ReadFile(filepath.Join(repoPath, "docs", "jtbd.md"))
	if err != nil {
		return Registry{}
	}
	return parse(string(b))
}

// Resolve returns the job title for a JTBD id and whether the registry holds it.
func (r Registry) Resolve(id string) (string, bool) {
	job, ok := r.jobs[id]
	return job, ok
}

// Cite extracts the JTBD id a description cites, and whether one is present.
func Cite(description string) (string, bool) {
	m := citeRE.FindStringSubmatch(description)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// parse pulls id→job pairs from the pipe-table rows in a docs/jtbd.md body.
// Only table rows (lines fenced by `|`) are read; prose and frontmatter around
// the table are ignored. The header row (first cell "id") and the separator row
// (first cell all dashes/colons) are skipped, so a well-formed table needs no
// special casing by the caller.
func parse(body string) Registry {
	jobs := make(map[string]string)
	for line := range strings.SplitSeq(body, "\n") {
		cells, ok := tableRow(line)
		if !ok || len(cells) < 2 {
			continue
		}
		id, job := cells[0], cells[1]
		if id == "" || strings.EqualFold(id, "id") || isSeparator(id) {
			continue
		}
		jobs[id] = job
	}
	if len(jobs) == 0 {
		return Registry{}
	}
	return Registry{jobs: jobs}
}

// tableRow splits a Markdown table row into its trimmed cells, reporting whether
// the line is a row at all (fenced by a leading and trailing `|`).
func tableRow(line string) ([]string, bool) {
	t := strings.TrimSpace(line)
	// len(t) < 2 guards the slice below: a lone "|" is prefix and suffix "|" at
	// once, so t[1:len(t)-1] would be t[1:0] — a low>high slice panic.
	if len(t) < 2 || !strings.HasPrefix(t, "|") || !strings.HasSuffix(t, "|") {
		return nil, false
	}
	parts := strings.Split(t[1:len(t)-1], "|")
	cells := make([]string, len(parts))
	for i, p := range parts {
		cells[i] = strings.TrimSpace(p)
	}
	return cells, true
}

// isSeparator reports whether a cell is a table separator (dashes and colons),
// the `|---|` row beneath the header.
func isSeparator(cell string) bool {
	if cell == "" {
		return false
	}
	return strings.Trim(cell, "-:") == ""
}
