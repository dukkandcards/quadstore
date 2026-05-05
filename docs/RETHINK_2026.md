# If we were building quadstore today

A rigorous self-audit, written 2026-05-05 by the maintainer. Not a roadmap commitment — a pre-mortem on the current shape, separating the parts that have aged well from the parts we'd change if we started over today.

The point of this document: if any of the changes here would be worth measuring, name them. Keep this honest and let the numbers decide.

## What we'd keep

These are the choices that have aged well in production. We would make them again.

- **The label namespace.** `source:` / `derived:` / `human:{tenant}/` / `meta:` enforced at write time was the central insight. Every other "should I do X?" question reduces to "what's the label?" and falls out for free. Keep.
- **Embedded library, single binary.** No server, no port, no auth surface. The smallest correct deployment. Keep.
- **Pure Go, no CGo.** Cross-compiles trivially, runs on Lambda + distroless without ceremony. The day-one dependency on `modernc.org/sqlite` is the single best engineering decision in the project. Keep.
- **Reader / Writer / Batch / BulkLoader API surface.** Audit shows ~2% Go overhead vs hand-rolled at the same schema. The interface was right; it was designed to outlive its first backend. Keep.
- **`iter.Seq2[Quad, error]` reads.** Range-over-func is the right shape for stream-shaped reads. Keep.
- **Provenance / audit as a write-time invariant** rather than a separate observability tool. Keep, with the new `Batch.NoAudit` opt-out for hot paths.
- **Single-machine scope.** No clustering, no consensus protocol, no replication. The right scope. Keep.

## What we'd change

These are the parts where, with the benefit of running it in production, we would pick differently.

### 1. Storage engine: Pebble, not SQLite

**The single most consequential change we'd make.**

SQLite is a phenomenal piece of software. We use it as a key-value store with a B-tree index manager bolted on top, and we pay for SQL we never invoke. The structural costs that show up in [`docs/LIMITATIONS.md`](./LIMITATIONS.md) all come from the same place:

- Single-writer per file (SQLite hard rule).
- Secondary indexes are B-trees; bulk loading them requires a `DROP / load / CREATE INDEX` dance whose `CREATE INDEX` step dominates large loads (>30 min observed at 157M rows).
- `INSERT OR IGNORE` is a UNIQUE constraint check on every write.
- No parallelism on index build — `modernc/sqlite` doesn't support it.
- All values are `TEXT`; no native typed storage.

Most of those vanish on a log-structured merge (LSM) backend. [Pebble](https://github.com/cockroachdb/pebble) (CockroachDB's storage engine, pure-Go, Apache 2.0) was built to host a transactional SQL database; it gives us:

- **No single-writer ceiling.** MVCC + concurrent batchers. Two goroutines streaming quads from different sources land in parallel.
- **No index rebuild step.** sstables are append-only and sorted at write time. There is nothing to rebuild on close.
- **Bloom filters per sstable.** Point lookups (`Find` by full quad) are cheap even with millions of sstables.
- **Built-in zstd block compression.** ~3-5× on-disk reduction for typical predicate/object content.
- **Pure Go.** Matches our deploy-as-a-binary promise.

**What we'd build on top of Pebble:**

A quadstore is naturally four keyspaces, one per index direction. Pebble gives us four CFs (column families) trivially:

```
spo: <subject>\0<predicate>\0<object>\0<label>  → ε
pos: <predicate>\0<object>\0<subject>\0<label>  → ε
osp: <object>\0<subject>\0<predicate>\0<label>  → ε
lsp: <label>\0<subject>\0<predicate>\0<object>  → ε
```

Empty values, zero-byte separators (or varint-prefixed lengths), prefix scans for the natural query shapes. A `Pattern` lookup picks the keyspace by the bound prefix and does a single iterator. No JOIN, no query planner, no surprises.

A `Writer.Commit` is now a single `Pebble.Apply(WriteBatch)` of 4 puts per quad. No prepare, no fsync per write (Pebble flushes WAL on commit), no index rebuild. The expected single-quad commit cost is on the order of **10-20 µs** including audit rows — a 4-6× improvement on the current 60-108 µs.

A `BulkLoader` becomes "merge sorted batches into sstables." The 22% index-rebuild tax in our current profile goes to zero. Concurrent BulkLoaders against different keyspaces run truly in parallel. A 10 M quad load that takes ~75 seconds today would land in the **5-15 second range**.

**Honest tradeoffs:**

- We own the index management. Today SQLite manages it; tomorrow we write the multi-keyspace `Apply` + the iterator-merge for `Pattern` reads. That's ~500-1000 lines of new code.
- No SQL escape hatch. `sqlite3 quads.db` is genuinely useful for ad-hoc inspection. Replacement: a `cmd/quadq` REPL that takes a `Pattern` and prints rows.
- Read amplification is real on LSM. Without Bloom filters tuned + compaction policy tuned, point lookups can touch many sstables. Pebble handles most of this; we'd need to tune the rest.
- Tooling story changes. No `.dump`, no `EXPLAIN QUERY PLAN`. We'd need a `cmd/quadstore-inspect` for compaction stats, sstable-level info.

We would still ship a SQLite *export* target for compatibility — a `Migrate` destination that materializes the Pebble store as SQLite for users who want SQL escape hatch on a snapshot.

### 2. Predicate dictionary from day one

In production we observed **~140 distinct predicates across 133 M rows** (SecDek corpus). Storing predicate strings verbatim is paying ~20 bytes × 133 M = ~2.5 GB just to repeat the same ~140 strings.

A `predicates(id INTEGER, value TEXT)` lookup table + `quads.predicate_id INTEGER` column would be a **10-20× compression on the predicate column alone**. The cost is one extra hashmap lookup at write, one extra join (or in-memory cache) at read.

Today this is opt-out cost. With v0.1.x not yet stable, we'd ship it as the default. It's the kind of thing where there's no good time to add it later — the migration cost grows as the corpus grows.

### 3. Typed objects

Today every object is `TEXT`. Numeric predicates (`scheduled-at = "2026-05-01"`, `score = "0.87"`, `count = "42"`) round-trip through `strconv.ParseFloat` per read. Range queries on numeric predicates require Go-side filtering; SQLite can't index over typed values it doesn't know are typed.

A typed-object schema would carry the type tag with the value:

```
predicate "scheduled-at" → object_type=time, object_int=<unix_seconds>
predicate "works-at"     → object_type=ref,  object_text=<iri>
predicate "score"        → object_type=float, object_real=<float64>
```

This is straightforwardly faster for typed reads and enables SQL-side range queries. The cost is API: callers have to declare predicate-type bindings. We'd ship it as opt-in: untyped predicates keep working as text.

### 4. NoAudit as the default

Looking at production usage: most writes are bulk ingest pipelines that land via `BulkLoader` (no audit), or batched derivation runs that produce 10K+ quads per commit (where the audit row is a rounding error). The single-quad audited Commit is mostly used in tests.

If we were starting today, we'd flip the default. `Batch{Adds: ...}` would NOT write audit rows; `Batch{Adds: ..., Audited: true}` would opt in. This is a v1.0 breaking change; can't ship it before then.

### 5. CDC subscription API

Today, "tell me when new data lands" is "poll the commits table." That's fine for cron-shaped consumers and wrong for everything else.

A first-class `Subscribe(ctx, predicate, handler)` that fires on each Commit (filterable by label namespace) would let derived-fact regeneration pipelines stop polling. Implementation: the writer-slot holder wakes the subscriber chain on `tx.Commit()`. No polling, no commits-table scan, no missed events.

This composes cleanly with `derived:*` regeneration: a subscriber sees new `source:*` rows, regenerates `derived:*` for the affected subjects, ships. The whole "incremental processing" doc becomes a one-line example.

### 6. Sorted-batch BulkLoader

Today `BulkLoader.Add` buffers to a 500-quad slice and flushes a multi-row INSERT in arrival order. SQLite's index build at Close has to externally sort the entire table.

If the producer batches were pre-sorted by `(subject, predicate, object)`, the index build's external sort collapses to a near-linear merge. For ingest pipelines that decode JSON (where you can sort cheaply on the producer), this is a free 2-3× speedup on large loads.

This is purely a producer-side improvement; the API surface doesn't need to change. We'd add `BulkLoader.AddSorted(quads []Quad)` that documents the requirement and trusts the caller.

## What we'd test before committing

A redesign isn't worth doing on intuition. Three measurements would change our minds:

### Test 1: Pebble vs SQLite, head-to-head, same API

Build a `quadstore-pebble` package behind the same `Store` / `Reader` / `Writer` / `BulkLoader` interfaces. Run identical workloads against both:

| metric | M1 Pro target | Linux/EC2 target |
|---|---|---|
| `BulkLoad_1M` | < 5 s vs SQLite's ~75 s | < 8 s vs SQLite's ~110 s |
| `Commit_SingleQuad` (audited) | < 30 µs vs ~108 µs | < 50 µs vs ~140 µs |
| `Find_BySubject_100rows` | < 50 µs vs ~69 µs | < 80 µs vs ~95 µs |
| `ConcurrentWriters_8x` | scales linearly | scales linearly |
| `OnDisk_100M_quads` | ~50% of SQLite size | ~50% of SQLite size |

If Pebble wins on **at least three** of those, we ship the rewrite. If it loses on more than one, we don't.

### Test 2: M1 Pro vs cloud Linux

The current PERFORMANCE.md numbers are M1 Pro. M1 has unrealistically fast SSDs and aggressive thermal throttling under sustained load. Production deployments are typically Linux on cloud instances. We need numbers from there.

A `t4g.large` (4 vCPU / 8 GB / gp3 EBS) gives us:

- Real fsync latency (gp3 is ~ms, M1 SSD is ~tens-of-µs)
- Real thermal-stable throughput (no throttle)
- Real CPU-per-core (M1 P-cores are unusually fast)
- The deployment shape that actually matches SecDek production

We'd run the **same** benchmark suite on both and publish the deltas. The library shouldn't behave qualitatively differently on cloud Linux, but the *numbers* will. Today's PERFORMANCE.md is M1-only and that's a small honesty gap.

### Test 3: Storage density at scale

Build a 100 M-quad synthetic corpus modeled on the SecDek shape (140 distinct predicates, high-cardinality subjects, mixed-type objects). Measure on-disk size for:

- Current SQLite + 4 secondary indexes
- SQLite + predicate dictionary (TEXT → predicate_id INTEGER)
- SQLite + zstd via `sqlean` compression
- Pebble with default zstd block compression
- Pebble + predicate dictionary

The win we expect is **predicate dictionary alone** is 30-50% off, **zstd alone** is 3-5×, and the two together approach 10× off the current ~444 bytes/quad floor.

If predicate dictionary delivers, ship it before v1.0 inside SQLite — it's compatible with the current backend and pays back regardless of any storage-engine swap.

## What we would NOT add (still)

Even after this redesign, the things in [`docs/LIMITATIONS.md`](./LIMITATIONS.md) under "What we will not add" stay out:

- No clustering / sharding / replication. That's still a different system.
- No query language compiler. Pebble doesn't change this.
- No graph algorithms. PageRank is a downstream concern.
- No server mode.

The redesign would make the engine faster and denser. It would not make quadstore a cluster. We are deliberately the largest graph database that fits on one box.

## How to read this document

If a section here matches a problem you have hit using quadstore: open an issue and reference it. We will cite this doc back as "the candidate fix; here's the test we'd want to run before committing."

If you disagree with a tradeoff: open an issue. We are wrong about some of this and won't know which parts until someone tells us.

If you want to run Test 1 / 2 / 3 on a server we don't have access to: open a PR with the bench output. We will publish the numbers and credit you.
