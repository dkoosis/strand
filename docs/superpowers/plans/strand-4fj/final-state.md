# final-state — strand-4fj (Web UI exit button, graceful self-shutdown)

Shape: minimal · Profile: craft · Authority: stop-at-green · Date: 2026-06-22

## Outcome

`make audit` + `make check` green (0 lint, 0 clones, no vulns, nilaway pass, all `-race`
tests pass). Feature complete and reviewed. Branch pushed; **no PR, no `bd close`**
(stop-at-green — dk opens the PR; bd close happens at PR-open).

## What shipped

A `Quit strand` button in the footer. Click → `POST /shutdown` → the handler answers with a
"strand stopped" fragment, then raises SIGTERM at strand's own PID. That flows through the
graceful path already on main (`signal.NotifyContext` → `httpSrv.Shutdown`, 5s drain), so the
response flushes and in-flight requests finish before the listener closes. Local-only tool:
no confirm, no auth (dk's decision).

The shutdown is a **test seam** — `Server.shutdown func()`, defaulting to the real SIGTERM
hook; `package server` tests swap in a stub and assert it fires once without killing the test
process.

| File | Change |
|---|---|
| `internal/server/server.go` | `shutdown` field + `defaultShutdown`, `POST /shutdown` route, `handleShutdown`; imports `os`, `syscall` |
| `cmd/strand/main.go` | noctx fix: `net.Listen` → `(&net.ListenConfig{}).Listen(ctx,…)` (1 line) |
| `web/templates/page.html` | `{{define "shutdown"}}` fragment + Quit button + status span in footer |
| `web/static/app.css` | `.quit-btn`, `.stopped` |
| `internal/server/server_test.go` | `TestShutdownRoute` (200 + fragment + stub fired once) |

## Plan-vs-actual

Fully plan-adherent (R-A: all steps 1–5, Files, Tests, Acceptance, Don't-build verified;
north-star = yes). No scope creep.

**In-scope delta (dk-approved at rehearsal):** the pre-flight `make audit` surfaced one
pre-existing `noctx` finding (`cmd/strand/main.go:86`, on main since the graceful-shutdown
commit). dk chose option (a) — fix it in-scope so the gate reaches green. Limited to the one
line; `net` import kept (still used by `ListenConfig`).

## Review trail

- **R-self:** clean. Verified synchronous `s.shutdown()` is safe under graceful drain;
  `hx-swap="outerHTML"` preserves the `#shutdown-status` id; `lc.Listen(ctx,…)` bounds only the
  listen call, not the listener lifetime.
- **R-A (plan-adherence):** fully adherent, no deviations, north-star yes; `TestShutdownRoute`
  confirmed passing.
- **Triage:** no findings to act on. One note: `syscall.Kill` is POSIX-only — acceptable
  (strand deploys to darwin/linux via launchd; no Windows target).
- **/simplify:** ran as a focused self-pass rather than the 4-agent fan-out — the diff is ~60
  lines and already quality-reviewed by R-self + R-A, and context was at the checkpoint
  warning. Deliberate minimal-shape deviation; nothing duplicated or over-built found.

## north_star_answer

> Does this diff add a graceful UI quit control (button → POST /shutdown → 200 stopped
> fragment → SIGTERM via graceful path), local-only, and nothing more?

**Yes.** Exactly the seam-tested graceful self-shutdown plus the one dk-approved noctx fix;
the Don't-build list (no confirm, no auth, no restart, no `os.Exit`) is fully honored.
