package suggest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRefs writes a title/body prompt pair into dir, creating it.
func writeRefs(t *testing.T, dir, title, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, titleFile), []byte(title), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, bodyFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// cacheRefs is the references dir for one installed bdx version under home.
func cacheRefs(home, version string) string {
	return filepath.Join(home, pluginCacheHead, version, pluginCacheTail)
}

// TestLoadPromptsPrefersDisk: an installed bead-fmt wins over the vendored copies,
// so the canonical prompts are the live source whenever they can be found — the
// embedded pair exists only so the binary works without the plugin.
func TestLoadPromptsPrefersDisk(t *testing.T) {
	home := t.TempDir()
	writeRefs(t, cacheRefs(home, "0.31.1"), "disk title rules", "disk body rules")

	got := LoadPrompts(home)
	if got.Title != "disk title rules" || got.Body != "disk body rules" {
		t.Errorf("LoadPrompts read %q/%q, want the on-disk pair", got.Title, got.Body)
	}
	if got.Source == sourceEmbed {
		t.Error("Source reports embedded though an installed pair was found")
	}
}

// TestLoadPromptsPicksNewestVersion: with several bdx versions installed, the newest
// wins — a stale version left in the cache must not ground the proposal.
func TestLoadPromptsPicksNewestVersion(t *testing.T) {
	home := t.TempDir()
	writeRefs(t, cacheRefs(home, "0.29.0"), "old title rules", "old body rules")
	writeRefs(t, cacheRefs(home, "0.31.1"), "new title rules", "new body rules")

	if got := LoadPrompts(home); got.Title != "new title rules" {
		t.Errorf("LoadPrompts read %q, want the newest installed version", got.Title)
	}
}

// TestLoadPromptsOrdersVersionsNumerically: version segments compare as numbers, not
// strings. Lexically "0.9.0" outranks both "0.10.0" and "0.31.1", which would ground
// every proposal in whatever ancient bdx was left in the cache.
func TestLoadPromptsOrdersVersionsNumerically(t *testing.T) {
	for _, tt := range []struct {
		name  string
		older string
		newer string
	}{
		{"single vs double digit minor", "0.9.0", "0.10.0"},
		{"stale version alongside current", "0.9.0", "0.31.1"},
		{"double vs triple digit minor", "0.99.0", "0.100.0"},
		{"patch within a minor", "0.31.2", "0.31.10"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			writeRefs(t, cacheRefs(home, tt.older), "old title rules", "old body rules")
			writeRefs(t, cacheRefs(home, tt.newer), "new title rules", "new body rules")

			if got := LoadPrompts(home); got.Title != "new title rules" {
				t.Errorf("with %s and %s installed, LoadPrompts read %q, want %s's pair",
					tt.older, tt.newer, got.Title, tt.newer)
			}
		})
	}
}

// TestLoadPromptsRanksUnnumberedLast: a non-numeric cache directory (a "dev" checkout)
// sorts below every real version rather than winning the probe.
func TestLoadPromptsRanksUnnumberedLast(t *testing.T) {
	home := t.TempDir()
	writeRefs(t, cacheRefs(home, "dev"), "dev title rules", "dev body rules")
	writeRefs(t, cacheRefs(home, "0.31.1"), "new title rules", "new body rules")

	if got := LoadPrompts(home); got.Title != "new title rules" {
		t.Errorf("LoadPrompts read %q, want the numbered version over 'dev'", got.Title)
	}
}

// TestLoadPromptsEnvOverride: $STRAND_BEAD_FMT_REFS outranks the plugin cache, so a
// cc-plugins checkout can be pointed at directly.
func TestLoadPromptsEnvOverride(t *testing.T) {
	home := t.TempDir()
	writeRefs(t, cacheRefs(home, "0.31.1"), "cache title rules", "cache body rules")
	t.Setenv(refsEnv, writeRefs(t, filepath.Join(t.TempDir(), "refs"), "env title rules", "env body rules"))

	if got := LoadPrompts(home); got.Title != "env title rules" {
		t.Errorf("LoadPrompts read %q, want the env-override pair", got.Title)
	}
}

// TestLoadPromptsFallsBackToEmbedded covers every way disk resolution can come up
// empty. Assist is advisory: it must never fail on a plugin layout change, so each
// case yields the vendored pair rather than an error or a half-loaded prompt.
func TestLoadPromptsFallsBackToEmbedded(t *testing.T) {
	partial := t.TempDir()
	if err := os.WriteFile(filepath.Join(partial, titleFile), []byte("only the title"), 0o644); err != nil {
		t.Fatal(err)
	}
	blank := writeRefs(t, filepath.Join(t.TempDir(), "blank"), "   \n", "   \n")

	tests := []struct {
		name string
		home string
		env  string
	}{
		{name: "no home", home: ""},
		{name: "nothing installed", home: t.TempDir()},
		{name: "override missing", home: t.TempDir(), env: filepath.Join(t.TempDir(), "absent")},
		{name: "only one file present", home: t.TempDir(), env: partial},
		{name: "files are blank", home: t.TempDir(), env: blank},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(refsEnv, tt.env)
			got := LoadPrompts(tt.home)
			if got.Source != sourceEmbed {
				t.Errorf("Source = %q, want %q", got.Source, sourceEmbed)
			}
			if got.Title != embeddedTitlePrompt || got.Body != embeddedBodyPrompt {
				t.Error("fallback did not return the vendored prompt pair")
			}
		})
	}
}

// TestEmbeddedPromptsCarryTheRules: the vendored copies must carry the
// deterministic rules inline, since they ground every proposal on a machine with no
// bdx installed. This is the guard against vendoring a stub — the drift that
// retiring the hand-copied tier2Instruction/requiredSections was meant to end.
func TestEmbeddedPromptsCarryTheRules(t *testing.T) {
	title := strings.ToLower(embeddedTitlePrompt)
	for _, want := range []string{
		"names the done state", "literal and searchable", "self-contained", "one outcome",
		"vague verbs", "phase and version labels",
	} {
		if !strings.Contains(title, want) {
			t.Errorf("embedded title prompt missing the rule %q", want)
		}
	}
	body := strings.ToLower(embeddedBodyPrompt)
	for _, want := range []string{
		"success criteria", "steps to reproduce", "goal", "findings",
		"first line", "resolves by search",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("embedded body prompt missing the rule %q", want)
		}
	}
}
