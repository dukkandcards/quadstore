package main

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/dukkandcards/quadstore"
)

// A contention is a human assertion that two things correspond.
//
// In an INDEX:
//   subject = index entry term
//   target  = page location
//   assertion: "this concept lives at this location in the book"
//
// In a CHART:
//   subject = source element (prior art passage, product feature)
//   target  = claim element (patent claim language)
//   assertion: "this source teaches/meets this claim element"
//
// The contention model is the same. The error tolerance differs:
//   INDEX mode:  accepts gaps (compression), penalizes redundancy (noise)
//   CHART mode:  fears gaps (exposure), values redundancy (thoroughness)
//
// Both modes compute the same signals. The interpretation flips.

type contentionMode int

const (
	modeIndex contentionMode = iota // compression: accept gaps, penalize redundancy
	modeChart                       // coverage: fear gaps, value redundancy
)

func (m contentionMode) String() string {
	if m == modeChart {
		return "CHART"
	}
	return "INDEX"
}

type contention struct {
	subjectID   string
	subjectTerm string
	targets     []string // page IDs or claim element IDs
}

// analyzeContentions runs the unified contention analysis.
// For now we only have index data; chart data uses the same path later.
func analyzeContentions(s *quadstore.Store, label string, mode contentionMode) {
	fmt.Printf("\n=== CONTENTION ANALYSIS (label: %s, mode: %s) ===\n\n", label, mode)

	cLabel := label + ":contention"

	// Gather contentions (subject → targets).
	contentions := map[string]*contention{}
	it := s.Match("", "term", "", label)
	for it.Next() {
		id := it.Quad().Subject
		if _, ok := contentions[id]; !ok {
			contentions[id] = &contention{subjectID: id, subjectTerm: it.Quad().Object}
		}
	}
	it.Close()

	for id, c := range contentions {
		pit := s.Match(id, "has-page", "", label)
		for pit.Next() {
			c.targets = append(c.targets, pit.Quad().Object)
		}
		pit.Close()
	}

	// Target → subjects reverse index.
	targetSubjects := map[string][]string{}
	for id, c := range contentions {
		for _, t := range c.targets {
			targetSubjects[t] = append(targetSubjects[t], id)
		}
	}

	allTargets := map[string]bool{}
	for t := range targetSubjects {
		allTargets[t] = true
	}

	var quads []quadstore.Quad
	add := func(id, pred, val string) {
		quads = append(quads, quadstore.Quad{Subject: id, Predicate: pred, Object: val, Label: cLabel})
	}

	// --- Contention strength signals ---

	type contentionScore struct {
		term string
		id   string

		// Signal 1: Coverage breadth — fraction of all targets this subject touches.
		coverageBreadth float64

		// Signal 2: Exclusivity — fraction of this subject's targets that only
		// it reaches. High = this subject makes unique assertions.
		exclusivity float64

		// Signal 3: Redundancy — average number of OTHER subjects asserting
		// the same target. High = this subject's assertions are all backed up.
		redundancy float64

		// Signal 4: Contention density — how many targets per subject.
		// Raw count, normalized to the max.
		density float64

		// Signal 5: Agreement — for this subject's targets, how often do
		// co-occurring subjects also share OTHER targets? High = subjects
		// that map to the same targets tend to travel together (semantic cluster).
		agreement float64

		// Composite scores (same signals, different interpretation).
		indexValue float64 // for INDEX: high exclusivity + low redundancy = valuable
		chartValue float64 // for CHART: high redundancy + low gaps = valuable
	}

	// Compute raw signals.
	maxTargets := 0
	for _, c := range contentions {
		if len(c.targets) > maxTargets {
			maxTargets = len(c.targets)
		}
	}

	var scores []contentionScore

	for id, c := range contentions {
		if len(c.targets) == 0 {
			continue
		}

		cs := contentionScore{
			term: c.subjectTerm,
			id:   id,
		}

		// Coverage breadth.
		cs.coverageBreadth = float64(len(c.targets)) / float64(len(allTargets))

		// Exclusivity.
		exclusive := 0
		for _, t := range c.targets {
			if len(targetSubjects[t]) == 1 {
				exclusive++
			}
		}
		cs.exclusivity = float64(exclusive) / float64(len(c.targets))

		// Redundancy.
		totalOthers := 0
		for _, t := range c.targets {
			totalOthers += len(targetSubjects[t]) - 1
		}
		cs.redundancy = float64(totalOthers) / float64(len(c.targets))

		// Density (normalized).
		if maxTargets > 0 {
			cs.density = float64(len(c.targets)) / float64(maxTargets)
		}

		// Agreement: do co-subjects share other targets too?
		if len(c.targets) > 0 {
			coSubjects := map[string]int{} // other subject → shared target count
			for _, t := range c.targets {
				for _, otherID := range targetSubjects[t] {
					if otherID != id {
						coSubjects[otherID]++
					}
				}
			}
			if len(coSubjects) > 0 {
				// For each co-subject, what fraction of THIS subject's targets
				// does the co-subject also map to?
				totalAgreement := 0.0
				for _, shared := range coSubjects {
					totalAgreement += float64(shared) / float64(len(c.targets))
				}
				cs.agreement = totalAgreement / float64(len(coSubjects))
			}
		}

		// Composite: INDEX value.
		// High exclusivity (unique assertions) + foreground (not background)
		// + moderate density (not too broad, not too narrow).
		// Penalize high redundancy (every target is also asserted by many others).
		cs.indexValue = cs.exclusivity*0.35 +
			(1.0-math.Min(cs.redundancy/20.0, 1.0))*0.30 +
			cs.density*0.15 +
			(1.0-cs.coverageBreadth)*0.20

		// Composite: CHART value.
		// High redundancy (well-supported assertions) + high coverage (thorough)
		// + high agreement (co-subjects reinforce each other).
		// Penalize exclusivity (unique = unsupported = risky).
		cs.chartValue = math.Min(cs.redundancy/10.0, 1.0)*0.30 +
			cs.coverageBreadth*0.25 +
			cs.agreement*0.25 +
			(1.0-cs.exclusivity)*0.20

		add(id, "coverage-breadth", fmt.Sprintf("%.4f", cs.coverageBreadth))
		add(id, "exclusivity", fmt.Sprintf("%.4f", cs.exclusivity))
		add(id, "redundancy", fmt.Sprintf("%.4f", cs.redundancy))
		add(id, "contention-density", fmt.Sprintf("%.4f", cs.density))
		add(id, "agreement", fmt.Sprintf("%.4f", cs.agreement))
		add(id, "index-value", fmt.Sprintf("%.4f", cs.indexValue))
		add(id, "chart-value", fmt.Sprintf("%.4f", cs.chartValue))

		scores = append(scores, cs)
	}

	if err := s.AddBatch(quads); err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	fmt.Printf("Stored %d contention quads (label: %s)\n\n", len(quads), cLabel)

	// --- Observations ---

	// Sort by the relevant composite score.
	if mode == modeIndex {
		sort.Slice(scores, func(i, j int) bool { return scores[i].indexValue > scores[j].indexValue })
		fmt.Println("--- Highest INDEX value (exclusive, discriminating, not redundant) ---")
		fmt.Println("These entries make unique assertions the index needs.")
	} else {
		sort.Slice(scores, func(i, j int) bool { return scores[i].chartValue > scores[j].chartValue })
		fmt.Println("--- Highest CHART value (redundant, thorough, agreed-upon) ---")
		fmt.Println("These assertions are well-supported by multiple sources.")
	}

	fmt.Printf("  %-35s  %6s  %6s  %6s  %6s  %6s  %6s\n",
		"Term", "Cover", "Excl", "Redund", "Dense", "Agree", "Score")
	fmt.Printf("  %-35s  %6s  %6s  %6s  %6s  %6s  %6s\n",
		"----", "-----", "----", "------", "-----", "-----", "-----")
	limit := 25
	if len(scores) < limit {
		limit = len(scores)
	}
	for _, cs := range scores[:limit] {
		score := cs.indexValue
		if mode == modeChart {
			score = cs.chartValue
		}
		fmt.Printf("  %-35s  %5.1f%%  %5.1f%%  %6.1f  %5.1f%%  %5.1f%%  %.3f\n",
			cs.term, cs.coverageBreadth*100, cs.exclusivity*100,
			cs.redundancy, cs.density*100, cs.agreement*100, score)
	}

	// Now show the opposite end — what this mode considers least valuable.
	fmt.Println()
	if mode == modeIndex {
		sort.Slice(scores, func(i, j int) bool { return scores[i].indexValue < scores[j].indexValue })
		fmt.Println("--- Lowest INDEX value (fully redundant, background) ---")
		fmt.Println("Every assertion this entry makes is also made by many others.")
	} else {
		sort.Slice(scores, func(i, j int) bool { return scores[i].chartValue < scores[j].chartValue })
		fmt.Println("--- Lowest CHART value (exclusive, unsupported) ---")
		fmt.Println("These assertions lack corroboration — exposure risk.")
	}

	fmt.Printf("  %-35s  %6s  %6s  %6s  %6s  %6s  %6s\n",
		"Term", "Cover", "Excl", "Redund", "Dense", "Agree", "Score")
	fmt.Printf("  %-35s  %6s  %6s  %6s  %6s  %6s  %6s\n",
		"----", "-----", "----", "------", "-----", "-----", "-----")
	limit = 20
	if len(scores) < limit {
		limit = len(scores)
	}
	for _, cs := range scores[:limit] {
		score := cs.indexValue
		if mode == modeChart {
			score = cs.chartValue
		}
		fmt.Printf("  %-35s  %5.1f%%  %5.1f%%  %6.1f  %5.1f%%  %5.1f%%  %.3f\n",
			cs.term, cs.coverageBreadth*100, cs.exclusivity*100,
			cs.redundancy, cs.density*100, cs.agreement*100, score)
	}

	// Cross-check: if we have both labels, compare index-scored vs human.
	if label == "generated" {
		fmt.Println("\n--- INDEX-scored greedy vs human ---")
		humanTerms := map[string]bool{}
		hit := s.Match("", "term", "", "reference")
		for hit.Next() {
			humanTerms[strings.ToLower(hit.Quad().Object)] = true
		}
		hit.Close()

		sort.Slice(scores, func(i, j int) bool { return scores[i].indexValue > scores[j].indexValue })
		hits := 0
		limit := 30
		if len(scores) < limit {
			limit = len(scores)
		}
		for i, cs := range scores[:limit] {
			match := ""
			if humanTerms[strings.ToLower(cs.term)] {
				match = " ← HUMAN"
				hits++
			}
			fmt.Printf("  #%-3d  %-35s  idx:%.3f  chart:%.3f%s\n",
				i+1, cs.term, cs.indexValue, cs.chartValue, match)
		}
		fmt.Printf("\n  Human matches in top %d: %d (%.1f%%)\n",
			limit, hits, float64(hits)/float64(limit)*100)

		// Same data, CHART-scored.
		fmt.Println("\n--- Same entries, CHART-scored (what a lawyer would value) ---")
		sort.Slice(scores, func(i, j int) bool { return scores[i].chartValue > scores[j].chartValue })
		for i, cs := range scores[:limit] {
			match := ""
			if humanTerms[strings.ToLower(cs.term)] {
				match = " ← HUMAN"
			}
			fmt.Printf("  #%-3d  %-35s  chart:%.3f  idx:%.3f%s\n",
				i+1, cs.term, cs.chartValue, cs.indexValue, match)
		}
	}
}
