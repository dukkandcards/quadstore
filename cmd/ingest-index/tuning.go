package main

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/dukkandcards/quadstore"
)

// tuneFormulas tries multiple combination formulas against the generated
// index and compares human match rates. We have all the signals computed
// and stored as quads — this function reads them and recombines.
func tuneFormulas(s *quadstore.Store) {
	fmt.Println("\n=== FORMULA TUNING ===")
	fmt.Println("Trying multiple combinations of signals against human reference.")

	// Collect human terms.
	humanTerms := map[string]bool{}
	it := s.Match("", "term", "", "reference")
	for it.Next() {
		humanTerms[strings.ToLower(it.Quad().Object)] = true
	}
	it.Close()

	// Gather all generated entry data.
	type tuneEntry struct {
		id   string
		term string

		// Raw data.
		pageCount int
		wordCount int

		// Signals (read from quads).
		foregroundness   float64
		aboutnessAvg     float64
		aboutnessMax     float64
		strongPages      int
		coverageBreadth  float64
		exclusivity      float64
		redundancy       float64
		indexValue       float64
		pageOverlap      float64
		pageSpread       float64
		subsumesCount    int
		moreSpecific     int

		// Composition chain.
		childCount  int // terms this term contains
		parentCount int // terms that contain this term
	}

	entries := map[string]*tuneEntry{}
	it = s.Match("", "term", "", "generated")
	for it.Next() {
		id := it.Quad().Subject
		if _, ok := entries[id]; !ok {
			entries[id] = &tuneEntry{id: id, term: it.Quad().Object}
		}
	}
	it.Close()

	// Page counts.
	for id, e := range entries {
		pit := s.Match(id, "has-page", "", "generated")
		for pit.Next() {
			e.pageCount++
		}
		pit.Close()
		e.wordCount = len(strings.Fields(e.term))
	}

	// Read signals from various labels.
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
		e.foregroundness = readFloat(id, "foregroundness", "generated:background")
		e.aboutnessAvg = readFloat(id, "aboutness-avg", "generated:aboutness")
		e.aboutnessMax = readFloat(id, "aboutness-max", "generated:aboutness")
		e.strongPages = readInt(id, "strong-pages", "generated:aboutness")
		e.coverageBreadth = readFloat(id, "coverage-breadth", "generated:contention")
		e.exclusivity = readFloat(id, "exclusivity", "generated:contention")
		e.redundancy = readFloat(id, "redundancy", "generated:contention")
		e.indexValue = readFloat(id, "index-value", "generated:contention")
		e.pageOverlap = readFloat(id, "page-overlap-ratio", "generated:signals")
		e.pageSpread = readFloat(id, "page-spread", "generated:signals")
		e.subsumesCount = readInt(id, "term-subsumes-count", "generated:signals")
		e.moreSpecific = readInt(id, "more-specific-variants", "generated:signals")

		// Composition chain: count children and parents via term containment.
		eLow := strings.ToLower(e.term)
		eWords := strings.Fields(eLow)
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
		_ = eWords
	}

	// --- Define formulas ---
	type formula struct {
		name string
		fn   func(e *tuneEntry) float64
	}

	wordScore := func(wc int) float64 {
		switch wc {
		case 1:
			return 0.3
		case 2:
			return 1.0
		case 3:
			return 0.8
		default:
			return 0.4
		}
	}

	pageScore := func(pc int) float64 {
		switch {
		case pc == 0:
			return 0
		case pc == 1:
			return 0.2
		case pc <= 3:
			return 0.6
		case pc <= 7:
			return 1.0
		case pc <= 12:
			return 0.7
		default:
			return 0.3
		}
	}

	compScore := func(children, parents int) float64 {
		c := 0.0
		if children > 0 {
			c += 0.5
		}
		if parents > 0 {
			c += 0.3
		}
		if children > 0 && parents > 0 {
			c += 0.2
		}
		return c
	}

	formulas := []formula{
		{
			name: "A: Abstraction only (baseline 40%)",
			fn: func(e *tuneEntry) float64 {
				return wordScore(e.wordCount)*0.30 +
					pageScore(e.pageCount)*0.40 +
					compScore(e.childCount, e.parentCount)*0.30
			},
		},
		{
			name: "B: Abstraction + max aboutness (not avg)",
			fn: func(e *tuneEntry) float64 {
				abs := wordScore(e.wordCount)*0.30 +
					pageScore(e.pageCount)*0.40 +
					compScore(e.childCount, e.parentCount)*0.30
				// Use max aboutness as boost, capped at 1.0.
				maxAbt := math.Min(e.aboutnessMax, 1.0)
				return abs*0.65 + maxAbt*0.35
			},
		},
		{
			name: "C: Abstraction + aboutness as filter (>= 0.20)",
			fn: func(e *tuneEntry) float64 {
				if e.aboutnessMax < 0.20 && e.pageCount > 0 {
					return 0 // filtered out — pages aren't about this term
				}
				return wordScore(e.wordCount)*0.30 +
					pageScore(e.pageCount)*0.40 +
					compScore(e.childCount, e.parentCount)*0.30
			},
		},
		{
			name: "D: Abstraction + foreground + max aboutness",
			fn: func(e *tuneEntry) float64 {
				abs := wordScore(e.wordCount)*0.25 +
					pageScore(e.pageCount)*0.35 +
					compScore(e.childCount, e.parentCount)*0.25
				fg := e.foregroundness
				maxAbt := math.Min(e.aboutnessMax, 1.0)
				return abs*0.50 + fg*0.20 + maxAbt*0.30
			},
		},
		{
			name: "E: Abstraction + strong pages ratio",
			fn: func(e *tuneEntry) float64 {
				abs := wordScore(e.wordCount)*0.30 +
					pageScore(e.pageCount)*0.40 +
					compScore(e.childCount, e.parentCount)*0.30
				strongRatio := 0.0
				if e.pageCount > 0 {
					strongRatio = float64(e.strongPages) / float64(e.pageCount)
				}
				return abs*0.65 + strongRatio*0.35
			},
		},
		{
			name: "F: Abstraction + NOT too many subsumes",
			fn: func(e *tuneEntry) float64 {
				abs := wordScore(e.wordCount)*0.30 +
					pageScore(e.pageCount)*0.40 +
					compScore(e.childCount, e.parentCount)*0.30
				// Penalize terms that subsume too many others (too general).
				penalty := 0.0
				if e.subsumesCount > 10 {
					penalty = 0.5
				} else if e.subsumesCount > 5 {
					penalty = 0.3
				}
				return abs - penalty
			},
		},
		{
			name: "G: Abstraction + low redundancy",
			fn: func(e *tuneEntry) float64 {
				abs := wordScore(e.wordCount)*0.30 +
					pageScore(e.pageCount)*0.40 +
					compScore(e.childCount, e.parentCount)*0.30
				// Low redundancy bonus.
				redPenalty := math.Min(e.redundancy/50.0, 1.0) * 0.3
				return abs - redPenalty
			},
		},
		{
			name: "H: Abstraction + aboutness filter + foreground",
			fn: func(e *tuneEntry) float64 {
				if e.aboutnessMax < 0.15 && e.pageCount > 0 {
					return 0
				}
				abs := wordScore(e.wordCount)*0.25 +
					pageScore(e.pageCount)*0.35 +
					compScore(e.childCount, e.parentCount)*0.25
				fg := e.foregroundness
				return abs*0.70 + fg*0.30
			},
		},
		{
			name: "I: Abstraction + subsumes penalty + aboutness filter",
			fn: func(e *tuneEntry) float64 {
				if e.aboutnessMax < 0.15 && e.pageCount > 0 {
					return 0
				}
				abs := wordScore(e.wordCount)*0.30 +
					pageScore(e.pageCount)*0.40 +
					compScore(e.childCount, e.parentCount)*0.30
				penalty := 0.0
				if e.subsumesCount > 10 {
					penalty = 0.5
				} else if e.subsumesCount > 5 {
					penalty = 0.2
				}
				return abs - penalty
			},
		},
		{
			name: "J: Kitchen sink (all signals, equal weight)",
			fn: func(e *tuneEntry) float64 {
				abs := wordScore(e.wordCount)*0.30 +
					pageScore(e.pageCount)*0.40 +
					compScore(e.childCount, e.parentCount)*0.30
				fg := e.foregroundness
				maxAbt := math.Min(e.aboutnessMax, 1.0)
				strongRatio := 0.0
				if e.pageCount > 0 {
					strongRatio = float64(e.strongPages) / float64(e.pageCount)
				}
				subPenalty := math.Min(float64(e.subsumesCount)/20.0, 1.0)
				redPenalty := math.Min(e.redundancy/50.0, 1.0)

				return abs*0.25 + fg*0.15 + maxAbt*0.15 +
					strongRatio*0.15 + (1.0-subPenalty)*0.15 +
					(1.0-redPenalty)*0.15
			},
		},
		{
			name: "K: Luhn zone (mid-freq) + Rosch (2-word) + burst + about",
			fn: func(e *tuneEntry) float64 {
				// Luhn: mid-frequency is 3-7 pages.
				luhn := pageScore(e.pageCount)

				// Rosch: 2-word terms are basic level.
				rosch := wordScore(e.wordCount)

				// Burst: foregroundness (clustered, not uniform).
				burst := e.foregroundness

				// About: max aboutness on any page.
				about := math.Min(e.aboutnessMax, 1.0)

				// Composition: middle of hierarchy.
				comp := compScore(e.childCount, e.parentCount)

				return luhn*0.25 + rosch*0.20 + burst*0.15 +
					about*0.20 + comp*0.20
			},
		},
	}

	// Run each formula.
	topN := 30
	entrySlice := make([]*tuneEntry, 0, len(entries))
	for _, e := range entries {
		if e.pageCount > 0 {
			entrySlice = append(entrySlice, e)
		}
	}

	fmt.Printf("  %-55s  %6s  %6s\n", "Formula", "Top 30", "Rate")
	fmt.Printf("  %-55s  %6s  %6s\n", "-------", "------", "----")

	var bestName string
	var bestRate float64
	var bestRanking []*tuneEntry

	for _, f := range formulas {
		// Score and sort.
		type scored struct {
			entry *tuneEntry
			score float64
		}
		var scored2 []scored
		for _, e := range entrySlice {
			scored2 = append(scored2, scored{e, f.fn(e)})
		}
		sort.Slice(scored2, func(i, j int) bool { return scored2[i].score > scored2[j].score })

		hits := 0
		n := topN
		if len(scored2) < n {
			n = len(scored2)
		}
		for i := 0; i < n; i++ {
			if humanTerms[strings.ToLower(scored2[i].entry.term)] {
				hits++
			}
		}
		rate := float64(hits) / float64(n) * 100
		fmt.Printf("  %-55s  %6d  %5.1f%%\n", f.name, hits, rate)

		if rate > bestRate {
			bestRate = rate
			bestName = f.name
			bestRanking = make([]*tuneEntry, 0, n)
			for i := 0; i < n; i++ {
				bestRanking = append(bestRanking, scored2[i].entry)
			}
		}
	}

	// Show best formula's ranking.
	fmt.Printf("\n--- Best: %s (%.1f%%) ---\n\n", bestName, bestRate)
	for i, e := range bestRanking {
		match := ""
		if humanTerms[strings.ToLower(e.term)] {
			match = " ← HUMAN"
		}
		fmt.Printf("  #%-3d  %-35s  wc:%d pg:%d ch:%d pa:%d abt:%.2f fg:%.2f sub:%d%s\n",
			i+1, e.term, e.wordCount, e.pageCount, e.childCount, e.parentCount,
			e.aboutnessMax, e.foregroundness, e.subsumesCount, match)
	}
}

func containsWholeWord(haystack, needle string) bool {
	idx := strings.Index(haystack, needle)
	if idx < 0 {
		return false
	}
	before := idx == 0 || haystack[idx-1] == ' '
	after := idx+len(needle) == len(haystack) || haystack[idx+len(needle)] == ' '
	return before && after
}
