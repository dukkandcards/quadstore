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

// observeStats computes simple input-output statistics.
// For each reference entry × page pair: does the term appear in the text?
// No theory. Just counting.
func observeStats(store *quadstore.Store, wsPath string) {
	fmt.Println("=== INPUT-OUTPUT OBSERVATION ===")
	fmt.Println("For each entry-page pair: does the entry term appear in the page text?")
	fmt.Println()

	// Load pages.
	pages := map[int]string{} // page num → raw text (lowercased)
	chapters := map[int]string{}
	files, _ := filepath.Glob(filepath.Join(wsPath, "pages", "*.json"))
	for _, f := range files {
		data, _ := os.ReadFile(f)
		var p struct {
			PhysicalPage int    `json:"physical_page"`
			Chapter      string `json:"chapter"`
			RawText      string `json:"raw_text"`
		}
		json.Unmarshal(data, &p)
		if p.PhysicalPage > 0 {
			pages[p.PhysicalPage] = strings.ToLower(p.RawText)
			chapters[p.PhysicalPage] = strings.ToLower(p.Chapter)
		}
	}

	// Collect reference entries.
	type refEntry struct {
		id    string
		term  string
		pages []int
		typ   string
	}
	entries := map[string]*refEntry{}
	it := store.Match("", "term", "", "reference")
	for it.Next() {
		id := it.Quad().Subject
		if _, ok := entries[id]; !ok {
			entries[id] = &refEntry{id: id, term: it.Quad().Object}
		}
	}
	it.Close()

	for id, e := range entries {
		pit := store.Match(id, "has-page", "", "reference")
		for pit.Next() {
			var n int
			fmt.Sscanf(pit.Quad().Object, "page:%d", &n)
			if n > 0 {
				e.pages = append(e.pages, n)
			}
		}
		pit.Close()

		tit := store.Match(id, "type", "", "reference")
		if tit.Next() {
			e.typ = tit.Quad().Object
		}
		tit.Close()
	}

	// For each entry-page pair, check presence.
	type presence int
	const (
		exactMatch   presence = iota // full term found in text
		partialMatch                 // >50% of term words found
		chapterMatch                 // term found in chapter title
		noMatch                      // nothing found
	)

	type pairResult struct {
		term    string
		pageNum int
		result  presence
	}

	var pairs []pairResult
	termExact := 0
	termPartial := 0
	termChapter := 0
	termNone := 0

	// Also track per-entry: how is the term related to its pages?
	type entryPattern struct {
		term         string
		typ          string
		totalPages   int
		exactPages   int
		partialPages int
		chapterPages int
		nonePages    int
	}
	var patterns []entryPattern

	for _, e := range entries {
		if len(e.pages) == 0 {
			continue
		}

		termLow := strings.ToLower(e.term)
		termWords := strings.Fields(termLow)

		ep := entryPattern{term: e.term, typ: e.typ, totalPages: len(e.pages)}

		for _, pn := range e.pages {
			text, ok := pages[pn]
			if !ok {
				continue
			}
			chap := chapters[pn]

			var res presence

			// Check exact term.
			if strings.Contains(text, termLow) {
				res = exactMatch
				ep.exactPages++
				termExact++
			} else if strings.Contains(chap, termLow) {
				res = chapterMatch
				ep.chapterPages++
				termChapter++
			} else {
				// Check partial word match.
				hits := 0
				for _, w := range termWords {
					if len(w) >= 4 && strings.Contains(text, w) {
						hits++
					}
				}
				if len(termWords) > 0 && float64(hits)/float64(len(termWords)) >= 0.5 {
					res = partialMatch
					ep.partialPages++
					termPartial++
				} else {
					res = noMatch
					ep.nonePages++
					termNone++
				}
			}

			pairs = append(pairs, pairResult{e.term, pn, res})
		}
		patterns = append(patterns, ep)
	}

	totalPairs := len(pairs)
	fmt.Printf("Total entry-page pairs: %d\n", totalPairs)
	fmt.Printf("  Exact match (full term in text):  %4d  (%5.1f%%)\n", termExact, pct(termExact, totalPairs))
	fmt.Printf("  Partial match (>50%% of words):    %4d  (%5.1f%%)\n", termPartial, pct(termPartial, totalPairs))
	fmt.Printf("  Chapter title match:              %4d  (%5.1f%%)\n", termChapter, pct(termChapter, totalPairs))
	fmt.Printf("  No match at all:                  %4d  (%5.1f%%)\n", termNone, pct(termNone, totalPairs))

	// Per-entry patterns.
	fmt.Println("\n--- Entry-level patterns ---")

	// Entries where the term NEVER appears on any of its pages.
	var neverAppear []entryPattern
	var alwaysAppear []entryPattern
	var mixedAppear []entryPattern

	for _, ep := range patterns {
		switch {
		case ep.exactPages == 0 && ep.partialPages == 0 && ep.chapterPages == 0:
			neverAppear = append(neverAppear, ep)
		case ep.exactPages == ep.totalPages:
			alwaysAppear = append(alwaysAppear, ep)
		default:
			mixedAppear = append(mixedAppear, ep)
		}
	}

	fmt.Printf("\nEntries where term ALWAYS appears on its pages:  %d\n", len(alwaysAppear))
	fmt.Printf("Entries where term SOMETIMES appears:            %d\n", len(mixedAppear))
	fmt.Printf("Entries where term NEVER appears on any page:    %d\n", len(neverAppear))

	// Show the "never" entries — these are the conceptual abstractions.
	fmt.Println("\n--- Term NEVER appears on any of its pages ---")
	fmt.Print("These are concepts the indexer named, not words the author wrote.\n\n")
	sort.Slice(neverAppear, func(i, j int) bool { return neverAppear[i].totalPages > neverAppear[j].totalPages })
	for _, ep := range neverAppear {
		fmt.Printf("  %-35s  %d pages  [%s]\n", ep.term, ep.totalPages, ep.typ)
	}

	// Show the "always" entries — direct text extraction.
	fmt.Println("\n--- Term ALWAYS appears on every page it references ---")
	fmt.Print("These are words the author wrote, directly extracted.\n\n")
	sort.Slice(alwaysAppear, func(i, j int) bool { return alwaysAppear[i].totalPages > alwaysAppear[j].totalPages })
	limit := 25
	if len(alwaysAppear) < limit {
		limit = len(alwaysAppear)
	}
	for _, ep := range alwaysAppear[:limit] {
		fmt.Printf("  %-35s  %d pages  [%s]\n", ep.term, ep.totalPages, ep.typ)
	}

	// The mixed entries — some pages have the term, some don't.
	// These are the most interesting: the indexer extends the term's
	// coverage beyond where it literally appears.
	fmt.Println("\n--- Term appears on SOME but not all of its pages ---")
	fmt.Print("The indexer extends coverage beyond literal appearance.\n\n")
	sort.Slice(mixedAppear, func(i, j int) bool { return mixedAppear[i].totalPages > mixedAppear[j].totalPages })
	limit = 25
	if len(mixedAppear) < limit {
		limit = len(mixedAppear)
	}
	for _, ep := range mixedAppear[:limit] {
		fmt.Printf("  %-35s  %d pages: %d exact, %d partial, %d chapter, %d none\n",
			ep.term, ep.totalPages, ep.exactPages, ep.partialPages, ep.chapterPages, ep.nonePages)
	}
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}
