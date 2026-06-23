# domain-vocab — strand (repo scope)

RUN_ID: 2af4cc879761 · mode: report (no tree changes)

Reviewed `internal/`, `cmd/`, `web/`. The codebase is unusually clean for this
linter: **no multi-bool-param functions, no exported bool traps, no inline func
types with ≥2 params/returns left in handler signatures except the one below.**
Status values are already named constants (`statusOpen`/`statusClosed`/…), the
graph metric tuning is typed constants (`pageRankDamp`, `hitsTol`), and each
package keeps a tight, domain-aligned vocabulary (forest→region/epic/treemap,
bd→Issue/Client/Update/Claim/Close, graph→PageRank/Hub/Authority/Depth). The
team already aliases the one-arg seam as `SourceFunc`, showing the pattern is
understood. Two findings, both about the write-path callback contract.

---

### 1. [F1] `internal/server/server.go:992` — inline-func-type-repeated

**Diagnosis.** The write callback contract `func(context.Context, IssueSource) (*bd.Issue, error)` is re-declared inline six times: once as the `write` parameter of `writeAndRefresh` and five times at its call sites.

**Why.** Two params and two returns is the linter's "name it" threshold, and the identical type appears in ≥2 places (here, six). An aliased type makes the signature scannable, lets callers (and future tests/mocks) name the contract, and matches the package's own established habit — `SourceFunc` is already a named alias one screen up (line 80).

**Evidence.** The signature parameter:
```
992:func (s *Server) writeAndRefresh(w http.ResponseWriter, r *http.Request, id string, write func(context.Context, IssueSource) (*bd.Issue, error)) {
```
The same inline type repeated verbatim at every call site:
```
942:	s.writeAndRefresh(w, r, id, func(ctx context.Context, src IssueSource) (*bd.Issue, error) {
951:	s.writeAndRefresh(w, r, id, func(ctx context.Context, src IssueSource) (*bd.Issue, error) {
961:	s.writeAndRefresh(w, r, id, func(ctx context.Context, src IssueSource) (*bd.Issue, error) {
971:	s.writeAndRefresh(w, r, id, func(ctx context.Context, src IssueSource) (*bd.Issue, error) {
1019:	s.writeAndRefresh(w, r, id, func(ctx context.Context, src IssueSource) (*bd.Issue, error) {
```

**Fix.** Alias the contract beside `SourceFunc` and use it in the signature:
```go
// writeFunc runs one bd write and hands back the fresh issue (or nil) plus error.
type writeFunc func(context.Context, IssueSource) (*bd.Issue, error)

func (s *Server) writeAndRefresh(w http.ResponseWriter, r *http.Request, id string, write writeFunc) {
```
The call-site closure literals can stay as-is (Go infers the type) or be hinted; the signature is where the win lands.

**Tier.** action

---

### 2. [F2] `internal/server/server.go:972` — magic-literal-at-call-site

**Diagnosis.** `src.Update(...)` is called with the bare string `"status"` as the field name, while the *value* in the same call is the named constant `statusOpen`. The field key is a domain term with no name.

**Why.** The package already promotes bd status strings to constants (the `status*` block at lines 49–55) precisely so the magic strings don't float at call sites; the field-name key `"status"` is the one bd identifier in this call left as a raw literal. A `fieldStatus` constant (or a small set of `bd` field-name constants) would make the call self-documenting and guard the reopen path — which deliberately routes a status write through `Update` (per the comment at lines 967–969) — against a typo in the field key.

**Evidence.**
```
972:		iss, err := src.Update(ctx, id, "status", statusOpen)
```
Contrast the established constant pattern it sits beside:
```
50:	statusOpen       = "open"
```

**Tier.** borderline

---

## Not flagged (verified clean)

- **`orient(r box) (bool, float64)`** (`internal/forest/squarify.go:75`) — bare bool return, but an unexported one-off helper in a small file with a doc comment naming the bool ("horizontally"). Spec: "Don't flag small one-off helpers."
- **No bool traps anywhere** — `rg 'func.*bool.*bool'` over non-test source returns nothing; no exported single-bool API either.
- **Package vocabularies** are cohesive — no exported identifier reaches outside its package's domain words.
