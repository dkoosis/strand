# strand

A human-friendly web UI over [beads](https://github.com/steveyegge/beads) (`bd`).

`bd` is a great issue tracker for agents; strand makes its data pleasant for a
human to browse. strand shells out to the `bd` CLI and serves a small web app —
no direct access to the Dolt store, no schema to keep in sync.

## Run

```sh
go run ./cmd/strand            # serves http://127.0.0.1:7777 over the cwd's .beads
go run ./cmd/strand -dir /path/to/repo -addr :8080
```

Flags:

| flag    | default          | meaning                              |
|---------|------------------|--------------------------------------|
| `-addr` | `127.0.0.1:7777` | listen address                       |
| `-dir`  | cwd              | beads workspace directory            |
| `-bd`   | `bd`             | path to the `bd` binary              |

## API

The front-end is static; everything goes through a small JSON API you can also
hit directly:

- `GET /api/ready` — the actionable queue (no unmet blockers)
- `GET /api/issues` — all issues; `?status=open` to filter
- `GET /api/issues/{id}` — one issue's full record

## Layout

```
cmd/strand/      entrypoint, flags, http server
internal/bd/     thin wrapper over the bd CLI (shell out, parse JSON)
internal/server/ JSON API + static file serving
web/             embedded front-end (index.html, static/)
```

## Build

```sh
make build       # -> ./bin/strand (self-contained, assets embedded)
```
