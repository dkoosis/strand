package bdcounts

import (
	"os"
	"path/filepath"
	"testing"
)

// writeCounts drops a counts.json in a temp dir and returns a Reader over it.
func writeCounts(t *testing.T, body string) *Reader {
	t.Helper()
	path := filepath.Join(t.TempDir(), "counts.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write counts.json: %v", err)
	}
	return NewReaderAt(path)
}

// TestLookupMapsBuckets pins the bh/bo/bw/bb/bcl/bdf → glyph mapping against a row
// shaped exactly like the agent writes (extra keys the reader must ignore included).
func TestLookupMapsBuckets(t *testing.T) {
	r := writeCounts(t, `{"/repo/a":{"root":"/repo/a","prefix":"a","bh":1,"bo":2,"bw":3,"bb":4,"bcl":5,"bdf":6,"ts":99}}`)
	got, ok := r.Lookup("/repo/a")
	if !ok {
		t.Fatal("Lookup(/repo/a) = !ok, want the row")
	}
	want := Buckets{Waiting: 1, Open: 2, InProgress: 3, Blocked: 4, Closed: 5, Deferred: 6}
	if got != want {
		t.Errorf("Lookup = %+v, want %+v", got, want)
	}
}

// TestLookupCleansPath checks a trailing slash still matches the agent's canonical
// key, so a repo path that arrives un-normalized doesn't spuriously miss.
func TestLookupCleansPath(t *testing.T) {
	r := writeCounts(t, `{"/repo/a":{"bh":7}}`)
	got, ok := r.Lookup("/repo/a/")
	if !ok || got.Waiting != 7 {
		t.Errorf("Lookup(/repo/a/) = %+v ok=%v, want Waiting 7", got, ok)
	}
}

// TestLookupMisses covers every not-ok path — the caller must fall back to bd for
// each: no such repo, no file, malformed JSON.
func TestLookupMisses(t *testing.T) {
	t.Run("unknown repo", func(t *testing.T) {
		r := writeCounts(t, `{"/repo/a":{"bh":1}}`)
		if _, ok := r.Lookup("/repo/b"); ok {
			t.Error("Lookup of an unlisted repo = ok, want miss")
		}
	})
	t.Run("missing file", func(t *testing.T) {
		r := NewReaderAt(filepath.Join(t.TempDir(), "nope.json"))
		if _, ok := r.Lookup("/repo/a"); ok {
			t.Error("Lookup with no file = ok, want miss")
		}
	})
	t.Run("malformed json", func(t *testing.T) {
		r := writeCounts(t, `{not json`)
		if _, ok := r.Lookup("/repo/a"); ok {
			t.Error("Lookup over malformed json = ok, want miss")
		}
	})
}
