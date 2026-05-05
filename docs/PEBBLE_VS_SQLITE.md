# Pebble vs SQLite: head-to-head benchmark

Prototype comparison using identical workloads against two backends:

- **SQLite-backed quadstore** (current production library, with the
  recent `Batch.NoAudit` and conditional-prepare optimizations).
- **Pebble-backed prototype** at `internal/pebbleq/`. Same four-key
  encoding (SPO/POS/OSP/LSP keyspaces, NUL-separated), default
  Pebble options other than a quiet logger.

Both backends use **lazy-fsync durability** (SQLite `synchronous=NORMAL`,
Pebble `pebble.NoSync`). This is the apples-to-apples comparison;
strict per-commit durability would change Pebble's single-commit number
by ~1000× (see "Durability matters" below).

Status: M1 Pro / darwin-arm64 numbers only. Cloud Linux numbers
pending — see [`RETHINK_TEST_PLAN.md`](./RETHINK_TEST_PLAN.md) Test 2.

## The headline

Pebble wins **4 of 5** decision-gate metrics on M1 Pro at the
benchmark scales we tested:

| operation | SQLite audited | SQLite NoAudit | raw modernc-SQLite | **Pebble (NoSync)** | Pebble vs best SQLite |
|---|---|---|---|---|---|
| Single-quad commit | 106 µs | 59 µs | 25 µs | **3.4 µs** | **17× faster** than NoAudit |
| 1,000-quad batch commit | 13.1 ms | — | — | **4.5 ms** | **2.9× faster** |
| Find by subject (~100 rows from 10k) | 68.5 µs | — | 90.6 µs | **10.9 µs** | **6.3× faster** |
| Bulk load 1k | 6.9 ms | — | 2.5 ms | 41.6 ms | Pebble **6× slower** |
| Bulk load 10k | 72 ms | — | 32 ms | 87 ms | roughly even (1.2× slower) |
| Bulk load 100k | 762 ms | — | 351 ms | **306 ms** | **2.5× faster** |

### Where each engine shines

**Pebble dominates on:**

- **Point operations.** A single-quad `Writer.Commit` on Pebble takes
  3.4 µs. SQLite needs 59-106 µs for the same write, depending on
  whether the audit trail is on. The 17-31× gap is the cost of
  SQLite's `BeginTx → INSERT → INSERT (audit) → COMMIT` versus
  Pebble's `WriteBatch.Apply` to four keyspaces in a single in-memory
  skiplist insert + WAL append.
- **Subject-prefix lookups.** Pebble's per-sstable Bloom filters mean
  point-shaped reads skip most sstables entirely. Result: 10.9 µs vs
  SQLite's 68.5 µs — a 6× speedup despite quadstore's index layout
  being deliberately tuned for subject scans.
- **Large bulk loads.** At N=100k, Pebble's LSM "sstables are sorted
  on write, no rebuild needed" advantage wins out: 306 ms vs SQLite's
  762 ms (which spends ~22% of that time rebuilding three secondary
  indexes on `Close`).

**SQLite wins on:**

- **Small bulk loads.** At N=1k, Pebble's final `Flush()` (memtable →
  sstable, fsync) has fixed setup overhead that doesn't amortize.
  SQLite's BulkLoader on 1k rows finishes in ~7 ms; Pebble in ~42 ms.
  The crossover sits between N=10k and N=100k.

For the SecDek-class workload (single-machine, multi-million-row
ingest, point-shaped reads) the wins all line up where they matter:
fast single commits, fast subject lookups, fast large bulk loads.

## Why the deltas exist

Reading the SQLite path and the Pebble path side-by-side explains every
number above.

### Single-quad Commit

SQLite (audited):

1. Acquire writer slot.
2. `BEGIN TRANSACTION`.
3. `INSERT INTO commits (id, ts, label, metadata) VALUES (?,?,?,?)`.
4. `INSERT OR IGNORE INTO quads (s,p,o,l) VALUES (?,?,?,?)`.
5. `INSERT INTO commit_ops (cid, op, s,p,o,l) VALUES (?,?,?,?,?,?)`.
6. `COMMIT` → write WAL frame, fsync if at checkpoint.

Five SQL operations, two prepared statements, one B-tree maintenance
walk per index. ~106 µs.

Pebble:

1. `db.NewBatch()`.
2. Four `wb.Set(key, nil)` calls (one per keyspace).
3. `wb.Commit(NoSync)` → append to WAL, insert into memtable skiplist.

No prepares (Pebble doesn't have them — keys are bytes). No B-trees
(memtable is a skiplist; sstables are sorted-on-write). No audit
table. ~3.4 µs.

The structural cost SQLite pays — and Pebble doesn't — is the
relational ceremony around what is fundamentally a four-key-puts
operation. Pebble exposes the four-key-puts operation directly.

### Find by subject

SQLite walks `idx_spo` for the subject prefix. Per-row B-tree page
fetches; ~68 µs for ~100 rows.

Pebble does a prefix iterator on the SPO keyspace. The Bloom filter
on each sstable says "this subject definitely isn't in me, skip this
file." Most sstables are skipped. ~11 µs for the same ~100 rows.

The Bloom filter advantage grows as the corpus grows. At 100 M rows
across 100s of sstables, Pebble's win on point lookups should *widen*,
not narrow.

### Bulk load

SQLite BulkLoader: drop 3 indexes → multi-row INSERT in 500-row
batches → re-CREATE 3 indexes at Close. The CREATE INDEX phase
external-sorts the entire table; that's the ~22% of CPU we measured
in profiles.

Pebble BulkLoader: WriteBatch the four-key writes per quad, commit
NoSync each 500 quads, final `db.Flush()` to force memtable → sstable
+ fsync. **No rebuild step.** sstables are written sorted because
the keyspaces are sorted by definition.

At small N (1k rows), Pebble's `Flush()` has fixed overhead that
SQLite's tiny re-CREATE doesn't pay. At large N (100k+), Pebble's
"no rebuild" advantage compounds.

## Durability matters (and is the easiest mistake to make)

Our first run of these benchmarks showed Pebble's single-commit at
**4.5 ms** — 41× *slower* than SQLite. That was a fairness bug. Pebble
defaults the prototype to `pebble.Sync` which fsyncs the WAL on every
Commit. SQLite's default is `synchronous=NORMAL` which fsyncs the WAL
*periodically*, not per Commit. With identical durability levels:

| operation | Pebble Sync (fsync per Commit) | Pebble NoSync (lazy) |
|---|---|---|
| Single-quad commit | 4500 µs | 3.4 µs |

Three orders of magnitude. The fsync-per-commit semantic is what most
"real" durability looks like (Postgres, MySQL with `innodb_flush_log_at_trx_commit=1`).
SQLite's NORMAL mode is the lazy default that everyone uses anyway.
Comparing across durability levels is comparing different products.

The prototype defaults to `NoSync` to match SQLite's default. A
`Writer.CommitSync` exists for callers who need strict per-commit
durability. Production use would expose the choice as a `BatchOptions`
field.

## What this benchmark does NOT prove

Honesty list before anyone gets excited:

- **M1 Pro is not production hardware.** Apple's NVMe SSD has
  fsync latencies in the tens of microseconds. Cloud disks (AWS gp3,
  GCP pd-ssd) are typically 100s of microseconds to single-digit
  milliseconds. The bench numbers will move on Linux. Specifically,
  Pebble's bulk-load advantage at large N may *grow* (the Flush
  fsync hurts SQLite WAL more on slow disks) but its small-N
  disadvantage may also *grow*.
- **Synthetic data.** Subjects are `s%d-%d`, predicates and objects
  are constants. Real corpora have higher predicate cardinality,
  longer string values, more variable subject distribution. Storage
  density and Bloom filter effectiveness will both look different.
- **No concurrent-writers test yet.** The single biggest architectural
  win Pebble offers is "no single-writer ceiling." We haven't
  benchmarked it.
- **The Pebble prototype omits real features.** No audit trail, no
  partition routing, no label namespace validation. Adding them
  would close some of the gap on Pebble's side. Production estimate:
  ~10-20% slower than the prototype, still decisively faster than
  SQLite for the wins above.
- **No 100M-row dataset.** All benches top out at 100k. The full
  shape of the load curve at production scale is unmeasured.

## Reproduce

Bench file: `bench_pebble_test.go`. Pebble prototype: `internal/pebbleq/`.

```sh
go test -bench='BenchmarkPebble|BenchmarkCommit_SingleQuad$|BenchmarkCommit_SingleQuad_NoAudit|BenchmarkCommit_Batch1k|BenchmarkFind_BySubject|BenchmarkCompare_RawSQLite_BulkLoad|BenchmarkCompare_Quadstore_BulkLoader|BenchmarkCompare_RawSQLite_SingleInsert' \
   -benchtime=2s -run=^$ ./...
```

Above are stderr-clean (`grep -E "^Benchmark"` for the report-friendly
output).

## Decision

Per [`RETHINK_TEST_PLAN.md`](./RETHINK_TEST_PLAN.md) Test 1's rule of
"Pebble wins on at least 3 of 5 metrics," Pebble wins on M1 Pro.
**4 of 5.** This is sufficient signal to:

1. Run the same suite on cloud Linux (`t4g.large`, gp3 EBS) to
   confirm the deltas survive a real-disk fsync profile.
2. If Linux confirms, begin a production-shaped Pebble port: add
   audit trail, partition routing, label namespace validation, and
   a migration path from existing SQLite-backed stores.
3. Land Pebble as a v0.2.x feature gated behind `OpenPebble(...)`
   alongside the existing `Open(...)`. SQLite-backed Stores keep
   working unchanged.

This is **not** a recommendation to break existing deployments. The
SQLite backend stays as the default for several minor versions; we
publish the comparison numbers and let users opt in.
