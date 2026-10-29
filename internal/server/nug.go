package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dkoosis/strand/internal/bd"
)

// A nug link opens in Obsidian from any surface that opens http (st-e9s). Claude
// Code's console and pane open only http links on click, so an obsidian:// link
// is dead there; GET /nug/<id> (or /nug?path=<abs>) is the http stand-in. strand
// runs `open` on the obsidian:// URL and answers a page that closes its own tab.
// A cross-site request gets the same page with the link, but nothing opens: a
// foreign page must not be able to launch apps on dk's machine.

// nugIDPattern is a mnemd nug id: lowercase hex (recall prints 12 chars). The
// range leaves room for a longer id without admitting arbitrary argv.
var nugIDPattern = regexp.MustCompile(`^[0-9a-f]{6,64}$`)

// obsidianURL is the obsidian:// URL that opens the note at an absolute path.
// A space is %20, not QueryEscape's "+", which Obsidian would read literally.
func obsidianURL(path string) string {
	return "obsidian://open?path=" + strings.ReplaceAll(url.QueryEscape(path), "+", "%20")
}

// nugPage is the answer to every /nug request: it closes its own tab when the
// note opened, and otherwise leaves a link dk can click.
var nugPage = template.Must(template.New("nug").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>{{.Title}}</title></head>
<body style="font-family:system-ui,sans-serif;margin:2rem">
<p>{{if .Opened}}Opened in Obsidian.{{else}}Nothing opened.{{end}} <a href="{{.Link}}">Open {{.Title}} in Obsidian</a></p>
{{if .Opened}}<script>window.close()</script>{{end}}
</body></html>
`))

type nugPageData struct {
	Title  string
	Link   template.URL // obsidian:// is not a scheme html/template trusts on its own
	Opened bool
}

// handleNug serves GET /nug/{id} and GET /nug?path=<abs>.
func (s *Server) handleNug(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	path, err := s.resolveNug(ctx, r)
	if err != nil {
		s.renderError(w, err)
		return
	}
	link := obsidianURL(path)
	opened := false
	if sameSite(r) {
		if err := s.openURL(ctx, link); err != nil {
			log.Printf("strand: open %s: %v", link, err)
		} else {
			opened = true
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := nugPageData{Title: strings.TrimSuffix(filepath.Base(path), ".md"), Link: template.URL(link), Opened: opened} //nolint:gosec // link is obsidianURL's own construction over an escaped path
	if err := nugPage.Execute(w, data); err != nil {
		log.Printf("strand: render nug page: %v", err)
	}
}

// resolveNug turns the request into the note's absolute path: the {id} path value
// through mnemd, or the ?path= query as given.
func (s *Server) resolveNug(ctx context.Context, r *http.Request) (string, error) {
	if id := r.PathValue("id"); id != "" {
		if !nugIDPattern.MatchString(id) {
			return "", fmt.Errorf("%w: not a nug id: %q", bd.ErrInvalidArg, id)
		}
		return s.nugPath(ctx, id)
	}
	path := r.URL.Query().Get("path")
	if path == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: path must be absolute: %q", bd.ErrInvalidArg, path)
	}
	return filepath.Clean(path), nil
}

// defaultNugPath asks mnemd for a nug's path: `mnemd --json recall --id <id>`.
// An id mnemd does not know is a 404; a missing or failing mnemd is a 502.
func defaultNugPath(ctx context.Context, id string) (string, error) {
	bin, ok := binPath("mnemd")
	if !ok {
		return "", fmt.Errorf("%w: mnemd binary not found", bd.ErrBD)
	}
	out, runErr := exec.CommandContext(ctx, bin, "--json", "recall", "--id", id).Output()
	var res struct {
		Path  string `json:"path"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return "", fmt.Errorf("%w: mnemd recall %s: %w", bd.ErrBD, id, errors.Join(runErr, err))
	}
	switch {
	case strings.Contains(res.Error, "no nug has id"):
		return "", fmt.Errorf("%w: no nug has id %q", bd.ErrNotFound, id)
	case res.Error != "" || res.Path == "":
		return "", fmt.Errorf("%w: mnemd recall %s: %s", bd.ErrBD, id, res.Error)
	}
	return res.Path, nil
}

// defaultOpenURL hands a URL to macOS `open`, which routes obsidian:// to
// Obsidian.
func defaultOpenURL(ctx context.Context, link string) error {
	out, err := exec.CommandContext(ctx, "open", link).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}
