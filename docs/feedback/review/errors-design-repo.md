# errors-design — strand (RUN_ID 2af4cc879761)

Scope: project (`internal/`, `cmd/`, `web/`). Mode: report.

Overall the error design is strong. The `bd` package defines three sentinels (`ErrNotFound`, `ErrInvalidArg`, `ErrBD`) that `internal/server` branches on via `errors.Is` in `statusForError` to pick 404/400/502 — a clean, honest error API. No noop wraps (`fmt.Errorf("%w", err)`), no missing-arg format strings, no `recover()` sites, no typed-nil-as-interface traps, no log-and-return double-handling. Two borderline findings below.

---

### 1. [F1] `internal/registry/registry.go:21` — sentinel-without-callers

**Diagnosis:** `ErrUnknownRepo` and `ErrNoBeads` are exported sentinels, but no production code branches on them via `errors.Is` — only tests do.

**Why:** An exported sentinel is a public API commitment: "callers may branch on this." Here the only `errors.Is(err, ErrUnknownRepo)` / `errors.Is(err, ErrNoBeads)` sites are in `registry_test.go`. The two production callers (`handleSwitchRepo`, `handleAddRepo` in `repo.go`) take `err.Error()` and render it as a string — they never distinguish the sentinel from any other error. The message already carries the sentinel text plus the offending path via `%w: %s`, so the identity adds nothing the string doesn't. The export is dead weight unless a caller will eventually branch.

**Evidence:**
`internal/registry/registry.go:21,24`:
```go
var ErrUnknownRepo = errors.New("repo not registered")
...
var ErrNoBeads = errors.New("no .beads workspace")
```
Only non-test `errors.Is` consumers are absent — `rg 'errors\.Is.*ErrUnknownRepo|errors\.Is.*ErrNoBeads'` outside `_test.go` returns nothing. Production callers, `internal/server/repo.go:52-54`:
```go
	if _, err := s.reg.Switch(r.FormValue("path")); err != nil {
		s.render(w, "repoMenu", s.repoMenu(err.Error()))
		return
```
and `repo.go:64-65` for `Add`. Both consume the message, not the identity.

**Fix:** Either unexport (`errUnknownRepo`, `errNoBeads`) since they're only matched within the package's own tests, or accept them as documented API if a future caller will branch on a missing-`.beads` path differently from an unknown-repo path. If kept exported, the test coverage is the only thing pinning them — that's a thin justification for a public commitment.

**Tier:** borderline

---

### 2. [F2] `internal/server/server.go:1163` — boundary-leak-to-client

**Diagnosis:** `renderError` writes the full wrapped error chain — including `bd`'s raw stderr and absolute filesystem paths — to the HTTP client.

**Why:** The error reaching `renderError` is the chain `bd <args>: <bd-stderr>` (built in `bd/client.go:123`), and registry errors carry the absolute path (`registry.go:119,140`). `renderStatus` puts `err.Error()` into the HTML fragment; the fallback at line 1163 also writes `err.Error()` via `http.Error`. So bd's command args and any path bd prints land in the browser. The transport boundary should translate internal errors to a user-safe shape rather than forwarding the raw chain.

This is borderline, not action: strand binds `127.0.0.1:7777` by default (`cmd/strand/main.go:28`) — a single-localhost-operator tool where surfacing bd's own message inline is the intended UX (the code comments say so). The disclosure audience is the operator who already owns the box. The rule still applies because the shape forwards internal jargon and paths verbatim with no translation layer, so a non-loopback bind (`--addr`) immediately exposes it.

**Evidence:** `internal/server/server.go:1159-1164`:
```go
func (s *Server) renderError(w http.ResponseWriter, err error) {
	code := statusForError(err)
	if rerr := s.renderStatus(w, "error", err.Error(), code); rerr != nil {
		log.Printf("strand: render error page: %v", rerr)
		http.Error(w, err.Error(), code)
	}
}
```
The forwarded chain is built at `internal/bd/client.go:123`:
```go
		return nil, fmt.Errorf("bd %s: %w", strings.Join(args, " "), classify(msg))
```

**Fix:** Keep logging the full error server-side (already done on the fallback path), but render a translated, status-appropriate message to the client (e.g. map `ErrNotFound`→"issue not found", `ErrBD`→"backend error" + a request/correlation id) instead of `err.Error()`. If the raw-message UX is deliberate for the localhost case, gate the verbose shape behind a "loopback bind only" check so a `--addr 0.0.0.0:…` run doesn't leak.

**Tier:** borderline

---

Defer notes: mechanical wrap idiom (`fmt.Errorf("bd %s: %w", ...)`, `wrapWrite`) is informative, not redundant — the prefix names the command/action the caller couldn't otherwise reconstruct. Per-package caller-action evidence is `error-semantics`' job.
