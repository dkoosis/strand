package bd

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
