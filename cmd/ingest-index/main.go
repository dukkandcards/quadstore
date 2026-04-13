// Command ingest-index loads index data into a quadstore from two sources:
//
//  1. Reference DOCX — the professional indexer's human-created index
//  2. Workspace — the NLP pipeline's machine-generated index
//
// Both go into the same store with separate labels so we can compare
// what the human declared important vs what the machine found.
//
// Usage:
//
//	go run ./cmd/ingest-index \
//	  -ref Woodpeckers_submit.docx \
//	  -ws /tmp/woodpeckers-ws \
//	  -db /tmp/woodpeckers.db
package main

import (
	"archive/zip"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/dukkandcards/quadstore"
)

func main() {
	refPath := flag.String("ref", "", "path to reference index DOCX (human)")
	wsPath := flag.String("ws", "", "path to mega-index workspace dir (machine)")
	dbPath := flag.String("db", "", "path to quadstore SQLite file")
	flag.Parse()

	if *dbPath == "" {
		flag.Usage()
		os.Exit(1)
	}
	if *refPath == "" && *wsPath == "" {
		fmt.Fprintln(os.Stderr, "provide at least one of -ref or -ws")
		os.Exit(1)
	}

	// Fresh start for repeatable runs.
	os.Remove(*dbPath)

	store, err := quadstore.Open(*dbPath)
	if err != nil {
		log.Fatalf("opening quadstore: %v", err)
	}
	defer store.Close()

	// --- Load reference (human) index ---
	if *refPath != "" {
		entries, err := parseReferenceDocx(*refPath)
		if err != nil {
			log.Fatalf("parsing reference DOCX: %v", err)
		}
		quads := entriesToQuads(entries, "reference")
		if err := store.AddBatch(quads); err != nil {
			log.Fatalf("adding reference quads: %v", err)
		}
		fmt.Printf("[reference] %d entries → %d quads\n", len(entries), len(quads))
	}

	// --- Load generated (machine) index ---
	if *wsPath != "" {
		entries, err := loadWorkspace(*wsPath)
		if err != nil {
			log.Fatalf("loading workspace: %v", err)
		}
		quads := workspaceToQuads(entries, "generated")
		if err := store.AddBatch(quads); err != nil {
			log.Fatalf("adding generated quads: %v", err)
		}
		fmt.Printf("[generated] %d entries → %d quads\n", len(entries), len(quads))
	}

	total, preds, _ := store.Stats()
	fmt.Printf("\nTotal: %d quads, %d predicates\n", total, preds)

	// --- Comparison queries ---
	if *refPath != "" && *wsPath != "" {
		fmt.Println("\n=== Human vs Machine ===")
		compareIndexes(store)
	} else if *refPath != "" {
		fmt.Println("\n=== Reference index ===")
		summarizeLabel(store, "reference")
	} else {
		fmt.Println("\n=== Generated index ===")
		summarizeLabel(store, "generated")
	}

	// Topology analysis.
	if *refPath != "" && *wsPath != "" {
		fmt.Println()
		compareTopology(store)
	} else if *refPath != "" {
		fmt.Println()
		analyzeTopology(store, "reference")
	}

	// Compute and observe signals.
	if *refPath != "" {
		fmt.Println()
		computeSignals(store, "reference")
		computePredictionError(store, "reference")
		greedyReduce(store, "reference")
		computeWeightedEdges(store, "reference")
		computeBackgroundRate(store, "reference")
		analyzeContentions(store, "reference", modeIndex)
		analyzeInterEntryStructure(store, "reference")
	}
	if *wsPath != "" {
		fmt.Println()
		computeSignals(store, "generated")
		computePredictionError(store, "generated")
		greedyReduce(store, "generated")
		computeWeightedEdges(store, "generated")
		computeBackgroundRate(store, "generated")
		analyzeContentions(store, "generated", modeIndex)
		analyzeInterEntryStructure(store, "generated")

		// Bring in page text and compute aboutness.
		pages, err := ingestPageContent(store, *wsPath, "generated")
		if err != nil {
			log.Printf("page ingest: %v", err)
		} else {
			computeAboutness(store, "generated", pages)
		}

		// Tune formulas across all signals.
		tuneFormulas(store)

		// The real process: subtraction.
		subtractToIndex(store)
	}

	// Shape.
	shape, err := store.Shape()
	if err != nil {
		log.Fatalf("shape: %v", err)
	}
	fmt.Printf("\nShape: %d nodes, %d edges, predicates: %v\n",
		shape.NodeCount, len(shape.Edges), shape.Predicates)
}

func summarizeLabel(s *quadstore.Store, label string) {
	it := s.Match("book:woodpeckers", "has-entry", "", label)
	count := 0
	for it.Next() {
		count++
	}
	it.Close()
	fmt.Printf("Main entries: %d\n", count)
}

func compareIndexes(s *quadstore.Store) {
	// Collect terms from each label.
	refTerms := collectTerms(s, "reference")
	genTerms := collectTerms(s, "generated")

	fmt.Printf("Reference terms: %d\n", len(refTerms))
	fmt.Printf("Generated terms: %d\n", len(genTerms))

	// Overlap analysis.
	both := 0
	refOnly := 0
	genOnly := 0
	var bothList []string
	var refOnlyList []string
	var genOnlyTop []string // top confidence generated-only

	for term := range refTerms {
		if _, ok := genTerms[term]; ok {
			both++
			bothList = append(bothList, term)
		} else {
			refOnly++
			refOnlyList = append(refOnlyList, term)
		}
	}
	for term := range genTerms {
		if _, ok := refTerms[term]; !ok {
			genOnly++
			genOnlyTop = append(genOnlyTop, term)
		}
	}

	fmt.Printf("\nOverlap: %d terms in both\n", both)
	fmt.Printf("Human only: %d (machine missed these)\n", refOnly)
	fmt.Printf("Machine only: %d (human didn't include these)\n", genOnly)

	if both > 0 {
		pct := float64(both) / float64(len(refTerms)) * 100
		fmt.Printf("Coverage: machine found %.0f%% of human's terms\n", pct)
	}

	// Show some examples.
	sort.Strings(refOnlyList)
	sort.Strings(bothList)

	fmt.Println("\n--- Human only (sample, machine missed) ---")
	limit := 15
	if len(refOnlyList) < limit {
		limit = len(refOnlyList)
	}
	for _, t := range refOnlyList[:limit] {
		fmt.Printf("  %s\n", t)
	}
	if len(refOnlyList) > limit {
		fmt.Printf("  ... and %d more\n", len(refOnlyList)-limit)
	}

	fmt.Println("\n--- Both agree (sample) ---")
	limit = 15
	if len(bothList) < limit {
		limit = len(bothList)
	}
	for _, t := range bothList[:limit] {
		fmt.Printf("  %s\n", t)
	}
	if len(bothList) > limit {
		fmt.Printf("  ... and %d more\n", len(bothList)-limit)
	}

	// Page coverage comparison for shared terms.
	fmt.Println("\n--- Page agreement for shared terms (sample) ---")
	sort.Strings(bothList)
	shown := 0
	for _, term := range bothList {
		if shown >= 10 {
			break
		}
		id := termToID(term)
		refPages := collectPages(s, id, "reference")
		genPages := collectPages(s, id, "generated")
		if len(refPages) == 0 && len(genPages) == 0 {
			continue
		}

		overlap := pageOverlap(refPages, genPages)
		fmt.Printf("  %-30s ref:%2d gen:%2d overlap:%2d\n",
			term, len(refPages), len(genPages), overlap)
		shown++
	}
}

func collectTerms(s *quadstore.Store, label string) map[string]bool {
	terms := map[string]bool{}
	it := s.Match("", "term", "", label)
	for it.Next() {
		terms[strings.ToLower(it.Quad().Object)] = true
	}
	it.Close()
	return terms
}

func collectPages(s *quadstore.Store, entryID, label string) map[string]bool {
	pages := map[string]bool{}
	it := s.Match(entryID, "has-page", "", label)
	for it.Next() {
		pages[it.Quad().Object] = true
	}
	it.Close()
	return pages
}

func pageOverlap(a, b map[string]bool) int {
	count := 0
	for p := range a {
		if b[p] {
			count++
		}
	}
	return count
}

// --- DOCX parsing (minimal, from mega-index benchmark/reference.go) ---

type refEntry struct {
	term      string
	isMain    bool
	parent    string
	pages     string // raw page string, kept as-is for quads
	crossRefs []crossRef
	hasNote   bool
	rawText   string
}

type crossRef struct {
	typ    string // "see" or "see_also"
	target string
}

func parseReferenceDocx(path string) ([]refEntry, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	var docFile *zip.File
	for _, f := range r.File {
		if f.Name == "word/document.xml" {
			docFile = f
			break
		}
	}
	if docFile == nil {
		return nil, fmt.Errorf("word/document.xml not found")
	}

	rc, err := docFile.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}

	return parseDocXML(data)
}

type wxDoc struct {
	Body wxBody `xml:"body"`
}
type wxBody struct {
	Paragraphs []wxPara `xml:"p"`
}
type wxPara struct {
	Props wxPProps `xml:"pPr"`
	Runs  []wxRun  `xml:"r"`
}
type wxPProps struct {
	Style wxStyleVal `xml:"pStyle"`
}
type wxStyleVal struct {
	Val string `xml:"val,attr"`
}
type wxRun struct {
	Text wxText `xml:"t"`
}
type wxText struct {
	Value string `xml:",chardata"`
}

var (
	seeAlsoRe = regexp.MustCompile(`\.\s*See also\s+(.+)$`)
	seeRe     = regexp.MustCompile(`\.\s*See\s+(.+)$`)
	noteRe    = regexp.MustCompile(`\{[^}]+\}`)
	pageStartRe = regexp.MustCompile(`,\s*(\d|i{1,4}|iv|vi{0,3}|ix|x{1,3})`)
)

func parseDocXML(data []byte) ([]refEntry, error) {
	var doc wxDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}

	var entries []refEntry
	var currentMain string

	for _, para := range doc.Body.Paragraphs {
		style := para.Props.Style.Val
		if style != "Main" && style != "Sub1" {
			continue
		}

		var fullText string
		for _, run := range para.Runs {
			fullText += run.Text.Value
		}
		fullText = strings.TrimSpace(fullText)
		if fullText == "" {
			continue
		}

		e := refEntry{
			rawText: fullText,
			isMain:  style == "Main",
			hasNote: noteRe.MatchString(fullText),
		}

		// Strip notes for parsing.
		cleaned := noteRe.ReplaceAllString(fullText, "")

		// Extract cross-refs.
		if m := seeAlsoRe.FindStringSubmatch(cleaned); m != nil {
			for _, t := range strings.Split(m[1], ";") {
				t = strings.TrimSpace(t)
				if t != "" {
					e.crossRefs = append(e.crossRefs, crossRef{"see_also", t})
				}
			}
			cleaned = seeAlsoRe.ReplaceAllString(cleaned, "")
		} else if m := seeRe.FindStringSubmatch(cleaned); m != nil {
			for _, t := range strings.Split(m[1], ";") {
				t = strings.TrimSpace(t)
				if t != "" {
					e.crossRefs = append(e.crossRefs, crossRef{"see", t})
				}
			}
			cleaned = seeRe.ReplaceAllString(cleaned, "")
		}

		// Extract term.
		if loc := pageStartRe.FindStringIndex(cleaned); loc != nil {
			e.term = strings.TrimSpace(cleaned[:loc[0]])
			e.pages = strings.TrimLeft(cleaned[loc[0]:], ", ")
		} else {
			e.term = strings.TrimRight(strings.TrimSpace(cleaned), " ,.")
		}

		if style == "Main" {
			currentMain = e.term
		} else {
			e.parent = currentMain
		}

		entries = append(entries, e)
	}
	return entries, nil
}

// --- Quad conversion ---

func termToID(term string) string {
	id := strings.ToLower(term)
	id = strings.ReplaceAll(id, " ", "-")
	id = strings.ReplaceAll(id, ",", "")
	id = strings.ReplaceAll(id, "'", "")
	id = strings.ReplaceAll(id, ".", "")
	return "entry:" + id
}

func entriesToQuads(entries []refEntry, label string) []quadstore.Quad {
	var quads []quadstore.Quad
	add := func(s, p, o string) {
		quads = append(quads, quadstore.Quad{Subject: s, Predicate: p, Object: o, Label: label})
	}

	book := "book:woodpeckers"
	add(book, "title", "The Woodpeckers")
	add(book, "author", "Fannie Hardy Eckstorm")
	add(book, "indexer", "Michelle Guiliano")

	for _, e := range entries {
		id := termToID(e.term)
		add(id, "term", e.term)
		add(id, "raw-text", e.rawText)

		if e.isMain {
			add(book, "has-entry", id)
			add(id, "type", "main-entry")
		} else {
			add(id, "type", "sub-entry")
			if e.parent != "" {
				parentID := termToID(e.parent)
				add(parentID, "has-sub-entry", id)
			}
		}

		if e.pages != "" {
			// Store individual page refs as separate quads for traversal.
			// Also store the raw page string for display.
			add(id, "pages-raw", e.pages)
			for _, p := range parsePageNumbers(e.pages) {
				add(id, "has-page", p)
			}
		}

		for _, xr := range e.crossRefs {
			targetID := termToID(xr.target)
			add(id, xr.typ, targetID)
			// Ensure the target exists as a node even if not yet seen.
			add(targetID, "term", xr.target)
		}

		if e.hasNote {
			add(id, "has-note", "true")
		}
	}

	return quads
}

var pageNumRe = regexp.MustCompile(`\b(\d+)\b`)

func parsePageNumbers(raw string) []string {
	matches := pageNumRe.FindAllString(raw, -1)
	// Deduplicate.
	seen := map[string]bool{}
	var result []string
	for _, m := range matches {
		page := "page:" + m
		if !seen[page] {
			seen[page] = true
			result = append(result, page)
		}
	}
	return result
}
