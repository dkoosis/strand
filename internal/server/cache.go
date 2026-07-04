package server

import (
	"context"
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
// strand is the SOLE writer to its repos, so the snapshot stays correct by
// invalidating on every successful write (cachingSource's write methods drop the
// repo's entry) and on repo switch (a switched-to path is a different key, hence a
// miss — no explicit hook needed). execMu in package bd is untouched: it still
// serializes the bd calls that do happen, but the cache removes the contention by
// removing the calls.
//
// A snapshot has no time-based expiry: it lives until a write invalidates it or
// the repo switches. The model is "open a beadbase, look at each view in turn", so
// a snapshot must outlast a long look without re-paying the ~0.4s bd spawn on the
// next tab (str-udl supersedes the original 3s TTL, which punished lingering).
//
// Out-of-band staleness — a bd CLI run or another agent editing the same repo's
// store while strand holds a snapshot — is the one case writes can't catch. With
// no clock to age the snapshot out, a plain browser reload would serve the stale
// view, so the mitigation is the explicit refresh control (POST /refresh →
// invalidate → reload): out-of-band edits surface on a deliberate click, with a
// "data as of HH:MM" readout so the staleness window is visible.
type snapshotCache struct {
	mu      sync.Mutex
	now     func() time.Time
	gen     uint64
	entries map[string]*snapshot
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
	at      time.Time
	gen     uint64
	list    []bd.Issue
	deps    []bd.DepEdge
	depsOK  bool
	stats   bd.Stats
	statsOK bool
}

func newSnapshotCache(now func() time.Time) *snapshotCache {
	return &snapshotCache{now: now, entries: map[string]*snapshot{}}
}

// entryLocked returns the repo's snapshot if present, else nil (a miss). A
// snapshot lives until a write invalidates it or the repo switches — there is no
// time-based expiry. The usage model is "open a beadbase, look at each view", so
// a snapshot must survive a long look without re-paying the bd spawn; out-of-band
// edits surface through the explicit refresh control (str-udl), not a silent
// clock. The caller MUST hold c.mu: putDeps mutates an entry's deps/depsOK in
// place, so a reader that escapes the lock with the *snapshot races that write
// (strand-4sd). The public accessors below copy the fields they need out under
// the lock and never hand the pointer to a handler.
func (c *snapshotCache) entryLocked(repo string) *snapshot {
	return c.entries[repo]
}

// stampedAt reports when the repo's snapshot was fetched, for the "data as of …"
// readout. ok is false when no snapshot is warm yet.
func (c *snapshotCache) stampedAt(repo string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[repo]; ok {
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
	if e := c.entryLocked(repo); e != nil {
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
	if e := c.entryLocked(repo); e != nil && e.depsOK {
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
	if e := c.entryLocked(repo); e != nil && e.statsOK {
		return e.stats, true
	}
	return bd.Stats{}, false
}

// Shared-view contract (matches registry.Registry.Repos): a snapshot's list and
// deps slices are published once and never mutated in place. liveList/liveDeps
// hand the same backing array to every concurrent handler, which read it without
// copying and filter into fresh slices. putList replaces the whole *snapshot on a
// refresh rather than appending, so a handler holding an older slice keeps a
// valid, immutable view. Callers must treat the returned slices as read-only.

// putList records a fresh List result, opening the repo's snapshot and stamping it
// with the fetch time the TTL ages against.
func (c *snapshotCache) putList(repo string, list []bd.Issue) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	c.entries[repo] = &snapshot{at: c.now(), gen: c.gen, list: list}
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
// st-57y). Most views read unfiltered through this source; the one filtered read path
// (pulseListView's external cut) relies on this pass-through, so its status slice never
// serves from nor poisons the open snapshot.
func (c *cachingSource) List(ctx context.Context, opts bd.ListOpts) ([]bd.Issue, error) {
	if opts != (bd.ListOpts{}) {
		return c.IssueSource.List(ctx, opts)
	}
	if list, _, ok := c.cache.liveList(c.repo); ok {
		return list, nil
	}
	list, err := c.IssueSource.List(ctx, opts)
	if err != nil {
		return nil, err
	}
	c.cache.putList(c.repo, list)
	return list, nil
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

// The write methods pass through to bd, then drop the repo's snapshot on success so
// the next read reflects the change (strand is the sole writer — invalidate exactly
// on a successful write). A failed write leaves the snapshot, since nothing changed.

// done drops the repo's snapshot when a write succeeded and returns the write's
// error unchanged, so every wrapper is one line and the invalidate-on-success rule
// lives in one place.
func (c *cachingSource) done(err error) error {
	if err == nil {
		c.cache.invalidate(c.repo)
	}
	return err
}

func (c *cachingSource) Update(ctx context.Context, id, field, value string) (*bd.Issue, error) {
	iss, err := c.IssueSource.Update(ctx, id, field, value)
	return iss, c.done(err)
}

func (c *cachingSource) Claim(ctx context.Context, id string) (*bd.Issue, error) {
	iss, err := c.IssueSource.Claim(ctx, id)
	return iss, c.done(err)
}

func (c *cachingSource) Close(ctx context.Context, id, reason string) (*bd.Issue, error) {
	iss, err := c.IssueSource.Close(ctx, id, reason)
	return iss, c.done(err)
}

func (c *cachingSource) SetRank(ctx context.Context, id string, rank float64) (*bd.Issue, error) {
	iss, err := c.IssueSource.SetRank(ctx, id, rank)
	return iss, c.done(err)
}

func (c *cachingSource) SetParent(ctx context.Context, id, parent bd.ID) (*bd.Issue, error) {
	iss, err := c.IssueSource.SetParent(ctx, id, parent)
	return iss, c.done(err)
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
	return iss, c.done(err)
}

func (c *cachingSource) Delete(ctx context.Context, id string) error {
	return c.done(c.IssueSource.Delete(ctx, id))
}
