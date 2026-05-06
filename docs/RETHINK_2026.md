# If we were building quadstore today

A rigorous self-audit, written 2026-05-05 by the maintainer. Not a roadmap commitment — a pre-mortem on the current shape, separating the parts that have aged well from the parts we'd change if we started over today.

The point of this document: if any of the changes here would be worth measuring, name them. Keep this honest and let the numbers decide.

> **Update — 2026-05-06.** §1 below ("Storage engine: Pebble, not SQLite") has shipped. The Pebble backend is recommended via `quadstore.OpenPebble(path)`; measured numbers and parity status live in [`PEBBLE_VS_SQLITE.md`](./PEBBLE_VS_SQLITE.md). §2-§6 remain forward-looking work that has not yet landed.

## What we'd keep

These are the choices that have aged well in production. We would make them again.

- **The label namespace.** `source:` / `derived:` / `human:{tenant}/` / `meta:` enforced at write time was the central insight. Every other "should I do X?" question reduces to "what's the label?" and falls out for free. Keep.
- **Embedded library, single binary.** No server, no port, no auth surface. The smallest correct deployment. Keep.
- **Pure Go, no CGo.** Cross-compiles trivially, runs on Lambda + distroless without ceremony. The day-one constraint of "ship a binary, no toolchain dance" was the single best engineering decision in the project — `modernc.org/sqlite` for the SQLite path, Pebble itself for the LSM path, both pure Go. Keep.
- **Reader / Writer / Batch / BulkLoader API surface.** Audit shows ~2% Go overhead vs hand-rolled at the same schema. The interface was right; it was designed to outlive its first backend. Keep.
- **`iter.Seq2[Quad, error]` reads.** Range-over-func is the right shape for stream-shaped reads. Keep.
- **Provenance / audit as a write-time invariant** rather than a separate observability tool. Keep, with the new `Batch.NoAudit` opt-out for hot paths.
- **Single-machine scope.** No clustering, no consensus protocol, no replication. The right scope. Keep.

## What we'd change

These are the parts where, with the benefit of running it in production, we would pick differently.

### 1. Storage engine: Pebble, not SQLite — ✅ shipped v0.2

**The single most consequential change we made.**

SQLite is a phenomenal piece of software. We were using it as a key-value store with a B-tree index manager bolted on top, paying for SQL we never invoked. The structural costs in [`docs/LIMITATIONS.md`](./LIMITATIONS.md) all came from the same place: single-writer per file, B-tree secondary indexes that need a `DROP / load / CREATE INDEX` dance on bulk loads, `INSERT OR IGNORE` paying a UNIQUE check on every write, no parallelism on index build (`modernc/sqlite` doesn't support it), all values `TEXT` with no native typed storage.

Most of those vanish on a log-structured merge backend. **[Pebble](https://github.com/cockroachdb/pebble) shipped as `quadstore.OpenPebble(path)` in v0.2.** Pure-Go (no CGo), Apache 2.0 / BSD-3, the same Reader / Writer / BulkLoader / `LabelCounts` / `Stats` / `CommitStatsAt` surface as the SQLite backend. Cross-backend migration via `quadstore.MigrateToPebble`.

**Architecture as built.** A quadstore is naturally four keyspaces, one per index direction. Pebble exposes them as four key prefixes inside one DB:

```
spo: <subject>\0<predicate>\0<object>\0<label>  → ε
pos: <predicate>\0<object>\0<subject>\0<label>  → ε
osp: <object>\0<subject>\0<predicate>\0<label>  → ε
lsp: <label>\0<subject>\0<predicate>\0<object>  → ε
```

Empty values, zero-byte separators, prefix scans for the natural query shapes. A `Pattern` lookup picks the keyspace by bound prefix and does a single iterator. The audit trail (`commits` + `commit_ops`) is mirrored as additional keyspaces (`'c' | commitID` and `'C' | commitID | seq`) with the same logical semantics as the SQLite tables.

**Measured wins (full breakdown in [`PEBBLE_VS_SQLITE.md`](./PEBBLE_VS_SQLITE.md)):**

- Single-quad audited Commit on cloud Linux (gp3 EBS): **9.6 µs Pebble vs 384 µs SQLite — 40× faster.** On M1: 5.95 µs vs 107 µs — 18× faster.
- 1k-quad batch commit: **2.1× faster on M1, 4.5× faster on Linux.**
- 100k-quad bulk load: **2.5× faster on M1, 5.5× faster on Linux.** The "sstables are sorted on write, no rebuild step" advantage is a clean ~22% of total cost on the SQLite path.
- Find by subject (~100 rows from 10k): **3× faster on M1.**
- On-disk size: **≈10×** smaller at production scale — a 19,176,859-quad SecDek snapshot went from 28 GB SQLite → ~3 GB Pebble dir, default zstd block compression doing the work.
- Real-data round-trip: 19M-quad migration produced byte-identical subjects-hash and predicates-hash; 200 random subject point queries with zero mismatches.

**The expected 10-20 µs single-commit target landed under it** — 5.95 µs on M1, 9.6 µs on Linux. The 4-6× improvement we forecast turned out to be 18-40×. The cloud-disk delta widening past the M1 numbers is the part we couldn't have predicted from a laptop.

**Honest tradeoffs (still as forecast):**

- We own the index management — `pebble_store.go` is ~1k lines of multi-keyspace `Apply` + iterator-merge for `Pattern` reads. Forecast was 500-1000 lines; landed in that range.
- No SQL escape hatch. `sqlite3 quads.db` is genuinely useful for ad-hoc inspection. We have not yet built `cmd/quadstore-inspect`. Open work.
- Read amplification on LSM. Pebble's defaults handle most of it; we have not yet had to tune compaction policy or Bloom filters in production.
- Tooling story changes. No `.dump`, no `EXPLAIN QUERY PLAN`. Acceptable for now; will be papered over by a `cmd/quadstore-inspect` REPL when operators ask.

**SQLite stays supported.** `quadstore.Open(path)` is the legacy backend, kept for callers who want ~20 fewer transitive deps, smaller binaries, or `sqlite3`-CLI access on the data file. See [`README.md`](../README.md) "Why use the SQLite backend?" for the criteria. Whether `Open()` flips its default backend at v1.0 is open — see [`PEBBLE_VS_SQLITE.md`](./PEBBLE_VS_SQLITE.md) §Decision.

**Parity gaps still open on `*PebbleStore`** (will be added on user request): legacy `*Iterator` `Match`, Cayley-style `Path` traversal helpers (`From`/`Out`/`In`/`Has`/`Unique`), `OpenPartitioned`. Listed in [`LIMITATIONS.md`](./LIMITATIONS.md) "Pebble-only sharp edges."

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

A redesign isn't worth doing on intuition. Three measurements would change our minds.

> **Status — 2026-05-06.** Tests 1 and 2 have been run. Test 1's
> rule was "ship the rewrite if Pebble wins on at least 3 of 5
> metrics." Pebble won 5 of 6 on M1 and 6 of 6 on Linux gp3 —
> documented in [`PEBBLE_VS_SQLITE.md`](./PEBBLE_VS_SQLITE.md).
> Test 2's cloud-Linux numbers landed in the same doc. Test 3's
> storage-density question is partially answered by the real-data
> 19M-quad SecDek round-trip (28 GB → ~3 GB, ≈10× compression);
> the predicate-dictionary alone-and-stacked measurements are
> still open.

### Test 1: Pebble vs SQLite, head-to-head, same API — ✅ satisfied

Build a `quadstore-pebble` package behind the same `Store` / `Reader` / `Writer` / `BulkLoader` interfaces. Run identical workloads against both:

| metric | M1 Pro target | Linux/EC2 target |
|---|---|---|
| `BulkLoad_1M` | < 5 s vs SQLite's ~75 s | < 8 s vs SQLite's ~110 s |
| `Commit_SingleQuad` (audited) | < 30 µs vs ~108 µs | < 50 µs vs ~140 µs |
| `Find_BySubject_100rows` | < 50 µs vs ~69 µs | < 80 µs vs ~95 µs |
| `ConcurrentWriters_8x` | scales linearly | scales linearly |
| `OnDisk_100M_quads` | ~50% of SQLite size | ~50% of SQLite size |

If Pebble wins on **at least three** of those, we ship the rewrite. If it loses on more than one, we don't.

### Test 2: M1 Pro vs cloud Linux — ✅ satisfied

The original PERFORMANCE.md numbers were M1 Pro only. M1 has unrealistically fast SSDs and aggressive thermal throttling under sustained load. Production deployments are typically Linux on cloud instances. We need numbers from there.

A `t4g.large` (4 vCPU / 8 GB / gp3 EBS) gives us:

- Real fsync latency (gp3 is ~ms, M1 SSD is ~tens-of-µs)
- Real thermal-stable throughput (no throttle)
- Real CPU-per-core (M1 P-cores are unusually fast)
- The deployment shape that actually matches SecDek production

We'd run the **same** benchmark suite on both and publish the deltas. The library shouldn't behave qualitatively differently on cloud Linux, but the *numbers* will. Today's PERFORMANCE.md is M1-only and that's a small honesty gap.

### Test 3: Storage density at scale — partially satisfied

Real-data 19M-quad SecDek round-trip showed ≈10× compression (28 GB SQLite → ~3 GB Pebble), default zstd block compression doing the work. The predicate-dictionary alone-and-stacked measurements below are still open — they're the path to making §2 ("Predicate dictionary from day one") a measured decision rather than an intuition.

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
