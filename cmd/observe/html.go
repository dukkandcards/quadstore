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

// generateClusterHTML builds an interactive HTML page showing page clusters.
// The indexer opens it in a browser, reviews clusters, names them,
// splits/merges as needed, and exports the result.
func generateClusterHTML(wsPath, outPath string) {
	fmt.Printf("Generating cluster review page → %s\n", outPath)

	// Load pages.
	type htmlPage struct {
		Num     int    `json:"num"`
		Chapter string `json:"chapter"`
		Text    string `json:"text"`
		Top     []string `json:"top"`
	}

	type weightedP struct {
		num     int
		chapter string
		text    string
		vec     map[string]float64
		norm    float64
		top     []string
	}

	stopwords := buildStopwords()

	var rawPages []*weightedP
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
		rawPages = append(rawPages, &weightedP{
			num:     p.PhysicalPage,
			chapter: p.Chapter,
			text:    p.RawText,
			vec:     make(map[string]float64),
		})
	}
	sort.Slice(rawPages, func(i, j int) bool { return rawPages[i].num < rawPages[j].num })

	// Build TF-IDF (same as cluster.go).
	docFreq := map[string]int{}
	pageWords := map[int]map[string]int{}
	for _, p := range rawPages {
		pageWords[p.num] = map[string]int{}
		seen := map[string]bool{}
		for _, w := range tokenize(p.text) {
			if stopwords[w] || len(w) < 3 {
				continue
			}
			pageWords[p.num][w]++
			if !seen[w] {
				docFreq[w]++
				seen[w] = true
			}
		}
	}

	totalDocs := float64(len(rawPages))
	for w, df := range docFreq {
		if float64(df)/totalDocs > 0.50 || df < 2 {
			delete(docFreq, w)
		}
	}

	for _, p := range rawPages {
		for w, tf := range pageWords[p.num] {
			df, ok := docFreq[w]
			if !ok {
				continue
			}
			weight := float64(tf) * math.Log(totalDocs/float64(df))
			p.vec[w] = weight
			p.norm += weight * weight
		}
		p.norm = math.Sqrt(p.norm)

		type ww struct {
			w string
			v float64
		}
		var sorted []ww
		for w, v := range p.vec {
			sorted = append(sorted, ww{w, v})
		}
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].v > sorted[j].v })
		limit := 6
		if len(sorted) < limit {
			limit = len(sorted)
		}
		for _, s := range sorted[:limit] {
			p.top = append(p.top, s.w)
		}
	}

	// Cluster (same logic as cluster.go).
	parent := map[int]int{}
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

	for i := 0; i < len(rawPages); i++ {
		for j := i + 1; j < len(rawPages); j++ {
			sim := cosine(rawPages[i].vec, rawPages[j].vec, rawPages[i].norm, rawPages[j].norm)
			if sim >= 0.20 {
				union(rawPages[i].num, rawPages[j].num)
			}
		}
	}

	clusters := map[int][]int{}
	for _, p := range rawPages {
		root := find(p.num)
		clusters[root] = append(clusters[root], p.num)
	}

	type htmlCluster struct {
		ID      int        `json:"id"`
		Pages   []htmlPage `json:"pages"`
		Shared  []string   `json:"shared"`
		Name    string     `json:"name"`
	}

	var clusterData []htmlCluster
	clusterID := 1

	// Sort clusters by size desc.
	type cEntry struct {
		pages []int
	}
	var cEntries []cEntry
	for _, pnums := range clusters {
		if len(pnums) < 2 {
			continue
		}
		sort.Ints(pnums)
		cEntries = append(cEntries, cEntry{pnums})
	}
	sort.Slice(cEntries, func(i, j int) bool { return len(cEntries[i].pages) > len(cEntries[j].pages) })

	// Also collect singletons.
	var singletons []int
	for _, pnums := range clusters {
		if len(pnums) == 1 {
			singletons = append(singletons, pnums[0])
		}
	}
	sort.Ints(singletons)

	for _, ce := range cEntries {
		hc := htmlCluster{ID: clusterID}
		clusterID++

		// Shared words.
		wordCount := map[string]int{}
		for _, pn := range ce.pages {
			for _, p := range rawPages {
				if p.num == pn {
					for w := range p.vec {
						wordCount[w]++
					}
					break
				}
			}
		}
		thresh := len(ce.pages) / 2
		if thresh < 2 {
			thresh = 2
		}
		for w, c := range wordCount {
			if c >= thresh {
				hc.Shared = append(hc.Shared, w)
			}
		}
		sort.Strings(hc.Shared)
		if len(hc.Shared) > 10 {
			hc.Shared = hc.Shared[:10]
		}

		// Pages.
		for _, pn := range ce.pages {
			for _, p := range rawPages {
				if p.num == pn {
					snippet := cleanSnippet(p.text, 400)
					hc.Pages = append(hc.Pages, htmlPage{
						Num:     pn,
						Chapter: p.chapter,
						Text:    snippet,
						Top:     p.top,
					})
					break
				}
			}
		}
		clusterData = append(clusterData, hc)
	}

	// Build singleton cluster.
	if len(singletons) > 0 {
		hc := htmlCluster{ID: clusterID, Name: "Unclustered Pages"}
		for _, pn := range singletons {
			for _, p := range rawPages {
				if p.num == pn {
					snippet := cleanSnippet(p.text, 400)
					hc.Pages = append(hc.Pages, htmlPage{
						Num:     pn,
						Chapter: p.chapter,
						Text:    snippet,
						Top:     p.top,
					})
					break
				}
			}
		}
		clusterData = append(clusterData, hc)
	}

	// Build pairwise similarity matrix for client-side splitting.
	// Key: "pageA:pageB" (lower num first), Value: cosine similarity.
	simMatrix := map[string]float64{}
	for i := 0; i < len(rawPages); i++ {
		for j := i + 1; j < len(rawPages); j++ {
			sim := cosine(rawPages[i].vec, rawPages[j].vec, rawPages[i].norm, rawPages[j].norm)
			if sim > 0.05 { // only store non-trivial similarities to keep size down
				key := fmt.Sprintf("%d:%d", rawPages[i].num, rawPages[j].num)
				simMatrix[key] = math.Round(sim*1000) / 1000
			}
		}
	}
	simJSON, _ := json.Marshal(simMatrix)

	// Serialize cluster data for embedding in HTML.
	jsonData, _ := json.Marshal(clusterData)

	html := buildHTML(string(jsonData), string(simJSON), len(rawPages), len(clusterData))

	os.WriteFile(outPath, []byte(html), 0644)
	fmt.Printf("Done. Open %s in a browser.\n", outPath)
}

// cleanSnippet collapses PDF line breaks into flowing prose.
// Preserves paragraph breaks (double newlines) but joins single
// line breaks that are just print-layout artifacts.
func cleanSnippet(text string, maxLen int) string {
	// Normalize line endings.
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\f", "")

	// Split on double newlines to preserve paragraph breaks.
	paragraphs := strings.Split(text, "\n\n")
	var cleaned []string
	for _, para := range paragraphs {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		// Within a paragraph, collapse single newlines to spaces.
		lines := strings.Split(para, "\n")
		var joinedLines []string
		for _, l := range lines {
			l = strings.TrimSpace(l)
			if l == "" {
				continue
			}
			// Skip page number artifacts.
			if len(l) < 6 {
				trimmed := strings.Trim(l, "- \t")
				isNum := true
				for _, c := range trimmed {
					if c < '0' || c > '9' {
						isNum = false
						break
					}
				}
				if isNum && len(trimmed) > 0 {
					continue
				}
			}
			joinedLines = append(joinedLines, l)
		}
		if len(joinedLines) > 0 {
			cleaned = append(cleaned, strings.Join(joinedLines, " "))
		}
	}
	result := strings.Join(cleaned, "\n\n")
	if maxLen > 0 && len(result) > maxLen {
		result = result[:maxLen] + "..."
	}
	return result
}

func buildHTML(jsonData, simJSON string, totalPages, totalClusters int) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Index Clusters — Review</title>
<style>
* { box-sizing: border-box; margin: 0; padding: 0; }
body { font-family: Georgia, 'Times New Roman', serif; background: #f5f3ef; color: #2c2c2c; padding: 20px; max-width: 900px; margin: 0 auto; }
h1 { font-size: 1.4em; margin-bottom: 4px; }
.subtitle { color: #666; font-size: 0.9em; margin-bottom: 20px; }
.cluster { background: #fff; border: 1px solid #d4d0c8; border-radius: 6px; margin-bottom: 16px; padding: 16px; transition: all 0.2s; }
.cluster.sub { border-left: 3px solid #8b7355; margin-left: 20px; }
.cluster-header { display: flex; justify-content: space-between; align-items: flex-start; margin-bottom: 10px; }
.cluster-num { font-size: 0.8em; color: #999; text-transform: uppercase; letter-spacing: 0.05em; }
.cluster-pages { font-size: 0.85em; color: #666; }
.cluster-name { width: 100%%; margin: 8px 0; padding: 6px 10px; font-family: Georgia, serif; font-size: 1em; border: 1px solid #d4d0c8; border-radius: 4px; background: #faf9f6; }
.cluster-name:focus { border-color: #8b7355; outline: none; background: #fff; }
.cluster-name::placeholder { color: #b0a890; font-style: italic; }
.shared { font-size: 0.85em; color: #8b7355; margin-bottom: 10px; }
.shared span { background: #f0ebe0; padding: 1px 6px; border-radius: 3px; margin-right: 4px; display: inline-block; margin-bottom: 3px; }
.page { border-top: 1px solid #eee; padding: 8px 0; }
.page-header { display: flex; justify-content: space-between; align-items: center; cursor: pointer; user-select: none; }
.page-num { font-weight: bold; font-size: 0.9em; }
.page-chapter { font-size: 0.8em; color: #888; }
.page-top { font-size: 0.8em; color: #8b7355; margin-top: 2px; }
.page-text { display: none; margin-top: 8px; padding: 10px; background: #faf9f6; border-radius: 4px; font-size: 0.85em; line-height: 1.6; color: #444; max-height: 250px; overflow-y: auto; }
.page-text.open { display: block; }
.toggle { color: #999; font-size: 0.8em; }
.cluster-actions { margin-top: 10px; display: flex; gap: 6px; flex-wrap: wrap; }
.btn { padding: 5px 12px; border: 1px solid #d4d0c8; border-radius: 4px; background: #fff; cursor: pointer; font-family: Georgia, serif; font-size: 0.8em; color: #555; }
.btn:hover { background: #f0ebe0; border-color: #8b7355; }
.btn:disabled { opacity: 0.4; cursor: default; }
.btn-save { background: #8b7355; color: #fff; border-color: #8b7355; }
.btn-save:hover { background: #7a6348; }
.saved-msg { display: none; color: #5a8a5a; font-size: 0.85em; margin-left: 10px; align-self: center; }
.stats { font-size: 0.85em; color: #888; margin-bottom: 20px; }
.singleton { opacity: 0.7; }
.depth-label { font-size: 0.75em; color: #b0a890; margin-left: 8px; }
</style>
</head>
<body>
<h1>Page Clusters</h1>
<p class="subtitle">Groups of pages that share vocabulary. Name each group, or split to refine.</p>
<p class="stats" id="stats">%d pages, %d clusters</p>

<div id="clusters"></div>

<div class="cluster-actions" style="margin-top: 24px;">
  <button class="btn btn-save" onclick="exportJSON()">Export Named Clusters</button>
  <span class="saved-msg" id="exportMsg">Saved!</span>
</div>

<script>
let data = %s;
const simMatrix = %s;
let nextId = data.length ? Math.max(...data.map(c=>c.id)) + 1 : 1;

function getSim(a, b) {
  const lo = Math.min(a, b), hi = Math.max(a, b);
  return simMatrix[lo + ':' + hi] || 0;
}

function render() {
  const container = document.getElementById('clusters');
  container.innerHTML = '';
  document.getElementById('stats').textContent =
    countAllPages() + ' pages, ' + data.length + ' clusters';

  data.forEach((cl, idx) => {
    const isSingleton = cl.name === 'Unclustered Pages';
    const depth = cl.depth || 0;
    const div = document.createElement('div');
    div.className = 'cluster' + (isSingleton ? ' singleton' : '') + (depth > 0 ? ' sub' : '');

    const pageRange = formatPages(cl.pages.map(p => p.num));
    const depthLabel = depth > 0 ? '<span class="depth-label">split from cluster ' + (cl.parentId||'?') + '</span>' : '';

    div.innerHTML = ''
      + '<div class="cluster-header">'
      + '  <span class="cluster-num">Cluster ' + cl.id + depthLabel + '</span>'
      + '  <span class="cluster-pages">' + cl.pages.length + ' pages: ' + pageRange + '</span>'
      + '</div>'
      + (isSingleton
        ? '<div style="font-style:italic;color:#999;margin-bottom:8px;">' + cl.name + '</div>'
        : '<input class="cluster-name" type="text" placeholder="Name this group..." '
          + 'value="' + escapeAttr(cl.name || '') + '" '
          + 'onchange="data[' + idx + '].name = this.value">')
      + (cl.shared && cl.shared.length
        ? '<div class="shared">' + cl.shared.map(w => '<span>' + esc(w) + '</span>').join('') + '</div>'
        : '')
      + cl.pages.map((p, pi) => ''
        + '<div class="page">'
        + '  <div class="page-header" onclick="togglePage(this)">'
        + '    <div>'
        + '      <span class="page-num">p.' + p.num + '</span>'
        + (p.chapter ? ' <span class="page-chapter">' + esc(p.chapter) + '</span>' : '')
        + '      <div class="page-top">' + (p.top || []).join(', ') + '</div>'
        + '    </div>'
        + '    <span class="toggle">expand</span>'
        + '  </div>'
        + '  <div class="page-text">' + esc(p.text) + '</div>'
        + '</div>'
      ).join('')
      + (!isSingleton && cl.pages.length >= 2
        ? '<div class="cluster-actions">'
          + '<button class="btn" onclick="splitCluster(' + idx + ')">Split this group</button>'
          + '</div>'
        : '')
    ;
    container.appendChild(div);
  });
}

function splitCluster(idx) {
  const cl = data[idx];
  if (cl.pages.length < 4) {
    alert('Too few pages to split meaningfully (need at least 4).');
    return;
  }

  const pages = cl.pages;
  const n = pages.length;
  const nums = pages.map(p => p.num);

  // Find the two most dissimilar pages as seeds.
  let minSim = 1, seedA = 0, seedB = 1;
  for (let i = 0; i < n; i++) {
    for (let j = i + 1; j < n; j++) {
      const sim = getSim(nums[i], nums[j]);
      if (sim < minSim) { minSim = sim; seedA = i; seedB = j; }
    }
  }

  // Assign each page to nearest seed using real cosine similarity.
  const groupA = [], groupB = [];
  for (let i = 0; i < n; i++) {
    const simA = getSim(nums[i], nums[seedA]);
    const simB = getSim(nums[i], nums[seedB]);
    if (simA >= simB) groupA.push(pages[i]);
    else groupB.push(pages[i]);
  }

  // Enforce minimum group size of 2.
  while (groupA.length < 2 && groupB.length > 2) {
    // Move the page most similar to groupA's seed from B to A.
    let bestIdx = 0, bestSim = -1;
    for (let i = 0; i < groupB.length; i++) {
      const sim = getSim(groupB[i].num, nums[seedA]);
      if (sim > bestSim) { bestSim = sim; bestIdx = i; }
    }
    groupA.push(groupB.splice(bestIdx, 1)[0]);
  }
  while (groupB.length < 2 && groupA.length > 2) {
    let bestIdx = 0, bestSim = -1;
    for (let i = 0; i < groupA.length; i++) {
      const sim = getSim(groupA[i].num, nums[seedB]);
      if (sim > bestSim) { bestSim = sim; bestIdx = i; }
    }
    groupB.push(groupA.splice(bestIdx, 1)[0]);
  }

  if (groupA.length < 2 || groupB.length < 2) {
    alert('Pages are too similar to split further.');
    return;
  }

  // Sort by page number.
  groupA.sort((a, b) => a.num - b.num);
  groupB.sort((a, b) => a.num - b.num);

  function computeShared(group) {
    const counts = {};
    group.forEach(p => (p.top || []).forEach(w => { counts[w] = (counts[w]||0) + 1; }));
    const thresh = Math.max(2, Math.floor(group.length / 2));
    return Object.keys(counts).filter(w => counts[w] >= thresh).sort();
  }

  const parentId = cl.id;
  const depth = (cl.depth || 0) + 1;

  const newA = {
    id: nextId++, pages: groupA, shared: computeShared(groupA),
    name: '', depth: depth, parentId: parentId
  };
  const newB = {
    id: nextId++, pages: groupB, shared: computeShared(groupB),
    name: '', depth: depth, parentId: parentId
  };

  data.splice(idx, 1, newA, newB);
  render();
}

function togglePage(header) {
  const text = header.nextElementSibling;
  const toggle = header.querySelector('.toggle');
  text.classList.toggle('open');
  toggle.textContent = text.classList.contains('open') ? 'collapse' : 'expand';
}

function countAllPages() {
  const seen = new Set();
  data.forEach(c => c.pages.forEach(p => seen.add(p.num)));
  return seen.size;
}

function formatPages(nums) {
  if (!nums.length) return '';
  nums.sort((a,b) => a-b);
  const parts = [];
  let start = nums[0], end = nums[0];
  for (let i = 1; i < nums.length; i++) {
    if (nums[i] === end + 1) { end = nums[i]; }
    else {
      parts.push(start === end ? '' + start : start + '-' + end);
      start = end = nums[i];
    }
  }
  parts.push(start === end ? '' + start : start + '-' + end);
  return parts.join(', ');
}

function esc(s) { return (s||'').replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;'); }
function escapeAttr(s) { return esc(s).replace(/"/g,'&quot;'); }

function exportJSON() {
  const result = data.filter(c => c.name || c.pages.length).map(c => ({
    name: c.name || '(unnamed)',
    pages: c.pages.map(p => p.num),
    shared: c.shared,
    depth: c.depth || 0,
    parentId: c.parentId || null
  }));
  const blob = new Blob([JSON.stringify(result, null, 2)], {type: 'application/json'});
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = 'clusters.json';
  a.click();
  document.getElementById('exportMsg').style.display = 'inline';
  setTimeout(() => document.getElementById('exportMsg').style.display = 'none', 2000);
}

render();
</script>
</body>
</html>`, totalPages, totalClusters, jsonData)
}
