# Limitations

This is the list of every known way quadstore is worse than what you might have hoped for. Read it before you adopt. We will keep it current.

If you hit a limitation we haven't listed here, that's a bug in this doc — please open an issue.

> **Backend note:** the recommended backend is Pebble
> (`quadstore.OpenPebble`); see the top-level
> [README](../README.md) for the rationale and verified numbers.
> Items below apply to both backends except where tagged
> **[SQLite-only]** (the legacy `quadstore.Open(path)` path) or
> **[Pebble-only]** (`quadstore.OpenPebble(path)`). Measured
> deltas live in [`PEBBLE_VS_SQLITE.md`](./PEBBLE_VS_SQLITE.md).

## Hard constraints (will not change)

These follow from the embedded-Go, single-machine design. They are the price of the rest of the library being small.

- **One writer per partition at a time. [SQLite-only.]** SQLite serializes writes per database file. `modernc.org/sqlite` is a pure-Go port that does not work around this. Two goroutines opening `WriterFor` against the same partition will block; against different partitions they run independently. **Pebble has no single-writer ceiling** — concurrent Writers run in parallel via WriteBatch + WAL append.
- **No distribution, no sharding, no replication.** quadstore runs in one process against one filesystem. If you need writes coordinated across machines, this is the wrong tool. Use [Dgraph](https://github.com/dgraph-io/dgraph).
- **No query language.** Reads are Go functions: `Pattern`, `Find`, `Count`, `Match`, `Shape`, `Path`. There is no Cypher, no Gizmo, no GraphQL endpoint. We will not add one.
- **No graph algorithms.** No PageRank, no shortest-path, no community detection, no centrality measures. quadstore stores quads and lets you iterate over them. Anything algorithmic is your code.
- **The data file is the unit of fault.** If the file (SQLite) or directory (Pebble) is corrupted — disk failure, partial write of an unsupported PRAGMA combination, crash during `VACUUM INTO`, partial sstable on Pebble — every quad in it is at risk. Back it up. Single-node deployment is a feature, not a high-availability story.
- **`Migrate` is best-effort, not transactional.** It streams source → destination via `BulkLoader` and does not hold the source under a global lock; concurrent writes to the source during `Migrate` may or may not appear in the destination. For consistent migrations, snapshot first (`MigrateFromSnapshot`, SQLite source only).

## Performance ceilings (measured)

SQLite numbers are M1 Pro / darwin-arm64, `modernc.org/sqlite`, default PRAGMAs (`WAL` + `synchronous=NORMAL`). Pebble numbers are the same workload run against `OpenPebble` with default options + `pebble.NoSync` (matches SQLite's `NORMAL`). Reproduce with `go test -bench=. -benchtime=2s ./...`. Full head-to-head breakdown in [`docs/PEBBLE_VS_SQLITE.md`](./PEBBLE_VS_SQLITE.md).

- **Bulk write floor: ~7.5 µs/quad on SQLite [SQLite-only]** (~135K quads/sec sustained, flat across N=1k…100k). The library overhead vs hand-rolled SQLite at the same schema is **~2%**. The 2× vs the simple raw schema is the cost of the four secondary indexes that make `Pattern` reads fast in any direction. **Pebble at 100k rows: ~3 µs/quad** (5.5× faster on Linux gp3).
- **Single-quad `Writer.Commit`: ~108 µs audited on SQLite [SQLite-only], ~60 µs with `Batch.NoAudit: true`.** The audit cost is the `commits` + `commit_ops` rows — the primary reason `Writer.Commit` is slower than raw `INSERT`. **Pebble: ~5.95 µs on M1 / ~9.6 µs on Linux** at the same audit semantics — 18-40× faster.
- **Subject lookup on SQLite: ~69 µs** for ~100 returned rows; non-subject reads (predicate-only, object-only, label-only) walk a different index and pay for the secondary B-trees they're routed through. **Pebble: ~22.7 µs on M1 / ~68 µs on Linux** for the same query — 3× faster on M1, comparable on Linux because gp3's read path is not Bloom-bound at this size.
- **Storage on SQLite: ~444 bytes/quad [SQLite-only]** measured on a 133M-quad / 60 GB SecDek-class corpus. Subjects, predicates, objects, and labels are all stored as `TEXT` verbatim. No string interning, no predicate dictionary, no value compression. **Pebble at production scale: ~10× smaller** (28 GB SecDek SQLite snapshot → ~3 GB Pebble dir after default zstd block compression).
- **BulkLoader index rebuild on `Close` dominates large loads. [SQLite-only.]** At 100k rows it's ~22% of total bulk-load time per CPU profile; at 100M+ rows the rebuild can run for tens of minutes. We rebuild three secondary indexes (`idx_pos`, `idx_osp`, `idx_lsp`) in series — there's no parallel-build path in `modernc/sqlite`. **Pebble has no rebuild step** — sstables are sorted on write.
- **`OpenPartitioned` reads fan out by default. [SQLite-only.]** A `Pattern` with no `Label` and no `RoutePattern` callback queries every partition and merges the resulting iterators. Order across partitions is unspecified; per-partition order is sorted. Cross-partition reads are linear in the number of partitions. (Pebble runs single-dir; partitioning is on the roadmap.)

## Sharp edges (you will hit these)

These are the operational gotchas we've watched real users (us) walk into. None are bugs; all are surprising the first time.

- **`BulkLoader` holds the writer slot for its entire lifetime. [SQLite-only.]** From `BulkLoader(ctx)` to `Close()`, no other Writer or BulkLoader on that partition can make progress. A long load (30+ minutes on multi-million-row corpora) blocks every other writer until done. If you need concurrent writes during a long load, partition the corpus across files and route differently-prefixed labels to separate partitions. **Pebble has no writer slot** — concurrent BulkLoaders are fine.
- **`BulkLoader` flips PRAGMAs and restores them on `Close`. [SQLite-only.]** During a load: `synchronous=OFF`, `journal_mode=MEMORY`, `cache_size=-2000000` (2 GB), `temp_store=MEMORY`. A crash mid-load loses the in-flight pages but the file remains openable. The implication: **don't run a `BulkLoader` against a database you also need to query under WAL semantics in the same process** — readers may see inconsistent state during the load. Restart the process or wait for `Close` before resuming queries.
- **Cross-partition `Batch` writes are rejected. [SQLite-only — partitioning is SQLite-only today.]** A `Writer.Commit` whose `Adds` route to two different partitions returns `ErrCrossPartitionBatch`. The library does not pretend SQLite can give you a multi-file transaction. Caller splits by partition and commits each separately.
- **`Migrate` to N partitions allocates ~2 GB SQLite page cache per destination. [SQLite-only.]** A 2-destination migration of a 28 GB source observed ~7.4 GB peak RSS in production. This is not a memory leak; it's the BulkLoader's `cache_size = -2000000` PRAGMA times the number of destinations open simultaneously. **There is currently no knob to lower this.** Run migrations on hosts with at least 8 GB free per destination partition.
- **`BulkLoader.Add` silently drops duplicate quads.** A quad that already exists at `(subject, predicate, object, label)` is treated as a no-op (SQLite uses `INSERT OR IGNORE`; Pebble uses key-existence semantics). Your only signal is `BulkStats.Added < BulkStats.Attempted`. If you need "fail loudly on duplicates," `BulkLoader` is not the right path; use `Writer.Commit` and check `RowsAffected` (today this is awkward — see open issue below).
- **`v0.x` may break.** The public API is stabilizing but not frozen. Breaking changes between minor versions are possible until `v1.0.0`; `CHANGELOG.md` calls each one out explicitly. Pin the version, read the changelog before bumping.
- **Pure-Go SQLite is not bug-for-bug identical to libsqlite3. [SQLite-only.]** `modernc.org/sqlite` is excellent and well-maintained, but extreme edge cases (some PRAGMA combinations, very-recent SQLite features, certain virtual-table extensions) may behave differently or not be available. We've never hit it in production but it's possible.

## Pebble-only sharp edges

These apply when you call `quadstore.OpenPebble(path)`.

- **No `sqlite3`-CLI escape hatch.** Pebble's sstable + WAL format has no general-purpose ad-hoc query tool. If your operators are used to opening the data file in `sqlite3` or `litecli` for one-off inspection, that workflow goes away. The replacement is calling `Reader.Find` from a Go program. We may ship a `cmd/quadstore-inspect` REPL when this becomes painful.
- **`Match` and `Path` are not implemented yet.** The legacy `*Iterator` `Match` API and the Cayley-style traversal helpers (`From`, `Out`, `In`, `Has`, `Unique`) are SQLite-only today. `Reader.Find` with `iter.Seq2[Quad, error]` is the modern equivalent and works on both backends.
- **`OpenPartitioned` is not implemented yet.** Pebble runs single-dir today. If your workload needs the partitioning features (LabelRouter, fan-out reads, `WriterFor`), use the SQLite backend until Pebble partitioning lands.
- **`MigrateFromSnapshot` is SQLite-source-only.** The snapshot path uses SQLite's `VACUUM INTO` for a consistent point-in-time copy. There is no Pebble equivalent yet — Pebble snapshots use the engine's internal snapshot API, which we haven't exposed at the `quadstore` surface.
- **~20 transitive dependencies.** Pebble pulls in `cockroachdb/*` (errors, redact, swiss, crlib, logtags, tokenbucket, pebble/v2), `getsentry/sentry-go`, `prometheus/client_golang`, `klauspost/compress`, `golang/snappy`, and their transitives. All permissive licenses, all audited in [`docs/LICENSE_AUDIT.md`](./LICENSE_AUDIT.md), all rechecked quarterly. If transitive-dep size matters more than the perf wins, use the SQLite backend.
- **Larger compiled binaries.** ~30 MB more on Linux vs the SQLite-only build, driven by Pebble's transitive deps.

## Open issues (we know about, haven't fixed)

These are honest known problems, not nice-to-haves. Each is a candidate for a PR if you want to help.

- **`bulkBatchRows = 500` is hardcoded.** It's a defensible default (500 × 4 columns = 2,000 SQL params, well under `SQLITE_MAX_VARIABLE_NUMBER`'s 32,766) but workloads with smaller-than-500 quads-per-second arrival rates pay an unnecessary buffering delay. Should be a `BulkLoaderOpts` field. Not yet.
- **`Migrate` peak memory is ~2 GB × destination partitions.** Caused by the BulkLoader's `cache_size` PRAGMA, not held data. A `BulkLoaderOpts.CacheSize` knob would let migration tooling lower this; we haven't built it.
- **No string interning / predicate dictionary.** Subjects and objects are typically high-cardinality; predicates are typically low-cardinality (production observation: 140 distinct predicates across 133M rows). Storing predicates as TEXT verbatim costs ~2 GB on a 133M-row corpus that could be ~few MB. We have not added a `predicates(id, value)` lookup table or `quads.predicate_id INTEGER` column.
- **No value compression.** SQLite supports zstd via extensions (sqlean's `compress`, `sqlite-zstd`). For the predicate-and-object content typical of a graph store this is plausibly 3-5× on disk; we have not integrated either.
- **All values are TEXT.** Numeric predicates (timestamps, scores, counts, percentages) round-trip through `strconv.ParseFloat` on every read. There is no typed-object mode.
- **No streaming change-data-capture.** The `commits` + `commit_ops` tables hold a journal of every write, but there's no exposed iterator for "tail the commit log." A consumer wanting "tell me about new writes since commit X" must poll the commits table directly.
- **`tx.StmtContext` regresses Batch1k by ~14% under modernc.** We tried caching prepared statements on the partition connection and rebinding via `tx.StmtContext` in `Writer.Commit`. Single-commit got ~9% faster but batched writes got significantly slower. Reverted. Worth revisiting if `modernc/sqlite` changes its prepared-statement caching internals.

## What we will not add

Stated explicitly so issue triage stays honest. PRs implementing any of these will be politely declined.

- **No clustering / sharding / replication.** This is a single-machine library. Distributed graphs are a different category of system; we are not entering it.
- **No query language compiler.** No Cypher, Gizmo, GraphQL, SPARQL. The Go API *is* the query API.
- **No server mode.** No HTTP endpoint, no gRPC, no admin port. If you need a service, build one around the library.
- **No graph-algorithm primitives.** No PageRank, shortest-path, community detection, centrality. quadstore is for storing and pattern-matching quads with provenance. Algorithm code is downstream of that, in your application.
- **No schema-migration framework.** The `meta:schema-version` row exists; the library handles its own migrations between schema versions internally. We will not become a generic schema-migration tool for *your* derived data.
- **No backwards-compatibility shims for renamed APIs pre-v1.0.0.** Breaking changes are called out in the CHANGELOG and you bump.

## Empty-promise check

If we ever stop being honest about this list, we have failed.

- **Has every "open issue" been verified to still be open?** If you find one that's been quietly fixed, please open a PR removing it.
- **Has every "performance ceiling" number been reproduced recently?** Numbers age. If you reproduce different numbers on hardware that ought to be comparable, please open an issue with your `go test -bench` output.
- **Have any new sharp edges shown up in production?** If so, they belong here. Please open an issue.
