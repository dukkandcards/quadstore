// Multi-tenant example.
//
// Two tenants ("acme" and "globex") write private markup to a shared
// store. The label namespace `human:{tenant-id}/...` is enforced by
// Writer.Commit, and reads scope by label so a tenant only sees its
// own markup. Source data under `source:*` is shared across tenants
// (everyone sees the same HR feed).
//
// This is the property quadstore is built around: multi-tenancy is in
// the storage layer, not bolted on. No middleware, no row-level
// security, no per-tenant database. The label is the boundary.
//
// Run with:
//
//	go run ./examples/multi-tenant
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
	dir, err := os.MkdirTemp("", "quadstore-tenant-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	store, err := quadstore.Open(filepath.Join(dir, "tenants.db"))
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

	// Shared source data — everyone sees this.
	must(w.Commit(ctx, quadstore.Batch{
		Label: "source:hr-feed",
		Adds: []quadstore.Quad{
			{Subject: "person:alice", Predicate: "title", Object: "engineer"},
			{Subject: "person:bob", Predicate: "title", Object: "manager"},
			{Subject: "person:carol", Predicate: "title", Object: "director"},
		},
	}))

	// Acme's private markup — its analyst tagged a few people.
	must(w.Commit(ctx, quadstore.Batch{
		Label: "human:acme/notes",
		Adds: []quadstore.Quad{
			{Subject: "person:alice", Predicate: "tagged", Object: "high-performer"},
			{Subject: "person:bob", Predicate: "tagged", Object: "watch-list"},
		},
		Metadata: map[string]string{
			quadstore.MetaActor: "acme-analyst-1",
		},
	}))

	// Globex's private markup — different tags, same people.
	must(w.Commit(ctx, quadstore.Batch{
		Label: "human:globex/notes",
		Adds: []quadstore.Quad{
			{Subject: "person:alice", Predicate: "tagged", Object: "competitor-recruit"},
			{Subject: "person:carol", Predicate: "tagged", Object: "board-prospect"},
		},
		Metadata: map[string]string{
			quadstore.MetaActor: "globex-research-team",
		},
	}))

	// Wrong namespace — caught by Writer.Commit.
	bad := w.Commit(ctx, quadstore.Batch{
		Label: "private:acme",
		Adds:  []quadstore.Quad{{Subject: "person:alice", Predicate: "secret", Object: "x"}},
	})
	fmt.Printf("commit with bad label %q: %v\n\n", "private:acme", bad)

	// Acme's view: source data + Acme's notes only.
	r := store.Reader()
	fmt.Println("Acme's view of person:alice (source + acme notes):")
	dumpAlice(ctx, r, "source:hr-feed", "human:acme/notes")

	fmt.Println("\nGlobex's view of person:alice (source + globex notes):")
	dumpAlice(ctx, r, "source:hr-feed", "human:globex/notes")

	fmt.Println("\nNote: each tenant only sees its own human: namespace.")
	fmt.Println("Acme cannot see Globex's notes (and vice versa) — no row-level")
	fmt.Println("security, no middleware. The label is the boundary.")
}

func dumpAlice(ctx context.Context, r *quadstore.Reader, labels ...string) {
	for _, label := range labels {
		for q, err := range r.Find(ctx, quadstore.Pattern{
			Subject: "person:alice",
			Label:   label,
		}) {
			if err != nil {
				log.Fatal(err)
			}
			fmt.Printf("  %s %s %s   [%s]\n", q.Subject, q.Predicate, q.Object, q.Label)
		}
	}
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
