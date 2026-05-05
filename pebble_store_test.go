package quadstore

import (
	"context"
	"path/filepath"
	"testing"
)

func tempPebbleStoreT(t *testing.T) *PebbleStore {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenPebble(filepath.Join(dir, "pebble"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPebbleStore_OpenWriteRead(t *testing.T) {
	s := tempPebbleStoreT(t)
	ctx := context.Background()

	w, err := s.Writer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := w.Commit(ctx, Batch{
		Label: "source:test",
		Adds: []Quad{
			{Subject: "person:alice", Predicate: "works-at", Object: "org:acme"},
			{Subject: "person:alice", Predicate: "reports-to", Object: "person:bob"},
			{Subject: "person:bob", Predicate: "works-at", Object: "org:acme"},
		},
		Metadata: map[string]string{
			MetaActor:  "test",
			MetaSource: "unit",
		},
	}); err != nil {
		t.Fatal(err)
	}

	r := s.Reader()

	// Find by subject — should hit SPO scan.
	var aliceCount int
	for q, err := range r.Find(ctx, Pattern{Subject: "person:alice"}) {
		if err != nil {
			t.Fatal(err)
		}
		if q.Subject != "person:alice" {
			t.Errorf("got subject=%q, want person:alice", q.Subject)
		}
		aliceCount++
	}
	if aliceCount != 2 {
		t.Errorf("alice has %d quads, want 2", aliceCount)
	}

	// Find by predicate — should hit POS scan.
	var worksAtCount int
	for q, err := range r.Find(ctx, Pattern{Predicate: "works-at"}) {
		if err != nil {
			t.Fatal(err)
		}
		if q.Predicate != "works-at" {
			t.Errorf("got predicate=%q, want works-at", q.Predicate)
		}
		worksAtCount++
	}
	if worksAtCount != 2 {
		t.Errorf("works-at has %d quads, want 2", worksAtCount)
	}

	// Find by object — should hit OSP scan.
	var acmeCount int
	for _, err := range r.Find(ctx, Pattern{Object: "org:acme"}) {
		if err != nil {
			t.Fatal(err)
		}
		acmeCount++
	}
	if acmeCount != 2 {
		t.Errorf("org:acme has %d quads, want 2", acmeCount)
	}

	// Count via Pattern.
	n, err := r.Count(ctx, Pattern{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("total count=%d, want 3", n)
	}
}

func TestPebbleStore_LabelValidation(t *testing.T) {
	s := tempPebbleStoreT(t)
	ctx := context.Background()
	w, _ := s.Writer(ctx)
	defer w.Close()

	err := w.Commit(ctx, Batch{
		Adds: []Quad{{Subject: "a", Predicate: "b", Object: "c", Label: "nope"}},
	})
	if err == nil {
		t.Fatal("expected label-validation error")
	}
}

func TestPebbleStore_NoAuditWritesQuadsButNotAuditRows(t *testing.T) {
	s := tempPebbleStoreT(t)
	ctx := context.Background()
	w, _ := s.Writer(ctx)
	defer w.Close()

	if err := w.Commit(ctx, Batch{
		Adds: []Quad{
			{Subject: "x", Predicate: "y", Object: "z", Label: "source:fast"},
		},
		NoAudit: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Quad readable.
	n, err := s.Reader().Count(ctx, Pattern{Subject: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("count=%d, want 1", n)
	}
	// We don't (yet) expose a public API to enumerate audit rows on
	// a *PebbleStore — covered in internal/pebbleq tests.
}

func TestPebbleStore_BulkLoader(t *testing.T) {
	s := tempPebbleStoreT(t)
	ctx := context.Background()

	bl, err := s.BulkLoaderWithLabel(ctx, "source:bulk")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1500; i++ {
		if err := bl.Add(Quad{
			Subject:   "s",
			Predicate: "p",
			Object:    "o" + string(rune('0'+i%10)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := bl.Close(); err != nil {
		t.Fatal(err)
	}

	stats := bl.Stats()
	if stats.Added != 1500 {
		t.Errorf("Added=%d, want 1500", stats.Added)
	}
	if stats.Flushes < 3 {
		t.Errorf("Flushes=%d, want >= 3 (1500 / 500 batches)", stats.Flushes)
	}

	// Distinct objects = 10 → 10 distinct quads (the rest dedupe via
	// upsert semantics on (s,p,o,l)). Subject prefix scan returns 10.
	n, err := s.Reader().Count(ctx, Pattern{Subject: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Errorf("after bulk-load count=%d, want 10 distinct quads", n)
	}
}

func TestPebbleStore_RemovesPath(t *testing.T) {
	s := tempPebbleStoreT(t)
	ctx := context.Background()
	w, _ := s.Writer(ctx)
	defer w.Close()

	q := Quad{Subject: "u", Predicate: "v", Object: "w", Label: "source:rm"}
	if err := w.Commit(ctx, Batch{Adds: []Quad{q}}); err != nil {
		t.Fatal(err)
	}
	n, _ := s.Reader().Count(ctx, Pattern{Subject: "u"})
	if n != 1 {
		t.Fatalf("after add: count=%d, want 1", n)
	}

	if err := w.Commit(ctx, Batch{Removes: []Quad{q}}); err != nil {
		t.Fatal(err)
	}
	n, _ = s.Reader().Count(ctx, Pattern{Subject: "u"})
	if n != 0 {
		t.Errorf("after remove: count=%d, want 0", n)
	}
}
