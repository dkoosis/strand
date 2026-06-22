package bd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Static validation errors, wrapped with the calling op for context. Static so
// callers can errors.Is them and err113 stays satisfied.
var (
	ErrEmptyID      = errors.New("empty id")
	ErrEmptyTitle   = errors.New("empty title")
	ErrEmptyText    = errors.New("empty text")
	ErrUnknownField = errors.New("unknown field")
)

// requireID rejects an empty id; op names the calling method for the error.
func requireID(id, op string) error {
	if id == "" {
		return fmt.Errorf("%s: %w", op, ErrEmptyID)
	}
	return nil
}

// updateFlags maps a logical field name to bd's `update` flag. Callers name the
// field they mean; strand owns the flag spelling so a bd rename is a one-line fix.
// Status writeback is `-s` (O7: there is no `set-state` subcommand in bd).
var updateFlags = map[string]string{
	"status":      "-s",
	"priority":    "-p",
	"assignee":    "-a",
	"title":       "--title",
	"description": "-d",
}

// Update sets one field on an issue. id is always explicit — a bare `bd update`
// mutates the last-touched issue, the footgun this package exists to avoid.
// Returns the updated issue when bd emits one (nil if it stays silent).
func (c *Client) Update(ctx context.Context, id, field, value string) (*Issue, error) {
	if err := requireID(id, "update"); err != nil {
		return nil, err
	}
	flag, ok := updateFlags[field]
	if !ok {
		return nil, fmt.Errorf("update: %w %q", ErrUnknownField, field)
	}
	out, err := c.run(ctx, "update", id, flag, value, "--json")
	if err != nil {
		return nil, err
	}
	return firstIssue(out)
}

// Claim assigns the issue to the current user (bd update <id> --claim).
func (c *Client) Claim(ctx context.Context, id string) (*Issue, error) {
	if err := requireID(id, "claim"); err != nil {
		return nil, err
	}
	out, err := c.run(ctx, "update", id, "--claim", "--json")
	if err != nil {
		return nil, err
	}
	return firstIssue(out)
}

// Close marks an issue done (bd close <id>). reason is optional context recorded
// with the closure; pass "" to omit it. Status changes short of closing go
// through Update(id, "status", …) per O7.
func (c *Client) Close(ctx context.Context, id, reason string) (*Issue, error) {
	if err := requireID(id, "close"); err != nil {
		return nil, err
	}
	args := []string{"close", id}
	if reason != "" {
		args = append(args, "--reason", reason)
	}
	out, err := c.run(ctx, append(args, "--json")...)
	if err != nil {
		return nil, err
	}
	return firstIssue(out)
}

// CreateOpts carries the fields bd create accepts. Title is required; the rest
// are sent only when set, so bd applies its own defaults.
type CreateOpts struct {
	Title       string
	Description string
	Type        string // task | bug | feature | epic
	Priority    *int   // 0–4; nil leaves bd's default
	Assignee    string
}

// Create makes a new issue and returns it.
func (c *Client) Create(ctx context.Context, opts CreateOpts) (*Issue, error) {
	if opts.Title == "" {
		return nil, fmt.Errorf("create: %w", ErrEmptyTitle)
	}
	args := []string{"create", "--title", opts.Title}
	if opts.Description != "" {
		args = append(args, "--description", opts.Description)
	}
	if opts.Type != "" {
		args = append(args, "--type", opts.Type)
	}
	if opts.Priority != nil {
		args = append(args, "--priority", strconv.Itoa(*opts.Priority))
	}
	if opts.Assignee != "" {
		args = append(args, "--assignee", opts.Assignee)
	}
	out, err := c.run(ctx, append(args, "--json")...)
	if err != nil {
		return nil, err
	}
	return firstIssue(out)
}

// Comment adds a comment to an issue (bd comment <id> "text").
func (c *Client) Comment(ctx context.Context, id, text string) error {
	if err := requireID(id, "comment"); err != nil {
		return err
	}
	if text == "" {
		return fmt.Errorf("comment: %w", ErrEmptyText)
	}
	_, err := c.run(ctx, "comment", id, text, "--json")
	return err
}

// DeletePreview runs bd's bare delete (no --force): bd validates the id and
// returns a human-readable preview of what would be removed, deleting nothing.
// strand shows this as the free confirm step before Delete (spec O5).
func (c *Client) DeletePreview(ctx context.Context, id string) (string, error) {
	if err := requireID(id, "delete preview"); err != nil {
		return "", err
	}
	out, err := c.run(ctx, "delete", id)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Delete removes an issue. bd needs --force to delete non-interactively; the UI
// supplies the confirmation step (O5: destructive ops confirm).
func (c *Client) Delete(ctx context.Context, id string) error {
	if err := requireID(id, "delete"); err != nil {
		return err
	}
	_, err := c.run(ctx, "delete", id, "--force")
	return err
}

// firstIssue decodes bd's JSON (an array even for one issue) and returns the
// first record, or nil when bd emitted nothing. bd's error-object form is
// surfaced as an error by decodeIssues.
func firstIssue(out []byte) (*Issue, error) {
	issues, err := decodeIssues(out)
	if err != nil {
		return nil, err
	}
	if len(issues) == 0 {
		// bd succeeded but emitted no issue (e.g. a silent update) — a real
		// "no value, no error" outcome callers handle by nil-checking.
		//nolint:nilnil // the (nil, nil) outcome is the documented contract here.
		return nil, nil
	}
	return &issues[0], nil
}
