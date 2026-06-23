// Package bd is a thin wrapper over the `bd` (beads) CLI. It shells out and
// parses the JSON that bd emits, so strand never touches the Dolt store directly.
package bd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ErrNotFound means bd has no issue with the requested ID. Callers (e.g. the
// HTTP layer) can map it to a 404 with errors.Is.
var ErrNotFound = errors.New("issue not found")

// ErrInvalidArg means bd rejected an argument (e.g. an unknown --status value).
// Callers can map it to a 400 with errors.Is.
var ErrInvalidArg = errors.New("invalid argument")

// ErrBD wraps any non-zero bd exit or bd-reported error not otherwise classified.
var ErrBD = errors.New("bd command failed")

// classify maps a bd error message to a typed sentinel so the HTTP layer can
// choose the right status code. bd gives us only message text — no codes — so we
// match the stable phrases bd emits ("no issue found", "invalid status …").
// Anything unrecognized is a true bd failure (ErrBD -> 502).
func classify(msg string) error {
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "no issue found"), strings.Contains(low, "no issues found"):
		return fmt.Errorf("%w: %s", ErrNotFound, msg)
	case strings.HasPrefix(low, "invalid "):
		return fmt.Errorf("%w: %s", ErrInvalidArg, msg)
	default:
		return fmt.Errorf("%w: %s", ErrBD, msg)
	}
}

// execMu serializes every bd invocation process-wide. beads' embedded Dolt store
// is a single-writer lock — concurrent bd calls collide and can corrupt or error
// (spec D6/Q5: every bd call goes through one mutex'd helper). One global lock is
// the safest reading: it over-serializes across distinct repos, but strand is a
// single localhost user and that cost is nil next to a corrupted store.
var execMu sync.Mutex

// Issue mirrors the JSON shape bd emits from `list`/`show`. Fields bd omits stay
// at their zero value; extra fields bd adds later are ignored, not an error.
type Issue struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status Status `json:"status"`
	// Priority is 0–4 (0=highest); bd's `list`/`show` JSON always emits it,
	// defaulting to 2, so a plain 0 means P0 unambiguously — there is no
	// absent-vs-zero hazard on the read path (zero-sentinel F1, verified against
	// bd's contract). The write path still uses *int (CreateOpts) where omission
	// is a real option.
	Priority        int       `json:"priority"`
	IssueType       string    `json:"issue_type"`
	Parent          string    `json:"parent,omitempty"`
	Description     string    `json:"description,omitempty"`
	Design          string    `json:"design,omitempty"`
	Assignee        string    `json:"assignee,omitempty"`
	Labels          []string  `json:"labels,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	DependencyCount int       `json:"dependency_count"`
	DependentCount  int       `json:"dependent_count"`
	CommentCount    int       `json:"comment_count"`
}

// DepEdge is one dependency edge: IssueID depends on DependsOnID. Type tells the
// real blocking dependency ("blocks") from epic hierarchy ("parent-child") and
// soft links ("relates_to"); the graph view keeps only "blocks". The direction
// matches bd's "down" sense: the issue depends on (is blocked by) the target.
type DepEdge struct {
	IssueID     string `json:"issue_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
}

// Comment is one note on an issue, as bd emits it from `comments <id> --json`.
type Comment struct {
	ID        string    `json:"id"`
	IssueID   string    `json:"issue_id"`
	Author    string    `json:"author"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// Client runs bd commands against a single workspace directory.
type Client struct {
	// Dir is the working directory bd runs in (resolves the .beads workspace).
	// Empty means the process's current directory.
	Dir string
	// Bin is the bd binary name or path. Empty defaults to "bd" on PATH.
	Bin string
}

func (c *Client) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	return "bd"
}

// run executes bd with args and returns stdout. A non-zero exit becomes an error
// carrying bd's stderr, which is usually a readable hint.
func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	execMu.Lock()
	defer execMu.Unlock()
	//nolint:gosec // G204: bd is an operator-configured binary and args run via exec (no shell), so values like a status filter can't inject commands.
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Dir = c.Dir
	var out bytes.Buffer
	var errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("bd %s: %w", strings.Join(args, " "), classify(msg))
	}
	return out.Bytes(), nil
}

// List returns issues matching the given bd list flags (e.g. "--status", "open").
// With no extra args it lists everything bd's default `list` would show.
func (c *Client) List(ctx context.Context, args ...string) ([]Issue, error) {
	out, err := c.run(ctx, append([]string{"list", "--json"}, args...)...)
	if err != nil {
		return nil, err
	}
	return decodeIssues(out)
}

// Show returns the full record for one issue ID.
func (c *Client) Show(ctx context.Context, id string) (*Issue, error) {
	out, err := c.run(ctx, "show", id, "--json")
	if err != nil {
		return nil, err
	}
	iss, err := firstIssue(out)
	if err != nil {
		return nil, err
	}
	if iss == nil {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return iss, nil
}

// Comments returns an issue's comments, oldest-first, as bd lists them. An issue
// with no comments yields nil (bd emits `[]`), not an error.
func (c *Client) Comments(ctx context.Context, id string) ([]Comment, error) {
	if err := requireID(id, "comments"); err != nil {
		return nil, err
	}
	out, err := c.run(ctx, "comments", id, "--json")
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 || string(trimmed) == "[]" {
		return nil, nil
	}
	var cs []Comment
	if err := json.Unmarshal(trimmed, &cs); err != nil {
		return nil, fmt.Errorf("parse bd comments: %w", err)
	}
	return cs, nil
}

// Deps returns the dependency edges touching the given issue IDs (direction
// "down": what each issue depends on). With no IDs it returns nil — bd needs at
// least one. Callers pass the full ID set to fetch the whole graph and filter by
// Type themselves (the graph view wants "blocks", not "parent-child").
//
// bd's `dep list --json` has two output shapes: with several IDs it emits flat
// edge records ({issue_id, depends_on_id, type}); with exactly one ID it emits
// the dependency *issues* instead. decodeEdges handles both so a one-issue query
// still yields edges.
func (c *Client) Deps(ctx context.Context, ids ...string) ([]DepEdge, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out, err := c.run(ctx, append([]string{"dep", "list", "--json"}, ids...)...)
	if err != nil {
		return nil, err
	}
	return decodeEdges(out, ids)
}

// decodeEdges parses `dep list --json` in either shape. The flat batch shape has
// issue_id/depends_on_id; the single-ID shape returns dependency issues, which we
// turn into edges from the one queried ID. An empty array (no deps) yields nil.
func decodeEdges(out []byte, ids []string) ([]DepEdge, error) {
	trimmed, err := trimForDecode(out)
	if trimmed == nil || err != nil {
		return nil, err
	}
	var rows []struct {
		IssueID        string `json:"issue_id"`
		DependsOnID    string `json:"depends_on_id"`
		Type           string `json:"type"`
		ID             string `json:"id"`              // single-ID shape: the dependency issue
		DependencyType string `json:"dependency_type"` // single-ID shape
	}
	if err := json.Unmarshal(trimmed, &rows); err != nil {
		return nil, fmt.Errorf("parse bd dep list: %w", err)
	}
	edges := make([]DepEdge, 0, len(rows))
	for _, r := range rows {
		switch {
		case r.IssueID != "" && r.DependsOnID != "":
			edges = append(edges, DepEdge{IssueID: r.IssueID, DependsOnID: r.DependsOnID, Type: r.Type})
		case r.ID != "" && len(ids) == 1:
			// Single-ID query: the row is a dependency of the one queried issue.
			edges = append(edges, DepEdge{IssueID: ids[0], DependsOnID: r.ID, Type: r.DependencyType})
		}
	}
	return edges, nil
}

// decodeIssues parses bd's JSON. List-style commands emit an array; `create`
// emits a single bare issue object. bd reports its own errors as a JSON object
// with an "error" key; surface those.
func decodeIssues(out []byte) ([]Issue, error) {
	trimmed, err := trimForDecode(out)
	if trimmed == nil || err != nil {
		return nil, err
	}
	if trimmed[0] == '{' {
		// trimForDecode already ruled out the error object; a non-error object
		// is a single issue (bd create), so wrap it.
		var issue Issue
		if err := json.Unmarshal(trimmed, &issue); err != nil {
			return nil, fmt.Errorf("parse bd output: %w", err)
		}
		return []Issue{issue}, nil
	}
	var issues []Issue
	if err := json.Unmarshal(trimmed, &issues); err != nil {
		return nil, fmt.Errorf("parse bd output: %w", err)
	}
	return issues, nil
}

// trimForDecode trims bd's JSON output and short-circuits the two cases every
// decoder shares: an empty or `[]` response (returns nil, nil) and a bd error
// object {"error": ...} (returns the classified error). Otherwise it returns
// the trimmed bytes for the caller to unmarshal. A nil return with nil error
// means "nothing to decode".
func trimForDecode(out []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[]")) {
		return nil, nil
	}
	if trimmed[0] == '{' {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(trimmed, &e) == nil && e.Error != "" {
			return nil, classify(e.Error)
		}
	}
	return trimmed, nil
}
