# Partitioning Design

**Status:** DRAFT — for Jay's review (2026-05-05)
**Track:** Track 1 of the Performance/Backend ladder (per `TODO.md`)
**Scope:** This doc covers the library API, semantics, and migration tooling.
DuckDB / Pebble swap (Track 2) is intentionally **out of scope** here.

## Why

The single `quads` table hits a B-tree dilution wall when a Store accumulates
fact families that don't share queries. Concrete trigger: SecDek's live DB
grew from 1.3 GB → 28 GB across April–May 2026 after a comment-letter corpus
load added ~12 M `source:cmt-*` and `derived:cmt-*` quads. The product code
that operates on the ~256 K `source:sec-letter` rows now scans a B-tree where
~47× of the leaves are unrelated — every full-predicate scan pays the
dilution tax even when the consumer never reads the corpus rows.

Per `TODO.md`:

> Partition by label namespace. Today every quad lives in one `quads` table.
> As `derived:*` grows, scans pay for unrelated `source:*` rows. Either
> separate tables per namespace or label-prefixed indexes.

This is a library-level feature: any consumer with multiple fact families
that don't share queries (SlideDek's per-corpus loads, IGdek's per-customer
markup, LawDek's per-matter writes, mega-index's per-book corpora) hits
the same wall eventually.

## Non-goals

- **Cross-partition transactions.** SQLite cannot do them; we will not pretend.
- **Sharding for scale-out.** Partitions are colocated on one filesystem.
  This is not a distributed system.
- **Auto-rebalancing.** The Router is static and supplied by the consumer.
- **Hiding partitioning from queries that span fact families.** When a query
  legitimately needs all partitions, it pays a fan-out cost; we do not
  pretend otherwise.
- **Replacing the SQLite backend.** That is Track 2 (DuckDB prototype). This
  design is single-substrate.

## Design constraints (carried forward from existing decisions)

- **Reader / Writer / Batch interface stays stable.** Already documented as
  the migration target since Rung 1 (2026-04-13). Adding partitioning must
  not break existing call sites.
- **Pure-Go, modernc.org/sqlite, MIT-license-compatible.** Standing rule.
- **Additive migration.** No breaking schema changes until LawDek imports
  (per existing memory). Each partition file uses the unmodified schema v2.
- **Single-writer-per-partition.** SQLite's hard constraint applies
  per-file. Partitioning gives the consumer N writer slots, not infinite.

## API

The shape: a partitioned Store is a Store whose `Writer` and `Reader` route
operations across multiple backing files. The existing `Partition` type and
`WriterFor` method (writer.go:14, writer.go:60) were placed at API design
time as the slot for this feature; partitioning fills them in.

### New types

```go
// Router maps a label to a Partition. Returning the empty Partition means
// "default partition" (the one named in PartitionedConfig.Default).
//
// Routers are deterministic and stateless: the same input must always
// produce the same output. The library calls Route on every Writer.Commit
// to validate that all quads in a Batch route to the same partition, and
// on every Reader.Find with a non-empty Pattern.Label to scope reads.
type Router func(label string) Partition

// PartitionedConfig configures a partitioned Store.
type PartitionedConfig struct {
    // Root is the directory holding the per-partition .db files.
    Root string

    // Partitions enumerates the partitions and the path each one's
    // backing file should live at, relative to Root. Order is irrelevant.
    Partitions []PartitionSpec

    // Default is the partition that receives any quad whose Router output
    // is the empty Partition. Required.
    Default Partition

    // Route is the consumer-supplied label → partition function. Required.
    Route Router
}

// PartitionSpec names a partition and its backing file.
type PartitionSpec struct {
    Name Partition // unique within a PartitionedConfig
    File string   // relative to Root, e.g. "main.db" or "corpus.db"
}
```

### New constructor

```go
// OpenPartitioned opens or creates a partitioned Store. Each partition
// is a fully independent SQLite file with its own commits / commit_ops
// / quads tables. Operations route by the supplied Router.
//
// The returned Store is API-compatible with one obtained from Open:
// existing Reader / Writer / Batch code works unchanged. Partitioning
// is an internal routing concern.
//
// PartitionedStores cost N file handles, N writer slots, N WAL files.
// Open partitions when the consumer's data has clear non-overlapping
// query families; do not partition for the sake of it.
func OpenPartitioned(cfg PartitionedConfig) (*Store, error)
```

### Existing API: behavior under partitioning

| Method | Single Store | Partitioned Store |
|---|---|---|
| `Store.Reader()` | one Reader, one file | one Reader, fans out |
| `Store.Writer(ctx)` | acquires the writer slot | acquires **default** partition's slot |
| `Store.WriterFor(ctx, p)` | ignores p | acquires partition p's slot |
| `Reader.Find(ctx, pattern)` | one query | scoped if `pattern.Label` resolves to one partition; otherwise fan-out merge |
| `Reader.Count(ctx, pattern)` | one query | summed across partitions when fan-out |
| `Writer.Commit(ctx, batch)` | one transaction | validates batch routes to ONE partition, otherwise rejects |
| `Writer.PruneOps(ctx, t)` | one DELETE | delegates to the writer's partition only |
| `Store.Vacuum()` | one VACUUM | per-partition VACUUM, sequenced |
| `Store.Close()` | closes one file | closes all partitions |

### Reader semantics — fan-out details

The reader needs to handle three pattern shapes:

1. **`pattern.Label` non-empty and Router resolves to one partition.**
   Single-file query. No merge cost. This is the partitioning win — the
   typical product query.

2. **`pattern.Label` empty.** Fan-out: open one iterator per partition,
   yield quads from all. **Order is unspecified** across partitions; within
   a partition, order matches the underlying SQLite query.

3. **`pattern.Label` non-empty but Router is non-injective** (multiple
   labels map to one partition, but the *given* label maps to one).
   Same as case 1 — single-file.

Order across partitions is unspecified by design. Promising sorted output
would force a merge sort across N files which voids the speedup. Consumers
that need sorted output can sort in their layer or use `Reader.Count` for
volume queries.

### Writer semantics — Batch routing

`Writer.Commit(ctx, batch)` validates that every quad in `batch.Adds` and
`batch.Removes` routes to the same partition. The check uses, per quad:

```
label = q.Label    (if non-empty)
        else b.Label  (if non-empty)
        else error — quads without a resolvable label cannot route
target = Router(label)
```

If targets disagree across the batch, `Commit` returns an error
(`ErrCrossPartitionBatch`) and rolls back. Consumers split the batch by
partition before committing.

This rule is structural — there is no SQL transaction across SQLite files
and we will not fake atomicity at the library layer. A consumer that
genuinely needs cross-partition atomicity has the wrong data model for
this library and should not partition along that axis.

### `Store.Writer(ctx)` vs `Store.WriterFor(ctx, p)`

`Writer(ctx)` is preserved for API compatibility but on a partitioned Store
acquires the **default** partition's writer slot. Code that was written
against a single Store and is later partitioned along orthogonal axes will
keep working as long as the default partition holds what the legacy code
writes. New code on a partitioned Store should use `WriterFor`.

## Schema and storage

Each partition is a fully independent SQLite file:

```
<root>/
  main.db
  main.db-wal
  main.db-shm
  corpus.db
  corpus.db-wal
  corpus.db-shm
  ...
```

Each file:

- Carries the unmodified schema v2 (quads, commits, commit_ops, meta).
- Has its own WAL, busy_timeout, cache, etc. — applied at open time.
- Records its `name` in the `meta` table key `partition_name` so a stray
  open of a partition file outside its config can self-identify.
- Has its own commit-id space. UUIDv7 commit IDs are time-sortable across
  partitions but not contiguous — the audit trail of any single commit
  lives in exactly one partition.

What does **not** change:

- The `quads` schema, indexes (idx_spo / idx_pos / idx_osp / idx_lsp),
  validate-label rules, or migration framework.

## Migration tooling

### Race-free path: `MigrateFromSnapshot`

The recommended migration entrypoint when the source DB has any
concurrent writers (cron'd ingest jobs, a live web server, etc.):

```go
stats, err := quadstore.MigrateFromSnapshot(ctx, "/var/lib/myapp/legacy.db", dst,
    quadstore.SnapshotOptions{
        SnapshotPath: "/var/lib/myapp/snapshot.db",
        KeepSnapshot: false,
        Migrate: quadstore.MigrateOptions{
            ChunkSize:   10000,
            CopyCommits: true,
        },
    })
```

`MigrateFromSnapshot` runs in three phases:

1. **Snapshot.** Issues `VACUUM INTO 'SnapshotPath'` against the source.
   Per [SQLite docs](https://sqlite.org/lang_vacuum.html), this produces
   a consistent point-in-time copy without an exclusive lock — concurrent
   writers continue to write to the source throughout. The snapshot is
   a frozen, defragmented file that no longer mutates.
2. **Migrate.** Opens the snapshot read-only and calls the regular
   `Migrate` against it. Because the snapshot is frozen, every quad,
   commit, and commit_op the migration sees is from a single
   point-in-time — there is no torn-snapshot surface.
3. **Cleanup.** Removes the snapshot file unless `KeepSnapshot=true`
   (useful for verification / audit).

This is the right choice when:

- The source is being actively written to (a live system).
- A torn migration could under-count transient state — e.g., a daily
  refresh job that `[remove]`s and re-adds a derived label, where a
  migration starting mid-run would capture only the partially-rebuilt
  rows.
- Operator coordination ("stop the timers, run migrate, restart the
  timers") is fragile or expensive to enforce.

Cost: temporary 2× disk usage (snapshot + source coexist for the
migration's duration). VACUUM INTO time scales linearly with source size
— roughly 1–2 minutes per GB on gp3 6000 IOPS storage in our measurements.

### Direct path: `Migrate`

When the source is genuinely quiescent (no writers — e.g., a one-shot
import from an exported file), `Migrate` is fine and avoids the snapshot
disk overhead:

```go
stats, err := quadstore.Migrate(ctx, src, dst, opts)
```

The library does not enforce quiescence; it trusts the caller. Use
`MigrateFromSnapshot` if you can't guarantee it.

### CLI shape

A new command, `cmd/partition-migrate`, splits a single-file Store into a
partitioned set:

```
partition-migrate \
  -in /path/to/legacy.db \
  -out /path/to/new-root/ \
  -config partitions.json \
  [-dry-run]
```

`partitions.json` declares the target shape:

```json
{
  "default": "main",
  "partitions": [
    { "name": "main",   "file": "main.db" },
    { "name": "corpus", "file": "corpus.db" }
  ],
  "routing": [
    { "label_prefix": "source:cmt-",            "partition": "corpus" },
    { "label_prefix": "source:sec-comment-letter", "partition": "corpus" },
    { "label_prefix": "derived:cmt-",           "partition": "corpus" },
    { "label_prefix": "derived:body-",          "partition": "corpus" },
    { "label_prefix": "derived:paragraph-reuse","partition": "corpus" }
  ]
}
```

Routes resolve longest-prefix-match. Anything that does not match falls to
`default`.

The tool runs in five stages, each loud:

1. **Inspect.** Open the source DB read-only. Group `quads`, `commits`,
   `commit_ops` rows by destination partition. Print per-partition counts
   and estimated bytes.
2. **Plan.** Print which target file each partition writes to and the
   ChunkedCommit batch size that will be used. Stop here if `-dry-run`.
3. **Build.** Open each target DB (creating new files), copy quads in
   chunks, copy commits + commit_ops belonging to each partition. The
   source DB is never modified.
4. **Verify.** For each partition, count quads/commits/commit_ops
   destination vs. source-with-Router-applied. Refuse to proceed unless
   counts match.
5. **Report.** Print total quads moved, per-partition file sizes, total
   wall time. The source DB is left in place; the consumer flips its
   `Open(path)` call to `OpenPartitioned(cfg)` when ready.

Atomicity boundary: the migration is **read-only on the source**. The cut
over from single → partitioned is the consumer's deploy step (point at the
new root). Rollback is "point back at the old DB."

`-dry-run` is the default; `-apply` is required to actually write target
files. This protects from foot-shotting on a 28 GB source.

## Test plan

Library tests (`partitioned_test.go`):

- **Routing correctness.** Given a Router and a series of writes, each
  quad lands in the expected partition file. Verify by opening each
  partition file directly with `Open` and checking the quads table.
- **Fan-out correctness.** With quads pre-loaded into N partitions, a
  Reader.Find with empty Label returns the union; with a specific Label,
  returns only matches from the correct partition.
- **Cross-partition batch rejection.** A Batch whose adds route to
  multiple partitions returns `ErrCrossPartitionBatch` and writes nothing.
- **Default partition.** Quads whose Router returns "" land in the
  configured default partition. Quads with no resolvable label return an
  error from Commit (the existing label-validation path).
- **Writer slot independence.** Two goroutines acquiring writers for two
  different partitions both succeed concurrently. Two goroutines for the
  same partition serialize.
- **Migration round-trip.** Source DB → migrate → verify per-partition
  counts → re-merge into one DB → diff against source.
- **Schema migration on partition open.** Each partition file passes
  through `migrate(db)` independently. Mixed-version partitions refused.

App-level tests (consumer side, e.g. SecDek):

- Existing query test suite passes against a partitioned Store with the
  same data laid out across two partitions.

## Open-source readiness

- API is consumer-agnostic. No SecDek / SlideDek / LawDek terminology
  leaks into types or method names. Examples in `docs/` reference generic
  fact families (`source:invoices`, `derived:tax-summary`).
- `Router` is supplied by the consumer; the library carries no built-in
  routing tables.
- Migration tool config is JSON, not Go-coded routing.
- All errors are sentinel constants (`ErrCrossPartitionBatch`,
  `ErrUnknownPartition`, `ErrPartitionMissing`) — testable from outside
  the package.
- License: unchanged (MIT).

## Rollout

Doc-only review (this PR). No code lands until the design is approved.

If approved:

1. Implement `OpenPartitioned`, `Router`, `PartitionedConfig`,
   `PartitionSpec` in a new `partitioned.go`. ~300 lines.
2. Implement the Reader fan-out and Writer routing changes in `reader.go`
   and `writer.go`. ~150 lines + tests.
3. Implement `cmd/partition-migrate`. ~250 lines + tests.
4. Update `README.md` with a Partitioning section.
5. Bump `CHANGELOG.md`. No version-tag bump needed (additive).

First consumer: SecDek. Migration plan in `~/secdek/docs/PARTITION_MIGRATION_PLAN.md`.

## Decisions (locked 2026-05-05)

1. **Routing config: Go function literal at `OpenPartitioned` site.** The
   `Router` is a Go function compiled into the consumer's binary. No JSON
   parsed at runtime; consumers typecheck their own routing.
   - Consequence: the migration tool **is not a generic `cmd/partition-
     migrate` shipped by this library.** It is a Go API
     (`quadstore.Migrate`) that consumers wrap in their own CLI
     subcommand using their compiled-in Router. One source of truth: the
     consumer's Go code.
2. **Default partition name: consumer-controlled** via
   `PartitionedConfig.Default`. The library assumes nothing.
3. **Reader fan-out: optimize when routable, fan out only as last resort.**
   The library accepts a second optional callback,
   `PartitionedConfig.RoutePattern func(Pattern) Partition`, and consults
   it on every `Reader.Find` / `Reader.Count`. The consumer implements
   whatever deterministic logic it has — label, subject prefix, predicate
   namespace, anything — and the library routes to that partition when
   the callback returns a non-empty Partition. Only when *both* the
   pattern's Label is empty AND `RoutePattern` returns "" does the
   library fan out across partitions. The library itself never guesses
   from subject prefixes; the consumer encodes its routing knowledge in
   `RoutePattern`. This keeps the rigor (no library-side guessing) and
   buys the optimization (subject-prefix scoping when the consumer has
   that knowledge — which SecDek does, since `cmt:` subjects live only
   in corpus and `letter:` subjects live in main).
4. **Cross-partition Vacuum/Prune: per-partition variants AND sequential
   defaults.** Each partition is an independent unit (rigorous), so
   `Store.VacuumFor(p)` and `Writer.PruneOpsFor(p, t)` exist. The
   convenience methods `Store.Vacuum()` and `Writer.PruneOps(t)` iterate
   partitions sequentially (efficient — admin operations are IO-bound and
   running them in parallel just contends for the same disk).
