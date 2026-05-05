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

Read these as the floor, not the ceiling: SQLite synchronous-mode is `FULL` by default — every commit waits on `fsync`. Production workloads typically use `synchronous=NORMAL` for ingest pipelines and accept a small durability window in exchange for substantially higher commit rates.

## Production observations (SecDek, EC2 t4g.large, Linux/arm64)

- **Database size:** 28 GB SQLite file with the full SEC + CFTC corpus.
- **Steady-state ingest:** ~10K quads/sec sustained during partition migration, write-only workload, batches of 500 rows × 4 SQL parameters per BulkLoader transaction.
- **Point lookups:** sub-millisecond on indexed predicates; tested at 25K read ops/sec sustained across the EBS gp3 volume during corpus scan.
- **Cold start:** the SecDek server's two warmup indices (gridCorpusIndex + cmt review-window) build in ~91 seconds against the 28 GB DB. This is application-level pre-aggregation, not quadstore overhead.

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

## Benchmarks we'd like to add

The current `bench_test.go` covers commit + find. To make this doc concrete against alternatives, we'd like to add (PRs welcome):

- `BenchmarkBulkLoader_1M` — measure end-to-end BulkLoader throughput on a 1 M-quad ingest with `synchronous=NORMAL`.
- `BenchmarkConcurrentReaders` — N readers against a populated store while a writer streams; show contention shape.
- `BenchmarkMigrate_GB-scale` — `Migrate` end-to-end on a 1+ GB source with the various option combinations (`OnlySince`, `NoAudit`).
- Side-by-side vs raw SQLite (manual `quads` table, no library) to measure the library's overhead.
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
