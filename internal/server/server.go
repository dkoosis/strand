// Package server is strand's HTTP layer: a small JSON API over a bd.Client plus
// the embedded web UI that consumes it.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/dkoosis/strand/internal/bd"
)

// Server holds the bd client and serves both the API and the static UI.
type Server struct {
	bd     *bd.Client
	static http.Handler
}

// New builds a Server. static is the file system holding the web UI (templates
// and assets), wired in by the caller so package server stays free of embed.
func New(client *bd.Client, static http.Handler) *Server {
	return &Server{bd: client, static: static}
}

// Routes returns the mux with the API and UI wired up.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/issues", s.handleIssues)
	mux.HandleFunc("GET /api/ready", s.handleReady)
	mux.HandleFunc("GET /api/issues/{id}", s.handleShow)
	mux.Handle("/", s.static)
	return mux
}

// reqContext bounds every bd shell-out so a hung CLI can't wedge a request.
func reqContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 10*time.Second)
}

func (s *Server) handleIssues(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	var args []string
	if status := r.URL.Query().Get("status"); status != "" {
		args = append(args, "--status", status)
	}
	issues, err := s.bd.List(ctx, args...)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, issues)
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	issues, err := s.bd.Ready(ctx)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, issues)
}

func (s *Server) handleShow(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	issue, err := s.bd.Show(ctx, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, issue)
}

func writeJSON(w http.ResponseWriter, v any) {
	// Marshal before touching the response so a marshal failure becomes a clean
	// error instead of a half-written body with a 200 already committed.
	buf, err := json.Marshal(v)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(buf)
}

func writeError(w http.ResponseWriter, err error) {
	buf, marshalErr := json.Marshal(map[string]string{"error": err.Error()})
	if marshalErr != nil {
		buf = []byte(`{"error":"internal error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusForError(err))
	_, _ = w.Write(buf)
}

// statusForError maps a bd error to an HTTP status so the UI can tell a missing
// issue (404) from bad input (400) from a real upstream failure (502). An error
// from no bd sentinel (e.g. a JSON marshal failure in writeJSON) is ours: 500.
func statusForError(err error) int {
	switch {
	case errors.Is(err, bd.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, bd.ErrInvalidArg):
		return http.StatusBadRequest
	case errors.Is(err, bd.ErrBD):
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}
