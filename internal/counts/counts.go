// Package counts computes the shared bead-count cache (counts.json) that the
// Claude Code status line and strand's masthead both read. It is the write side of
// the contract internal/bdcounts reads: one process derives every bucket, so the
// two surfaces can never disagree (st-p1f).
//
// It exists to kill a predicate-drift class. The buckets used to be derived in jq
// by bd-counts-refresh.sh, which re-implemented strand's human-gate + lane rules and
// drifted from them (the badge showed 5 while the Waiting pane listed 7). Here the
// ◆/○/● counts come straight from internal/insight.Lanes — the SAME partition the
// pane renders — so the badge is definitionally the set the pane lists (st-2fy.2).
package counts

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/insight"
	"github.com/dkoosis/strand/internal/strandmd"
)

// Row is one repo's counts.json entry. The json tags + field order match the object
// bd-counts-refresh.sh emitted, so every consumer (the status line, bdcounts.Reader)
// reads a strand-written file unchanged: bh=◆ waiting, bo=○ open, bw=◐ in_progress,
// bb=● blocked, bcl=✓ closed, bdf=❄ deferred; eid/epct = the station bar's phase.
type Row struct {
	Root   string `json:"root"`
	Prefix string `json:"prefix"`
	BH     int    `json:"bh"`
	BO     int    `json:"bo"`
	BW     int    `json:"bw"`
	BB     int    `json:"bb"`
	BCl    int    `json:"bcl"`
	BDf    int    `json:"bdf"`
	EID    string `json:"eid"`
	EPct   *int   `json:"epct"` // nil → JSON null: no roadmap id maps to a live epic
	TS     int64  `json:"ts"`
}

// source is the bd read surface a row derives from. *bd.Client satisfies it; tests
// stub it. The four reads mirror exactly what strand's masthead pulse and station
// bar compute from, so the cache and the live views share one derivation.
type source interface {
	List(ctx context.Context, opts bd.ListOpts) ([]bd.Issue, error)
	Deps(ctx context.Context, ids ...string) ([]bd.DepEdge, error)
	Stats(ctx context.Context) (bd.Stats, error)
	EpicStatus(ctx context.Context) ([]bd.EpicStatus, error)
}

// computeRow derives one repo's row: the six buckets from bd reads folded through
// insight.Lanes, plus the station phase. List/Deps/Stats are load-bearing — an error
// fails the row so the caller keeps the last-good entry rather than zeroing it. The
// station is best-effort: a repo with no roadmap epic still open/in-progress degrades
// to an empty station (eid "", epct null) — that's a real answer, not a failure. An
// EpicStatus *read* failure is different: it carries prevEID/prevEPct forward (st-2fy.7,
// the previous row's station) rather than blanking a station a prior successful run
// already computed — matching the last-good semantics the buckets get from the caller.
// Only the two station fields are threaded through (not the whole prior Row) to keep
// the parameter light.
func computeRow(ctx context.Context, src source, root string, prevEID string, prevEPct *int) (Row, error) {
	issues, err := src.List(ctx, bd.ListOpts{})
	if err != nil {
		return Row{}, fmt.Errorf("list: %w", err)
	}
	ids := make([]string, len(issues))
	for i := range issues {
		ids[i] = issues[i].ID
	}
	deps, err := src.Deps(ctx, ids...)
	if err != nil {
		return Row{}, fmt.Errorf("deps: %w", err)
	}
	stats, err := src.Stats(ctx)
	if err != nil {
		return Row{}, fmt.Errorf("stats: %w", err)
	}
	bh, bo, bb := laneCounts(issues, deps)
	var eid string
	var epct *int
	if epics, err := src.EpicStatus(ctx); err == nil {
		eid, epct = station(strandmd.Roadmap(root), epics)
	} else {
		eid, epct = prevEID, prevEPct // read failed: carry the last-good station forward
	}
	return Row{
		Root: root, Prefix: prefix(root),
		BH: bh, BO: bo, BW: stats.InProgress, BB: bb,
		BCl: stats.Closed, BDf: stats.Deferred,
		EID: eid, EPct: epct,
		TS: lastTouched(root),
	}, nil
}

// laneCounts tallies the three human-facing lanes from insight.Lanes — the same
// partition strand's Waiting pane lists — so the badge ◆ count can't drift from it.
// ◐ (bw) is NOT taken here: it is the raw in_progress status total (stats.InProgress),
// which overlaps ◆ for a gated in-progress bead, matching the masthead.
func laneCounts(issues []bd.Issue, deps []bd.DepEdge) (bh, bo, bb int) {
	for _, l := range insight.Lanes(issues, deps) {
		switch l {
		case insight.LaneWaiting:
			bh++
		case insight.LaneOpen:
			bo++
		case insight.LaneBlocked:
			bb++
		case insight.LaneNone:
			// ◐ ungated in_progress / closed / deferred — not a human-facing lane;
			// bw/bcl/bdf come from Stats, not here.
		}
	}
	return bh, bo, bb
}

// station walks the roadmap ids in route order and returns the first that is still an
// open/in-progress epic, with its percent-done (closed/total children, rounded). eid
// "" and epct nil when no roadmap id maps to a live epic — the empty station.
func station(roadmap []string, epics []bd.EpicStatus) (string, *int) {
	byID := make(map[string]bd.EpicStatus, len(epics))
	for _, e := range epics {
		byID[e.Epic.ID] = e
	}
	for _, id := range roadmap {
		e, ok := byID[id]
		if !ok || (e.Epic.Status != bd.StatusOpen && e.Epic.Status != bd.StatusInProgress) {
			continue
		}
		pct := 0
		if e.TotalChildren > 0 {
			pct = int(math.Round(float64(e.ClosedChildren) / float64(e.TotalChildren) * 100))
		}
		return id, &pct
	}
	return "", nil
}

// prefix is the repo's short name: the Dolt database from .beads/metadata.json, or
// the directory basename when that's absent — matching the shell's derivation.
func prefix(root string) string {
	data, err := os.ReadFile(filepath.Join(root, ".beads", "metadata.json"))
	if err == nil {
		var m struct {
			DoltDatabase string `json:"dolt_database"`
		}
		if json.Unmarshal(data, &m) == nil && m.DoltDatabase != "" {
			return m.DoltDatabase
		}
	}
	return filepath.Base(root)
}

// lastTouched is the mtime (unix seconds) of the repo's .beads/last-touched, the
// change stamp the shell wrote as ts; 0 when the file is absent.
func lastTouched(root string) int64 {
	fi, err := os.Stat(filepath.Join(root, ".beads", "last-touched"))
	if err != nil {
		return 0
	}
	return fi.ModTime().Unix()
}
