package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dukkandcards/quadstore"
)

// subtractToIndex starts with all machine-generated entries and removes
// what doesn't belong, step by step. Each step is a named elimination
// with a count of what was removed and what survives.
//
// An index is a subtraction process:
//   Book (thousands of words)
//     → extract terms (700+)
//     → remove atoms (single words too fine-grained)
//     → remove over-general (terms that are the book's subject)
//     → remove fully redundant (every page already reachable)
//     → remove not-about (term appears but page isn't about it)
//     → remove vocabulary fragments (not composed concepts)
//     → what remains is the candidate index
//
// The only ADDITION is routing structure (See→target), which predicts
// user vocabulary. That's the part the machine can't do.
func subtractToIndex(s *quadstore.Store) {
	fmt.Println("\n=== SUBTRACTION PIPELINE ===")
	fmt.Println("Starting with all generated entries. Removing what doesn't belong.")

	// Collect human terms for comparison.
	humanTerms := map[string]bool{}
	it := s.Match("", "term", "", "reference")
	for it.Next() {
		humanTerms[strings.ToLower(it.Quad().Object)] = true
	}
	it.Close()

	// Gather all generated entries with data.
	type subEntry struct {
		id             string
		term           string
		pageCount      int
		wordCount      int
		subsumesCount  int
		aboutnessMax   float64
		redundancy     float64
		foregroundness float64
		childCount     int
		parentCount    int
		pages          []string
	}

	entries := map[string]*subEntry{}
	it = s.Match("", "term", "", "generated")
	for it.Next() {
		id := it.Quad().Subject
		if _, ok := entries[id]; !ok {
			entries[id] = &subEntry{id: id, term: it.Quad().Object}
		}
	}
	it.Close()

	for id, e := range entries {
		pit := s.Match(id, "has-page", "", "generated")
		for pit.Next() {
			e.pageCount++
			e.pages = append(e.pages, pit.Quad().Object)
		}
		pit.Close()
		e.wordCount = len(strings.Fields(e.term))
	}

	// Read signals.
	readFloat := func(id, pred, label string) float64 {
		qit := s.Match(id, pred, "", label)
		defer qit.Close()
		if qit.Next() {
			var f float64
			fmt.Sscanf(qit.Quad().Object, "%f", &f)
			return f
		}
		return 0
	}
	readInt := func(id, pred, label string) int {
		qit := s.Match(id, pred, "", label)
		defer qit.Close()
		if qit.Next() {
			var n int
			fmt.Sscanf(qit.Quad().Object, "%d", &n)
			return n
		}
		return 0
	}

	for id, e := range entries {
		e.subsumesCount = readInt(id, "term-subsumes-count", "generated:signals")
		e.aboutnessMax = readFloat(id, "aboutness-max", "generated:aboutness")
		e.redundancy = readFloat(id, "redundancy", "generated:contention")
		e.foregroundness = readFloat(id, "foregroundness", "generated:background")

		// Composition chain.
		eLow := strings.ToLower(e.term)
		for _, other := range entries {
			if other.id == id {
				continue
			}
			oLow := strings.ToLower(other.term)
			if len(eLow) > len(oLow) && containsWholeWord(eLow, oLow) {
				e.childCount++
			}
			if len(oLow) > len(eLow) && containsWholeWord(oLow, eLow) {
				e.parentCount++
			}
		}
	}

	// --- Subtraction steps ---

	// Track survivors.
	survivors := map[string]bool{}
	for id := range entries {
		survivors[id] = true
	}

	remove := func(stepName string, shouldRemove func(e *subEntry) bool) {
		removed := 0
		removedHuman := 0
		_ = len(survivors)

		for id := range survivors {
			e := entries[id]
			if shouldRemove(e) {
				delete(survivors, id)
				removed++
				if humanTerms[strings.ToLower(e.term)] {
					removedHuman++
				}
			}
		}

		surviving := len(survivors)
		humanHits := 0
		for id := range survivors {
			if humanTerms[strings.ToLower(entries[id].term)] {
				humanHits++
			}
		}

		fmt.Printf("  %-45s  -%4d  →%4d survive  (human: %d match, %d lost)\n",
			stepName, removed, surviving, humanHits, removedHuman)
	}

	totalStart := len(survivors)
	startHuman := 0
	for id := range survivors {
		if humanTerms[strings.ToLower(entries[id].term)] {
			startHuman++
		}
	}
	fmt.Printf("  %-45s  %5d  →%4d survive  (human: %d match)\n",
		"START: all generated entries", totalStart, totalStart, startHuman)

	// Step 1: Remove entries with no pages (pure orphans).
	remove("Remove: no pages", func(e *subEntry) bool {
		return e.pageCount == 0
	})

	// Step 2: Remove single-character or very short terms.
	remove("Remove: term < 3 characters", func(e *subEntry) bool {
		return len(e.term) < 3
	})

	// Step 3: Remove terms that are just numbers or measurements.
	remove("Remove: numeric/measurement terms", func(e *subEntry) bool {
		t := strings.ToLower(e.term)
		for _, w := range strings.Fields(t) {
			cleaned := strings.Trim(w, ".-+")
			isNum := true
			for _, c := range cleaned {
				if c < '0' || c > '9' {
					isNum = false
					break
				}
			}
			if !isNum {
				return false
			}
		}
		return true // all words are numbers
	})

	// Step 4: Remove atoms (1-word terms) that subsume too many others.
	// "woodpecker" subsumes 55 terms — it's the book's subject, not an entry.
	// But keep 1-word terms that subsume few (like "acorns" or "grubs").
	remove("Remove: 1-word terms subsuming 5+ others", func(e *subEntry) bool {
		return e.wordCount == 1 && e.subsumesCount >= 5
	})

	// Step 5: Remove over-general multi-word terms.
	// Terms that subsume 10+ others are too broad.
	remove("Remove: multi-word terms subsuming 10+ others", func(e *subEntry) bool {
		return e.wordCount >= 2 && e.subsumesCount >= 10
	})

	// Step 6: Remove fully redundant (high blocking).
	// Every page this entry points to has 50+ other entries pointing to it.
	remove("Remove: fully redundant (avg 50+ other paths)", func(e *subEntry) bool {
		return e.redundancy >= 50
	})

	// Step 7: Remove terms with very high coverage (background).
	// If a term appears on >25% of pages, it's the book's subject matter.
	remove("Remove: >25% page coverage (background)", func(e *subEntry) bool {
		return e.pageCount > 0 && float64(e.pageCount)/68.0 > 0.25
	})

	// Step 8: Remove long noun phrases (4+ words) that are likely fragments.
	// "female of american three-toed woodpecker" is a species key fragment.
	remove("Remove: 4+ word terms (likely fragments)", func(e *subEntry) bool {
		return e.wordCount >= 4
	})

	// Step 9: Remove terms where aboutness is zero on all pages.
	// The term appears on pages but no page is actually about it.
	remove("Remove: zero aboutness (mentioned, not discussed)", func(e *subEntry) bool {
		return e.pageCount > 0 && e.aboutnessMax < 0.01
	})

	// Step 10: Remove terms that are purely geographic/directional.
	remove("Remove: geographic/directional fragments", func(e *subEntry) bool {
		geo := map[string]bool{
			"north": true, "south": true, "east": true, "west": true,
			"northern": true, "southern": true, "eastern": true, "western": true,
			"pacific": true, "atlantic": true, "gulf": true, "coast": true,
			"states": true, "united": true, "lower": true, "upper": true,
		}
		for _, w := range strings.Fields(strings.ToLower(e.term)) {
			if !geo[w] {
				return false
			}
		}
		return true
	})

	// --- Report survivors ---
	fmt.Printf("\n  TOTAL: %d → %d (%.0f%% reduction)\n\n",
		totalStart, len(survivors),
		(1-float64(len(survivors))/float64(totalStart))*100)

	// Collect and sort survivors.
	type survivorDisplay struct {
		term      string
		pages     int
		words     int
		aboutness float64
		fg        float64
	}
	var survList []survivorDisplay
	for id := range survivors {
		e := entries[id]
		survList = append(survList, survivorDisplay{
			e.term, e.pageCount, e.wordCount, e.aboutnessMax, e.foregroundness,
		})
	}
	sort.Slice(survList, func(i, j int) bool {
		if survList[i].pages != survList[j].pages {
			return survList[i].pages > survList[j].pages
		}
		return survList[i].term < survList[j].term
	})

	humanHits := 0
	fmt.Printf("  %-35s  %5s  %5s  %7s  %5s\n", "Term", "Pages", "Words", "MaxAbt", "Fg")
	fmt.Printf("  %-35s  %5s  %5s  %7s  %5s\n", "----", "-----", "-----", "------", "--")
	for _, sv := range survList {
		match := ""
		if humanTerms[strings.ToLower(sv.term)] {
			match = " ← HUMAN"
			humanHits++
		}
		fmt.Printf("  %-35s  %5d  %5d  %7.2f  %5.2f%s\n",
			sv.term, sv.pages, sv.words, sv.aboutness, sv.fg, match)
	}

	fmt.Printf("\n  Survivors: %d  Human matches: %d (%.1f%%)\n",
		len(survList), humanHits, float64(humanHits)/float64(len(survList))*100)
	fmt.Printf("  Human reference has %d unique terms\n", len(humanTerms))
	fmt.Printf("  Recall: %.1f%% of human terms survived\n",
		float64(humanHits)/float64(len(humanTerms))*100)
}
