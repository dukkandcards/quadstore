package main

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/dukkandcards/quadstore"
)

// computeBackgroundRate measures how much each entry's page distribution
// looks like "background" vs "foreground."
//
// The intuition: a term that appears on 30% of pages at roughly uniform
// intervals is the book's SUBJECT, not an index-worthy concept. It's
// background. A term that appears on 5% of pages clustered in one chapter
// is FOREGROUND — the book discusses it substantively in a specific place.
//
// For an INDEX, background = noise, penalize it.
// For a CHART, the same signal inverts — high repetition means the lawyer
// is afraid of missing something, and GAPS in repetition are the signal.
//
// We compute the observation; interpretation depends on the product.
type bgEntry struct {
	id    string
	term  string
	pages []int // page numbers
}

type bgResult struct {
	term           string
	coverageRate   float64
	clusterScore   float64
	gapVariance    float64
	longestRun     int
	backgroundness float64
	foregroundness float64
}

func computeBackgroundRate(s *quadstore.Store, label string) {
	fmt.Printf("\n=== BACKGROUND RATE ANALYSIS (label: %s) ===\n\n", label)

	bgLabel := label + ":background"

	entries := map[string]*bgEntry{}
	it := s.Match("", "term", "", label)
	for it.Next() {
		id := it.Quad().Subject
		if _, ok := entries[id]; !ok {
			entries[id] = &bgEntry{id: id, term: it.Quad().Object}
		}
	}
	it.Close()

	for id, e := range entries {
		pit := s.Match(id, "has-page", "", label)
		for pit.Next() {
			var n int
			fmt.Sscanf(pit.Quad().Object, "page:%d", &n)
			if n > 0 {
				e.pages = append(e.pages, n)
			}
		}
		pit.Close()
		sort.Ints(e.pages)
	}

	// Find book extent.
	minPage, maxPage := math.MaxInt32, 0
	for _, e := range entries {
		for _, p := range e.pages {
			if p < minPage {
				minPage = p
			}
			if p > maxPage {
				maxPage = p
			}
		}
	}
	bookLength := maxPage - minPage + 1
	if bookLength <= 0 {
		fmt.Println("No page data.")
		return
	}

	var quads []quadstore.Quad
	add := func(id, pred, val string) {
		quads = append(quads, quadstore.Quad{Subject: id, Predicate: pred, Object: val, Label: bgLabel})
	}

	var results []bgResult

	for id, e := range entries {
		if len(e.pages) < 1 {
			continue
		}

		// Coverage rate: what fraction of the book does this term touch?
		coverageRate := float64(len(e.pages)) / float64(bookLength)

		// Cluster score: are the pages bunched together or spread out?
		// Measured by: (actual spread) / (expected spread if uniformly distributed)
		// Low ratio = clustered. High ratio = spread out.
		var clusterScore float64
		var gapVariance float64
		longestRun := 1

		if len(e.pages) >= 2 {
			// Actual spread = max - min.
			actualSpread := float64(e.pages[len(e.pages)-1] - e.pages[0])
			// Expected spread for N uniformly distributed pages in a book of L pages.
			// For N pages in L slots, expected max-min ≈ L * (N-1)/(N+1)
			expectedSpread := float64(bookLength) * float64(len(e.pages)-1) / float64(len(e.pages)+1)

			if expectedSpread > 0 {
				spreadRatio := actualSpread / expectedSpread
				// Invert: 0 = maximally spread (background-like), 1 = maximally clustered
				clusterScore = math.Max(0, 1.0-spreadRatio)
			}

			// Gap analysis: variance in gaps between consecutive pages.
			// Uniform gaps = background. Variable gaps = foreground (burst + silence).
			gaps := make([]float64, 0, len(e.pages)-1)
			currentRun := 1
			for i := 1; i < len(e.pages); i++ {
				gap := float64(e.pages[i] - e.pages[i-1])
				gaps = append(gaps, gap)
				if e.pages[i]-e.pages[i-1] == 1 {
					currentRun++
					if currentRun > longestRun {
						longestRun = currentRun
					}
				} else {
					currentRun = 1
				}
			}

			if len(gaps) > 0 {
				// Compute coefficient of variation (std/mean).
				mean := 0.0
				for _, g := range gaps {
					mean += g
				}
				mean /= float64(len(gaps))

				variance := 0.0
				for _, g := range gaps {
					variance += (g - mean) * (g - mean)
				}
				variance /= float64(len(gaps))

				if mean > 0 {
					gapVariance = math.Sqrt(variance) / mean // CV
				}
			}
		}

		// Backgroundness: combines coverage rate and distribution uniformity.
		// High coverage + low clustering + low gap variance = BACKGROUND
		// (the term is everywhere at roughly uniform intervals)
		//
		// Low coverage + high clustering + high gap variance = FOREGROUND
		// (the term appears in bursts in specific places)
		//
		// Coverage rate is the dominant signal. A term on 30% of pages is
		// almost certainly background regardless of distribution.
		// A term on 3% of pages is almost certainly foreground.
		// Distribution (cluster, gap variance) disambiguates the middle range.

		uniformity := 1.0 - clusterScore // high = uniformly distributed
		backgroundness := coverageRate*0.5 + uniformity*0.3 + (1.0-math.Min(gapVariance, 1.0))*0.2
		foregroundness := 1.0 - backgroundness

		add(id, "coverage-rate", fmt.Sprintf("%.4f", coverageRate))
		add(id, "cluster-score", fmt.Sprintf("%.4f", clusterScore))
		add(id, "gap-variance", fmt.Sprintf("%.4f", gapVariance))
		add(id, "longest-run", fmt.Sprintf("%d", longestRun))
		add(id, "backgroundness", fmt.Sprintf("%.4f", backgroundness))
		add(id, "foregroundness", fmt.Sprintf("%.4f", foregroundness))

		results = append(results, bgResult{
			e.term, coverageRate, clusterScore, gapVariance,
			longestRun, backgroundness, foregroundness,
		})
	}

	if err := s.AddBatch(quads); err != nil {
		fmt.Printf("Error storing background quads: %v\n", err)
		return
	}
	fmt.Printf("Stored %d background rate quads (label: %s)\n\n", len(quads), bgLabel)

	// --- Observations ---

	// Most background-like (noise for an index, signal for a chart).
	fmt.Println("--- Most background-like (uniform, high coverage) ---")
	fmt.Println("For INDEX: these are noise — penalize them.")
	fmt.Println("For CHART: these would be defensive repetition — investigate WHY.\n")
	sort.Slice(results, func(i, j int) bool { return results[i].backgroundness > results[j].backgroundness })
	fmt.Printf("  %-35s  %6s  %7s  %7s  %5s  %5s\n",
		"Term", "Cover", "Cluster", "GapVar", "Run", "Bg")
	fmt.Printf("  %-35s  %6s  %7s  %7s  %5s  %5s\n",
		"----", "-----", "-------", "------", "---", "--")
	limit := 20
	if len(results) < limit {
		limit = len(results)
	}
	for _, r := range results[:limit] {
		fmt.Printf("  %-35s  %5.1f%%  %7.3f  %7.3f  %5d  %.3f\n",
			r.term, r.coverageRate*100, r.clusterScore, r.gapVariance,
			r.longestRun, r.backgroundness)
	}

	// Most foreground-like (signal for an index).
	fmt.Println("\n--- Most foreground-like (clustered, low coverage) ---")
	fmt.Println("For INDEX: these are signal — the book discusses them in specific places.\n")
	sort.Slice(results, func(i, j int) bool { return results[i].foregroundness > results[j].foregroundness })
	fmt.Printf("  %-35s  %6s  %7s  %7s  %5s  %5s\n",
		"Term", "Cover", "Cluster", "GapVar", "Run", "Fg")
	fmt.Printf("  %-35s  %6s  %7s  %7s  %5s  %5s\n",
		"----", "-----", "-------", "------", "---", "--")
	for _, r := range results[:limit] {
		fmt.Printf("  %-35s  %5.1f%%  %7.3f  %7.3f  %5d  %.3f\n",
			r.term, r.coverageRate*100, r.clusterScore, r.gapVariance,
			r.longestRun, r.foregroundness)
	}

	// Now: foreground-weighted greedy.
	fmt.Println("\n--- Foreground-weighted greedy reduction ---")
	fmt.Println("Selecting by marginal coverage × foregroundness.\n")
	foregroundGreedy(s, entries, results, label)
}

func foregroundGreedy(s *quadstore.Store, entries map[string]*bgEntry,
	results []bgResult, label string) {

	// Build foregroundness lookup.
	fgScore := map[string]float64{}
	for _, r := range results {
		for id, e := range entries {
			if e.term == r.term {
				fgScore[id] = r.foregroundness
				break
			}
		}
	}

	// Build page sets.
	pageSet := map[string]map[string]bool{}
	for id, e := range entries {
		pages := map[string]bool{}
		for _, p := range e.pages {
			pages[fmt.Sprintf("page:%d", p)] = true
		}
		if len(pages) > 0 {
			pageSet[id] = pages
		}
	}

	covered := map[string]bool{}
	remaining := map[string]bool{}
	for id := range pageSet {
		remaining[id] = true
	}

	type step struct {
		term  string
		score float64
		newPg int
		total int
	}
	var steps []step
	totalCovered := 0

	for len(remaining) > 0 {
		bestID := ""
		bestScore := 0.0
		bestNew := 0

		for id := range remaining {
			newPages := 0
			for p := range pageSet[id] {
				if !covered[p] {
					newPages++
				}
			}
			// Score = new pages × foregroundness.
			// A foreground term covering 3 new pages beats a background
			// term covering 5 new pages.
			fg := fgScore[id]
			if fg == 0 {
				fg = 0.5 // default for entries without computed fg
			}
			score := float64(newPages) * fg
			if score > bestScore {
				bestScore = score
				bestID = id
				bestNew = newPages
			}
		}

		if bestNew == 0 || bestID == "" {
			break
		}

		for p := range pageSet[bestID] {
			covered[p] = true
		}
		totalCovered += bestNew
		steps = append(steps, step{entries[bestID].term, bestScore, bestNew, totalCovered})
		delete(remaining, bestID)
	}

	fmt.Printf("  %-4s  %-35s  %8s  %6s  %6s\n", "#", "Term", "FgScore", "New", "Total")
	fmt.Printf("  %-4s  %-35s  %8s  %6s  %6s\n", "--", "----", "-------", "---", "-----")
	limit := 25
	if len(steps) < limit {
		limit = len(steps)
	}
	for i, st := range steps[:limit] {
		fmt.Printf("  %-4d  %-35s  %8.3f  %6d  %6d\n",
			i+1, st.term, st.score, st.newPg, st.total)
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

		fmt.Println("\n  Foreground-weighted vs human (first 25):")
		hits := 0
		for i, st := range steps {
			if i >= 25 {
				break
			}
			match := ""
			if humanTerms[strings.ToLower(st.term)] {
				match = " ← HUMAN MATCH"
				hits++
			}
			fmt.Printf("    #%-3d  %-35s%s\n", i+1, st.term, match)
		}
		top := 25
		if len(steps) < top {
			top = len(steps)
		}
		fmt.Printf("\n  Human matches in top %d: %d (%.1f%%)\n", top, hits, float64(hits)/float64(top)*100)
	}
}
