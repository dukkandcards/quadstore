package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dukkandcards/quadstore"
)

// workspaceEntry mirrors mega-index internal/index/model.go IndexEntry.
type workspaceEntry struct {
	Term       string         `json:"term"`
	SortKey    string         `json:"sort_key"`
	PageRefs   []wsPageRef    `json:"page_refs"`
	SubEntries []wsSubEntry   `json:"sub_entries,omitempty"`
	CrossRefs  []wsCrossRef   `json:"cross_refs,omitempty"`
	Notes      []string       `json:"notes,omitempty"`
	Source     string         `json:"source"`
	Confidence float64        `json:"confidence"`
	Approved   bool           `json:"approved"`
	Rejected   bool           `json:"rejected,omitempty"`
}

type wsSubEntry struct {
	Term      string       `json:"term"`
	PageRefs  []wsPageRef  `json:"page_refs"`
	CrossRefs []wsCrossRef `json:"cross_refs,omitempty"`
	Notes     []string     `json:"notes,omitempty"`
}

type wsPageRef struct {
	Page     int    `json:"page"`
	EndPage  int    `json:"end_page,omitempty"`
	IsFigure bool   `json:"is_figure,omitempty"`
	Label    string `json:"label,omitempty"`
}

type wsCrossRef struct {
	Type   string `json:"type"`
	Target string `json:"target"`
}

func loadWorkspace(dir string) ([]workspaceEntry, error) {
	entriesDir := filepath.Join(dir, "entries")
	files, err := filepath.Glob(filepath.Join(entriesDir, "*.json"))
	if err != nil {
		return nil, err
	}
	var all []workspaceEntry
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", f, err)
		}
		var entries []workspaceEntry
		if err := json.Unmarshal(data, &entries); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", f, err)
		}
		all = append(all, entries...)
	}
	return all, nil
}

func workspaceToQuads(entries []workspaceEntry, label string) []quadstore.Quad {
	var quads []quadstore.Quad
	add := func(s, p, o string) {
		quads = append(quads, quadstore.Quad{Subject: s, Predicate: p, Object: o, Label: label})
	}

	book := "book:woodpeckers"
	add(book, "title", "The Woodpeckers")

	for _, e := range entries {
		id := termToID(e.Term)
		add(id, "term", e.Term)
		add(book, "has-entry", id)
		add(id, "type", "main-entry")
		add(id, "source", e.Source)
		add(id, "confidence", fmt.Sprintf("%.3f", e.Confidence))

		if e.Approved {
			add(id, "status", "approved")
		} else if e.Rejected {
			add(id, "status", "rejected")
		} else {
			add(id, "status", "pending")
		}

		for _, pr := range e.PageRefs {
			addPageRefQuads(&quads, id, pr, label)
		}

		for _, sub := range e.SubEntries {
			subID := termToID(e.Term + "/" + sub.Term)
			add(subID, "term", sub.Term)
			add(subID, "type", "sub-entry")
			add(id, "has-sub-entry", subID)

			for _, pr := range sub.PageRefs {
				addPageRefQuads(&quads, subID, pr, label)
			}
			for _, xr := range sub.CrossRefs {
				targetID := termToID(xr.Target)
				add(subID, strings.ReplaceAll(xr.Type, "_", "-"), targetID)
			}
		}

		for _, xr := range e.CrossRefs {
			targetID := termToID(xr.Target)
			add(id, strings.ReplaceAll(xr.Type, "_", "-"), targetID)
		}
	}

	return quads
}

func addPageRefQuads(quads *[]quadstore.Quad, id string, pr wsPageRef, label string) {
	if pr.EndPage > 0 && pr.EndPage > pr.Page {
		for p := pr.Page; p <= pr.EndPage; p++ {
			*quads = append(*quads, quadstore.Quad{
				Subject: id, Predicate: "has-page",
				Object: "page:" + strconv.Itoa(p), Label: label,
			})
		}
	} else {
		*quads = append(*quads, quadstore.Quad{
			Subject: id, Predicate: "has-page",
			Object: "page:" + strconv.Itoa(pr.Page), Label: label,
		})
	}
}
