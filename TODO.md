# Quad Store — TODO

## Where to Start (2026-04-13)

The research hit a wall at 40% human match rate. Every signal we added
after the Rosch basic-level abstraction (2-word terms, 3-7 pages, middle
of composition chain) made the result worse, not better. The subtraction
pipeline confirmed that indexing IS subtraction, but the thresholds destroy
the human's routing structure because we're theorizing about WHY the indexer
makes decisions instead of observing the actual input-output transformation.

**Jay's correction**: Stop modeling the indexer's intent. Model the
transformation. Pages went in, index came out. What function turns input
into output? A bad thesis (the why) would produce a mismatched outcome
(what is in the index). Follow the outcome, not the assumed reasoning.

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

- [ ] Consider whether the library needs anything before other products use it
- [ ] First real product integration (likely mega-index or LawDek)
- [ ] Decide: does this stay standalone or merge into decklib?

## Future Analysis

- [ ] Run contention model on chart data when available (LawDek)
- [ ] PubDek corpus: do same terms hit Rosch basic level across similar books?
- [ ] The composition problem (atoms → molecules) — is it approachable via
      co-occurrence clustering, or does it need external knowledge?
