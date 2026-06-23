# solid — repo review (strand)

RUN_ID: 2af4cc879761 · scope: project · mode: report
git_commit: 163908b

SOLID at type/interface granularity (SRP/LSP/ISP). strand is a small Go web app
that shells out to the `bd` CLI; no DB. The single seam is `IssueSource`
(internal/server), with one production impl (`bd.Client`) and one test fake
(`stubBD`). The tree is well-factored — two findings, both borderline. No LSP
violations (the only non-prod stub is a test double, excluded by rule), no
persistence-on-domain (forest/graph types are pure value types).

---

### 1. [F1] `internal/server/server.go:992` — isp-caller-uses-subset

**Diagnosis.** `IssueSource` is an 11-method interface, but the write handlers
each touch one method of it. `writeAndRefresh` passes the full interface to a
`write` closure that calls a single method:

```
992  func (s *Server) writeAndRefresh(w http.ResponseWriter, r *http.Request, id string, write func(context.Context, IssueSource) (*bd.Issue, error)) {
```

The closures consume only one write each — `handleClaim` calls `src.Claim`,
`handleClose` calls `src.Close`, `handleComment` calls `src.Comment`, `handleEdit`/`handleReopen` call `src.Update`:

```
951  s.writeAndRefresh(w, r, id, func(ctx context.Context, src IssueSource) (*bd.Issue, error) {
952  	iss, err := src.Claim(ctx, id)
```

And the read-only handlers resolve the same fat interface for ≤2 methods —
`handleBead` (`Show`+`Comments`), `handleDeletePreview` (`DeletePreview` only),
`handleDelete` (`Delete` only):

```
1107  if err := src.Delete(ctx, r.PathValue("id")); err != nil {
```

**Why.** Each consumer's signature claims a dependency on reads + every write
when it needs one method. The `write` closure type especially: it advertises the
whole 11-method surface to express "do one bd write." The signature lies about
what the handler depends on (isp-caller-uses-subset).

**Evidence.** server.go:992 (closure takes `IssueSource`); server.go:951-952
(`handleClaim` uses only `Claim`); server.go:1107 (`handleDelete` uses only
`Delete`). Verified against the file at HEAD 163908b.

**Fix.** Low-value to split fully — `src` is resolved once per request from the
active repo and threaded through, so the seam is genuinely one object. The
honest narrowing is the closure type: `writeAndRefresh` only ever needs to run a
write and then `Show`, so its closure could take no source at all (capture `src`
in the handler and pass the already-resolved `src` into a `func(context.Context)
(*bd.Issue, error)`), dropping `IssueSource` from five closure signatures. The
read handlers can keep the full interface — the cost there is cosmetic.

**Tier.** borderline

---

### 2. [F2] `internal/server/server.go:62` — interface-with-one-impl

**Diagnosis.** `IssueSource` has exactly one production implementer
(`bd.Client`, wired in `cmd/strand/main.go:54-56`) and one test fake
(`stubBD`, internal/server/server_test.go:30). The interface carries 11 methods:

```
62  type IssueSource interface {
63  	List(ctx context.Context, args ...string) ([]bd.Issue, error)
...
73  	Delete(ctx context.Context, id string) error
74  }
```

main.go's `srcFor` returns the single concrete type:

```
54  srcFor := func(repo registry.Repo) server.IssueSource {
55  	return &bd.Client{Dir: repo.Path, Bin: bdBin}
56  }
```

**Why.** One prod impl + one test double is the textbook `interface-with-one-impl`
shape, and an 11-method interface is the most expensive form of it — every method
is one a fake must stub (here `stubBD` stubs all 11). The rule's carve-out for
test-mock-demanded interfaces applies *only* when a struct-with-funcs alternative
doesn't fit; that's the open question worth flagging, not an automatic pass.

**Evidence.** server.go:62-74 (the 11-method interface); cmd/strand/main.go:54-56
(sole production impl `&bd.Client`); server_test.go:30 (`type stubBD struct`, the
only other impl). Verified against the files at HEAD 163908b.

**Fix.** Defensible to keep: `SourceFunc func(registry.Repo) IssueSource` needs an
interface so the per-repo source can be a real `bd.Client` in prod and the
in-memory `stubBD` in tests, and a struct-of-funcs over 11 stateful methods would
be clumsier than the interface. So this is a *documented* keep, not a removal —
the worth-it move is trimming the surface (see F1) rather than deleting the seam.
If the read/write split lands, the narrower interfaces also shrink the fake.

**Tier.** borderline

---

_No action-tier findings. LSP: clean (only stub is a test double). SRP:
`bd.Client`'s reads+writes are one cohesive role (shell out to `bd`);
`registry.Registry`'s state+persistence+discovery is a cohesive "known-repos"
role with thin persistence — not flagged._
