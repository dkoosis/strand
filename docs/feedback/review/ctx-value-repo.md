# ctx-value — repo review

RUN_ID: 2af4cc879761
Scope: project (whole repo) · Mode: report

## Result: 0 findings

The codebase contains **no `context.WithValue` and no `ctx.Value(...)` calls**. Context is used only for cancellation/deadline propagation — exactly its intended role.

### Evidence

Inventory queries returned empty:

```
$ rg -n 'context\.WithValue|ctx\.Value\(|\.Value\(' --type go
(no matches)

$ rg -n 'WithValue' --type go
(no matches)

$ rg -n 'ctx\.Value\(\s*"' --type go   # string-key probe
(no matches)
```

Every function that takes a `context.Context` accepts it as the leading parameter and passes its real dependencies explicitly — no service-locator smuggling:

- `internal/bd/client.go:108` — `func (c *Client) run(ctx context.Context, args ...string) ([]byte, error)`
- `internal/bd/write.go:42` — `func (c *Client) Update(ctx context.Context, id, field, value string) (*Issue, error)`
- `internal/server/server.go:219` — `func reqContext(r *http.Request) (context.Context, context.CancelFunc)` (cancellation only)
- `internal/server/server.go:540` — `func (s *Server) graphModel(ctx context.Context, src IssueSource, v *listView) (string, error)`

Dependencies (`*Client`, `IssueSource`, `registry.Repo`) and business identifiers (`id`, `field`, `value`) are real arguments. Nothing is fetched from the context bag.

### Rules checked, none triggered

- ctx-hidden-dependency — no `ctx.Value` reads of deps
- ctx-untyped-key — no string/builtin keys (no keys at all)
- ctx-type-assert-scattered — no context value assertions
- ctx-stacked-chain — no `WithValue` calls
- ctx-value-for-business-id — business IDs passed as params
- ctx-value-with-mutex-or-state — no mutable state in context

Clean for this linter's lens. No action.
