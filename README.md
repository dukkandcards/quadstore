# quadstore

[![CI](https://github.com/dukkandcards/quadstore/actions/workflows/ci.yml/badge.svg)](https://github.com/dukkandcards/quadstore/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/dukkandcards/quadstore.svg)](https://pkg.go.dev/github.com/dukkandcards/quadstore)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](./LICENSE)
[![Go 1.25+](https://img.shields.io/badge/go-1.25%2B-blue.svg)](https://go.dev)

*Graph with a purpose.*

Here's what I kept hitting.

I had a graph. A real one — entities and relationships, the shape was right, a relational schema would have been the wrong tool. I started on a triple-store. `(subject, predicate, object)`. Fine for two weeks.

Then the questions started.

*Where did this fact come from?* Couldn't tell. Every quad lived in the same anonymous pile. I bolted on a `source` column. Tried to keep it in sync. Drifted within a month.

*Whose data is this?* Multi-tenancy on a triple-store is row-by-row glue. Every read needs an extra clause. Every write has to remember to set it. Every "delete this customer" turns into a query plan I don't trust.

*Can I throw away the derived stuff and rebuild?* Not without taking source data with it. Once derived facts mingled with sources in the same table, the rebuild stopped being safe.

*Who wrote this row, and when?* Audit. Always last on the list, always urgent the day someone asks.

I started writing the same code into every project. Same provenance columns. Same tenant scoping. Same regeneration scripts that didn't quite work. After the third project, I stopped pretending and built the thing I wanted.

quadstore is that thing.

```sh
go get github.com/dukkandcards/quadstore
```

```go
import "github.com/dukkandcards/quadstore"

store, _ := quadstore.Open("graph.db")
defer store.Close()

w, _ := store.Writer(ctx)
w.Commit(ctx, quadstore.Batch{
    Label: "source:hr-feed",
    Adds: []quadstore.Quad{
        {Subject: "person:alice", Predicate: "works-at",   Object: "org:acme"},
        {Subject: "person:alice", Predicate: "reports-to", Object: "person:bob"},
    },
})

r := store.Reader()
for q, _ := range r.Find(ctx, quadstore.Pattern{Subject: "person:alice"}) {
    fmt.Println(q.Predicate, q.Object, "from", q.Label)
}
```

## The fix is the fourth field

quadstore adds a label, and the writer rejects any quad whose label is missing or doesn't begin with one of:

- `source:*` — raw external data; immutable in principle
- `derived:*` — computed from source; deletable and regenerable as a unit
- `human:{tenant}/*` — per-tenant markup; multi-tenancy in the storage, not bolted on
- `meta:*` — system state, ingest bookkeeping, schema versions

The questions that used to leak into every project answer themselves now:

*Where did this come from?* The label says, the commit knows.
*Whose is it?* The tenant is in the label.
*Can I rebuild derivations?* Drop `derived:*`, rebuild from `source:*`.
*Who wrote this and when?* The commit recorded actor and source.

The database refuses to accept rows that don't carry their own provenance, and that single rule makes the rest fall out for free.

## Is this for you

If your graph fits on one machine, your writes go through Go code you control, and you would rather ship a binary than run a server — this is the kind of tool I would hand you.

If you need a query language an analyst can run, sharding across machines, or built-in graph algorithms (PageRank, shortest-path, community detection) — this isn't it. [Dgraph](https://github.com/dgraph-io/dgraph) is the right answer for clusters. [Cayley](https://github.com/cayleygraph/cayley) was the project that showed an embedded graph store could live as a library; it's been unmaintained since 2024.

| | quadstore | [Cayley](https://github.com/cayleygraph/cayley) | [Dgraph](https://github.com/dgraph-io/dgraph) | raw SQLite |
|---|---|---|---|---|
| **Deployment** | embedded Go library | embedded or server | distributed cluster | embedded |
| **Distributed / sharded** | never | no | yes | no |
| **Query language** | Go functions | Gizmo / GraphQL / MQL | DQL (GraphQL+−) | SQL |
| **Schema enforcement** | label namespace | none | strict types | manual |
| **Multi-tenancy** | label-encoded | manual | manual | manual |
| **Idempotent writes** | INSERT OR IGNORE | manual | manual | manual |
| **Provenance / audit** | `commits` table per write | none | none | manual |
| **Pure Go** | yes (modernc.org/sqlite) | yes | yes | depends on driver |
| **License** | MIT | Apache-2.0 | Apache-2.0 (Community) | Public domain |

## The other things that mattered

**Pure Go.** `modernc.org/sqlite` is a Go-native SQLite — no `libsqlite3`, no CGo, no toolchain. `go build` is enough. Cross-compiles to `linux/arm64` from `darwin/arm64` with no setup. Lambda and distroless containers work without ceremony. Most embedded SQLite stories have a CGo footnote that breaks somebody's day; this one doesn't.

**Idempotent ingest.** Real ingest pipelines retry. `INSERT OR IGNORE` means re-runs don't double-count. I have burned a weekend on this exact problem.

**Per-fact-family partitioning when one file isn't enough.** When two fact families don't share queries, `OpenPartitioned` splits them across SQLite files behind one Reader/Writer surface. Bigger graphs without a cluster.

**Writes are within ~2% of hand-rolled SQLite.** [docs/PERFORMANCE.md](docs/PERFORMANCE.md) shows the side-by-side: BulkLoader running quadstore's exact schema is within 2% of what an expert hand-rolled equivalent gets on the same `modernc.org/sqlite` driver. The library overhead is the schema (four-direction index coverage so `Pattern` reads stay fast), not the Go layer.

## Where it stands

`v0.1.x`. API is stabilizing — breaking changes are possible before `v1.0.0` and the CHANGELOG calls them out explicitly. Running in production at [SecDek](https://sfy.io): 28 GB graph, ~10K quads/sec sustained, sub-millisecond point lookups on indexed predicates.

If you ship something on quadstore, open a PR adding it here.

## License

MIT. No paid tier. No enterprise edition. No cloud-only product pulling features back behind a paywall.

The work this stands on — Cayley, SQLite, `modernc.org/sqlite`, the Go toolchain — reached its author because someone else made it freely usable. This is MIT for the same reason: so the next person can pick it up, build on it, and keep going.

## Acknowledgements

quadstore stands on the shoulders of [**Cayley**](https://github.com/cayleygraph/cayley) — the open-source Go graph database written by **Barak Michener**, **Robert Hessmann**, and the contributors who followed. Cayley was released in 2014, generalized across backends (BoltDB, LevelDB, SQL, MongoDB) and query languages (Gizmo, GraphQL, MQL), and is the project that showed an entire generation of Go developers that a graph-shaped store could live as an embedded library, not an enterprise product. The decisions that quadstore takes for granted — quad-shape over triple-shape, idempotent commits, the embedded-library deployment shape — are downstream of choices Barak and Robert made first.

quadstore is the deliberate shrink of that idea: one backend (SQLite via [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite), pure Go), no query language, label namespaces enforced at write time. If you worked on Cayley, this will feel familiar — and the parts that aren't familiar are usually places where we picked the more opinionated path Cayley left to backend authors.

Thank you, Barak and Robert. We are happily here because you were there first. If you ever want to take a look at the code, open an issue, or tell us we got something wrong — we'd be honored.

## Start here

- [`examples/minimal`](./examples/minimal) — open, write, read in one file
- [`examples/audit-log`](./examples/audit-log) — append-only event log with provenance metadata
- [`examples/multi-tenant`](./examples/multi-tenant) — `human:{tenant}/...` labels as the security boundary
- [`docs/PERFORMANCE.md`](./docs/PERFORMANCE.md) — measured numbers, what gets slow, how to fix it
- [`docs/LIMITATIONS.md`](./docs/LIMITATIONS.md) — every known way this is worse than what you might have hoped for; read before adopting
- [`docs/RETHINK_2026.md`](./docs/RETHINK_2026.md) — pre-mortem: what we'd build differently today, and the tests that would change our minds
- [`docs/PARTITIONING_DESIGN.md`](./docs/PARTITIONING_DESIGN.md) — partition routing model and migration semantics
- [`docs/INCREMENTAL_PROCESSING.md`](./docs/INCREMENTAL_PROCESSING.md) — patterns for ingest pipelines that don't re-derive the whole world every tick
- [`CHANGELOG.md`](./CHANGELOG.md) — version history with breaking-change callouts
- [`CONTRIBUTING.md`](./CONTRIBUTING.md) — small patches welcome; distributed-consensus PRs politely declined
