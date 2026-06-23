// Package forest builds strand's landing model — the "forest" view (spec §1a):
// the work in motion as sized story tiles, not a flat catalog. It turns a flat
// list of bd issues into regions of epics, each epic a treemap tile sized by its
// open work, so weight and heat read before a word does.
//
// Regions are the project's top-level epics — the trunks the human shapes (spec
// §1a: trixi's MEMORY, WETWARE, RETRIEVAL …). A region's tiles are the epics one
// level beneath its trunk; deeper tasks and subtasks roll up into the tile they
// descend from. Live work that doesn't ladder up to any trunk collects in a
// catch-all region. The trunk hierarchy lives in bd (parent edges), so the
// synthesis layer spec §1a deferred is now read straight from the data.
package forest

import (
	"hash/fnv"
	"sort"
	"strings"

	"github.com/dkoosis/strand/internal/bd"
)

// tilePalette holds the tile hues for the treemap. Reds and ambers are left
// out so a tile's color never collides with the status/priority signals
// (blocked, in-progress, P0/P1). Each entry is a vivid mid-tone that reads as a
// gentle tint once mixed into the card at --tile-mix.
const nTileHues = 8

var tilePalette = [nTileHues]string{
	"#3e63dd", // indigo
	"#6e56cf", // violet
	"#ab4aba", // plum
	"#d6409f", // pink
	"#12a594", // teal
	"#30a46c", // green
	"#00a2c7", // cyan
	"#0090ff", // blue
}

// tileColor maps an epic id to a stable palette hue. Hashing the id (rather
// than using sort position) keeps a tile's color fixed as its open count and
// rank shift between requests.
func tileColor(id string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	// Modulo by the untyped const keeps the math in uint32 with no int->uint32
	// conversion, and the array size ties to nTileHues so the index can't escape.
	return tilePalette[h.Sum32()%nTileHues]
}

// Bead is one issue as the bead-list renders it.
type Bead struct {
	ID       string
	Title    string
	Status   bd.Status
	Priority int
	Type     string
	Assignee string
	Rank     float64 // manual-rank order (V1 list); meaningful only when HasRank
	HasRank  bool
}

// NewBead projects a bd.Issue onto the render-facing Bead — the one place that
// maps bd's field names (IssueType) to the view's, so the epic roll-up and the
// board's single-card refresh can't drift.
func NewBead(i *bd.Issue) Bead {
	rank, hasRank := i.Rank()
	return Bead{
		ID:       i.ID,
		Title:    i.Title,
		Status:   i.Status,
		Priority: i.Priority,
		Type:     i.IssueType,
		Assignee: i.Assignee,
		Rank:     rank,
		HasRank:  hasRank,
	}
}

// Epic is a story tile: a root issue plus its open descendants. Open is the tile
// weight; Flag marks an epic holding active P0/P1 work.
type Epic struct {
	ID    string
	Name  string
	Open  int
	Flag  bool
	Color string // tile hue, hashed from the tile's id so it holds steady across requests
	Beads []Bead
	Rect  Rect // geometry within its region's body, in 0–100 percentages
}

// Region is a trunk: a top-level epic whose tiles are the epics beneath it.
// Key is the trunk's bd id (or looseKey for off-trunk work); Color is stable per
// trunk across requests.
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
func openish(status bd.Status) bool {
	return status != bd.StatusClosed && status != bd.StatusDeferred
}

// regionPalette colors the trunks. Colors are assigned by sorted region key, so
// a trunk keeps its color across requests no matter its current weight.
var regionPalette = []string{"#4c7ef0", "#e0663d", "#3fa66a", "#a463d6", "#d6a13f", "#2fa6a0"}

const (
	// looseKey is the catch-all region for live work that doesn't ladder up to a
	// trunk (standalone tasks, orphaned features).
	looseKey   = "__loose__"
	looseColor = "#7a8290"
)

// isTrunk reports whether an issue is a region trunk: a top-level epic. A trunk
// defines a region; it is never itself a tile or a bead.
func isTrunk(is *bd.Issue) bool {
	return is.Parent == "" && is.IssueType == "epic"
}

// Build assembles the forest from a flat issue list and the synthesis layer.
func Build(issues []bd.Issue, syn Synthesis) Forest {
	byID := make(map[string]bd.Issue, len(issues))
	for i := range issues {
		byID[issues[i].ID] = issues[i]
	}

	// Group every live non-trunk issue under its tile (the epic one level below
	// its trunk), and record which region the tile belongs to. Count in-progress
	// work in the same pass.
	members := make(map[string][]bd.Issue)
	regionOf := make(map[string]string)
	inProgress := 0
	for i := range issues {
		if !openish(issues[i].Status) || isTrunk(&issues[i]) {
			continue
		}
		if issues[i].Status == bd.StatusInProgress {
			inProgress++
		}
		region, epic := placeIssue(issues[i].ID, byID)
		members[epic] = append(members[epic], issues[i])
		regionOf[epic] = region
	}

	f := Forest{NorthStar: syn.NorthStar, InProgress: inProgress}
	if len(members) == 0 {
		return f
	}

	// Collect tiles into their regions.
	byRegion := make(map[string][]Epic)
	for epicID, ms := range members {
		e := buildEpic(epicID, ms, byID)
		byRegion[regionOf[epicID]] = append(byRegion[regionOf[epicID]], e)
	}

	regions := make([]Region, 0, len(byRegion))
	for key, epics := range byRegion {
		sortEpics(epics)
		layoutEpics(epics)
		r := Region{Key: key, Name: regionName(key, byID), Epics: epics}
		for i := range epics {
			r.Open += epics[i].Open
		}
		regions = append(regions, r)
		f.Open += r.Open
	}
	colorRegions(regions)
	// A project with no trunk structure is one big loose region — name it for
	// the project rather than "off-trunk".
	if len(regions) == 1 && regions[0].Key == looseKey {
		regions[0].Name = syn.Project
	}
	sortRegions(regions)
	layoutRegions(regions)
	f.Regions = regions
	return f
}

// placeIssue returns the (region, tile) an issue belongs to: its trunk and the
// epic one level beneath that trunk. Work that descends from no trunk lands in
// the catch-all region, keyed by its own top-level ancestor's child.
func placeIssue(id string, byID map[string]bd.Issue) (region, epic string) {
	root := rootOf(id, byID)
	if root == id {
		return looseKey, id // standalone top-level non-epic: its own tile, off-trunk
	}
	if r, ok := byID[root]; ok && isTrunk(&r) {
		return root, childOfRoot(id, root, byID)
	}
	return looseKey, childOfRoot(id, root, byID)
}

// childOfRoot returns the ancestor of id whose parent is root — the depth-1 node
// directly under the trunk, which is the tile id rolls up into. It falls back to
// the last node reached if the chain doesn't reach root (missing link or cycle).
func childOfRoot(id, root string, byID map[string]bd.Issue) string {
	if id == root {
		return id
	}
	seen := map[string]bool{}
	cur := id
	for {
		is, ok := byID[cur]
		if !ok || is.Parent == "" || is.Parent == root || seen[cur] {
			return cur
		}
		seen[cur] = true
		cur = is.Parent
	}
}

// regionName cleans a trunk title down to its label ("MEMORY trunk — …" →
// "MEMORY"); the catch-all region is named by the caller.
func regionName(key string, byID map[string]bd.Issue) string {
	if key == looseKey {
		return "off-trunk"
	}
	if is, ok := byID[key]; ok {
		title := is.Title
		if i := strings.Index(title, " — "); i >= 0 {
			title = title[:i]
		}
		title = strings.TrimSpace(title)
		title = strings.TrimSuffix(title, " trunk")
		return strings.TrimSpace(title)
	}
	return key
}

// colorRegions assigns each region a stable color by sorted key, so trunk colors
// hold steady across requests regardless of weight order.
func colorRegions(regions []Region) {
	keys := make([]string, len(regions))
	for i := range regions {
		keys[i] = regions[i].Key
	}
	sort.Strings(keys)
	color := make(map[string]string, len(keys))
	ci := 0
	for _, k := range keys {
		if k == looseKey {
			color[k] = looseColor
			continue
		}
		color[k] = regionPalette[ci%len(regionPalette)]
		ci++
	}
	for i := range regions {
		regions[i].Color = color[regions[i].Key]
	}
}

// sortEpics / sortRegions order tiles largest-first, ties broken by id/key for a
// deterministic layout across requests.
func sortEpics(epics []Epic) {
	sort.SliceStable(epics, func(a, b int) bool {
		if epics[a].Open != epics[b].Open {
			return epics[a].Open > epics[b].Open
		}
		return epics[a].ID < epics[b].ID
	})
}

func sortRegions(regions []Region) {
	sort.SliceStable(regions, func(a, b int) bool {
		if regions[a].Open != regions[b].Open {
			return regions[a].Open > regions[b].Open
		}
		return regions[a].Key < regions[b].Key
	})
}

// layoutRegions squarifies the regions into the treemap's 0–100 space by their
// open weight.
func layoutRegions(regions []Region) {
	weights := make([]float64, len(regions))
	for i := range regions {
		weights[i] = float64(regions[i].Open)
	}
	rects := squarify(weights)
	for i := range regions {
		regions[i].Rect = rects[i]
	}
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
	e := Epic{ID: rootID, Open: len(members), Color: tileColor(rootID)}
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
	sortBeads(e.Beads)
	return e
}

// sortBeads orders an epic's beads. An untouched epic (no bead manually ranked)
// keeps the default priority-asc, id tiebreak — unchanged behavior. The first
// manual reorder seeds a rank onto every bead in the group (server handleRank),
// so a ranked group is wholly rank-ordered; mixing the two states never happens,
// which keeps the key space monotonic and drag inserts collision-safe.
func sortBeads(beads []Bead) {
	ranked := false
	for i := range beads {
		if beads[i].HasRank {
			ranked = true
			break
		}
	}
	sort.SliceStable(beads, func(a, b int) bool {
		if ranked {
			// A bead created into an already-ranked epic has no rank yet
			// (HasRank false, Rank 0); since head-insert drags can mint
			// negative ranks, a zero default could land it mid-list. Sort
			// unranked beads after ranked ones so they collect at the bottom.
			if beads[a].HasRank != beads[b].HasRank {
				return beads[a].HasRank
			}
			if beads[a].Rank != beads[b].Rank {
				return beads[a].Rank < beads[b].Rank
			}
			return beads[a].ID < beads[b].ID
		}
		if beads[a].Priority != beads[b].Priority {
			return beads[a].Priority < beads[b].Priority
		}
		return beads[a].ID < beads[b].ID
	})
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
