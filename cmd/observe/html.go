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
					snippet := strings.TrimSpace(p.text)
					if len(snippet) > 300 {
						snippet = snippet[:300] + "..."
					}
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
					snippet := strings.TrimSpace(p.text)
					if len(snippet) > 300 {
						snippet = snippet[:300] + "..."
					}
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

	// Serialize cluster data for embedding in HTML.
	jsonData, _ := json.Marshal(clusterData)

	html := buildHTML(string(jsonData), len(rawPages), len(clusterData))

	os.WriteFile(outPath, []byte(html), 0644)
	fmt.Printf("Done. Open %s in a browser.\n", outPath)
}

func buildHTML(jsonData string, totalPages, totalClusters int) string {
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
.cluster { background: #fff; border: 1px solid #d4d0c8; border-radius: 6px; margin-bottom: 16px; padding: 16px; }
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
.page-text { display: none; margin-top: 8px; padding: 10px; background: #faf9f6; border-radius: 4px; font-size: 0.85em; line-height: 1.5; white-space: pre-wrap; color: #444; max-height: 200px; overflow-y: auto; }
.page-text.open { display: block; }
.toggle { color: #999; font-size: 0.8em; }
.actions { margin-top: 16px; display: flex; gap: 8px; }
.btn { padding: 6px 14px; border: 1px solid #d4d0c8; border-radius: 4px; background: #fff; cursor: pointer; font-family: Georgia, serif; font-size: 0.85em; color: #555; }
.btn:hover { background: #f0ebe0; border-color: #8b7355; }
.btn-save { background: #8b7355; color: #fff; border-color: #8b7355; }
.btn-save:hover { background: #7a6348; }
.saved-msg { display: none; color: #5a8a5a; font-size: 0.85em; margin-left: 10px; align-self: center; }
.stats { font-size: 0.85em; color: #888; margin-bottom: 20px; }
.singleton { opacity: 0.7; }
</style>
</head>
<body>
<h1>Page Clusters</h1>
<p class="subtitle">Groups of pages that share vocabulary. Name each group.</p>
<p class="stats">%d pages, %d clusters</p>

<div id="clusters"></div>

<div class="actions" style="margin-top: 24px;">
  <button class="btn btn-save" onclick="exportJSON()">Export Named Clusters</button>
  <span class="saved-msg" id="exportMsg">Saved!</span>
</div>

<script>
const data = %s;

function render() {
  const container = document.getElementById('clusters');
  container.innerHTML = '';

  data.forEach((cl, idx) => {
    const isSingleton = cl.name === 'Unclustered Pages';
    const div = document.createElement('div');
    div.className = 'cluster' + (isSingleton ? ' singleton' : '');

    const pageRange = formatPages(cl.pages.map(p => p.num));

    div.innerHTML = ''
      + '<div class="cluster-header">'
      + '  <span class="cluster-num">Cluster ' + cl.id + '</span>'
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
    ;
    container.appendChild(div);
  });
}

function togglePage(header) {
  const text = header.nextElementSibling;
  const toggle = header.querySelector('.toggle');
  text.classList.toggle('open');
  toggle.textContent = text.classList.contains('open') ? 'collapse' : 'expand';
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
  const named = data.filter(c => c.name).map(c => ({
    name: c.name,
    pages: c.pages.map(p => p.num),
    shared: c.shared
  }));
  const blob = new Blob([JSON.stringify(named, null, 2)], {type: 'application/json'});
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
