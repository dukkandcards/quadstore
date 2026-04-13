package main

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/dukkandcards/quadstore"
)

// computeWeightedEdges assigns a contextual weight to every has-page edge.
//
// A flat has-page edge says "this term appears on this page." But not all
// appearances are equal. "Woodpecker" on page 5 is noise (the book's subject).
// "Body adaptations" on page 5 is signal (substantive discussion).
//
// Four factors determine the weight:
//
// 1. Term rarity (IDF) — how many pages does this term appear on?
//    Fewer = each appearance is more informative.
//
// 2. Page specificity — how many entries point to this page?
//    Fewer = the page is about something specific, not everything.
//
// 3. Co-occurrence coherence — do other terms on this page relate to
//    this term? (share words, parent/child relationship)
//    Higher = the page is substantively about this term.
//
// 4. Neighborhood continuity — does this term appear on adjacent pages?
//    Consecutive pages = sustained discussion. Isolated page = passing mention.
//
// The weight is an observation, not a judgment. It says "this page reference
// is more/less contextually loaded than that one."
type weightedEntry struct {
	id    string
	term  string
	pages []string
}

func computeWeightedEdges(s *quadstore.Store, label string) {
	fmt.Printf("\n=== WEIGHTED PAGE REFERENCES (label: %s) ===\n\n", label)

	wLabel := label + ":weighted"

	entries := map[string]*weightedEntry{}
	it := s.Match("", "term", "", label)
	for it.Next() {
		id := it.Quad().Subject
		if _, ok := entries[id]; !ok {
			entries[id] = &weightedEntry{id: id, term: it.Quad().Object}
		}
	}
	it.Close()

	for id, e := range entries {
		pit := s.Match(id, "has-page", "", label)
		for pit.Next() {
			e.pages = append(e.pages, pit.Quad().Object)
		}
		pit.Close()
	}

	// Page → entries reverse index.
	pageEntries := map[string][]string{}
	for id, e := range entries {
		for _, p := range e.pages {
			pageEntries[p] = append(pageEntries[p], id)
		}
	}

	totalPages := len(pageEntries)
	if totalPages == 0 {
		fmt.Println("No pages found.")
		return
	}

	// Precompute page numbers for continuity check.
	pageNum := func(p string) int {
		var n int
		fmt.Sscanf(p, "page:%d", &n)
		return n
	}

	// --- Factor 1: Term rarity (IDF) ---
	// IDF = log(totalPages / pagesWithTerm)
	// "woodpecker" on 21/68 pages → IDF = log(68/21) = 1.17
	// "bones of" on 3/68 pages → IDF = log(68/3) = 3.12
	termIDF := map[string]float64{}
	for id, e := range entries {
		if len(e.pages) == 0 {
			continue
		}
		idf := math.Log(float64(totalPages) / float64(len(e.pages)))
		termIDF[id] = idf
	}

	// --- Compute per-edge weight ---
	type edgeWeight struct {
		entryID   string
		term      string
		page      string
		idf       float64 // term rarity
		pageSpec  float64 // page specificity
		coherence float64 // co-occurrence coherence
		contiguity float64 // neighborhood continuity
		combined  float64 // product of all factors
	}

	var weights []edgeWeight
	var quads []quadstore.Quad

	for id, e := range entries {
		if len(e.pages) == 0 {
			continue
		}

		// Precompute page numbers for this entry (for continuity).
		entryPageNums := map[int]bool{}
		for _, p := range e.pages {
			entryPageNums[pageNum(p)] = true
		}

		idf := termIDF[id]
		termWords := strings.Fields(strings.ToLower(e.term))

		for _, page := range e.pages {
			// Factor 2: Page specificity.
			// Fewer entries on this page = more specific.
			// Normalize: 1/log(entriesOnPage + 1)
			entriesOnPage := len(pageEntries[page])
			pageSpec := 1.0 / math.Log(float64(entriesOnPage)+1)

			// Factor 3: Co-occurrence coherence.
			// How many other entries on this page share words with this term?
			coherent := 0
			for _, otherID := range pageEntries[page] {
				if otherID == id {
					continue
				}
				other := entries[otherID]
				if other == nil {
					continue
				}
				otherWords := strings.Fields(strings.ToLower(other.term))
				for _, tw := range termWords {
					found := false
					for _, ow := range otherWords {
						if tw == ow || tw+"s" == ow || ow+"s" == tw {
							found = true
							break
						}
					}
					if found {
						coherent++
						break
					}
				}
			}
			// Normalize: coherent neighbors / total neighbors.
			coherence := 0.0
			if entriesOnPage > 1 {
				coherence = float64(coherent) / float64(entriesOnPage-1)
			}

			// Factor 4: Neighborhood continuity.
			// Does this term also appear on adjacent pages?
			pn := pageNum(page)
			adjacent := 0
			if entryPageNums[pn-1] {
				adjacent++
			}
			if entryPageNums[pn+1] {
				adjacent++
			}
			// 0 = isolated mention, 1 = one neighbor, 2 = both neighbors (sustained).
			contiguity := float64(adjacent) / 2.0

			// Combined weight (geometric mean to avoid any single factor dominating).
			// Add small epsilon to avoid zero.
			eps := 0.01
			combined := math.Pow(
				(idf+eps)*(pageSpec+eps)*(coherence+eps)*(contiguity+eps),
				0.25,
			)

			w := edgeWeight{
				entryID:    id,
				term:       e.term,
				page:       page,
				idf:        idf,
				pageSpec:   pageSpec,
				coherence:  coherence,
				contiguity: contiguity,
				combined:   combined,
			}
			weights = append(weights, w)

			quads = append(quads, quadstore.Quad{
				Subject:   id,
				Predicate: "page-weight:" + page,
				Object:    fmt.Sprintf("%.4f", combined),
				Label:     wLabel,
			})
		}
	}

	// Store entry-level aggregate weights.
	entryWeights := map[string][]float64{}
	for _, w := range weights {
		entryWeights[w.entryID] = append(entryWeights[w.entryID], w.combined)
	}
	for id, ww := range entryWeights {
		sum := 0.0
		for _, w := range ww {
			sum += w
		}
		avg := sum / float64(len(ww))
		quads = append(quads, quadstore.Quad{
			Subject: id, Predicate: "page-weight-sum",
			Object: fmt.Sprintf("%.4f", sum), Label: wLabel,
		})
		quads = append(quads, quadstore.Quad{
			Subject: id, Predicate: "page-weight-avg",
			Object: fmt.Sprintf("%.4f", avg), Label: wLabel,
		})
	}

	if err := s.AddBatch(quads); err != nil {
		fmt.Printf("Error storing weighted quads: %v\n", err)
		return
	}
	fmt.Printf("Stored %d weighted edge quads (label: %s)\n\n", len(quads), wLabel)

	// --- Observations ---

	// Top weighted entries by sum (total contextual load).
	type entrySummary struct {
		term    string
		sum     float64
		avg     float64
		pages   int
		idf     float64
	}
	var summaries []entrySummary
	for id, ww := range entryWeights {
		e := entries[id]
		if e == nil {
			continue
		}
		sum := 0.0
		for _, w := range ww {
			sum += w
		}
		avg := sum / float64(len(ww))
		summaries = append(summaries, entrySummary{e.term, sum, avg, len(ww), termIDF[id]})
	}

	fmt.Println("--- Highest total contextual weight (sum across all pages) ---")
	fmt.Println("Entries whose page references are collectively most informative.\n")
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].sum > summaries[j].sum })
	limit := 25
	if len(summaries) < limit {
		limit = len(summaries)
	}
	fmt.Printf("  %-35s  %8s  %8s  %5s  %6s\n", "Term", "Sum", "Avg", "Pages", "IDF")
	fmt.Printf("  %-35s  %8s  %8s  %5s  %6s\n", "----", "---", "---", "-----", "---")
	for _, sm := range summaries[:limit] {
		fmt.Printf("  %-35s  %8.3f  %8.4f  %5d  %6.2f\n",
			sm.term, sm.sum, sm.avg, sm.pages, sm.idf)
	}

	fmt.Println("\n--- Highest average contextual weight (per page reference) ---")
	fmt.Println("Entries where each individual page reference is most meaningful.\n")
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].avg > summaries[j].avg })
	fmt.Printf("  %-35s  %8s  %8s  %5s  %6s\n", "Term", "Avg", "Sum", "Pages", "IDF")
	fmt.Printf("  %-35s  %8s  %8s  %5s  %6s\n", "----", "---", "---", "-----", "---")
	for _, sm := range summaries[:limit] {
		fmt.Printf("  %-35s  %8.4f  %8.3f  %5d  %6.2f\n",
			sm.term, sm.avg, sm.sum, sm.pages, sm.idf)
	}

	// Lowest weighted entries (most noise).
	fmt.Println("\n--- Lowest total contextual weight (most noise-like) ---")
	fmt.Println("Entries whose page references carry the least contextual information.\n")
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].sum < summaries[j].sum })
	limit = 20
	if len(summaries) < limit {
		limit = len(summaries)
	}
	fmt.Printf("  %-35s  %8s  %8s  %5s  %6s\n", "Term", "Sum", "Avg", "Pages", "IDF")
	fmt.Printf("  %-35s  %8s  %8s  %5s  %6s\n", "----", "---", "---", "-----", "---")
	for _, sm := range summaries[:limit] {
		fmt.Printf("  %-35s  %8.3f  %8.4f  %5d  %6.2f\n",
			sm.term, sm.sum, sm.avg, sm.pages, sm.idf)
	}

	// Now run weighted greedy reduction.
	weightedGreedyReduce(s, entries, entryWeights, pageEntries, label)
}

// weightedGreedyReduce picks entries by WEIGHTED marginal coverage instead
// of raw page count. An entry that covers 3 high-weight pages beats an entry
// that covers 5 low-weight pages.
func weightedGreedyReduce(s *quadstore.Store, entries map[string]*weightedEntry,
	entryWeights map[string][]float64, pageEntries map[string][]string, label string) {

	fmt.Println("\n--- Weighted greedy reduction ---")
	fmt.Println("Selecting entries by weighted marginal coverage.\n")

	// Build per-entry per-page weight lookup.
	type pageWeight struct {
		page   string
		weight float64
	}
	entryPageWeights := map[string][]pageWeight{}
	for id, e := range entries {
		if len(e.pages) == 0 {
			continue
		}
		// Read weights from the per-edge weights we computed.
		// We stored them as individual weights per edge, but for greedy
		// selection we need the entry's total uncovered weight.
		// Reconstruct from entry's page list and the weight arrays.
		ww := entryWeights[id]
		for i, p := range e.pages {
			w := 0.0
			if i < len(ww) {
				w = ww[i]
			}
			entryPageWeights[id] = append(entryPageWeights[id], pageWeight{p, w})
		}
	}

	covered := map[string]bool{}
	var selected []string

	type step struct {
		term           string
		weightGained   float64
		totalWeight    float64
		pagesGained    int
		totalPages     int
	}
	var steps []step

	totalWeight := 0.0
	for _, ww := range entryWeights {
		for _, w := range ww {
			totalWeight += w
		}
	}

	accumWeight := 0.0
	accumPages := 0
	allPages := map[string]bool{}
	for p := range pageEntries {
		allPages[p] = true
	}

	remaining := map[string]bool{}
	for id, e := range entries {
		if len(e.pages) > 0 {
			remaining[id] = true
		}
	}

	for len(remaining) > 0 {
		bestID := ""
		bestWeight := 0.0
		bestNewPages := 0

		for id := range remaining {
			pw := entryPageWeights[id]
			wGain := 0.0
			newP := 0
			for _, p := range pw {
				if !covered[p.page] {
					wGain += p.weight
					newP++
				}
			}
			if wGain > bestWeight {
				bestWeight = wGain
				bestID = id
				bestNewPages = newP
			}
		}

		if bestWeight <= 0 || bestID == "" {
			break
		}

		for _, p := range entryPageWeights[bestID] {
			covered[p.page] = true
		}
		accumWeight += bestWeight
		accumPages += bestNewPages
		selected = append(selected, bestID)
		delete(remaining, bestID)

		steps = append(steps, step{
			entries[bestID].term, bestWeight, accumWeight,
			bestNewPages, accumPages,
		})
	}

	fmt.Printf("  %-4s  %-35s  %10s  %6s  %6s\n", "#", "Term", "Wt gained", "New pg", "Total")
	fmt.Printf("  %-4s  %-35s  %10s  %6s  %6s\n", "--", "----", "---------", "------", "-----")
	limit := 30
	if len(steps) < limit {
		limit = len(steps)
	}
	for i, st := range steps[:limit] {
		fmt.Printf("  %-4d  %-35s  %10.3f  %6d  %6d\n",
			i+1, st.term, st.weightGained, st.pagesGained, st.totalPages)
	}
	if len(steps) > limit {
		fmt.Printf("  ... %d more\n", len(steps)-limit)
	}

	// Compare to human.
	if label == "generated" {
		humanTerms := map[string]bool{}
		it := s.Match("", "term", "", "reference")
		for it.Next() {
			humanTerms[strings.ToLower(it.Quad().Object)] = true
		}
		it.Close()

		fmt.Println("\n  Weighted greedy vs human (first 25 selections):")
		for i, id := range selected {
			if i >= 25 {
				break
			}
			e := entries[id]
			match := ""
			if humanTerms[strings.ToLower(e.term)] {
				match = " ← HUMAN MATCH"
			}
			fmt.Printf("    #%-3d  %-35s%s\n", i+1, e.term, match)
		}
	}
}
