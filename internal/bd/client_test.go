package bd

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// TestRun_ReturnsContextErr_When_CancelledWaitingOnHeldLock pins the liveness fix
// from st-kl8: the process-wide single-writer lock (execMu) must honor ctx while a
// caller waits for it. We hold the lock for the whole test so the caller can never
// acquire it; with a context-blind Lock() the caller blocks behind the holder
// indefinitely (the bug). A ctx-aware acquisition returns ctx.Err() promptly even
// though the lock is still held — proving the wait, not just the run, respects ctx.
// Holding the lock the entire time is the proof: run can only exit via ctx.Done().
func TestRun_ReturnsContextErr_When_CancelledWaitingOnHeldLock(t *testing.T) {
	// Acquire and hold the single-writer lock; release only at test end (after we
	// have observed the caller's return) so the lock is unavailable throughout.
	execMu <- struct{}{}
	defer func() { <-execMu }()

	c := &Client{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := c.run(ctx, "list", "--json")
		errCh <- err
	}()

	// Cancel while the caller is parked waiting on the held lock.
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run blocked on the held lock past ctx cancel; execMu must honor ctx")
	}
}

// TestDecodeIssuePriority pins the Priority decode contract now that Issue.Priority
// is *int. A present field decodes to a non-nil pointer (0 included); an absent
// field decodes to nil — no longer collapsing to a false P0.
func TestDecodeIssuePriority(t *testing.T) {
	cases := []struct {
		name string
		json string
		want *int
	}{
		{name: "present zero is P0", json: `[{"id":"a-1","priority":0}]`, want: new(0)},
		{name: "present nonzero round-trips", json: `[{"id":"a-2","priority":2}]`, want: new(2)},
		{name: "present highest boundary", json: `[{"id":"a-3","priority":4}]`, want: new(4)},
		{name: "absent decodes to nil (no false P0)", json: `[{"id":"a-4"}]`, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues, err := decodeIssues([]byte(tc.json))
			if err != nil {
				t.Fatalf("decodeIssues: %v", err)
			}
			if len(issues) != 1 {
				t.Fatalf("got %d issues, want 1", len(issues))
			}
			got := issues[0].Priority
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("Priority = %d, want nil", *got)
			case tc.want != nil && got == nil:
				t.Errorf("Priority = nil, want %d", *tc.want)
			case tc.want != nil && got != nil && *got != *tc.want:
				t.Errorf("Priority = %d, want %d", *got, *tc.want)
			}
		})
	}
}

// TestDecodeStatsWireShape pins the `summary` wrapper key that Stats() depends on.
// bd stats --json emits { "summary": { "open_issues": N, … } }; if that key is
// ever renamed or removed, json.Unmarshal silently returns an all-zero Stats with
// no error. This test asserts:
//
//  1. A realistic wire payload decodes to the expected non-zero Stats.
//  2. A payload with the wrong wrapper key ("data" instead of "summary") yields
//     all-zero Stats — documenting and guarding the silent-degradation risk.
func TestDecodeStatsWireShape(t *testing.T) {
	// realisticPayload mirrors `bd stats --json` output. Field names must match
	// Stats json tags exactly — this is the contract we are pinning.
	const realisticPayload = `{
		"summary": {
			"open_issues": 5,
			"in_progress_issues": 2,
			"blocked_issues": 1,
			"closed_issues": 10,
			"deferred_issues": 3,
			"total_issues": 21
		}
	}`

	t.Run("correct summary key decodes non-zero Stats", func(t *testing.T) {
		var resp struct {
			Summary Stats `json:"summary"`
		}
		if err := json.Unmarshal([]byte(realisticPayload), &resp); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		s := resp.Summary
		if s.Open != 5 || s.InProgress != 2 || s.Blocked != 1 || s.Closed != 10 || s.Deferred != 3 || s.Total != 21 {
			t.Errorf("Stats fields mismatch: got %+v; check Stats json tags match the bd wire format", s)
		}
	})

	// Document the silent-degradation risk: a wrong wrapper key yields zero Stats
	// and no error, so callers cannot detect the failure. This test pins that the
	// Stats struct itself is not broken — only the outer decode path would be.
	t.Run("wrong wrapper key silently yields zero Stats (silent-degradation pinned)", func(t *testing.T) {
		wrongKey := `{"data":{"open_issues":5,"in_progress_issues":2,"total_issues":7}}`
		var resp struct {
			Summary Stats `json:"summary"`
		}
		if err := json.Unmarshal([]byte(wrongKey), &resp); err != nil {
			t.Fatalf("unexpected Unmarshal error with wrong key: %v", err)
		}
		s := resp.Summary
		if s.Open != 0 || s.Total != 0 {
			t.Errorf("expected all-zero Stats on missing 'summary' key, got %+v", s)
		}
	})

	t.Run("Stats json field tags round-trip correctly", func(t *testing.T) {
		// Encode a known Stats value and decode it back through the summary wrapper.
		// Regression: if any Stats json tag is wrong, the round-trip produces zero.
		original := Stats{Open: 3, InProgress: 1, Blocked: 0, Closed: 7, Deferred: 2, Total: 13}
		payload, err := json.Marshal(struct {
			Summary Stats `json:"summary"`
		}{Summary: original})
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		var got struct {
			Summary Stats `json:"summary"`
		}
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("json.Unmarshal round-trip: %v", err)
		}
		if got.Summary != original {
			t.Errorf("round-trip mismatch: got %+v, want %+v", got.Summary, original)
		}
	})
}

// TestRankPresentZero guards the present-zero case: a metadata rank of exactly 0
// must return (0, true), not (0, false). The existing test suite covers absent,
// string-encoded, and garbage values but not float64(0). This gap is the exact
// regression a bare-float64 simplification of the Rank switch would introduce —
// returning false for a numerically-zero-but-present rank.
func TestRankPresentZero(t *testing.T) {
	cases := []struct {
		name    string
		issue   Issue
		wantVal float64
		wantOk  bool
	}{
		{
			name:    "present zero returns (0, true)",
			issue:   Issue{Metadata: map[string]any{"rank": float64(0)}},
			wantVal: 0,
			wantOk:  true,
		},
		{
			name:    "absent rank key returns (0, false)",
			issue:   Issue{},
			wantVal: 0,
			wantOk:  false,
		},
		{
			name:    "nil metadata returns (0, false)",
			issue:   Issue{Metadata: nil},
			wantVal: 0,
			wantOk:  false,
		},
		{
			name:    "present positive float returns correct value",
			issue:   Issue{Metadata: map[string]any{"rank": float64(3.5)}},
			wantVal: 3.5,
			wantOk:  true,
		},
		{
			name:    "present negative float returns correct value",
			issue:   Issue{Metadata: map[string]any{"rank": float64(-1.0)}},
			wantVal: -1.0,
			wantOk:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotVal, gotOk := tc.issue.Rank()
			if gotOk != tc.wantOk {
				t.Errorf("Rank() ok = %v, want %v", gotOk, tc.wantOk)
			}
			if gotVal != tc.wantVal {
				t.Errorf("Rank() val = %v, want %v", gotVal, tc.wantVal)
			}
		})
	}
}

// TestDecodeIssueAbsentPriorityIsIndistinguishable was the red-on-fix handoff from
// str-vuq: with a plain int, "priority":0 and an absent field decoded to the SAME
// value. Now that Priority is *int the two are distinct — present-0 is a non-nil
// pointer to 0, absent is nil. This test asserts that distinction is real.
func TestDecodeIssueAbsentPriorityIsIndistinguishable(t *testing.T) {
	withZero, err := decodeIssues([]byte(`[{"id":"x","priority":0}]`))
	if err != nil {
		t.Fatalf("decode present-zero: %v", err)
	}
	absent, err := decodeIssues([]byte(`[{"id":"x"}]`))
	if err != nil {
		t.Fatalf("decode absent: %v", err)
	}
	if len(withZero) != 1 || len(absent) != 1 {
		t.Fatalf("expected exactly 1 issue in each result, got len(withZero)=%d, len(absent)=%d", len(withZero), len(absent))
	}
	if withZero[0].Priority == nil {
		t.Fatal("present-0 decoded to nil; expected a non-nil pointer to 0")
	}
	if *withZero[0].Priority != 0 {
		t.Fatalf("present-0 = %d, want 0", *withZero[0].Priority)
	}
	if absent[0].Priority != nil {
		t.Fatalf("absent = %d, want nil — the int collapse must be closed", *absent[0].Priority)
	}
}
