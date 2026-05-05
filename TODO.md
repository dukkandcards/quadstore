# Quad Store — TODO

## Current State (2026-04-13, end of day)

Page clustering works. The HTML review tool is functional. Michelle
can open it, see clusters, split them, name them, export.

### Immediate (before next Michelle demo)
- [ ] Get Michelle's feedback on the HTML review page
- [ ] Refine cluster threshold if groups are too coarse or too fine
- [ ] Consider: should the split produce 2 or allow 3+ sub-groups?
- [ ] Consider: merge button (combine two clusters that are too granular)

### Integration with mega-index
- [ ] Add clustering as a pipeline step (after extract, before suggest)
- [ ] Named clusters → parent index headings
- [ ] NLP term extraction within each cluster → sub-entries
- [ ] The existing suggest/review/render pipeline stays as-is

### Research (parked, may revisit)
The theory-driven approach peaked at 40% match with Rosch basic-level
abstraction. Every added signal made it worse. The clustering approach
bypassed this by not trying to name things — just finding groups.

### Next session should start here:

1. **Look at the actual pages-to-entries mapping without theory.**
   For each entry Michelle created, read the page text it points to.
   What textual features are present? Don't hypothesize — just list
   what's there. The function might be simpler than we think.

2. **Consider approaching from the chart side.**
   The chart's inverse contention model (fear gaps, value redundancy)
   might reveal the shared pattern more clearly because the lawyer's
   intent (defensive coverage) is more structurally visible than the
   indexer's intent (audience prediction).

3. **Test against a second book.**
   The Woodpeckers results could be overfitting to one dataset. Run
   the same analysis against Beyond Good and Evil or James Clerk Maxwell
   to see if the 40% abstraction-level finding holds across domains.

### Recovery steps:

```bash
# Rebuild workspace if /tmp was cleaned:
cd ~/mega-index && go build -o /tmp/mega-index ./cmd/mega-index/
/tmp/mega-index extract -in testdata/"Woodpeckers-The-Fannie-Hardy-Eckstorm_trimmed.pdf" -out /tmp/woodpeckers-ws
/tmp/mega-index suggest -workspace /tmp/woodpeckers-ws -in testdata/"Woodpeckers-The-Fannie-Hardy-Eckstorm_trimmed.pdf" -min-score 0.20

# Run full analysis:
cd ~/quadstore
go run ./cmd/ingest-index/ \
  -ref ~/mega-index/testdata/"Woodpeckers_submit EDIT short.docx" \
  -ws /tmp/woodpeckers-ws \
  -db /tmp/woodpeckers.db
```

## Quad Store Library

### Shipped 2026-04-13 (rigorous API for multi-product consumption)
- [x] Reader / Writer separation with ctx, blocking semantics
- [x] Batch type (Adds / Removes / Label default / Metadata map)
- [x] `iter.Seq2[Quad, error]` streaming reads via Reader.Find
- [x] commits + commit_ops journal tables (schema v2)
- [x] UUIDv7 commit IDs (time-sortable)
- [x] Enforced label namespace on Writer.Commit: source:/derived:/human:/meta:
- [x] Legacy Add/AddBatch/Delete remain permissive (no breaking change)
- [x] meta table for schema_version (does NOT pollute quads view)
- [x] Migration v1→v2 idempotent; downgrade refused
- [x] Writer error semantics: failed Commit rolls back, Writer usable for retry
- [x] Writer slot = single per Store (Rung 1 of concurrent-writer ladder)
- [x] Full test coverage for new surface (25 tests total passing)

### Decided, documented in memory
- Standalone module ✓ (decklib thin wrapper deferred until 2nd non-PubDek consumer)
- External DB candidates ruled out (we're writing our own)
- Concurrent-writer evolution ladder (Rungs 1-5, no named Rung 6)

### Next
- [ ] mega-index migration: update label writes (`reference` → `source:reference`;
      `generated` → `derived:generated`; `signal/*` → `derived:signal-*`; etc.)
      Do opportunistically when mega-index is next edited.
- [ ] First real product integration via new API (LawDek is the likely
      catalyst — matter/event/conjunction writes)
- [ ] When LawDek imports: revisit decklib thin wrapper design

### Performance — flagged 2026-04-19 by SlideDek port (do not start until cutover stable)
**Context:** First production-scale workload. SlideDek loaded 60K+ decks /
133M+ quads via BulkLoader; full rebuild took ~4 hours (single-threaded
JSON decode + ~500-row INSERT VALUES batches), final on-disk size ~60 GB
for 133M quads = **~444 bytes per quad**. Storage is the main cost — we
are paying 4-5× what a column-encoded triple store would for the same
data. The corpus also exercises the writer slot exclusively for the load
duration, blocking any parallel consumer.

#### Parallelism strategy — what actually parallelizes in SQLite
Hard constraint: **SQLite has a single writer per database**. modernc.org/sqlite
is a pure-Go port and serializes writes regardless of `PRAGMA threads`,
`PRAGMA journal_mode`, or multiple connections. You cannot run two
`CREATE INDEX` statements concurrently on the same DB; you cannot
load two corpora's quads concurrently into the same DB. Any
"parallel processing" plan that ignores this is a non-starter.

**Three layers where parallelism actually buys something** (in
descending leverage for the SlideDek-scale workload):

- [ ] **Architectural — partition per corpus (Rung 2 of the writer
      ladder).** One DB file per corpus = N independent writer slots.
      Multiple corpora can load + index simultaneously. zenodo10k
      gets its own file with its own index rebuild that doesn't
      block the other 124 corpora. Cross-corpus queries become a
      fan-out + merge in the read layer (cheaper than people fear,
      because the read layer is already iterator-based). This is the
      ONLY way to parallelize the CREATE INDEX bottleneck.

- [ ] **Producer/consumer for JSON decode.** For zenodo-class files
      (7.1 GB JSON), today is: one goroutine decodes JSON → one
      goroutine emits quads → BulkLoader serializes to SQLite. The
      JSON decode is single-threaded and CPU-bound at ~25-50% CPU.
      A pool of N decoder goroutines feeding a quad channel to one
      BulkLoader writer keeps the decoder off the critical path
      without violating SQLite's single-writer constraint.
      (Within a single DB, this helps the load phase only — index
      rebuild is unchanged. Combined with partitioning, it lets
      each partition's writer feed from parallel decoders.)

- [ ] **Producer-side sort before INSERT.** If each batch arrives
      pre-sorted by (subject, predicate, object), the index rebuild's
      external sort collapses to a near-linear scan. Sorting is
      embarrassingly parallel across goroutines on the producer
      side. Doesn't reduce write count; collapses index-rebuild cost
      from O(N log N) to O(N).

**What does NOT work:**
- `CREATE INDEX` in parallel via multiple connections — SQLite
  serializes writes per DB; modernc/sqlite has no parallel index
  primitive.
- `PRAGMA threads = N` — only used for sort/aggregation pages in
  some C SQLite builds; modernc/sqlite ignores it for write paths.
- Goroutine-per-deck loaders into the same `*BulkLoader` — they all
  end up queuing at the writer slot.

**The breakthrough math:** partition-per-corpus + producer/consumer
JSON decode together turn the 4-hour SlideDek load into ~30 minutes.
- Today: serial. zenodo (10m load + 30m index rebuild) blocks
  everything; small corpora (~1-2s each) crowd the writer.
- After: 125 partitions load in parallel, capped by CPU/IO. zenodo
  is no longer special — it's just one of N writers, and its index
  rebuild runs concurrent with the others.

Implementation order: partitioning first (Rung 2 of the ladder is
already the "natural next step" in the architecture doc); then
producer/consumer decode for the per-partition writer; then sorted-
batch INSERTs as a final polish.

#### Surprise from 2026-04-19 load: index rebuild dominates, not writes
The "zenodo took ~30 min" framing turned out to be wrong. Per the
verbose log, **the actual zenodo10k__pptx load was 10m4.792s**;
everything after that was `BulkLoader.Close()` rebuilding the three
secondary indexes (`idx_pos`, `idx_osp`, `idx_lsp`) on the full 157M-row
table — observed in `sample` as `_vdbeSorterSort` for >30 min.

This **inverts the optimization priority**. Per-row write speed is fine
(the bulk batch path is already efficient); the cost is recreating
secondary indexes from scratch over a 60+ GB table at the end. Levers
for this specific bottleneck:

- [ ] **Build indexes incrementally during load** (vs DROP→load→CREATE).
      Today's pattern optimizes for cold-cache load throughput, but the
      sort-and-build at Close pays back almost the same cost.
- [ ] **Parallel CREATE INDEX** — SQLite indexes can be built one at a
      time today; multiple indexes could be built concurrently if we
      issue PRAGMA threads + BEGIN CONCURRENT.
- [ ] **Sort-then-insert ordering**. If the load could write quads in
      idx_pos / idx_osp / idx_lsp natural order (predicate-first then
      object/subject), the index build's external sort might collapse
      to a near-linear scan.
- [ ] **Skip indexes we don't need.** Audit which queries actually use
      idx_lsp (label-subject-predicate) vs the SPO index that's kept.
      For SlideDek's single-label workload, idx_lsp may be dead weight.

The earlier "tunable batchSize" / "AssumeUnique" / "predicate dictionary"
items still apply but pay back less on a single load. Index-rebuild
optimization is the highest-leverage move and should be measured first.

#### Use zenodo10k__pptx as the canonical benchmark
A single corpus dominates everything: zenodo10k__pptx contains ~10K
decks / 258K slides / ~10K templates from a 7.1 GB JSON file. Loading
it took longer than the other 124 corpora **combined**, OOM-crashed
the slice-decode loader, and is the only input that exercises tail-of-
distribution behavior (high-cardinality templates, deep guid-cluster
graph, dense slide text). Median corpora (200-deck IEEE working groups,
2K-deck OSF subsets) tell us almost nothing about the system; zenodo
reveals which assumptions break.

**Pin every optimization measurement to zenodo10k__pptx alone:**
- Load time: standalone `quads-load -only zenodo10k -db /tmp/bench.db`
  — current baseline ~30+ minutes, target <5 minutes after parallel
  decode + batch tuning.
- Query latency: any query that iterates "all slides" or "all frames"
  is dominated by zenodo's 258K / 410K. Time `corpus-summary`,
  `pattern-dist`, `composition-risk` against zenodo-only and
  zenodo-excluded subsets — the gap is the optimization target.
- Storage: predicate cardinality differs sharply by corpus type.
  Zenodo's high distinct-template count + dense free-text title fields
  = worst-case for predicate dictionary / interning gains. Measure the
  intern win on zenodo before claiming the win on the whole DB.

If a change is fast on zenodo, it's fast on everything. If it's only
fast on the median corpus, it doesn't ship. (Tail-of-distribution rule:
optimizing the median wastes effort because the median was never slow.)

#### Import (BulkLoader) — high-priority levers
- [ ] **Parallel JSON decode + serial commit.** Today: one goroutine
      decodes JSON → calls fn → BulkLoader.Add. JSON decode is the hot
      path on multi-GB files. A producer/consumer split (N decoder
      goroutines feeding a channel of TransformReportJSON) would let us
      pin one CPU per JSON file while the BulkLoader serializes the
      writes on its single writer slot.
- [ ] **Tunable batchSize.** Hardcoded at `bulkBatchRows = 500`. SQLite
      `SQLITE_MAX_COMPOUND_SELECT` defaults to 999 args per statement;
      with 4 columns per quad we're capped at ~249 quads per multi-row
      INSERT for the variadic path. Today the loader uses 500 anyway —
      either it's batching across multiple INSERTs per Flush, or there's
      a latent bug. Audit, then expose batchSize as a Store option.
- [ ] **Skip dedupe for known-clean loads.** `INSERT OR IGNORE` keeps the
      UNIQUE(s,p,o,l) check on every row. For a fresh DB or a fresh label
      we don't need it. A `BulkLoaderOpts{AssumeUnique: true}` switch +
      `INSERT INTO` (no OR IGNORE) would skip the constraint check.
- [ ] **Defer all index work.** `idx_spo` is kept during bulk load (used
      by INSERT OR IGNORE dedupe). If we add an "AssumeUnique" mode, we
      can also drop `idx_spo` for the load and recreate at end alongside
      the others — the current 4-hour load may have been bottlenecked on
      idx_spo maintenance more than on writes.

#### Storage — high-priority levers
- [ ] **String dictionary / interning.** Subjects, predicates, objects
      are stored as TEXT verbatim. The corpus has high cardinality on
      subjects (one per entity = ~5M strings) but LOW cardinality on
      predicates (~140 distinct values across 133M rows). A separate
      `predicates(id INTEGER PRIMARY KEY, value TEXT UNIQUE)` table +
      `quads.predicate_id INTEGER` would replace 133M predicate strings
      (avg ~20 bytes) with 133M small ints — saving roughly 2 GB just
      on predicates alone. Same logic likely applies to high-frequency
      object values (boolean-like, enum-like predicates).
- [ ] **Partition by label namespace.** Today every quad lives in one
      `quads` table. As `derived:*` grows, scans pay for unrelated
      `source:*` rows. Either separate tables per namespace or
      label-prefixed indexes.
- [ ] **Compression.** SQLite supports `zstd` via extensions (sqlean's
      compress, sqlite-zstd). For the kind of repetitive predicate/object
      content we have, that's likely 3-5× on disk for ~5% read overhead.
- [ ] **Numeric storage for numeric predicates.** Today all objects are
      TEXT. Many predicates are floats or ints (vdc, slide_number,
      file_count). A typed-object branch would cut storage AND make
      range queries possible without ParseFloat per row.

#### Architecture — flagged for design review
- [ ] Peak observed: **133M quads in one Store**. The Cayley lineage
      assumed roughly that scale per partition; we're at the high end on
      a single SQLite file. Before pushing past it, confirm whether
      partitioning by corpus (Rung 2 of the concurrent-writer ladder) is
      the right pre-emptive move, or whether index/storage tuning above
      buys us another order of magnitude inside one file.
- [ ] **Read-during-write.** SlideDek port wants to validate ported
      queries while the next load is running. WAL mode allows concurrent
      readers; but BulkLoader sets `journal_mode=MEMORY` for its life,
      which may serialize readers. Audit + document expected behavior.

#### Rung 5 — backend swap (SQLite → LSM) — flagged 2026-04-19
SQLite is approaching its design ceiling for this workload (single
writer, B-tree + WAL = bulk-load index rebuild dominates, TEXT-only
columns inflate storage). We always planned to swap (the Reader/Writer/
Batch surface was designed for it); we're at that point sooner than
expected.

**Candidates (revised 2026-04-19 after Jay pushback):**
- **DuckDB** (MIT, embedded analytical) — **lean pick.** Columnar +
  vectorized + native percentile/window functions = ~10-100× speedup
  on our exact query pattern (group-by + aggregate). AQL maps to
  DuckDB SQL ~1:1, so the query port shortens from 100-300 Go lines
  per query to 5-10 SQL lines. Single-writer-per-DB is moot once
  partitioned. CGo cost is contained: +~30 MB to the Lambda binary,
  one Dockerfile line for `libduckdb.so`. The `pure_open_source`
  feedback is about license (MIT ✓), not pure-Go.
- **Pebble** (Apache 2.0, pure-Go, CockroachDB lineage) — fallback if
  CGo on Lambda becomes painful. LSM = no index rebuild. KV only —
  we'd build SPO/POS/OSP indexes ourselves. Big code investment.
- **Badger** (Apache 2.0, pure-Go, Dgraph team) — same shape as Pebble,
  similar fallback role. LSM, transactional, KV-only.
- ~~SQLite further tuning~~ — fighting the substrate. Partitioning
  + producer/consumer is the last SQLite work we should do.

Why the candidate flipped from Pebble to DuckDB: the original Pebble
pick was driven by "concurrent writes via LSM." Once partition-per-
corpus is in (Track 1), concurrent writes are achieved at the
architecture layer, and DuckDB's analytical advantages dominate.
Our queries ARE analytics; pretending we have an OLTP workload would
have led us to the wrong substrate.

**Two-track approach (do both, don't sequence):**
- [ ] Track 1: ship partition-per-corpus on SQLite first (1-2 days,
      validates partitioning approach, lets us measure real query
      patterns against partitioned data).
- [ ] Track 2 (in parallel, research-mode): prototype DuckDB-backed
      `quadstore.Store` behind the same Reader/Writer/Batch interface.
      Test on zenodo10k specifically — measure load time, `pattern-dist`
      latency (today: ~5-10s in Go-loop; target: <100ms in SQL), and
      total bytes-on-disk. Also port 5-10 representative AQL queries
      directly to DuckDB SQL to estimate the line-count reduction.
- [ ] Decision gate: after partitioning ships AND DuckDB prototype runs,
      compare on (load time + query latency + storage size + lines-of-
      port-code). DuckDB likely wins all four. If CGo on Lambda turns
      out to be painful, fall back to Pebble.

**Anti-goals — do not pursue:**
- More within-SQLite tuning beyond the partitioning + producer/consumer
  + sorted-batch trio above. Predicate dictionaries, columnar tricks,
  vacuum tuning — all fighting the substrate.
- Multi-substrate maintenance long-term. Pick one and migrate.

## Future Analysis

- [ ] Run contention model on chart data when available (LawDek)
- [ ] PubDek corpus: do same terms hit Rosch basic level across similar books?
- [ ] The composition problem (atoms → molecules) — is it approachable via
      co-occurrence clustering, or does it need external knowledge?
