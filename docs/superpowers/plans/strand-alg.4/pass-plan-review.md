# pass-plan-review — strand-alg.4 (plan v1)

Reviewer: fresh general-purpose subagent, plan-only brief. Verdict: sound seam, correct
unranked-default proof, one P0 cross-group flaw + P1 gaps.

| # | Sev | Finding | Disposition |
|---|-----|---------|-------------|
| 1 | P0 | One `rank` float, four groups (V1 epic + V2 status/priority/assignee columns) → reorder in one surface scrambles others; priority-space midpoint meaningless in non-priority groups | **Accepted.** Surfaced to dk → chose V1-list-only. V2 cut to follow-up. |
| 2 | P1 | Edge insert `prev+1`/`next-1` collides with priority floors; mixing fractional rank + priority fallback is non-monotonic | **Accepted.** Fixed by pure-rank-after-seed model (seed dense ranks on first drag; no priority fallback in a touched group → edge-safe). |
| 3 | P1 | `data-rank` goes stale after renormalize; group markers unspecified; whole-pane re-render churns Sortable/Cytoscape | **Accepted, dissolved.** Server re-reads ranks from bd (authoritative) → no `data-rank` in DOM at all. Client posts `order` ids only. V1-only → list pane re-render doesn't touch the graph. |
| 4 | P1 | `update --json` metadata echo unverified; `SetRank` can return `(nil,nil)` | **Accepted.** Probe confirmed update echoes metadata; handler re-`List`s regardless and ignores `SetRank`'s returned issue. |
| 5 | P2 | Unranked-default proof correct | Confirmed; kept. |
| 6 | P2 | Route/guard/interface widening fit existing patterns; list-drag needs its own `_revert` | **Accepted** — added `_revert` for the list path to plan §6. |
| 7 | P2 | If renormalize deferred, edge inserts must not no-op into priority space | Moot — renormalize is in scope (seed path), not deferred. |

Plan revised v1→v2 incorporating all. No finding rejected.
