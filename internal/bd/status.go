package bd

import "slices"

// Status is a bead's lifecycle state — the domain vocabulary strand reasons about
// most (triage, the kanban pivot, the strand filter, the stale check). bd is the
// source of these values, so the named type and its closed set live here; strand
// and server read these constants rather than re-spelling the literals, so a typo
// or a drift from bd's emitted spelling is a compile error, not a silent miss.
type Status string

const (
	StatusOpen       Status = "open"
	StatusInProgress Status = "in_progress"
	StatusBlocked    Status = "blocked"
	StatusClosed     Status = "closed"
	StatusDeferred   Status = "deferred"
)

// IssueType is a bead's kind. Like Status it is a closed vocabulary bd owns, so a
// named type plus the IssueTypes set is the single source the create clamp and any
// type dropdown both read — adding a kind means editing this set and nothing else.
// Typing it turns a drifted or misspelled kind into a rejected value (IssueType.Valid)
// rather than a string bd silently refuses or, worse, a dropdown that omits a real kind.
type IssueType string

const (
	IssueTypeTask    IssueType = "task"
	IssueTypeBug     IssueType = "bug"
	IssueTypeFeature IssueType = "feature"
	IssueTypeStory   IssueType = "story"
	IssueTypeEpic    IssueType = "epic"
	IssueTypeChore   IssueType = "chore"
)

// IssueTypes is the ordered closed set of issue kinds — the one source a clamp and
// a dropdown share. Order is display order. Valid scans it, so membership and the
// list can never drift apart.
var IssueTypes = []IssueType{
	IssueTypeTask, IssueTypeBug, IssueTypeFeature,
	IssueTypeStory, IssueTypeEpic, IssueTypeChore,
}

// Valid reports whether t is a known issue kind.
func (t IssueType) Valid() bool { return slices.Contains(IssueTypes, t) }

// DepType is a dependency edge's kind. The graph view keeps only the real blocking
// edge (DepBlocks); epic hierarchy (DepParentChild) and soft links (DepRelatesTo)
// are filtered out. bd owns this closed set, so it lives here as a named type:
// a consumer that filters against DepBlocks gets a compile error on a typo, where
// a bare "blocks" misspelling would silently empty the graph view (the F2 hazard).
type DepType string

const (
	DepBlocks      DepType = "blocks"
	DepParentChild DepType = "parent-child"
	DepRelatesTo   DepType = "relates_to"
)

// DepTypes is the closed set of dependency-edge kinds.
var DepTypes = []DepType{DepBlocks, DepParentChild, DepRelatesTo}

// Valid reports whether t is a known dependency-edge kind.
func (t DepType) Valid() bool { return slices.Contains(DepTypes, t) }

// ID is a bead's identifier. It is a named type, not a bare string, so an id can't
// be transposed with a same-typed neighbour or filled from a non-id string without
// a deliberate conversion. Methods that take ids (DepAdd/DepRemove/SetParent/Create)
// still accept string at the package boundary today — they sit behind the server's
// IssueSource interface, so narrowing them to ID is a coordinated follow-up; this
// type establishes the concept and its validity check now.
type ID string

// Valid reports whether the id is non-empty — the same bar requireID enforces.
func (id ID) Valid() bool { return id != "" }
