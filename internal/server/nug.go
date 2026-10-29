package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
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
	if r.PathValue("id") == "" && !sameSite(r) {
		// A ?path= answer reads the disk (is there a file, which id); a foreign
		// page must not learn that from the status.
		s.renderError(w, errCrossSite)
		return
	}
	path, err := s.resolveNug(ctx, r)
	if errors.Is(err, bd.ErrNotFound) {
		subject := r.PathValue("id")
		if subject == "" {
			subject = r.URL.Query().Get("path")
		}
		renderNugNotFound(w, subject)
		return
	}
	if err != nil {
		s.renderError(w, err)
		return
	}
	link := obsidianURL(registeredPath(path, s.nugVaults()))
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

// nugNotFoundPage answers a link no nug answers to: a page, never a dead
// obsidian:// redirect.
var nugNotFoundPage = template.Must(template.New("nugNotFound").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>Nug not found</title></head>
<body style="font-family:system-ui,sans-serif;margin:2rem">
<p>Nug not found: {{.}}</p>
</body></html>
`))

func renderNugNotFound(w http.ResponseWriter, subject string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	if rerr := nugNotFoundPage.Execute(w, subject); rerr != nil {
		log.Printf("strand: render nug not-found page: %v", rerr)
	}
}

// resolveNug turns the request into the note's absolute path: the {id} path value
// through mnemd, or the ?path= query as a hint for the nug it names.
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
	return s.resolveNugPath(ctx, filepath.Clean(path))
}

// resolveNugPath finds where the nug a ?path= link names lives now. A link's
// path goes stale (mnemd move) and may differ in case from the vault, so the
// path is only a hint: the file's own id, else the id mnemd moved away from
// that path, names the nug, and mnemd says where it is. A path no nug answers to is bd.ErrNotFound.
func (s *Server) resolveNugPath(ctx context.Context, path string) (string, error) {
	if id := frontmatterID(path); id != "" {
		if cur, err := s.nugPath(ctx, id); err == nil {
			return cur, nil
		}
		return path, nil // mnemd cannot say; the file itself is there
	}
	if fileExists(path) {
		return path, nil
	}
	root, ok := vaultRoot(path, s.nugVaults())
	if !ok {
		root, ok = vaultRoot(path, []string{os.Getenv("MNEMD_NUGBASE")})
	}
	if ok {
		rel := strings.TrimPrefix(path[len(root):], "/")
		if id := movedTo(filepath.Join(root, ".mnemd", "mnemd-write-ledger.jsonl"), rel); id != "" {
			cur, err := s.nugPath(ctx, id)
			if err == nil || !errors.Is(err, bd.ErrNotFound) {
				return cur, err // a failing mnemd is a 502, not "not found"
			}
		}
	}
	return "", fmt.Errorf("%w: no nug at %s", bd.ErrNotFound, path)
}

func fileExists(path string) bool {
	fi, err := os.Stat(path) //nolint:gosec // a stat of the link's own path reads nothing
	return err == nil && !fi.IsDir()
}

// frontmatterID reads the id: line of a note's frontmatter, or "" when the file
// is unreadable, has no frontmatter or carries no valid id.
func frontmatterID(path string) string {
	f, err := os.Open(path) //nolint:gosec // path is the link's own, read-only, first 4 KB
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	head, _ := io.ReadAll(io.LimitReader(f, 4096))
	lines := strings.Split(string(head), "\n")
	if strings.TrimRight(lines[0], "\r") != "---" {
		return ""
	}
	for _, line := range lines[1:] {
		line = strings.TrimRight(line, "\r")
		if line == "---" {
			return ""
		}
		if v, ok := strings.CutPrefix(line, "id:"); ok {
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			if nugIDPattern.MatchString(v) {
				return v
			}
			return ""
		}
	}
	return ""
}

// vaultRoot is the registered vault that holds path, matched without regard to
// case and spelled as registered; the longest wins.
func vaultRoot(path string, vaults []string) (string, bool) {
	best := ""
	for _, v := range vaults {
		v = strings.TrimRight(v, "/")
		if v == "" || len(path) < len(v) || len(v) < len(best) {
			continue
		}
		if !strings.EqualFold(path[:len(v)], v) || (len(path) > len(v) && path[len(v)] != '/') {
			continue
		}
		best = v
	}
	return best, best != ""
}

// registeredPath rewrites path onto the vault as Obsidian registers it: Obsidian
// matches the vault path case-sensitively, and a link may spell it otherwise.
func registeredPath(path string, vaults []string) string {
	root, ok := vaultRoot(path, vaults)
	if !ok {
		return path
	}
	return root + path[len(root):]
}

// movedTo reads mnemd's write ledger for the nug that most recently left the
// vault-relative path rel by a move, and returns its id, "" when none did. A
// later nug that took the path and left it is the likelier target of a link
// made while it lived there; the id, not the path, is what mnemd then resolves.
func movedTo(ledger, rel string) string {
	b, err := os.ReadFile(ledger) //nolint:gosec // the ledger inside a registered vault
	if err != nil {
		return ""
	}
	id := ""
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"from"`)) {
			continue
		}
		var e struct {
			Verb string `json:"verb"`
			ID   string `json:"id"`
			From string `json:"from"`
		}
		if json.Unmarshal(line, &e) == nil && e.Verb == "move" && strings.EqualFold(e.From, rel) {
			id = e.ID
		}
	}
	return id
}

// defaultNugVaults lists the vault paths as Obsidian registers them.
func defaultNugVaults() []string {
	var vaults []string
	if home, err := os.UserHomeDir(); err == nil {
		b, err := os.ReadFile(filepath.Join(home, "Library", "Application Support", "obsidian", "obsidian.json"))
		var cfg struct {
			Vaults map[string]struct {
				Path string `json:"path"`
			} `json:"vaults"`
		}
		if err == nil && json.Unmarshal(b, &cfg) == nil {
			for _, v := range cfg.Vaults {
				vaults = append(vaults, v.Path)
			}
		}
	}
	return vaults
}

// defaultNugPath asks mnemd for a nug's path: `mnemd --json recall --id <id>`.
// An id mnemd does not know is a 404; a missing or failing mnemd is a 502.
func defaultNugPath(ctx context.Context, id string) (string, error) {
	bin, ok := binPath("mnemd")
	if !ok {
		return "", fmt.Errorf("%w: mnemd binary not found", bd.ErrBD)
	}
	out, runErr := exec.CommandContext(ctx, bin, "--json", "recall", "--id", id).Output() //nolint:gosec // id passed nugIDPattern (lowercase hex)
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
