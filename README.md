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

store, _ := quadstore.OpenPebble("graph")
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

`OpenPebble` is the recommended backend. `Open(path)` returns the SQLite-backed store with the same Reader/Writer/BulkLoader API — see [Why use the SQLite backend?](#why-use-the-sqlite-backend) below.

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

## What it does, in a sentence

**quadstore observes.** It records what was written, by whom, where it came from, and when — and it refuses to accept anything that doesn't carry its own answer to those questions. It does not compute, rank, classify, judge, or recommend. The application built on top does whatever it does with the data; quadstore is the patient custodian underneath, the one that lets you ask "where did this come from" and "who wrote this row" months later and get a straight answer because it kept watching while everything else moved on.

This stance shows up everywhere in the surface:

- **The label namespace is enforced at write time**, not validated at read. The library refuses bad provenance up front instead of letting you sort it out later.
- **The `commits` + `commit_ops` audit trail is a write-time invariant**, not a separate observability tool. Every Commit records who, what source, what reason, what time, with a UUIDv7 commit ID — at the same transaction boundary as the data.
- **No query language. No graph algorithms.** quadstore surfaces what's there. PageRank, shortest-path, community detection, scoring, ranking — all live downstream of this layer, in the application's code. Mixing computed judgment into the storage layer is how provenance dies.
- **`derived:*` is regenerable from `source:*` as a unit.** The library doesn't pretend to know which derivations are correct; it lets the application drop the lot and rebuild whenever the rules change.

If that posture matches yours — *observe carefully, expose patterns, don't editorialize* — the rest of the docs will feel familiar. If you want a database that scores its rows for you, this isn't the right tool.

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
| **Idempotent writes** | four-key dedup, both backends | manual | manual | manual |
| **Provenance / audit** | `commits` + `commit_ops` per write, both backends | none | none | manual |
| **Pure Go** | yes (Pebble or modernc.org/sqlite) | yes | yes | depends on driver |
| **License** | MIT | Apache-2.0 | Apache-2.0 (Community) | Public domain |

## The other things that mattered

**Pebble-backed by default.** quadstore runs on [Pebble](https://github.com/cockroachdb/pebble), CockroachDB's pure-Go LSM storage engine. The Pebble work was sized for an anticipated 100M+ quad analysis corpus — a 2026-04-19 storage-replacement benchmark loaded a 53K-deck SlideDek port-target into the SQLite backend and showed `Close`-time index rebuild dominating for tens of minutes on 60+ GB / 133M-quad shape. The benchmark's consumer ultimately moved to a columnar (parquet + DuckDB) storage layer better suited to its aggregation-heavy workload, so the 133M-quad figure is *projected* sizing rather than a live workload. The SQLite scaling pain measured by that benchmark is real, and addressing it is what v0.2 delivered. On cloud disks (gp3 EBS), single-quad audited Commit is **40× faster** than the SQLite-backed alternative; bulk loads at 100k rows are **5.5× faster**; on-disk size is **≈10× smaller** (30 GB SecDek production snapshot → 3.4 GB live Pebble dir after default zstd block compression). Validated end-to-end on a 19M-quad production graph round-tripped byte-perfectly between backends, then deployed to live production on SecDek 2026-05-08. Numbers and methodology in [`docs/PEBBLE_VS_SQLITE.md`](docs/PEBBLE_VS_SQLITE.md).

**Pure Go.** Both backends. No CGo, no `libsqlite3`, no `librocksdb`. `go build` is enough. Cross-compiles to `linux/arm64` from `darwin/arm64` with no setup. Lambda and distroless containers work without ceremony. Most embedded-graph stories have a CGo footnote that breaks somebody's day; this one doesn't.

**Idempotent ingest.** Real ingest pipelines retry. The four-key SPO/POS/OSP/LSP layout means re-runs don't double-count on either backend. I have burned a weekend on this exact problem.

**Provenance / audit as a write-time invariant.** Every `Writer.Commit` records a `commits` row (UUIDv7, time-sortable) plus a `commit_ops` op-log row per add/remove. Same semantics on both backends. `Batch.NoAudit: true` opts out for hot-path ingest.

**Per-fact-family partitioning when one file isn't enough.** When two fact families don't share queries, `OpenPartitioned` splits them across SQLite files behind one Reader/Writer surface. Bigger graphs without a cluster. (Pebble partitioning is on the roadmap; today `OpenPebble` is single-dir.)

### Why use the SQLite backend?

`Open(path)` returns the SQLite-backed Store. Same Reader/Writer/BulkLoader API as Pebble; trades the perf wins for these:

- **~20 fewer transitive dependencies.** Pebble pulls in `cockroachdb/*`, `getsentry/sentry-go`, `prometheus/client_golang`, `klauspost/compress`, etc. SQLite-backed Stores need only `modernc.org/sqlite`.
- **`sqlite3` CLI on the data file.** Open the file in any SQLite tool, run ad-hoc SQL, dump tables. Pebble's sstable format has no equivalent escape hatch.
- **Smaller binaries.** ~30 MB difference in compiled size on Linux (Pebble's transitive deps).
- **Hand-rolled bulk-load parity.** BulkLoader is within ~2% of a hand-rolled SQLite equivalent on the same driver — see [`docs/PERFORMANCE.md`](docs/PERFORMANCE.md).

Use `Open(path)` when binary size or dep audit matters more than per-commit latency, or when downstream operators need SQL escape hatch on the data file. Everything else: `OpenPebble`.

## Where it stands

`v0.2-track`. **Pebble-backed `OpenPebble` is the recommended path.**
Same Writer / Reader / BulkLoader / LabelCounts / Stats /
CommitStatsAt surface as the SQLite backend; cross-backend
migration via `MigrateToPebble(ctx, src, dst, opts)`. Two
parity gaps remain on `*PebbleStore` — the legacy `*Iterator`
`Match` API and the Cayley-style `Path` traversal helpers
(`From`/`Out`/`In`/`Has`/`Unique`) — which will be added when
a concrete user requests them.

**Why this exists.** quadstore came out of moving off
ArangoDB after its 3.12 BSL / Community License change made
it unworkable for our projects. The first big port target was
a 60K+ PPTX-analysis product whose 2026-04-19 storage-replacement
benchmark hit ~4-hour bulk loads on the SQLite path with the
index rebuild on `Close` dominating for tens of minutes. The
consuming project ultimately moved its analytics corpus to
parquet + DuckDB (better fit for its aggregation queries), but
the SQLite pain that benchmark surfaced was real and is what the
Pebble work in v0.2 was answering.

**Production users today (2026-05-15).**
[**SecDek**](https://sfy.io) — corporate-intel SaaS over SEC
no-action letters — runs quadstore on the **Pebble** backend at
**3.4 GB on disk / ~19M+ quads / ~280 active predicates / 27
in-process Jobs** (single-binary EC2 t4g.large, nightly Checkpoint
+ tar.gz backup to S3 with GLACIER_IR + DEEP_ARCHIVE lifecycle).
First production Pebble cutover landed 2026-05-08 after a
2026-05-06 attempt was rolled back the next day on a multi-writer-
process incompatibility; the architectural rebuild (in-process Job
scheduler folding 5+ timer-driven binaries into goroutines inside
the server process) unblocked the second cutover. SecDek's
pre-cutover 19M-quad SQLite snapshot was the byte-perfect
round-trip that validated `MigrateToPebble` on real production
data.

[**lawdek-v2**](https://lawdek.com) — patent litigation scheduling
+ chart corpus — runs quadstore on the **SQLite** backend inside
AWS Lambda. `<10 MB` SQLite file on Lambda `/tmp`, restored from
`s3://lawdek-v2-state/lambda/lawdek.db` on cold start, synchronous
PutObject after every write (WAL `TRUNCATE` checkpoint first, fix
shipped 2026-04-16 after a silent-data-loss-on-cold-start
incident). ~47 active predicates total; stateless HMAC-signed
session cookies (no `meta:session` rows). Pinned to a 2026-04-14
commit — pre-v0.2 Pebble — and feature-sufficient for the
workload.

Both backends are production-validated. Other internal dek
products that were originally planned to host on quadstore
(SlideDek, IGdek) ended up on different stacks (parquet + DuckDB
for analytics; bespoke storage per product) — none import
quadstore today, and that's fine. Match tool to workload.

Both backends are supported indefinitely. Whether `Open()`
flips its default backend at `v1.0.0` is an open question —
the API is stabilizing and [`CHANGELOG`](./CHANGELOG.md) calls
breaking changes out explicitly.

If you ship something on quadstore, open a PR adding it here.

## License

MIT. No paid tier. No enterprise edition. No cloud-only product pulling features back behind a paywall.

The work this stands on — Cayley, SQLite, `modernc.org/sqlite`, the Go toolchain — reached its author because someone else made it freely usable. This is MIT for the same reason: so the next person can pick it up, build on it, and keep going.

### Dependency policy

Every direct and transitive dependency must remain on a permissive license (MIT / BSD / Apache 2.0). No AGPL, BSL, SSPL, or commercial-tier dual-licensing. The Pebble backend pulls in libraries maintained by Cockroach Labs — currently all Apache 2.0; explicitly inventoried and rechecked quarterly. See [`docs/LICENSE_AUDIT.md`](./docs/LICENSE_AUDIT.md) for the dep-by-dep list and [`docs/CONTINGENCY.md`](./docs/CONTINGENCY.md) for what happens if any of them relicenses.

## Acknowledgements

quadstore stands on the shoulders of [**Cayley**](https://github.com/cayleygraph/cayley) — the open-source Go graph database written by **Barak Michener**, **Robert Hessmann**, and the contributors who followed. Cayley was released in 2014, generalized across backends (BoltDB, LevelDB, SQL, MongoDB) and query languages (Gizmo, GraphQL, MQL), and is the project that showed an entire generation of Go developers that a graph-shaped store could live as an embedded library, not an enterprise product. The decisions that quadstore takes for granted — quad-shape over triple-shape, idempotent commits, the embedded-library deployment shape — are downstream of choices Barak and Robert made first.

quadstore is the deliberate shrink of that idea: two backends (Pebble recommended, SQLite via [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) supported), both pure Go, no query language, label namespaces enforced at write time. If you worked on Cayley, this will feel familiar — and the parts that aren't familiar are usually places where we picked the more opinionated path Cayley left to backend authors.

Thank you, Barak and Robert Hessmann. We are happily here because you were there first. If you ever want to take a look at the code, open an issue, or tell us we got something wrong — we'd be honored.

Before any of this came together, [**Robert Melton**](https://robertmelton.com/) introduced me to Barak. Robert is on the Cayley contributor list himself and someone I've been talking to about software for the last decade — quadstore exists because that introduction did, and a lot of the taste behind these decisions traces back to conversations with him. Thank you, Robert.

The Pebble backend (recommended as of v0.2) stands on [**Pebble**](https://github.com/cockroachdb/pebble), the pure-Go LSM storage engine maintained by Cockroach Labs. Pebble is an extraction from CockroachDB's storage layer with a clean Go-idiomatic surface, BSD-3-Clause licensed, with the kind of operational maturity that comes from running the world's CockroachDB clusters. The auxiliary libraries (`cockroachdb/errors`, `redact`, `swiss`, `crlib`, `logtags`, `tokenbucket`) are all Apache 2.0 and inventoried in [`docs/LICENSE_AUDIT.md`](./docs/LICENSE_AUDIT.md). If you've shipped Pebble in production: thank you. We benefit from your bug reports.

## Start here

- [`examples/minimal`](./examples/minimal) — open, write, read in one file
- [`examples/audit-log`](./examples/audit-log) — append-only event log with provenance metadata
- [`examples/multi-tenant`](./examples/multi-tenant) — `human:{tenant}/...` labels as the security boundary
- [`docs/PERFORMANCE.md`](./docs/PERFORMANCE.md) — measured numbers, what gets slow, how to fix it
- [`docs/LIMITATIONS.md`](./docs/LIMITATIONS.md) — every known way this is worse than what you might have hoped for; read before adopting
- [`docs/RETHINK_2026.md`](./docs/RETHINK_2026.md) — self-audit: §1 (storage engine) shipped as Pebble in v0.2; §2-§6 still forward-looking
- [`docs/PEBBLE_VS_SQLITE.md`](./docs/PEBBLE_VS_SQLITE.md) — head-to-head bench numbers (5 of 6 metrics Pebble on M1, 6 of 6 on Linux gp3) and the v1.0 default-flip question
- [`docs/MIGRATING_TO_PEBBLE.md`](./docs/MIGRATING_TO_PEBBLE.md) — practical migration guide; includes the SecDek case study (2026-05-06 first cutover rolled back next day on multi-writer-process incompatibility; in-process scheduler rebuild on 2026-05-07/08 unblocked the second cutover; production has been on Pebble since 2026-05-08)
- [`docs/PARTITIONING_DESIGN.md`](./docs/PARTITIONING_DESIGN.md) — partition routing model and migration semantics
- [`docs/INCREMENTAL_PROCESSING.md`](./docs/INCREMENTAL_PROCESSING.md) — patterns for ingest pipelines that don't re-derive the whole world every tick
- [`CHANGELOG.md`](./CHANGELOG.md) — version history with breaking-change callouts
- [`CONTRIBUTING.md`](./CONTRIBUTING.md) — small patches welcome; distributed-consensus PRs politely declined
