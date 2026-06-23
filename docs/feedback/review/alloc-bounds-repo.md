# alloc-bounds — strand (repo scope)

RUN_ID: 2af4cc879761
Mode: report. Findings: 0 (action=0 borderline=0).

## Verdict

No actionable allocation/fanout-bounds findings. The threat this linter targets —
an allocation or goroutine fanout sized by **external** input with no bound check —
does not appear in strand's source.

## What was checked

**Allocation sites (`make([]T,n)`, `make(map,n)`, `make(chan,n)`).**
Every sizing `n` in non-test code is `len(slice)` of a slice decoded from the `bd`
CLI's JSON output, e.g. `internal/server/server.go:565` `make([]string, len(beads))`,
`internal/bd/client.go:213` `make([]DepEdge, 0, len(rows))`,
`internal/forest/forest.go:88` `make(map[string]bd.Issue, len(issues))`,
`internal/graph/graph.go:141` `make([]string, len(comp))`. None is sized by an HTTP
request field, query param, or any caller-supplied count.

The single upstream source of this data is `Client.run`
(`internal/bd/client.go:108-126`), which execs the `bd` binary and captures stdout.
`bd` is an **operator-configured local CLI** over the operator's own beads repo — the
code documents this at `client.go:111` (`//nolint:gosec // G204: bd is an
operator-configured binary…`). This is the spec's explicit "don't flag" case:
bounds derived from server/operator config, not attacker input. Padding the report
with these internal-bounded sites is forbidden by the linter's Don't list.

**Read-all / scanner bounds.** No `io.ReadAll(r.Body)`, no `ioutil.ReadAll`, no
`bufio.NewScanner` over a request body in non-test code. The only `json.Unmarshal`
calls decode `bd` stdout (`client.go`) and the on-disk registry file
(`internal/registry/registry.go:229`, operator-owned) — neither is request input.
HTTP handlers read only discrete `r.FormValue(...)` strings
(`internal/server/server.go:457,941,1052-1055`; `internal/server/repo.go:52,64`);
`FormValue` triggers Go's default 32 MB `ParseMultipartForm` cap. No request body is
read into a sized buffer.

**Goroutine fanout.** No fanout sized by input. The only `go func()` is the quit
watcher in `cmd/strand/main.go:74` (a single fixed goroutine). No `errgroup`,
`semaphore`, or `for … { go f() }` loop exists.

**Channel buffer / quadratic string build.** The only buffered channel is
`make(chan Metrics, 1)` in `internal/graph/graph_test.go:106` (test, constant size).
No `s += part` accumulation over an externally-bounded loop.

## Self-reflection checklist

- Request/RPC field → allocation size? None found.
- `bd` output external? No — local operator-trusted CLI; matches the internal/config
  exclusion.
- `io.ReadAll(r.Body)` without `MaxBytesReader`? None exist.
- `bufio.Scanner` on external input? None in non-test code.
- Fanout sized by input? No.
- `make(chan T, n)` with input `n`? Only a constant-1 buffer in a test.
- Quadratic string build over external N? None.

Honest zero. Every allocation traces to operator-trusted `bd` output, which the spec
directs not to flag.
