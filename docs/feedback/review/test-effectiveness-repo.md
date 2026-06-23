# test-effectiveness — strand (repo scope)

RUN_ID: 2af4cc879761 · mode: report (no tree changes)

Reviewed every `*_test.go` under `internal/` (the `strand-alg.4/` worktree copy was skipped as a duplicate tree). Eight files, ~1.4k lines of test code.

**Headline: this suite is unusually effective.** Tests fake at the `bd` CLI boundary and the `IssueSource` interface, then assert on observable output — rendered HTML fragments, HTTP status, persisted registry state, re-read values. The honest-write pattern (`TestWriteErrorIsHonest`, `TestAddEmptyCommentIsHonest`) checks that a failed write surfaces bd's error AND keeps the old value, which catches a real class of UI-lies-about-state bugs. Regression pins carry the bug id they guard (`TestNodesNoEdgesTerminates` → strand-d6f, `TestClassifyMapsBDMessages` → the 502). The `reflect.DeepEqual` / whole-struct compares I checked (`graph_test.go:30`, `deps_test.go:23`, `server_test.go:948`) all compare the *entire* output of an aggregator/decoder where every field is the unit's responsibility — correct, not over-broad. No evergreen `NotNil`-after-constructor, no self-compares, no call-count-on-internal mock assertions, no exported-for-testing smell (internal-package tests drive unexported funcs directly, which is the right call).

One thin spot below. It is borderline, not action — reported for completeness, not because the suite needs work.

---

### 1. [F1] `internal/server/server_test.go:887` — test-without-assertion

**Diagnosis.** `TestGraphNoRepo` (and its twin `TestInsightsNoRepo` at line 1116) run the no-repo handler path but assert only the HTTP status code. The doc comment promises more than the test checks:

```go
// TestGraphNoRepo: with no active repo the graph view degrades to the empty pane
// at 200, like Board.
func TestGraphNoRepo(t *testing.T) {
	...
	rec := do(t, srv, "/graph")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /graph (no repo) = %d, want 200", rec.Code)
	}
}
```

**Why.** The comment claims the view "degrades to the empty pane," but nothing asserts the body renders that pane. A regression that returns 200 with a blank body, a half-built graph fragment, or a stray error fragment would pass. The status check still guards the real risk (a no-repo path panicking or 500ing), so this isn't assertion-free — it's assertion-thin. The empty-state *content* is in fact covered next door by `TestEmptyStateWhenNoRepo` (`repo_test.go:85`), which asserts `empty-state` is present and `error-fragment` is absent — so the gap is narrow.

**Evidence.** `internal/server/server_test.go:894-897`:
```go
	rec := do(t, srv, "/graph")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /graph (no repo) = %d, want 200", rec.Code)
	}
```
Same shape at `internal/server/server_test.go:1123-1126` (`TestInsightsNoRepo`).

**Fix.** Add one body assertion matching the comment's claim, e.g. `if !strings.Contains(rec.Body.String(), "empty-state") { ... }` and the negative `error-fragment` check, mirroring `TestEmptyStateWhenNoRepo`. Or drop the prose comment's "empty pane" claim to match what the test actually guards.

**Tier.** borderline

---

## Summary

| Tier | Count |
|------|-------|
| action | 0 |
| borderline | 1 |

No action-tier findings. The test suite is a model of effectiveness-first design; the single borderline note is a comment-vs-assertion mismatch on two no-repo degradation tests, already covered in substance by `TestEmptyStateWhenNoRepo`.
