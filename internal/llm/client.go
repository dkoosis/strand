// Package llm is a thin, key-gated client for strand's drawer assist: one
// non-streaming Anthropic Messages call to Sonnet 4.6. It is deliberately minimal
// — no retry loop, no streaming, no multi-model abstraction, no provider
// indirection (st-suggest.3 SIMPLICITY principle). The caller assembles the prompt
// text; this package just sends text and returns text.
package llm

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// model is the server-side assist model: Sonnet 4.6 (claude-sonnet-4-6). Shaping a
// bead is a judgment task, so assist runs on a mid-tier model, not the Haiku floor.
// The id is already version-pinned (4.6, not a floating "latest"), so a default
// shift can't silently re-route Suggest.
const model = anthropic.ModelClaudeSonnet4_6

// maxTokens caps one Suggest response. A title plus a short body fits well under
// this; it is a ceiling, not a target.
const maxTokens int64 = 2048

// Client wraps the Anthropic SDK for a single Messages call. It is constructed
// only when an API key is present (see New) — there is no "unavailable" Client
// instance; absence is signalled by New's bool.
type Client struct {
	client anthropic.Client
	model  anthropic.Model
}

// New builds a Client from ANTHROPIC_API_KEY in the environment. The bool reports
// availability, mirroring the `bd find-duplicates --ai` gate: key present -> a
// usable client and true; key absent -> nil and false. Absence is NOT an error —
// the caller renders no assist affordance at all. The key stays server-side; it
// never reaches the browser.
func New() (*Client, bool) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		return nil, false
	}
	// NewClient reads ANTHROPIC_API_KEY itself; passing it explicitly keeps the
	// gate and the construction reading from one source.
	return newWithOptions(), true
}

// newWithOptions constructs a Client with arbitrary SDK request options. Tests
// inject option.WithBaseURL / option.WithAPIKey to hit an httptest server instead
// of the live API; New calls it with no options so the SDK reads the key from the
// environment.
//
// WithMaxRetries(0) is prepended so the one-call/no-retry contract holds: the SDK
// retries twice by default on 429/5xx/transient errors, which would let Complete
// issue up to three Messages calls. A best-effort assist fails fast and says so
// instead. It is prepended (not appended) so a caller option can still override it.
func newWithOptions(opts ...option.RequestOption) *Client {
	opts = append([]option.RequestOption{option.WithMaxRetries(0)}, opts...)
	return &Client{
		client: anthropic.NewClient(opts...),
		model:  model,
	}
}

// Complete sends one non-streaming Messages request to Haiku and returns the
// assembled text of the reply. systemOrPrompt becomes the system prompt (omitted
// when empty so the API doesn't see a blank block); userInput is the single user
// turn. Both are pre-assembled by the caller — this method adds no prompt shaping.
func (c *Client) Complete(ctx context.Context, systemOrPrompt, userInput string) (string, error) {
	params := anthropic.MessageNewParams{
		Model:     c.model,
		MaxTokens: maxTokens,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(userInput)),
		},
	}
	if systemOrPrompt != "" {
		params.System = []anthropic.TextBlockParam{{Text: systemOrPrompt}}
	}

	resp, err := c.client.Messages.New(ctx, params)
	if err != nil {
		return "", fmt.Errorf("llm: messages call: %w", err)
	}

	var b strings.Builder
	for i := range resp.Content {
		if t, ok := resp.Content[i].AsAny().(anthropic.TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String(), nil
}
