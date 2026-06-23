# change-smells — strand (repo)

RUN_ID 2af4cc879761 · scope project · mode report

Fowler smells derivable from git history + the call graph. History is thin — **15 commits to the reviewed files in 6mo** (spec warns <30), so the git-mined axes (shotgun-surgery, divergent-change) are weak signals here and graded conservatively. The call graph is clean: deps form an acyclic DAG (`metrics-cycles.txt`: 0 nontrivial SCCs), no mutual imports (no **inappropriate-intimacy**), no `a().b().c()` chains (no **message-chains**), and the server handlers are a legitimate orchestration layer over forest+graph+bd (no **feature-envy** — spec exempts orchestration).

2 findings (action=1 borderline=1).

---

### 1. [F1] `internal/forest/forest.go:83` — primitive-obsession

**Diagnosis:** Bead status is a domain vocabulary (`open` / `in_progress` / `blocked` / `closed` / `deferred`) carried as a bare `string` everywhere, with no named type and no single owner. `server.go` names the values as constants; `forest.go` re-spells the same literals by hand.

**Why:** Status is the most-reasoned-about domain concept in strand (triage, kanban pivot, forest filter, stale-check). With it untyped and its literal spellings duplicated across packages, a typo or a drift between `bd`'s emitted value and a hand-written literal is a silent miss, not a compile error. `forest.go:83` and `forest.go:101` already hold an independent copy of the spellings that `server.go:49-55` declares as constants — two sources of truth for one vocabulary across two packages.

**Evidence:**
- `internal/forest/forest.go:83` — `	return status != "closed" && status != "deferred"`
- `internal/forest/forest.go:101` — `		if issues[i].Status == "in_progress" {`
- `internal/server/server.go:51` — `	statusInProgress = "in_progress"`
- `internal/server/server.go:54` — `	statusClosed     = "closed"`
- `internal/server/server.go:55` — `	statusDeferred   = "deferred"`

**Fix:** Promote the status vocabulary to one named type in the `bd` package (the source of the values), e.g. `type Status string` with `StatusOpen`, `StatusInProgress`, … exported constants, and have `forest`, `server`, and `bd` all read those. That removes `forest.go`'s hand-spelled copies and makes a status comparison type-checked rather than string-matched. (The dependency edge `Type` field — the `"blocks"` literal duplicated at `server.go:582,796` and `bd/client.go:72-73` — is the same pattern at smaller scale; fold it in if you touch this.)

**Tier:** action

---

### 2. [F2] `internal/server/server.go:1` — divergent-change

**Diagnosis:** `server.go` (1181 lines) accretes five unrelated rendering axes — list/forest, kanban board, dependency graph, insights dashboard, plus the CSRF guard and the full CRUD drawer — each landed by its own commit for its own reason.

**Why:** Every recent feature commit reopens this one file for an unrelated concern: V2 board, V3 graph, V4 insights, the app-wide CSRF guard. The data-model types (`boardView`, `graphData`, `insightsView`, `drawerData`) and their build helpers (`buildBoard`, `graphModel`, `insightsModel`) are independent feature slices sharing only the `listView` scope chrome. New view work keeps converging here.

**Evidence (6mo history of this file, distinct feature axes):**
```
feat(strand): app-wide CSRF/Origin guard on mutate routes (strand-a7w) (#12)
feat(strand): V4 insights/stats view (strand-alg.3) (#11)
feat(strand): V3 dependency-graph view — Cytoscape+dagre over gonum metrics (strand-alg.2) (#10)
feat(strand): web UI Quit button — graceful self-shutdown (strand-4fj) (#9)
feat(strand): kanban board — drag-to-mutate (V2) — strand-5ri.7
```
File-level co-change confirms the convergence: `server.go` pairs with `bd/client.go` 7×, `main.go` 6×, `web/embed.go` 5× over the window.

**Tier:** borderline. The commit verb is consistently `feat` (rule's "high churn but consistent verb" caveat), and the history is short and early-stage (the file is genuinely growing as V1→V4 ship, not rotting). Flagged as a watch item, not an act-now: when V5 lands, carve the per-view model+build helpers into `board.go` / `graph.go` / `insights.go` alongside their handlers so each view owns its file and `server.go` keeps only routing + the shared render/error/guard plumbing. Note: oversized-file proper is arch's call, not this linter's — the smell here is the divergent *reasons-to-change*, not the line count.

---

**Not flagged (verified absent or below threshold):**
- **shotgun-surgery** — top git pairs (`bd/client.go ↔ server.go` 7×) are the same feature being built out in an early-stage app; spec exempts same-feature and impl-pair co-changes.
- **data-clumps** — `(id, field, value string)` appears in only 2 signatures (`bd.Update` + the `IssueSource.Update` it backs), below the ≥4 threshold; `(ctx, id)` is an idiomatic pair, not a 3-clump.
- **feature-envy / inappropriate-intimacy / message-chains** — none in the call graph (acyclic DAG, no mutual imports, no chained calls).
