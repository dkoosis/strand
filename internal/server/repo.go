package server

import (
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

// repoMenu builds the selector view from the registry, flagging the repo this
// request is SCOPED to (a `?repo=` deep-link, else the persistent active repo)
// and carrying an optional inline error. ActiveName is "—" when nothing
// resolves, matching the empty header caption.
//
// Scoped, not active: st-ga4 made `?repo=` a pure per-request override for the
// body of the page but left this chrome reading registry.Active(), so a status
// line click rendered the right repo's beads under the wrong repo's name. The
// caption is how a human answers "what am I looking at" — it has to track the
// same repo source(r) resolved.
func (s *Server) repoMenu(scoped registry.Repo, ok bool, errMsg string) repoMenu {
	repos := s.reg.Repos()
	items := make([]repoItem, len(repos))
	for i, r := range repos {
		items[i] = repoItem{Repo: r, Active: ok && r.Path == scoped.Path}
	}
	name := "—"
	if ok {
		name = scoped.Name
	}
	return repoMenu{Items: items, ActiveName: name, Err: errMsg}
}

// scopedMenu is repoMenu for a request that has not already resolved its repo:
// it re-runs the same read-only Resolve source(r) uses, so the dropdown a
// deep-linked page opens marks the deep-link's repo, not the registry default.
// FormValue, not URL.Query: htmx sends the page's inherited hx-vals as query
// params on a GET and as form fields on a POST, and both shapes reach this.
func (s *Server) scopedMenu(r *http.Request, errMsg string) repoMenu {
	scoped, ok := s.reg.Resolve(r.FormValue("repo"))
	return s.repoMenu(scoped, ok, errMsg)
}

// handleRepos renders the selector dropdown fragment (the known repos plus the
// add field and rescan control).
func (s *Server) handleRepos(w http.ResponseWriter, r *http.Request) {
	s.render(w, "repoMenu", s.scopedMenu(r, ""))
}

// handleSwitchRepo makes the posted repo active and tells htmx to reload, so
// every view re-scopes to the new repo's beads (spec R1). An unknown path
// re-renders the menu with bd's error rather than scoping to nothing.
func (s *Server) handleSwitchRepo(w http.ResponseWriter, r *http.Request) {
	s.activateRepo(w, r, s.reg.Switch)
}

// handleAddRepo registers an explicitly-typed path (spec O6: scan + add) and
// switches to it, reloading on success. A path with no .beads re-renders the menu
// with the error and the empty state's guidance, never adding a bare directory.
func (s *Server) handleAddRepo(w http.ResponseWriter, r *http.Request) {
	s.activateRepo(w, r, s.reg.Add)
}

// activateRepo runs a registry mutation that makes the posted path the active repo
// (Switch or Add) and, on success, sends the browser to the UNSCOPED landing so
// every view re-scopes. Any failure — a mistyped/unregistered path, a path with
// no .beads, or unexpected resolve-abs/persistence trouble — surfaces its message
// inline so the user keeps their typed path, never scoping to nothing or adding a
// bare directory. Switch and Add differ only in the registry call, so both
// handlers share this shape.
//
// Redirect, not HX-Refresh: a reload replays the CURRENT url, and a page opened
// from a status-line deep-link still carries `?repo=<old>` — the per-request
// override (st-ga4) would then win over the repo just switched to, so picking a
// repo from the dropdown appeared to do nothing. Dropping the query string makes
// the new active repo the one that renders.
func (s *Server) activateRepo(w http.ResponseWriter, r *http.Request, activate func(string) (registry.Repo, error)) {
	if _, err := activate(r.FormValue("path")); err != nil {
		s.render(w, "repoMenu", s.scopedMenu(r, err.Error()))
		return
	}
	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusNoContent)
}

// handleRescan re-scans ~/Projects for workspaces and re-renders the menu with
// any newly-found repos. The active selection is untouched, so no reload.
func (s *Server) handleRescan(w http.ResponseWriter, r *http.Request) {
	if err := s.reg.Rescan(registry.ScanRoot()); err != nil {
		s.render(w, "repoMenu", s.scopedMenu(r, err.Error()))
		return
	}
	s.render(w, "repoMenu", s.scopedMenu(r, ""))
}
