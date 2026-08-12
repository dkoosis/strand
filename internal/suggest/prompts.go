package suggest

import (
	_ "embed"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The canonical assist prompts, vendored from bdx:bead-fmt's references/ so the
// binary works with no plugin installed. They are a copy, so Prompts prefers the
// installed originals on disk whenever it can find them — the embedded pair is
// the fallback, never the preferred source.
//
//go:embed prompts/title.md
var embeddedTitlePrompt string

//go:embed prompts/body.md
var embeddedBodyPrompt string

// Prompts is the pair of canonical element prompts one assist call is built from.
// Source names where they came from — the resolved directory, or "embedded" — so
// a caller can log which copy the proposal was grounded in.
type Prompts struct {
	Title  string
	Body   string
	Source string
}

// refsEnv names the directory holding bead-fmt's references/, overriding the
// plugin-cache probe. Set it when the skill lives somewhere else — a checkout of
// cc-plugins, a test fixture.
const refsEnv = "STRAND_BEAD_FMT_REFS"

// bead-fmt's reference file names, and the plugin-cache path segments Prompts
// probes under a home directory: ~/.claude/plugins/cache/cc-plugins/bdx/<version>/
// skills/bead-fmt/references. The version segment is globbed and the highest
// lexical match wins, which is the newest installed bdx for the zero-padded
// semver-ish directories the plugin cache writes.
const (
	titleFile   = "title-prompt.md"
	bodyFile    = "body-prompt.md"
	sourceEmbed = "embedded"
)

var (
	pluginCacheHead = filepath.Join(".claude", "plugins", "cache", "cc-plugins", "bdx")
	pluginCacheTail = filepath.Join("skills", "bead-fmt", "references")
)

// LoadPrompts resolves the canonical title and body prompts. It reads the
// bead-fmt references directory when it can find one — $STRAND_BEAD_FMT_REFS
// first, then the newest bdx under homeDir's plugin cache — and falls back to the
// vendored copies embedded in the binary. homeDir is injected (never read from
// os.UserHomeDir) so tests choose the root, matching strandmd.Load.
//
// It never fails: an unreadable or half-populated directory yields the embedded
// pair, because assist is advisory and must not break on a plugin layout change.
func LoadPrompts(homeDir string) Prompts {
	for _, dir := range promptDirs(homeDir) {
		title, terr := os.ReadFile(filepath.Join(dir, titleFile))
		body, berr := os.ReadFile(filepath.Join(dir, bodyFile))
		if terr != nil || berr != nil {
			continue // a partial directory is not a source — try the next one
		}
		if strings.TrimSpace(string(title)) == "" || strings.TrimSpace(string(body)) == "" {
			continue
		}
		return Prompts{Title: string(title), Body: string(body), Source: dir}
	}
	return Prompts{Title: embeddedTitlePrompt, Body: embeddedBodyPrompt, Source: sourceEmbed}
}

// promptDirs lists the candidate references directories in preference order: the
// env override, then every installed bdx version under homeDir's plugin cache,
// newest first.
func promptDirs(homeDir string) []string {
	var dirs []string
	if env := strings.TrimSpace(os.Getenv(refsEnv)); env != "" {
		dirs = append(dirs, env)
	}
	if homeDir == "" {
		return dirs
	}
	versions, err := filepath.Glob(filepath.Join(homeDir, pluginCacheHead, "*"))
	if err != nil {
		return dirs
	}
	sort.Sort(sort.Reverse(sort.StringSlice(versions)))
	for _, v := range versions {
		dirs = append(dirs, filepath.Join(v, pluginCacheTail))
	}
	return dirs
}
