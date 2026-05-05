# quadstore

**A small graph database for small applications.** Written in Go, backed by SQLite, MIT-licensed.

```
go get github.com/dukkandcards/quadstore
```

## What it is

quadstore is a Go library that stores `(subject, predicate, object, label)` quads in a SQLite database on disk. No server, no cluster, no query language. You import it, you get a `Store`, you write quads, you read them back.

It is designed for the class of application whose graph fits comfortably on one machine — tens of thousands to a few million quads — and whose operational budget is one binary, one database file, and possibly one Lambda.

## Why the fourth field matters

Triple-stores give you `(subject, predicate, object)`. That is fine for a weekend project and rots the moment a real product grows on top of it, because every fact in the graph ends up mixed together with no record of where it came from or who it belongs to.

quadstore adds a **label** and enforces it at write time. `Writer.Commit` rejects any quad whose label is not prefixed with one of:

- `source:*` — raw external data. Immutable in principle.
- `derived:*` — anything computed from source. Always regenerable by deleting every row matching `derived:%` and replaying the pipeline.
- `human:{tenant-id}/...` — markup added by a specific human or tenant. Private by default. Multi-tenancy is encoded in the label, not bolted on top.
- `meta:*` — system state: sessions, schema version, ingest bookkeeping.

Every row knows where it came from and whose it is, because the database refuses to accept rows that don't.

## A minimal example

```go
package main

import (
    "log"

    "github.com/dukkandcards/quadstore"
)

func main() {
    store, err := quadstore.Open("graph.db")
    if err != nil {
        log.Fatal(err)
    }
    defer store.Close()

    w := store.Writer()
    w.Add("person:alice", "works-at", "org:acme", "source:hr-feed")
    w.Add("person:alice", "reports-to", "person:bob", "source:hr-feed")
    w.Add("person:alice", "tagged", "keep-an-eye-on", "human:jay/notes")
    if err := w.Commit(); err != nil {
        log.Fatal(err) // rejected if any label is unprefixed
    }

    // Who does alice report to, according to the HR feed?
    for _, obj := range store.Objects("person:alice", "reports-to", "source:hr-feed") {
        log.Println(obj)
    }
}
```

See [`examples/`](./examples) for runnable programs.

## Partitioning (when one file isn't enough)

A single SQLite file is the right shape for most quadstore deployments. When a Store accumulates fact families that don't share queries — say, a graph database for SEC no-action letters that also ingests a million unrelated public-comment letters — the unified `quads` table starts charging the no-action queries for B-tree pages they never read.

`OpenPartitioned` lets you place fact families in independent SQLite files behind the same Reader / Writer / Batch surface:

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
- **Cross-partition batches are rejected.** Commit returns `ErrCrossPartitionBatch` if a batch's quads disagree on partition. Atomicity stops at the partition boundary; we do not pretend to give you cross-file transactions SQLite cannot provide.
- **Reads scope, then fan out.** A `Pattern` whose `Label` resolves through `RouteLabel` reads from one file. A `Pattern` with no Label fans out, merging `iter.Seq2` streams. Order across partitions is unspecified by design.
- **Optional read optimisation.** A second callback, `RoutePattern func(Pattern) Partition`, lets the consumer scope reads by any field — subject prefix, predicate namespace, anything deterministic. The library does not guess; you encode your routing knowledge in this function.
- **One-time migration.** `quadstore.Migrate(ctx, src, dst, opts)` streams a single-file (or differently-partitioned) source into a partitioned destination, routing every quad / commit / commit_op via the destination's `RouteLabel`. The source is read-only throughout. `OnlySince` supports incremental top-up.

When *not* to partition: if your queries naturally span the entire graph, partitioning makes them slower (fan-out cost without the scoping payoff). Partition along a real query-family boundary, or stay single-file.

Full design rationale: [`docs/PARTITIONING_DESIGN.md`](docs/PARTITIONING_DESIGN.md).

## What it isn't

- Not a distributed graph. If you need sharding across machines, use Dgraph, TigerGraph, or JanusGraph.
- Not a query language. We expose Go functions, not a compiler.
- Not a database server. No port, no auth, no admin surface.
- Not cloud-only. A filesystem is enough.

## Applications using it

- [**SecDek**](https://sfy.io) — graph over SEC and CFTC staff letters joined to EDGAR filings, counsel networks, partner-continuity timelines, and timing clusters. Production.
- **lawdek** — general-purpose legal-document graph.
- **igdek** — interest / information graph explorer.
- **pubdek** — public-records and publications graph.

## Inspiration

quadstore is a spiritual descendant of [**Cayley**](https://github.com/cayleygraph/cayley), the open-source Go graph database released in 2014 and authored by Barak Michener, Robert, and the contributors who followed. Cayley generalized across backends and query languages; quadstore deliberately shrank the idea further to one backend, no query language, and enforced label namespaces. If you worked on Cayley, this will feel familiar.

## Message to the community

Graph databases should be ordinary. The shape is useful for a very large class of applications — anything where the hard question is *what connects to what* — and it is absurd that the current market treats "graph database" as a category that implies enterprise procurement.

This project will stay MIT-licensed. No paid tier for larger footprints, no enterprise edition with features held back, no cloud-only offering that gates the interesting parts.

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md). The short version: small patches welcome, backend additions welcome, distributed-consensus PRs politely declined.

## License

[MIT](./LICENSE).
