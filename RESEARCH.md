# Quad Store Research: Contention-Based Pattern Detection

## What We Built

**Quad store library** (`~/quadstore`) — 481-line Go module backed by SQLite.
Cayley-inspired first principles: subject-predicate-object-label, schema-on-read,
one file per product, path-based traversal. Zero external deps beyond
`modernc.org/sqlite` (pure Go, BSD).

**Ingest tool** (`~/quadstore/cmd/ingest-index/`) — loads a professional
indexer's reference DOCX and an NLP pipeline's machine-generated index into
the same store with separate labels. Computes signals, runs analysis, compares
human vs machine topology.

## The Dataset

Fannie Hardy Eckstorm, *The Woodpeckers* (1901). 72 pages, 16 chapters.

- **Reference index** (human): Michelle Guiliano's professional index.
  243 entries (117 main + 126 sub), 20 cross-references.
- **Generated index** (machine): mega-index NLP pipeline output.
  703 entries, no routing structure, minimal hierarchy.

Both loaded into one quad store. 37,705 edges across all labels at final state.

## Core Thesis

An index and a patent claim chart are both **contentions about correspondence** —
a human asserting that two things map to each other.

- **Index**: "this term corresponds to this page location"
- **Chart**: "this source element corresponds to this claim element"

Same mechanism, opposite error tolerance:

- Index accepts gaps (compression). Chart fears gaps (exposure).
- Index penalizes redundancy (noise). Chart values redundancy (thoroughness).
- Index is subtraction. Chart is coverage.

Both are combinatorial noise reduction operating on the same signal: the
distribution of repetition across a structured space.

## What We Found

### 1. The topology diff

The human index is a **compressed routing network**. The machine index is a
**flat content list**.

| | Human | Machine |
|---|---|---|
| Content entries | 143 | 761 |
| Content+hub (destination with inbound routes) | 6 | 0 |
| Routers (See → target, no own pages) | 16 | 0 |
| Convergence funnels (parent with sub-entries) | 21 | 8 |
| Marker/navigation nodes | 19 | 0 |

The machine builds zero routing structure and zero navigation nodes. The human
builds a navigation layer on top of the content — predicting user search
vocabulary and mapping it to the book's internal vocabulary.

### 2. The convergence function

The human index is a convergence function. Multiple starting terms route to
the same pages:

```
baby birds → See raising young → pages 11-12
young birds → See raising young → pages 11-12  
feeding young → pages 11-12
raising young → pages 11-12
```

Four users, four starting words, one destination. The "See" redirects predict
which terms readers will bring *from outside the book* and map them into the
book's vocabulary. This is impossible to extract from the text — it requires
modeling the reader, not the content.

### 3. The abstraction level

The single strongest predictor of what the human picks: **the Rosch basic
level of the term hierarchy**.

- 2-word composed terms
- 3-7 page references
- Sitting in the middle of a composition chain (has children, has parents)

This signal alone achieves 40% human match rate in the top 30 — better than
every content-based signal we computed.

| Method | Top-30 human matches | Rate |
|---|---|---|
| Raw term overlap | ~73 of 700+ | ~10% |
| INDEX contention score | 3 of 30 | 10.0% |
| Unweighted greedy | 4 of 15 | 26.7% |
| Foreground greedy | 4 of 16 | 25.0% |
| Aboutness + abstraction | 9 of 30 | 30.0% |
| Abstraction + subsumes penalty | 11 of 30 | 36.7% |
| **Abstraction level alone** | **12 of 30** | **40.0%** |

### 4. Adding signals hurts

The "kitchen sink" formula (all signals, equal weight) scores 20% — worse
than abstraction alone at 33-40%. More signals = more noise in the
combination. Each signal is individually valid but combining them dilutes
rather than converges.

The only signal that improves on pure abstraction: **penalizing terms that
subsume too many others** (removing over-general terms). This is subtraction,
not addition.

### 5. The subtraction pipeline

The index is a subtraction process: start with all words, reduce to entries.

```
761 generated entries
 → remove 1-word terms subsuming 5+ others     → 718 (lost 5 human)
 → remove fully redundant (50+ other paths)    → 463 (lost 49 human)  ← too aggressive
 → remove 4+ word fragments                    → 443 (lost 1 human)
 → remove zero aboutness                       → 423 (lost 16 human)  ← too literal
 → remove geographic fragments                 → 421 (lost 0 human)
```

**Redundancy removal is the most destructive step.** It kills 49 human terms
because Michelle deliberately keeps redundant entries as routing structure.
What the machine sees as redundancy, the human uses as navigation.

**Aboutness is too literal.** It counts term occurrences in page text, but
conceptual terms ("habitat", "sex differentiation") don't appear verbatim.
The indexer abstracts over what the text discusses — the aboutness signal
needs semantic proximity, not string matching.

## Theoretical Frameworks

The pattern we're investigating has names in multiple fields:

- **Burstiness** (Katz 1996, Church & Gale 1995): terms that cluster in bursts
  carry topical weight. Uniform distribution = background/functional.
- **Luhn's resolving power** (1958): mid-frequency terms carry the most
  discriminating power. The "Luhn cut" — not too frequent, not too rare.
- **Rosch's basic-level categories** (1976): humans categorize at the level
  that maximizes cue validity. "Dog" beats "animal" and "golden retriever."
- **Aboutness vs ofness** (Hutchins 1977, Beghtol 1986): what terms *appear*
  (ofness) vs what the text is *about* (aboutness). The machine measures
  ofness. The human indexes aboutness.
- **Rescorla-Wagner prediction error** (1972): associative strength updates
  only when there's a mismatch between prediction and outcome. Fully predicted
  = no new information (blocking).
- **Citation function taxonomy** (Moravcsik & Murugesan 1975): perfunctory vs
  organic, defensive vs persuasive citation. Over-citation per proposition =
  perceived vulnerability.

The unifying concept: **the distribution of repetition across a structured
space is itself a signal.** The indexer, the topic model, the citation
analyst, and the information theorist all read the same pattern.

## The Dog Training Analogy

The mechanism of how a dog learns "sit" maps directly:

1. The dog doesn't learn the *word*. It learns that a cluster of noisy,
   variable inputs (voice, gesture, context) reliably predict reward when
   paired with a specific output.
2. The indexer doesn't extract *terms*. The indexer builds routing structure
   so that variable reader inputs (different search terms) converge on the
   same content.
3. The trainer eliminates noise until the dog responds to the invariant
   signal. The indexer eliminates noise until the reader converges regardless
   of starting term.

## What Remains Unsolved

### The composition problem (atoms → molecules)

The machine extracts atoms ("tongue", "bill", "foot"). The human composes
molecules ("tongue adaptations", "bill adaptations", "foot adaptations").
The composition is a semantic decision — which atoms belong together as a
named concept. Our abstraction-level signal catches the *shape* of molecules
(2 words, mid-frequency) but not the *semantic validity* of the composition.

### The routing problem (predicting user vocabulary)

The 16 "See" redirects are predictions about what readers will search for.
"Baby birds → See raising young" requires knowing that a reader might use
colloquial language. This is fundamentally about modeling the *user*, not
the *content*. No content-based signal can produce this.

### The redundancy paradox

What the machine sees as redundancy (multiple entries pointing to the same
pages) is what the human uses as navigation (multiple entry points for the
same content). The same structural property is noise in one context and
signal in another. The threshold for removing redundancy cannot be set
without knowing *why* the redundancy exists — defensive coverage (chart)
or navigational routing (index).

### The aboutness gap

Conceptual terms ("habitat", "sex differentiation") index content they
describe but don't literally name. The text discusses habitat without
using the word "habitat." Bridging this gap requires semantic understanding
beyond string matching — topic modeling, embedding similarity, or frame
detection.

### The subtraction boundary

The index is definitionally subtraction — reducing many words to few entries.
But each subtraction decision is itself the human judgment we're trying to
model. The subtraction criteria we can compute (frequency, redundancy,
aboutness, abstraction level) approximate the human's decisions but
systematically fail where the decision depends on knowing the audience
rather than the content.

This may be **theoretically impossible** to fully reverse-engineer from
content alone. The routing structure encodes knowledge about the *reader*
that doesn't exist in the *book*. The best the machine can do is identify
the molecule zone (Rosch basic level + Luhn mid-frequency) and present
candidates for human review — which is exactly what mega-index's pipeline
already does.

## Cross-Product Applicability

The same contention model applies to:

- **LawDek charts**: claim element ↔ source element. Chart mode (fear gaps,
  value redundancy). Over-citation per claim element = perceived weakness.
- **LawDek briefs**: case citation ↔ legal proposition. Citation density
  maps to argument confidence.
- **PubDek**: index term ↔ page location across 400+ books. The corpus
  reveals which index terms are universal vs domain-specific.
- **SlideDek**: slide selection ↔ presentation purpose. Selection is a
  contention that "this slide serves this purpose."

## Quad Store State

- `~/quadstore` — library (481 lines), standalone Go module
- `~/quadstore/cmd/ingest-index/` — ingest + analysis tool (~1800 lines)
- Final state: 7,657 nodes, 37,705 edges, 100+ predicates across labels
- All signals stored as quads: foregroundness, aboutness, contention scores,
  prediction error, background rate, weighted edges, abstraction level

## Critical Correction (Jay, 2026-04-13)

We have been theorizing about WHY the indexer makes decisions — audience
modeling, routing prediction, convergence functions. But the theory is
unverifiable. The only verifiable thing is the OUTCOME: pages went in,
index came out.

If the theory doesn't match the outcome, the theory is wrong — not the
outcome. A bad thesis (the why) would produce a mismatched outcome (what
is in the index).

**The right question is not "why did she pick these terms."**
**The right question is "what transformation turns pages into this index."**

This reframes the entire approach:
- Stop modeling the indexer's intent (audience, routing, convergence)
- Start modeling the input-output transformation (pages → entries)
- The transformation function might be simpler than we think
- We've been overcomplicating it by theorizing about the human process
  instead of observing the mechanical relationship between input and output

All the signals we computed (redundancy, aboutness, abstraction, contention)
are theories about WHY. The next step is to examine the actual input-output
mapping without theoretical overlay — what textual features on a page
correlate with an entry appearing in the index, full stop. Not why. Just
what.

## Input-Output Observation (2026-04-13, second session)

Following Jay's correction, we built `cmd/observe` to look at the raw
mapping between entry terms and page text with no theory.

### Initial claim: 46.6% of entry-page pairs have no text match

This was too strict. Our first-pass matching only checked exact string
containment. After accounting for morphological variants:

| Category | Pairs | Rate |
|---|---|---|
| Exact match (term in text) | 125 | 25.8% |
| Plural/singular variant | 15 | 3.1% |
| Possessive variant | 6 | 1.2% |
| Stem/root variant | 95 | 19.6% |
| All words present, not adjacent | 33 | 6.8% |
| Most words present (>50%) | 5 | 1.0% |
| One word present | 95 | 19.6% |
| **Genuine absence** | **111** | **22.9%** |

### Corrected finding: the indexer performs three operations

1. **Direct extraction** (25.8%) — the exact term is on the page. The
   author wrote "acorns" and Michelle indexed "acorns." String match.

2. **Normalization** (30.7%) — a morphological variant is on the page.
   The author wrote "grub" or "grubs" or "woodpecker's", Michelle
   normalized to a canonical form. Includes plurals, possessives, stems,
   and scattered-but-present words.

3. **Extension + abstraction** (43.5%) — the term partially appears
   (19.6% one word present) or doesn't appear at all (22.9% genuine
   absence). Michelle read the page content and either:
   - Extended a term to pages that discuss the concept without using the
     exact word ("grubs" → 12 pages, but "grubs" only appears on 7)
   - Named what the page discusses with a term she invented
     ("description and coloration" → 13 pages, never appears on any)

### What this means

The NLP pipeline can handle Operation 1 (extraction) and partially
handle Operation 2 (normalization via stemming/lemmatization).

Operation 3 is the majority of the work (43.5% of pairs) and it's
the operation we haven't been measuring correctly. This is where the
indexer adds value — reading page content and asserting what it's ABOUT,
not just what words it CONTAINS.

### Notable: "body adaptations" → 19 pages, 0 exact matches

The largest entry in the index is a pure abstraction. The term "body
adaptations" never appears in the book. Michelle invented this label
to organize 19 pages of content about woodpecker bills, feet, tails,
and tongues. The author discussed these body parts individually; the
indexer unified them under a concept the author never named.

This is the most extreme example but it's the same pattern repeated
at smaller scale across 34 entries (23% of all entries with pages).

### Implication for charts

In a patent claim chart, the equivalent of Operation 3 would be:
the lawyer reads a passage of prior art and asserts it "teaches"
a claim element, even though the claim language doesn't appear in
the passage. The passage discusses the concept without using the
patent's terminology. This is the core of claim construction — the
human judgment that two different descriptions refer to the same thing.

## Page Clustering (the breakthrough)

After a full session of signal computation, formula tuning, and theory-driven
approaches that peaked at 40% match rate, the simplest approach worked best.

**Method:** TF-IDF cosine similarity on page word vectors, union-find
clustering at 0.20 threshold. ~200 lines. Filters background words (>50%
of pages) and rare words (<2 pages).

**Result:** 10 clusters from 72 pages. Every major cluster maps directly
to a group of Michelle's index entries:

| Cluster | Pages | Shared vocabulary | Michelle's entries |
|---|---|---|---|
| 1 | 23-33 | acorn, bark, carpenter, food | food foraging, acorns, hoarding |
| 2 | 4-6, 36, 50-55 | bill, tongue, long | body adaptations (tools) |
| 3 | 1, 60-68 | american, black, white, barred | species identification, key |
| 4 | 17-22 | sap, holes, bark, tree | sapsuckers, tree damage |
| 5 | 43-49 | feather, tail, curve | tail, tail adaptations |
| 6 | 38-41 | foot, toes, climbing | foot, foot adaptations |
| 9 | 11-12 | young, feed, nest | raising young |

17 pages are singletons (unclustered).

**Key insight:** Don't try to name the clusters. Don't try to pick terms.
Find page boundaries and present them with shared vocabulary. The indexer
sees "11 pages share acorn/bark/carpenter/food" and names it instantly.
The tool finds the cluster. The indexer names it.

**Why this works better than everything else:**
- No theory about why the indexer does things
- No scoring formulas that dilute when combined
- No subtraction thresholds that destroy routing structure
- Just: which pages share vocabulary? Present the groups.

### Interactive HTML review tool

Built a self-contained HTML page (`cmd/observe/ -html`) for the indexer:

1. **See** — clusters displayed as cards with shared vocabulary tags
2. **Split** — click to divide any cluster into sub-groups (client-side,
   uses precomputed cosine similarity matrix, enforces min 2 pages per side)
3. **Name** — text input on each cluster card
4. **Read** — expand any page to read the flowing prose text
5. **Repeat** — keep splitting until granularity feels right
6. **Export** — download named clusters as JSON

The workflow models what indexers probably do: start with big groups, split
until the granularity is right, name each group. The depth at which they
stop splitting reveals the natural granularity of index entries.

Competing indexing software is output formatting only (typesetting,
alphabetization, page range formatting). None look at source text to find
page clusters. This would be unique to mega-index.

## Open Questions

1. Is the composition problem (atoms → molecules) approachable via
   co-occurrence clustering within the Luhn zone, or does it require
   external knowledge (WordNet, embeddings)?

2. Can the routing problem be approximated by synonym detection +
   colloquial/formal vocabulary mapping, or is it inherently about
   audience modeling?

3. For charts: does the inverse contention model (fear gaps → value
   redundancy) produce useful gap detection when applied to real
   claim chart data?

4. Across books (PubDek corpus): do the same terms consistently appear
   at the Rosch basic level across similar-domain books, or is the
   "right level" book-specific?

5. Is the subtraction boundary — the point where machine signals can't
   substitute for human judgment — the same boundary in indexing as in
   chart construction? If so, is it the *same human capability* being
   exercised in both contexts?
