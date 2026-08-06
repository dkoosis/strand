package server

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/dkoosis/strand/internal/bd"
)

// snapshotCache holds one in-process read snapshot per repo, keyed by the repo's
// path. Each view (strand/list/board/insights) used to shell out to `bd
// list --limit 0` (~0.5s, opens the Dolt store) and insights added a `dep
// list` call; the compute over the result is in-memory and negligible, so the bd
// subprocess spawn is the whole cost, paid back-to-back through execMu on a
// multi-fragment page. The snapshot folds the List result and the Deps result for
// one repo so every view after the first hits memory.
//
// Viewer writes patch the snapshot from bd's response. A bounded background
// reconciliation catches writes made by other bd processes; repo paths remain
// independent keys. execMu in package bd still serializes the calls that remain.
//
// A snapshot has no request-path expiry. It remains usable while reconciliation
// is running or failing, avoiding a ~0.4s spawn on navigation and filtering.
//
// Out-of-band staleness — a bd CLI run or another agent editing the same repo's
// store while strand holds a snapshot — is the case strand's own writes can't
// catch. The gate for it is storeMTime: putList stamps the snapshot with the Dolt
// store's manifest mtime at fetch time, and every read drops the snapshot when the
// store has moved since (freshEntryLocked). Dolt rewrites the noms manifest on
// every commit, so any writer — bd CLI, another agent, a /team run — advances that
// mtime and the next view re-fetches. This is view-triggered, not a clock: one
// stat() per read (~µs), and the bd spawn is paid only when the store actually
// changed (st-69h). The explicit refresh control (POST /refresh) and the "data as
// of HH:MM" readout remain the manual backstop.
//
// storeMTime is the seam over that stat: it reports the repo's store mtime, or ok
// false when it can't be read. A false — or a snapshot stamped with a zero mtime
// (the stat failed at fetch) — degrades to the old behavior: serve until a write
// invalidates, never falsely stale. Tests inject a fake so staleness is driven off
// the seam, not real filesystem time (which wouldn't align with the fixed clock).
type snapshotCache struct {
	mu         sync.Mutex
	now        func() time.Time
	storeMTime func(repo string) (time.Time, bool)
	gen        uint64
	entries    map[string]*snapshot
	flights    map[string]*refreshFlight
	onChange   func(string)
}

type refreshFlight struct {
	done chan struct{}
	err  error
}

// snapshot is one repo's cached reads: the full `list --limit 0` result and the
// repo-wide Deps result, stamped with the wall time they were fetched so the TTL
// floor can age them out. List and Deps share one entry (and the TTL stamp set at
// the List fetch) so strand/list/insights are one logical snapshot — and Deps is
// fetched once over the whole repo, not per scope, so the second structural view
// reuses the first's edges (depsOK distinguishes "no deps" from "not fetched yet").
//
// gen is the snapshot's identity: a monotonic stamp set at putList that lets a
// late Deps fetch tell whether the snapshot it read the ids from is still the one
// it's about to write deps into (see putDeps). It is clock-independent, so the
// fixed-clock tests still distinguish versions even when every `at` is equal.
type snapshot struct {
	at       time.Time
	storeAt  time.Time // Dolt store mtime observed at fetch; zero when the stat failed (then never gates)
	gen      uint64
	list     []bd.Issue
	deps     []bd.DepEdge
	depsOK   bool
	stats    bd.Stats
	statsOK  bool
	closed   []bd.Issue // the `--status closed` set bd's default list omits; folded lazily
	closedOK bool
}

func newSnapshotCache(now func() time.Time) *snapshotCache {
	return &snapshotCache{now: now, storeMTime: bd.StoreMTime, entries: map[string]*snapshot{}, flights: map[string]*refreshFlight{}}
}

// refreshList coalesces loads and atomically publishes only complete successful
// JSON snapshots. A failed refresh leaves the last-known-good entry untouched.
func (c *snapshotCache) refreshList(ctx context.Context, repo string, src IssueSource, force bool) ([]bd.Issue, error) {
	c.mu.Lock()
	if !force {
		if e := c.entries[repo]; e != nil {
			list := e.list
			c.mu.Unlock()
			return list, nil
		}
	}
	if f := c.flights[repo]; f != nil {
		c.mu.Unlock()
		return c.waitFlight(ctx, repo, f)
	}
	f := &refreshFlight{done: make(chan struct{})}
	c.flights[repo] = f
	c.mu.Unlock()
	// The fetch is a shared flight: other goroutines are waiting on f.done with
	// their own live contexts. Detach from the leader's ctx (context.WithoutCancel)
	// so a canceled leader doesn't hand every follower a spurious ctx.Err() —
	// only this call's deadline/cancel would leak into a result the followers
	// never asked to be canceled by.
	list, err := src.List(context.WithoutCancel(ctx), bd.ListOpts{})
	changed, onChange := c.publishList(repo, f, list, err)
	if changed && onChange != nil {
		onChange(repo)
	}
	if err != nil {
		if good := c.goodList(repo); good != nil {
			return good, err
		}
	}
	return list, err
}

// waitFlight blocks on an in-progress refresh (f) and returns its published list,
// or the flight's error when it published nothing, or ctx cancellation. The
// follower reads with its own live ctx; the leader detached from it, so a canceled
// leader never hands the follower a spurious ctx.Err().
func (c *snapshotCache) waitFlight(ctx context.Context, repo string, f *refreshFlight) ([]bd.Issue, error) {
	select {
	case <-f.done:
		c.mu.Lock()
		e := c.entries[repo]
		c.mu.Unlock()
		if e != nil {
			return e.list, nil
		}
		return nil, f.err
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for in-flight refresh: %w", ctx.Err())
	}
}

// publishList records a completed fetch under the lock: on success it opens a new
// snapshot when the list changed (else just re-stamps the existing one), then
// retires the flight and wakes its followers. It returns whether a change fired
// and the onChange callback to run outside the lock.
func (c *snapshotCache) publishList(repo string, f *refreshFlight, list []bd.Issue, err error) (bool, func(string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := false
	if err == nil {
		old := c.entries[repo]
		changed = old == nil || !reflect.DeepEqual(old.list, list)
		if changed {
			c.gen++
			c.entries[repo] = &snapshot{at: c.now(), gen: c.gen, list: list}
		} else {
			old.at = c.now()
		}
	}
	f.err = err
	delete(c.flights, repo)
	close(f.done)
	return changed, c.onChange
}

// goodList returns the repo's last-known-good list, or nil when none is cached —
// the fallback a failed refresh serves so a fetch error never drops a live entry.
func (c *snapshotCache) goodList(repo string) []bd.Issue {
	c.mu.Lock()
	defer c.mu.Unlock()
	if good := c.entries[repo]; good != nil {
		return good.list
	}
	return nil
}

// The Dolt-store-mtime computation lives in internal/bd (bd.StoreMTime): the counts
// refresher gates on the SAME helper, so the board and the badge can't diverge on
// freshness (st-nm5). newSnapshotCache wires it into the storeMTime seam above.

// freshEntryLocked returns the repo's snapshot if present and still fresh, else nil
// (a miss). A snapshot has no time-based expiry: it lives until strand's own write
// invalidates it, the repo switches, or the Dolt store moves under it. That last
// case is the out-of-band gate — if the store's manifest mtime has advanced past
// the mtime stamped at fetch, some other writer changed the beads and the snapshot
// is dropped so the next read re-fetches bd's truth (st-69h). A zero storeAt (the
// stat failed at fetch) or a storeMTime that can't read now disables the gate, so a
// missing signal degrades to "serve until a write invalidates" rather than churning.
//
// The caller MUST hold c.mu: putDeps mutates an entry's deps/depsOK in place, so a
// reader that escapes the lock with the *snapshot races that write (strand-4sd). The
// public accessors below copy the fields they need out under the lock and never hand
// the pointer to a handler.
func (c *snapshotCache) freshEntryLocked(repo string) *snapshot {
	e := c.entries[repo]
	if e == nil {
		return nil
	}
	if !e.storeAt.IsZero() && c.storeMTime != nil {
		if mt, ok := c.storeMTime(repo); ok && mt.After(e.storeAt) {
			delete(c.entries, repo)
			return nil
		}
	}
	return e
}

// checkStale is freshEntryLocked's staleness rule, factored out for a caller that
// isn't reading (the masthead poll tick, st-2fy.5) rather than serving a view. A
// stale entry is evicted exactly as freshEntryLocked would evict it on the next
// read — checkStale just lets the poll tick catch the eviction first, before any
// reader shows up, so the caller can kick a background rebuild (goBackground) and
// have the snapshot warm again by the time a filter switch actually asks for it.
//
// No entry yet → false: a cold repo nobody has viewed has nothing to proactively
// rebuild, so this never turns a lazy miss into an eager fetch. An unreadable or
// unmoved store mtime → false, same as freshEntryLocked's degrade. Only a moved
// mtime evicts and reports true.
//
// Concurrent pollers for the same repo (two open tabs) can each observe true and
// each kick a rebuild; that's harmless (putList just overwrites the entry twice
// with equivalent data), not a correctness bug — no de-dup needed for this bead.
func (c *snapshotCache) checkStale(repo string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[repo]
	if e == nil {
		return false
	}
	if e.storeAt.IsZero() || c.storeMTime == nil {
		return false
	}
	mt, ok := c.storeMTime(repo)
	if !ok || !mt.After(e.storeAt) {
		return false
	}
	delete(c.entries, repo)
	return true
}

// stampedAt reports when the repo's snapshot was fetched, for the "data as of …"
// readout. ok is false when no snapshot is warm yet.
func (c *snapshotCache) stampedAt(repo string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.freshEntryLocked(repo); e != nil {
		return e.at, true
	}
	return time.Time{}, false
}

// liveList returns the repo's cached list and true on a live snapshot. The slice
// header is copied out under the lock; its backing array is the shared read-only
// view (see the contract above putList), never mutated after publish.
func (c *snapshotCache) liveList(repo string) ([]bd.Issue, uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.freshEntryLocked(repo); e != nil {
		return e.list, e.gen, true
	}
	return nil, 0, false
}

// liveDeps returns the repo-wide dependency edges and true only when a live
// snapshot has them (depsOK). Read under the lock that putDeps writes under, so
// the deps/depsOK fields never race; the returned slice is the shared read-only
// view (see the contract above putList).
func (c *snapshotCache) liveDeps(repo string) ([]bd.DepEdge, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.freshEntryLocked(repo); e != nil && e.depsOK {
		return e.deps, true
	}
	return nil, false
}

// liveStats returns the repo-wide status counts and true only when a live snapshot
// already holds them (statsOK). Read under the lock putStats writes under, so the
// stats/statsOK fields never race.
func (c *snapshotCache) liveStats(repo string) (bd.Stats, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.freshEntryLocked(repo); e != nil && e.statsOK {
		return e.stats, true
	}
	return bd.Stats{}, false
}

// liveClosed returns the repo's closed beads and true only when a live snapshot
// already holds them (closedOK). bd's default `list` omits closed, so the closed
// cut fetches this set once and folds it here; every later closed view hits memory
// until a write or the mtime gate drops the entry. Read under the lock putClosed
// writes under, so the closed/closedOK fields never race; the returned slice is the
// shared read-only view (see the contract above putList).
func (c *snapshotCache) liveClosed(repo string) ([]bd.Issue, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.freshEntryLocked(repo); e != nil && e.closedOK {
		return e.closed, true
	}
	return nil, false
}

// Shared-view contract (matches registry.Registry.Repos): a snapshot's list and
// deps slices are published once and never mutated in place. liveList/liveDeps
// hand the same backing array to every concurrent handler, which read it without
// copying and filter into fresh slices. putList replaces the whole *snapshot on a
// refresh rather than appending, so a handler holding an older slice keeps a
// valid, immutable view. Callers must treat the returned slices as read-only.

// putList records a fresh List result, opening the repo's snapshot and stamping it
// with both the wall time (the "data as of" readout) and the Dolt store mtime the
// out-of-band gate ages against. A storeMTime that can't read now leaves storeAt
// zero, which disables the gate for this entry (freshEntryLocked) — the same
// degrade-to-old-behavior a missing signal gets everywhere else.
func (c *snapshotCache) putList(repo string, list []bd.Issue) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	var storeAt time.Time
	if c.storeMTime != nil {
		if mt, ok := c.storeMTime(repo); ok {
			storeAt = mt
		}
	}
	c.entries[repo] = &snapshot{at: c.now(), storeAt: storeAt, gen: c.gen, list: list}
}

func (c *snapshotCache) upsert(repo string, issue *bd.Issue) {
	if issue == nil {
		return
	}
	c.mu.Lock()
	e := c.entries[repo]
	if e == nil {
		c.mu.Unlock()
		return
	}
	list := append([]bd.Issue(nil), e.list...)
	found := false
	for i := range list {
		if list[i].ID == issue.ID {
			list[i] = *issue
			found = true
			break
		}
	}
	if !found {
		list = append(list, *issue)
	}
	c.gen++
	c.entries[repo] = &snapshot{at: c.now(), gen: c.gen, list: list}
	onChange := c.onChange
	c.mu.Unlock()
	if onChange != nil {
		onChange(repo)
	}
}

func (c *snapshotCache) remove(repo, id string) {
	c.mu.Lock()
	e := c.entries[repo]
	if e == nil {
		c.mu.Unlock()
		return
	}
	list := make([]bd.Issue, 0, len(e.list))
	for i := range e.list {
		if e.list[i].ID != id {
			list = append(list, e.list[i])
		}
	}
	c.gen++
	c.entries[repo] = &snapshot{at: c.now(), gen: c.gen, list: list}
	onChange := c.onChange
	c.mu.Unlock()
	if onChange != nil {
		onChange(repo)
	}
}

// putDeps records the repo-wide Deps result into the snapshot it was fetched for,
// identified by gen (the value liveList returned alongside the ids). It writes only
// when the current snapshot is still that one: an invalidate or a fresh putList
// between the List read and here replaces the entry with a different gen, and the
// stale deps are dropped rather than stapled onto a newer list (the version-skew
// guard). The next read re-warms List, and Deps follows.
func (c *snapshotCache) putDeps(repo string, gen uint64, deps []bd.DepEdge) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[repo]; ok && e.gen == gen {
		e.deps, e.depsOK = deps, true
	}
}

// putStats records the repo-wide Stats result into the snapshot it was fetched for,
// identified by gen, mirroring putDeps: it writes only when the current snapshot is
// still that one, so an invalidate or a fresh putList between the read and here drops
// the stale counts rather than stapling them onto a newer list. gen 0 (a Stats before
// any List) never matches a live snapshot, so the counts simply aren't cached until a
// list is warm.
func (c *snapshotCache) putStats(repo string, gen uint64, stats bd.Stats) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[repo]; ok && e.gen == gen {
		e.stats, e.statsOK = stats, true
	}
}

// putClosed records the repo's closed beads into the snapshot they were fetched for,
// identified by gen, mirroring putDeps/putStats: it writes only when the current
// snapshot is still that one, so an invalidate or a fresh putList between the read and
// here drops the stale set rather than stapling it onto a newer list. gen 0 (a closed
// fetch before any list) never matches a live snapshot, so the set simply isn't cached
// until a list is warm.
func (c *snapshotCache) putClosed(repo string, gen uint64, closed []bd.Issue) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[repo]; ok && e.gen == gen {
		e.closed, e.closedOK = closed, true
	}
}

// invalidate drops a repo's snapshot. Every successful write calls this so the
// next read re-fetches bd's truth.
func (c *snapshotCache) invalidate(repo string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, repo)
}

// cachingSource wraps a repo's bd issue source so reads are served from the
// snapshot and writes drop it. It embeds IssueSource, so every method strand
// doesn't override (Show, Comments, the writes' default pass-through) goes
// straight to bd; only List, Deps, and the mutating methods carry cache behavior.
type cachingSource struct {
	IssueSource
	cache *snapshotCache
	repo  string
}

// List serves the repo's cached snapshot for the unfiltered full read only — the
// zero-value ListOpts (`list --limit 0`), fetching once on a miss. Any non-zero opts
// (a filter) bypasses the snapshot entirely: it passes straight through to bd and
// neither serves from nor writes to the cache, so a filter can never silently return
// the full list. The discriminator keys on the whole opts, not one field, so it stays
// the invariant ("only the full read is cacheable") even if ListOpts grows another
// filter — no future field can drift a filtered read onto the open snapshot (st-4g0,
// st-57y). Most views read unfiltered through this source; the closed fold's fallback
// `--status closed` read (Closed, for a non-caching source) relies on this pass-through,
// so its status slice never serves from nor poisons the open snapshot.
func (c *cachingSource) List(ctx context.Context, opts bd.ListOpts) ([]bd.Issue, error) {
	if opts != (bd.ListOpts{}) {
		return c.IssueSource.List(ctx, opts)
	}
	if list, _, ok := c.cache.liveList(c.repo); ok {
		return list, nil
	}
	return c.cache.refreshList(ctx, c.repo, c.IssueSource, false)
}

// Deps serves the repo-wide dependency edges, fetching once over the whole repo on
// a miss and caching the superset. Callers pass a scope's ids (graph/insights) or
// one id (drawer), but every caller already filters the result to its in-scope
// "blocks" edges (blocksEdges / blockerIDs), so a superset is correct and lets all
// structural views share one fetch. On a cold cache it fetches deps for the full
// cached list's ids; if List hasn't run yet it falls back to the requested ids.
// computePulse reads the warm deps for the masthead straight from the snapshot cache
// (snapshotCache.liveDeps), never fetching — the non-blocking peek that lets the
// masthead use the exact effective-blocked set when it's warm without paying a spawn
// when it isn't (st-x66, honoring the str-47z no-deps-on-landing rule).

// Stats serves the repo-wide status counts from the snapshot, fetching once on a
// miss and binding the result to the current snapshot's gen (like Deps). The counts
// live in the same entry as the list and drop on the same invalidate, so a write
// re-fetches bd's truth. computePulse reads Stats before List, so on a cold cache the
// first fetch can't bind yet (gen 0, no live entry) and pays the spawn; once a list is
// warm the next render caches the counts and every render after hits memory.
func (c *cachingSource) Stats(ctx context.Context) (bd.Stats, error) {
	if stats, ok := c.cache.liveStats(c.repo); ok {
		return stats, nil
	}
	_, gen, _ := c.cache.liveList(c.repo)
	stats, err := c.IssueSource.Stats(ctx)
	if err != nil {
		return bd.Stats{}, err
	}
	c.cache.putStats(c.repo, gen, stats)
	return stats, nil
}

// Closed serves the repo's closed beads from the snapshot, fetching the `--status
// closed` set once on a miss and binding it to the current snapshot's gen (like Deps
// and Stats). bd's default `list` omits closed, so this is the one read that surfaces
// them; folding it here turns the closed cut from a ~0.5s spawn on every switch into a
// warm read. It warms the open-list snapshot first when cold so the closed set has a
// live gen to bind to — a closed-first request (a `?filter=closed` deep-link) then
// caches instead of re-fetching every switch, and that warm list serves the other
// views for free. The set drops on the same invalidate (a write) and the same mtime
// gate as every other fold.
func (c *cachingSource) Closed(ctx context.Context) ([]bd.Issue, error) {
	if closed, ok := c.cache.liveClosed(c.repo); ok {
		return closed, nil
	}
	if _, err := c.List(ctx, bd.ListOpts{}); err != nil {
		return nil, err
	}
	_, gen, _ := c.cache.liveList(c.repo)
	closed, err := c.IssueSource.List(ctx, bd.ListOpts{Status: bd.StatusClosed})
	if err != nil {
		return nil, err
	}
	c.cache.putClosed(c.repo, gen, closed)
	return closed, nil
}

func (c *cachingSource) Deps(ctx context.Context, ids ...string) ([]bd.DepEdge, error) {
	if deps, ok := c.cache.liveDeps(c.repo); ok {
		return deps, nil
	}
	fetchIDs, gen := c.repoIDs(ids)
	deps, err := c.IssueSource.Deps(ctx, fetchIDs...)
	if err != nil {
		return nil, err
	}
	c.cache.putDeps(c.repo, gen, deps)
	return deps, nil
}

// repoIDs returns the id set to fetch deps over plus the gen of the snapshot it
// read them from (so putDeps can bind the result to that exact snapshot): every id
// in the cached list (so one fetch covers all scopes), falling back to the caller's
// ids and gen 0 when the list isn't cached yet (a Deps before any List — not a path
// the views take, but safe; gen 0 never matches a live snapshot, so putDeps skips).
func (c *cachingSource) repoIDs(reqIDs []string) ([]string, uint64) {
	list, gen, ok := c.cache.liveList(c.repo)
	if !ok || len(list) == 0 {
		return reqIDs, 0
	}
	ids := make([]string, len(list))
	for i := range list {
		ids[i] = list[i].ID
	}
	return ids, gen
}

// Writes that return an issue atomically upsert bd's response. Operations whose
// CLI contract returns no issue preserve last-good data until reconciliation.
// Failed writes never alter the snapshot.
func (c *cachingSource) done(err error) error {
	return err
}

func (c *cachingSource) Update(ctx context.Context, id, field, value string) (*bd.Issue, error) {
	iss, err := c.IssueSource.Update(ctx, id, field, value)
	if err == nil {
		c.cache.upsert(c.repo, iss)
	}
	return iss, err
}

func (c *cachingSource) Claim(ctx context.Context, id string) (*bd.Issue, error) {
	iss, err := c.IssueSource.Claim(ctx, id)
	if err == nil {
		c.cache.upsert(c.repo, iss)
	}
	return iss, err
}

func (c *cachingSource) Close(ctx context.Context, id, reason string) (*bd.Issue, error) {
	iss, err := c.IssueSource.Close(ctx, id, reason)
	if err == nil {
		c.cache.upsert(c.repo, iss)
	}
	return iss, err
}

func (c *cachingSource) SetRank(ctx context.Context, id string, rank float64) (*bd.Issue, error) {
	iss, err := c.IssueSource.SetRank(ctx, id, rank)
	if err == nil {
		c.cache.upsert(c.repo, iss)
	}
	return iss, err
}

func (c *cachingSource) SetParent(ctx context.Context, id, parent bd.ID) (*bd.Issue, error) {
	iss, err := c.IssueSource.SetParent(ctx, id, parent)
	if err == nil {
		c.cache.upsert(c.repo, iss)
	}
	return iss, err
}

func (c *cachingSource) Comment(ctx context.Context, id, text string) error {
	return c.done(c.IssueSource.Comment(ctx, id, text))
}

func (c *cachingSource) DepAdd(ctx context.Context, id, dependsOn bd.ID) error {
	return c.done(c.IssueSource.DepAdd(ctx, id, dependsOn))
}

func (c *cachingSource) DepRemove(ctx context.Context, id, dependsOn bd.ID) error {
	return c.done(c.IssueSource.DepRemove(ctx, id, dependsOn))
}

func (c *cachingSource) LabelAdd(ctx context.Context, id, label string) error {
	return c.done(c.IssueSource.LabelAdd(ctx, id, label))
}

func (c *cachingSource) LabelRemove(ctx context.Context, id, label string) error {
	return c.done(c.IssueSource.LabelRemove(ctx, id, label))
}

func (c *cachingSource) Create(ctx context.Context, opts *bd.CreateOpts) (*bd.Issue, error) {
	iss, err := c.IssueSource.Create(ctx, opts)
	if err == nil {
		c.cache.upsert(c.repo, iss)
	}
	return iss, err
}

func (c *cachingSource) Delete(ctx context.Context, id string) error {
	err := c.IssueSource.Delete(ctx, id)
	if err == nil {
		c.cache.remove(c.repo, id)
	}
	return err
}
