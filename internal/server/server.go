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

// issueSource is the slice of bd.Client the server needs. An interface keeps the
// handlers testable with a stub and the bd CLI behind one seam (spec Q0).
type issueSource interface {
	List(ctx context.Context, args ...string) ([]bd.Issue, error)
	Show(ctx context.Context, id string) (*bd.Issue, error)
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
	data := pageData{Forest: f}
	if len(f.Regions) > 0 {
		data.List = listView{Region: f.Regions[0]}
	}
	s.render(w, "page", data)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	f, err := s.buildForest(ctx)
	if err != nil {
		s.renderError(w, err)
		return
	}
	if len(f.Regions) == 0 {
		s.render(w, "list", listView{})
		return
	}
	view := listView{Region: f.Regions[0]}
	// epic=<id> narrows the pane to a single tile; absent means the whole region.
	if id := r.URL.Query().Get("epic"); id != "" {
		for _, e := range view.Region.Epics {
			if e.ID == id {
				view.Epic, view.HasEpic = e, true
				break
			}
		}
	}
	s.render(w, "list", view)
}

func (s *Server) handleBead(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	issue, err := s.src.Show(ctx, r.PathValue("id"))
	if err != nil {
		s.renderError(w, err)
		return
	}
	s.render(w, "drawer", issue)
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
	// Render into a buffer first so a template failure becomes a clean 500
	// instead of a 200 with a half-written body.
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("strand: render %q: %v", name, err)
		s.renderError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// renderError sends an HTML error fragment with the status mapped from the bd
// error, so htmx and a plain browser both show something legible.
func (s *Server) renderError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(statusForError(err))
	_ = s.tmpl.ExecuteTemplate(w, "error", err.Error())
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
