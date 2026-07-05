package server

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/strand"
)

// rankWriter is the write slice seedRanks needs: just SetRank. Narrowing the
// rank-seed path to this seam mirrors readSource on the write side — the rest of
// the write surface (Update/Claim/Create/…) stays out of reach where only ranks
// are written, so the helper can't grow a stray write. Any IssueSource — the real
// *bd.Client, the caching wrapper, a test stub — satisfies it for free.
type rankWriter interface {
	SetRank(ctx context.Context, id string, rank float64) (*bd.Issue, error)
}

// Compile-time proof the fat source and its caching wrapper still satisfy the
// narrow rank-write seam, so seedRanks accepts whatever source() hands back.
var (
	_ rankWriter = (IssueSource)(nil)
	_ rankWriter = (*cachingSource)(nil)
)

// splitIDs parses a comma-separated id list, dropping blanks so a trailing comma
// or empty field never yields a phantom "" id.
func splitIDs(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// walkBeads visits every bead in the strand. It flattens the epic/story/bead
// nesting so callers read as one loop, not three.
func walkBeads(f strand.Model, fn func(strand.Bead)) {
	for ei := range f.Epics {
		for si := range f.Epics[ei].Stories {
			beads := f.Epics[ei].Stories[si].Beads
			for bi := range beads {
				fn(beads[bi])
			}
		}
	}
}

// groupRanks reads the current rank of every id in order from the strand and
// reports which ids the strand actually yielded (present) and whether the whole
// group is already manually ranked. A group that is not wholly ranked must be
// seeded, not midpoint-inserted (the SortBeads invariant). An id the strand never
// yielded (closed mid-drag, say) reads as not-ranked and not-present, so a partial
// group seeds over only its live members rather than mis-inserting.
func groupRanks(f strand.Model, order []string) (ranks map[string]float64, present map[string]bool, allRanked bool) {
	want := make(map[string]bool, len(order))
	for _, id := range order {
		want[id] = true
	}
	ranks = make(map[string]float64, len(order))
	present = make(map[string]bool, len(order))
	unranked := false
	walkBeads(f, func(b strand.Bead) {
		if !want[b.ID] {
			return
		}
		present[b.ID] = true
		if b.HasRank {
			ranks[b.ID] = b.Rank
		} else {
			unranked = true
		}
	})
	return ranks, present, !unranked && len(present) == len(order)
}

// movedID finds the one bead a single drag relocated: deleting it from the posted
// order yields the same sequence as deleting it from the prior (rank-sorted) order.
// A pure swap matches on either element; the first is a fine, stable choice.
func movedID(order []string, ranks map[string]float64) string {
	prior := make([]string, len(order))
	copy(prior, order)
	slices.SortStableFunc(prior, func(a, b string) int {
		if c := cmp.Compare(ranks[a], ranks[b]); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	for _, m := range order {
		if slices.Equal(without(order, m), without(prior, m)) {
			return m
		}
	}
	return order[0] // unreachable for a real single move; safe default
}

// without returns ids with the first occurrence of m removed.
func without(ids []string, m string) []string {
	out := make([]string, 0, len(ids))
	dropped := false
	for _, id := range ids {
		if !dropped && id == m {
			dropped = true
			continue
		}
		out = append(out, id)
	}
	return out
}

// rankFor computes the new rank for the moved bead from its neighbors in the
// post-drop order: the midpoint of the two it now sits between, or one step past
// the edge it now leads or trails. It returns renorm=true when the neighbors leave
// no representable gap (float exhaustion), the signal to reseed the whole group.
func rankFor(order []string, ranks map[string]float64, moved string) (rank float64, renorm bool) {
	j := slices.Index(order, moved)
	switch {
	case j <= 0: // new head: just below the next bead
		next := ranks[order[1]]
		r := next - 1
		return r, r >= next
	case j >= len(order)-1: // new tail: just above the prior bead
		prev := ranks[order[j-1]]
		r := prev + 1
		return r, r <= prev
	default: // interior: midpoint of the two neighbors
		prev, next := ranks[order[j-1]], ranks[order[j+1]]
		r := prev + (next-prev)/2
		return r, r <= prev || r >= next
	}
}

// seedRanks writes dense ranks 1..M over the live ids in order, making the group
// wholly rank-ordered. Used to seed an untouched group on its first drag and to
// renormalize when midpoint space runs out. An id absent from present (closed
// mid-drag) is skipped so no rank lands on a bead the strand no longer shows; the
// counter only advances on a write, keeping the survivors' ranks dense.
func seedRanks(ctx context.Context, src rankWriter, order []string, present map[string]bool) error {
	rank := 1
	for _, id := range order {
		if !present[id] {
			continue
		}
		if _, err := src.SetRank(ctx, id, float64(rank)); err != nil {
			return wrapWrite("rank", err)
		}
		rank++
	}
	return nil
}
