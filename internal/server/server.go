// Package server is strand's HTTP layer: it renders the embedded web UI as HTML
// (html/template) and swaps fragments over htmx. It reads beads through a small
// issue source so the bd CLI stays the only data path (spec D8).
package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"time"

	"github.com/dkoosis/strand/internal/bd"
	"github.com/dkoosis/strand/internal/forest"
)

// issueSource is the slice of bd.Client the server needs: reads plus the V1
// writes. An interface keeps the handlers testable with a stub and the bd CLI
// behind one seam (spec Q0). Writes go through the write-client (spec D6) so the
// bare-`bd update` footgun stays impossible.
type issueSource interface {
	List(ctx context.Context, args ...string) ([]bd.Issue, error)
	Show(ctx context.Context, id string) (*bd.Issue, error)
	Update(ctx context.Context, id, field, value string) (*bd.Issue, error)
	Claim(ctx context.Context, id string) (*bd.Issue, error)
	Close(ctx context.Context, id, reason string) (*bd.Issue, error)
}

// Server renders the forest landing and its htmx fragments over an issueSource.
type Server struct {
	src    issueSource
	tmpl   *template.Template
	static http.Handler
	syn    forest.Synthesis
}

// New builds a Server. tmpl holds the parsed UI templates and static serves the
// embedded assets, both wired in by the caller so package server stays free of
// embed. syn is the human-shaped synthesis layer (project label, north star).
func New(src issueSource, tmpl *template.Template, static http.Handler, syn forest.Synthesis) *Server {
	return &Server{src: src, tmpl: tmpl, static: static, syn: syn}
}

// Routes returns the mux: the forest page, its htmx fragments, and static assets.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleForest)
	mux.HandleFunc("GET /list", s.handleList)
	mux.HandleFunc("GET /bead/{id}", s.handleBead)
	mux.HandleFunc("PATCH /bead/{id}", s.handleEdit)
	mux.HandleFunc("POST /bead/{id}/claim", s.handleClaim)
	mux.HandleFunc("POST /bead/{id}/close", s.handleClose)
	mux.HandleFunc("POST /bead/{id}/reopen", s.handleReopen)
	mux.Handle("GET /static/", s.static)
	return mux
}

// reqContext bounds every bd shell-out so a hung CLI can't wedge a request.
func reqContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 10*time.Second)
}

// listView is the bead-list pane: a region, optionally narrowed to one epic.
type listView struct {
	Region  forest.Region
	Epic    forest.Epic
	HasEpic bool // false = show the whole region
}

// pageData is the full landing render.
type pageData struct {
	Forest forest.Forest
	List   listView
}

func (s *Server) handleForest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	f, err := s.buildForest(ctx)
	if err != nil {
		s.renderError(w, err)
		return
	}
	s.render(w, "page", pageData{Forest: f, List: listViewFor(f, "")})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	f, err := s.buildForest(ctx)
	if err != nil {
		s.renderError(w, err)
		return
	}
	// epic=<id> narrows the pane to a single tile; absent means the whole region.
	s.render(w, "list", listViewFor(f, r.URL.Query().Get("epic")))
}

// listViewFor builds the bead-list pane from the forest: its first region,
// optionally narrowed to one epic by id. An empty forest yields an empty view.
// Both the full-page render and the htmx list swap go through here, so the two
// panes can't diverge.
func listViewFor(f forest.Forest, epicID string) listView {
	if len(f.Regions) == 0 {
		return listView{}
	}
	view := listView{Region: f.Regions[0]}
	if epicID != "" {
		for _, e := range view.Region.Epics {
			if e.ID == epicID {
				view.Epic, view.HasEpic = e, true
				break
			}
		}
	}
	return view
}

// drawerData is the detail panel: a bead plus an optional write error. Embedding
// promotes the issue's fields, so the template reads .Title etc. directly, and
// .Err carries a bd write failure to show inline (spec Q2).
type drawerData struct {
	*bd.Issue
	Err string
}

func (s *Server) handleBead(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	issue, err := s.src.Show(ctx, r.PathValue("id"))
	if err != nil {
		s.renderError(w, err)
		return
	}
	s.render(w, "drawer", drawerData{Issue: issue})
}

// handleEdit writes one field from the detail panel (hx-patch with field+value).
func (s *Server) handleEdit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	field, value := r.FormValue("field"), r.FormValue("value")
	s.writeAndRefresh(w, r, id, func(ctx context.Context) error {
		_, err := s.src.Update(ctx, id, field, value)
		return wrapWrite("edit", err)
	})
}

// handleClaim assigns the bead to the current user.
func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.writeAndRefresh(w, r, id, func(ctx context.Context) error {
		_, err := s.src.Claim(ctx, id)
		return wrapWrite("claim", err)
	})
}

// handleClose closes the bead, with an optional reason.
func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	reason := r.FormValue("reason")
	s.writeAndRefresh(w, r, id, func(ctx context.Context) error {
		_, err := s.src.Close(ctx, id, reason)
		return wrapWrite("close", err)
	})
}

// handleReopen flips a closed bead back to open. There is no bd `reopen` in the
// write-client; a status write does it (O7: status goes through update -s).
func (s *Server) handleReopen(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.writeAndRefresh(w, r, id, func(ctx context.Context) error {
		_, err := s.src.Update(ctx, id, "status", "open")
		return wrapWrite("reopen", err)
	})
}

// wrapWrite tags a write failure with the action so the surfaced message names
// what the user tried; nil stays nil. bd's own message is preserved via %w.
func wrapWrite(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", action, err)
}

// writeAndRefresh runs a write, then re-reads the bead and redraws the drawer, so
// the panel always shows bd's truth — never an optimistic guess (spec Q2). A
// write error is shown inside the drawer next to the unchanged, re-read values:
// the user sees bd's message and the UI never claims a change that didn't land.
// If the re-read itself fails, fall back to the hard error page.
func (s *Server) writeAndRefresh(w http.ResponseWriter, r *http.Request, id string, write func(context.Context) error) {
	ctx, cancel := reqContext(r)
	defer cancel()
	writeErr := write(ctx)
	issue, showErr := s.src.Show(ctx, id)
	if showErr != nil {
		s.renderError(w, showErr)
		return
	}
	data := drawerData{Issue: issue}
	if writeErr != nil {
		data.Err = writeErr.Error()
	}
	s.render(w, "drawer", data)
}

// buildForest pulls the live issue list once and folds it into the landing model.
func (s *Server) buildForest(ctx context.Context) (forest.Forest, error) {
	issues, err := s.src.List(ctx)
	if err != nil {
		return forest.Forest{}, fmt.Errorf("list issues: %w", err)
	}
	return forest.Build(issues, s.syn), nil
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	if err := s.renderStatus(w, name, data, http.StatusOK); err != nil {
		log.Printf("strand: render %q: %v", name, err)
		s.renderError(w, err)
	}
}

// renderStatus renders a template into a buffer first, then writes it with the
// given status — so a template failure becomes a clean error instead of a 200
// with a half-written body. On failure it writes nothing and returns the error.
func (s *Server) renderStatus(w http.ResponseWriter, name string, data any, code int) error {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		return fmt.Errorf("render %q: %w", name, err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_, _ = buf.WriteTo(w)
	return nil
}

// renderError sends an HTML error fragment with the status mapped from the bd
// error, so htmx and a plain browser both show something legible. If the error
// template itself fails, it falls back to a plaintext error.
func (s *Server) renderError(w http.ResponseWriter, err error) {
	code := statusForError(err)
	if rerr := s.renderStatus(w, "error", err.Error(), code); rerr != nil {
		log.Printf("strand: render error page: %v", rerr)
		http.Error(w, err.Error(), code)
	}
}

// statusForError maps a bd error to an HTTP status so the UI can tell a missing
// issue (404) from bad input (400) from a real upstream failure (502). An error
// from no bd sentinel (e.g. a template failure) is ours: 500.
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
