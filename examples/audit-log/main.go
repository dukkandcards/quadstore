// Audit-log example.
//
// HR feed sends events over time; we append each one as a small batch
// under source:hr-feed with metadata that captures who initiated the
// import. The quadstore's `commits` table preserves the metadata so
// you can answer "where did this fact come from" for any row, even
// long after the fact.
//
// Demonstrates:
//   - Multiple commits under the same label (each is its own audit row).
//   - Metadata fields (MetaActor, MetaSource, MetaReason) pinned per commit.
//   - Querying "everything we know about this subject" across time.
//   - The label namespace prefix ("source:") is enforced by Writer.Commit;
//     try to commit with label "audit" (no prefix) and you get an error.
//
// Run with:
//
//	go run ./examples/audit-log
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/dukkandcards/quadstore"
)

func main() {
	dir, err := os.MkdirTemp("", "quadstore-audit-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	store, err := quadstore.Open(filepath.Join(dir, "audit.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()

	// Three events arriving over time. Each one is its own commit so
	// the audit trail is faithful.
	events := []struct {
		when    string
		actor   string
		quad    quadstore.Quad
	}{
		{"2026-04-01T09:00:00Z", "import-2026-04-01", quadstore.Quad{Subject: "person:alice", Predicate: "joined", Object: "org:acme"}},
		{"2026-04-15T10:00:00Z", "manager-bob",       quadstore.Quad{Subject: "person:alice", Predicate: "promoted-to", Object: "title:senior-engineer"}},
		{"2026-05-01T14:30:00Z", "manager-bob",       quadstore.Quad{Subject: "person:alice", Predicate: "team-changed-to", Object: "team:platform"}},
	}

	w, err := store.Writer(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close()

	// Demonstrate the namespace enforcement first: a label without a
	// recognized prefix is rejected by Commit.
	bad := w.Commit(ctx, quadstore.Batch{
		Label: "audit", // no prefix → rejected
		Adds:  []quadstore.Quad{events[0].quad},
	})
	fmt.Printf("commit with bad label: %v\n", bad)

	// Real commits: source: prefix, one per event.
	for _, e := range events {
		err := w.Commit(ctx, quadstore.Batch{
			Label: "source:hr-feed",
			Adds:  []quadstore.Quad{e.quad},
			Metadata: map[string]string{
				quadstore.MetaActor:  e.actor,
				quadstore.MetaSource: "hr-feed-v3",
				quadstore.MetaReason: "event-as-of-" + e.when,
			},
		})
		if err != nil {
			log.Fatal(err)
		}
	}

	// Read the full history for person:alice.
	fmt.Println("\nperson:alice — what we know:")
	r := store.Reader()
	for q, err := range r.Find(ctx, quadstore.Pattern{Subject: "person:alice"}) {
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("  %s %s %s   [%s]\n", q.Subject, q.Predicate, q.Object, q.Label)
	}

	// Query just promotions, regardless of subject. The label argument
	// scopes the read so we only look at HR-feed source data.
	fmt.Println("\nall promoted-to events:")
	for q, err := range r.Find(ctx, quadstore.Pattern{
		Predicate: "promoted-to",
		Label:     "source:hr-feed",
	}) {
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("  %s → %s\n", q.Subject, q.Object)
	}

	// LabelCounts gives a fast indexed roll-up — useful for dashboards.
	counts, err := store.LabelCounts(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\nstore composition:\n")
	for label, n := range counts {
		fmt.Printf("  %-30s %d quads\n", label, n)
	}

	_ = time.Now() // keep import alive even if you trim the example
}
