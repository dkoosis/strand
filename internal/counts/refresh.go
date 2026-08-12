package counts

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dkoosis/atomicfile"
	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/bdcounts"
)

// mode is which repos a refresh visits.
type mode int

const (
	modeChanged  mode = iota // default: every discovered repo, skipping mtime-unchanged ones
	modeAll                  // every discovered repo, unconditionally (what launchd invokes)
	modeExplicit             // only the repo roots named on the command line
)

// config is a resolved refresh run. newSource is the seam: production builds a
// bd.Client per repo; tests inject a canned source so the discovery/merge/mode logic
// runs with no subprocess and no real store.
type config struct {
	cacheDir  string
	projects  string
	bin       string
	version   string // the strand binary version, stamped into counts.json's liveness meta
	mode      mode
	targets   []string
	newSource func(root string) source
	now       func() time.Time // clock seam for the liveness stamp; nil → time.Now
}

// Run is the `strand counts` entry point. It resolves the run from flags + the same
// env the shell honored (BD_COUNTS_CACHE_DIR, BD_COUNTS_PROJECTS_DIR), then refreshes.
//
//	strand counts              # every changed repo (skip mtime-unchanged)
//	strand counts --all        # every discovered repo, unconditionally
//	strand counts <dir>...      # only the named repo roots
func Run(args []string, version string) error {
	fs := flag.NewFlagSet("counts", flag.ContinueOnError)
	all := fs.Bool("all", false, "refresh every discovered repo unconditionally")
	bin := fs.String("bd", "bd", "path to the bd binary")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("counts: parse flags: %w", err)
	}
	cfg := config{
		cacheDir:  cacheDir(),
		projects:  projectsDir(),
		bin:       *bin,
		version:   version,
		mode:      modeChanged,
		targets:   fs.Args(),
		newSource: nil, // set below
	}
	cfg.newSource = func(root string) source { return &bd.Client{Dir: root, Bin: cfg.bin} }
	switch {
	case len(cfg.targets) > 0:
		cfg.mode = modeExplicit
	case *all:
		cfg.mode = modeAll
	}
	return refresh(context.Background(), &cfg) //nolint:forbidigo // CLI subcommand root: `strand counts` is a one-shot command, not request-scoped
}

// refresh visits the run's repos, recomputes each row, and writes counts.json. It is
// last-good per repo: the output starts from the existing file, and only a successful
// recompute overwrites a repo's row, so a repo whose bd reads fail keeps its previous
// entry rather than being zeroed or dropped. The whole file is written atomically
// (tmp + rename), and the mtime state is recorded even for a failed repo so a broken
// read doesn't hot-retry every run.
//
// modeChanged additionally guards against a torn bd read latching forever (st-3p8): a
// bd write touches last-touched synchronously but its DB commit can still be landing
// when the watch-fire triggers this run, so the derive can read a stale snapshot. Once
// that stale row is cached AND the observed mtime is recorded, a naive "mtime changed
// since last run?" check would never revisit it — nothing touches last-touched again.
// Each repo's stored state therefore carries a pending bit: a change-triggered derive
// this cycle always schedules exactly one guaranteed follow-up derive next cycle
// (regardless of whether the mtime moves again), by which time bd's commit has
// settled. See repoState.next for the exact carry-forward rule.
func refresh(ctx context.Context, cfg *config) error {
	targets := cfg.targets
	if cfg.mode != modeExplicit {
		targets = discover(cfg.projects)
	}

	outPath := filepath.Join(cfg.cacheDir, "counts.json")
	statePath := filepath.Join(cfg.cacheDir, "counts-mtimes")
	rows := readRows(outPath)
	seen := readState(statePath)
	nextState := make(map[string]repoState, len(targets))

	for _, root := range targets {
		cur := changeKey(root)
		prior := seen[root]
		mtimeChanged := prior.mtime != cur
		_, cached := rows[root]

		if cfg.mode == modeChanged && cached && !mtimeChanged && !prior.pending {
			nextState[root] = repoState{mtime: cur} // settled + already cached → skip the reads
			continue
		}

		prev := rows[root]
		row, err := computeRow(ctx, cfg.newSource(root), root, prev.EID, prev.EPct)
		if err == nil {
			rows[root] = row
		}
		nextState[root] = repoState{mtime: cur, pending: nextPending(cfg.mode, mtimeChanged, err)}
	}

	if err := os.MkdirAll(cfg.cacheDir, 0o755); err != nil {
		return fmt.Errorf("counts: cache dir: %w", err)
	}
	// counts.json is rewritten every run, even when no row changed: the write stamps a
	// fresh liveness meta (last-run time + binary version) so a dead or wedged refresher
	// shows a stale flag in the masthead instead of freezing the file indistinguishably
	// from "nothing changed" (st-18l/B4). The per-repo read-skip above still holds — an
	// unchanged repo pays no bd reads; only the meta stamp and a small atomic rewrite are
	// unconditional.
	meta := bdcounts.Meta{LastRun: cfg.clock().Unix(), Version: cfg.version}
	if err := writeRowsAtomic(outPath, rows, meta); err != nil {
		return err
	}
	return writeState(statePath, nextState)
}

// clock returns the config's clock, defaulting to time.Now — the seam a test pins to
// stamp a deterministic last-run time.
func (c *config) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// nextPending decides whether a repo should be revisited unconditionally next cycle,
// regardless of mtime:
//
//   - modeChanged, this cycle was change-triggered (mtimeChanged) → always re-arm.
//     Whether the derive just now succeeded or failed, the settled DB state hasn't
//     necessarily been observed yet, so schedule the guaranteed follow-up. A failed
//     change-triggered derive re-arms exactly like the existing last-good retry
//     behavior (nextState still records failure → next mtime-changed cycle retries).
//   - modeChanged, pending carried in from before (mtimeChanged false, i.e. this is
//     the guaranteed follow-up) → clear pending ONLY on success. A failed
//     pending-triggered derive must NOT clear pending, or the stale row latches
//     forever with no mtime change left to re-trigger anything (P0 from plan review).
//   - modeAll/modeExplicit → a visited repo always gets a real read; clear pending on
//     success (satisfies any inherited flag), keep it set on failure so a later
//     modeChanged run still gets its guaranteed follow-up.
func nextPending(m mode, mtimeChanged bool, err error) bool {
	if m == modeChanged && mtimeChanged {
		return true
	}
	return err != nil
}

// discover returns every repo root under projects with a real .beads workspace (a
// last-touched stamp or a metadata.json), matching the shell's discovery.
func discover(projects string) []string {
	entries, err := os.ReadDir(projects)
	if err != nil {
		return nil
	}
	var roots []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		root := filepath.Join(projects, e.Name())
		if hasFile(root, ".beads", "last-touched") || hasFile(root, ".beads", "metadata.json") {
			roots = append(roots, root)
		}
	}
	return roots
}

func hasFile(parts ...string) bool {
	_, err := os.Stat(filepath.Join(parts...))
	return err == nil
}

// readRows loads the existing counts.json as the last-good base; a missing or
// malformed file starts an empty set (the next write rebuilds it).
func readRows(path string) map[string]Row {
	rows := map[string]Row{}
	data, err := os.ReadFile(path)
	if err != nil {
		return rows
	}
	_ = json.Unmarshal(data, &rows) // malformed → empty base, rebuilt this run
	delete(rows, bdcounts.MetaKey)  // the liveness stamp is not a repo row — re-injected on write
	return rows
}

// writeRowsAtomic writes the rows plus the liveness meta as compact JSON via a temp
// file + rename, so a concurrent reader never sees a half-written file. The meta rides
// under bdcounts.MetaKey — a reserved key no repo path collides with — so keyed readers
// (bdcounts.Reader.Lookup, the status line's `.[$repo]`) are untouched by its presence.
//
// NOTE (RMW race, not fixed by atomic write): two concurrent refreshes (the
// launchd --all run and a manual `strand counts`) each read-compute-write the
// whole rows set independently. atomicfile.WriteFile stops either write from
// being torn, but it does not serialize the two writers — whichever finishes
// last wins wholesale, silently dropping the other's rows. Follow-up: a lock
// around the refresh, or a merge-on-write like writeState's.
func writeRowsAtomic(path string, rows map[string]Row, meta bdcounts.Meta) error {
	out := make(map[string]any, len(rows)+1)
	for k, v := range rows {
		out[k] = v
	}
	out[bdcounts.MetaKey] = meta
	data, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("counts: marshal: %w", err)
	}
	if err := atomicfile.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("counts: write: %w", err)
	}
	return nil
}

// repoState is one repo's carry-forward bookkeeping between refresh runs: the
// last-observed change key (changeKey — the Dolt store mtime, or last-touched when
// no store), plus the st-3p8 pending bit (a change-triggered derive schedules exactly
// one guaranteed follow-up derive next cycle — see nextPending). Stored as the state
// file's 2nd/3rd tab-separated columns. The field is named mtime for the on-disk
// format's sake; its value is whatever changeKey returned that run.
type repoState struct {
	mtime   int64
	pending bool
}

// readState loads the per-repo state recorded last run ("root\tmtime\tpending" per
// line); a missing/garbled file is an empty map (everything reads as changed). A
// pre-st-3p8 2-field line ("root\tmtime", no pending column) parses with pending
// defaulted false — correct, since a state file written before this fix never had a
// torn read recorded against it.
func readState(path string) map[string]repoState {
	state := map[string]repoState{}
	f, err := os.Open(path)
	if err != nil {
		return state
	}
	defer f.Close()
	sc := bufio.NewScanner(f) //nolint:gocritic // state-file lines are short (repo path + two ints), far under bufio's 64KB default cap
	for sc.Scan() {
		root, rest, ok := strings.Cut(sc.Text(), "\t")
		if !ok {
			continue
		}
		mt, pendingField, _ := strings.Cut(rest, "\t") // pendingField "" on legacy 2-field lines
		v, err := strconv.ParseInt(mt, 10, 64)
		if err != nil {
			continue
		}
		state[root] = repoState{mtime: v, pending: pendingField == "1"}
	}
	return state
}

// writeState records the visited repos' state for the next run's changed-check,
// atomically like the rows file. It MERGES the just-computed entries over the state
// already on disk rather than replacing it wholesale: modeExplicit (and any run that
// visits a subset of the discovered repos) only carries state for its named roots, so
// a plain overwrite would truncate every other repo's gate entry — the next launchd
// changed-mode run would then find no prior mtime for those repos and cold-recompute
// them all (st-dd9). Reading the prior state and overlaying keeps the untouched repos'
// gate entries intact.
//
// NOTE (RMW race, not fixed by atomic write): the read-merge-write above is not
// serialized against a concurrent writer — two refreshes racing this function can
// each read the same prior state, merge their own entries on top, and whichever
// atomicfile.WriteFile finishes last wins wholesale, silently dropping the other's
// merged entries. Same follow-up as writeRowsAtomic.
func writeState(path string, state map[string]repoState) error {
	merged := readState(path)
	maps.Copy(merged, state)

	var b strings.Builder
	for root, st := range merged {
		pendingField := "0"
		if st.pending {
			pendingField = "1"
		}
		fmt.Fprintf(&b, "%s\t%d\t%s\n", root, st.mtime, pendingField)
	}
	if err := atomicfile.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("counts: write state: %w", err)
	}
	return nil
}

// cacheDir resolves the counts cache dir, honoring BD_COUNTS_CACHE_DIR like the
// writer and reader, else ~/.cache/cc-dashboard.
func cacheDir() string {
	if dir := os.Getenv("BD_COUNTS_CACHE_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "cc-dashboard")
}

// projectsDir resolves the repo-scan root, honoring BD_COUNTS_PROJECTS_DIR, else
// ~/Projects.
func projectsDir() string {
	if dir := os.Getenv("BD_COUNTS_PROJECTS_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Projects")
}
