package main

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/dukkandcards/quadstore"
)

// analyzeInterEntryStructure looks at relationships BETWEEN entries,
// not properties OF individual entries.
//
// Hypothesis: the human doesn't pick terms independently. The human
// picks STRUCTURES — groups of terms that relate to each other.
// "Body adaptations → bill, foot, tail, tongue" is one structural
// decision, not five independent picks.
//
// If true, the human's entries should show different inter-entry
// relationship patterns than the machine's entries.
func analyzeInterEntryStructure(s *quadstore.Store, label string) {
	fmt.Printf("\n=== INTER-ENTRY STRUCTURE (label: %s) ===\n\n", label)

	type sEntry struct {
		id    string
		term  string
		pages map[string]bool
		words []string // lowercase words in term
	}

	entries := map[string]*sEntry{}
	it := s.Match("", "term", "", label)
	for it.Next() {
		id := it.Quad().Subject
		if _, ok := entries[id]; !ok {
			entries[id] = &sEntry{
				id:    id,
				term:  it.Quad().Object,
				pages: map[string]bool{},
				words: strings.Fields(strings.ToLower(it.Quad().Object)),
			}
		}
	}
	it.Close()

	for id, e := range entries {
		pit := s.Match(id, "has-page", "", label)
		for pit.Next() {
			e.pages[pit.Quad().Object] = true
		}
		pit.Close()
	}

	// --- Signal 1: Term composition chains ---
	// Does this term CONTAIN another entry's term?
	// "tongue adaptations" contains "tongue" — it's a composed term built
	// from a simpler concept. Michelle builds these chains:
	// "body adaptations" → "bill adaptations" → "bill"
	// The machine generates them randomly.
	//
	// A composition chain is: term A contains term B, and both are entries.
	// Length of chain = depth of human abstraction hierarchy.

	fmt.Println("--- Composition chains (term A contains term B, both are entries) ---")

	type chain struct {
		parent string
		child  string
		depth  int
	}
	var chains []chain

	for _, a := range entries {
		for _, b := range entries {
			if a.id == b.id {
				continue
			}
			aLow := strings.ToLower(a.term)
			bLow := strings.ToLower(b.term)
			// A contains B as a complete word boundary match.
			if len(aLow) > len(bLow) && strings.Contains(aLow, bLow) {
				// Verify word boundary (not just substring).
				idx := strings.Index(aLow, bLow)
				before := idx == 0 || aLow[idx-1] == ' '
				after := idx+len(bLow) == len(aLow) || aLow[idx+len(bLow)] == ' '
				if before && after {
					chains = append(chains, chain{a.term, b.term, 0})
				}
			}
		}
	}

	// Find chain depth: how many levels of containment.
	termDepth := map[string]int{}
	for _, c := range chains {
		// If the child itself is a parent in another chain, depth increases.
		parentLow := strings.ToLower(c.parent)
		depth := 1
		for _, c2 := range chains {
			if strings.ToLower(c2.parent) == parentLow {
				continue
			}
			if strings.Contains(parentLow, strings.ToLower(c2.child)) {
				depth++
			}
		}
		if depth > termDepth[c.parent] {
			termDepth[c.parent] = depth
		}
	}

	// Group chains by parent.
	parentChildren := map[string][]string{}
	for _, c := range chains {
		parentChildren[c.parent] = append(parentChildren[c.parent], c.child)
	}

	// Show parents with most children (composition hubs).
	type compHub struct {
		term     string
		children []string
		depth    int
	}
	var hubs []compHub
	for parent, children := range parentChildren {
		sort.Strings(children)
		hubs = append(hubs, compHub{parent, children, termDepth[parent]})
	}
	sort.Slice(hubs, func(i, j int) bool { return len(hubs[i].children) > len(hubs[j].children) })

	limit := 15
	if len(hubs) < limit {
		limit = len(hubs)
	}
	for _, h := range hubs[:limit] {
		fmt.Printf("  %-35s  (%d children, depth %d)\n", h.term, len(h.children), h.depth)
		for _, c := range h.children {
			fmt.Printf("    └ %s\n", c)
		}
	}

	// --- Signal 2: Page-set nesting ---
	// Is one entry's page set a SUBSET of another's?
	// If entry A's pages ⊂ entry B's pages, A is a more specific assertion
	// about a narrower region of B's territory.
	// Human indexes show this nesting deliberately (sub-entry pages ⊂ parent pages).
	// Machine indexes show it accidentally.

	fmt.Println("\n--- Page-set nesting (A's pages ⊂ B's pages) ---")
	fmt.Println("Deliberate nesting = human structure. Accidental = noise.")

	type nestPair struct {
		child, parent   string
		childPages      int
		parentPages     int
		sharedFraction  float64
	}
	var nests []nestPair

	entryList := make([]*sEntry, 0)
	for _, e := range entries {
		if len(e.pages) >= 2 {
			entryList = append(entryList, e)
		}
	}

	for i := 0; i < len(entryList); i++ {
		for j := 0; j < len(entryList); j++ {
			if i == j {
				continue
			}
			a := entryList[i]
			b := entryList[j]
			if len(a.pages) >= len(b.pages) {
				continue // a should be smaller (child)
			}
			// Check if a ⊂ b.
			shared := 0
			for p := range a.pages {
				if b.pages[p] {
					shared++
				}
			}
			fraction := float64(shared) / float64(len(a.pages))
			if fraction >= 0.80 {
				nests = append(nests, nestPair{
					a.term, b.term, len(a.pages), len(b.pages), fraction,
				})
			}
		}
	}

	sort.Slice(nests, func(i, j int) bool {
		if nests[i].sharedFraction != nests[j].sharedFraction {
			return nests[i].sharedFraction > nests[j].sharedFraction
		}
		return nests[i].childPages > nests[j].childPages
	})

	limit = 20
	if len(nests) < limit {
		limit = len(nests)
	}
	for _, n := range nests[:limit] {
		fmt.Printf("  %-30s (%d pp) ⊂ %-30s (%d pp)  %.0f%%\n",
			n.child, n.childPages, n.parent, n.parentPages, n.sharedFraction*100)
	}

	// --- Signal 3: Word-family clusters ---
	// Group entries that share a root word. "bill", "bill adaptations",
	// "bill adpatations for drilling" form a FAMILY around "bill."
	// Human families are intentional hierarchies.
	// Machine families are accidental vocabulary collisions.

	fmt.Println("\n--- Word families (entries sharing a root word) ---")
	fmt.Println("Human = intentional hierarchy. Machine = vocabulary collision.")

	// Find significant roots (words that appear in 3+ entry terms).
	wordEntries := map[string][]string{} // word → entry terms
	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "and": true, "or": true,
		"of": true, "in": true, "to": true, "for": true, "with": true,
		"by": true, "from": true, "as": true, "at": true, "on": true,
		"his": true, "her": true, "its": true, "not": true, "is": true,
		"are": true, "was": true, "be": true, "has": true, "had": true,
		"but": true, "if": true, "no": true, "so": true, "do": true,
	}

	for _, e := range entries {
		for _, w := range e.words {
			if len(w) < 3 || stopWords[w] {
				continue
			}
			wordEntries[w] = append(wordEntries[w], e.term)
		}
	}

	type family struct {
		root    string
		members []string
	}
	var families []family
	for root, members := range wordEntries {
		if len(members) >= 3 && len(members) <= 20 {
			sort.Strings(members)
			families = append(families, family{root, members})
		}
	}
	sort.Slice(families, func(i, j int) bool { return len(families[i].members) > len(families[j].members) })

	limit = 15
	if len(families) < limit {
		limit = len(families)
	}
	for _, f := range families[:limit] {
		fmt.Printf("  [%s] (%d members)\n", f.root, len(f.members))
		show := 8
		if len(f.members) < show {
			show = len(f.members)
		}
		for _, m := range f.members[:show] {
			fmt.Printf("    %s\n", m)
		}
		if len(f.members) > show {
			fmt.Printf("    ... +%d more\n", len(f.members)-show)
		}
	}

	// --- Signal 4: Abstraction level ---
	// The key insight might be about LEVEL. Michelle picks terms at a
	// specific level of abstraction — not too general ("bird"), not too
	// specific ("occiput"). The right level is where:
	//   - The term has enough page references to be useful (coverage)
	//   - The term is specific enough to be discriminating (not background)
	//   - The term composes from simpler terms (it's a molecule, not an atom)
	//   - The term decomposes into sub-entries (it's a node, not a leaf)
	//
	// Measure: word count × page count × composition score.

	fmt.Println("\n--- Abstraction level (the molecule zone) ---")
	fmt.Println("Terms that are composed (not atoms) but not over-general.")

	type abstraction struct {
		term       string
		wordCount  int
		pageCount  int
		childCount int // terms this contains
		parentCount int // terms that contain this
		level      float64
	}

	var abstractions []abstraction
	for _, e := range entries {
		if len(e.pages) == 0 {
			continue
		}

		children := len(parentChildren[e.term])
		parents := 0
		eLow := strings.ToLower(e.term)
		for parent := range parentChildren {
			if strings.Contains(strings.ToLower(parent), eLow) && strings.ToLower(parent) != eLow {
				parents++
			}
		}

		// Level score: penalize extremes.
		// Too few words (1) = atom. Too many (5+) = fragment.
		// Too few pages (1) = anecdotal. Too many (20+) = background.
		// Having children = molecule. Having parents = embedded in hierarchy.
		wordScore := 0.0
		switch len(e.words) {
		case 1:
			wordScore = 0.3
		case 2:
			wordScore = 1.0
		case 3:
			wordScore = 0.8
		default:
			wordScore = 0.4
		}

		pageScore := 0.0
		switch {
		case len(e.pages) == 1:
			pageScore = 0.2
		case len(e.pages) <= 3:
			pageScore = 0.6
		case len(e.pages) <= 7:
			pageScore = 1.0
		case len(e.pages) <= 12:
			pageScore = 0.7
		default:
			pageScore = 0.3
		}

		compScore := 0.0
		if children > 0 {
			compScore += 0.5
		}
		if parents > 0 {
			compScore += 0.3
		}
		if children > 0 && parents > 0 {
			compScore += 0.2 // middle of hierarchy = sweet spot
		}

		level := wordScore*0.30 + pageScore*0.40 + compScore*0.30
		_ = math.Abs // keep import

		abstractions = append(abstractions, abstraction{
			e.term, len(e.words), len(e.pages), children, parents, level,
		})
	}

	sort.Slice(abstractions, func(i, j int) bool { return abstractions[i].level > abstractions[j].level })
	limit = 30
	if len(abstractions) < limit {
		limit = len(abstractions)
	}
	fmt.Printf("  %-35s  %5s  %5s  %5s  %5s  %6s\n",
		"Term", "Words", "Pages", "Child", "Parnt", "Level")
	fmt.Printf("  %-35s  %5s  %5s  %5s  %5s  %6s\n",
		"----", "-----", "-----", "-----", "-----", "-----")
	for _, a := range abstractions[:limit] {
		fmt.Printf("  %-35s  %5d  %5d  %5d  %5d  %.3f\n",
			a.term, a.wordCount, a.pageCount, a.childCount, a.parentCount, a.level)
	}

	// Compare to human if generated.
	if label == "generated" {
		humanTerms := map[string]bool{}
		hit := s.Match("", "term", "", "reference")
		for hit.Next() {
			humanTerms[strings.ToLower(hit.Quad().Object)] = true
		}
		hit.Close()

		fmt.Println("\n  Abstraction-level top 30 vs human:")
		hits := 0
		for i, a := range abstractions[:limit] {
			match := ""
			if humanTerms[strings.ToLower(a.term)] {
				match = " ← HUMAN"
				hits++
			}
			fmt.Printf("    #%-3d  %-35s  level:%.3f%s\n", i+1, a.term, a.level, match)
		}
		fmt.Printf("\n  Human matches in top %d: %d (%.1f%%)\n", limit, hits, float64(hits)/float64(limit)*100)
	}
}
