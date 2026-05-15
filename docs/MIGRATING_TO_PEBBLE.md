# Migrating from the SQLite backend to Pebble

A practical guide for moving an existing SQLite-backed quadstore deployment to the Pebble backend. Written from the actual experience of preparing SecDek (live at sfy.io) for cutover; the same shape applies to anyone using `quadstore.Open` who's considering `OpenPebble`.

This is **not** a "you must migrate" doc. The SQLite backend is supported indefinitely. Migrate when the tradeoffs add up for your deployment — and stop when they don't. The guide below is the framework for deciding *and* the mechanics for executing.

> **SecDek cutover status (2026-05-15): LANDED on second attempt.**
> The full sequence — preserved for posterity because the
> rollback-then-retry shape is itself the lesson:
>
> 1. **2026-05-06** first cutover. The two-partition Option-B
>    hypothesis turned out moot — production was always single-file
>    SQLite — so this was just `single-file SQLite → single-Pebble
>    dir`. Cutover landed; first hot-read bench surfaced a
>    structural gap: `Reader.Count(Pattern{Label: X})` was 4-5×
>    *slower* than SQLite's covering-index path (180 ms vs 38 ms).
>    Fix shipped: per-label counter keyspace via Pebble's Merge
>    operator. After the fix, **5,418× faster** than SQLite on the
>    same query (0.01 ms vs 38 ms).
> 2. **2026-05-07 morning** rolled back. The Pebble cutover broke
>    SecDek's pre-cutover multi-binary writer stack — server +
>    5 timer-driven writer binaries each opened the SQLite file
>    via WAL. Pebble holds an *exclusive process file lock* on its
>    data directory: only one process at a time. Rollback to
>    SQLite was a clean env-var flip in ~30 seconds.
> 3. **2026-05-07/08** architectural rebuild: in-process Job
>    scheduler (`internal/scheduler`) inside the server binary;
>    the 5+ writer binaries became goroutines instead of separate
>    processes. The cmd/X binaries remain for operator backfill
>    but no longer hold the Pebble lock concurrently. Documented
>    as the **dek-scheduler pattern** and lifted to the dek
>    architecture rule set.
> 4. **2026-05-08+** second cutover landed incrementally as the
>    Phase A/B corpus loads (speeches, press releases, advisory
>    materials, PCAOB, weekly movers, BrokerCheck resolvers)
>    ran directly into the prod Pebble dir.
> 5. **2026-05-15** current state: production has been on Pebble
>    for a week. ~3.4 GB Pebble dir (vs ~30 GB pre-cutover
>    SQLite), 27 in-process Jobs gated on
>    `SECDEK_SCHEDULER_ENABLED=1`, nightly Checkpoint + tar.gz
>    backup to S3. SQLite file (`/var/lib/secdek/secdek.db`)
>    removed post-soak.
>
> **The lesson worth carrying:** Pebble's single-writer-process
> constraint is binding. Any consumer migrating from a
> multi-writer-process SQLite shape (server + timer binaries +
> backfill CLIs all opening the file) must consolidate writers
> *before* the cutover, not as a recovery step. Three viable
> shapes for the consolidation: in-process scheduler goroutines
> (what SecDek shipped), admin-endpoint writers, or file-feed
> writers. See `feedback_pebble_single_writer` for the binding
> constraint and `project_dek_scheduler_pattern` for the
> consolidation pattern.

## Decide whether to migrate at all

A 30-second self-check. If the **answer to any of these is "no"**, the SQLite backend is probably the right call for now.

| Question | If "no", stay on SQLite |
|---|---|
| Is your binary deployment **persistent compute** (EC2 / VM / container) rather than Lambda / serverless? | Pebble adds ~20 transitive deps + ~30 MB to compiled binary. On Lambda this hurts cold-start budget and zip-size limits; the SQLite path is friendlier. |
| Is your data on **slow-fsync storage** (gp3 EBS, network SSD, spinning disk)? | Pebble's gap over SQLite *widens* on slow disks. On M1 NVMe / local SSD the gap is smaller; the case for migrating is weaker. |
| Are your hot reads **point lookups or subject-prefix scans**? | Pebble's per-sstable Bloom filters dominate here. If you mostly do full-corpus aggregates, the win shrinks. |
| Is your dataset **bigger than ~1 GB**, or growing past it? | The on-disk compression win (~10× on real data) compounds with size. At <1 GB the absolute savings are small. |
| Can you accept the **parity gaps** (see "What's still missing on Pebble" below) for your call sites? | If your code uses `Match`, `Path`, or `OpenPartitioned` heavily, migration requires either staying on SQLite or doing some of the work the library hasn't done yet. |

If you cleared all five: read on.

## What's still missing on Pebble (parity gaps)

As of v0.2-track, four things exist in `*Store` (SQLite) but not yet in `*PebbleStore`:

- **`Match`** (legacy `*Iterator` API). Use `Reader.Find` with `iter.Seq2[Quad, error]` instead — works on both backends and is the modern path. If your code already uses `Reader.Find`, you don't have a problem.
- **`Path` traversal helpers** (`From` / `Out` / `In` / `Has` / `Unique` / `All` / `Count` / `First`). If you do Cayley-style multi-hop traversals from Go, you'll need to either keep SQLite in scope, port the traversal to a `Reader.Find` chain, or wait for Pebble parity.
- **`OpenPartitioned`** (label-routed multi-file Stores with `LabelRouter` / `PatternRouter` / `WriterFor`). See "The partitioning question" below — this is the most common blocker for production migrations and there's a workaround.
- **`MigrateFromSnapshot` from a Pebble source**. Today the snapshot path uses SQLite's `VACUUM INTO`. If your source is SQLite, you're fine; if your source were Pebble, the race-free snapshot migration isn't exposed yet.

If your code calls anything in this list and you can't easily route around it, the migration plan should sequence "library work first, migration second."

## The simple case: single-file SQLite → single Pebble dir

This is what `MigrateToPebble` was built for and what we validated end-to-end on a 19M-quad / 28 GB SecDek production snapshot (byte-perfect, see `docs/PEBBLE_VS_SQLITE.md` §"Real-data validation"). Mechanics:

```go
import "github.com/dukkandcards/quadstore"

src, _ := quadstore.Open("/path/to/old.db")           // SQLite source
defer src.Close()

dst, _ := quadstore.OpenPebble("/path/to/new-pebble") // Pebble destination
defer dst.Close()

stats, err := quadstore.MigrateToPebble(ctx, src, dst, quadstore.MigrateOpts{
    ChunkSize: 10000,        // progress reporting cadence
})
// stats.Quads = number migrated
```

Key behaviors:

- **Source is read-only throughout.** The migration tool never writes to the source. Concurrent writers on the source are tolerated but may produce torn reads — for a quiescent migration, copy the file first or use `MigrateFromSnapshot` (SQLite source only) for `VACUUM INTO`-based race safety.
- **Audit trail does NOT round-trip in v0.2.** The source's `commits` + `commit_ops` tables stay on the SQLite side; the destination's audit starts fresh. If your application keeps long-running provenance state in the audit trail, plan for this.
- **Bulk load throughput on cloud Linux (`t4g.large` / gp3 EBS) was ~20K quads/sec sustained** in the SecDek 19M-quad migration (15 min 36 sec total).
- **Validation is your job after the migration.** The library does not run a correctness sweep automatically. Run one before flipping production traffic — see "Validate before cutover" below.

## The not-simple case: partitioned SQLite

This is the case we discovered while planning the SecDek cutover. **`OpenPartitioned` is not implemented on Pebble yet.** If your production uses partitioned SQLite (SecDek's `main.db` + `corpus.db` shape, for instance), there is no one-line flip.

You have three options. Pick by reasoning, not by reflex.

### Option A — wait for OpenPartitioned-on-Pebble in the library

Cleanest architecturally. The library closes a known parity gap; your migration becomes "swap one constructor, validate, cutover." Cost: real quadstore engineering work (multi-Pebble-dir routing, fan-out reads, partitioned bulk-load, partitioned migration tool). Days, not hours.

Pick this if your migration is not time-sensitive and you'd rather invest in the library than in a one-off solution.

### Option B — consolidate to a single Pebble dir

The insight that makes this attractive: **the reason you partitioned in SQLite probably doesn't apply on Pebble.**

The classic SQLite partitioning rationale is *B-tree dilution*: when one `quads` table accumulates fact families that don't share queries, every full-predicate scan pays the cost of unrelated rows. The fix is to split the families into separate files so each scan touches only the relevant tree.

Pebble doesn't have B-tree dilution as a first-class problem. Each sstable is sorted on write, has its own per-sstable Bloom filter, and compaction naturally segments the keyspace by access pattern as data lands. A `Pattern` lookup on a subject prefix that lives in only some of the sstables will skip the rest via Bloom — *without* the application doing anything.

So: if you partitioned to dodge B-tree dilution, consolidating to a single Pebble dir often gets you the same query performance for free. **Test it before deciding.** Don't rip out partitioning on architectural grounds; rip it out only if measurements show single-Pebble matches or beats partitioned-SQLite on your hot paths.

Cost: hours of measurement, plus a code change in your wrapper (drop the routing logic, hand back a `*PebbleStore`).

### Option C — keep partitioning at the application layer

Open N `*PebbleStore` instances side-by-side. Move your `LabelRouter` / `PatternRouter` logic out of quadstore and into your application's wrapper around it. Reads / writes dispatch externally; cross-partition reads fan out and merge in your code.

Cost: your wrapper grows (the routing layer the library *should* eventually own). Acceptable as a transient state until Option A lands; not great as a permanent shape.

Pick this if your partitioning shape is genuinely load-bearing (you have predicates that produce data on the order of TB and queries that legitimately scope to one partition) and you can't wait for the library work.

### How to choose

The framework, in priority order:

1. **Try Option B first.** If a measured single-Pebble run on your real data is competitive with your current partitioned-SQLite, you're done — consolidate. This is the lowest total cost.
2. **If single-Pebble loses on your hot paths**, try Option C as a transient — keeps you on Pebble while the library catches up.
3. **If your data shape genuinely needs partitioning long-term** (rare at single-machine scale), back Option A by contributing to the library. Match / Path / OpenPartitioned are exactly the parity gaps the project is open about and tracking.

The default failure mode is reflexively choosing C because "we already have routing code." Don't. Measure single-Pebble first.

## Validate before cutover

The SecDek 2026-05-05 round-trip was a clean correctness check. Generalized:

1. **Snapshot the source.** For SQLite, `MigrateFromSnapshot` does `VACUUM INTO` for you. For partitioned SQLite, snapshot each partition separately.
2. **Migrate to the Pebble destination.** Plain `MigrateToPebble` for single-file; if consolidating partitions (Option B), migrate each partition into the same Pebble dir in sequence — `Writer.Commit` is additive, the destination doesn't care that the data arrived in two passes.
3. **Run a correctness sweep.** At minimum:
   - Total `quads` count matches between source and destination.
   - Distinct predicates count matches.
   - SHA-256 of sorted distinct subjects matches.
   - SHA-256 of sorted distinct predicates matches.
   - 200+ random-subject point queries return identical row sets on both sides.
4. **Run a representative read benchmark** against the destination. If your app has hot pages or cron-heavy queries, hit those query shapes and time them.
5. **Compare disk size** to confirm the compression win materialized. SecDek went 28 GB → ~3 GB; if your numbers don't show ~3-10× compression, something's off (look at predicate cardinality and string lengths).

`cmd/pebble-correctness` in the quadstore repo is the reference tool. Copy its shape and adapt.

### How the 200 random point queries work (and what they can / can't catch)

The "200 random subject point queries; 0 mismatches" line in `cmd/pebble-correctness`'s output is the strongest single correctness signal in the sweep. The mechanics:

1. **Build the subject pool.** Full-scan the source DB once, collect every distinct subject string into a sorted list. (Read-only; the sort makes the index reproducible from run to run.)
2. **Pick *n* random indices.** Use `math/rand` with a deterministic seed (default `1`; configurable via `-seed`). Same seed = same 200 subjects across re-runs; investigating an intermittent failure becomes "rerun with the same seed and look at exactly that subject."
3. **For each chosen subject S, compare row sets.**
   - `srcRows = source.Reader().Find(Pattern{Subject: S})` — every quad in the source whose subject is S.
   - `dstRows = dest.Reader().Find(Pattern{Subject: S})` — same on the destination.
   - Assert set-equal: same set of `(subject, predicate, object, label)` tuples on both sides. Ordering is not required; multiplicity is (no missing or duplicate quads).
4. **Report `n / m mismatches / X.X seconds`.** Zero mismatches means every sampled subject's full row set survived the round-trip byte-for-byte. Non-zero mismatches abort the sweep and log the failing subject(s); the log line is the diagnostic starting point.

**What this catches:**
- Quads dropped on the destination side (set-cardinality mismatch).
- Quads accidentally duplicated.
- Predicate / object / label corruption that count alone wouldn't notice (a `(s,p,o,l)` tuple that's wrong is a different set element from the right one).
- Encoding bugs that affect specific string shapes (Unicode, empty strings, near-NUL bytes, very long values) — proportional to how many of those shapes are sampled.

**What this does NOT catch:**
- Bugs in subjects we didn't sample. With 200 samples on a ~1.7M-subject corpus, that's a 0.012% sample rate. Per Chernoff-style bounds, this catches a 1%-affecting corruption with ~87% probability and a 5%-affecting corruption with ~99.996% probability — but a 0.01%-affecting corruption (e.g., a single subject's quads got dropped) we'd likely miss. Bump `-sample` for higher confidence.
- Bugs in the *commit_ops* journal (audit trail). The sweep only validates current-state quads. The audit-trail round-trip is a v0.2 known limitation — `MigrateToPebble` does not preserve `commits` / `commit_ops` from the source.
- Performance regressions. Correctness sweep ≠ benchmark. Run the read-side benchmark separately.

**Tuning the sweep for your corpus.** The default 200 is the SecDek-validated number; for very large corpora (>10M subjects) push `-sample` up; for smaller corpora (<100k subjects) the default is already overkill. Cost per sample is two `Find` calls per subject — negligible compared to the upstream baseline scan.

## Cutover and rollback

A few principles from the SecDek planning that generalize:

- **Cutover is a config flip, not a destruction.** Point your app at the Pebble dir; leave the SQLite file or partitioned root in place untouched. If the new path breaks, point back at the old one. This is the entire rollback story.
- **Keep the SQLite path readable for the soak window.** Don't `rm -rf` the SQLite source the day of cutover. Wait at least 30 days, ideally past one full ingest cycle / billing close / audit window — whatever's load-bearing in your domain.
- **Drift recovery, if you cut over while writers are running.** If your app is non-trivially active during migration, the destination will be missing writes that landed on the source after the snapshot. The pattern is: capture the snapshot's max `created_at`, run the migration, then run an incremental top-up using the source's `commits` table filtered by that timestamp. `MigrateToPebble` can be re-run additively for this.
- **Validate the cutover after flipping.** Hit a sample of pages / endpoints / queries in production immediately. Don't wait for a user to find the bug.

## Case study: SecDek (data point for sizing your own work)

| Dimension | SecDek value (refreshed 2026-05-06) |
|---|---|
| Live URL | https://sfy.io |
| Hosting | EC2 t4g.large, gp3 EBS, systemd |
| Workload | Letter graph + comment-letter corpus + counsel network + EDGAR/USPTO joins |
| Production shape | **Single-file SQLite at `/var/lib/secdek/secdek.db`.** The partition split discussed in earlier drafts of this guide was tooled (`cmd/partition-migrate`) but never deployed — production was always single-file. The "Option B" framing above is therefore moot for SecDek specifically; the migration is a straightforward single-file → single-Pebble cutover. The three-options framework (A / B / C) still applies for any consumer that *did* deploy partitioned SQLite. |
| Data volume (snapshot 2026-05-06) | **16,155,295 quads / 30 GB SQLite** (snapshot taken 09:29 UTC; corpus is ~15-30% smaller in quad count than 2026-05-05's 19M because a `regen-graph` cycle had recently rebuilt `derived:*`. DB size grew despite fewer quads because the `commit_ops` journal accumulates separately.) |
| Distinct predicates | 340 |
| Distinct subjects | 1,733,370 |
| Bench host | EC2 `t4g.xlarge` arm64, 120 GB gp3 EBS, Ubuntu 24.04 |
| Pebble migration time | **15 min 26 sec** end-to-end (`MigrateToPebble` of 16.15M quads at 17,444 quads/sec sustained) |
| Pebble dir size | **2.6 GB** (vs 30 GB SQLite source → **≈11.5× smaller on disk**) |
| Correctness sweep | Total quad count matches; distinct-predicates count matches; subjects-hash matches (`c7dc26f4c0ff0c4f`); predicates-hash matches (`86f7eac73ed4f9b8`); 200 random subject point queries with **zero mismatches** in 0.8s |
| Read scan (full-Count): SQLite vs Pebble | 159.0s vs **13.7s** (~12× faster on Pebble) |
| Read scan (distinct subjects + predicates): SQLite vs Pebble | 319.7s vs **18.0s** (~18× faster on Pebble) |
| Hot-read benchmark (subject point lookups, label scopes) | *— filled in after `secdek-pebble-bench` run completes; see "Hot-read benchmark" subsection below* |
| Cost (one-shot bench host) | t4g.xlarge for ~1 hour + 120 GB gp3 = ~$0.14 of EC2 time |
| Status | **Migration validated end-to-end on real production data 2026-05-06.** The next decision is the read-benchmark outcome — if Pebble's hot-page latency is competitive with or faster than SQLite, cutover proceeds (planned separately, not on bench-day). If reads regress, falling back to Option A in the migration framework is the next library-side work. |

### Hot-read benchmark

Methodology: identical workload primitives (subject point lookups in the
primary namespace, label-scoped `Reader.Count` across four label
namespaces) replayed against both backends; 2 warmup + 10 hot iterations
each; deterministic seed (`-seed 1`); p50 / p99 reported. Full-corpus
iteration is intentionally skipped here because it's already covered by
the `pebble-correctness` correctness sweep (Phase 1 SQLite full scan
vs Phase 3 Pebble full scan, above).

**First run, before the label-count merger fix:**

| pattern | SQLite p50 | SQLite p99 | Pebble p50 | Pebble p99 | speedup |
|---|---|---|---|---|---|
| subject-letter (200 random subjects) | 17.66 ms | 17.83 ms | 3.81 ms | 3.86 ms | **4.64× Pebble** |
| `Reader.Count(Pattern{Label: "source:sec-letter"})` | 38.05 ms | 38.22 ms | 179.98 ms | 209.58 ms | **0.21× — SQLite wins** |
| `Reader.Count(Pattern{Label: "source:cmt-pipeline"})` | 0.02 ms | 0.03 ms | 0.00 ms | 0.01 ms | (empty namespace post-regen-graph) |
| `Reader.Count(Pattern{Label: "derived:counsel-graph"})` | 0.02 ms | 0.02 ms | 0.01 ms | 0.01 ms | (empty) |
| `Reader.Count(Pattern{Label: "derived:cluster"})` | 2.13 ms | 2.19 ms | 8.61 ms | 9.21 ms | **0.25× — SQLite wins** |

Subject point lookups (the dominant analyst hot-path query shape) win cleanly on Pebble. But **SQLite beats Pebble 4-5× on `Reader.Count(Pattern{Label: X})`** — SQLite uses the covering `idx_lsp(label, subject, predicate)` index for an O(log N) descent, while Pebble was iterating the LSP keyspace and counting tuples in O(N). Raw archive: [`secdek-bench-no-labelcount-merger-2026-05-06.log`](./bench-output/secdek-bench-no-labelcount-merger-2026-05-06.log).

**The fix: a per-label counter keyspace maintained via Pebble's Merge operator.**

Added `'L' | label → 8-byte LE int64` keyspace, registered a custom Pebble Merger that interprets values as int64 deltas and sums them associatively. `Writer.Commit` aggregates per-label deltas across `Adds` and `Removes` and issues one `wb.Merge` per affected label per batch; `BulkLoader.flush` does the same once per flush. `Reader.Count(Pattern{Label: X})` short-circuits to a single 8-byte `Get` on the counter keyspace. `Store.RebuildLabelCounters()` walks the LSP keyspace and resets the counter to truth — used after migrations or to recover from drift. ~250 lines + 6 unit tests in `internal/pebbleq/labelcount.go`. Per-Commit overhead: one `Merge` per distinct label per batch; in this run it cost ~9% on overall migration time (15m26s → 17m1.9s for 16.15M quads).

**Second run, with the label-count merger active on the destination Pebble dir:**

| pattern | SQLite p50 | SQLite p99 | Pebble p50 | Pebble p99 | speedup |
|---|---|---|---|---|---|
| subject-letter (200 random subjects) | 17.66 ms | 18.58 ms | 3.75 ms | 3.83 ms | **4.71× Pebble** |
| `Reader.Count(Pattern{Label: "source:sec-letter"})` | 37.93 ms | 38.12 ms | **0.01 ms** | **0.01 ms** | **5,418× Pebble** |
| `Reader.Count(Pattern{Label: "source:cmt-pipeline"})` | 0.02 ms | 0.03 ms | 0.01 ms | 0.01 ms | 3× (empty) |
| `Reader.Count(Pattern{Label: "derived:counsel-graph"})` | 0.02 ms | 0.02 ms | 0.01 ms | 0.01 ms | 3× (empty) |
| `Reader.Count(Pattern{Label: "derived:cluster"})` | 2.10 ms | 2.19 ms | **0.01 ms** | **0.01 ms** | **350× Pebble** |

Pebble now wins every measured access pattern. The previously-mixed result is gone. The `source:sec-letter` Count dropped from 180 ms (broken Pebble) → 0.01 ms (counter-driven Pebble) — an 18,000× improvement against the prior broken Pebble baseline, and 5,418× against the SQLite covering-index baseline. Raw archive: [`secdek-bench-with-labelcount-merger-2026-05-06.log`](./bench-output/secdek-bench-with-labelcount-merger-2026-05-06.log).

### IngestSorted: variants and their memory ceilings

The standard `BulkLoader` write path settles at ~17K quads/sec because it goes through Pebble's memtable + WAL + compaction. For pure-migration workloads, Pebble exposes `db.Ingest([]string)` — hand it pre-built sorted sstables, it places them directly into the appropriate level, no compaction overhead. CockroachDB's bulk-restore uses this; it's typically 5-10× faster than the regular write path. quadstore exposes this as a three-level ladder, each variant trading complexity for memory bound:

| variant | memory ceiling | when to use |
|---|---|---|
| **`IngestSorted` (in-memory)** | ~500 bytes/quad working set (Quad slice + four key encodings + sort overhead). **Hard ceiling: ~10-15M quads on a 16 GB box; ~30-50M quads on a 64 GB box.** Caller is responsible for staying under. | Small-to-medium corpora that fit in RAM. mega-index, PubDek, single-corpus SlideDek chunks. Simplest API: `Store.IngestSorted(ctx, []Quad, opts)`. |
| **`IngestSortedExternal`** *(planned)* | Bounded memory regardless of corpus size — channel-fed; sorted runs flush to disk; k-way merge stream feeds `sstable.Writer`. | Corpora that exceed in-memory RAM. SlideDek-class (133M+ quads), multi-billion-quad consumers. |
| **Per-corpus driver pattern** | Same as in-memory per chunk; caller groups input by corpus / shard / partition and calls `IngestSorted` per chunk into the same Pebble dir. | When the source data has natural boundaries (per-table, per-shard, per-corpus) that fit in memory individually even when the union doesn't. Best fit for the SlideDek ArangoDB→quadstore port. Documented as a usage pattern, not a new API. |

**Live measurement of the in-memory ceiling (2026-05-06).** First attempt to run `IngestSorted` against the 16,155,295-quad SecDek production snapshot on `t4g.xlarge` (16 GB RAM) **OOM-killed by the kernel during the sort phase**:

```
oom-kill:constraint=CONSTRAINT_NONE ... task=secdek-pebble-i,pid=70176
Out of memory: Killed process 70176 (secdek-pebble-i)
total-vm:17.5 GB, anon-rss:15.7 GB
```

The empirical ceiling on `t4g.xlarge` for SecDek-shape data lands somewhere between **10M and 16M quads**. We pushed past it cleanly. Resized the bench host to `r6g.2xlarge` (64 GB RAM, same arm64) and re-ran — measurements below.

**Successful run on `r6g.2xlarge` (62 GB RAM available):**

| metric | standard `BulkLoader` (with merger) | **`IngestSorted` (in-memory)** | speedup |
|---|---|---|---|
| 16.15M-quad migration | 17m 1.9s | **2m 32.1s** | **6.72×** |
| Sustained ingest rate | 15,809 quads/sec | **106,203 quads/sec** | 6.72× |
| Pebble dir size | 2.9 GB | **2.34 GB** | (fewer compaction artifacts) |
| Compression vs SQLite source | 10.3× | **12.7×** | — |
| Correctness sweep | all pass | all pass — byte-equivalent | — |
| Memory peak | ~few hundred MB | ~9 GB / 62 GB available | (well under ceiling) |

The 6.7× win sits squarely in the 5-10× range CockroachDB's bulk-restore docs predicted. Memory peaked at ~9 GB during the sort phase — comfortable headroom on a 64 GB box, confirming the empirical "~30-50M quads on a 64 GB box" ceiling estimate.

Raw archive: [`secdek-ingest-sorted-r6g-2026-05-06.log`](./bench-output/secdek-ingest-sorted-r6g-2026-05-06.log).

**What this proves and what it doesn't.** Proves: the in-memory `IngestSorted` is byte-equivalent to the standard write path on real production data, and 6-7× faster at the same scale. Doesn't prove: the same path works at SlideDek-class scale (133M+ quads) — we'd need either a much bigger memory box (~200 GB) OR `IngestSortedExternal` (which now exists; results below).

### IngestSortedExternal validation: bounded memory, same hardware

`IngestSortedExternal` is the bounded-memory variant — channel-fed input, chunks sorted in memory, sorted runs flushed to disk, k-way merged into per-keyspace sstables and ingested. Working set: ~400 bytes/quad × `ChunkSize` regardless of total corpus size. The whole reason this path exists is to handle corpora that don't fit in RAM.

**Test 1: same data, bigger host (sanity).** Run the SecDek 16.15M-quad snapshot through `IngestSortedExternal` on `r6g.2xlarge` (62 GB RAM, same as the in-memory IngestSorted run):

- Migration time: **3m 4.9s** (87,373 quads/sec sustained)
- Memory peak: **~1 GB / 62 GB available** (vs ~9 GB for the in-memory variant)
- Runs created: 33 (16.15M / 500k chunk size = 32 + 1 partial)
- Sstables written: 4 (one per keyspace)
- All correctness checks pass; byte-equivalent to the in-memory + standard paths
- Pebble dir: 2.34 GB (identical to in-memory)

22% slower than in-memory IngestSorted (2m 32.1s), but uses 1/9th the memory.

**Test 2: same data, the host that OOM-killed the in-memory variant.** Resized the bench to `t4g.xlarge` (16 GB RAM) — the exact hardware where the in-memory `IngestSorted` was OOM-killed during the sort phase (15.7 GB anon RSS). Same SecDek snapshot, same code, same `ChunkSize=500_000`:

- Migration time: **3m 12.9s** (83,757 quads/sec sustained)
- Memory peak: **~1 GB / 15 GB available**
- All correctness checks pass; same subjects/predicates hashes as the r6g run
- No OOM. Process completed cleanly.

**Only 4% slower than the same workload on `r6g.2xlarge`.** That's the bounded-memory claim landing — the algorithm's working set is set by `ChunkSize`, not by corpus size or host RAM. SlideDek-class workloads (133M+ quads) on `t4g.xlarge` should land at roughly the same memory profile, scaled up only by the number of intermediate run files written to disk.

Raw archives:
- [`secdek-ingest-external-r6g-2026-05-06.log`](./bench-output/secdek-ingest-external-r6g-2026-05-06.log)
- [`secdek-ingest-external-t4g-2026-05-06.log`](./bench-output/secdek-ingest-external-t4g-2026-05-06.log)

### The full ladder, measured

| variant | host | 16M quads time | sustained rate | memory peak | OOM safe at this scale? |
|---|---|---|---|---|---|
| Standard `BulkLoader` (with merger) | t4g.xlarge / 16 GB | 17m 1.9s | 15,809 q/s | few hundred MB | ✓ |
| `IngestSorted` (in-memory) | r6g.2xlarge / 62 GB | **2m 32.1s** | 106,203 q/s | ~9 GB | ✓ |
| `IngestSorted` (in-memory) | t4g.xlarge / 16 GB | OOM-killed | — | 15.7 GB RSS at kill | ❌ |
| `IngestSortedExternal` | r6g.2xlarge / 62 GB | 3m 4.9s | 87,373 q/s | **~1 GB** | ✓ |
| **`IngestSortedExternal`** | **t4g.xlarge / 16 GB** | **3m 12.9s** | **83,757 q/s** | **~1 GB** | **✓** |

Choosing between the two fast paths:
- **In-memory** is 22% faster but has a hard memory ceiling. Right when you know the corpus fits in RAM (caller controls this) and you want maximum speed.
- **External sort** is bounded-memory and runs at 80-90% of in-memory speed on the same workload. Right when corpus size approaches host RAM, OR when running on a smaller bench host than the source data would fit on.

For SecDek's 16M corpus at `t4g.xlarge` scale, either path works after sizing up to a 64 GB box for the in-memory variant. For SlideDek's 133M+ corpus, `IngestSortedExternal` is the only viable path on any host smaller than ~60 GB.

### The per-corpus driver pattern (no new API; just a usage shape)

The third level of the ladder isn't a new API — it's a way to use the existing `IngestSorted` calls that maps cleanly onto how upstream data actually arrives.

**The observation.** Real-world bulk-ingest workloads almost never look like "one giant slice of every quad in the universe." They look like *many corpora, ingested sequentially*: one Turtle file per book, one ArangoDB collection per query family, one S3 prefix per data source. Each individual corpus fits in RAM; the union doesn't.

If your producer can group input by such a natural boundary, you don't need `IngestSortedExternal`'s on-disk merge sort at all. You can use the **in-memory `IngestSorted` per corpus**, calling it sequentially against the same destination Pebble dir. Each call is its own atomic ingest at the Pebble layer. Memory peak stays bounded at the size of the *largest individual corpus*, not the sum.

**The shape:**

```go
import (
    "context"
    "log"

    "github.com/dukkandcards/quadstore"
)

func ingestAll(ctx context.Context, dst *quadstore.PebbleStore, corpora []Corpus) error {
    for _, c := range corpora {
        quads, err := c.LoadAll()       // caller-supplied; may be a streaming
        if err != nil {                  // pull from a source DB, a Turtle parse,
            return err                   // an ArangoDB AQL result, etc.
        }

        stats, err := dst.IngestSorted(ctx, quads, quadstore.IngestSortedOptions{
            DefaultLabel: c.Label,       // "source:turtle-book-N",
        })                                // "source:aql-collection-X", etc.
        if err != nil {
            return err
        }
        log.Printf("ingested %s: %d quads in %s",
            c.Name, stats.QuadsIngested, stats.Duration)
    }

    // After all corpora are in, label counters reflect "writes
    // attempted" (matching the per-Commit semantics). If your
    // upstream had duplicates within or across corpora, the LSP
    // keyspace deduped them but the counters didn't — rebuild from
    // truth here:
    if err := dst.RebuildLabelCounters(); err != nil {
        return err
    }

    return nil
}
```

**Why this is the right shape for many real consumers:**

- **Failure isolation.** If corpus 47 of 200 fails to load from upstream, you retry corpus 47. Corpora 1-46 already landed; 48-200 haven't started. No `WHERE imported_at >= 'last attempt'` ceremony.
- **Memory bounded by *individual* corpus size, not corpus count.** 200 corpora at 5M quads each fit on a 16 GB host one at a time, even though the union (1B quads) doesn't fit anywhere reasonable.
- **No external-sort overhead.** Each `IngestSorted` call uses the in-memory variant, which is 22% faster than `IngestSortedExternal`. You skip the run-file write/read cost.
- **Composable with concurrency** (carefully). Pebble's `db.Ingest` is atomic per call but not parallel-safe across calls; you need to serialize the `IngestSorted` calls. The data-loading work upstream of each call (AQL execution, Turtle parsing, etc.) can run in parallel — typically a producer-pool that fills a "ready corpus" channel that one ingester goroutine drains.
- **Resumable migrations.** Track which corpora have been ingested in a sentinel file or a separate small SQLite/Pebble of "ingest progress." A re-run after a crash skips already-completed corpora.

**Where it doesn't fit:**

- Your upstream is one logical thing that doesn't decompose into corpora — e.g., a single huge JSON file you can't seek into. Use `IngestSortedExternal` directly.
- Your upstream's "natural" corpora are still individually larger than RAM. Use `IngestSortedExternal` per corpus.
- You need the destination Pebble dir to reflect a fully-consistent snapshot at every moment (no partial state visible). Either ingest into a fresh dir then atomically swap pointers (caller-side), or use a transaction-shaped layer above quadstore.

**SlideDek's port shape (forward-looking).** The AQL→quadstore port (`yeti-portrait/cmd/aql-checklist`, 197 AQL queries to translate) is the canonical case. Each AQL collection becomes one corpus; each port-pass through the checklist is "load corpus N from ArangoDB, IngestSorted it into the destination Pebble dir, commit checklist progress." 200-ish corpora, none individually larger than RAM, summed they're 133M+ quads. Per-corpus driver is the right pattern; `IngestSortedExternal` is the fallback if any single corpus turns out to be RAM-sized.

**Implication for callers.** If your corpus exceeds your bench host's RAM divided by ~500 bytes per quad, the in-memory variant will OOM-kill you cleanly mid-migration. There's no graceful degradation. Either size up the bench host (cheaper than dev time most of the time) or wait for `IngestSortedExternal`. Production cutover hosts can be smaller than the bench host — the bulk-load box doesn't have to be the steady-state runtime.

### Why migration takes the time it does

The migration phase ran 16.15M quads in 15m26s (no-merger baseline) / 17m1.9s (with merger active), or 15-17K quads/sec sustained. That's slower than the 20K quads/sec the 2026-05-05 round-trip showed — the difference is mostly per-Commit Merge overhead in the new path (~9%) plus pebble compaction catching up at higher data sizes.

Three layers of structural cost explain the absolute number, in priority order:

1. **LSM write amplification.** Pebble is a log-structured merge tree: every key write lands first in an in-memory memtable, gets flushed to an L0 sstable, and is eventually compacted down through L1, L2, ... merging into bigger sorted files. A single logical key write becomes O(log N) physical writes across levels over time. This is fundamental to LSM trees and documented in Pebble's [README](https://github.com/cockroachdb/pebble/blob/master/README.md) and the broader literature (RocksDB, LevelDB, BigTable). Steady-state writes settle at a sustained rate that's compaction-bound, not write-bound.
2. **Compactor catching up.** During a bulk load the compactor competes for I/O and CPU with incoming writes. Per the bench: ~34K quads/sec early in the run, ~17K sustained — the compactor catching up is the slope you see in the per-100k progress lines.
3. **We're using the normal write path, not Pebble's bulk-ingest fast path.** Pebble exposes `db.Ingest([]string)` for handing pre-sorted external sstables directly to the engine; CockroachDB's bulk-restore uses it and runs 5-10× faster than the regular write path. quadstore's `BulkLoader` does plain `wb.Set` per quad — correct but suboptimal for pure-migration workloads. **This is the next optimization on the roadmap (`BulkLoader.IngestSorted` / `IngestSortedExternal`); see TODO.md.**

For SecDek's actual cutover this is fine — 17 min in a maintenance window is bounded and predictable, scales linearly with corpus size. For SlideDek-class workloads (133M+ quads) the IngestSorted path becomes load-bearing.

### What we're going to measure

When the consolidation run completes, this section will be filled in:

- **Total Pebble dir size** vs the two SQLite partition files combined.
- **Full-scan `Count`** time vs the same on partitioned SQLite.
- **Subject point lookups against the primary namespace** (the dominant hot-page shape) — 200 random samples, p50 / p99 latency vs partitioned SQLite.
- **Subject point lookups against the secondary namespace** — same shape.
- **Cross-partition fan-out reads** that today touch both partitions in SQLite — what does single-Pebble look like for those?
- **Label-scoped reads** across the application's main label namespaces (workload primitives that the higher-level aggregations are built on top of).
- **Migration time** for the consolidation pass itself.

We're deliberately measuring workload primitives (subject lookups, label-scoped reads, full iteration) rather than the application's specific aggregation builders. If Pebble wins the primitives, the application-level pages follow; testing primitives is also more reproducible for any other quadstore consumer who reads this case study and wants to apply the same framework.

The thesis succeeds if Pebble single-dir is competitive (≥ as fast on the hot reads at acceptable size). It fails if the partition split was actually doing real work that Pebble's natural sstable segmentation can't replicate at this corpus shape.

If your numbers are within an order of magnitude of these, the SecDek experience is directly transferable. If you're 10-100× larger or smaller, scale the validation effort accordingly — bigger corpora warrant more thorough rehearsal, smaller corpora might not need the formal correctness sweep at all.

## Anti-patterns

- **Don't flip production by changing one constructor and pushing the deploy.** Always measure on a snapshot first, even if your data round-trips identically.
- **Don't rip out the SQLite source on the same day as the cutover.** The rollback story disappears the moment you delete it.
- **Don't assume Pebble's transitive-dep cost is acceptable just because the perf wins are big.** If you're on Lambda, the binary-size delta and cold-start budget might still make SQLite the right call. Re-check the "Decide whether to migrate at all" table.
- **Don't reflexively keep partitioning if you're moving to Pebble.** Partitioning was load-bearing in SQLite for a specific structural reason that may not exist on an LSM. Test single-Pebble first.

## Where to ask

- Open an issue at https://github.com/dukkandcards/quadstore — questions about the migration shape, parity gaps, or specific call-site concerns are all on-topic.
- If you migrate and want to be listed in the README "Production users today" section, open a PR adding yourself.
