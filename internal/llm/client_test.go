package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
)

// TestNew_ReportsUnavailable_When_KeyAbsent pins the gate: no ANTHROPIC_API_KEY
// means New reports unavailable with no error and no client — the caller falls
// back to Tier-1 silently (mirrors `bd find-duplicates --ai`).
func TestNew_ReportsUnavailable_When_KeyAbsent(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	c, ok := New()
	if ok {
		t.Errorf("New() ok = true, want false when key absent")
	}
	if c != nil {
		t.Errorf("New() client = %v, want nil when key absent", c)
	}
}

// TestNew_ReportsAvailable_When_KeyPresent pins the other side of the gate: a
// present key yields an available client.
func TestNew_ReportsAvailable_When_KeyPresent(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-123")
	c, ok := New()
	if !ok {
		t.Errorf("New() ok = false, want true when key present")
	}
	if c == nil {
		t.Error("New() client = nil, want non-nil when key present")
	}
}

// TestComplete_SendsSonnetRequestAndReturnsText_When_Called drives the SDK against
// an httptest server (no real network) and asserts: one POST to the Messages
// endpoint, the Sonnet 4.6 model id on the wire, the assembled system + user text
// reach the request body, and the response text maps back to the return value.
func TestComplete_SendsSonnetRequestAndReturnsText_When_Called(t *testing.T) {
	var calls int
	var gotMethod, gotPath string
	var gotRaw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotRaw, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id": "msg_test",
			"type": "message",
			"role": "assistant",
			"model": "claude-sonnet-4-6",
			"content": [{"type": "text", "text": "Grounded title"}],
			"stop_reason": "end_turn",
			"stop_sequence": null,
			"usage": {"input_tokens": 12, "output_tokens": 3}
		}`)
	}))
	defer srv.Close()

	c := newWithOptions(option.WithAPIKey("sk-test-123"), option.WithBaseURL(srv.URL))
	got, err := c.Complete(context.Background(), "system rubric", "the bead text")
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}

	if got != "Grounded title" {
		t.Errorf("Complete() = %q, want %q", got, "Grounded title")
	}
	if calls != 1 {
		t.Errorf("server saw %d calls, want exactly 1 (no streaming, no retry)", calls)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("request method = %q, want POST", gotMethod)
	}
	if !strings.Contains(gotPath, "/messages") {
		t.Errorf("request path = %q, want it to contain /messages", gotPath)
	}

	var body struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(gotRaw, &body); err != nil {
		t.Fatalf("decode request body: %v (raw=%s)", err, gotRaw)
	}
	if body.Model != "claude-sonnet-4-6" {
		t.Errorf("request model = %q, want claude-sonnet-4-6", body.Model)
	}
	if !strings.Contains(string(gotRaw), "the bead text") {
		t.Errorf("request body missing user input; raw=%s", gotRaw)
	}
	if !strings.Contains(string(gotRaw), "system rubric") {
		t.Errorf("request body missing system prompt; raw=%s", gotRaw)
	}
}
