# Changelog

## v0.3-track — 2026-05-06 — Per-label counter (Pebble Merge)

### Added
- **Per-label counter keyspace on the Pebble backend.** New `'L'` keyspace (`'L' | label → 8-byte LE int64`) maintained via Pebble's [Merge operator](https://github.com/cockroachdb/pebble) on every `Writer.Commit` and `BulkLoader.flush`. Turns `Reader.Count(Pattern{Label: X})` into a single 8-byte `Get` — measured **5,418× faster** than the SQLite covering-index baseline on a 16.15M-quad SecDek snapshot. See [`docs/MIGRATING_TO_PEBBLE.md`](docs/MIGRATING_TO_PEBBLE.md) "Hot-read benchmark" for the before/after table.
- **`Store.RebuildLabelCounters()`** — walks the LSP keyspace, computes per-label totals, replaces the `'L'` keyspace atomically (DeleteRange + Sets in one batch). Use after migrations that bypass the Merge path or any time drift is suspected.
- 6 unit tests in `internal/pebbleq/labelcount_test.go` covering: counter increments via Commit, decrements via Removes, BulkLoader counter persistence, fast-path matches slow-path, RebuildLabelCounters resets to truth, counters survive close+reopen.

### Compatibility / breaking change
- **The Pebble backend now registers a custom Merger** (name: `quadstore.label-count.v1`). Existing Pebble dirs created before this change were opened with `pebble.DefaultMerger` (name: `pebble.concatenate`); attempting to open them with the new code raises a merger-name mismatch. The pre-v0.3 Pebble backend was opt-in (`OpenPebble`) and not yet running in production — known fresh dirs need recreating from source via `MigrateToPebble`. Documented as the v0.3-track migration step.

### Performance
- Per-Commit overhead: one `wb.Merge` per distinct label per batch. Measured ~9% slowdown on overall migration time vs the pre-merger path (15m26s → 17m1.9s on a 16.15M-quad SecDek snapshot, t4g.xlarge / gp3 EBS). The trade-off is unconditionally favorable for any workload that reads `Reader.Count(Pattern{Label: X})`.
- `Reader.Count(Pattern{Label: X})` fast path: ~10 µs (single 8-byte `Get`) regardless of how many quads are under that label. Other Count patterns continue to use the iter-and-count slow path.

### Real-data validation
- Re-ran `cmd/pebble-correctness` on the 16.15M-quad / 30 GB SecDek production snapshot end-to-end with the new merger. All correctness checks pass: total quads match, distinct subjects/predicates hashes match (`c7dc26f4c0ff0c4f` / `86f7eac73ed4f9b8`), 200 random subject point queries with zero mismatches. Pebble dir size: 2.9 GB (≈10× compression vs 30 GB SQLite source). Raw archive: [`docs/bench-output/secdek-correctness-with-labelcount-merger-2026-05-06.log`](./docs/bench-output/secdek-correctness-with-labelcount-merger-2026-05-06.log).

## v0.2.0 — 2026-05-05 — Pebble backend (opt-in)

### Added
- **`OpenPebble(path)` returns a `*PebbleStore`**: a Pebble-backed
  Store using the same Writer / Reader / BulkLoader surface as the
  default SQLite-backed `Open(path)`. Keeps SQLite as the default
  backend; users opt in.
- `*PebbleStore` supports: Writer.Commit (and CommitSync for
  per-Commit fsync), Reader.Find with `iter.Seq2[Quad, error]` and
  Pattern routing across SPO / POS / OSP / LSP keyspaces,
  Reader.Count, BulkLoader (and BulkLoaderWithLabel) with Add /
  Flush / Close / Stats.
- Audit trail (`commits` + `commit_ops`) is implemented in Pebble
  keyspaces, mirroring the SQLite tables; `Batch.NoAudit` suppresses
  the audit writes the same way it does on SQLite.
- Label namespace validation (`source:` / `derived:` /
  `human:{tenant}/` / `meta:`) enforced at Writer.Commit on both
  backends.

### Performance
- Single-quad audited Commit on M1 Pro: **5.95 µs** (Pebble) vs
  ~107 µs (SQLite), 18× faster.
- 1k-quad batch commit: **6.13 ms** (Pebble) vs ~13.1 ms (SQLite),
  2.1× faster.
- Find by subject (~100 rows from 10k): **22.7 µs** (Pebble) vs
  ~68.9 µs (SQLite), 3× faster.
- 100k-quad bulk load: **305 ms** (Pebble) vs ~764 ms (SQLite),
  2.5× faster.
- On M1 only: bulk loads under ~5k rows pay a fixed memtable-flush
  cost on `Close` and are slower on Pebble. The crossover
  disappears on cloud disks — see "Cloud Linux confirms and
  amplifies" below: at N=1k Pebble is within 13% of SQLite on
  gp3 EBS, and at N=10k it's already 3× faster.

Full breakdown including durability semantics: see
[`docs/PEBBLE_VS_SQLITE.md`](./docs/PEBBLE_VS_SQLITE.md).

### What's now in `*PebbleStore`
- Higher-level helpers: `LabelCounts`, `Stats`, `CommitStatsAt`.
  Same return contracts as the SQLite versions; cost shapes differ
  (SeekGE-driven traversals vs SQL aggregates) but both are O(N) at
  worst and faster on the per-label / per-predicate paths.
- Cross-backend migration: `quadstore.MigrateToPebble(ctx, src,
  dst, opts)` streams a SQLite-backed `*Store` into a
  `*PebbleStore` via `Reader.Find` + `BulkLoader`. Audit trail is
  not copied in v0.2 (the source's commit log stays on the SQLite
  side; the destination's audit starts fresh).

### What's still NOT in `*PebbleStore`
- Partitioning (single Pebble dir per Store).
- `Match` (legacy `*Iterator` API — use `Reader.Find` with `iter.Seq2` instead).
- `Path` traversal helpers (`From` / `Out` / `In` / `Has` / `Unique`).
- These will be ported when there's a concrete user request.

### Cloud Linux confirms M1 result and amplifies it
- Re-ran the same bench suite on a fresh AWS `t4g.large` (gp3 EBS,
  Ubuntu 24.04 ARM64). The slow-fsync cloud disk widened most
  Pebble wins:
  - Single-quad commit: M1 18× → Linux **40× faster**.
  - 1k batch: M1 2.1× → Linux **4.5× faster**.
  - Bulk load 100k: M1 2.5× → Linux **5.5× faster**.
  - The "Pebble loses small-N bulk loads" footnote on M1 disappears
    on real disks: at N=1k Pebble is now within 13% of SQLite, and
    by N=10k it's 3× faster.
- Raw bench output archived at `docs/bench-output/linux-t4g-large-2026-05-05.txt`.

### Dependency cost
- Pebble pulls in ~20 transitive packages (CockroachDB redact,
  Sentry SDK, Prometheus client, snappy, klauspost/compress).
  Pure Go (no CGo). If transitive-dep size matters more than the
  perf wins, stay on `Open(path)`.
- All Pebble-introduced deps are permissive licenses (BSD-2 / BSD-3 /
  MIT / Apache 2.0). No AGPL, no BSL, no commercial-use restrictions.
  Audited 2026-05-05.

## v0.1.0 — 2026-05-05 — first public release

First public release. Repo flipped to public visibility on GitHub.
API is stabilizing; breaking changes are possible before `v1.0.0` and
will be called out explicitly in this file.

### Highlights
- Single-node graph database for Go applications. Pure Go (no CGo) via
  `modernc.org/sqlite`. `go install`, cross-compile, embed in your
  binary — no toolchain dance.
- Quad model (`subject`, `predicate`, `object`, `label`) with enforced
  namespace prefixes (`source:`, `derived:`, `human:{tenant}/`, `meta:`).
  Multi-tenancy and provenance live in the storage layer, not bolted on.
- Reader / Writer / BulkLoader split. `iter.Seq2[Quad, error]` over
  patterns; range-over-func from Go 1.23+.
- Optional partitioning via `OpenPartitioned` for fact families that
  don't share queries.
- `Migrate` / `MigrateFromSnapshot` for moving data between Stores
  without holding the source DB at write-locking risk.
- New: `Store.LabelCounts(ctx)` — fast indexed `GROUP BY label` for
  migration planning, dashboards, "what fact families are in this DB".
- Used in production by [SecDek](https://sfy.io) (28 GB graph,
  ~10K quads/sec sustained ingest, sub-millisecond point lookups).

### What this release does not include (intentionally)
- Distributed / sharded operation. Single-node by design.
- A query language. Go functions, not a compiler.
- Server mode. Library only.
- Graph algorithms beyond pattern lookup.

If you need any of those, use Dgraph, JanusGraph, or Neo4j. quadstore
is for the class of application where the graph fits on one machine
and the operational budget is one binary.

## 2026-05-05 — race-free migration (`MigrateFromSnapshot`)

### Why
- The first real-world apply of `Migrate` on a live SecDek source caught
  a torn-snapshot surface: a daily cron job had `[remove]`d 258 K
  derived rows and was mid-rebuild when `Migrate` started scanning.
  Migration captured ~50 K instead of the steady-state 258 K. The fix
  in operations was to wait for the cron job to finish; the fix in the
  library is to remove the surface entirely.

### API (additive, no break)
- `MigrateFromSnapshot(ctx, srcPath, dst, opts) (SnapshotStats, error)`
  — takes a consistent point-in-time copy of `srcPath` via SQLite's
  `VACUUM INTO`, then migrates from the frozen snapshot. Concurrent
  writers on `srcPath` are explicitly supported per SQLite's
  documentation. Pure-Go; no external `sqlite3` CLI dependency.
- `SnapshotOptions{SnapshotPath, KeepSnapshot, Migrate}` — caller
  controls where the snapshot file lands and whether it persists.
- `SnapshotStats` — embeds `MigrateStats`; adds `SnapshotDuration` and
  `SnapshotBytes` for the snapshot phase.

### When to use which
- `MigrateFromSnapshot` — recommended when the source has any
  concurrent writers (a live system, a cron'd ingest job, anything).
- `Migrate` — fine when the source is genuinely quiescent (one-shot
  import from an exported file). The library trusts the caller; use
  the snapshot path if you cannot guarantee quiescence.

### Test coverage
- 3 new tests: round-trip with auto-delete snapshot, KeepSnapshot
  contract, required-options validation. 46 total pass (was 43).

## 2026-05-05 — fix(migrate): don't override BulkLoader.batchSize

`Migrate` was setting `bl.batchSize = opts.ChunkSize` (default 10000),
producing 40000 SQL variables per multi-row INSERT and tripping
modernc.org/sqlite's `SQLITE_MAX_VARIABLE_NUMBER` ceiling with
`SQL logic error: too many SQL variables`. `BulkLoader`'s 500-row
default is sized for that ceiling and is now left alone.
`opts.ChunkSize` is repurposed as the progress-reporting cadence,
decoupled from the per-INSERT row count.

## 2026-05-05 — partitioning (`OpenPartitioned`, `Migrate`)

### Why
- A single `quads` table hits a B-tree dilution wall when a Store
  accumulates fact families that don't share queries. Concrete trigger:
  SecDek's live DB grew 1.3 GB → 28 GB in two weeks after a comment-
  letter corpus load, and product queries on the (still-256K-row)
  no-action letter family now scan a B-tree where ~67% of leaves are
  unrelated. See `docs/PARTITIONING_DESIGN.md`.

### API (additive, no break)
- `OpenPartitioned(cfg PartitionedConfig) (*Store, error)` — opens N
  independent SQLite files behind one Reader / Writer / Batch surface.
- `LabelRouter` (consumer-supplied `func(label string) Partition`) —
  routes writes by label.
- `PatternRouter` (optional, `func(p Pattern) Partition`) — routes
  reads when the consumer has deterministic knowledge beyond label
  (e.g., subject-prefix scoping). Library never guesses; consumers
  encode their routing knowledge here.
- `Store.WriterFor(ctx, p)` — acquires a writer slot for a named
  partition. Slots are independent across partitions; concurrent
  Writers on different partitions are allowed.
- `Store.BulkLoaderFor(ctx, p)` — direct partition target for migration
  tooling that routes externally.
- `Store.VacuumFor(ctx, p)` — vacuum a single partition.
- `Store.Partitions()`, `Store.PartitionFor(label)` — introspection.
- `Migrate(ctx, src, dst, opts) (MigrateStats, error)` — copies a
  single-file or partitioned source into a partitioned destination,
  routing each quad / commit / commit_op via the destination's
  `RouteLabel`. `OnlySince` enables incremental top-up. The source is
  read-only throughout.

### Sentinel errors
`ErrCrossPartitionBatch`, `ErrUnknownPartition`, `ErrUnroutableLabel`,
`ErrNoPartitions`, `ErrDuplicatePartition`, `ErrEmptyPartitionName`,
`ErrMissingDefault`, `ErrMissingRouter`, `ErrDestinationNotPartitioned`.

### Behaviour
- Single-file `Open(path)` is unchanged in behaviour; internally a
  single-partition Store with an empty partition name.
- `Reader.Find` / `Reader.Count` scope to one partition when routing
  resolves; otherwise fan out across every partition. Order across
  partitions is unspecified.
- `Writer.Commit` validates every quad in a Batch routes to the
  Writer's partition; cross-partition batches return
  `ErrCrossPartitionBatch` and roll back.
- `Store.Stats` sums quads across partitions; the DISTINCT predicate
  count is the union of distinct predicates seen in any partition.
- `Store.Vacuum` runs sequentially across partitions (admin operations
  on a shared disk parallelise poorly).

### Test coverage
- 13 new tests in `partitioned_test.go` (routing, fan-out, cross-batch
  rejection, concurrent writers across partitions, validation matrix,
  `Migrate` round-trip). All 43 tests pass.

## 2026-04-20 — commit-journal retention (`Writer.PruneOps` + `cmd/prune`)

### Why
- `commit_ops` (one row per add/remove per commit) grows unbounded and is
  typically the largest table in a mature store. SecDek at 1.13 M quads:
  202 commits / **1.87 M commit_ops** rows, ~42% of the 1.7 GB DB file.
- The `quads` table (current state) is the product's live data. `commit_ops`
  is an audit trail — useful but not load-bearing for queries.
- `derived:*` labels are regeneratable from `source:*` by project
  convention, so their audit trail is explicitly disposable.

### API (additive, no break)
- `Writer.PruneOps(ctx, olderThan time.Time) (int64, error)` — deletes
  `commit_ops` rows whose parent commit predates `olderThan`. Preserves
  `commits` metadata rows and the `quads` current-state table.
- `Store.CommitStatsAt(cutoff) (CommitStats, error)` — preview counts
  (total + eligible) before running a sweep.
- `Store.Vacuum()` — reclaim freed pages after a sweep.

### New command: `cmd/prune`
- `prune --db <path> --older-than 90d [--apply] [--vacuum]` — retention
  sweep with a dry-run default.
- `prune --db <path> --before YYYY-MM-DD ...` — absolute cutoff for
  one-time aggressive sweeps (e.g., after a bulk regen).
- Dry run reports DB size, total / eligible commits and ops. Apply
  reports delete count + duration; vacuum reports MB reclaimed.

### Test coverage
- `TestWriterPruneOps` exercises: delete eligible ops, preserve commits
  metadata, preserve quads table, idempotent second sweep, error after
  Close. All 26 tests pass.

## 2026-04-20 — DSN PRAGMA fix (latent misconfiguration)

### What changed
- `Open(path)` DSN rewritten to use `_pragma=key(value)` form for all four settings: `journal_mode(WAL)`, `busy_timeout(5000)`, `synchronous(NORMAL)`, `cache_size(-262144)` (256 MB).

### Why (the latent bug)
- The previous DSN (`?_journal_mode=WAL&_busy_timeout=5000`) was **silently ignored** by `modernc.org/sqlite` — those underscore-prefixed shortcuts aren't honored on file-backed DBs in the version used here.
- Verified 2026-04-20 against the live SecDek database (1.7 GB): `journal_mode=delete`, `synchronous=2` (FULL), `cache_size=2000` (~2 MB default), `busy_timeout=0`. Every dek product opening a quadstore has been running in default rollback-journal mode since project start.
- Post-fix verification on a fresh DB: `journal=wal`, `synchronous=1` (NORMAL), `cache_size=-262144`, `busy_timeout=5000`, WAL sidecar file created.

### Impact
- On next open by any consumer (SecDek, LawDek, PubDek, SlideDek-future), `journal_mode` persists to the file header and stays WAL. The other three pragmas are per-connection and apply on every open.
- No breaking changes, no API changes, no behavior change at the Reader/Writer level.
- All 25 existing tests pass unchanged.

### Cache size rationale
- `cache_size=-262144` = 256 MiB page cache. With WAL, the OS page cache is the main backstop; 256 MB keeps hot indexes (4 ms p,o lookups) resident for SecDek's 1.13M-quad working set while leaving plenty of room for 10M+ quads. Easily tuned per-product later via quadstore option surface if needed.

## 2026-04-13 — Third Session: Rigorous Multi-Product API

### Writer / Reader API (prepares ~/quadstore for LawDek + PubDek consumption)
- **Writer / Reader separation** with explicit types and `context.Context` throughout
- **Batch** as the atomic commit unit (`Adds`, `Removes`, default `Label`, `Metadata`)
- **`iter.Seq2[Quad, error]`** streaming reads via `Reader.Find`; `Reader.Count` for pattern counts
- **`Partition`** opaque routing key (no-op today; reserved for per-partition backing files — Rung 2 of concurrent-writer evolution ladder)
- **`Writer(ctx)` / `WriterFor(ctx, p)`** block on writer-slot acquisition with ctx cancellation
- **Failed `Commit` rolls back**, Writer remains usable for retry; `Close` always releases slot
- **Rung 1** of the concurrent-writer ladder (single writer slot per Store) lives inside the library — callers never see mutexes

### Provenance / Journal (schema v2)
- New `commits` table: UUIDv7 id (time-sortable), created_at, label, metadata JSON
- New `commit_ops` journal: one row per add/remove per commit (full audit trail)
- `quads` table unchanged — pure current-state projection; history lives in `commit_ops`
- Documented well-known metadata keys: `MetaActor`, `MetaSource`, `MetaReason`

### Label namespace (enforced on Writer.Commit)
- Valid prefixes: `source:`, `derived:`, `human:`, `meta:` (empty label also valid)
- Legacy `Add` / `AddBatch` / `Delete` remain permissive — no breaking change
- Migration mapping for mega-index: `reference` → `source:reference`, `generated` → `derived:generated`, etc. (apply when mega-index next touched)

### Schema migration
- `meta(key, value)` table for store-level metadata — **NOT a quad**, to avoid polluting user views (`Stats`, `Match`, `Shape`)
- `v1 → v2` migration idempotent; downgrade refused with clear error
- `schema_version` starts at 2 on fresh stores

### Test coverage
- 11 new tests covering Writer/Reader, label validation, ctx cancellation, commit-after-close, retry-after-error, v1→v2 migration, downgrade refused, meta-not-visible-to-user
- All 25 tests (14 legacy + 11 new) passing; existing behavior preserved

### Incidental: `cmd/observe` vet cleanup
- Fixed 5 `go vet` warnings (`fmt.Println("text\n")` → `fmt.Print("text\n\n")`) preserving visual blank-line separation

### Design principle captured
- **Rigorous first, adapt later** — structured/strict design over loose-with-plan-to-adapt; rigor gives future changes a reference point

## 2026-04-13 — Second Session: Clustering Breakthrough

### Observe Tool (`cmd/observe/`)
- Built `cmd/observe` for raw input-output analysis
- `-stats` mode: counts term presence on pages (25.8% exact, 22.7% genuine absence)
- `-cluster` mode: TF-IDF cosine page clustering (10 clusters from 72 pages)
- `-html` mode: self-contained interactive HTML review page for indexer
- Page range parser fix: en-dash ranges now properly expand (23–27 → 23,24,25,26,27)

### HTML Cluster Review Page
- Self-contained HTML, no dependencies, opens in any browser
- Clusters shown as cards with shared vocabulary tags
- Text input to name each cluster
- Expandable page text (PDF line breaks collapsed to flowing prose)
- Split button: divides cluster into 2 sub-groups using precomputed cosine similarity
- Minimum 2 pages per side enforced
- Export named clusters as JSON
- Designed for professional indexer (Michelle) to review and name groups

### Key Findings
- Simple TF-IDF cosine clustering found every major section the indexer indexed
- 22.7% of entry-page pairs are genuine absence (term never appears on page in any form)
- The indexer performs 3 operations: extract (25.8%), normalize (30.7%), abstract (22.7%)
- The tool's job: find the clusters. The indexer's job: name them.
- Competing software is output formatting only — none do source text clustering

### Research Correction
- Stop modeling indexer's intent (why). Model the transformation (pages in, index out).
- The simplest approach (page vocabulary clustering) outperformed all theory-driven signals.

## 2026-04-13 — Initial Research Session

### Quad Store Library (`quadstore.go`, `shape.go`)
- Built Cayley-inspired SQLite quad store: 481 lines, pure Go
- Subject-predicate-object-label data model, schema-on-read
- `Open` / `Close` / `Add` / `AddBatch` / `Delete` / `Stats`
- `Match` with wildcard pattern matching (any field)
- `From().Out().In().Has().Unique()` path traversal
- `Shape()` tokenized topology export for cross-product review
- 14 tests covering all paths including file-level isolation
- Single dep: `modernc.org/sqlite` (pure Go, BSD)

### Ingest Tool (`cmd/ingest-index/`)
- DOCX parser for professional reference index (Michelle's Woodpeckers)
- Workspace reader for NLP-generated index (mega-index pipeline output)
- Both loaded into same store with separate labels (`reference`, `generated`)

### Research: Topology Diff
- Human index: 143 content + 6 hubs + 16 routers + 21 funnels + 19 markers
- Machine index: 761 content + 0 hubs + 0 routers + 8 funnels + 0 markers
- Machine builds zero routing or navigation structure

### Research: Signal Computation (9 types)
- Page overlap ratio, co-density, term subsumption, funnel width,
  inbound count, orphan pages, page spread, word count, colloquial distance
- Prediction error: discriminative power, blocking, stimulus generalization,
  marker candidates
- Background rate: coverage, clustering, gap variance, burstiness
- Weighted edges: IDF × page specificity × co-occurrence coherence × continuity
- Contention model: coverage breadth, exclusivity, redundancy, density,
  agreement — with INDEX and CHART composite scores
- Aboutness: term density, chapter match, position, sentence ratio
- Abstraction level: word count score × page count score × composition score

### Research: Key Findings
- **Best predictor**: Rosch basic-level abstraction (2 words, 3-7 pages,
  middle of composition chain) — 40% human match rate in top 30
- **Adding signals hurts**: kitchen sink formula scores 20% vs 40% baseline
- **Only subtraction helps**: penalizing over-general terms (+subsumes penalty)
  is the only improvement over pure abstraction
- **Index-chart duality**: same contention model, opposite error tolerance.
  INDEX-scored and CHART-scored rankings are genuinely inverse.
- **Subtraction pipeline**: 761 → 421 entries (45% reduction) but redundancy
  removal too aggressive — destroys 49 human terms that are routing structure

### Research: Theoretical Frameworks Identified
- Burstiness (Katz 1996), Luhn's resolving power (1958), Rosch basic-level
  categories (1976), aboutness vs ofness (Hutchins 1977), Rescorla-Wagner
  prediction error, citation function taxonomy (Moravcsik & Murugesan 1975)

### Research: Critical Correction
- Stop modeling indexer's intent (why). Model the transformation (what).
- Pages in, index out — what function produces the output from the input?
- The theories about audience, routing, convergence are unverifiable.
  The outcome (the actual index) is the only verifiable thing.

### Dog Training Analogy
- Same mechanism as training a dog to sit: eliminate noise until the
  invariant signal remains. The trainer doesn't teach every variation
  of "sit" — the trainer reduces until the dog responds to the pattern.
- Applied: the indexer doesn't find terms, the indexer eliminates until
  the reader converges regardless of starting term.
