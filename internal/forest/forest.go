// Package forest builds strand's landing model — the "forest" view (spec §1a):
// the work in motion as sized story tiles, not a flat catalog. It turns a flat
// list of bd issues into regions of epics, each epic a treemap tile sized by its
// open work, so weight and heat read before a word does.
//
// The region layer is a synthesis the human shapes (modules, north star, color);
// bd does not yet provide it (spec §1a, lines 100–102, tracks that mechanism
// separately). Until it lands, Build groups every epic under one project region.
package forest

import (
	"sort"

	"github.com/dkoosis/strand/internal/bd"
)

// Bead is one issue as the bead-list renders it.
type Bead struct {
	ID       string
	Title    string
	Status   string
	Priority int
	Type     string
	Assignee string
}

// NewBead projects a bd.Issue onto the render-facing Bead — the one place that
// maps bd's field names (IssueType) to the view's, so the epic roll-up and the
// board's single-card refresh can't drift.
func NewBead(i *bd.Issue) Bead {
	return Bead{
		ID:       i.ID,
		Title:    i.Title,
		Status:   i.Status,
		Priority: i.Priority,
		Type:     i.IssueType,
		Assignee: i.Assignee,
	}
}

// Epic is a story tile: a root issue plus its open descendants. Open is the tile
// weight; Flag marks an epic holding active P0/P1 work.
type Epic struct {
	ID    string
	Name  string
	Open  int
	Flag  bool
	Beads []Bead
	Rect  Rect // geometry within its region's body, in 0–100 percentages
}

// Region groups epics. One synthetic project region today; the module synthesis
// layer (spec §1a) will fan this out later without touching the renderer.
type Region struct {
	Key   string
	Name  string
	Color string
	Open  int
	Epics []Epic
	Rect  Rect // geometry within the treemap, in 0–100 percentages
}

// Forest is the whole landing model the page template renders.
type Forest struct {
	NorthStar  string
	Regions    []Region
	Open       int
	InProgress int
}

// Synthesis is the human-shaped layer bd does not yet provide (spec §1a): the
// north-star keystone and the module grouping. Today it carries the project
// label and north-star line; when module derivation lands it grows the
// region-mapping without changing Build's callers.
type Synthesis struct {
	Project   string
	NorthStar string
}

// openish reports whether a status counts as live work. Closed and deferred
// issues are not part of the forest — it shows what's in motion.
func openish(status string) bool {
	return status != "closed" && status != "deferred"
}

// Build assembles the forest from a flat issue list and the synthesis layer.
func Build(issues []bd.Issue, syn Synthesis) Forest {
	byID := make(map[string]bd.Issue, len(issues))
	for i := range issues {
		byID[issues[i].ID] = issues[i]
	}

	// Group every live issue under its top-level ancestor (the story/epic),
	// counting in-progress work in the same pass.
	groups := make(map[string][]bd.Issue)
	inProgress := 0
	for i := range issues {
		if !openish(issues[i].Status) {
			continue
		}
		if issues[i].Status == "in_progress" {
			inProgress++
		}
		root := rootOf(issues[i].ID, byID)
		groups[root] = append(groups[root], issues[i])
	}

	f := Forest{NorthStar: syn.NorthStar, InProgress: inProgress}
	epics := make([]Epic, 0, len(groups))
	for rootID, members := range groups {
		e := buildEpic(rootID, members, byID)
		epics = append(epics, e)
		f.Open += e.Open
	}
	// Largest epics first: stable, weight-ordered, ties broken by id for a
	// deterministic layout across requests.
	sort.SliceStable(epics, func(a, b int) bool {
		if epics[a].Open != epics[b].Open {
			return epics[a].Open > epics[b].Open
		}
		return epics[a].ID < epics[b].ID
	})

	if len(epics) == 0 {
		return f
	}
	layoutEpics(epics)
	f.Regions = []Region{{
		Key:   "project",
		Name:  syn.Project,
		Color: "#4c7ef0",
		Open:  f.Open,
		Epics: epics,
		Rect:  Rect{X: 0, Y: 0, W: 100, H: 100},
	}}
	return f
}

// rootOf walks the parent chain to the top-level ancestor id. A missing or empty
// parent (or a cycle hitting a seen id) stops the walk.
func rootOf(id string, byID map[string]bd.Issue) string {
	is, ok := byID[id]
	if !ok || is.Parent == "" {
		return id // common case: top-level or standalone, no map alloc
	}
	seen := map[string]bool{id: true}
	id = is.Parent
	for {
		is, ok = byID[id]
		if !ok || is.Parent == "" || seen[id] {
			return id
		}
		seen[id] = true
		id = is.Parent
	}
}

// buildEpic turns a root id and its live members into an Epic. The name comes
// from the root issue when known; beads sort priority-asc then most-recent.
func buildEpic(rootID string, members []bd.Issue, byID map[string]bd.Issue) Epic {
	e := Epic{ID: rootID, Open: len(members)}
	if root, ok := byID[rootID]; ok {
		e.Name = root.Title
	} else {
		e.Name = rootID
	}
	e.Beads = make([]Bead, 0, len(members))
	for i := range members {
		if members[i].Priority <= 1 {
			e.Flag = true
		}
		e.Beads = append(e.Beads, NewBead(&members[i]))
	}
	sort.SliceStable(e.Beads, func(a, b int) bool {
		if e.Beads[a].Priority != e.Beads[b].Priority {
			return e.Beads[a].Priority < e.Beads[b].Priority
		}
		return e.Beads[a].ID < e.Beads[b].ID
	})
	return e
}

// layoutEpics fills each epic's Rect by squarifying their open weights into the
// region body's 0–100 space.
func layoutEpics(epics []Epic) {
	weights := make([]float64, len(epics))
	for i, e := range epics {
		weights[i] = float64(e.Open)
	}
	rects := squarify(weights)
	for i := range epics {
		epics[i].Rect = rects[i]
	}
}
