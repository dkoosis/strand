package counts

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dkoosis/strand/internal/bd"
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
	mode      mode
	targets   []string
	newSource func(root string) source
}

// Run is the `strand counts` entry point. It resolves the run from flags + the same
// env the shell honored (BD_COUNTS_CACHE_DIR, BD_COUNTS_PROJECTS_DIR), then refreshes.
//
//	strand counts              # every changed repo (skip mtime-unchanged)
//	strand counts --all        # every discovered repo, unconditionally
//	strand counts <dir>...      # only the named repo roots
func Run(args []string) error {
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
	return refresh(context.Background(), &cfg)
}

// refresh visits the run's repos, recomputes each row, and writes counts.json. It is
// last-good per repo: the output starts from the existing file, and only a successful
// recompute overwrites a repo's row, so a repo whose bd reads fail keeps its previous
// entry rather than being zeroed or dropped. The whole file is written atomically
// (tmp + rename), and the mtime state is recorded even for a failed repo so a broken
// read doesn't hot-retry every run.
func refresh(ctx context.Context, cfg *config) error {
	targets := cfg.targets
	if cfg.mode != modeExplicit {
		targets = discover(cfg.projects)
	}

	outPath := filepath.Join(cfg.cacheDir, "counts.json")
	statePath := filepath.Join(cfg.cacheDir, "counts-mtimes")
	rows := readRows(outPath)
	seen := readState(statePath)
	nextState := make(map[string]int64, len(targets))

	changed := false
	for _, root := range targets {
		cur := lastTouched(root)
		if cfg.mode == modeChanged {
			if _, cached := rows[root]; cached && seen[root] == cur {
				nextState[root] = cur // unchanged + already cached → carry state, skip the reads
				continue
			}
		}
		prev := rows[root]
		if row, err := computeRow(ctx, cfg.newSource(root), root, prev.EID, prev.EPct); err == nil {
			rows[root] = row
			changed = true
		}
		nextState[root] = cur // record even on failure → no hot-retry loop
	}

	if err := os.MkdirAll(cfg.cacheDir, 0o755); err != nil {
		return fmt.Errorf("counts: cache dir: %w", err)
	}
	if changed {
		if err := writeRowsAtomic(outPath, rows); err != nil {
			return err
		}
	}
	return writeState(statePath, nextState)
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
	return rows
}

// writeRowsAtomic writes the rows as compact JSON via a temp file + rename, so a
// concurrent reader never sees a half-written file.
func writeRowsAtomic(path string, rows map[string]Row) error {
	data, err := json.Marshal(rows)
	if err != nil {
		return fmt.Errorf("counts: marshal: %w", err)
	}
	tmp := tmpPath(path)
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("counts: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("counts: rename: %w", err)
	}
	return nil
}

// tmpPath is a per-process temp name for the atomic write. The pid suffix keeps two
// concurrent refreshes (the launchd --all and a manual `strand counts`) off one
// shared tmp, where they would clobber each other's write and race the rename to a
// spurious ENOENT — the same guard the shell got from `$OUT.$$`.
func tmpPath(path string) string {
	return fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
}

// readState loads the per-repo last-touched mtimes recorded last run ("root\tmtime"
// per line); a missing/garbled file is an empty map (everything reads as changed).
func readState(path string) map[string]int64 {
	state := map[string]int64{}
	f, err := os.Open(path)
	if err != nil {
		return state
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		root, mt, ok := strings.Cut(sc.Text(), "\t")
		if !ok {
			continue
		}
		if v, err := strconv.ParseInt(mt, 10, 64); err == nil {
			state[root] = v
		}
	}
	return state
}

// writeState records the visited repos' mtimes for the next run's changed-check,
// atomically like the rows file.
func writeState(path string, state map[string]int64) error {
	var b strings.Builder
	for root, mt := range state {
		fmt.Fprintf(&b, "%s\t%d\n", root, mt)
	}
	tmp := tmpPath(path)
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("counts: write state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("counts: rename state: %w", err)
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
