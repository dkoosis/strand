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

	"github.com/dkoosis/strand/internal/bd"
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
	"statusLabel": func(s bd.Status) string { return strings.ReplaceAll(string(s), "_", " ") },
	"pclass":      func(p int) string { return "p" + strconv.Itoa(clampPri(p)) },
	"shortID":     shortID,
	"cleanName":   cleanName,
	"regionLabel": regionLabel,
	"priorities":  priorities,
	"beadTypes":   func() []string { return beadTypes },
	"metaVal":     metaVal,
	"rankLabel":   rankLabel,
	"labelPart":   labelPart,
}

// metaVal renders a bd metadata value for display in the read-only system-metadata
// block. It is view-only: the drawer never writes these back, so this is a one-way
// any→string projection. Absent keys render as an em dash so the row reads as
// "unset" rather than disappearing. Floats that are whole numbers (rank, est_cost)
// drop the trailing ".0"; bd emits JSON numbers as float64.
func metaVal(meta map[string]any, key string) string {
	v, ok := meta[key]
	if !ok || v == nil {
		return "—"
	}
	switch t := v.(type) {
	case string:
		if t == "" {
			return "—"
		}
		return t
	case bool:
		if t {
			return "yes"
		}
		return "no"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}

// rankLabel renders an *bd.Issue's manual rank for the read-only metadata block,
// or an em dash when the bead is unranked. Rank() returns (float64, bool); a
// template can't consume the second value, so collapse it here.
func rankLabel(i *bd.Issue) string {
	r, ok := i.Rank()
	if !ok {
		return "—"
	}
	return strconv.FormatFloat(r, 'f', -1, 64)
}

// LabelPart decodes one label string for the drawer. A `key=value` label renders
// as a key-value pair; anything else is a plain chip. bd has no native pairs —
// the convention lives here and in the add handler, the one encode/decode seam.
type LabelPart struct {
	Raw   string // the full label as bd stores it (what remove posts back)
	Key   string // pair key, or the whole label when not a pair
	Value string // pair value, empty for a plain chip
	Pair  bool   // true when the label is a key=value pair
}

// labelPart splits a label into its rendered parts. A pair needs a non-empty key
// before the first `=`; "=v" or a bare label stays a plain chip.
func labelPart(label string) LabelPart {
	if key, val, ok := strings.Cut(label, "="); ok && key != "" {
		return LabelPart{Raw: label, Key: key, Value: val, Pair: true}
	}
	return LabelPart{Raw: label, Key: label}
}

// beadTypes are the issue types the create form offers, matching bd's --type.
var beadTypes = []string{"task", "bug", "feature", "epic"}

// maxPri is the highest bd priority (P0 is most urgent). One source for both the
// clamp and the dropdown so the range can't drift.
const maxPri = 4

// priorities lists the selectable priority levels 0..maxPri for the edit dropdown.
func priorities() []int {
	ps := make([]int, maxPri+1)
	for i := range ps {
		ps[i] = i
	}
	return ps
}

// regionLabel is the repo-button caption: the first region's name, or a dash
// when the forest is empty.
func regionLabel(regions []forest.Region) string {
	if len(regions) == 0 {
		return "—"
	}
	return regions[0].Name
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
	if p > maxPri {
		return maxPri
	}
	return p
}
