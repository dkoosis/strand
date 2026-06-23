# strand-alg.4 — R6 manual rank (bd-metadata rank, drag-order) · plan v2

Shape: standard · Profile: craft · Branch: strand-alg.4

## Scope (post-review decision)

**V1 epic list only.** D5/R6#2 is "a human *next-up* order within a queue" — that queue is the
V1 list. A single bd-metadata `rank` float can't coherently order a bead across V1 *and* V2's
three pivot columns (status/priority/assignee) — a reorder in one surface would scramble the
others (plan-review P0). So V2 intra-column reorder stays the no-op it is today; the board's
drag-to-change-field (already shipped) is untouched. V2 manual rank, if ever wanted, is a
separate bead with per-group namespaced keys. Logged to final-state.md as a scope cut from the
bead's "V1 + V2" wording, approved by dk.

## Storage

Probe-confirmed: `bd update <id> --set-metadata rank=1.5 --json` stores `rank` as a JSON
**number** and echoes the updated metadata. Read at `.metadata.rank`.

## Ordering model — pure-rank-after-seed (resolves review findings 2 & 3)

A group (one epic's bead list) is in one of two states:
- **Unranked** — no bead has a `rank`. Order = today's `(priority, id)` (forest.go:174). Zero
  behavior change. This is every epic until someone drags.
- **Ranked** — *every* bead in the group has an explicit `rank`. Order = `(rank, id)`.

The transition is the **first drag in a group**: the server seeds dense ranks (`1, 2, 3, …`)
onto the post-drop order, so the group becomes purely rank-ordered. Mixing fractional ranks
with a priority fallback (the v1 plan's idea) is what made edge inserts collide with priority
floors — a pure-rank space has no floors, so `first-1` / `last+1` / midpoints are all safe.

## Reorder protocol

Client (SortableJS) posts only the moved id (URL) + `order` = the epic list's **post-drop id
sequence**. No `data-rank` in the DOM — the server is authoritative, re-reading ranks from bd,
so nothing can go stale.

`handleRank`:
1. `List` the active scope → map id→(rank, hasRank) for the moved bead's epic members.
2. Locate the moved id in `order`; its neighbors are `order[i-1]` / `order[i+1]` (may be absent
   at head/tail).
3. **Fast path** (group already fully ranked): newRank = midpoint of neighbor ranks
   (`(prev+next)/2`); head → `next-1`; tail → `prev+1`. Write one `SetRank`.
   - **Collision** = the float midpoint equals a neighbor (`mid==prev || mid==next`, no float
     room left). Epsilon-free. Falls to renormalize.
4. **Renormalize path** (group not yet fully ranked — first drag — OR collision): assign dense
   ranks `i+1` to every id in `order`; one `SetRank` per bead. N writes, rare (first drag +
   the occasional float-exhaustion).
5. Re-render the **list pane** for the scope and return it (swap `#listPane`) — shows bd's truth,
   re-binds SortableJS via the existing `htmx:afterSwap` hook (app.js:96).

N = one epic's beads (small); reorders are a human planning action (infrequent). Acceptable.

## Plan

1. **`bd.Issue` metadata** (`internal/bd/client.go`):
   `Metadata map[string]any \`json:"metadata,omitempty"\``; `func (i *Issue) Rank() (float64, bool)`
   reading `metadata["rank"]`, tolerating `float64` (real case) and `string` (defensive);
   `ok=false` when absent/unparseable.
2. **Write client** (`internal/bd/write.go`):
   `func (c *Client) SetRank(ctx, id string, rank float64) (*Issue, error)` →
   `update <id> --set-metadata rank=<FormatFloat(rank,'f',-1,64)> --json`. No entry in the
   `updateFlags` map (metadata is not a single-flag field). Caller treats a `(nil,nil)` return
   as "wrote silently"; handler re-reads via `List` regardless.
3. **Effective key** (`internal/forest/forest.go`):
   `Bead` gains `Rank float64` + `HasRank bool`; `NewBead` fills from `Issue.Rank()`.
   `buildEpic` sort: if any member has a rank, sort by `(rank, id)`; else unchanged `(priority,
   id)`. (Equivalent: `orderKey = rank if HasRank else +inf`, but the two-state branch is
   clearer and matches the seed invariant — a touched group is all-ranked.)
4. **Server** (`internal/server/server.go`):
   - `IssueSource` + stub gain `SetRank(ctx, id string, rank float64) (*bd.Issue, error)`.
   - `handleRank` per the protocol above.
   - Route: `s.mutate(mux, "POST /bead/{id}/rank", s.handleRank)`.
5. **List template** (`web/templates/partials.html`): the epic's `<tbody>` gets a marker
   (`data-epic="{{.Epic.ID}}"`) so the client can scope the `order` it posts. No rank attrs.
6. **Client** (`web/static/app.js`): bind SortableJS to each epic `<tbody>`; on drop, collect the
   tbody's post-drop row ids → POST `/bead/{id}/rank` with `order`, target `#listPane`,
   swap `outerHTML`. Add a `_revert` closure (the board path has one; the list needs its own) so
   a rejected write visually reverts. Reuse the existing `htmx:responseError` handler.

## Files

| File | Change |
|---|---|
| `internal/bd/client.go` | `Metadata` field, `Rank()` accessor |
| `internal/bd/write.go` | `SetRank` |
| `internal/bd/write_test.go` | argv + numeric round-trip |
| `internal/forest/forest.go` | `Bead.Rank/HasRank`, two-state sort |
| `internal/forest/forest_test.go` | ranked vs unranked sort cases |
| `internal/server/server.go` | interface widen, `handleRank`, route |
| `internal/server/server_test.go` | stub `SetRank`; seed-on-first-drag, midpoint insert, collision-renormalize, head/tail, error-reverts |
| `web/templates/partials.html` | `data-epic` on the list tbody |
| `web/static/app.js` | list-row SortableJS + reorder POST + revert |
| `web/static/app.css` | row drag affordance (only if needed) |

## Acceptance

- `make audit` green.
- httptest: first drag in an unranked epic seeds dense ranks (`SetRank` per bead) and a re-read
  returns the list in the dragged order.
- httptest: a drag in an already-ranked epic issues one `SetRank` with the midpoint rank; re-read
  sorted by it.
- httptest: head and tail drops compute `next-1` / `prev+1`; re-read keeps them at the edges.
- httptest: a float-exhaustion collision falls back to renormalize.
- httptest: a rejected `SetRank` returns non-2xx (client reverts); no partial reorder asserted.
- Default V1 order unchanged for any epic with no ranked bead.

## Out of scope (deferred, not folded in)

- V2 kanban intra-column manual rank (needs per-group namespaced keys) — follow-up bead if wanted.
- Any change to the board's drag-to-change-field path.
