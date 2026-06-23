package bd

import "testing"

func pi(n int) *int { return &n }

// TestDecodeIssuePriority pins the Priority decode contract now that Issue.Priority
// is *int. A present field decodes to a non-nil pointer (0 included); an absent
// field decodes to nil — no longer collapsing to a false P0.
func TestDecodeIssuePriority(t *testing.T) {
	cases := []struct {
		name string
		json string
		want *int
	}{
		{name: "present zero is P0", json: `[{"id":"a-1","priority":0}]`, want: pi(0)},
		{name: "present nonzero round-trips", json: `[{"id":"a-2","priority":2}]`, want: pi(2)},
		{name: "present highest boundary", json: `[{"id":"a-3","priority":4}]`, want: pi(4)},
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
