// Package strand builds strand's landing model — the "strand" view (spec §1a):
// the work in motion as sized stories, not a flat catalog. It turns a flat list
// of bd issues into epics of stories, each story a map cell sized by its open
// work, so weight and heat read before a word does.
//
// Epics are the project's top-level work — the keystones the human shapes (spec
// §1a: trixi's MEMORY, WETWARE, RETRIEVAL …). An epic's stories are the issues
// one level beneath it; deeper tasks and subtasks roll up into the story they
// descend from. Live work that doesn't ladder up to any epic collects in a
// catch-all epic. The epic hierarchy lives in bd (parent edges), so the synthesis
// layer spec §1a deferred is now read straight from the data.
package strand

import (
	"sort"
	"strings"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/jtbd"
)

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
	// JTBDID is the job-to-be-done id a story cites in its description (str-r1q);
	// empty when the bead cites none. JTBDJob is that id resolved against the
	// project registry — empty when the id has no row, which the view renders as
	// an "(unresolved)" marker. The bare id is never the human's only signal.
	JTBDID  string
	JTBDJob string
}

// defaultPriority is the render priority for a bead whose bd.Issue omitted the
// field (Issue.Priority == nil). It matches bd's own default (P2) and keeps an
// absent-priority bead off the top of the priority-asc sort — never a false P0
// (str-zvh).
const defaultPriority = 2

// NewBead projects a bd.Issue onto the render-facing Bead — the one place that
// maps bd's field names (IssueType) to the view's, so the story roll-up and the
// board's single-card refresh can't drift.
func NewBead(i *bd.Issue) Bead {
	rank, hasRank := i.Rank()
	priority := defaultPriority
	if i.Priority != nil {
		priority = *i.Priority
	}
	return Bead{
		ID:       i.ID,
		Title:    i.Title,
		Status:   i.Status,
		Priority: priority,
		Type:     i.IssueType,
		Assignee: i.Assignee,
		Rank:     rank,
		HasRank:  hasRank,
	}
}

// ResolveJTBD fills the bead's JTBD fields from its source issue's description
// and a registry. It is separate from NewBead so the resolution stays the one
// place JTBD maps onto the view, while callers without a registry handy can
// skip it. A cited id with no registry row leaves JTBDJob empty (the unresolved
// state); no citation leaves both fields empty and the bead unchanged.
func (b *Bead) ResolveJTBD(description string, reg jtbd.Registry) {
	id, ok := jtbd.Cite(description)
	if !ok {
		return
	}
	b.JTBDID = id
	if job, ok := reg.Resolve(id); ok {
		b.JTBDJob = job
	}
}

// Story is one story: a root issue plus its open descendants. Open is its weight;
// Flag marks a story holding a bug-type bead (the "bug dot").
type Story struct {
	ID    string
	Name  string
	Open  int
	Flag  bool
	Beads []Bead
	Rect  Rect // geometry within its epic's body, in 0–100 percentages
}

// Epic is a top-level epic: a root bd epic whose stories are the issues beneath
// it. Key is the epic's bd id (or looseKey for off-epic work); Color is stable
// per epic across requests.
type Epic struct {
	Key     string
	Name    string
	Color   string
	Open    int
	Stories []Story
	Rect    Rect // geometry within the map, in 0–100 percentages
}

// BeadID returns the bd id of the epic's underlying bead, or "" when the epic
// has none — the off-epic catch-all (looseKey) and the synthetic "everything"/
// "bugs" scopes are groupings, not beads, so they can't open a drawer (str-scn).
func (e *Epic) BeadID() string {
	if e.Key == looseKey {
		return ""
	}
	return e.Key
}

// Model is the whole landing model the page template renders.
type Model struct {
	NorthStar  string
	Epics      []Epic
	Open       int
	InProgress int
}

// Synthesis is the human-shaped layer bd does not yet provide (spec §1a): the
// north-star keystone and the epic grouping. Today it carries the project label
// and north-star line; when grouping derivation lands it grows the epic-mapping
// without changing Build's callers.
type Synthesis struct {
	Project   string
	NorthStar string
	// JTBD resolves a story's cited job-to-be-done id to its job title (str-r1q).
	// The zero value resolves nothing — the safe state for a repo with no
	// docs/jtbd.md.
	JTBD jtbd.Registry
}

// openish reports whether a status counts as live work. Closed and deferred
// issues are not part of the strand — it shows what's in motion.
func openish(status bd.Status) bool {
	return status != bd.StatusClosed && status != bd.StatusDeferred
}

// epicPalette colors the epics. Colors are assigned by sorted epic key, so an
// epic keeps its color across requests no matter its current weight.
var epicPalette = []string{"#4c7ef0", "#e0663d", "#3fa66a", "#a463d6", "#d6a13f", "#2fa6a0"}

const (
	// looseKey is the catch-all epic for live work that doesn't ladder up to a
	// top-level epic (standalone tasks, orphaned features).
	looseKey   = "__loose__"
	looseColor = "#7a8290"
)

// isEpic reports whether an issue is a top-level epic: a root bd epic. It defines
// an epic group; it is never itself a story or a bead.
func isEpic(is *bd.Issue) bool {
	return is.Parent == "" && is.IssueType == "epic"
}

// Build assembles the strand from a flat issue list and the synthesis layer.
func Build(issues []bd.Issue, syn Synthesis) Model {
	byID := make(map[string]bd.Issue, len(issues))
	for i := range issues {
		byID[issues[i].ID] = issues[i]
	}

	// Group every live non-epic issue under its story (the issue one level below
	// its top-level epic), and record which epic the story belongs to. Count
	// in-progress work in the same pass.
	members := make(map[string][]bd.Issue)
	epicOf := make(map[string]string)
	inProgress := 0
	for i := range issues {
		if !openish(issues[i].Status) || isEpic(&issues[i]) {
			continue
		}
		if issues[i].Status == bd.StatusInProgress {
			inProgress++
		}
		epic, story := placeIssue(issues[i].ID, byID)
		members[story] = append(members[story], issues[i])
		epicOf[story] = epic
	}

	f := Model{NorthStar: syn.NorthStar, InProgress: inProgress}
	if len(members) == 0 {
		return f
	}

	// Collect stories into their epics.
	byEpic := make(map[string][]Story)
	for storyID, ms := range members {
		st := buildStory(storyID, ms, byID, syn.JTBD)
		byEpic[epicOf[storyID]] = append(byEpic[epicOf[storyID]], st)
	}

	epics := make([]Epic, 0, len(byEpic))
	for key, stories := range byEpic {
		sortStories(stories)
		layoutStories(stories)
		e := Epic{Key: key, Name: epicName(key, byID), Stories: stories}
		for i := range stories {
			e.Open += stories[i].Open
		}
		epics = append(epics, e)
		f.Open += e.Open
	}
	colorEpics(epics)
	// A project with no epic structure is one big loose epic — name it for the
	// project rather than "Off-epic".
	if len(epics) == 1 && epics[0].Key == looseKey {
		epics[0].Name = syn.Project
	}
	sortEpics(epics)
	layoutEpics(epics)
	f.Epics = epics
	return f
}

// placeIssue returns the (epic, story) an issue belongs to: its top-level epic
// and the issue one level beneath that epic. Work that descends from no epic
// lands in the catch-all epic, keyed by its own top-level ancestor's child.
func placeIssue(id string, byID map[string]bd.Issue) (epic, story string) {
	root := rootOf(id, byID)
	if root == id {
		return looseKey, id // standalone top-level non-epic: its own story, off-epic
	}
	if r, ok := byID[root]; ok && isEpic(&r) {
		return root, childOfRoot(id, root, byID)
	}
	return looseKey, childOfRoot(id, root, byID)
}

// childOfRoot returns the ancestor of id whose parent is root — the depth-1 node
// directly under the top-level epic, which is the story id rolls up into. It
// falls back to the last node reached if the chain doesn't reach root (missing
// link or cycle).
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

// epicName cleans a top-level epic's title down to its label ("MEMORY — …" →
// "MEMORY"); the catch-all epic is named by the caller.
func epicName(key string, byID map[string]bd.Issue) string {
	if key == looseKey {
		return "No epic"
	}
	if is, ok := byID[key]; ok {
		title := is.Title
		if i := strings.Index(title, " — "); i >= 0 {
			title = title[:i]
		}
		return strings.TrimSpace(title)
	}
	return key
}

// colorEpics assigns each epic a stable color by sorted key, so epic colors hold
// steady across requests regardless of weight order.
func colorEpics(epics []Epic) {
	keys := make([]string, len(epics))
	for i := range epics {
		keys[i] = epics[i].Key
	}
	sort.Strings(keys)
	color := make(map[string]string, len(keys))
	ci := 0
	for _, k := range keys {
		if k == looseKey {
			color[k] = looseColor
			continue
		}
		color[k] = epicPalette[ci%len(epicPalette)]
		ci++
	}
	for i := range epics {
		epics[i].Color = color[epics[i].Key]
	}
}

// sortStories / sortEpics order children largest-first, ties broken by id/key for
// a deterministic layout across requests.
func sortStories(stories []Story) {
	sort.SliceStable(stories, func(a, b int) bool {
		if stories[a].Open != stories[b].Open {
			return stories[a].Open > stories[b].Open
		}
		return stories[a].ID < stories[b].ID
	})
}

func sortEpics(epics []Epic) {
	sort.SliceStable(epics, func(a, b int) bool {
		if epics[a].Open != epics[b].Open {
			return epics[a].Open > epics[b].Open
		}
		return epics[a].Key < epics[b].Key
	})
}

// layoutEpics squarifies the epics into the map's 0–100 space by their open
// weight.
func layoutEpics(epics []Epic) {
	weights := make([]float64, len(epics))
	for i := range epics {
		weights[i] = float64(epics[i].Open)
	}
	rects := squarify(weights)
	for i := range epics {
		epics[i].Rect = rects[i]
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

// buildStory turns a root id and its live members into a Story. The name comes
// from the root issue when known; beads sort priority-asc then most-recent.
func buildStory(rootID string, members []bd.Issue, byID map[string]bd.Issue, reg jtbd.Registry) Story {
	st := Story{ID: rootID, Open: len(members)}
	if root, ok := byID[rootID]; ok {
		st.Name = root.Title
	} else {
		st.Name = rootID
	}
	st.Beads = make([]Bead, 0, len(members))
	for i := range members {
		b := NewBead(&members[i])
		b.ResolveJTBD(members[i].Description, reg)
		if b.Type == "bug" {
			st.Flag = true
		}
		st.Beads = append(st.Beads, b)
	}
	sortBeads(st.Beads)
	return st
}

// sortBeads orders a story's beads. An untouched story (no bead manually ranked)
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
			// A bead created into an already-ranked story has no rank yet
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

// layoutStories fills each story's Rect by squarifying their open weights into
// the epic body's 0–100 space.
func layoutStories(stories []Story) {
	weights := make([]float64, len(stories))
	for i, st := range stories {
		weights[i] = float64(st.Open)
	}
	rects := squarify(weights)
	for i := range stories {
		stories[i].Rect = rects[i]
	}
}
