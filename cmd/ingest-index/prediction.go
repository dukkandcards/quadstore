package main

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/dukkandcards/quadstore"
)

// computePredictionError implements Rescorla-Wagner-inspired signal analysis.
//
// Core idea from dog training: the dog learns when there's a mismatch between
// prediction and outcome. A fully predicted reward generates no learning signal.
//
// Applied to index entries:
// - If an entry's pages are fully predicted by other entries, it adds no new
//   information (blocking). It might still be valuable as a routing entry
//   (different search term, same destination).
// - If an entry reaches pages nothing else reaches, it has high prediction
//   error — it's the sole path to content (discriminative).
// - An entry's "value" to the index is some combination of:
//   a) Does it reach content others don't? (discriminative)
//   b) Does it offer a different starting point to known content? (routing)
//   c) Does it compress multiple paths into one? (convergence)
//
// We compute these as observations, not judgments.
func computePredictionError(s *quadstore.Store, label string) {
	fmt.Printf("\n=== PREDICTION ERROR ANALYSIS (label: %s) ===\n\n", label)

	peLabel := label + ":prediction"

	// Gather entries and their pages.
	type peEntry struct {
		id    string
		term  string
		pages map[string]bool
	}

	entries := map[string]*peEntry{}
	it := s.Match("", "term", "", label)
	for it.Next() {
		id := it.Quad().Subject
		if _, ok := entries[id]; !ok {
			entries[id] = &peEntry{id: id, term: it.Quad().Object, pages: map[string]bool{}}
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

	// Build page → entry reverse index.
	pageEntries := map[string][]string{}
	for id, e := range entries {
		for p := range e.pages {
			pageEntries[p] = append(pageEntries[p], id)
		}
	}

	var quads []quadstore.Quad
	add := func(id, pred, val string) {
		quads = append(quads, quadstore.Quad{Subject: id, Predicate: pred, Object: val, Label: peLabel})
	}

	// --- Signal 1: Discriminative power ---
	// For each entry, what fraction of its pages are ONLY reachable through
	// this entry? High = this entry is the sole path to content.
	// Like a dog learning that ONLY the clicker (not the voice) predicts
	// treat in a certain context — the clicker has high discriminative power.
	fmt.Println("--- Discriminative power ---")
	fmt.Println("What fraction of this entry's pages are only reachable through it?")
	fmt.Println("High = sole gateway to content. Low = redundant path.\n")

	type discEntry struct {
		term          string
		uniquePages   int
		totalPages    int
		discPower     float64
		uniquePageIDs []string
	}
	var discEntries []discEntry

	for id, e := range entries {
		if len(e.pages) == 0 {
			continue
		}
		uniquePages := 0
		var uniqueIDs []string
		for p := range e.pages {
			if len(pageEntries[p]) == 1 {
				uniquePages++
				uniqueIDs = append(uniqueIDs, p)
			}
		}
		power := float64(uniquePages) / float64(len(e.pages))
		add(id, "discriminative-power", fmt.Sprintf("%.3f", power))
		if uniquePages > 0 {
			add(id, "unique-page-count", fmt.Sprintf("%d", uniquePages))
		}
		discEntries = append(discEntries, discEntry{e.term, uniquePages, len(e.pages), power, uniqueIDs})
	}
	sort.Slice(discEntries, func(i, j int) bool { return discEntries[i].discPower > discEntries[j].discPower })
	shown := 0
	for _, d := range discEntries {
		if d.discPower > 0 && shown < 15 {
			fmt.Printf("  %-35s  %.0f%% unique (%d of %d pages)\n",
				d.term, d.discPower*100, d.uniquePages, d.totalPages)
			shown++
		}
	}

	// --- Signal 2: Prediction redundancy (blocking) ---
	// For each entry, how well do OTHER entries collectively predict its
	// page set? If every page this entry points to is also pointed to by
	// 5+ other entries, this entry is fully "blocked" — it adds no new
	// page prediction. It may still add routing value (different search term).
	fmt.Println("\n--- Prediction redundancy (blocking) ---")
	fmt.Println("How well do other entries already predict this entry's pages?")
	fmt.Println("High = fully blocked (content is reachable without this entry).\n")

	type blockEntry struct {
		term           string
		avgOtherPaths  float64
		minOtherPaths  int
		totalPages     int
		fullyRedundant bool // every page has 3+ other entries pointing to it
	}
	var blockEntries []blockEntry

	for id, e := range entries {
		if len(e.pages) == 0 {
			continue
		}
		totalOther := 0
		minOther := math.MaxInt32
		fullyRedundant := true
		for p := range e.pages {
			otherCount := len(pageEntries[p]) - 1 // exclude self
			totalOther += otherCount
			if otherCount < minOther {
				minOther = otherCount
			}
			if otherCount < 3 {
				fullyRedundant = false
			}
		}
		avg := float64(totalOther) / float64(len(e.pages))
		add(id, "avg-other-paths", fmt.Sprintf("%.1f", avg))
		add(id, "min-other-paths", fmt.Sprintf("%d", minOther))
		if fullyRedundant {
			add(id, "fully-redundant", "true")
		}
		blockEntries = append(blockEntries, blockEntry{e.term, avg, minOther, len(e.pages), fullyRedundant})
	}

	// Show most blocked (highest avg other paths).
	sort.Slice(blockEntries, func(i, j int) bool {
		return blockEntries[i].avgOtherPaths > blockEntries[j].avgOtherPaths
	})
	shown = 0
	for _, b := range blockEntries {
		if shown >= 15 {
			break
		}
		flag := ""
		if b.fullyRedundant {
			flag = " [FULLY BLOCKED]"
		}
		fmt.Printf("  %-35s  avg %.1f other paths to same pages (min: %d)%s\n",
			b.term, b.avgOtherPaths, b.minOtherPaths, flag)
		shown++
	}

	// Show least blocked (lowest avg other paths, with pages).
	fmt.Println("\n  Least redundant (hardest to reach without this entry):")
	sort.Slice(blockEntries, func(i, j int) bool {
		return blockEntries[i].avgOtherPaths < blockEntries[j].avgOtherPaths
	})
	shown = 0
	for _, b := range blockEntries {
		if shown >= 15 {
			break
		}
		fmt.Printf("  %-35s  avg %.1f other paths (min: %d, %d pages)\n",
			b.term, b.avgOtherPaths, b.minOtherPaths, b.totalPages)
		shown++
	}

	// --- Signal 3: Stimulus generalization ---
	// Groups of entries that point to highly overlapping page sets.
	// Like the dog learning that "sit" from different speakers/volumes
	// all mean the same thing — these entries are the "same command"
	// expressed as different search terms.
	fmt.Println("\n--- Stimulus generalization (same destination, different terms) ---")
	fmt.Println("Entry clusters that reach nearly identical page sets.")
	fmt.Println("These are the same 'command' in different words.\n")

	type entryPair struct {
		termA, termB string
		overlap      float64
		sharedPages  int
	}
	var pairs []entryPair

	entryList := make([]*peEntry, 0, len(entries))
	for _, e := range entries {
		if len(e.pages) >= 2 { // need at least 2 pages for meaningful overlap
			entryList = append(entryList, e)
		}
	}

	for i := 0; i < len(entryList); i++ {
		for j := i + 1; j < len(entryList); j++ {
			a := entryList[i]
			b := entryList[j]
			shared := 0
			for p := range a.pages {
				if b.pages[p] {
					shared++
				}
			}
			if shared == 0 {
				continue
			}
			// Jaccard similarity.
			union := len(a.pages) + len(b.pages) - shared
			jaccard := float64(shared) / float64(union)
			if jaccard >= 0.60 {
				pairs = append(pairs, entryPair{a.term, b.term, jaccard, shared})
			}
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].overlap > pairs[j].overlap })

	// Cluster the pairs into groups.
	shown = 0
	seen := map[string]bool{}
	for _, p := range pairs {
		key := p.termA + "|" + p.termB
		if seen[key] {
			continue
		}
		seen[key] = true
		if shown >= 20 {
			break
		}
		fmt.Printf("  %-30s  ≈  %-30s  (%.0f%% overlap, %d shared pages)\n",
			p.termA, p.termB, p.overlap*100, p.sharedPages)
		shown++
	}

	// --- Signal 4: Marker signal candidates ---
	// In clicker training, the clicker bridges the gap between behavior and
	// reward. In an index, some entries are "markers" — they're not where
	// the content is, but they precisely mark which content you need.
	// These are entries with: few own pages, but high connectivity
	// (many sub-entries or many inbound routes). They're navigation aids,
	// not content destinations.
	fmt.Println("\n--- Marker signal candidates ---")
	fmt.Println("Entries that navigate rather than contain.")
	fmt.Println("Few own pages, but high structural connectivity.\n")

	type markerEntry struct {
		term      string
		ownPages  int
		subCount  int
		inbound   int
		hasSee    bool
		totalConn int
	}
	var markers []markerEntry

	for id, e := range entries {
		subCount := 0
		sit := s.Match(id, "has-sub-entry", "", label)
		for sit.Next() {
			subCount++
		}
		sit.Close()

		inbound := 0
		for _, otherID := range pageEntries {
			_ = otherID // use reverse index differently
		}
		iit := s.Match("", "see", id, label)
		for iit.Next() {
			inbound++
		}
		iit.Close()
		iit = s.Match("", "see-also", id, label)
		for iit.Next() {
			inbound++
		}
		iit.Close()

		hasSee := false
		sit = s.Match(id, "see", "", label)
		if sit.Next() {
			hasSee = true
		}
		sit.Close()

		totalConn := subCount + inbound
		if hasSee {
			totalConn++
		}

		if len(e.pages) <= 2 && totalConn >= 2 {
			markers = append(markers, markerEntry{
				e.term, len(e.pages), subCount, inbound, hasSee, totalConn,
			})
		}
	}
	sort.Slice(markers, func(i, j int) bool { return markers[i].totalConn > markers[j].totalConn })
	for _, m := range markers {
		role := ""
		parts := []string{}
		if m.subCount > 0 {
			parts = append(parts, fmt.Sprintf("funnels:%d", m.subCount))
		}
		if m.inbound > 0 {
			parts = append(parts, fmt.Sprintf("inbound:%d", m.inbound))
		}
		if m.hasSee {
			parts = append(parts, "routes-to-another")
		}
		role = strings.Join(parts, ", ")
		fmt.Printf("  %-35s  pages:%d  [%s]\n", m.term, m.ownPages, role)
	}

	// Store quads.
	if err := s.AddBatch(quads); err != nil {
		fmt.Printf("Error storing prediction quads: %v\n", err)
		return
	}
	fmt.Printf("\nStored %d prediction error quads (label: %s)\n", len(quads), peLabel)
}
