---
id: cff7acda1209
type: project
name: strand-design
tags:
    - project
origin: index.reconcile
created: "2026-06-20T16:56:13-04:00"
updated: "2026-06-20T20:05:00-04:00"
importance: 5
cssclasses:
    - project
last_distilled: "2026-06-20T00:00:00Z"
project: strand
repo: github.com/dkoosis/strand
status: converging
trixiQuality: 0.43
trixiReads: 0
trixiViews: 0
---
# strand — Design Spec

A human-friendly planning layer over [beads](https://github.com/steveyegge/beads) (`bd`).

> **Format note:** §1–§5 are the *why/shape/feel* preamble. §6 onward is the behavioral
> spec in OpenSpec style (Requirement SHALL → WHEN/THEN scenarios), per the
> `example-real-spec` template (nug `8dba802302a2`). Scenarios are acceptance tests:
> a capability isn't "done" until its scenarios pass.

---

## 1. Vision


> [!TIP] strand
> strand is a human-friendly planning layer over beads, the agent-friendly issue tracker.

beads is **deliberately agent-centric**. Yegge's own guidance: beads tracks the work
agents do; for *human* project planning, use something like Linear / Asana / Jira. That leaves a
solo human staring at a CLI built for robots.

**strand is the missing middle.** Just enough planning affordance for one human to
triage, prioritize, see structure, and decide what to do next — without leaving the
beads data and without standing up a "real" PM tool.

Most of what makes Linear *feel* good for a solo dev is a handful of views
(a board, a ranked queue, a dependency picture, fast edits) — not the 90% of PM-suite
surface area. strand provides that in a simple single local binary.

## 2. Non-goals

- **Not** a beads reimplementation. strand never touches Dolt or JSONL; `bd` is the
  only write path. If `bd` can't do it, strand doesn't either.
- **Not** multi-user / collaborative. Single human, local, one session, *embedded beads only; not server* No auth, no realtime sync, no comments-as-chat.
- **Not** a full PM suite. No sprints-with-burndown, no time tracking, no Gantt, no
  custom workflow engines.
- **Not** a real-time dashboard for monitoring the work of agent swarms.
- **Not** an agent tool. Agents already have `bd`. strand is for the human in the loop.

## 3. Usage context

- **User:** dk, solo, on macOS, many repos under `~/Projects` each with its own `.beads`.
- **Cadence:** opens strand to plan a session, triage the queue, or check "what's next"
  — minutes at a time, clarify or correct a bead or epic, several times a day.
- **Mental model:** "show me this repo's work the way a planning tool would, let me
  reshape it, write it back to beads."

## 4. Architecture

```
┌────────── browser (localhost) ──────────┐
│  web/  htmx + token CSS, embedded assets │
└───────────────┬──────────────────────────┘
                │ HTML fragments over HTTP (htmx swap)
┌───────────────▼──────────────────────────┐
│  internal/server  html/template + static  │
│  internal/bd       shells out to `bd`,     │
│                    parses JSON, no Dolt    │
│  internal/registry repo discovery/switch   │  ← new
└───────────────┬──────────────────────────┘
                │ exec (two JSON-emitting CLIs)
        ┌───────▼────────┐   ┌──────────────┐
        │   bd  CLI      │   │   bv  CLI    │  ← analytics (D7)
        │  data + CRUD   │   │ --robot-*    │     PageRank, graph,
        └───────┬────────┘   └──────┬───────┘     bottlenecks, health
                └─────────┬─────────┘
                  → .beads (Dolt)  (bv read-only)
```

Decisions:
- **D1 — `bd` is the sole data path.** Every read and write is a `bd` invocation.
  No SQL, no Dolt API. Keeps strand immune to schema churn; cost is shell-out latency
  (acceptable for a local single-user tool).
- **D2 — Single self-contained binary, no build step.** Front-end embedded via `embed`;
  all assets vendored into `embed.FS` → `go build` → one binary. No npm/esbuild/
  node_modules, no build seam. Concrete stack in **D8**. The hard constraint: nothing that
  needs vite/webpack/a transpile — that build step is the seam we'd resent.
  *(At build time, drive the visual work with the `frontend-design` skill.)*
- **D3 — One *active* repo at a time.** A registry of known repos + a selector; all
  views scope to the active one. (Cross-repo aggregation is explicitly deferred — §9.)
- **D4 — Stateless server, thin client.** Server holds no session beyond the active-repo
  selection + registry file. Client re-fetches; no client-side store of record.
- **D5 — strand's own state lives outside beads.** Planning metadata that beads can't
  hold (see §6 R6 open question) must either map onto bd fields/labels or live in a
  strand sidecar. **Decision pending — gates R6 manual-ranking + planning-notes; must close
  before those are built (out of epic 1).**
- **D6 — Serialize every `bd` call (mutex).** beads' embedded Dolt store is a
  single-writer lock; concurrent `bd` invocations collide. The current scaffold runs them
  unguarded — a latent corruption/error bug the moment writes land. strand SHALL funnel
  all `bd` exec through one mutex'd helper. *(This is the #1 landmine from beads-ui's open
  PRs — accounted for up front, cheap; retrofitted, painful.)*
- **D7 — `bv` is the analytics/graph backend; `bd` is the data/CRUD backend.** strand
  shells out to *two* JSON-emitting CLIs. `bd` for reads + every write. `bv`
  (Dicklesworthstone's `beads_viewer`) for the expensive graph metrics — PageRank,
  betweenness, HITS, critical-path, cycles, label health — via its `--robot-*` JSON
  (`--robot-insights`, `--robot-triage`, `--robot-plan`, `--robot-graph`,
  `--robot-label-*`, `--robot-history`, `--robot-diff`). **strand computes no analytics
  itself**; the Insights/Graph/Stats views are renderers over `bv` JSON. Same immune-to-
  schema-churn property as D1. (`bv` is read-only — never a write path; writes stay `bd`.)
- **D8 — Tech stack: server-rendered HTML + htmx, no SPA framework.** strand is a Go
  server that renders data and writes back to `bd` — the server-rendered + light-JS shape,
  *not* a reactive SPA. So:
  - **Go stdlib `net/http` + `html/template`** — the server renders HTML (fragments +
    pages), not JSON. ≤10 routes, no web framework. `html/template` gives auto
    HTML-escaping for free (retires the hand-rolled `escape()` XSS handling).
  - **htmx** (one vendored JS file, no build) — read *and* write are fragment swaps:
    `hx-get` loads a view, `hx-patch`/`hx-post` fires a mutation and the server returns the
    updated row/card HTML to swap in place. Clean RU loop, minimal bespoke JS.
  - **SortableJS** (~12KB vendored) — kanban drag (V2). `onEnd` → `hx-patch` the moved
    card's new field value.
  - **Alpine.js** (~15KB vendored) — *only if* local UI state (panels, dropdowns) gets
    fiddly. Defer; add when felt, not before.
  - **Hand-authored token CSS** — one `app.css` on CSS custom properties (type/space/color
    scales, light+dark) per §4a. ✗ classless frameworks (Pico/water) — generic-by-design,
    can't hit dk's data-ink bar.
  - **Graph lib** (Cytoscape or d3-force) — V3 only; defer the pick to V3.

  Rationale: every real SPA framework (React/Vue/Svelte) brings a build step → breaks D2,
  for reactive state we don't need across three simple views. htmx matches the actual
  shape and stays single-binary. **Adopt htmx from P1** (server renders HTML from the
  start) rather than building a JSON+vanilla read layer and pivoting at V2 — the codebase
  is tiny now (~90-line server, ~99-line `app.js`), so the pivot is cheap today, costly once
  views pile up. *Escape hatch if reactivity is ever genuinely needed: Preact + htm over
  ESM — still single-binary, no transpile. Anything needing a bundler is out.*

### Verified `bd`/`bv` command map (bd 1.0.5)

Probed, not assumed. These are the exec strings strand issues.

| Action | Command | Notes |
|--------|---------|-------|
| list | `bd list --json [--status S]` | array even for one |
| ready | `bd ready --json` | actionable queue |
| show | `bd show <id> --json` | full record |
| **update field** | `bd update <id> -s/-p/-a/--title/-d/--design …` | RU core; `-p` is 0–4 |
| **claim** | `bd update <id> --claim` | atomic, idempotent |
| close / reopen | `bd close <id> [-r reason]` / `bd update <id> -s open` | |
| **create** | `bd create "<title>" --type T -p N [-d desc]` | returns new id |
| **delete** | `bd delete <id> --force` | **bare = preview** (free confirm); `--cascade`, `--dry-run` |
| comments (read) | `bd comments <id> --json` | *not* `comments list` |
| comment (add) | `bd comment <id> "text" --json` | |
| dep add | `bd dep add <blocked> <blocker>` | **not `bd link`** (spec was wrong) |
| dep remove | `bd dep remove <blocked> <blocker>` | |
| dep list | `bd dep list <id> --json` | |
| insights/stats | `bv --robot-insights` / `--robot-triage` | PageRank, bottlenecks, health |
| graph | `bv --robot-graph` | JSON/DOT/Mermaid, subgraph extraction |

**Footgun (must guard):** `bd update`/`bd close` with **no ID** mutates the *last-touched*
issue. strand SHALL always pass an explicit ID. **auto-commit:** this repo's
`dolt.auto-commit=on` (writes persist); global default is `off` — strand should not assume.
strand SHALL probe `bd config get dolt.auto-commit` (or equivalent) for the active repo on
startup; if `off`, surface a persistent banner ("writes won't persist") rather than silently
mutating into a void. No attempt to force-commit on bd's behalf — that's bd's contract, not
strand's.

## 4a. UI design principles `[settled]`

dk is picky about UI. The bar: **a strong, contemporary, clarity-first interface that
respects Tufte's data-ink ratio** — maximize the ink that carries information, cut the rest.
This is a first-class constraint on every view, not polish bolted on at the end.

- **P1 — Data-ink.** Every pixel earns its place by conveying a bead's data. ✗ decorative
  borders/cards/shadows/gradients. Grouping by whitespace + hairline rules, not boxes.
- **P2 — Color = signal, never decoration.** Saturated color is reserved for meaning:
  priority and status (a chip + a dot). Everything else lives on a grayscale ramp. A glance
  reads state from color alone.
- **P3 — Density done right.** Compact rows (Linear/Height register), but breathing room via
  a real space scale. Tabular numerals for IDs, counts, dates so columns align.
- **P4 — Typographic hierarchy over chrome.** One clean variable sans; a tight type scale
  carries structure. Weight/size/spacing do the work boxes would otherwise do.
- **P5 — Inline micro-charts (Tufte).** Sparklines / small multiples for graph scores and
  trends, rendered inline in the table (bv already emits unicode sparkline data; strand
  draws crisp inline SVG). Information at the point of need, no separate chart trip.
- **P6 — Calm motion.** Animation only when it explains a state change (drag, transition,
  popover). No ambient movement.
- **P7 — Light + dark, both first-class**, system-aware; tokens make this one variable set.
- **P8 — Keyboard-navigable.** Master/detail is `j/k`/arrow driven; the mouse is optional.

*Reference register: Linear, Height, Superhuman — dense, quiet, fast. ✗ Bootstrap/Material
generic-SaaS look.* A V1 visual spike (static mockup) gets dk's sign-off **before** the
view beads are built — taste locked early, not discovered mid-implementation.

## 5. Data model

### From bd (read; mirrors `bd …--json`)
`Issue { id, title, status, priority(0–4), issue_type, description, design, assignee,
labels[], created_at, updated_at, dependency_count, dependent_count, comment_count }`
plus, for the graph, dependency edges (`bd dep`/`bd show` relations).

### strand-owned
`Repo { name, path, prefix, last_used }` — the registry, persisted to
`~/.config/strand/repos.json` (XDG; honor `$XDG_CONFIG_HOME` — O9, locked). Discovered by
scanning `~/Projects/*/.beads` and/or explicit add. **✗ `os.UserConfigDir()`** — it resolves
to `~/Library/Application Support` on macOS (dk's platform), defeating the XDG intent. Roll
the path by hand: `$XDG_CONFIG_HOME/strand`, else `~/.config/strand`, on every platform.

### Derived (computed, not stored)
- **Ready queue** — from `bd ready`.
- **Dependency graph** — nodes = issues, edges = blocks/blocked-by, for §6 R4.
- **Board columns** — issues bucketed by status (or a chosen dimension) for §6 R6.

---

## 6. Capabilities (behavioral spec)

> Status tags: `[settled]` design is clear, `[draft]` proposed, needs dk sign-off,
> `[open]` genuine fork, see §8.

> **Canonical decomposition axis = the V-views (R0 table + §10 build order), NOT the
> R-numbers.** R1–R6 below are *scenario sources*: each is tagged to the view(s) it feeds,
> and its scenarios become that view's acceptance tests. Cut beads per-V, never per-R.
> Mapping: R1→repo selector (cross-cutting) · R2,R3,R5→**V1** · R6 board→**V2** · R4→**V3**.

### R0 — View catalog (the product spine) `[settled]`

strand is **N views over one beads dataset**, mutation woven into the views. One repo
active at a time (D3); switching repo re-scopes every view. The views, in build order:

| # | View | Backend | Mutates? | Status |
|---|------|---------|----------|--------|
| V1 | **Tabular list + detail panel** — sortable/filterable table; select row → detail panel; edit fields, claim, comment, light create/delete inline | `bd` | yes (RU-heavy core) | **first — easiest, highest value** |
| V2 | **Kanban** — *pivotable on any bead field* (status/priority/assignee/type); columns = that field's values; drag card → `bd update --<field>` | `bd` | yes (drag writeback) | next |
| V3 | **Dependency graph** — blocks/blocked-by DAG; nodes sized by PageRank, cycles flagged | `bv --robot-graph` | navigate only | after V2 |
| V4 | **Insights / stats** — PageRank influencers, betweenness bottlenecks, critical-path keystones, label health, quick-ref counts (the `bv` six-panel dashboard) | `bv --robot-insights/-triage` | no | after V3 |
| V5 | **Calendar / gantt / timeline** — time-axis views over due/created/closed | `bd` (+`bv --robot-history`) | maybe | later (deferred §9) |

Design notes:
- **V2's pivot dissolves O1.** Because columns map to a *real bd field*, dragging is just
  `bd update --<field>` — no "planning state" sidecar needed for the board. (D5's sidecar
  question now only bites if a *non-bd* planning dimension is wanted later.)
- **V1 is the RU-heavy core** dk called out (Read+Update heavy; Create/Delete/claim light;
  comments yes; dep light). It absorbs old R2 (browse) + R5 (mutations).
- **V3/V4 are `bv` renderers** (D7) — strand fetches `--robot-*` JSON and draws; the nine
  graph metrics are precomputed by `bv`, not strand.

The R-requirements below are scenario sources tagged to views (see canonical-axis note).

### R1 — Repo selection `[settled]` · cross-cutting (own bead)
The system SHALL let the user choose which beads workspace is active.

#### Scenario: List known repos
- **WHEN** the user opens strand
- **THEN** the header shows a repo selector listing registered repos by name
- **AND** the most-recently-used repo is active by default

#### Scenario: Switch active repo
- **WHEN** the user picks a different repo from the selector
- **THEN** all views re-scope to that repo's beads
- **AND** the choice persists as last-used

#### Scenario: Discover repos
- **WHEN** the user triggers "find repos" (or on first run)
- **THEN** the system scans `~/Projects/*/.beads` and offers found workspaces to register

#### Scenario: No repos / empty
- **WHEN** no repo is registered or the active repo has no `.beads`
- **THEN** the system shows an actionable empty state (how to add/init), not an error dump

### R2 — Browse & filter issues `[settled]` · feeds **V1**
The system SHALL present the active repo's issues in a scannable list with filters.

**V1 table concreteness (resolves item-4 underspec):**
- **Columns:** priority (chip), id (mono, tabular), title, status (dot+label), type,
  assignee, dependent_count (sparkline-ready), updated (relative). id/counts/dates use
  tabular numerals (§4a P3).
- **Default sort:** priority asc (P0 first), then updated desc. Click a column header to
  re-sort; sort is server-side (the handler re-renders ordered rows).
- **Filters compose (AND):** status, type, assignee, label, and a free-text title/id
  search. Filtering is server-side (htmx `hx-get` with query params → re-rendered tbody).
- **Scale / pagination:** server-side filter+sort then **cap at 200 rows per response with
  a "load more"** (htmx `hx-get` appending the next page). Avoids shipping a 1000-row DOM;
  no client virtualization needed at single-user scale. Count reflects the full filtered
  set, not the page.

#### Scenario: List all
- **WHEN** the user selects the "All" view
- **THEN** issues render with the columns above
- **AND** the count is the full filtered total (even when paged)

#### Scenario: Compose filters
- **WHEN** the user sets status = open AND type = bug
- **THEN** only open bugs render, server-side, and the count updates

#### Scenario: Large repo paging
- **WHEN** the filtered set exceeds the page cap
- **THEN** the first page renders with a "load more" control that appends the next page

#### Scenario: Open detail
- **WHEN** the user clicks an issue
- **THEN** a detail pane shows full record (description, design, deps, comments count)

### R3 — "What's next" / ready queue `[draft]` · feeds **V1** (a saved filter/view)
The system SHALL surface the actionable queue — issues with no unmet blockers — ranked
for human decision.

#### Scenario: Show ready
- **WHEN** the user opens the "Ready" view
- **THEN** issues from `bd ready` render, sorted by priority then age
- **AND** each shows why it's ready (no open blockers)

#### Scenario: Rank reflects priority + dependents
- **WHEN** two ready issues share a priority
- **THEN** the one unblocking more work (higher dependent_count) sorts first
- *(open: is dependent-count the right tiebreaker, or explicit manual rank? → §8)*

### R4 — Dependency visualization `[draft]` · feeds **V3** (epic 2)
The system SHALL show the blocks/blocked-by structure that `bd`'s CLI renders poorly.

#### Scenario: Graph for an issue
- **WHEN** the user opens an issue's "graph" view
- **THEN** the system shows it with its blockers (upstream) and dependents (downstream)
- **AND** clicking a node navigates to that issue

#### Scenario: Epic / chain overview
- **WHEN** the user opens an epic or a chain root
- **THEN** the system renders the dependency tree beneath it with status coloring

#### Scenario: Cycle / anomaly surfacing
- **WHEN** the dependency data contains a cycle or an orphaned blocker
- **THEN** the system flags it visibly rather than rendering a broken graph
- *(open: render lib — server-computed layout vs client (d3/cytoscape)? → §8)*

### R5 — Mutations (read+write client) `[draft]` · feeds **V1**
The system SHALL write changes back through `bd`: create, close, reopen, set
status/priority, edit fields, add/remove dependencies. All write commands per the verified
map (§4) — `bd update`, `bd close`, `bd create`, `bd delete`, `bd comment`,
`bd dep add`/`bd dep remove`. There is **no `bd link`**.

#### Scenario: Close an issue
- **WHEN** the user closes an issue from the UI
- **THEN** strand runs `bd close <id>` and the view reflects the new status
- **AND** a failure from `bd` surfaces as a readable message, leaving UI state honest

#### Scenario: Set priority / status
- **WHEN** the user changes priority or status
- **THEN** the corresponding `bd` command runs and the change is confirmed by re-read

#### Scenario: Create an issue
- **WHEN** the user fills the create form (title, type, priority, description)
- **THEN** strand runs `bd create …` and the new issue appears with its assigned id

#### Scenario: Add a dependency
- **WHEN** the user declares "A blocks B" (A must finish before B)
- **THEN** strand runs `bd dep add B A` (`<blocked> <blocker>`) and both issues' relations
  update
- *(remove → `bd dep remove B A`)*

#### Scenario: Optimistic-but-honest writes
- **WHEN** any mutation is in flight
- **THEN** the UI may show a pending state, but only commits to the new value after
  `bd` returns success; on error it reverts and explains
- *(open: confirmation prompts for destructive ops like delete/close? → §8)*

### R6 — Planning layer (the middle ground) `[open]` ← the product's reason to exist
The system SHALL provide planning affordances beads lacks, mapped onto bd data where
possible. **This is the section to get right; below is a proposed starting set.**

> **Phasing (resolves item-6 over-scope):** only the **board pivot (#1)** is in **Phase 3 /
> V2** — it maps to a real bd field, O1 dissolved, no sidecar. Candidates **#2 manual
> ranking** and **#4 planning notes** depend on the undecided **D5 sidecar** and are
> **out of epic 1** — they wait until D5 closes (§8). #3 epic progress and #5 saved views
> are derivable from bd reads and can ride V1 opportunistically.

Candidate affordances (proposed — dk to cut/keep/add):
1. **Board view** — kanban columns pivoted on a real bd field (status default),
   drag a card to set that field (→ `bd update -<field>`). *Phase 3 / V2.*
2. **Manual ranking within a queue** — a human "next up" order that bd's integer
   priority can't express finely. *(Storage open: bd label? sidecar? → D5/§8. Out of epic 1.)*
3. **Epics / milestones with progress** — group child beads, show % done, surface the
   epic's critical path.
4. **Human planning notes** — a place to think that isn't an agent-facing `description`.
   *(Open: append as a distinct `bd note`/comment, or strand sidecar? → D5.)*
5. **Saved views / filters** — e.g. "P0-P1 ready in this epic."

#### Scenario: Board reflects and writes status
- **WHEN** the user drags a card to another column
- **THEN** strand issues the matching `bd` state change and the board reflects it

#### Scenario: Epic progress
- **WHEN** the user opens an epic
- **THEN** the system shows child completion (closed / total) and what's blocking it

*(Remaining R6 scenarios intentionally unwritten until the affordance set is chosen — §8.)*

---

## 7. Quality / cross-cutting requirements `[draft]`

- **Q0 How scenarios are verified (the test strategy).** Each R-scenario is a bead's
  acceptance test, exercised the same way: **Go `httptest`** drives the htmx handler with
  the scenario's request (query params / form / patch), and the test **asserts on the
  returned HTML fragment** — substring/structural checks for the happy path, golden-fragment
  comparison where layout matters. The `bd` boundary is faked (a stub `Client`) so tests
  don't shell out. `requires_test: true` beads ship with these. Manual click-through is a
  supplement, not the gate.
- **Q1 Latency:** any view's first paint SHALL tolerate `bd` shell-out cost; show a
  loading state, never a frozen UI. Bound each `bd` call (timeout) so a hung CLI can't
  wedge a request. *(server already does 10s.)*
- **Q2 Honest errors:** every `bd` failure SHALL reach the user as `bd`'s own message,
  not a generic 500. UI state never claims a change that didn't land.
- **Q3 Safety:** read is free; writes are explicit. Destructive ops (delete) SHALL
  confirm. strand SHALL NOT mutate a repo that wasn't explicitly selected.
- **Q4 No lock-in:** all strand-owned state (registry, any sidecar) SHALL be plain files
  the user can read/delete; removing strand leaves beads untouched.
- **Q5 `bd` invocation discipline (the landmines).** Every `bd` call SHALL: (a) pass the
  active repo's **cwd** (already done via `Client.Dir`); (b) go through the **serialized
  mutex** helper (D6); (c) use `exec.Command` with **no shell** (already done — no
  injection surface); (d) use the **correct subcommand** for each write. These are exactly
  what beads-ui's open PRs were fixing; we go in knowing them.
  - *Status writeback is `bd update -s <status>` (O7 RESOLVED — no `set-state` in bd 1.0.5).
    There is no `set-state` subcommand anywhere in strand.*

## 8. Open decisions (to pin before/while building)

| # | Decision | Options | Status |
|---|----------|---------|--------|
| O1 | Planning-state storage (D5) | bd labels/fields / sidecar / hybrid | **mostly dissolved** — V2 pivots on real bd fields (R0); revisit only for a non-bd planning dimension |
| O2 | Ready-queue tiebreaker (R3) | dependent-count vs manual rank vs both | leaning both (auto default, manual override) — defer, not V1 |
| O3 | Graph rendering (V3/R4) | server-computed vs client lib | client lib over `bv --robot-graph` JSON; pick lib at V3 |
| O4 | View build order (R0) | — | **RESOLVED**: V1 tabular+detail first, then V2 kanban, V3 graph, V4 stats (R0 table) |
| O5 | Mutation confirmations (V1/V2) | all / destructive-only / none | destructive-only; delete uses `bd delete` bare-preview as the confirm |
| O6 | Repo discovery scope (R1) | `~/Projects/*` scan vs explicit-add | scan + add |
| O7 | Status-writeback subcommand (Q5) | `bd update -s` vs `bd set-state` | **RESOLVED**: `bd update -s <status>`; no `set-state` in bd 1.0.5 |
| O8 | `bv` availability | required vs optional dependency | **decide**: degrade V3/V4 gracefully if `bv` absent, or hard-require? Leaning optional (V1/V2 work without it) |
| O9 | Registry config path | locked | **RESOLVED**: `~/.config/strand/repos.json` (XDG; honor `$XDG_CONFIG_HOME`). Low-pri follow-ups: detect prefix collisions across registered repos; prune/flag stale paths on load |

**Two decisions gate epic 2, neither gates epic 1 — close before starting V3+:**
- **D5 (sidecar)** gates R6 manual-ranking + planning-notes. Until closed, those stay out.
- **O8 (`bv` required vs optional)** gates Phases 4–5 (V3 graph, V4 insights).

## 9. Explicitly deferred

- **Cross-project aggregation.** One unified view across all repos. Real want (§2 hints),
  big lift (cross-repo identity, merged graphs). Revisit after single-repo is solid.
- **Remote/hosted strand.** localhost only for now.
- **Live/auto-refresh dashboard.** Manual refresh first.

## 10. Build order (proposed)

Ordered by dk's "first and easiest," views as the spine (R0).

**Two hard blockers the DAG must enforce as bead edges (not prose):**
- **§4a visual spike sign-off** blocks every view-build bead — taste locks before UI code.
- **D6 mutex** blocks every write bead — the scaffold runs `bd` unguarded; no write lands
  until exec is serialized.

*(Both are encoded in the epic-1 DAG: spike `strand-5ri.1` → read `…2`; mutex `…4` → writes `…5`.)*

1. **Phase 0 — done:** scaffold, read API, basic list/detail (current `main`).
   *(Built JSON+vanilla; D8 pivots the read layer to html/template + htmx — do it at P1
   start while the surface is ~90+99 lines, not after views pile up.)*
2. **Phase 1 — V1 tabular list + detail (read), htmx-native:** server renders HTML via
   `html/template`; htmx (`hx-get`) loads list + detail fragments. Sortable/filterable
   table, detail panel, repo selector + registry (R1). Pin O6. This phase establishes the
   server-rendered shape every later view builds on.
3. **Phase 2 — V1 writes (the RU-heavy core):** edit fields, claim, comments, light
   create/delete via `bd update/create/delete/comment`. Mutations are `hx-patch`/`hx-post`
   → server runs `bd`, returns updated row HTML to swap. Mutex (D6), honest errors (Q2),
   destructive-confirm (O5). *This is the heart of what dk asked for.*
4. **Phase 3 — V2 kanban (board pivot ONLY):** pivot-on-field columns, drag →
   `bd update -<field>` (SortableJS `onEnd` → `hx-patch`). Light dep editing rides here or
   in V1. **Out of scope:** R6 manual-ranking + planning-notes (gated on D5) — not in this
   epic.
5. **Phase 4 — V3 dependency graph:** client lib over `bv --robot-graph`. Pin O3, O8.
6. **Phase 5 — V4 insights/stats:** render `bv --robot-insights/-triage` (PageRank, etc.).
7. **Later (§9):** V5 calendar/gantt/timeline; cross-repo aggregation.

---

*Epic 1 (Phases 1–3, V1 read → V1 write → V2 kanban) is filed: **`strand-5ri`** + children
`.1`–`.7`. Phases 4–5 (`bv` views) become epic 2 — held until **D5** (sidecar) and **O8**
(`bv` required/optional) close (§8).*
