# concurrency-safety — strand (repo)

RUN_ID: 2af4cc879761 · commit 163908b · scope: project · mode: report

## Pre-work

`go test -race -timeout=3m ./...` — **clean**, all packages pass under the race detector:

```
ok  github.com/dkoosis/strand/internal/bd        4.346s
ok  github.com/dkoosis/strand/internal/forest    1.305s
ok  github.com/dkoosis/strand/internal/graph     1.485s
ok  github.com/dkoosis/strand/internal/registry  1.666s
ok  github.com/dkoosis/strand/internal/server    2.527s
```

No confirmed data race anywhere → no P0 finding. The concurrency surface is small and well-disciplined: one process-wide bd-exec mutex (`internal/bd`), one registry mutex (`internal/registry`), per-request contexts with deferred cancel (`internal/server`), and a single process-lifetime shutdown goroutine in `main`. The notable design call — serializing every bd call behind one global lock held across `cmd.Run()` — is correct and documented (beads' embedded Dolt store is single-writer); `TestExecSerialized` pins it. Not a finding.

One borderline item: the registry mutex is held across disk I/O.

---

### 1. [F1] `internal/registry/registry.go:124` — lock-held-during-io

**Diagnosis.** `Registry.mu` is held across file-system I/O. `Switch`, `Add`, and `Rescan` all call `saveLocked()` — which does `os.MkdirAll` + `os.WriteFile` — while holding the same mutex that every read (`Active`, `Repos`) takes.

**Why.** The lock's stated job is protecting in-memory state so "concurrent HTTP requests can read and switch safely" (the `Registry` doc comment), not deliberately serializing the disk write. Every HTTP request resolves the active repo through `s.reg.Active()`, which blocks on `r.mu`. While a `Switch`/`Add`/`Rescan` is mid-write — `MkdirAll` then a `WriteFile` of the whole repo list — all concurrent reads stall behind that disk op. The blast radius is bounded (strand is a single localhost user, the file is tiny), so this is latent contention, not a live problem — hence borderline.

**Evidence.** `internal/registry/registry.go:114-127` (`Switch`):
```go
func (r *Registry) Switch(path string) (Repo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	...
	r.sortLocked()
	if err := r.saveLocked(); err != nil {
		return Repo{}, err
	}
```
and `saveLocked` does the I/O under that held lock — `internal/registry/registry.go:241,248`:
```go
	if err := os.MkdirAll(filepath.Dir(r.file), 0o755); err != nil {
		...
	if err := os.WriteFile(r.file, data, 0o600); err != nil {
```
Same pattern in `Add` (`saveLocked` at :151) and `Rescan` (:164).

**Fix.** Snapshot the repo slice under the lock, release, then marshal + write outside it:
```go
func (r *Registry) Switch(path string) (Repo, error) {
	r.mu.Lock()
	... // mutate in-memory state, sortLocked
	out := r.repos[r.indexOf(path)]
	data, err := r.marshalLocked()
	r.mu.Unlock()
	if err != nil { return Repo{}, err }
	if err := writeFile(r.file, data); err != nil { return Repo{}, err }
	return out, nil
}
```
This keeps the critical section to in-memory work and lets reads proceed during the disk write. Two writers could then race on the file itself; serialize them with a separate `writeMu` (not the state mutex) or accept last-writer-wins, which the MRU/active model tolerates. Note: this changes the failure ordering slightly (a save error surfaces after the in-memory mutation commits) — call it out if you take the fix.

Alternative: if serializing the write *is* the intent, say so in the `saveLocked` comment ("the lock also serializes the config write to a single-writer file") and this drops off the list entirely, mirroring how `internal/bd` documents its global exec lock.

**Tier.** borderline
