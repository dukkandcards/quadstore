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

- [ ] Consider whether the library needs anything before other products use it
- [ ] First real product integration (likely mega-index or LawDek)
- [ ] Decide: does this stay standalone or merge into decklib?

## Future Analysis

- [ ] Run contention model on chart data when available (LawDek)
- [ ] PubDek corpus: do same terms hit Rosch basic level across similar books?
- [ ] The composition problem (atoms → molecules) — is it approachable via
      co-occurrence clustering, or does it need external knowledge?
