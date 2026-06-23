package server

import (
	"errors"
	"net/http"

	"github.com/dkoosis/strand/internal/registry"
)

// repoItem is one row in the repo selector: a known repo and whether it is the
// active one (so the menu can mark it).
type repoItem struct {
	registry.Repo
	Active bool
}

// repoMenu is the selector dropdown's data: every known repo (active flagged),
// the active repo's name for the header caption, and an optional error from a
// failed add, shown inline so the user keeps their typed path.
type repoMenu struct {
	Items      []repoItem
	ActiveName string
	Err        string
}

// repoMenu builds the selector view from the registry, flagging the active repo
// and carrying an optional inline error. ActiveName is "—" when no repo is active,
// matching the empty header caption.
func (s *Server) repoMenu(errMsg string) repoMenu {
	active, ok := s.reg.Active()
	repos := s.reg.Repos()
	items := make([]repoItem, len(repos))
	for i, r := range repos {
		items[i] = repoItem{Repo: r, Active: r.Path == active.Path}
	}
	name := "—"
	if ok {
		name = active.Name
	}
	return repoMenu{Items: items, ActiveName: name, Err: errMsg}
}

// handleRepos renders the selector dropdown fragment (the known repos plus the
// add field and rescan control).
func (s *Server) handleRepos(w http.ResponseWriter, _ *http.Request) {
	s.render(w, "repoMenu", s.repoMenu(""))
}

// handleSwitchRepo makes the posted repo active and tells htmx to reload, so
// every view re-scopes to the new repo's beads (spec R1). An unknown path
// re-renders the menu with bd's error rather than scoping to nothing.
func (s *Server) handleSwitchRepo(w http.ResponseWriter, r *http.Request) {
	if _, err := s.reg.Switch(r.FormValue("path")); err != nil {
		// ErrUnknownRepo is the user-correctable case (mistyped/unregistered
		// path); anything else is unexpected persistence/system trouble. Both
		// surface the message inline today — the errors.Is split consumes the
		// sentinel in prod and marks the seam where the two could diverge.
		if errors.Is(err, registry.ErrUnknownRepo) {
			s.render(w, "repoMenu", s.repoMenu(err.Error()))
			return
		}
		s.render(w, "repoMenu", s.repoMenu(err.Error()))
		return
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// handleAddRepo registers an explicitly-typed path (spec O6: scan + add) and
// switches to it, reloading on success. A path with no .beads re-renders the menu
// with the error and the empty state's guidance, never adding a bare directory.
func (s *Server) handleAddRepo(w http.ResponseWriter, r *http.Request) {
	if _, err := s.reg.Add(r.FormValue("path")); err != nil {
		// ErrNoBeads is the user-correctable case (a path with no .beads); other
		// failures (resolve-abs, persistence) are unexpected. Both surface the
		// message inline today — the errors.Is split consumes the sentinel in
		// prod and marks the seam where the two could diverge.
		if errors.Is(err, registry.ErrNoBeads) {
			s.render(w, "repoMenu", s.repoMenu(err.Error()))
			return
		}
		s.render(w, "repoMenu", s.repoMenu(err.Error()))
		return
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}

// handleRescan re-scans ~/Projects for workspaces and re-renders the menu with
// any newly-found repos. The active selection is untouched, so no reload.
func (s *Server) handleRescan(w http.ResponseWriter, _ *http.Request) {
	if err := s.reg.Rescan(registry.ScanRoot()); err != nil {
		s.render(w, "repoMenu", s.repoMenu(err.Error()))
		return
	}
	s.render(w, "repoMenu", s.repoMenu(""))
}
