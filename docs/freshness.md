# Viewer freshness model

Strand treats `bd`'s documented JSON output as the only database contract. It
does not read `.beads/issues.jsonl` and does not use Dolt files as evidence that
the issue set is current. The cheapest reliable check available from Beads is
therefore the snapshot itself: `bd list --json --limit 0`. A separate probe
would add a subprocess and still require this command after every change.

## Baseline (before the reconciler)

Code-path instrumentation tests showed that the first ordinary page opened one
`bd list` process and one background `bd dep list`; subsequent sorting,
filtering, and cross-view navigation used memory. A viewer write invalidated the
whole entry, so the next render synchronously paid another `bd list`. External
changes were inferred from a private Dolt manifest mtime and open browsers polled
`/pulse` every 15 seconds. The existing source documentation records typical
`bd list` latency as about 0.5 seconds; `bd` is unavailable in the development
container, so no misleading machine-specific wall-clock number was fabricated.

## Current contract

* One immutable, last-known-good snapshot is kept per repository. Refresh JSON
  is decoded completely by the bd client before atomic publication.
* Successful write responses are upserted (and deletes removed) immediately.
  Failed writes do not invalidate good data.
* One process-wide background reconciler runs `bd list --json --limit 0` every
  **2 seconds** for the active repository. Thus an external list-visible change
  is detected within 2 seconds plus the duration of one serialized bd command.
* Concurrent cold loads and periodic refreshes for a repository share one
  in-flight operation. A failed/cancelled/partial refresh retains the previous
  snapshot. Unchanged refreshes retain derived dependency/count folds.
* Ordinary warm reads, sorting, filtering, and navigation run in memory and
  spawn zero `bd` processes. Steady state is one `bd list` process per 2 seconds
  per Strand process, independent of browser/tab count; a viewer write is its
  write process only, rather than write plus a request-blocking reload.
* Server-Sent Events notify every open browser only after a changed snapshot is
  published. EventSource reconnects automatically, eliminating per-tab polling.
* The server owns the reconciler context and waits for it during `Stop`, so no
  refresh subprocess or publisher outlives shutdown.

The list snapshot invalidates when its canonical JSON value changes or a
successful viewer write supplies a newer issue. Derived dependency, closed, and
stats folds are retained across identical checks and discarded when a changed
list is published. Filesystem observation may be added as a wake-up hint, but it
must only request this same JSON refresh and must never publish or invalidate on
its own.
