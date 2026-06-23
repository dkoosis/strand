# Triage — strand-alg.4 (R6 manual rank)

Two review passes: R-A plan-adherence, R-B server/correctness. R-A returned zero
findings (full adherence, no scope creep). R-B returned 2 P1, 0 P0, and confirmed
six paths sound (movedID fwd/back/swap, rankFor edge math, negative/zero ranks,
groupRanks allRanked gate, htmx 204-no-swap, cross-epic prevention, exec
serialization).

| # | Finding | Sev | Decision | Why |
|---|---------|-----|----------|-----|
| P1-1 | `_revert` is a single slot on the row element; two rapid drags before the first 204 returns → second `onEnd` stomps the closure, wrong revert fires on error | P1 | **Defer** → `strand-vd2` | Real but narrow (single-user localhost, ~10ms window). The identical pattern already shipped in `initBoard` (board); a correct fix covers list **and** board, which is outside this bead's V1-list scope. No asymmetric half-fix (D9). |
| P1-2 | `seedRanks` wrote a rank onto an id absent from the forest (closed mid-drag), since the seed path iterates the raw posted `order` | P1 | **Apply** | In-scope, in my new code, small, testable. `groupRanks` now returns a `present` set; `seedRanks` skips absent ids and keeps survivors dense. New test `TestRankSeedSkipsAbsentID`. |

## Deferred follow-ups filed
- `strand-vd2` (P2 bug) — drag-revert single-slot race, fix list + board together.
- `strand-45o` (P3 feature) — V2 kanban manual rank (scope-deferred at rehearsal; carries the P0 one-rank-can't-span-V1+board design note from plan review).

## Sound-path confirmations (no action)
movedID correctness incl. adjacent-swap and the `order[0]` non-single-move fallback;
rankFor head/tail/interior + float-exhaustion `renorm` guards; negative/zero head
ranks format + sort fine; concurrency stale-read window acceptable for a single-user
local tool (`execMu` serializes all bd calls).
