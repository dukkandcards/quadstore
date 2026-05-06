# Performance

Measured numbers on commodity hardware. Reproduce with `go test -bench=. -benchtime=2s ./...`.

## Microbenchmarks (M1 Pro, darwin/arm64, modernc SQLite)

```
BenchmarkCommit_SingleQuad-10    10255    115942 ns/op
BenchmarkCommit_Batch1k-10         100  12434558 ns/op
BenchmarkFind_BySubject-10       17401     69250 ns/op
```

Translated:

| operation | per call | per quad | inverse rate |
|---|---|---|---|
| Single-quad `Writer.Commit` (own transaction, fsync each) | 116 µs | 116 µs | ~8,600 commits/sec |
| 1,000-quad `Writer.Commit` batch | 12.4 ms | 12.4 µs | **~80,000 quads/sec** |
| `Reader.Find` by subject (indexed) | 69 µs | 69 µs | ~14,400 lookups/sec |

These numbers are taken with quadstore's defaults: `journal_mode=WAL`,
`synchronous=NORMAL`, 256 MB page cache. `BulkLoader` flips to
`synchronous=OFF` + `journal_mode=MEMORY` for the duration of a load
and restores the originals on `Close`.

## Production observations (SecDek, EC2 t4g.large, Linux/arm64)

- **Database size:** 28 GB SQLite file with the full SEC + CFTC corpus.
- **Steady-state ingest:** ~10K quads/sec sustained during partition migration, write-only workload, batches of 500 rows × 4 SQL parameters per BulkLoader transaction.
- **Point lookups:** sub-millisecond on indexed predicates; tested at 25K read ops/sec sustained across the EBS gp3 volume during corpus scan.
- **Cold start:** the application's two warmup indices on top of this 28 GB DB build in ~91 seconds at process start. This is application-level pre-aggregation, not quadstore overhead.

## Practical guidance

**Use `BulkLoader` for ingest paths.** It batches writes into transactions sized for SQLite's `SQLITE_MAX_VARIABLE_NUMBER` ceiling (32,766 — the BulkLoader's default `batchSize=500` rows × 4 columns = 2,000 vars per INSERT). Single-quad `Writer.Commit` is correct for one-shot writes but ~7× slower per quad.

**Use `Pattern.Label` to scope reads.** A read with no Label fans out across partitions on a `OpenPartitioned` store; a read with a Label resolves to one partition via your `RouteLabel` callback.

**Use `Pattern.Subject` whenever you can.** The primary index is `(label, subject, predicate)`; subject-prefixed reads are O(log N). Reads without a subject scan more pages.

**Pragmas that matter for ingest workloads.** modernc.org/sqlite defaults to safe (`synchronous=FULL`, `journal_mode=DELETE`). For ingest-heavy paths, override via DSN:

```
?_pragma=synchronous(NORMAL)&_pragma=journal_mode(WAL)&_pragma=cache_size(-64000)
```

`-64000` means "64 MB of cache." `cache_size(-2000)` ("2 MB") is appropriate for memory-constrained processes (e.g. concurrent BulkLoaders against multiple partitions).

**Don't fight SQLite's single-writer rule.** One Writer per partition at a time. Two goroutines opening `WriterFor` against the same partition will serialize; opening against different partitions will not.

## Side-by-side vs raw SQLite (the cost of using the library)

quadstore is a thin layer over `modernc.org/sqlite` (no CGo). To make
the overhead concrete, `bench_compare_test.go` runs the same workload
against a hand-rolled `quads` table on the same driver. Same Go
version, same machine (M1 Pro, darwin/arm64), same PRAGMAs:

- The "default" raw bench uses `journal_mode=WAL` + `synchronous=NORMAL`,
  which matches what quadstore opens with.
- The "BulkLoader-equivalent" raw bench additionally applies the same
  PRAGMAs `BulkLoader` flips internally (`synchronous=OFF`,
  `journal_mode=MEMORY`, 2 GB cache, `temp_store=MEMORY`).

Reproduce with `go test -bench='Compare|BenchmarkCommit_|BenchmarkFind_' -benchtime=2s ./...`:

```
BenchmarkCompare_RawSQLite_SingleInsert-10                   25.6 µs/op
BenchmarkCommit_SingleQuad-10                               115.7 µs/op

BenchmarkCompare_RawSQLite_Batch1k-10                        3.81 ms/op   (3.8 µs/quad)
BenchmarkCommit_Batch1k-10                                  12.87 ms/op   (12.9 µs/quad)

BenchmarkCompare_RawSQLite_FindBySubject-10                  90.6 µs/op
BenchmarkFind_BySubject-10                                   69.0 µs/op

BenchmarkCompare_RawSQLite_BulkLoad/N=1000-10                2.49 ms/op   (2.5 µs/quad)
BenchmarkCompare_Quadstore_BulkLoader/N=1000-10              7.26 ms/op   (7.3 µs/quad)

BenchmarkCompare_RawSQLite_BulkLoad/N=10000-10              32.16 ms/op   (3.2 µs/quad)
BenchmarkCompare_Quadstore_BulkLoader/N=10000-10            72.33 ms/op   (7.2 µs/quad)

BenchmarkCompare_RawSQLite_BulkLoad/N=100000-10            345.11 ms/op   (3.5 µs/quad)
BenchmarkCompare_Quadstore_BulkLoader/N=100000-10          761.59 ms/op   (7.6 µs/quad)
```

Read these as overhead numbers, not headline numbers:

| operation | raw SQLite | quadstore | overhead |
|---|---|---|---|
| Single-quad commit | 25.6 µs | 115.7 µs | **~4.5×** |
| 1,000-quad transaction | 3.8 µs/quad | 12.9 µs/quad | **~3.4×** |
| Subject lookup (~100 rows) | 90.6 µs | 69.0 µs | **0.76×** (quadstore wins) |
| 1k-quad bulk load | 2.5 µs/quad | 7.3 µs/quad | **~2.9×** |
| 10k-quad bulk load | 3.2 µs/quad | 7.2 µs/quad | **~2.25×** |
| 100k-quad bulk load | 3.5 µs/quad | 7.6 µs/quad | **~2.2×** |

Per-quad rate stays flat across N for both — the overhead is structural,
not amortizable, but bounded at roughly 2× at scale.

### Where the 2× actually lives

The simple raw SQLite schema has one unique constraint on
`(label, subject, predicate, object)` and one secondary index on
`subject`. quadstore's schema has a unique constraint on
`(subject, predicate, object, label)` plus four secondary indexes
(`idx_spo`, `idx_pos`, `idx_osp`, `idx_lsp`) so that `Pattern` reads
are fast in any direction. To isolate "is the overhead from schema or
from the Go layer?", `BenchmarkCompare_RawSQLite_QuadstoreShape` runs
raw modernc SQLite using quadstore's exact schema and exact pattern
(drop 3 indexes, multi-row INSERT in 500-row batches, rebuild). Same
benchtime, same machine:

```
BenchmarkCompare_RawSQLite_QuadstoreShape/N=1000-10            6.76 ms/op
BenchmarkCompare_Quadstore_BulkLoader/N=1000-10                7.20 ms/op   (+6%)

BenchmarkCompare_RawSQLite_QuadstoreShape/N=10000-10          73.05 ms/op
BenchmarkCompare_Quadstore_BulkLoader/N=10000-10              74.46 ms/op   (+2%)

BenchmarkCompare_RawSQLite_QuadstoreShape/N=100000-10        773.77 ms/op
BenchmarkCompare_Quadstore_BulkLoader/N=100000-10            788.05 ms/op   (+2%)
```

**The Go-side BulkLoader is within ~2% of expert hand-rolled SQLite
using the same schema.** The 2× vs the simple raw schema is the price
of the four-direction index coverage that makes `Pattern` reads fast
regardless of which columns you bind. Drop those indexes and bulk
load gets faster but `Pattern` reads degrade to table scans on the
unbound dimensions.

If your workload only ever reads by subject, you can replicate
quadstore's pattern in 200 lines yourself — that's the lower bound.
If you read by predicate, object, or label too, you'd reproduce
quadstore's schema and end up at ~the same throughput.

Where the overhead comes from:

- **Schema (~98% of bulk-path overhead).** Four secondary indexes
  (idx_spo, idx_pos, idx_osp, idx_lsp) exist so that `Pattern` reads
  are fast no matter which columns you bind. SQLite has to maintain
  those B-trees. The raw simple schema has one secondary index, which
  is why it's faster on the writer side and slower on every read
  shape that isn't subject-prefixed.
- **Label namespace validation** on every write (`source:` /
  `derived:` / `human:{tenant}` / `meta:` prefix check). Cheap, but
  non-zero.
- **Commit-row writes** — `Writer.Commit` writes a `commits` +
  `commit_ops` audit row recording the partition, label, count, and
  timestamp. That's why single-quad commit shows the largest relative
  cost: one user quad + one commit row + per-statement plumbing.
  `BulkLoader` skips the audit trail by design.
- **Index drop+rebuild on bulk** — `BulkLoader.Close` rebuilds three
  secondary indexes. The drop+rebuild is a net win at 10k+ rows
  (without it the same 100k load is 27% slower on our hardware);
  for loads under ~1-2k rows the rebuild cost can match or exceed
  the inline-maintenance cost it would have replaced, so for small
  loads `Writer.Commit` batches are usually the right choice.
- **Per-Writer plumbing** — context propagation, partition routing,
  per-call validation, the `iter.Seq2` read pipeline. ~2% of bulk
  total; basically free.

Where quadstore wins:

- **Subject lookup is faster** than the raw schema's natural choice
  (`PRIMARY KEY (label, subject, predicate, object)` + a separate
  `idx_subject`). quadstore's primary index is `(label, subject,
  predicate)` directly — fewer pages touched on a leading-subject
  scan.

What this means in practice: for a single-quad commit hot path you
will measure quadstore overhead. For a bulk ingest you will measure
quadstore overhead. For everything else (mixed reads, audit-log
appends, partitioned multi-tenant access) the overhead is in the
noise relative to disk, network, and application logic.

Honest framing: if you have a workload where 4× single-commit overhead
is the bottleneck, you do not want a library — you want to write the
SQL yourself, accept the schema lock-in, and deal with namespace
enforcement, audit logging, and partition routing on your own.

## Benchmarks we'd like to add

The current `bench_test.go` + `bench_compare_test.go` cover commit,
find, and side-by-side raw-SQLite comparison. To make this doc
concrete against external alternatives, we'd like to add (PRs welcome):

- `BenchmarkBulkLoader_1M` — measure end-to-end BulkLoader throughput on a 1 M-quad ingest with `synchronous=NORMAL`.
- `BenchmarkConcurrentReaders` — N readers against a populated store while a writer streams; show contention shape.
- `BenchmarkMigrate_GB-scale` — `Migrate` end-to-end on a 1+ GB source with the various option combinations (`OnlySince`, `NoAudit`).
- Side-by-side vs Cayley with the BoltDB backend (Cayley is unmaintained as of 2024 but still installable for reference numbers).

Until those land, the production observations from SecDek are our most honest answer to "is this fast enough for me?"

## When performance gets worse

Predictable slow-downs you should know about:

1. **`synchronous=FULL` plus a slow disk** → commit latency dominates. Fix with `synchronous=NORMAL` if your durability budget permits.
2. **Pattern reads without an indexed leading column** (no Subject, no Label) → table scan. Pattern reads should always set Subject OR Label, ideally both.
3. **Writer contention.** Multiple Writers against one partition serialize. If you're partitioned, route writes by partition; if you're single-file and need high write fan-in, consider partitioning.
4. **Many small commits.** Each is a transaction with `fsync`. Aggregate into batches.
5. **Cross-partition fan-out reads.** A Pattern with no Label on a partitioned Store reads from every partition, then merges. Provide a `RoutePattern` callback or supply a Label to scope.

## Memory profile

`Writer.Commit` allocates a small batch buffer per call; `BulkLoader` keeps a single allocator across batches. `Reader.Find` returns a streaming `iter.Seq2[Quad, error]` — no quad slice is materialized; the caller pulls one row at a time and decides the buffer policy.

Practical numbers from SecDek:
- Server steady-state RSS: ~150 MB on the 28 GB DB.
- BulkLoader peak RSS during ingest of ~10 M quads: ~300 MB transient.
- The migrate path allocates more — a 2-destination `Migrate` against the 28 GB source held ~7.4 GB RSS at peak. This is being investigated; in the interim, run migrations on a host with at least 8 GB free.
