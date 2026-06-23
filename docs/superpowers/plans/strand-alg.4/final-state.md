# Final state — strand-alg.4 (R6 manual rank)

**Shape:** standard · **Profile:** craft · **Authority:** ship
**Outcome:** converged, PR opened, bead closed.

## Summary
Manual "next-up" ordering for the V1 bead list. Drag a row; the post-drop order
persists as a `rank` float in bd metadata (D5, no sidecar). The server is
authoritative: the client posts only the id order, the server re-reads ranks via
`buildForest` and writes the minimal change. An untouched epic keeps today's
(priority, id) order until the first drag, which seeds dense ranks 1..N
(pure-rank-after-seed); later drags midpoint-insert or step past an edge, and
renormalize the whole group only when float space runs out. Success → 204 (the
optimistic DOM already matches bd); a write error → non-2xx, the client reverts.

## Plan vs actual — file delta
All within plan scope; no out-of-plan files.

| File | Plan | Delta |
|------|------|-------|
| internal/bd/client.go | Metadata field + `Rank()` accessor | as planned |
| internal/bd/write.go | `SetRank` (set-metadata) | as planned |
| internal/bd/write_test.go | SetRank + Rank tests | as planned |
| internal/forest/forest.go | `Rank`/`HasRank` on Bead, `sortBeads` | as planned |
| internal/forest/forest_test.go | ranked-sort + tiebreak tests | as planned |
| internal/server/server.go | `SetRank` on IssueSource, `POST /bead/{id}/rank`, `handleRank` + helpers | as planned + `present`-set filter (triage P1-2) |
| internal/server/server_test.go | 6 acceptance tests | + `TestRankSeedSkipsAbsentID` (P1-2) |
| web/static/app.js | `initList` SortableJS + reorder POST + revert | as planned |
| web/templates/partials.html | `data-epic` tbody + `data-id` row | as planned |

## Review
- R-A plan-adherence: zero findings, full adherence, no scope creep.
- R-B server/correctness: 0 P0, 2 P1 (see triage.md). P1-2 applied + tested;
  P1-1 deferred to `strand-vd2` (shared with already-shipped board pattern).
- Six paths confirmed sound (movedID, rankFor edges, neg/zero ranks, groupRanks
  gate, htmx 204-no-swap, cross-epic prevention).

## Quality gates
`make audit` green (vet, golangci-lint, go test -race, jscpd 0 clones, govulncheck
0 vulns, nilaway). All package tests green.

## Deferred follow-ups
- `strand-45o` (P3) — V2 kanban manual rank (scope-deferred; carries the P0
  one-rank-can't-span-V1+board design note).
- `strand-vd2` (P2) — drag-revert single-slot race, fix list + board together.

## Shape/scope notes
Classified standard (missing-metadata fallback). dk scoped to **V1 list only** at
the rehearsal banner (V2 deferred), resolving the plan-reviewer's P0 design flaw
(one rank float can't order a bead across V1 + 3 board pivot columns).

## north_star_answer
_pending dk_ — SR recenter question posed in the receipt.
