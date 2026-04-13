package main

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/dukkandcards/quadstore"
)

type signalEntry struct {
	id        string
	term      string
	pages     map[string]bool
	subIDs    []string
	parentIDs []string // entries this is a sub-entry of
	seeTarget string
	inbound   []string // entries that "see" or "see-also" to this
}

func computeSignals(s *quadstore.Store, label string) {
	fmt.Printf("=== COMPUTING SIGNALS (label: %s) ===\n\n", label)

	entries := map[string]*signalEntry{}

	// Collect terms.
	it := s.Match("", "term", "", label)
	for it.Next() {
		id := it.Quad().Subject
		if _, ok := entries[id]; !ok {
			entries[id] = &signalEntry{id: id, term: it.Quad().Object, pages: map[string]bool{}}
		}
	}
	it.Close()

	// Collect pages.
	for id, e := range entries {
		pit := s.Match(id, "has-page", "", label)
		for pit.Next() {
			e.pages[pit.Quad().Object] = true
		}
		pit.Close()
	}

	// Collect sub-entries.
	it = s.Match("", "has-sub-entry", "", label)
	for it.Next() {
		parentID := it.Quad().Subject
		childID := it.Quad().Object
		if p, ok := entries[parentID]; ok {
			p.subIDs = append(p.subIDs, childID)
		}
		if c, ok := entries[childID]; ok {
			c.parentIDs = append(c.parentIDs, parentID)
		}
	}
	it.Close()

	// Collect see routes.
	it = s.Match("", "see", "", label)
	for it.Next() {
		if e, ok := entries[it.Quad().Subject]; ok {
			e.seeTarget = it.Quad().Object
		}
		if e, ok := entries[it.Quad().Object]; ok {
			e.inbound = append(e.inbound, it.Quad().Subject)
		}
	}
	it.Close()
	it = s.Match("", "see-also", "", label)
	for it.Next() {
		if e, ok := entries[it.Quad().Object]; ok {
			e.inbound = append(e.inbound, it.Quad().Subject)
		}
	}
	it.Close()

	// Build page→entries reverse index.
	pageEntries := map[string][]string{} // page → entry IDs
	for id, e := range entries {
		for p := range e.pages {
			pageEntries[p] = append(pageEntries[p], id)
		}
	}

	// --- Compute signals and store as quads ---
	signalLabel := label + ":signals"
	var quads []quadstore.Quad
	add := func(id, predicate, value string) {
		quads = append(quads, quadstore.Quad{
			Subject: id, Predicate: predicate, Object: value, Label: signalLabel,
		})
	}

	// Signal 1: page-overlap-ratio
	// For each entry, what fraction of its pages are shared with another
	// single entry? High ratio = this entry's pages are a subset of
	// something else's pages. Smells like redundancy or a redirect candidate.
	for id, e := range entries {
		if len(e.pages) == 0 {
			continue
		}
		maxOverlap := 0.0
		var maxOverlapWith string
		for otherID, other := range entries {
			if otherID == id || len(other.pages) == 0 {
				continue
			}
			overlap := 0
			for p := range e.pages {
				if other.pages[p] {
					overlap++
				}
			}
			ratio := float64(overlap) / float64(len(e.pages))
			if ratio > maxOverlap {
				maxOverlap = ratio
				maxOverlapWith = otherID
			}
		}
		add(id, "page-overlap-ratio", fmt.Sprintf("%.2f", maxOverlap))
		if maxOverlapWith != "" && maxOverlap >= 0.5 {
			add(id, "page-overlaps-with", maxOverlapWith)
		}
	}

	// Signal 2: page-co-density
	// How many other entries share this entry's pages? High density means
	// this entry lives in a crowded page neighborhood — its pages are
	// "hot" pages that many terms point to.
	for id, e := range entries {
		if len(e.pages) == 0 {
			add(id, "page-co-density", "0.00")
			continue
		}
		totalNeighbors := 0
		for p := range e.pages {
			totalNeighbors += len(pageEntries[p]) - 1 // minus self
		}
		avgDensity := float64(totalNeighbors) / float64(len(e.pages))
		add(id, "page-co-density", fmt.Sprintf("%.2f", avgDensity))
	}

	// Signal 3: term-subsumption
	// Is this entry's term a substring of another entry's term, or vice
	// versa? "pine" inside "pine trees", "bill" inside "bill adaptations".
	// Indicates potential parent-child or redirect relationship.
	for id, e := range entries {
		termLow := strings.ToLower(e.term)
		subsumes := 0  // this term appears inside other terms
		subsumed := 0  // other terms appear inside this term
		for otherID, other := range entries {
			if otherID == id {
				continue
			}
			otherLow := strings.ToLower(other.term)
			if strings.Contains(otherLow, termLow) && len(termLow) < len(otherLow) {
				subsumes++
			}
			if strings.Contains(termLow, otherLow) && len(otherLow) < len(termLow) {
				subsumed++
			}
		}
		if subsumes > 0 {
			add(id, "term-subsumes-count", fmt.Sprintf("%d", subsumes))
		}
		if subsumed > 0 {
			add(id, "term-subsumed-count", fmt.Sprintf("%d", subsumed))
		}
	}

	// Signal 4: funnel-width
	// How many sub-entries does this entry have? Wider funnel = more
	// compression. Zero = leaf node.
	for id, e := range entries {
		if len(e.subIDs) > 0 {
			add(id, "funnel-width", fmt.Sprintf("%d", len(e.subIDs)))
		}
	}

	// Signal 5: inbound-count
	// How many other entries route here via see or see-also?
	for id, e := range entries {
		if len(e.inbound) > 0 {
			add(id, "inbound-count", fmt.Sprintf("%d", len(e.inbound)))
		}
	}

	// Signal 6: orphan-pages
	// Pages this entry points to that NO other entry points to.
	// If an entry has pages only it references, those pages depend
	// entirely on this term for discoverability.
	for id, e := range entries {
		if len(e.pages) == 0 {
			continue
		}
		orphans := 0
		for p := range e.pages {
			if len(pageEntries[p]) == 1 {
				orphans++
			}
		}
		if orphans > 0 {
			ratio := float64(orphans) / float64(len(e.pages))
			add(id, "orphan-page-ratio", fmt.Sprintf("%.2f", ratio))
		}
	}

	// Signal 7: page-spread
	// How spread out are this entry's pages across the book?
	// Concentrated = probably a single discussion. Spread = recurring theme.
	for id, e := range entries {
		if len(e.pages) < 2 {
			continue
		}
		var pageNums []int
		for p := range e.pages {
			var n int
			fmt.Sscanf(p, "page:%d", &n)
			if n > 0 {
				pageNums = append(pageNums, n)
			}
		}
		if len(pageNums) < 2 {
			continue
		}
		sort.Ints(pageNums)
		spread := float64(pageNums[len(pageNums)-1]-pageNums[0]) / 72.0 // normalize to book length
		add(id, "page-spread", fmt.Sprintf("%.2f", spread))
	}

	// Signal 8: term-word-count
	// Single word, two words, three+. Affects how specific/general the term is.
	for id, e := range entries {
		words := len(strings.Fields(e.term))
		add(id, "term-word-count", fmt.Sprintf("%d", words))
	}

	// Signal 9: colloquial-distance
	// Is there a more "formal" or specific version of this term in the index?
	// Measured by: this term has 1-2 words, another term contains all those
	// words plus more. The shorter term might be a colloquial entry point.
	for id, e := range entries {
		termWords := strings.Fields(strings.ToLower(e.term))
		if len(termWords) > 2 {
			continue
		}
		moreSpecific := 0
		for otherID, other := range entries {
			if otherID == id {
				continue
			}
			otherWords := strings.Fields(strings.ToLower(other.term))
			if len(otherWords) <= len(termWords) {
				continue
			}
			// Check if all words of this term appear in the other.
			allFound := true
			for _, tw := range termWords {
				found := false
				for _, ow := range otherWords {
					if tw == ow || tw+"s" == ow || ow+"s" == tw {
						found = true
						break
					}
				}
				if !allFound {
					break
				}
				if !found {
					allFound = false
				}
			}
			if allFound {
				moreSpecific++
			}
		}
		if moreSpecific > 0 {
			add(id, "more-specific-variants", fmt.Sprintf("%d", moreSpecific))
		}
	}

	// Store all signal quads.
	if err := s.AddBatch(quads); err != nil {
		fmt.Printf("Error storing signals: %v\n", err)
		return
	}
	fmt.Printf("Stored %d signal quads (label: %s)\n\n", len(quads), signalLabel)

	// --- Now observe: what do the signals expose? ---
	printSignalObservations(s, entries, signalLabel, label)
}

func printSignalObservations(s *quadstore.Store, entries map[string]*signalEntry, signalLabel, dataLabel string) {
	fmt.Println("=== OBSERVATIONS ===\n")

	// Helper to read a signal value.
	readSignal := func(id, pred string) string {
		it := s.Match(id, pred, "", signalLabel)
		defer it.Close()
		if it.Next() {
			return it.Quad().Object
		}
		return ""
	}

	readFloat := func(id, pred string) float64 {
		v := readSignal(id, pred)
		if v == "" {
			return 0
		}
		var f float64
		fmt.Sscanf(v, "%f", &f)
		return f
	}

	// Observation 1: Entries with high page overlap — potential redirects
	// the machine doesn't know about.
	fmt.Println("--- High page overlap (>= 0.80) ---")
	fmt.Println("These entries' pages are almost entirely contained in another entry's pages.")
	fmt.Println("Observation: one might be a redirect or sub-entry of the other.\n")

	type overlapEntry struct {
		term, overlapsWith string
		ratio              float64
		ownPages           int
	}
	var overlaps []overlapEntry
	for id, e := range entries {
		ratio := readFloat(id, "page-overlap-ratio")
		if ratio >= 0.80 {
			overlapWith := readSignal(id, "page-overlaps-with")
			otherTerm := ""
			if ow, ok := entries[overlapWith]; ok {
				otherTerm = ow.term
			}
			overlaps = append(overlaps, overlapEntry{e.term, otherTerm, ratio, len(e.pages)})
		}
	}
	sort.Slice(overlaps, func(i, j int) bool { return overlaps[i].ratio > overlaps[j].ratio })
	limit := 20
	if len(overlaps) < limit {
		limit = len(overlaps)
	}
	for _, o := range overlaps[:limit] {
		fmt.Printf("  %-35s → %.0f%% overlap with %-30s (%d pages)\n",
			o.term, o.ratio*100, o.overlapsWith, o.ownPages)
	}

	// Observation 2: Entries with high co-density but few own pages —
	// sitting in hot neighborhoods but barely contributing.
	fmt.Println("\n--- High page co-density, few own pages ---")
	fmt.Println("These entries point to busy pages but contribute few references.")
	fmt.Println("Observation: possibly noise, or a concept the book touches without dwelling on.\n")

	type densityEntry struct {
		term    string
		density float64
		pages   int
	}
	var dense []densityEntry
	for id, e := range entries {
		d := readFloat(id, "page-co-density")
		if d >= 10 && len(e.pages) <= 2 {
			dense = append(dense, densityEntry{e.term, d, len(e.pages)})
		}
	}
	sort.Slice(dense, func(i, j int) bool { return dense[i].density > dense[j].density })
	limit = 15
	if len(dense) < limit {
		limit = len(dense)
	}
	for _, d := range dense[:limit] {
		fmt.Printf("  %-35s  co-density: %5.1f  pages: %d\n", d.term, d.density, d.pages)
	}

	// Observation 3: Entries with orphan pages — sole gateway to content.
	fmt.Println("\n--- Entries with orphan pages (sole reference to a page) ---")
	fmt.Println("If you remove this entry, those pages become unreachable from the index.\n")

	type orphanEntry struct {
		term        string
		orphanRatio float64
		totalPages  int
	}
	var orphans []orphanEntry
	for id, e := range entries {
		ratio := readFloat(id, "orphan-page-ratio")
		if ratio > 0 {
			orphans = append(orphans, orphanEntry{e.term, ratio, len(e.pages)})
		}
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].orphanRatio > orphans[j].orphanRatio })
	limit = 15
	if len(orphans) < limit {
		limit = len(orphans)
	}
	for _, o := range orphans[:limit] {
		fmt.Printf("  %-35s  orphan: %.0f%%  (%d pages)\n", o.term, o.orphanRatio*100, o.totalPages)
	}

	// Observation 4: High term-subsumes-count — general terms that appear
	// inside many specific terms. Natural funnel candidates.
	fmt.Println("\n--- Terms that appear inside many other terms ---")
	fmt.Println("Observation: general terms that could organize specific variants beneath them.\n")

	type subsumesEntry struct {
		term  string
		count int
	}
	var subs []subsumesEntry
	for id, e := range entries {
		v := readSignal(id, "term-subsumes-count")
		if v != "" {
			var c int
			fmt.Sscanf(v, "%d", &c)
			if c >= 3 {
				subs = append(subs, subsumesEntry{e.term, c})
			}
		}
	}
	sort.Slice(subs, func(i, j int) bool { return subs[i].count > subs[j].count })
	for _, sb := range subs {
		fmt.Printf("  %-35s  appears inside %d other terms\n", sb.term, sb.count)
	}

	// Observation 5: Widest page spread — recurring themes vs focused discussions.
	fmt.Println("\n--- Widest page spread (recurring themes) ---")
	fmt.Println("Observation: terms that span the whole book are likely major themes.\n")

	type spreadEntry struct {
		term   string
		spread float64
		pages  int
	}
	var spreads []spreadEntry
	for id, e := range entries {
		sp := readFloat(id, "page-spread")
		if sp >= 0.50 {
			spreads = append(spreads, spreadEntry{e.term, sp, len(e.pages)})
		}
	}
	sort.Slice(spreads, func(i, j int) bool { return spreads[i].spread > spreads[j].spread })
	limit = 15
	if len(spreads) < limit {
		limit = len(spreads)
	}
	for _, sp := range spreads[:limit] {
		fmt.Printf("  %-35s  spread: %.0f%% of book  (%d pages)\n", sp.term, sp.spread*100, sp.pages)
	}

	// Observation 6: Composite view — entries with multiple strong signals.
	fmt.Println("\n--- Multi-signal entries (3+ signals firing) ---")
	fmt.Println("These entries light up on multiple dimensions. Worth looking at.\n")

	type multiSignal struct {
		term    string
		signals []string
		count   int
	}
	var multi []multiSignal
	for id, e := range entries {
		var sigs []string
		if readFloat(id, "page-overlap-ratio") >= 0.80 {
			sigs = append(sigs, fmt.Sprintf("overlap:%.0f%%", readFloat(id, "page-overlap-ratio")*100))
		}
		if readFloat(id, "page-co-density") >= 10 {
			sigs = append(sigs, fmt.Sprintf("co-dense:%.0f", readFloat(id, "page-co-density")))
		}
		if v := readSignal(id, "term-subsumes-count"); v != "" {
			var c int
			fmt.Sscanf(v, "%d", &c)
			if c >= 2 {
				sigs = append(sigs, fmt.Sprintf("subsumes:%d", c))
			}
		}
		if readFloat(id, "page-spread") >= 0.40 {
			sigs = append(sigs, fmt.Sprintf("spread:%.0f%%", readFloat(id, "page-spread")*100))
		}
		if v := readSignal(id, "funnel-width"); v != "" {
			sigs = append(sigs, "funnel:"+v)
		}
		if v := readSignal(id, "inbound-count"); v != "" {
			sigs = append(sigs, "inbound:"+v)
		}
		if readFloat(id, "orphan-page-ratio") >= 0.50 {
			sigs = append(sigs, fmt.Sprintf("orphan:%.0f%%", readFloat(id, "orphan-page-ratio")*100))
		}
		if v := readSignal(id, "more-specific-variants"); v != "" {
			var c int
			fmt.Sscanf(v, "%d", &c)
			if c >= 3 {
				sigs = append(sigs, fmt.Sprintf("general:%d-variants", c))
			}
		}
		_ = math.Abs // keep import used
		if len(sigs) >= 3 {
			multi = append(multi, multiSignal{e.term, sigs, len(sigs)})
		}
	}
	sort.Slice(multi, func(i, j int) bool { return multi[i].count > multi[j].count })
	limit = 25
	if len(multi) < limit {
		limit = len(multi)
	}
	for _, m := range multi[:limit] {
		fmt.Printf("  %-35s  [%s]\n", m.term, strings.Join(m.signals, ", "))
	}
}
