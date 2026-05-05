# Acknowledgements

This project would not exist in its current form without the work of
others. Calling them out by name.

## Cayley

[**Cayley**](https://github.com/cayleygraph/cayley) was released in 2014
by **Barak Michener** and **Robert Hessmann**, with significant contributions
from the open-source community over the following decade. Cayley was the
project that established, for the Go community, that a graph-shaped store
could be an ordinary embedded library — that you could `import` your way
to a graph without standing up a cluster.

quadstore is a deliberate shrink of Cayley's idea — one backend, no query
language, namespace-enforced labels. The shape of `Reader`, `Writer`,
`Quad`, and the idea that idempotent INSERT-OR-IGNORE commits are the
right primitive: these are things Cayley taught us first.

If Barak or Robert ever read this, want to look at the code, open an
issue, or tell us we got something wrong: we'd be honored. The door is
open. quadstore is MIT-licensed and intended to stay that way; if either
of you ever want to ship something Cayley-shaped on top of it, you have
our enthusiastic permission and our help.

## Pebble

[**Pebble**](https://github.com/cockroachdb/pebble) is the pure-Go LSM
storage engine extracted from CockroachDB's storage layer, BSD-3-Clause
licensed, maintained by **Cockroach Labs**. quadstore's optional v0.2
Pebble backend (`OpenPebble`) is a thin adapter over Pebble's
single-process Reader/Writer API — Pebble does the actual work of
"durable bytes on disk, fast." The 40× single-quad-commit improvement
on EC2 vs. modernc.org/sqlite is Pebble's win, not ours.

Pebble is unusual in that it's a serious production storage engine
that has been packaged as an importable Go library — most engines of
its caliber assume they're the database. The decision to release it
that way, and to keep it permissively licensed even after CockroachDB
itself moved off Apache 2.0, is the kind of thing we don't take for
granted. Auxiliary libraries (`cockroachdb/errors`, `redact`, `swiss`,
`crlib`, `logtags`, `tokenbucket`) are all Apache 2.0 and inventoried
in [`docs/LICENSE_AUDIT.md`](docs/LICENSE_AUDIT.md).

## modernc.org/sqlite

[**modernc.org/sqlite**](https://pkg.go.dev/modernc.org/sqlite) is the
pure-Go SQLite reimplementation maintained by **Jan Mercl** and contributors.
quadstore is pure Go end-to-end because of their work — there is no
`libsqlite3` to compile, no CGo to debug, no platform-specific fallback.
This is the kind of unglamorous infrastructure that makes everything
above it possible.

## SQLite

[**SQLite**](https://www.sqlite.org/) is the reason any of this works.
**D. Richard Hipp** and team have spent decades building the world's
most reliable embedded database, and made it free. We use it because it
is correct, fast, ubiquitous, and small. Nothing we do here would be
possible without it.

## Go

The fact that quadstore can be a 50 MB binary that includes the entire
graph database — embedded, single-file, cross-compiled to five platforms
without a toolchain — is the Go team's quiet gift. `iter.Seq2` from Go
1.23 (range-over-func) made the Reader API substantially cleaner; we
adopted it the moment it landed.

## Open-source maintainers, generally

Every project named in `go.sum` is somebody's work. We try not to
take it for granted.
