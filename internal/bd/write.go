package bd

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Pre-flight validation failures wrap ErrInvalidArg (the package's existing
// invalid-argument sentinel, classified to 400 by the server) with a specific
// message. Wrapping a static error keeps err113 satisfied; the descriptive text
// carries which field was bad, so no per-field sentinel is needed.

// requireID rejects an empty id; op names the calling method for the error.
func requireID(id, op string) error {
	if id == "" {
		return fmt.Errorf("%s: empty id: %w", op, ErrInvalidArg)
	}
	return nil
}

// Logical field names for Update — the keys of updateFlags. strand callers name
// the field through these consts rather than re-spelling the literal, so a typo
// is a compile error and the set of writable fields has one source of truth.
const (
	FieldStatus      = "status"
	FieldPriority    = "priority"
	FieldAssignee    = "assignee"
	FieldTitle       = "title"
	FieldDescription = "description"
)

// updateFlags maps a logical field name to bd's `update` flag. Callers name the
// field they mean; strand owns the flag spelling so a bd rename is a one-line fix.
// Status writeback is `-s` (O7: there is no `set-state` subcommand in bd).
var updateFlags = map[string]string{
	FieldStatus:      "-s",
	FieldPriority:    "-p",
	FieldAssignee:    "-a",
	FieldTitle:       "--title",
	FieldDescription: "-d",
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
		return nil, fmt.Errorf("update: unknown field %q: %w", field, ErrInvalidArg)
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

// SetRank writes the manual-rank order into bd metadata (rank=<float>), the
// store D5 settled on (no sidecar). It bypasses updateFlags because metadata is
// one repeatable `--set-metadata key=value` token, not a single-value flag. The
// float is formatted without exponent so bd round-trips it as a plain number.
// Returns the updated issue when bd emits one (nil if it stays silent — callers
// that need the new order re-read).
func (c *Client) SetRank(ctx context.Context, id string, rank float64) (*Issue, error) {
	if err := requireID(id, "setrank"); err != nil {
		return nil, err
	}
	kv := "rank=" + strconv.FormatFloat(rank, 'f', -1, 64)
	out, err := c.run(ctx, "update", id, "--set-metadata", kv, "--json")
	if err != nil {
		return nil, err
	}
	return firstIssue(out)
}

// SetParent reparents an issue onto a new epic (bd update <id> --parent <parent>).
// It is its own method rather than an updateFlags field because reparenting is a
// deliberate structural move, not a leaf-field edit, and the generic drawer edit
// path must not expose it. parent must be non-empty here — strand only ever
// attaches an orphan story to an epic; clearing a parent is not a strand gesture.
// Returns the updated issue when bd emits one (nil if it stays silent).
func (c *Client) SetParent(ctx context.Context, id, parent ID) (*Issue, error) {
	if err := requireID(string(id), "setparent"); err != nil {
		return nil, err
	}
	if err := requireID(string(parent), "setparent parent"); err != nil {
		return nil, err
	}
	out, err := c.run(ctx, "update", string(id), "--parent", string(parent), "--json")
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
	// Parent is the parent issue id for the strand's parent axis. Empty means the
	// bead is created with no epic (no --parent); the create handler enforces that
	// an empty Parent is a deliberate off-epic choice, never an accidental
	// parentless bead. Typed ID so a caller can't pass a non-id string here.
	Parent ID
}

// Create makes a new issue and returns it. opts is taken by pointer (it grew
// past the by-value lint threshold once Parent was added); callers pass &opts.
func (c *Client) Create(ctx context.Context, opts *CreateOpts) (*Issue, error) {
	if opts.Title == "" {
		return nil, fmt.Errorf("create: empty title: %w", ErrInvalidArg)
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
	if opts.Parent != "" {
		args = append(args, "--parent", string(opts.Parent))
	}
	out, err := c.run(ctx, append(args, "--json")...)
	if err != nil {
		return nil, err
	}
	return firstIssue(out)
}

// DepAdd records that id depends on (is blocked by) dependsOn, the "blocks" edge
// the graph reads. bd's default type is blocks, so no -t is needed. Both ids are
// explicit; bd validates they exist and rejects a self- or duplicate edge.
func (c *Client) DepAdd(ctx context.Context, id, dependsOn ID) error {
	if err := requireID(string(id), "dep add"); err != nil {
		return err
	}
	if err := requireID(string(dependsOn), "dep add target"); err != nil {
		return err
	}
	_, err := c.run(ctx, "dep", "add", string(id), string(dependsOn))
	return err
}

// DepRemove drops the dependency from id to dependsOn (bd dep remove <id> <on>).
func (c *Client) DepRemove(ctx context.Context, id, dependsOn ID) error {
	if err := requireID(string(id), "dep remove"); err != nil {
		return err
	}
	if err := requireID(string(dependsOn), "dep remove target"); err != nil {
		return err
	}
	_, err := c.run(ctx, "dep", "remove", string(id), string(dependsOn))
	return err
}

// LabelAdd attaches label to an issue (bd label add <id> <label>). Labels are not
// a single-value update flag — bd manages them with the add/remove subcommands —
// so they route here, the way SetRank routes metadata outside updateFlags.
// Key-value pairs are plain `key=value` label strings; bd gives them no special
// support, so the view layer encodes/decodes them.
func (c *Client) LabelAdd(ctx context.Context, id, label string) error {
	if err := requireID(id, "label add"); err != nil {
		return err
	}
	if label == "" {
		return fmt.Errorf("label add: empty text: %w", ErrInvalidArg)
	}
	_, err := c.run(ctx, "label", "add", id, label)
	return err
}

// LabelRemove detaches label from an issue (bd label remove <id> <label>).
func (c *Client) LabelRemove(ctx context.Context, id, label string) error {
	if err := requireID(id, "label remove"); err != nil {
		return err
	}
	if label == "" {
		return fmt.Errorf("label remove: empty text: %w", ErrInvalidArg)
	}
	_, err := c.run(ctx, "label", "remove", id, label)
	return err
}

// Comment adds a comment to an issue (bd comment <id> "text").
func (c *Client) Comment(ctx context.Context, id, text string) error {
	if err := requireID(id, "comment"); err != nil {
		return err
	}
	if text == "" {
		return fmt.Errorf("comment: empty text: %w", ErrInvalidArg)
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
