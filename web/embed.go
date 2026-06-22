// Package web holds strand's embedded front-end: html/template sources and the
// static assets (token CSS, vendored htmx, a thin progressive-enhancement JS).
package web

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/dkoosis/strand/internal/forest"
)

//go:embed templates static
var assets embed.FS

// Static serves the embedded assets under /static/ (htmx, CSS, JS). Templates
// live in the same FS but are not routed, so they never leak as files.
func Static() http.Handler {
	return http.FileServer(http.FS(assets))
}

// Templates parses the UI templates with the formatting helpers the views use.
func Templates() (*template.Template, error) {
	tmpl, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return tmpl, nil
}

var funcs = template.FuncMap{
	"statusLabel": func(s string) string { return strings.ReplaceAll(s, "_", " ") },
	"pclass":      func(p int) string { return "p" + strconv.Itoa(clampPri(p)) },
	"shortID":     shortID,
	"cleanName":   cleanName,
	"regionLabel": regionLabel,
	"epicArgs":    epicArgs,
}

// regionLabel is the repo-button caption: the first region's name, or a dash
// when the forest is empty.
func regionLabel(regions []forest.Region) string {
	if len(regions) == 0 {
		return "—"
	}
	return regions[0].Name
}

// epicArgs bundles an epic with whether to draw its group header, so the
// epic-group template serves both the single-epic and whole-region views. The
// value copy is intrinsic to a template FuncMap and the epic is read-only here.
//
//nolint:gocritic // template helpers receive values; Epic is not mutated.
func epicArgs(e forest.Epic, head bool) map[string]any {
	return map[string]any{"Epic": e, "Head": head}
}

// shortID drops the project prefix ("strand-5ri.2" → "5ri.2") so the id recedes
// to a locator without shouting the repo on every row.
func shortID(id string) string {
	if _, rest, ok := strings.Cut(id, "-"); ok {
		return rest
	}
	return id
}

var (
	epicPrefix = regexp.MustCompile(`(?i)^(\[epic\]|\(epic\))\s*`)
	trailParen = regexp.MustCompile(`\s*\([^)]*\)\s*$`)
)

// cleanName strips an "[epic]"/"(EPIC)" prefix and a trailing parenthetical so a
// story title reads as a story, not a tracker label.
func cleanName(t string) string {
	t = epicPrefix.ReplaceAllString(t, "")
	t = trailParen.ReplaceAllString(t, "")
	return strings.TrimSpace(t)
}

func clampPri(p int) int {
	if p < 0 {
		return 0
	}
	if p > 4 {
		return 4
	}
	return p
}
