# Rethink test plan — 2026-05-05

Discussion artifact. Decisions captured here are decisions to *test*,
not decisions to *ship*. The companion analysis is in
[`RETHINK_2026.md`](./RETHINK_2026.md); this doc is the actionable
short version.

> **Status — 2026-05-06.** All three tests have been run. Test 1
> (Pebble vs SQLite head-to-head) passed the decision gate — Pebble
> won 5 of 6 metrics on M1 Pro and 6 of 6 on Linux gp3. Test 2
> (M1 Pro vs cloud Linux) was published in
> [`PEBBLE_VS_SQLITE.md`](./PEBBLE_VS_SQLITE.md). Test 3 (storage
> density) is partially answered — real-data 19M-quad SecDek
> round-trip showed ≈10× compression; the predicate-dictionary
> alone-and-stacked variants are still open. The Pebble backend
> shipped publicly in v0.2; this doc is retained as the historical
> record of the test plan and the decision rule. Current state and
> measured numbers live in [`CHANGELOG.md`](../CHANGELOG.md) and
> [`PEBBLE_VS_SQLITE.md`](./PEBBLE_VS_SQLITE.md).

## Recommendation

If we built quadstore today, the single largest change would be
swapping SQLite for **Pebble** (CockroachDB's pure-Go LSM-tree
storage engine, Apache 2.0).

**Why this lever.** Every structural cost on
[`LIMITATIONS.md`](./LIMITATIONS.md) — the single-writer ceiling, the
B-tree index rebuild on `Close`, the `INSERT OR IGNORE` per row, the
all-TEXT storage — comes from SQLite's design choices. None of them
are "bugs in our usage of SQLite." They are the cost of running a
relational store as a key-value index manager.

**What it would feel like.** Same `Store` / `Reader` / `Writer` /
`Batch` / `BulkLoader` API. Behind it: four sorted keyspaces
(SPO / POS / OSP / LSP) where each quad is four keys with empty
values; a `Writer.Commit` is a single `Pebble.Apply` of a write
batch; a `Pattern` lookup picks the keyspace by which columns are
bound and does one prefix-iterator scan.

**Order-of-magnitude expectations** (from Pebble's own docs +
CockroachDB's published numbers; we have not measured these in our
context yet):

| operation | SQLite today | Pebble target |
|---|---|---|
| Single-quad commit (audited) | 108 µs | 10-20 µs |
| 1M-quad bulk load | ~75 s | < 5 s |
| Subject lookup | 69 µs | < 50 µs |
| Concurrent 8-writer | 1× (serializes) | 4-8× linear |
| 100M-quad on-disk | ~44 GB | ~20-25 GB |

If those numbers don't reproduce in our setup, we don't ship the
swap. Numbers always win over architecture.

## The test, three parts

In priority order. Test 1 is the decision gate; Tests 2 and 3 are
honesty fixes that pay back regardless of the Test 1 outcome.

### Test 1 — Pebble vs SQLite head-to-head (decision gate)

Build a `quadstore-pebble` prototype behind the existing
`Reader` / `Writer` / `Batch` / `BulkLoader` interfaces. Same API
contract; different backend. Run identical benchmarks against both.

Decision rule:
- **Ship the Pebble swap** if Pebble wins on **at least 3 of 5**
  metrics (single-commit, bulk-load, subject-lookup,
  concurrent-writers-8x, on-disk size).
- **Don't ship** if Pebble loses on more than one. The current
  ~2% Go overhead is too clean to risk on a backend swap that
  doesn't decisively win.

Estimated effort:
- Prototype: ~500-1000 lines (the four keyspaces, a `Pattern`
  iterator-merger, batch `Apply` for writes).
- Bench harness: lift our existing `bench_test.go` and run it
  against `*Store` regardless of backend (already mostly possible
  through the interface).

### Test 2 — M1 Pro vs cloud Linux

Today's `PERFORMANCE.md` numbers are M1 Pro / darwin-arm64. Cloud
deployments — including SecDek production — run on Linux instances
with cloud SSDs. The numbers will move; we should publish the delta
honestly.

Target instance: `t4g.large` (4 vCPU / 8 GB / 50 GB gp3 EBS) on
Ubuntu 24.04 / Go 1.25. Run the same `go test -bench` suite, capture
the output, append a "Linux/cloud reference" section to
`PERFORMANCE.md`.

Why this matters even if we don't ship Pebble: M1 SSDs hide
write-fsync cost in a way that gp3 doesn't. Anyone deploying our
library on cloud will see different numbers than the README implies.
That's a credibility gap we should close.

### Test 3 — Storage density at 100M scale

Build a 100 M-quad synthetic corpus shaped like SecDek (140 distinct
predicates, high-cardinality subjects, mixed-type objects). Measure
on-disk size for five variants:

1. SQLite, current schema (baseline)
2. SQLite, predicate-dictionary added (predicates as INT, lookup table)
3. SQLite + zstd compression via `sqlean`
4. Pebble, default zstd block compression
5. Pebble + predicate-dictionary

Hypothesis: **predicate-dictionary alone is 30-50% off** on the SecDek
shape. If true, ship it inside the current SQLite backend before
v1.0 — it's an additive schema change with no API impact and the
compounding cost of waiting grows with the corpus.

## Resource asks

To run Test 1 + Test 2:

- One Linux server, x86_64 or arm64. Spec floor: 8 GB RAM, 4 vCPU,
  50 GB free disk (SSD). `t4g.large` is fine. Doesn't need a public
  IP; SSH-only is enough.
- ~3 hours wall time on the box (most of it idle while benches run).
- Git access to the repo (already public).

If we're spinning up a dedicated bench host anyway, ensure it has
`fio` available — we'd want an `fio` baseline of the disk's random
4 KB write IOPS to contextualize whatever we measure.

## What I'd build first if we go

The minimum prototype to answer Test 1, in commit order:

1. `internal/pebble/store.go` — `Open`, `Close`, the four keyspace
   handles, the four-key `WriteBatch` builder.
2. `internal/pebble/writer.go` — `Writer.Commit` against the batch
   builder. NoAudit-equivalent first; audit comes second.
3. `internal/pebble/reader.go` — `Pattern` → keyspace selection +
   prefix iterator. Single-keyspace reads first; iterator-merge for
   multi-direction reads second.
4. `internal/pebble/bulk.go` — `BulkLoader` against Pebble's
   `IngestExternalFiles` if we want to win the bulk load decisively.
5. Lift `bench_test.go` to take a `func() Store` factory; run it
   against both backends.

Tests 2 and 3 are independent of Test 1 progress. Test 2 we can run
this week; Test 3 needs a corpus generator (~150 lines).

## What stays the same regardless

The library's public surface — what users `import` — does not change
during Test 1. That's the whole point of having built the
Reader/Writer/Batch/BulkLoader interface up front. The prototype
lives behind the same imports; users opt in via `quadstore.Open` vs
`quadstore.OpenPebble` (or eventually a config flag).

If the swap ships, deprecation runway for the SQLite backend is at
least one minor version. We do not break existing deployments to
land a backend swap.
