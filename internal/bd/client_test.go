package bd

import "testing"

// TestDecodeIssuePriority pins the Priority decode contract (zero-sentinel F1).
//
// Issue.Priority is a plain int with `json:"priority"`, decoded by decodeIssues
// — the shared path behind both List and Show. The hazard: an int cannot tell
// "priority present and 0" (a real P0) from "priority absent" (Go's zero value,
// also 0). client.go closes this hazard with a guarantee comment that bd's
// list/show JSON ALWAYS emits priority. No test pinned that contract; a future
// bd that drops the field would silently revive false P0s.
//
// This test pins the contract so a regression is visible:
//   - present 0 -> 0 (P0), present 2 -> 2: the value round-trips.
//   - absent    -> 0: documents the collapse. If bd ever stops emitting
//     priority, this case is where a false P0 enters; the test names it as the
//     structural blind spot (a plain int cannot distinguish absent from 0).
func TestDecodeIssuePriority(t *testing.T) {
	cases := []struct {
		name string
		json string
		want int
	}{
		{
			name: "present zero is P0",
			json: `[{"id":"a-1","priority":0}]`,
			want: 0,
		},
		{
			name: "present nonzero round-trips",
			json: `[{"id":"a-2","priority":2}]`,
			want: 2,
		},
		{
			name: "present highest boundary",
			json: `[{"id":"a-3","priority":4}]`,
			want: 4,
		},
		{
			// The hazard case. bd's contract says this never happens; if it ever
			// does, Priority collapses to 0 (P0) indistinguishably from a real P0.
			name: "absent collapses to zero (P0) — bd-contract hazard",
			json: `[{"id":"a-4"}]`,
			want: 0,
		},
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
			if got := issues[0].Priority; got != tc.want {
				t.Errorf("Priority = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestDecodeIssueAbsentPriorityIsIndistinguishable makes the structural blind
// spot explicit and machine-checked: with Priority as a plain int, JSON with
// "priority":0 and JSON without the field decode to the SAME value. This is the
// thing the guarantee comment in client.go promises bd will never do. If a
// future maintainer hardens the type (e.g. *int) to actually distinguish the
// two, this test should be updated to assert the distinction instead — its
// failure then is the signal that the hazard was closed for real.
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
		t.Fatalf("expected exactly 1 issue in each decoded result, got len(withZero)=%d, len(absent)=%d", len(withZero), len(absent))
	}
	if withZero[0].Priority != absent[0].Priority {
		t.Fatalf("present-0 (%d) and absent (%d) now differ — the int collapse is "+
			"closed; update Priority's guarantee comment and this test",
			withZero[0].Priority, absent[0].Priority)
	}
}
