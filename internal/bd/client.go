// Package bd is a thin wrapper over the `bd` (beads) CLI. It shells out and
// parses the JSON that bd emits, so strand never touches the Dolt store directly.
package bd

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Issue mirrors the JSON shape bd emits from `list`/`show`. Fields bd omits stay
// at their zero value; extra fields bd adds later are ignored, not an error.
type Issue struct {
	ID              string    `json:"id"`
	Title           string    `json:"title"`
	Status          string    `json:"status"`
	Priority        int       `json:"priority"`
	IssueType       string    `json:"issue_type"`
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
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Dir = c.Dir
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("bd %s: %s", strings.Join(args, " "), msg)
	}
	return []byte(out.String()), nil
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

// Ready returns issues with no unmet blockers — the actionable queue.
func (c *Client) Ready(ctx context.Context) ([]Issue, error) {
	out, err := c.run(ctx, "ready", "--json")
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
	issues, err := decodeIssues(out)
	if err != nil {
		return nil, err
	}
	if len(issues) == 0 {
		return nil, fmt.Errorf("no issue %q", id)
	}
	return &issues[0], nil
}

// decodeIssues parses bd's JSON, which is an array even for a single issue.
// bd reports its own errors as a JSON object with an "error" key; surface those.
func decodeIssues(out []byte) ([]Issue, error) {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "[]" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, "{") {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(trimmed), &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("%s", e.Error)
		}
	}
	var issues []Issue
	if err := json.Unmarshal([]byte(trimmed), &issues); err != nil {
		return nil, fmt.Errorf("parse bd output: %w", err)
	}
	return issues, nil
}
