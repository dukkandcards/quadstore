package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dukkandcards/quadstore"
)

type pageContent struct {
	PhysicalPage int    `json:"physical_page"`
	Section      string `json:"section"`
	Chapter      string `json:"chapter"`
	RawText      string `json:"raw_text"`
}

// ingestPageContent loads page text from a mega-index workspace into the
// quad store. This gives us the raw material to compute aboutness — whether
// a term is substantively discussed on a page vs merely mentioned.
func ingestPageContent(s *quadstore.Store, wsPath, label string) (map[int]*pageContent, error) {
	pagesDir := filepath.Join(wsPath, "pages")
	files, err := filepath.Glob(filepath.Join(pagesDir, "*.json"))
	if err != nil {
		return nil, err
	}

	pages := map[int]*pageContent{}
	var quads []quadstore.Quad
	pgLabel := label + ":pages"

	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var pc pageContent
		if err := json.Unmarshal(data, &pc); err != nil {
			continue
		}
		if pc.PhysicalPage == 0 || pc.RawText == "" {
			continue
		}
		pages[pc.PhysicalPage] = &pc

		pageID := fmt.Sprintf("page:%d", pc.PhysicalPage)
		if pc.Chapter != "" {
			quads = append(quads, quadstore.Quad{
				Subject: pageID, Predicate: "chapter", Object: pc.Chapter, Label: pgLabel,
			})
		}
		if pc.Section != "" {
			quads = append(quads, quadstore.Quad{
				Subject: pageID, Predicate: "section", Object: pc.Section, Label: pgLabel,
			})
		}
		// Store word count as a signal.
		words := len(strings.Fields(pc.RawText))
		quads = append(quads, quadstore.Quad{
			Subject: pageID, Predicate: "word-count",
			Object: fmt.Sprintf("%d", words), Label: pgLabel,
		})
	}

	if err := s.AddBatch(quads); err != nil {
		return nil, err
	}
	fmt.Printf("Ingested %d pages (%d quads) from %s\n", len(pages), len(quads), wsPath)
	return pages, nil
}

// computeAboutness measures how substantively each term is present on each
// page it references. The difference between ofness and aboutness.
//
// "Woodpecker" on page 50 → ofness (the word appears because it's a book
// about woodpeckers). "Tongue adaptations" on page 50 → aboutness (the page
// is specifically about tongue adaptations, per the chapter title and content).
//
// Signals per term-page edge:
//   1. Term density — occurrences of term / total words on page
//   2. Chapter match — does the page's chapter title contain the term?
//   3. Position — does the term appear in the first or last paragraph?
//   4. Dominance — is this the most frequent multi-word term on the page?
//   5. Sentence ratio — what fraction of sentences mention the term?
func computeAboutness(s *quadstore.Store, label string, pages map[int]*pageContent) {
	fmt.Printf("\n=== ABOUTNESS ANALYSIS (label: %s) ===\n\n", label)

	abLabel := label + ":aboutness"

	// Gather entries and their page references.
	type abEntry struct {
		id    string
		term  string
		pages []int
	}

	entries := map[string]*abEntry{}
	it := s.Match("", "term", "", label)
	for it.Next() {
		id := it.Quad().Subject
		if _, ok := entries[id]; !ok {
			entries[id] = &abEntry{id: id, term: it.Quad().Object}
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
	}

	var quads []quadstore.Quad

	type aboutnessScore struct {
		entryID      string
		term         string
		pageNum      int
		density      float64 // term occurrences / page words
		chapterMatch float64 // 1.0 if chapter title contains term
		positionHit  float64 // 1.0 if in first or last paragraph
		sentenceRatio float64 // fraction of sentences mentioning term
		combined     float64
	}

	var allScores []aboutnessScore

	// Aggregate per entry.
	type entryAboutness struct {
		term       string
		avgScore   float64
		maxScore   float64
		totalPages int
		strongPages int // pages where aboutness > threshold
	}
	entryAgg := map[string]*entryAboutness{}

	for id, e := range entries {
		if len(e.pages) == 0 {
			continue
		}

		ea := &entryAboutness{term: e.term, totalPages: len(e.pages)}
		termLow := strings.ToLower(e.term)
		termWords := strings.Fields(termLow)

		var scoreSum float64

		for _, pn := range e.pages {
			pc, ok := pages[pn]
			if !ok {
				continue
			}

			textLow := strings.ToLower(pc.RawText)
			pageWords := strings.Fields(textLow)
			if len(pageWords) == 0 {
				continue
			}

			// 1. Term density: count occurrences of the full term.
			occurrences := 0
			if len(termWords) == 1 {
				for _, w := range pageWords {
					// Strip punctuation for matching.
					cleaned := strings.Trim(w, ".,;:!?\"'()[]")
					if cleaned == termLow || cleaned+"s" == termLow || termLow+"s" == cleaned {
						occurrences++
					}
				}
			} else {
				occurrences = strings.Count(textLow, termLow)
			}
			density := float64(occurrences) / float64(len(pageWords))

			// 2. Chapter match: does the chapter title contain the term?
			chapterMatch := 0.0
			chapterLow := strings.ToLower(pc.Chapter)
			if termLow != "" && strings.Contains(chapterLow, termLow) {
				chapterMatch = 1.0
			} else {
				// Check if any word of the term appears in the chapter.
				chapterHits := 0
				for _, tw := range termWords {
					if len(tw) >= 4 && strings.Contains(chapterLow, tw) {
						chapterHits++
					}
				}
				if len(termWords) > 0 {
					chapterMatch = float64(chapterHits) / float64(len(termWords))
				}
			}

			// 3. Position: first or last paragraph.
			paragraphs := strings.Split(pc.RawText, "\n\n")
			// Clean empty paragraphs.
			var cleanParas []string
			for _, p := range paragraphs {
				p = strings.TrimSpace(p)
				if len(p) > 20 { // skip very short fragments
					cleanParas = append(cleanParas, p)
				}
			}
			positionHit := 0.0
			if len(cleanParas) > 0 {
				firstLow := strings.ToLower(cleanParas[0])
				lastLow := strings.ToLower(cleanParas[len(cleanParas)-1])
				if strings.Contains(firstLow, termLow) {
					positionHit = 1.0
				} else if strings.Contains(lastLow, termLow) {
					positionHit = 0.7
				}
			}

			// 4. Sentence ratio: what fraction of sentences mention the term?
			sentences := splitSentences(textLow)
			mentioning := 0
			for _, sent := range sentences {
				if strings.Contains(sent, termLow) {
					mentioning++
				} else if len(termWords) >= 2 {
					// Check if most words of the term appear in the sentence.
					hits := 0
					for _, tw := range termWords {
						if len(tw) >= 4 && strings.Contains(sent, tw) {
							hits++
						}
					}
					if float64(hits)/float64(len(termWords)) >= 0.5 {
						mentioning++
					}
				}
			}
			sentenceRatio := 0.0
			if len(sentences) > 0 {
				sentenceRatio = float64(mentioning) / float64(len(sentences))
			}

			// Combined aboutness.
			combined := density*100*0.25 + chapterMatch*0.30 +
				positionHit*0.20 + sentenceRatio*0.25

			as := aboutnessScore{
				entryID: id, term: e.term, pageNum: pn,
				density: density, chapterMatch: chapterMatch,
				positionHit: positionHit, sentenceRatio: sentenceRatio,
				combined: combined,
			}
			allScores = append(allScores, as)

			scoreSum += combined
			if combined > ea.maxScore {
				ea.maxScore = combined
			}
			if combined >= 0.30 {
				ea.strongPages++
			}

			// Store per-edge aboutness.
			pageID := fmt.Sprintf("page:%d", pn)
			quads = append(quads, quadstore.Quad{
				Subject: id, Predicate: "aboutness:" + pageID,
				Object: fmt.Sprintf("%.4f", combined), Label: abLabel,
			})
		}

		if ea.totalPages > 0 {
			ea.avgScore = scoreSum / float64(ea.totalPages)
		}
		entryAgg[id] = ea

		quads = append(quads, quadstore.Quad{
			Subject: id, Predicate: "aboutness-avg",
			Object: fmt.Sprintf("%.4f", ea.avgScore), Label: abLabel,
		})
		quads = append(quads, quadstore.Quad{
			Subject: id, Predicate: "aboutness-max",
			Object: fmt.Sprintf("%.4f", ea.maxScore), Label: abLabel,
		})
		quads = append(quads, quadstore.Quad{
			Subject: id, Predicate: "strong-pages",
			Object: fmt.Sprintf("%d", ea.strongPages), Label: abLabel,
		})
	}

	if err := s.AddBatch(quads); err != nil {
		fmt.Printf("Error storing aboutness quads: %v\n", err)
		return
	}
	fmt.Printf("Stored %d aboutness quads\n\n", len(quads))

	// --- Observations ---

	// Top entries by average aboutness.
	fmt.Println("--- Highest average aboutness (term is ABOUT these pages) ---\n")

	type aggDisplay struct {
		term        string
		avg, max    float64
		pages       int
		strongPages int
	}
	var aggs []aggDisplay
	for _, ea := range entryAgg {
		aggs = append(aggs, aggDisplay{
			ea.term, ea.avgScore, ea.maxScore, ea.totalPages, ea.strongPages,
		})
	}

	sort.Slice(aggs, func(i, j int) bool { return aggs[i].avg > aggs[j].avg })
	limit := 25
	if len(aggs) < limit {
		limit = len(aggs)
	}
	fmt.Printf("  %-35s  %7s  %7s  %5s  %6s\n", "Term", "AvgAbt", "MaxAbt", "Pages", "Strong")
	fmt.Printf("  %-35s  %7s  %7s  %5s  %6s\n", "----", "------", "------", "-----", "------")
	for _, a := range aggs[:limit] {
		fmt.Printf("  %-35s  %7.3f  %7.3f  %5d  %6d\n",
			a.term, a.avg, a.max, a.pages, a.strongPages)
	}

	// Lowest aboutness (mentioned but not about).
	fmt.Println("\n--- Lowest average aboutness (mentioned but pages aren't ABOUT this) ---\n")
	sort.Slice(aggs, func(i, j int) bool { return aggs[i].avg < aggs[j].avg })
	limit = 20
	if len(aggs) < limit {
		limit = len(aggs)
	}
	for _, a := range aggs[:limit] {
		fmt.Printf("  %-35s  %7.3f  %7.3f  %5d  %6d\n",
			a.term, a.avg, a.max, a.pages, a.strongPages)
	}

	// Combined: abstraction level + aboutness.
	if label == "generated" {
		fmt.Println("\n--- Combined: Abstraction level × Aboutness (the full signal) ---")
		fmt.Println("Molecule-level terms that pages are actually ABOUT.\n")

		humanTerms := map[string]bool{}
		hit := s.Match("", "term", "", "reference")
		for hit.Next() {
			humanTerms[strings.ToLower(hit.Quad().Object)] = true
		}
		hit.Close()

		// Read abstraction level from prior analysis.
		// We don't have it in quads, so recompute quickly.
		type combined struct {
			term       string
			aboutness  float64
			pages      int
			wordCount  int
			combined   float64
		}
		var combos []combined

		for id, ea := range entryAgg {
			e := entries[id]
			if e == nil || ea.totalPages == 0 {
				continue
			}

			wc := len(strings.Fields(e.term))
			wordScore := 0.0
			switch wc {
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
			case ea.totalPages == 1:
				pageScore = 0.2
			case ea.totalPages <= 3:
				pageScore = 0.6
			case ea.totalPages <= 7:
				pageScore = 1.0
			case ea.totalPages <= 12:
				pageScore = 0.7
			default:
				pageScore = 0.3
			}

			// Aboutness-weighted abstraction.
			absLevel := wordScore*0.25 + pageScore*0.25 + ea.avgScore*0.50
			_ = math.Abs

			combos = append(combos, combined{
				ea.term, ea.avgScore, ea.totalPages, wc, absLevel,
			})
		}

		sort.Slice(combos, func(i, j int) bool { return combos[i].combined > combos[j].combined })
		limit = 30
		if len(combos) < limit {
			limit = len(combos)
		}

		fmt.Printf("  %-35s  %7s  %5s  %5s  %7s\n",
			"Term", "About", "Pages", "Words", "Score")
		fmt.Printf("  %-35s  %7s  %5s  %5s  %7s\n",
			"----", "-----", "-----", "-----", "-----")
		hits := 0
		for i, c := range combos[:limit] {
			match := ""
			if humanTerms[strings.ToLower(c.term)] {
				match = " ← HUMAN"
				hits++
			}
			fmt.Printf("  #%-3d %-35s  %7.3f  %5d  %5d  %7.3f%s\n",
				i+1, c.term, c.aboutness, c.pages, c.wordCount, c.combined, match)
		}
		fmt.Printf("\n  Human matches in top %d: %d (%.1f%%)\n", limit, hits, float64(hits)/float64(limit)*100)
	}
}

func splitSentences(text string) []string {
	// Simple sentence splitter.
	var sentences []string
	for _, delim := range []string{". ", "! ", "? ", ".\n", "!\n", "?\n"} {
		text = strings.ReplaceAll(text, delim, "|||")
	}
	for _, s := range strings.Split(text, "|||") {
		s = strings.TrimSpace(s)
		if len(s) > 10 {
			sentences = append(sentences, s)
		}
	}
	return sentences
}
