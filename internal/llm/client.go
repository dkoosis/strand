// Package llm is a thin, key-gated client for strand's Tier-2 Suggest: one
// non-streaming Anthropic Messages call to Haiku 4.5. It is deliberately minimal
// — no retry loop, no streaming, no multi-model abstraction, no provider
// indirection (st-suggest.3 SIMPLICITY principle). The caller (st-suggest.3.3)
// assembles the prompt text; this package just sends text and returns text.
package llm

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// model is the server-side Tier-2 model: Haiku 4.5, the cheapest current model
// (claude-haiku-4-5-20251001). Pinned to the dated id so a future default shift
// can't silently re-route Suggest.
const model = anthropic.ModelClaudeHaiku4_5_20251001

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
// the caller silently falls back to Tier-1, with no log and no degraded UX. The
// key stays server-side; it never reaches the browser.
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
func newWithOptions(opts ...option.RequestOption) *Client {
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
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String(), nil
}
