# quadstore examples

Three runnable programs demonstrating the library's main shapes. Each
is a self-contained `main.go` you can `go run` directly. All three
build clean (CI verifies it on every commit) and write their data to
a `MkdirTemp` directory that is cleaned up on exit.

| example | what it shows |
|---|---|
| [`minimal/`](./minimal) | Open a store, write 3 quads under one source: label, read them back via `Reader.Find`. The 30-second walkthrough. |
| [`audit-log/`](./audit-log) | Append-only event log under `source:hr-feed` with `MetaActor` / `MetaSource` / `MetaReason` per commit. Demonstrates the namespace check rejecting an un-prefixed label. |
| [`multi-tenant/`](./multi-tenant) | Two tenants writing private markup under `human:acme/notes` and `human:globex/notes` against shared source data. The label IS the security boundary — no middleware, no row-level security, no per-tenant database. |

## Running them

```
go run github.com/dukkandcards/quadstore/examples/minimal
go run github.com/dukkandcards/quadstore/examples/audit-log
go run github.com/dukkandcards/quadstore/examples/multi-tenant
```

Or, from a checkout:

```
go run ./examples/minimal
go run ./examples/audit-log
go run ./examples/multi-tenant
```

## What's deliberately not here

- A "search" example. quadstore doesn't ship a query language; the
  pattern-match API is the entire thing.
- A web-server example. quadstore is a library; serve however you serve.
- A "graph algorithm" example. Pattern matching is what we do; PageRank
  / shortest-path / community detection live downstream of this layer.

For production-shaped patterns (sentinel-driven incremental ingest,
typed wrappers around Reader / Writer / BulkLoader, label-namespace
discipline), see [`docs/INCREMENTAL_PROCESSING.md`](../docs/INCREMENTAL_PROCESSING.md).
Examples in this directory deliberately stay minimal so the API
shape is clear; the patterns doc covers what you actually do at
scale.
