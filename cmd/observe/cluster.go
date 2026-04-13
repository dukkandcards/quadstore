package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// clusterPages groups pages by textual similarity. No naming, no theory.
// Just: these pages share enough vocabulary to belong together.
//
// The indexer sees the groups and names them. We find the clusters.
//
// Method: build a word vector per page (TF, skip stopwords and very
// common words), compute cosine similarity between all page pairs,
// group pages that exceed a similarity threshold.
type pageCluster struct {
	pages   []int
	shared  []string
	chapter string
}

func clusterPages(wsPath string) {
	fmt.Println("=== PAGE CLUSTERS ===")
	fmt.Println("Groups of pages that share vocabulary. Unnamed.")
	fmt.Println()

	// Load pages.
	type pageData struct {
		num     int
		chapter string
		text    string
		words   map[string]int // word → count
		norm    float64        // vector norm for cosine
	}

	var pages []*pageData
	files, _ := filepath.Glob(filepath.Join(wsPath, "pages", "*.json"))
	for _, f := range files {
		data, _ := os.ReadFile(f)
		var p struct {
			PhysicalPage int    `json:"physical_page"`
			Chapter      string `json:"chapter"`
			RawText      string `json:"raw_text"`
		}
		json.Unmarshal(data, &p)
		if p.PhysicalPage == 0 || p.RawText == "" {
			continue
		}
		pages = append(pages, &pageData{
			num:     p.PhysicalPage,
			chapter: p.Chapter,
			text:    p.RawText,
			words:   make(map[string]int),
		})
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].num < pages[j].num })

	// Build word vectors.
	stopwords := buildStopwords()

	// First pass: document frequency (how many pages contain each word).
	docFreq := map[string]int{}
	for _, p := range pages {
		seen := map[string]bool{}
		for _, w := range tokenize(p.text) {
			if stopwords[w] || len(w) < 3 {
				continue
			}
			p.words[w]++
			if !seen[w] {
				docFreq[w]++
				seen[w] = true
			}
		}
	}

	totalDocs := float64(len(pages))

	// Remove words that appear on >50% of pages (background) or <2 pages (noise).
	for w, df := range docFreq {
		if float64(df)/totalDocs > 0.50 || df < 2 {
			delete(docFreq, w)
			for _, p := range pages {
				delete(p.words, w)
			}
		}
	}

	// Compute TF-IDF weights and vector norms.
	type weightedPage struct {
		num     int
		chapter string
		vec     map[string]float64
		norm    float64
		top     []string // top weighted words
	}

	var wPages []*weightedPage
	for _, p := range pages {
		wp := &weightedPage{
			num:     p.num,
			chapter: p.chapter,
			vec:     make(map[string]float64),
		}
		for w, tf := range p.words {
			df := docFreq[w]
			if df == 0 {
				continue
			}
			idf := math.Log(totalDocs / float64(df))
			weight := float64(tf) * idf
			wp.vec[w] = weight
			wp.norm += weight * weight
		}
		wp.norm = math.Sqrt(wp.norm)

		// Top words by weight.
		type ww struct {
			word   string
			weight float64
		}
		var sorted []ww
		for w, weight := range wp.vec {
			sorted = append(sorted, ww{w, weight})
		}
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].weight > sorted[j].weight })
		limit := 8
		if len(sorted) < limit {
			limit = len(sorted)
		}
		for _, s := range sorted[:limit] {
			wp.top = append(wp.top, s.word)
		}

		wPages = append(wPages, wp)
	}

	// Compute pairwise cosine similarity.
	type pagePair struct {
		a, b       int
		similarity float64
	}

	var pairs []pagePair
	for i := 0; i < len(wPages); i++ {
		for j := i + 1; j < len(wPages); j++ {
			sim := cosine(wPages[i].vec, wPages[j].vec, wPages[i].norm, wPages[j].norm)
			if sim >= 0.15 { // low threshold to catch clusters
				pairs = append(pairs, pagePair{wPages[i].num, wPages[j].num, sim})
			}
		}
	}

	sort.Slice(pairs, func(i, j int) bool { return pairs[i].similarity > pairs[j].similarity })

	// Build clusters via single-linkage at threshold 0.20.
	threshold := 0.20
	parent := map[int]int{} // union-find
	var find func(int) int
	find = func(x int) int {
		p, ok := parent[x]
		if !ok {
			return x
		}
		root := find(p)
		parent[x] = root
		return root
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}

	for _, p := range pairs {
		if p.similarity >= threshold {
			union(p.a, p.b)
		}
	}

	// Collect clusters.
	clusters := map[int][]int{} // root → page numbers
	for _, wp := range wPages {
		root := find(wp.num)
		clusters[root] = append(clusters[root], wp.num)
	}

	// Sort clusters by size.
	var clusterList []pageCluster
	for _, pageNums := range clusters {
		if len(pageNums) < 2 {
			continue // skip singletons
		}
		sort.Ints(pageNums)

		// Find words shared by >50% of pages in this cluster.
		wordCount := map[string]int{}
		chapCount := map[string]int{}
		for _, pn := range pageNums {
			for _, wp := range wPages {
				if wp.num == pn {
					for w := range wp.vec {
						wordCount[w]++
					}
					if wp.chapter != "" {
						chapCount[wp.chapter]++
					}
					break
				}
			}
		}

		threshold := len(pageNums) / 2
		if threshold < 2 {
			threshold = 2
		}
		var shared []string
		for w, c := range wordCount {
			if c >= threshold {
				shared = append(shared, w)
			}
		}
		sort.Strings(shared)

		// Truncate shared words.
		if len(shared) > 12 {
			shared = shared[:12]
		}

		// Most common chapter.
		bestChap := ""
		bestChapCount := 0
		for ch, c := range chapCount {
			if c > bestChapCount {
				bestChap = ch
				bestChapCount = c
			}
		}

		clusterList = append(clusterList, pageCluster{pageNums, shared, bestChap})
	}

	sort.Slice(clusterList, func(i, j int) bool { return len(clusterList[i].pages) > len(clusterList[j].pages) })

	// Present.
	fmt.Printf("Found %d clusters (threshold: %.2f cosine similarity)\n", len(clusterList), threshold)
	fmt.Printf("Singletons (unclustered pages): %d\n\n",
		len(wPages)-countClustered(clusterList))

	for i, cl := range clusterList {
		fmt.Printf("CLUSTER %d — %d pages\n", i+1, len(cl.pages))
		if cl.chapter != "" {
			fmt.Printf("  Primary chapter: %s\n", cl.chapter)
		}

		// Show page range compactly.
		fmt.Printf("  Pages: %s\n", formatPageList(cl.pages))

		// Show shared vocabulary.
		if len(cl.shared) > 0 {
			fmt.Printf("  Shared words: %s\n", strings.Join(cl.shared, ", "))
		}

		// Show top words per page (the texture of the cluster).
		fmt.Println("  Page details:")
		for _, pn := range cl.pages {
			for _, wp := range wPages {
				if wp.num == pn {
					chap := ""
					if wp.chapter != "" {
						chap = fmt.Sprintf("  [%s]", wp.chapter)
					}
					fmt.Printf("    p.%-3d  top: %-50s%s\n", pn,
						strings.Join(wp.top, ", "), chap)
					break
				}
			}
		}
		fmt.Println()
	}
}

func tokenize(text string) []string {
	text = strings.ToLower(text)
	// Replace punctuation with spaces.
	replacer := strings.NewReplacer(
		".", " ", ",", " ", ";", " ", ":", " ", "!", " ", "?", " ",
		"(", " ", ")", " ", "[", " ", "]", " ", "\"", " ", "'", " ",
		"\u201c", " ", "\u201d", " ", "\u2018", " ", "\u2019", " ",
		"\n", " ", "\r", " ", "\t", " ", "\u2014", " ", "\u2013", " ",
		"/", " ",
	)
	text = replacer.Replace(text)
	return strings.Fields(text)
}

func buildStopwords() map[string]bool {
	words := []string{
		"the", "a", "an", "and", "or", "but", "in", "on", "at", "to",
		"for", "of", "with", "by", "from", "as", "is", "was", "are",
		"were", "be", "been", "being", "have", "has", "had", "do",
		"does", "did", "will", "would", "shall", "should", "may",
		"might", "can", "could", "not", "no", "nor", "so", "if",
		"than", "that", "this", "these", "those", "it", "its",
		"he", "she", "him", "her", "his", "they", "them", "their",
		"we", "us", "our", "you", "your", "who", "whom", "which",
		"what", "when", "where", "how", "why", "all", "each", "every",
		"both", "few", "more", "most", "other", "some", "such",
		"only", "own", "same", "very", "just", "also", "into",
		"about", "up", "out", "off", "over", "under", "again",
		"then", "once", "here", "there", "any", "too", "much",
		"many", "one", "two", "three", "four", "five",
	}
	m := map[string]bool{}
	for _, w := range words {
		m[w] = true
	}
	return m
}

func cosine(a, b map[string]float64, normA, normB float64) float64 {
	if normA == 0 || normB == 0 {
		return 0
	}
	dot := 0.0
	for w, wa := range a {
		if wb, ok := b[w]; ok {
			dot += wa * wb
		}
	}
	return dot / (normA * normB)
}

func countClustered(clusters []pageCluster) int {
	n := 0
	for _, cl := range clusters {
		n += len(cl.pages)
	}
	return n
}

func formatPageList(pages []int) string {
	if len(pages) == 0 {
		return ""
	}
	var parts []string
	start := pages[0]
	end := pages[0]
	for i := 1; i < len(pages); i++ {
		if pages[i] == end+1 {
			end = pages[i]
		} else {
			if start == end {
				parts = append(parts, fmt.Sprintf("%d", start))
			} else {
				parts = append(parts, fmt.Sprintf("%d-%d", start, end))
			}
			start = pages[i]
			end = pages[i]
		}
	}
	if start == end {
		parts = append(parts, fmt.Sprintf("%d", start))
	} else {
		parts = append(parts, fmt.Sprintf("%d-%d", start, end))
	}
	return strings.Join(parts, ", ")
}
