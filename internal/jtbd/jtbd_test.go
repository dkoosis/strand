package jtbd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseTableResolves(t *testing.T) {
	body := `# Jobs to be done

Some prose before the table.

| id    | job                          |
|-------|------------------------------|
| j-001 | Triage what to work on next  |
| j-002 | See why a bead matters       |

Trailing prose.`
	reg := parse(body)
	cases := map[string]string{
		"j-001": "Triage what to work on next",
		"j-002": "See why a bead matters",
	}
	for id, want := range cases {
		got, ok := reg.Resolve(id)
		if !ok || got != want {
			t.Errorf("Resolve(%q) = (%q, %v), want (%q, true)", id, got, ok, want)
		}
	}
	if _, ok := reg.Resolve("id"); ok {
		t.Error("header row leaked into the registry")
	}
	if _, ok := reg.Resolve("j-999"); ok {
		t.Error("Resolve of an absent id reported ok")
	}
}

func TestCite(t *testing.T) {
	tests := []struct {
		desc   string
		wantID string
		wantOK bool
	}{
		{"why this matters\nJTBD: j-001\nmore", "j-001", true},
		{"jtbd: j-7.a citation lowercased", "j-7.a", true},
		{"no citation here", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		gotID, gotOK := Cite(tc.desc)
		if gotID != tc.wantID || gotOK != tc.wantOK {
			t.Errorf("Cite(%q) = (%q, %v), want (%q, %v)", tc.desc, gotID, gotOK, tc.wantID, tc.wantOK)
		}
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	reg := Load(t.TempDir()) // no docs/jtbd.md
	if _, ok := reg.Resolve("j-001"); ok {
		t.Error("empty registry resolved an id")
	}
	if _, ok := (Registry{}).Resolve("j-001"); ok {
		t.Error("zero-value registry resolved an id")
	}
}

func TestLoadReadsRepoFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "| id | job |\n|----|-----|\n| j-001 | Ship the thing |\n"
	if err := os.WriteFile(filepath.Join(dir, "docs", "jtbd.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := Load(dir).Resolve("j-001")
	if !ok || got != "Ship the thing" {
		t.Errorf("Load.Resolve = (%q, %v), want (%q, true)", got, ok, "Ship the thing")
	}
}
