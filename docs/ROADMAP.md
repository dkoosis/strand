# strand

★ strand renders synthesis, not the catalog — where the mass, bugs, and stuck work sit, so a human can orient → decide → shape

(mirrors `docs/NORTH_STAR.md`, which owns the line — dk edits that file and nothing
else is a source.)

strand is the renderer: it reads the fleet's work and shows where the mass, the bugs
and the stuck work sit. This file owns the epic inventory below — what is done, what
is underway, what comes next. Hand-written; agents do not edit it unprompted.

## Epics

Ordered, one line per epic. Progress is never written here — it derives at read time
from the bd DAG joined against these ids.

1. [in progress] End counts-key resolution drift: strand resolves each repo's row, every tally renderer consumes it → st-one-resolver

## Non-goals

- Owning the catalog. strand renders synthesis over work other tools track.

## Resources

- Direction: `docs/NORTH_STAR.md`
- The queue: bd — `bd ready`, `bd show <id>`
