package quadstore

import (
	"context"
	"path/filepath"
	"testing"
)

// TestBatch_AddsThenRemoves_DocumentedSemantics locks in the documented
// "Adds-then-Removes within a single Commit" ordering. If a quad
// appears in BOTH lists, the net effect is REMOVE — which is what the
// SQLite path does (INSERT OR IGNORE → DELETE). Callers that want
// "replace" semantics must diff first; see Batch godoc.
//
// Surfaced 2026-05-10 in secdek's force-reemit backfill — adding this
// test so the ordering can't silently change.
func TestBatch_AddsThenRemoves_DocumentedSemantics(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	w, err := s.Writer(ctx)
	if err != nil {
		t.Fatal(err)
	}

	q := Quad{Subject: "person:atkins", Predicate: "person:role", Object: "Chair", Label: "source:test"}

	// Seed.
	if err := w.Commit(ctx, Batch{Adds: []Quad{q}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	w.Close()

	// Same quad in BOTH Adds and Removes — documented behavior is REMOVE.
	w2, err := s.Writer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := w2.Commit(ctx, Batch{
		Adds:    []Quad{q},
		Removes: []Quad{q},
	}); err != nil {
		t.Fatalf("paired commit: %v", err)
	}
	w2.Close()

	// Verify the quad is GONE — confirms documented Adds-then-Removes
	// ordering. If the writer ever changed to Removes-then-Adds, this
	// test would fail and force a docs + caller update.
	r := s.Reader()
	count := 0
	for q2, err := range r.Find(ctx, Pattern{Subject: "person:atkins"}) {
		if err != nil {
			t.Fatal(err)
		}
		_ = q2
		count++
	}
	if count != 0 {
		t.Errorf("expected 0 quads after Add+Remove of same key (docs say Adds first then Removes), got %d", count)
	}
}
