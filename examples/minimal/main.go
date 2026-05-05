// Minimal quadstore example.
//
// Open a store, write three quads under a single source: label, read
// them back via the Reader's iter.Seq2 surface. Demonstrates the
// idiomatic Go shape: open, defer close, batch+commit, range-over-func
// queries.
//
// Run with:
//
//	go run ./examples/minimal
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/dukkandcards/quadstore"
)

func main() {
	dir, err := os.MkdirTemp("", "quadstore-minimal-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	store, err := quadstore.Open(filepath.Join(dir, "graph.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()

	w, err := store.Writer(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close()

	// One Commit, three quads, all under the same source: label. The
	// label namespace ("source:" prefix) is enforced by Writer.Commit;
	// remove the prefix and the call returns an error.
	err = w.Commit(ctx, quadstore.Batch{
		Label: "source:hr-feed",
		Adds: []quadstore.Quad{
			{Subject: "person:alice", Predicate: "works-at", Object: "org:acme"},
			{Subject: "person:alice", Predicate: "reports-to", Object: "person:bob"},
			{Subject: "person:bob", Predicate: "works-at", Object: "org:acme"},
		},
		Metadata: map[string]string{
			quadstore.MetaActor:  "import-2026-05-05",
			quadstore.MetaSource: "hr-feed-v3",
			quadstore.MetaReason: "initial-load",
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	// Read everything Alice. Pattern empty fields are wildcards.
	fmt.Println("Alice:")
	r := store.Reader()
	for q, err := range r.Find(ctx, quadstore.Pattern{Subject: "person:alice"}) {
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("  %s %s %s   [%s]\n", q.Subject, q.Predicate, q.Object, q.Label)
	}

	// Count how many quads landed under source:hr-feed.
	n, err := r.Count(ctx, quadstore.Pattern{Label: "source:hr-feed"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\ntotal source:hr-feed quads: %d\n", n)
}
