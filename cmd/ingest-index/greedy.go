package main

import (
	"fmt"
	"strings"

	"github.com/dukkandcards/quadstore"
)

// greedyReduce implements a Rescorla-Wagner-inspired greedy selection.
//
// At each step, pick the entry that provides the most NEW page coverage
// (highest prediction error against the already-selected set). As entries
// are selected, they "block" future entries that cover the same pages.
//
// This doesn't decide what the final index should be. It observes how
// the machine's entries stack-rank when measured by marginal information
// contribution — and compares that ranking to what the human actually chose.
func greedyReduce(s *quadstore.Store, label string) {
	fmt.Printf("\n=== GREEDY NOISE REDUCTION (label: %s) ===\n", label)
	fmt.Println("Selecting entries by marginal page coverage (prediction error).\n")

	// Gather entries and pages.
	type greedyEntry struct {
		id    string
		term  string
		pages map[string]bool
	}

	entries := map[string]*greedyEntry{}
	it := s.Match("", "term", "", label)
	for it.Next() {
		id := it.Quad().Subject
		if _, ok := entries[id]; !ok {
			entries[id] = &greedyEntry{id: id, term: it.Quad().Object, pages: map[string]bool{}}
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

	// Find all pages in the corpus.
	allPages := map[string]bool{}
	for _, e := range entries {
		for p := range e.pages {
			allPages[p] = true
		}
	}
	totalPages := len(allPages)

	// Greedy selection.
	covered := map[string]bool{}
	selected := []string{}
	remaining := make(map[string]*greedyEntry)
	for id, e := range entries {
		if len(e.pages) > 0 { // only consider entries with pages
			remaining[id] = e
		}
	}

	type selectionStep struct {
		term        string
		newPages    int
		totalCover  int
		coverPct    float64
		marginalPct float64
	}
	var steps []selectionStep

	for len(remaining) > 0 {
		// Find entry with most uncovered pages.
		bestID := ""
		bestNew := 0
		for id, e := range remaining {
			newPages := 0
			for p := range e.pages {
				if !covered[p] {
					newPages++
				}
			}
			if newPages > bestNew || (newPages == bestNew && bestID == "") {
				bestNew = newPages
				bestID = id
			}
		}

		if bestNew == 0 {
			break // no remaining entry adds new coverage
		}

		best := remaining[bestID]
		for p := range best.pages {
			covered[p] = true
		}
		selected = append(selected, bestID)
		delete(remaining, bestID)

		coverPct := float64(len(covered)) / float64(totalPages) * 100
		marginalPct := float64(bestNew) / float64(totalPages) * 100

		steps = append(steps, selectionStep{
			best.term, bestNew, len(covered), coverPct, marginalPct,
		})
	}

	// Report: first N selections (where the signal is strongest).
	fmt.Printf("Total pages in corpus: %d\n", totalPages)
	fmt.Printf("Entries with pages: %d\n", len(entries)-len(remaining))
	fmt.Printf("Entries needed for full coverage: %d\n\n", len(selected))

	fmt.Println("--- Selection order (by marginal coverage) ---")
	fmt.Printf("  %-4s  %-35s  %6s  %6s  %7s\n", "#", "Term", "New", "Total", "Cover%")
	fmt.Printf("  %-4s  %-35s  %6s  %6s  %7s\n", "--", "----", "---", "-----", "------")

	for i, step := range steps {
		if i >= 40 {
			fmt.Printf("  ... %d more entries for full coverage\n", len(steps)-40)
			break
		}
		fmt.Printf("  %-4d  %-35s  %6d  %6d  %6.1f%%\n",
			i+1, step.term, step.newPages, step.totalCover, step.coverPct)
	}

	// Coverage curve: how many entries to reach 80%, 90%, 95%, 100%.
	fmt.Println("\n--- Coverage curve ---")
	thresholds := []float64{50, 70, 80, 90, 95, 100}
	for _, thresh := range thresholds {
		for i, step := range steps {
			if step.coverPct >= thresh {
				fmt.Printf("  %3.0f%% coverage: %d entries\n", thresh, i+1)
				break
			}
		}
	}

	// If we have a reference label, compare greedy selection against human choices.
	if label == "generated" {
		termLookup := map[string]string{}
		for id, e := range entries {
			termLookup[id] = e.term
		}
		compareGreedyToHuman(s, selected, termLookup)
	}
}

func compareGreedyToHuman(s *quadstore.Store, selectedIDs []string, termLookup map[string]string) {
	fmt.Println("\n--- Greedy selection vs human index ---")

	// Collect human terms.
	humanTerms := map[string]bool{}
	it := s.Match("", "term", "", "reference")
	for it.Next() {
		humanTerms[strings.ToLower(it.Quad().Object)] = true
	}
	it.Close()

	// Check how many of the greedy-selected entries match human terms.
	matchedInTop := make([]int, 0)
	for i, id := range selectedIDs {
		term, ok := termLookup[id]
		if !ok {
			continue
		}
		if humanTerms[strings.ToLower(term)] {
			matchedInTop = append(matchedInTop, i+1)
		}
	}

	// Report at thresholds.
	checkpoints := []int{10, 25, 50, 100, 150, len(selectedIDs)}
	fmt.Printf("\n  %-20s  %10s  %10s\n", "Greedy top-N", "Human hits", "Hit rate")
	fmt.Printf("  %-20s  %10s  %10s\n", "------------", "----------", "--------")
	for _, cp := range checkpoints {
		if cp > len(selectedIDs) {
			cp = len(selectedIDs)
		}
		hits := 0
		for _, rank := range matchedInTop {
			if rank <= cp {
				hits++
			}
		}
		fmt.Printf("  top %-15d  %10d  %9.1f%%\n", cp, hits, float64(hits)/float64(cp)*100)
		if cp == len(selectedIDs) {
			break
		}
	}

	// Show early greedy selections that match human.
	fmt.Println("\n  Greedy selections that match human index (first 20):")
	shown := 0
	for _, rank := range matchedInTop {
		if shown >= 20 {
			break
		}
		if rank <= len(selectedIDs) {
			term := termLookup[selectedIDs[rank-1]]
			fmt.Printf("    #%-3d  %s\n", rank, term)
			shown++
		}
	}
}
