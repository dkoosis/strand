package bdcounts

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
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

// TestLookupIgnoresMetaKey is the schema-coexistence guard: the reserved _meta key
// (st-18l's liveness stamp) rides alongside the repo rows, and a keyed Lookup must be
// blind to it — both that _meta never masquerades as a repo and that its presence
// doesn't perturb a real row's decode.
func TestLookupIgnoresMetaKey(t *testing.T) {
	r := writeCounts(t, `{"_meta":{"lastRun":1710000000,"version":"1.2.3"},"/repo/a":{"bh":1,"bo":2}}`)
	if _, ok := r.Lookup("_meta"); ok {
		t.Error("Lookup(_meta) = ok — the meta key must not read as a repo row")
	}
	got, ok := r.Lookup("/repo/a")
	if !ok || got.Waiting != 1 || got.Open != 2 {
		t.Errorf("Lookup(/repo/a) = %+v ok=%v, want Waiting 1 Open 2 alongside _meta", got, ok)
	}
}

// TestIsStale pins the stale-detection rule (st-18l): a last run older than 2× the
// refresh interval is stale; anything fresher is not; an absent stamp (0) is never
// stale (its unknown-ness is surfaced by Meta's ok, not by a false alarm here).
func TestIsStale(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	within := int64(2 * RefreshInterval / time.Second) // exactly on the 2× boundary — not yet stale
	tests := []struct {
		name    string
		lastRun int64
		want    bool
	}{
		{"fresh — just ran", now.Unix(), false},
		{"one interval old — a single missed tick is fine", now.Add(-RefreshInterval).Unix(), false},
		{"exactly 2× — the boundary is not yet stale", now.Unix() - within, false},
		{"just past 2× — stale", now.Unix() - within - 1, true},
		{"long dead", now.Add(-time.Hour).Unix(), true},
		{"no stamp", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsStale(tc.lastRun, now); got != tc.want {
				t.Errorf("IsStale(lastRun=%d) = %v, want %v", tc.lastRun, got, tc.want)
			}
		})
	}
}

// TestReaderStaleAndMeta covers the file-backed wrappers: Meta round-trips the stamp,
// a stale stamp reads stale, and an unstamped or missing file reports ok=false so the
// caller shows no flag rather than a false alarm.
func TestReaderStaleAndMeta(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	t.Run("stamped + stale", func(t *testing.T) {
		old := now.Add(-time.Hour).Unix()
		r := writeCounts(t, `{"/repo/a":{"bh":1},"_meta":{"lastRun":`+strconv.FormatInt(old, 10)+`,"version":"9.9.9"}}`)
		m, ok := r.Meta()
		if !ok || m.Version != "9.9.9" || m.LastRun != old {
			t.Fatalf("Meta = %+v ok=%v, want lastRun %d version 9.9.9", m, ok, old)
		}
		stale, ok := r.Stale(now)
		if !ok || !stale {
			t.Errorf("Stale = %v ok=%v, want stale ok", stale, ok)
		}
	})

	t.Run("stamped + fresh", func(t *testing.T) {
		r := writeCounts(t, `{"/repo/a":{"bh":1},"_meta":{"lastRun":`+strconv.FormatInt(now.Unix(), 10)+`,"version":"v"}}`)
		stale, ok := r.Stale(now)
		if !ok || stale {
			t.Errorf("Stale = %v ok=%v, want fresh ok", stale, ok)
		}
	})

	t.Run("no meta stamp — ok false", func(t *testing.T) {
		r := writeCounts(t, `{"/repo/a":{"bh":1}}`)
		if _, ok := r.Meta(); ok {
			t.Error("Meta() over an unstamped file = ok, want !ok")
		}
		if stale, ok := r.Stale(now); ok || stale {
			t.Errorf("Stale over unstamped file = %v ok=%v, want not-stale and not-ok", stale, ok)
		}
	})

	t.Run("missing file — ok false", func(t *testing.T) {
		r := NewReaderAt(filepath.Join(t.TempDir(), "nope.json"))
		if _, ok := r.Stale(now); ok {
			t.Error("Stale over a missing file = ok, want !ok")
		}
	})
}
