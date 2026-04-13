// Command observe reads the reference index and the actual page text
// side by side. For each human entry, it shows: the entry term, the
// pages it points to, and what those pages actually say.
//
// No scoring, no signals, no theory. Just input and output side by side.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dukkandcards/quadstore"
)

type page struct {
	PhysicalPage int    `json:"physical_page"`
	Chapter      string `json:"chapter"`
	RawText      string `json:"raw_text"`
}

func main() {
	dbPath := flag.String("db", "", "quadstore path")
	wsPath := flag.String("ws", "", "workspace path")
	termFilter := flag.String("term", "", "show only this term (substring match)")
	limit := flag.Int("n", 10, "number of entries to show")
	stats := flag.Bool("stats", false, "show input-output statistics only")
	cluster := flag.Bool("cluster", false, "show page clusters")
	flag.Parse()

	if *dbPath == "" || *wsPath == "" {
		flag.Usage()
		os.Exit(1)
	}

	if *cluster {
		clusterPages(*wsPath)
		return
	}

	if *stats {
		store, err := quadstore.Open(*dbPath)
		if err != nil {
			log.Fatal(err)
		}
		defer store.Close()
		observeStats(store, *wsPath)
		fmt.Println()
		verifyMatching(store, *wsPath)
		return
	}

	// Load pages.
	pages := map[int]*page{}
	files, _ := filepath.Glob(filepath.Join(*wsPath, "pages", "*.json"))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var p page
		if err := json.Unmarshal(data, &p); err != nil {
			continue
		}
		pages[p.PhysicalPage] = &p
	}
	fmt.Printf("Loaded %d pages\n\n", len(pages))

	// Open store.
	store, err := quadstore.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	// Collect reference entries.
	type entry struct {
		id       string
		term     string
		entryType string // main-entry or sub-entry
		pages    []int
		parentID string
		seeTarget string
	}

	entries := map[string]*entry{}
	it := store.Match("", "term", "", "reference")
	for it.Next() {
		id := it.Quad().Subject
		if _, ok := entries[id]; !ok {
			entries[id] = &entry{id: id, term: it.Quad().Object}
		}
	}
	it.Close()

	for id, e := range entries {
		// Pages.
		pit := store.Match(id, "has-page", "", "reference")
		for pit.Next() {
			var n int
			fmt.Sscanf(pit.Quad().Object, "page:%d", &n)
			if n > 0 {
				e.pages = append(e.pages, n)
			}
		}
		pit.Close()
		sort.Ints(e.pages)

		// Type.
		tit := store.Match(id, "type", "", "reference")
		if tit.Next() {
			e.entryType = tit.Quad().Object
		}
		tit.Close()

		// See target.
		sit := store.Match(id, "see", "", "reference")
		if sit.Next() {
			targetID := sit.Quad().Object
			if t, ok := entries[targetID]; ok {
				e.seeTarget = t.term
			} else {
				e.seeTarget = targetID
			}
		}
		sit.Close()
	}

	// Find parent relationships.
	it = store.Match("", "has-sub-entry", "", "reference")
	for it.Next() {
		childID := it.Quad().Object
		parentID := it.Quad().Subject
		if child, ok := entries[childID]; ok {
			child.parentID = parentID
		}
	}
	it.Close()

	// Sort by page count descending, then term.
	sorted := make([]*entry, 0, len(entries))
	for _, e := range entries {
		if *termFilter != "" {
			if !strings.Contains(strings.ToLower(e.term), strings.ToLower(*termFilter)) {
				continue
			}
		}
		sorted = append(sorted, e)
	}
	sort.Slice(sorted, func(i, j int) bool {
		if len(sorted[i].pages) != len(sorted[j].pages) {
			return len(sorted[i].pages) > len(sorted[j].pages)
		}
		return sorted[i].term < sorted[j].term
	})

	if *limit > 0 && len(sorted) > *limit {
		sorted = sorted[:*limit]
	}

	// Show each entry alongside the page text.
	for _, e := range sorted {
		fmt.Println(strings.Repeat("=", 72))
		typeLabel := e.entryType
		if e.seeTarget != "" {
			typeLabel = "See → " + e.seeTarget
		}
		parentLabel := ""
		if e.parentID != "" {
			if p, ok := entries[e.parentID]; ok {
				parentLabel = fmt.Sprintf(" (under: %s)", p.term)
			}
		}
		fmt.Printf("ENTRY: %s  [%s]%s\n", e.term, typeLabel, parentLabel)
		fmt.Printf("PAGES: %v\n", e.pages)

		if len(e.pages) == 0 {
			fmt.Println("  (no pages — routing entry or parent heading)")
			fmt.Println()
			continue
		}

		for _, pn := range e.pages {
			p, ok := pages[pn]
			if !ok {
				fmt.Printf("\n  PAGE %d: (not found in workspace)\n", pn)
				continue
			}

			fmt.Printf("\n  PAGE %d  [%s]\n", pn, p.Chapter)

			// Show the raw text, trimmed and truncated.
			text := strings.TrimSpace(p.RawText)
			// Remove page number artifacts.
			lines := strings.Split(text, "\n")
			var cleanLines []string
			for _, l := range lines {
				l = strings.TrimSpace(l)
				if l == "" || l == fmt.Sprintf("%d", pn) || l == fmt.Sprintf("-%d-", pn) {
					continue
				}
				cleanLines = append(cleanLines, l)
			}
			text = strings.Join(cleanLines, "\n")

			// Truncate to ~400 chars for readability.
			if len(text) > 400 {
				text = text[:400] + "..."
			}

			// Highlight the entry term in the text.
			termLow := strings.ToLower(e.term)
			textLow := strings.ToLower(text)
			if strings.Contains(textLow, termLow) {
				fmt.Printf("  ✓ Term appears in text\n")
			} else {
				// Check individual words.
				termWords := strings.Fields(termLow)
				found := 0
				for _, tw := range termWords {
					if len(tw) >= 4 && strings.Contains(textLow, tw) {
						found++
					}
				}
				if found > 0 {
					fmt.Printf("  ~ %d of %d term words appear in text\n", found, len(termWords))
				} else {
					fmt.Printf("  ✗ Term does NOT appear in text\n")
				}
			}

			fmt.Printf("  ---\n%s\n", indent(text, "  | "))
		}
		fmt.Println()
	}
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
