package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dukkandcards/quadstore"
)

// verifyMatching digs into the "no match" pairs to understand WHY
// they don't match. Is it:
//   a) The term genuinely doesn't appear (conceptual abstraction)
//   b) A variant form appears (plural, possessive, synonym)
//   c) Individual words appear but not together
//   d) A text extraction artifact (bad OCR, encoding, truncation)
func verifyMatching(store *quadstore.Store, wsPath string) {
	fmt.Println("=== VERIFYING THE 46.6% NO-MATCH CLAIM ===")
	fmt.Print("Digging into every 'no match' pair to see WHY.\n\n")

	// Load pages.
	pages := map[int]string{}
	files, _ := filepath.Glob(filepath.Join(wsPath, "pages", "*.json"))
	for _, f := range files {
		data, _ := os.ReadFile(f)
		var p struct {
			PhysicalPage int    `json:"physical_page"`
			RawText      string `json:"raw_text"`
		}
		json.Unmarshal(data, &p)
		if p.PhysicalPage > 0 {
			pages[p.PhysicalPage] = strings.ToLower(p.RawText)
		}
	}

	// Collect reference entries with pages.
	type refEntry struct {
		term  string
		pages []int
	}
	var entries []refEntry
	termsByID := map[string]string{}

	it := store.Match("", "term", "", "reference")
	for it.Next() {
		termsByID[it.Quad().Subject] = it.Quad().Object
	}
	it.Close()

	for id, term := range termsByID {
		var pageNums []int
		pit := store.Match(id, "has-page", "", "reference")
		for pit.Next() {
			var n int
			fmt.Sscanf(pit.Quad().Object, "page:%d", &n)
			if n > 0 {
				pageNums = append(pageNums, n)
			}
		}
		pit.Close()
		if len(pageNums) > 0 {
			entries = append(entries, refEntry{term, pageNums})
		}
	}

	// Classification of "no match" reasons.
	type reason int
	const (
		exactMatch        reason = iota // full term in text (not a miss)
		pluralMatch                     // "grub" vs "grubs" or "grub's"
		possessiveMatch                 // "woodpecker" vs "woodpecker's"
		stemMatch                       // "boring" vs "borer", "adapt" root
		allWordsPresent                 // every word of term on page, just not together
		mostWordsPresent                // >50% of words present
		oneWordPresent                  // exactly one significant word present
		genuineAbsence                  // nothing even close
	)

	reasonName := map[reason]string{
		exactMatch:       "exact match (not a miss)",
		pluralMatch:      "plural/singular variant",
		possessiveMatch:  "possessive variant",
		stemMatch:        "stem/root variant",
		allWordsPresent:  "all words present, not adjacent",
		mostWordsPresent: "most words present (>50%)",
		oneWordPresent:   "one word present",
		genuineAbsence:   "genuine absence",
	}

	counts := map[reason]int{}
	var genuineAbsences []string // term → page for inspection

	totalPairs := 0

	for _, e := range entries {
		termLow := strings.ToLower(e.term)
		termWords := strings.Fields(termLow)

		for _, pn := range e.pages {
			text, ok := pages[pn]
			if !ok {
				continue
			}
			totalPairs++

			// Level 1: Exact match.
			if strings.Contains(text, termLow) {
				counts[exactMatch]++
				continue
			}

			// Level 2: Plural/singular variants.
			found := false
			variants := pluralVariants(termLow)
			for _, v := range variants {
				if strings.Contains(text, v) {
					counts[pluralMatch]++
					found = true
					break
				}
			}
			if found {
				continue
			}

			// Level 3: Possessive ("woodpecker" → "woodpecker's").
			possessive := termLow + "'s"
			possessive2 := termLow + "\u2019s" // curly quote
			if strings.Contains(text, possessive) || strings.Contains(text, possessive2) {
				counts[possessiveMatch]++
				continue
			}
			// Also check each word with possessive.
			foundPoss := false
			for _, w := range termWords {
				if len(w) < 4 {
					continue
				}
				if strings.Contains(text, w+"'s") || strings.Contains(text, w+"\u2019s") {
					// Only count if most other words also present.
					otherHits := 0
					for _, ow := range termWords {
						if ow == w {
							continue
						}
						if len(ow) >= 4 && strings.Contains(text, ow) {
							otherHits++
						}
					}
					significantWords := 0
					for _, tw := range termWords {
						if len(tw) >= 4 {
							significantWords++
						}
					}
					if significantWords <= 1 || otherHits > 0 {
						counts[possessiveMatch]++
						foundPoss = true
						break
					}
				}
			}
			if foundPoss {
				continue
			}

			// Level 4: Stem/root variants.
			foundStem := false
			for _, w := range termWords {
				if len(w) < 5 {
					continue
				}
				// Try common stem reductions.
				stems := stemVariants(w)
				for _, stem := range stems {
					if strings.Contains(text, stem) {
						foundStem = true
						break
					}
				}
				if foundStem {
					break
				}
			}
			if foundStem {
				counts[stemMatch]++
				continue
			}

			// Level 5: All words present but not adjacent.
			significantWords := 0
			presentWords := 0
			for _, w := range termWords {
				if len(w) < 3 {
					continue
				}
				significantWords++
				// Check word and its plural/possessive.
				if strings.Contains(text, w) {
					presentWords++
				} else {
					for _, v := range pluralVariants(w) {
						if strings.Contains(text, v) {
							presentWords++
							break
						}
					}
				}
			}

			switch {
			case significantWords > 0 && presentWords == significantWords:
				counts[allWordsPresent]++
			case significantWords > 1 && float64(presentWords)/float64(significantWords) > 0.5:
				counts[mostWordsPresent]++
			case presentWords >= 1:
				counts[oneWordPresent]++
			default:
				counts[genuineAbsence]++
				genuineAbsences = append(genuineAbsences,
					fmt.Sprintf("%-30s → page %d", e.term, pn))
			}
		}
	}

	// Report.
	fmt.Printf("Total entry-page pairs examined: %d\n\n", totalPairs)

	fmt.Println("--- Match classification ---")
	fmt.Printf("  %-40s  %5s  %6s\n", "Reason", "Count", "Rate")
	fmt.Printf("  %-40s  %5s  %6s\n", "------", "-----", "----")

	// Order by reason.
	for r := exactMatch; r <= genuineAbsence; r++ {
		c := counts[r]
		fmt.Printf("  %-40s  %5d  %5.1f%%\n", reasonName[r], c, pct(c, totalPairs))
	}

	nonExact := totalPairs - counts[exactMatch]
	trueAbsence := counts[genuineAbsence]
	fmt.Printf("\n  Original 'no match' claim:    %.1f%% of pairs\n",
		pct(nonExact, totalPairs))
	fmt.Printf("  After accounting for variants: %.1f%% genuine absence\n",
		pct(trueAbsence, totalPairs))
	fmt.Printf("  Difference: %.1f%% were variant matches misclassified as absent\n",
		pct(nonExact-trueAbsence, totalPairs)-pct(trueAbsence, totalPairs))

	// Show genuine absences.
	if len(genuineAbsences) > 0 {
		fmt.Printf("\n--- Genuine absences (%d pairs) ---\n", len(genuineAbsences))
		fmt.Print("No form of the term appears on the page.\n\n")
		sort.Strings(genuineAbsences)
		limit := 40
		if len(genuineAbsences) < limit {
			limit = len(genuineAbsences)
		}
		for _, ga := range genuineAbsences[:limit] {
			fmt.Printf("  %s\n", ga)
		}
		if len(genuineAbsences) > limit {
			fmt.Printf("  ... and %d more\n", len(genuineAbsences)-limit)
		}
	}
}

func pluralVariants(word string) []string {
	var variants []string
	// Add s.
	variants = append(variants, word+"s")
	// Remove s.
	if strings.HasSuffix(word, "s") && len(word) > 3 {
		variants = append(variants, word[:len(word)-1])
	}
	// -es / remove -es.
	variants = append(variants, word+"es")
	if strings.HasSuffix(word, "es") && len(word) > 4 {
		variants = append(variants, word[:len(word)-2])
	}
	// -ies / -y.
	if strings.HasSuffix(word, "y") {
		variants = append(variants, word[:len(word)-1]+"ies")
	}
	if strings.HasSuffix(word, "ies") {
		variants = append(variants, word[:len(word)-3]+"y")
	}
	// æ / ae.
	if strings.Contains(word, "æ") {
		variants = append(variants, strings.ReplaceAll(word, "æ", "ae"))
	}
	if strings.Contains(word, "ae") {
		variants = append(variants, strings.ReplaceAll(word, "ae", "æ"))
	}
	return variants
}

func stemVariants(word string) []string {
	var stems []string
	// Common suffix removals.
	suffixes := []string{"ing", "tion", "ation", "ment", "ness", "ous", "ive", "ed", "er", "est", "ly", "al"}
	for _, suf := range suffixes {
		if strings.HasSuffix(word, suf) && len(word)-len(suf) >= 3 {
			root := word[:len(word)-len(suf)]
			stems = append(stems, root)
			// Also try adding common endings to root.
			stems = append(stems, root+"e")
			stems = append(stems, root+"ing")
			stems = append(stems, root+"ed")
			stems = append(stems, root+"s")
		}
	}
	return stems
}
