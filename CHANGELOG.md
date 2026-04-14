# Changelog

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
