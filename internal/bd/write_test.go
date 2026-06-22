package bd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeBD writes an executable shell script standing in for the bd binary and
// returns a Client pointed at it plus the path of the file it logs args to.
// body is the script's case body (it may emit JSON on stdout); every call also
// appends its full arg list as one line to the log.
func fakeBD(t *testing.T, body string) (*Client, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "args.log")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> '" + log + "'\n" +
		body + "\n"
	path := filepath.Join(dir, "bd")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	return &Client{Bin: path}, log
}

func readLog(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestUpdatePassesExplicitIDAndFlag(t *testing.T) {
	c, log := fakeBD(t, `echo '[{"id":"x-1","status":"closed"}]'`)
	got, err := c.Update(context.Background(), "x-1", "status", "closed")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got == nil || got.ID != "x-1" || got.Status != "closed" {
		t.Fatalf("parsed issue = %+v, want id x-1 status closed", got)
	}
	line := readLog(t, log)[0]
	for _, want := range []string{"update", "x-1", "-s", "closed", "--json"} {
		if !strings.Contains(line, want) {
			t.Errorf("args %q missing %q", line, want)
		}
	}
}

func TestUpdateUnknownFieldRejected(t *testing.T) {
	c, _ := fakeBD(t, `echo '[]'`)
	if _, err := c.Update(context.Background(), "x-1", "bogus", "v"); err == nil {
		t.Fatal("want error for unknown field, got nil")
	}
}

func TestUpdateEmptyIDRejected(t *testing.T) {
	c, _ := fakeBD(t, `echo '[]'`)
	if _, err := c.Update(context.Background(), "", "status", "open"); err == nil {
		t.Fatal("want error for empty id, got nil")
	}
}

func TestClaimUsesClaimFlag(t *testing.T) {
	c, log := fakeBD(t, `echo '[{"id":"x-2"}]'`)
	if _, err := c.Claim(context.Background(), "x-2"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	line := readLog(t, log)[0]
	for _, want := range []string{"update", "x-2", "--claim"} {
		if !strings.Contains(line, want) {
			t.Errorf("args %q missing %q", line, want)
		}
	}
}

func TestCloseWithReason(t *testing.T) {
	c, log := fakeBD(t, `echo '[{"id":"x-3","status":"closed"}]'`)
	if _, err := c.Close(context.Background(), "x-3", "done & dusted"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	line := readLog(t, log)[0]
	for _, want := range []string{"close", "x-3", "--reason", "done & dusted"} {
		if !strings.Contains(line, want) {
			t.Errorf("args %q missing %q", line, want)
		}
	}
}

func TestCloseWithoutReasonOmitsFlag(t *testing.T) {
	c, log := fakeBD(t, `echo '[{"id":"x-3"}]'`)
	if _, err := c.Close(context.Background(), "x-3", ""); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if line := readLog(t, log)[0]; strings.Contains(line, "--reason") {
		t.Errorf("args %q should not contain --reason", line)
	}
}

// bd create emits a single bare issue object, not the array shape used by
// update/close. Create must decode it; otherwise every successful create is
// reported as a parse failure after the issue already exists.
func TestCreateSendsSetFieldsOnly(t *testing.T) {
	c, log := fakeBD(t, `echo '{"id":"x-9","title":"hi","status":"open"}'`)
	p := 1
	got, err := c.Create(context.Background(), CreateOpts{Title: "hi", Type: "task", Priority: &p})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got == nil || got.ID != "x-9" {
		t.Fatalf("parsed issue = %+v, want id x-9", got)
	}
	line := readLog(t, log)[0]
	for _, want := range []string{"create", "--title", "hi", "--type", "task", "--priority", "1"} {
		if !strings.Contains(line, want) {
			t.Errorf("args %q missing %q", line, want)
		}
	}
	if strings.Contains(line, "--description") || strings.Contains(line, "--assignee") {
		t.Errorf("args %q sent an unset field", line)
	}
}

func TestCreateEmptyTitleRejected(t *testing.T) {
	c, _ := fakeBD(t, `echo '[]'`)
	if _, err := c.Create(context.Background(), CreateOpts{}); err == nil {
		t.Fatal("want error for empty title, got nil")
	}
}

func TestDeleteUsesForce(t *testing.T) {
	c, log := fakeBD(t, ``)
	if err := c.Delete(context.Background(), "x-4"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	line := readLog(t, log)[0]
	for _, want := range []string{"delete", "x-4", "--force"} {
		if !strings.Contains(line, want) {
			t.Errorf("args %q missing %q", line, want)
		}
	}
}

func TestCommentPassesText(t *testing.T) {
	c, log := fakeBD(t, `echo '[]'`)
	if err := c.Comment(context.Background(), "x-5", "looks good"); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if line := readLog(t, log)[0]; !strings.Contains(line, "comment") || !strings.Contains(line, "x-5") || !strings.Contains(line, "looks good") {
		t.Errorf("args %q missing comment/id/text", line)
	}
}

func TestCommentEmptyTextRejected(t *testing.T) {
	c, _ := fakeBD(t, `echo '[]'`)
	if err := c.Comment(context.Background(), "x-5", ""); err == nil {
		t.Fatal("want error for empty text, got nil")
	}
}

func TestCommentsReadsThread(t *testing.T) {
	c, log := fakeBD(t, `echo '[{"id":"c1","issue_id":"x-5","author":"ada","text":"hi"}]'`)
	cs, err := c.Comments(context.Background(), "x-5")
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if len(cs) != 1 || cs[0].Author != "ada" || cs[0].Text != "hi" {
		t.Fatalf("parsed comments = %+v, want one from ada", cs)
	}
	if line := readLog(t, log)[0]; !strings.Contains(line, "comments") || !strings.Contains(line, "x-5") || !strings.Contains(line, "--json") {
		t.Errorf("args %q missing comments/id/--json", line)
	}
}

func TestCommentsEmptyIsNotError(t *testing.T) {
	c, _ := fakeBD(t, `echo '[]'`)
	cs, err := c.Comments(context.Background(), "x-5")
	if err != nil || cs != nil {
		t.Fatalf("Comments empty = (%v, %v), want (nil, nil)", cs, err)
	}
}

// DeletePreview runs bd's bare delete (no --force) and returns its text.
func TestDeletePreviewIsBareAndDestroysNothing(t *testing.T) {
	c, log := fakeBD(t, `echo 'DELETE PREVIEW: x-4'`)
	out, err := c.DeletePreview(context.Background(), "x-4")
	if err != nil {
		t.Fatalf("DeletePreview: %v", err)
	}
	if !strings.Contains(out, "DELETE PREVIEW") {
		t.Errorf("preview text = %q, want bd's preview", out)
	}
	line := readLog(t, log)[0]
	if !strings.Contains(line, "delete") || !strings.Contains(line, "x-4") {
		t.Errorf("args %q missing delete/id", line)
	}
	if strings.Contains(line, "--force") {
		t.Errorf("preview must not pass --force, got %q", line)
	}
}

// bd's error-object output (a JSON {"error":...}) must surface as a Go error,
// not be swallowed as a successful no-op.
func TestWriteSurfacesBdError(t *testing.T) {
	c, _ := fakeBD(t, `echo '{"error":"no such issue"}'`)
	_, err := c.Update(context.Background(), "x-1", "status", "open")
	if err == nil || !strings.Contains(err.Error(), "no such issue") {
		t.Fatalf("err = %v, want it to carry bd's message", err)
	}
}

// TestExecSerialized proves D6: concurrent calls never overlap. The fake brackets
// each invocation with start/end markers around a sleep; with the mutex the log
// is perfectly paired (start,end,start,end,…). An interleave would show two
// starts in a row.
func TestExecSerialized(t *testing.T) {
	body := "echo start >> '%LOG%'\nsleep 0.01\necho end >> '%LOG%'\necho '[]'"
	c, log := fakeBD(t, "")
	// rewrite the script to use start/end bracketing around the same log file
	script := "#!/bin/sh\n" + strings.ReplaceAll(body, "%LOG%", log) + "\n"
	if err := os.WriteFile(c.Bin, []byte(script), 0o755); err != nil {
		t.Fatalf("rewrite fake bd: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			_, _ = c.Update(context.Background(), "x-1", "status", "open")
		})
	}
	wg.Wait()

	lines := readLog(t, log)
	if len(lines) != 2*n {
		t.Fatalf("got %d markers, want %d", len(lines), 2*n)
	}
	for i, want := range cyclePairs(n) {
		if lines[i] != want {
			t.Fatalf("marker %d = %q, want %q (calls interleaved — mutex not holding)", i, lines[i], want)
		}
	}
}

func cyclePairs(n int) []string {
	out := make([]string, 0, 2*n)
	for range n {
		out = append(out, "start", "end")
	}
	return out
}
