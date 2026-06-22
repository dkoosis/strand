package bd

import (
	"context"
	"errors"
	"testing"
)

// TestClassifyMapsBDMessages pins the phrase->sentinel mapping the HTTP layer
// relies on. bd reports kind only via message text, so these phrases are the
// contract; if a bd upgrade changes them, this test is where it surfaces.
func TestClassifyMapsBDMessages(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want error
	}{
		{"show missing", `Error fetching x-9: no issue found matching "x-9"`, ErrNotFound},
		{"list missing ids", "no issues found matching the provided IDs", ErrNotFound},
		{"bad status", `invalid status "bogus" (valid: open, closed)`, ErrInvalidArg},
		{"unknown failure", "dolt: connection refused", ErrBD},
		{"invalid mid-string is not arg error", "dolt: invalid connection handle", ErrBD},
		{"empty", "", ErrBD},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classify(tt.msg)
			if !errors.Is(got, tt.want) {
				t.Errorf("classify(%q) = %v, want errors.Is %v", tt.msg, got, tt.want)
			}
		})
	}
}

// TestShowMissingIssueIsNotFound is the end-to-end path the 502 bug was about:
// bd exits non-zero with a "no issue found" stderr, and Show must surface
// ErrNotFound (not a generic ErrBD) so the server can answer 404.
func TestShowMissingIssueIsNotFound(t *testing.T) {
	c, _ := fakeBD(t, `echo 'Error fetching x-9: no issue found matching "x-9"' >&2; exit 1`)
	_, err := c.Show(context.Background(), "x-9")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Show missing = %v, want ErrNotFound", err)
	}
}

// TestListBadStatusIsInvalidArg covers bd's quirk: a bad --status exits 0 but
// emits a JSON error on stdout. decodeIssues must classify it as ErrInvalidArg.
func TestListBadStatusIsInvalidArg(t *testing.T) {
	c, _ := fakeBD(t, `echo '{"error":"invalid status \"bogus\" (valid: open)","schema_version":1}'`)
	_, err := c.List(context.Background(), "--status", "bogus")
	if !errors.Is(err, ErrInvalidArg) {
		t.Fatalf("List bad status = %v, want ErrInvalidArg", err)
	}
}
