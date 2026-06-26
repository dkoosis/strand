# json-shape — strand (project scope)

RUN_ID: 2af4cc879761 · mode: report (no tree changes)

strand's JSON surface is small and mostly disciplined. All decode sites parse
`bd` CLI output (a trusted, forward-compat-by-design boundary the spec exempts
from `DisallowUnknownFields`) or the internal registry file. No money floats
(the only `float64` JSON field is a PageRank score — lossy-acceptable), no
custom `MarshalJSON`/`UnmarshalJSON`, no `any`/`interface{}` decode targets, no
time-format drift, bead IDs are strings (no int-ID precision risk). The
omitempty fields all carry honest absent==zero semantics. Two borderline items
below; neither is action-grade.

---

### 1. [F1] `internal/bd/client.go:64` — time-format-drift

**Diagnosis.** `Issue.CreatedAt`/`UpdatedAt` and `Comment.CreatedAt` decode bd's
timestamps into bare `time.Time`, whose JSON unmarshaler accepts only RFC3339.
If bd ever emits a non-RFC3339 layout (unix seconds, a custom layout), the
unmarshal fails and takes the **entire** issue/list decode down with it, not
just the one field.

**Why.** `time.Time.UnmarshalJSON` is strict-RFC3339. A wire-format change on
bd's side (which strand treats as forward-compat elsewhere — comment line 52:
"extra fields bd adds later are ignored, not an error") would turn a tolerated
schema drift into a hard parse error for every record in the batch. The
tolerance the rest of the struct is built for does not extend to the time
fields.

**Evidence.** `internal/bd/client.go:64-65`:

```go
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
```

and `internal/bd/client.go:87`:

```go
	CreatedAt time.Time `json:"created_at"`
```

**Fix.** Leave as-is if bd's RFC3339 output is contractually stable (it likely
is). If you want the same drift-tolerance the rest of the struct has, decode
into a wrapper type whose `UnmarshalJSON` tries RFC3339 then falls back (or
records the raw string on failure) so one timestamp quirk can't void the whole
batch.

**Tier:** borderline

---

### 2. [F2] `internal/bd/client.go:53` — omitempty-loses-meaningful-zero

**Diagnosis.** `Issue` is an *input* struct (decode target for bd output), yet it
carries `omitempty` on `Parent`/`Description`/`Design`/`Assignee`/`Labels`. On
decode omitempty is inert. But this struct has no separate output type — if it
is ever re-encoded (debug dump, cache, API echo), omitempty makes an explicitly
cleared `Assignee` ("") indistinguishable from "never set," and an empty
`Labels` slice indistinguishable from "labels removed." For these string/slice
fields the distinction rarely bites; flagging mainly to note the tags are
decoration on a decode-only struct, not a guard.

**Why.** Mixing `omitempty` semantics onto a decode struct invites a later
re-encode to silently drop fields a reader might need (cleared-vs-absent). Today
nothing re-encodes `Issue`, so the risk is latent, not live.

**Evidence.** `internal/bd/client.go:59-63`:

```go
	Parent          string    `json:"parent,omitempty"`
	Description     string    `json:"description,omitempty"`
	Design          string    `json:"design,omitempty"`
	Assignee        string    `json:"assignee,omitempty"`
	Labels          []string  `json:"labels,omitempty"`
```

**Fix.** None needed while `Issue` is decode-only. If an output path appears,
split a wire-output type without omitempty (or use `*string`) for any field
where cleared-vs-absent must survive the round trip — `Assignee` is the most
likely to matter.

**Tier:** borderline
