# io-parallel — strand (RUN_ID 2af4cc879761)

**0 findings (action=0 borderline=0)**

## Verdict

No actionable sequential-independent-I/O findings. The codebase is structurally
immune to this smell, by an explicit design decision.

## Why nothing fires

strand shells out to the `bd` CLI for all data. Every invocation funnels through
one helper, and that helper holds a process-wide mutex:

`internal/bd/client.go:44-49`
```go
// execMu serializes every bd invocation process-wide. beads' embedded Dolt store
// is a single-writer lock — concurrent bd calls collide and can corrupt or error
// (spec D6/Q5: every bd call goes through one mutex'd helper). One global lock is
// the safest reading: it over-serializes across distinct repos, but strand is a
// single localhost user and that cost is nil next to a corrupted store.
var execMu sync.Mutex
```

`internal/bd/client.go:108-110`
```go
func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	execMu.Lock()
	defer execMu.Unlock()
```

bd's backing store is single-writer Dolt. Parallelizing bd calls is not a free
latency win — it is forbidden. Concurrent `bd` processes collide on the store
lock. The `sequential-db-reads` rule already carves out this case ("For SQLite,
parallelism may not help — note backend specifics"); Dolt single-writer is the
same story, stronger. Any errgroup over bd calls would queue on `execMu` and gain
nothing while risking store corruption.

## Call-site survey (every bd I/O site, all in the server layer)

All bd I/O lives in `internal/server/server.go`; the `forest`, `graph`,
`registry`, and `web` packages do no bd I/O. No bd call sits inside a `range`
loop, so there is no `loop-independent-io` fan-out either.

The handlers that make ≥2 bd calls were checked for independence:

- **handleGraph / graphModel** (`server.go:520,547`): `buildForest` → `List`,
  then `graphModel` → `Deps(ids...)`. `Deps` consumes the scope ids that `List`
  produces. Genuinely dependent — sequential by data flow.
- **handleInsights / insightsModel** (`server.go:678,705`): `List`, then
  `Deps(ids...)` over the listed ids. Same dependency. Sequential by data flow.
- **handleBead → renderDrawer** (`server.go:909,932`): `Show(pathID)`, then
  `Comments(issue.ID)`. The two are *data-independent* (the id is known from the
  path before `Show`), but (a) `execMu` serializes them regardless, so a split
  saves zero wall-clock, and (b) issuing `Comments` before confirming the issue
  exists wastes a bd process on a 404. Not a finding.
- **writeAndRefresh** (`server.go:1000-1008`): write, then `Show` only on the
  failure / silent-write branch. Ordered by intent (re-read bd's truth after a
  write) — explicitly serialized, the rule's "don't flag" carve-out.

## Note (not a finding)

`internal/registry/registry.go:257` `discover` walks the filesystem with
`os.Stat` per candidate `.beads` dir. That is local stat I/O, not the
latency-bound RPC/DB pattern io-parallel targets, and it runs at repo-discovery
time, not on a user-latency request path. Out of scope.
