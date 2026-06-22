package server

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/dkoosis/strand/internal/bd"
)

// errMarshal stands in for a non-bd error (e.g. a JSON marshal failure in
// writeJSON) — the kind statusForError must treat as our own 500.
var errMarshal = errors.New("json: unsupported type")

// TestStatusForError pins the bd-error -> HTTP-status mapping the htmx UI needs
// to render the right state. Wrapped errors must classify through errors.Is, so
// each case wraps its sentinel the way the bd client does in the field.
func TestStatusForError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"not found", fmt.Errorf("bd show x-9: %w", bd.ErrNotFound), http.StatusNotFound},
		{"invalid arg", fmt.Errorf("bd list: %w", bd.ErrInvalidArg), http.StatusBadRequest},
		{"bd failure", fmt.Errorf("bd ready: %w", bd.ErrBD), http.StatusBadGateway},
		{"unclassified is internal", errMarshal, http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusForError(tt.err); got != tt.want {
				t.Errorf("statusForError(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}
