# quadstore

[![CI](https://github.com/dukkandcards/quadstore/actions/workflows/ci.yml/badge.svg)](https://github.com/dukkandcards/quadstore/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/dukkandcards/quadstore.svg)](https://pkg.go.dev/github.com/dukkandcards/quadstore)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](./LICENSE)
[![Go 1.25+](https://img.shields.io/badge/go-1.25%2B-blue.svg)](https://go.dev)

**A small graph database for Go applications.** Pure Go, single-node, embedded in your binary, backed by SQLite. No CGo. No server. No cluster. No query language. `import` it, get a `Store`, write quads, read them back.

```
go get github.com/dukkandcards/quadstore
```

## Status

`v0.1.x`. API is stabilizing. Pure Go (no CGo) — `go build` is enough; cross-compilation to `linux/arm64` from `darwin/arm64` works without a toolchain. Used in production by [SecDek](https://sfy.io) on a 28 GB graph, ~10K quads/sec sustained ingest, sub-millisecond point lookups. Breaking changes are possible before `v1.0.0`; CHANGELOG calls them out explicitly.

quadstore deliberately doesn't shard. If you need a graph distributed across machines, use [Dgraph](https://github.com/dgraph-io/dgraph) or [JanusGraph](https://janusgraph.org/). If your graph fits on one machine — and most do — quadstore is for you.

## What you get (idiomatic Go)

- A `Store` you `Open` like a SQLite database.
- A `Writer` that takes typed `Batch` writes with namespace-enforced labels.
- A `Reader` that returns `iter.Seq2[Quad, error]` for pattern matches — Go 1.23+ range-over-func.
- A `BulkLoader` for ingest paths, sized for SQLite's `SQLITE_MAX_VARIABLE_NUMBER` ceiling.
- Optional `OpenPartitioned` to split fact families across SQLite files behind one Reader/Writer interface.
- A `Migrate` / `MigrateFromSnapshot` pair for moving data between Stores without holding the source DB at write-locking risk.

Everything is pure Go — `modernc.org/sqlite` is the only DB dependency and it's a Go-native SQLite, no `libsqlite3` shared object required. No CGo means no toolchain headaches, no surprise platform breakage on `linux/arm64` Lambda or distroless containers, and `go install` Just Works.

## Why the fourth field matters

Triple-stores give you `(subject, predicate, object)`. That's fine for a weekend project and rots the moment a real product grows on top of it — every fact ends up mixed together with no record of where it came from or who it belongs to.

quadstore adds a **label** and enforces a namespace at write time. `Writer.Commit` rejects any quad whose label is not prefixed with one of:

| prefix | meaning |
|---|---|
| `source:*` | raw external data; immutable in principle |
| `derived:*` | computed from source; deletable + regenerable as a unit |
| `human:{tenant-id}/*` | per-tenant markup; multi-tenancy in the storage, not bolted on |
| `meta:*` | system state — sessions, schema versions, ingest bookkeeping |

Every row knows where it came from and whose it is, because the database refuses to accept rows that don't. This single property turns out to make a *lot* of normally-hard things easy: tenant deletion, derivation regeneration, audit trails, schema migration.

## A minimal example

```go
package main

import (
    "context"
    "log"

    "github.com/dukkandcards/quadstore"
)

func main() {
    store, err := quadstore.Open("graph.db")
    if err != nil {
        log.Fatal(err)
    }
    defer store.Close()

    ctx := context.Background()

    // Write
    w, err := store.Writer(ctx)
    if err != nil {
        log.Fatal(err)
    }
    defer w.Close()
    err = w.Commit(ctx, quadstore.Batch{
        Label: "source:hr-feed",
        Adds: []quadstore.Quad{
            {Subject: "person:alice", Predicate: "works-at",   Object: "org:acme"},
            {Subject: "person:alice", Predicate: "reports-to", Object: "person:bob"},
        },
        Metadata: map[string]string{
            quadstore.MetaActor:  "import-2026-05-05",
            quadstore.MetaSource: "hr-feed-v3",
        },
    })
    if err != nil {
        log.Fatal(err) // rejected if any label lacks a namespace prefix
    }

    // Read
    r := store.Reader()
    for q, err := range r.Find(ctx, quadstore.Pattern{
        Subject:   "person:alice",
        Predicate: "reports-to",
    }) {
        if err != nil {
            log.Fatal(err)
        }
        log.Printf("alice reports to %s (%s)", q.Object, q.Label)
    }
}
```

See [`examples/`](./examples) for runnable programs (audit log, multi-tenant graph, ingest pipeline).

## Comparison

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

If you're choosing between these, the practical question is: do you need to scale beyond one machine? If yes, Dgraph. If no, quadstore is built for you. Cayley is the spiritual ancestor of this project — generalized across backends and query languages — but is unmaintained as of 2024.

## Partitioning (when one file isn't enough)

A single SQLite file is the right shape for most quadstore deployments. Once a Store accumulates fact families that don't share queries — say, a graph of SEC no-action letters that also ingests millions of unrelated public-comment letters — the unified `quads` table starts charging the no-action queries for B-tree pages they never read.

`OpenPartitioned` places fact families in independent SQLite files behind the same Reader / Writer / Batch surface:

```go
import "strings"

s, err := quadstore.OpenPartitioned(quadstore.PartitionedConfig{
    Root:    "/var/lib/myapp",
    Default: "main",
    Partitions: []quadstore.PartitionSpec{
        {Name: "main",   File: "main.db"},
        {Name: "corpus", File: "corpus.db"},
    },
    RouteLabel: func(label string) quadstore.Partition {
        if strings.HasPrefix(label, "source:cmt-") ||
            strings.HasPrefix(label, "derived:cmt-") {
            return "corpus"
        }
        return "main"
    },
})
```

Properties:

- **Independent writer slots.** Two goroutines acquiring `WriterFor("main")` and `WriterFor("corpus")` run concurrently; SQLite's single-writer rule applies per file.
- **Cross-partition batches are rejected.** Commit returns `ErrCrossPartitionBatch` when a batch's quads disagree on partition. Atomicity stops at the partition boundary; we don't pretend to give you cross-file transactions SQLite can't provide.
- **Reads scope, then fan out.** A `Pattern` whose `Label` resolves through `RouteLabel` reads from one file. A `Pattern` with no Label fans out, merging `iter.Seq2` streams. Order across partitions is unspecified by design.
- **Optional read scoping.** A `RoutePattern func(Pattern) Partition` callback lets the consumer scope reads by any field — subject prefix, predicate namespace, anything deterministic. The library does not guess; you encode your routing knowledge.
- **One-time migration.** `quadstore.Migrate(ctx, src, dst, opts)` streams a single-file (or differently-partitioned) source into a partitioned destination, routing every quad / commit / commit_op through the destination's `RouteLabel`. The source is read-only throughout. `OnlySince` supports incremental top-up.

When *not* to partition: if your queries naturally span the entire graph, partitioning makes them slower (fan-out cost without the scoping payoff). Partition along a real query-family boundary, or stay single-file.

Full design rationale: [`docs/PARTITIONING_DESIGN.md`](docs/PARTITIONING_DESIGN.md).

## What it isn't

- Not a distributed graph. Single-node by design.
- Not a query language. Go functions, not a compiler.
- Not a database server. No port, no auth, no admin surface.
- Not cloud-only. A filesystem is enough.
- Not a graph-algorithm library. We give you typed pattern matching, not PageRank.

## Documentation

- [`docs/PARTITIONING_DESIGN.md`](docs/PARTITIONING_DESIGN.md) — the partition routing model and migration semantics.
- [`docs/INCREMENTAL_PROCESSING.md`](docs/INCREMENTAL_PROCESSING.md) — patterns for ingest pipelines that don't re-derive the whole world every tick.
- [`CHANGELOG.md`](CHANGELOG.md) — version history with breaking-change callouts.
- [`CONTRIBUTING.md`](CONTRIBUTING.md) — small patches welcome; distributed-consensus PRs politely declined.
- [`RESEARCH.md`](RESEARCH.md) — open notes on derivation/clustering work that informs library direction.

## Used in production

- [**SecDek**](https://sfy.io) — graph over SEC and CFTC staff letters joined to EDGAR filings, counsel networks, partner-continuity timelines. 28 GB graph, ~10K quads/sec sustained ingest, sub-millisecond point lookups on indexed predicates.

If you ship something on quadstore, open a PR adding it here.

## Inspiration

quadstore is a spiritual descendant of [**Cayley**](https://github.com/cayleygraph/cayley), the open-source Go graph database written by Barak Michener, Robert Hessmann, and the contributors who followed. Cayley was released in 2014 and generalized across backends (BoltDB, LevelDB, SQL, Mongo) and query languages (Gizmo, GraphQL, MQL); the project showed that a graph-shaped store didn't need to be an enterprise product to be useful, and a lot of what's quietly correct about quadstore — quad-shape over triple-shape, idempotent commits, the embedded-library shape — is downstream of decisions Cayley made first.

quadstore is the deliberate shrink of that idea: one backend (SQLite via [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite), pure Go), no query language, label namespaces enforced at write time. If you worked on Cayley, this will feel familiar — and the parts that aren't familiar are usually places where we picked the more opinionated path Cayley left to backend authors.

## A note on philosophy

Graph databases should be ordinary. The shape is useful for a very large class of applications — anything where the hard question is *what connects to what* — and it is absurd that the current market treats "graph database" as a category that implies enterprise procurement.

This project will stay MIT-licensed. No paid tier. No enterprise edition. No cloud-only offering that gates the interesting parts. If you want to run it, run it.

## License

[MIT](./LICENSE).
