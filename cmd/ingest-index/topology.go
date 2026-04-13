package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dukkandcards/quadstore"
)

func compareTopology(s *quadstore.Store) {
	fmt.Println("=== TOPOLOGY DIFF: Human vs Machine ===\n")

	// --- Role distribution comparison ---
	refRoles := classifyRoles(s, "reference")
	genRoles := classifyRoles(s, "generated")

	fmt.Println("--- Role distribution ---")
	fmt.Printf("  %-20s  %6s  %6s\n", "Role", "Human", "Machine")
	fmt.Printf("  %-20s  %6s  %6s\n", "----", "-----", "-------")
	allRoles := []string{"content", "content+hub", "router", "convergence", "orphan"}
	for _, role := range allRoles {
		fmt.Printf("  %-20s  %6d  %6d\n", role, len(refRoles[role]), len(genRoles[role]))
	}

	// --- Routing structure comparison ---
	fmt.Println("\n--- Routing structure ---")
	refRoutes := collectRoutes(s, "reference")
	genRoutes := collectRoutes(s, "generated")
	fmt.Printf("  Human has %d See-routes, Machine has %d\n", len(refRoutes), len(genRoutes))

	if len(refRoutes) > 0 && len(genRoutes) == 0 {
		fmt.Println("  Machine built ZERO routing structure.")
		fmt.Println("  Human's routing graph (entirely missing from machine):")
		for from, to := range refRoutes {
			fmt.Printf("    %s → %s\n", from, to)
		}
	}

	// --- Convergence funnel comparison ---
	fmt.Println("\n--- Convergence funnels ---")
	refFunnels := collectFunnels(s, "reference")
	genFunnels := collectFunnels(s, "generated")
	fmt.Printf("  Human has %d parent-with-subs funnels, Machine has %d\n",
		len(refFunnels), len(genFunnels))

	// Match funnels by term similarity.
	fmt.Println("\n  Funnel comparison (human parent → machine equivalent):")
	for parentTerm, refSubs := range refFunnels {
		genSubs, found := genFunnels[parentTerm]
		if !found {
			// Try fuzzy.
			for gTerm, gSubs := range genFunnels {
				if termsOverlap(parentTerm, gTerm) {
					genSubs = gSubs
					found = true
					parentTerm = parentTerm + " ≈ " + gTerm
					break
				}
			}
		}
		if found {
			// Count sub-entry overlap.
			overlap := 0
			for _, rs := range refSubs {
				for _, gs := range genSubs {
					if termsOverlap(rs, gs) {
						overlap++
						break
					}
				}
			}
			fmt.Printf("\n    %s\n", parentTerm)
			fmt.Printf("      Human: %d subs, Machine: %d subs, Overlap: %d\n",
				len(refSubs), len(genSubs), overlap)
			// Show human-only subs.
			for _, rs := range refSubs {
				matched := false
				for _, gs := range genSubs {
					if termsOverlap(rs, gs) {
						matched = true
						break
					}
				}
				if !matched {
					fmt.Printf("      human only:   %s\n", rs)
				}
			}
			// Show machine-only subs.
			for _, gs := range genSubs {
				matched := false
				for _, rs := range refSubs {
					if termsOverlap(rs, gs) {
						matched = true
						break
					}
				}
				if !matched {
					fmt.Printf("      machine only: %s\n", gs)
				}
			}
		} else {
			fmt.Printf("\n    %s\n", parentTerm)
			fmt.Printf("      Human: %d subs, Machine: NO EQUIVALENT FUNNEL\n", len(refSubs))
			for _, rs := range refSubs {
				fmt.Printf("        %s\n", rs)
			}
		}
	}

	// --- Page hotspot comparison ---
	fmt.Println("\n--- Page hotspot comparison (top 15) ---")
	refHot := collectPageHotspots(s, "reference")
	genHot := collectPageHotspots(s, "generated")

	fmt.Printf("  %-10s  %6s  %6s  %6s\n", "Page", "Human", "Machine", "Delta")
	fmt.Printf("  %-10s  %6s  %6s  %6s\n", "----", "-----", "-------", "-----")

	// Union of top pages from both.
	topPages := map[string]bool{}
	count := 0
	for _, hp := range refHot {
		topPages[hp.page] = true
		count++
		if count >= 15 {
			break
		}
	}
	count = 0
	for _, hp := range genHot {
		topPages[hp.page] = true
		count++
		if count >= 15 {
			break
		}
	}

	refMap := map[string]int{}
	for _, hp := range refHot {
		refMap[hp.page] = hp.count
	}
	genMap := map[string]int{}
	for _, hp := range genHot {
		genMap[hp.page] = hp.count
	}

	type combined struct {
		page     string
		ref, gen int
	}
	var rows []combined
	for p := range topPages {
		rows = append(rows, combined{p, refMap[p], genMap[p]})
	}
	sort.Slice(rows, func(i, j int) bool {
		// Sort by human count descending.
		if rows[i].ref != rows[j].ref {
			return rows[i].ref > rows[j].ref
		}
		return rows[i].page < rows[j].page
	})

	for _, r := range rows {
		delta := r.gen - r.ref
		sign := "+"
		if delta < 0 {
			sign = ""
		}
		fmt.Printf("  %-10s  %6d  %6d  %s%d\n", r.page, r.ref, r.gen, sign, delta)
	}

	// --- The key question: what topology does the human build that the machine doesn't? ---
	fmt.Println("\n--- Structural capabilities: Human vs Machine ---")

	fmt.Printf("\n  Routing (See→target):     Human: YES (%d)  Machine: %s\n",
		len(refRoutes), yesNo(len(genRoutes)))
	fmt.Printf("  Convergence funnels:      Human: YES (%d)  Machine: %s\n",
		len(refFunnels), yesNo(len(genFunnels)))

	// Cross-refs (see-also).
	refSeeAlso := 0
	it := s.Match("", "see-also", "", "reference")
	for it.Next() {
		refSeeAlso++
	}
	it.Close()
	genSeeAlso := 0
	it = s.Match("", "see-also", "", "generated")
	for it.Next() {
		genSeeAlso++
	}
	it.Close()
	fmt.Printf("  See-also cross-refs:      Human: %d        Machine: %d\n",
		refSeeAlso, genSeeAlso)

	// Inverted terms (comma in term).
	refInverted := 0
	genInverted := 0
	it = s.Match("", "term", "", "reference")
	for it.Next() {
		if strings.Contains(it.Quad().Object, ",") {
			refInverted++
		}
	}
	it.Close()
	it = s.Match("", "term", "", "generated")
	for it.Next() {
		if strings.Contains(it.Quad().Object, ",") {
			genInverted++
		}
	}
	it.Close()
	fmt.Printf("  Inverted terms (a, b):    Human: %d        Machine: %d\n",
		refInverted, genInverted)

	// Parenthetical aliases.
	refParen := 0
	genParen := 0
	it = s.Match("", "term", "", "reference")
	for it.Next() {
		if strings.Contains(it.Quad().Object, "(") {
			refParen++
		}
	}
	it.Close()
	it = s.Match("", "term", "", "generated")
	for it.Next() {
		if strings.Contains(it.Quad().Object, "(") {
			genParen++
		}
	}
	it.Close()
	fmt.Printf("  Parenthetical aliases:    Human: %d        Machine: %d\n",
		refParen, genParen)
}

func yesNo(n int) string {
	if n > 0 {
		return fmt.Sprintf("YES (%d)", n)
	}
	return "NO (0)"
}

type roleMap map[string][]*entryData

type entryData struct {
	id        string
	term      string
	pageCount int
	subCount  int
	hasSee    bool
	inbound   int
}

func classifyRoles(s *quadstore.Store, label string) roleMap {
	entries := map[string]*entryData{}

	it := s.Match("", "term", "", label)
	for it.Next() {
		id := it.Quad().Subject
		if _, ok := entries[id]; !ok {
			entries[id] = &entryData{id: id, term: it.Quad().Object}
		}
	}
	it.Close()

	for id, e := range entries {
		pit := s.Match(id, "has-page", "", label)
		for pit.Next() {
			e.pageCount++
		}
		pit.Close()

		sit := s.Match(id, "has-sub-entry", "", label)
		for sit.Next() {
			e.subCount++
		}
		sit.Close()
	}

	it = s.Match("", "see", "", label)
	for it.Next() {
		if e, ok := entries[it.Quad().Subject]; ok {
			e.hasSee = true
		}
		if e, ok := entries[it.Quad().Object]; ok {
			e.inbound++
		}
	}
	it.Close()
	it = s.Match("", "see-also", "", label)
	for it.Next() {
		if e, ok := entries[it.Quad().Object]; ok {
			e.inbound++
		}
	}
	it.Close()

	roles := roleMap{}
	for _, e := range entries {
		var role string
		switch {
		case e.hasSee && e.pageCount == 0:
			role = "router"
		case e.subCount > 0 && e.pageCount == 0:
			role = "convergence"
		case e.pageCount > 0 && e.inbound > 0:
			role = "content+hub"
		case e.pageCount > 0:
			role = "content"
		default:
			role = "orphan"
		}
		roles[role] = append(roles[role], e)
	}
	return roles
}

func collectRoutes(s *quadstore.Store, label string) map[string]string {
	routes := map[string]string{}
	// Resolve terms for readability.
	termOf := map[string]string{}
	it := s.Match("", "term", "", label)
	for it.Next() {
		termOf[it.Quad().Subject] = it.Quad().Object
	}
	it.Close()

	it = s.Match("", "see", "", label)
	for it.Next() {
		from := termOf[it.Quad().Subject]
		to := termOf[it.Quad().Object]
		if from == "" {
			from = it.Quad().Subject
		}
		if to == "" {
			to = it.Quad().Object
		}
		routes[from] = to
	}
	it.Close()
	return routes
}

type funnelMap map[string][]string // parent term → sub-entry terms

func collectFunnels(s *quadstore.Store, label string) funnelMap {
	termOf := map[string]string{}
	it := s.Match("", "term", "", label)
	for it.Next() {
		termOf[it.Quad().Subject] = it.Quad().Object
	}
	it.Close()

	funnels := funnelMap{}
	it = s.Match("", "has-sub-entry", "", label)
	for it.Next() {
		parent := termOf[it.Quad().Subject]
		child := termOf[it.Quad().Object]
		if parent != "" && child != "" {
			funnels[parent] = append(funnels[parent], child)
		}
	}
	it.Close()

	// Sort sub-entries.
	for k := range funnels {
		sort.Strings(funnels[k])
	}
	return funnels
}

func termsOverlap(a, b string) bool {
	a = strings.ToLower(a)
	b = strings.ToLower(b)
	if a == b {
		return true
	}
	if strings.Contains(a, b) || strings.Contains(b, a) {
		return true
	}
	// Word overlap.
	aWords := strings.Fields(a)
	bWords := strings.Fields(b)
	hits := 0
	for _, aw := range aWords {
		for _, bw := range bWords {
			if aw == bw || aw+"s" == bw || bw+"s" == aw {
				hits++
				break
			}
		}
	}
	min := len(aWords)
	if len(bWords) < min {
		min = len(bWords)
	}
	return min >= 1 && float64(hits)/float64(min) >= 0.5
}

type pageHot struct {
	page  string
	count int
}

func collectPageHotspots(s *quadstore.Store, label string) []pageHot {
	pageCounts := map[string]int{}
	it := s.Match("", "has-page", "", label)
	for it.Next() {
		pageCounts[it.Quad().Object]++
	}
	it.Close()

	var hot []pageHot
	for p, c := range pageCounts {
		hot = append(hot, pageHot{p, c})
	}
	sort.Slice(hot, func(i, j int) bool { return hot[i].count > hot[j].count })
	return hot
}

func analyzeTopology(s *quadstore.Store, label string) {
	fmt.Println("=== INDEX TOPOLOGY ===")
	fmt.Printf("Label: %s\n\n", label)

	// Classify every entry by role.
	type entryInfo struct {
		id        string
		term      string
		role      string
		pageCount int
		subCount  int
		seeTarget string   // for routers
		inbound   []string // entries that route TO this one
	}

	entries := map[string]*entryInfo{}

	// Collect all entries.
	it := s.Match("", "term", "", label)
	for it.Next() {
		id := it.Quad().Subject
		if _, ok := entries[id]; !ok {
			entries[id] = &entryInfo{id: id, term: it.Quad().Object}
		}
	}
	it.Close()

	// Count pages per entry.
	for id, e := range entries {
		it := s.Match(id, "has-page", "", label)
		for it.Next() {
			e.pageCount++
		}
		it.Close()
	}

	// Count sub-entries.
	for id, e := range entries {
		it := s.Match(id, "has-sub-entry", "", label)
		for it.Next() {
			e.subCount++
		}
		it.Close()
	}

	// Trace "see" routing edges.
	it = s.Match("", "see", "", label)
	for it.Next() {
		fromID := it.Quad().Subject
		toID := it.Quad().Object
		if from, ok := entries[fromID]; ok {
			from.seeTarget = toID
		}
		if to, ok := entries[toID]; ok {
			to.inbound = append(to.inbound, fromID)
		}
	}
	it.Close()

	// Also trace "see-also" as weaker routing.
	it = s.Match("", "see-also", "", label)
	for it.Next() {
		toID := it.Quad().Object
		fromID := it.Quad().Subject
		if to, ok := entries[toID]; ok {
			to.inbound = append(to.inbound, fromID)
		}
	}
	it.Close()

	// Classify roles.
	var routers, convergence, content, hybrid []*entryInfo
	for _, e := range entries {
		switch {
		case e.seeTarget != "" && e.pageCount == 0:
			e.role = "router"
			routers = append(routers, e)
		case e.subCount > 0 && e.pageCount == 0:
			e.role = "convergence"
			convergence = append(convergence, e)
		case e.pageCount > 0 && len(e.inbound) > 0:
			e.role = "content+hub"
			hybrid = append(hybrid, e)
		case e.pageCount > 0:
			e.role = "content"
			content = append(content, e)
		default:
			e.role = "orphan"
		}
	}

	fmt.Printf("Roles:\n")
	fmt.Printf("  Content:       %d (has pages, no inbound routes)\n", len(content))
	fmt.Printf("  Content+Hub:   %d (has pages AND other entries route here)\n", len(hybrid))
	fmt.Printf("  Router:        %d (See → another entry, no own pages)\n", len(routers))
	fmt.Printf("  Convergence:   %d (parent heading, sub-entries only)\n", len(convergence))

	// Show the routing graph.
	fmt.Println("\n--- Routing graph (See → target) ---")
	sortByTerm := func(es []*entryInfo) {
		sort.Slice(es, func(i, j int) bool { return es[i].term < es[j].term })
	}
	sortByTerm(routers)
	for _, e := range routers {
		targetTerm := "?"
		if t, ok := entries[e.seeTarget]; ok {
			targetTerm = t.term
		}
		fmt.Printf("  %-35s → %s\n", e.term, targetTerm)
	}

	// Show convergence points — entries where multiple paths arrive.
	fmt.Println("\n--- Convergence hubs (most inbound routes) ---")
	var hubs []*entryInfo
	for _, e := range entries {
		if len(e.inbound) > 0 {
			hubs = append(hubs, e)
		}
	}
	sort.Slice(hubs, func(i, j int) bool { return len(hubs[i].inbound) > len(hubs[j].inbound) })
	for _, e := range hubs {
		sources := make([]string, len(e.inbound))
		for i, srcID := range e.inbound {
			if src, ok := entries[srcID]; ok {
				sources[i] = src.term
			} else {
				sources[i] = srcID
			}
		}
		sort.Strings(sources)
		fmt.Printf("  %-30s (%d inbound, %d pages, role: %s)\n",
			e.term, len(e.inbound), e.pageCount, e.role)
		for _, src := range sources {
			fmt.Printf("    ← %s\n", src)
		}
	}

	// Show convergence parents with their funnels.
	fmt.Println("\n--- Convergence funnels (parent → sub-entries) ---")
	sortByTerm(convergence)
	for _, e := range convergence {
		subIt := s.Match(e.id, "has-sub-entry", "", label)
		var subs []string
		for subIt.Next() {
			subID := subIt.Quad().Object
			if sub, ok := entries[subID]; ok {
				subs = append(subs, fmt.Sprintf("%s (%d pp)", sub.term, sub.pageCount))
			}
		}
		subIt.Close()
		sort.Strings(subs)
		fmt.Printf("  %s [%d sub-entries]\n", e.term, len(subs))
		for _, sub := range subs {
			fmt.Printf("    └ %s\n", sub)
		}
	}

	// Page convergence: which pages are reached from the most different entries?
	fmt.Println("\n--- Page hotspots (pages reached by most entries) ---")
	pageReach := map[string][]string{} // page → entry terms
	for _, e := range entries {
		if e.pageCount == 0 {
			continue
		}
		pageIt := s.Match(e.id, "has-page", "", label)
		for pageIt.Next() {
			page := pageIt.Quad().Object
			pageReach[page] = append(pageReach[page], e.term)
		}
		pageIt.Close()
	}
	type pageHot struct {
		page  string
		count int
		terms []string
	}
	var hotPages []pageHot
	for page, terms := range pageReach {
		sort.Strings(terms)
		hotPages = append(hotPages, pageHot{page, len(terms), terms})
	}
	sort.Slice(hotPages, func(i, j int) bool { return hotPages[i].count > hotPages[j].count })
	limit := 15
	if len(hotPages) < limit {
		limit = len(hotPages)
	}
	for _, hp := range hotPages[:limit] {
		fmt.Printf("  %-10s reached by %d entries\n", hp.page, hp.count)
		termStr := strings.Join(hp.terms, ", ")
		if len(termStr) > 100 {
			termStr = termStr[:100] + "..."
		}
		fmt.Printf("    %s\n", termStr)
	}
}
