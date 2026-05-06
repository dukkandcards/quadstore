# Migrating from the SQLite backend to Pebble

A practical guide for moving an existing SQLite-backed quadstore deployment to the Pebble backend. Written from the actual experience of preparing SecDek (live at sfy.io) for cutover; the same shape applies to anyone using `quadstore.Open` who's considering `OpenPebble`.

This is **not** a "you must migrate" doc. The SQLite backend is supported indefinitely. Migrate when the tradeoffs add up for your deployment — and stop when they don't. The guide below is the framework for deciding *and* the mechanics for executing.

> **Live experiment status (started 2026-05-06).** SecDek is the
> first real-world cutover from partitioned SQLite to a single
> Pebble dir (Option B below). The hypothesis under test:
> *Pebble's LSM + Bloom-filter shape removes the B-tree dilution
> problem that forced SQLite-side partitioning, so consolidating
> SecDek's two-partition layout (`main.db` + `corpus.db`) into
> one Pebble dir wins or breaks even on the hot reads while
> halving operational complexity.* Updates will land in this
> doc's "Case study" section as measurements come in. If the
> experiment fails, this doc will say so explicitly — that
> outcome is just as useful for the next person as the success
> case.

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

## Cutover and rollback

A few principles from the SecDek planning that generalize:

- **Cutover is a config flip, not a destruction.** Point your app at the Pebble dir; leave the SQLite file or partitioned root in place untouched. If the new path breaks, point back at the old one. This is the entire rollback story.
- **Keep the SQLite path readable for the soak window.** Don't `rm -rf` the SQLite source the day of cutover. Wait at least 30 days, ideally past one full ingest cycle / billing close / audit window — whatever's load-bearing in your domain.
- **Drift recovery, if you cut over while writers are running.** If your app is non-trivially active during migration, the destination will be missing writes that landed on the source after the snapshot. The pattern is: capture the snapshot's max `created_at`, run the migration, then run an incremental top-up using the source's `commits` table filtered by that timestamp. `MigrateToPebble` can be re-run additively for this.
- **Validate the cutover after flipping.** Hit a sample of pages / endpoints / queries in production immediately. Don't wait for a user to find the bug.

## Case study: SecDek (data point for sizing your own work)

| Dimension | SecDek value |
|---|---|
| Live URL | https://sfy.io |
| Hosting | EC2 t4g.small, gp3 EBS, systemd |
| Workload | Letter graph + comment-letter corpus + counsel network + EDGAR/USPTO joins |
| Data volume | 19,176,859 quads / 28 GB SQLite (post comment-letter expansion) |
| Distinct predicates | 337 |
| Distinct subjects | 2,688,183 |
| Production shape | Partitioned (`main.db` + `corpus.db`) since 2026-05-05 |
| Measured Pebble migration | 15 min 36 sec at 20,478 quads/sec (single-file source → single Pebble dir, t4g.xlarge bench) |
| Measured Pebble dir size | ~3 GB (≈10× compression vs 28 GB SQLite) |
| Correctness | Subjects-hash + predicates-hash matched byte-for-byte; 200 random subject point-queries with zero mismatches |
| Status | **Option B chosen 2026-05-06.** Pebble validated end-to-end on the single-file SQLite snapshot (2026-05-05). Next step: consolidate `main.db` + `corpus.db` into one Pebble dir on a t4g.xlarge bench host, measure size + hot-read latency vs current partitioned production, decide cutover. Results will be written back into this section as they land. |

### What we're going to measure

When the consolidation run completes, this section will be filled in:

- **Total Pebble dir size** vs current `main.db` + `corpus.db` combined.
- **Full-scan `Count`** time vs the same on partitioned SQLite.
- **`letter:*` subject point lookups** (the dominant query shape on `/grid/letter:*`) — 200 random samples, p50 / p99 latency vs partitioned SQLite.
- **`cmt:*` subject point lookups** (the dominant query shape on the comment-letter pages) — same shape.
- **Cross-partition fan-out reads** that today touch both partitions in SQLite — what does single-Pebble look like for those?
- **A representative read benchmark** of the actual hot-page query mix (`gridCorpusIndex` build, `byFirmDomain` aggregation, `partners` career-timeline rollup).
- **Migration time** for the consolidation pass itself.

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
