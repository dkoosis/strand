# strand-4fj — Web UI exit button (graceful self-shutdown)

Shape: minimal · Profile: craft · Branch: strand-4fj (off main) · Authority: stop-at-green

## Direction

A Quit control in the web UI that stops the server gracefully. strand is a localhost
single-user tool; today you stop it from the terminal. The button POSTs `/shutdown`, the
handler answers with a "stopped" fragment, then raises SIGTERM to its own PID — which flows
through the graceful path already on main (`signal.NotifyContext` → `httpSrv.Shutdown`,
5s drain). No confirm, no auth (dk: local-only trust model).

## Plan

1. **Server seam (`internal/server/server.go`).**
   - Add field `shutdown func()` to `Server`. `New` defaults it to `defaultShutdown` =
     `func() { _ = syscall.Kill(os.Getpid(), syscall.SIGTERM) }`. The field is the test seam —
     `package server` tests set `srv.shutdown` to a stub and assert it fired, without killing
     the test process.
   - Route `POST /shutdown` → `handleShutdown`.
   - `handleShutdown`: `s.render(w, "shutdown", nil)` (200 + stopped fragment), **then**
     `s.shutdown()`. Synchronous is safe: `httpSrv.Shutdown` drains in-flight requests, so the
     response flushes before the server stops. The default `shutdown` only raises a signal
     (non-blocking); the real drain happens in main's existing goroutine.
2. **Pre-existing noctx fix (in-scope, dk-approved).** `cmd/strand/main.go:86`:
   `net.Listen("tcp", *addr)` → `(&net.ListenConfig{}).Listen(ctx, "tcp", *addr)`, reusing the
   `ctx` from `signal.NotifyContext` already in scope. Drops the lone `make audit` finding so
   the gate reaches green. Drop the now-unused `net` import iff unreferenced elsewhere.
3. **Fragment (`web/templates/page.html`** or partials**).** `{{define "shutdown"}}` →
   `<span id="shutdown-status" class="stopped">strand stopped — you can close this tab.</span>`.
4. **Button (footer in `page.html`).** A `Quit` button + the status span it targets:
   `<button class="quit-btn" type="button" hx-post="/shutdown" hx-target="#shutdown-status"
   hx-swap="outerHTML">Quit strand</button><span id="shutdown-status"></span>`.
5. **CSS (`web/static/app.css`).** Minimal: `.quit-btn` (de-emphasized, right-aligned in
   footer) and `.stopped` (muted confirmation).

## Files

| File | Change |
|---|---|
| `internal/server/server.go` | `shutdown` field, `defaultShutdown`, `POST /shutdown` route, `handleShutdown`; imports `os`, `syscall` |
| `cmd/strand/main.go` | `net.Listen` → `ListenConfig.Listen(ctx,…)` (noctx fix) |
| `web/templates/page.html` | `{{define "shutdown"}}` fragment + Quit button in footer |
| `web/static/app.css` | `.quit-btn`, `.stopped` |
| `internal/server/server_test.go` | shutdown-route test (200 + fragment + stub fired once) |

## Tests (requires_test)

`server_test.go`, httptest, `package server`:
- `TestShutdownRoute`: inject `srv.shutdown = func(){ called++ }`; `POST /shutdown` → 200,
  body contains the stopped message, `called == 1`. The stub replaces the real SIGTERM so the
  test process survives.

## Acceptance

`make audit` green (incl. the noctx fix); clicking Quit in the UI stops the server gracefully
and shows the stopped message; `POST /shutdown` returns 200 and triggers exactly one shutdown.

## Don't build

- ✗ confirm dialog · ✗ auth / loopback guard · ✗ restart · ✗ `os.Exit` (use the graceful path).
