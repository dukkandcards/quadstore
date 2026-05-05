package quadstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"
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

func TestPebbleStore_LabelCountsAndStats(t *testing.T) {
	s := tempPebbleStoreT(t)
	ctx := context.Background()
	w, _ := s.Writer(ctx)
	defer w.Close()

	if err := w.Commit(ctx, Batch{
		Adds: []Quad{
			{Subject: "a", Predicate: "p1", Object: "o", Label: "source:a"},
			{Subject: "b", Predicate: "p2", Object: "o", Label: "source:a"},
			{Subject: "c", Predicate: "p1", Object: "o", Label: "source:b"},
			{Subject: "d", Predicate: "p3", Object: "o", Label: "derived:x"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	counts, err := s.LabelCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts["source:a"] != 2 {
		t.Errorf("source:a count=%d, want 2", counts["source:a"])
	}
	if counts["source:b"] != 1 {
		t.Errorf("source:b count=%d, want 1", counts["source:b"])
	}
	if counts["derived:x"] != 1 {
		t.Errorf("derived:x count=%d, want 1", counts["derived:x"])
	}

	quads, predicates, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if quads != 4 {
		t.Errorf("Stats quads=%d, want 4", quads)
	}
	if predicates != 3 {
		t.Errorf("Stats predicates=%d, want 3 (p1, p2, p3)", predicates)
	}
}

func TestPebbleStore_CommitStatsAt(t *testing.T) {
	s := tempPebbleStoreT(t)
	ctx := context.Background()
	w, _ := s.Writer(ctx)
	defer w.Close()

	// Two commits, each with two ops.
	if err := w.Commit(ctx, Batch{
		Adds: []Quad{
			{Subject: "a", Predicate: "p", Object: "1", Label: "source:t"},
			{Subject: "a", Predicate: "p", Object: "2", Label: "source:t"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(ctx, Batch{
		Adds: []Quad{
			{Subject: "b", Predicate: "p", Object: "1", Label: "source:t"},
			{Subject: "b", Predicate: "p", Object: "2", Label: "source:t"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	cs, err := s.CommitStatsAt(time.Time{}) // zero cutoff: no Old fields
	if err != nil {
		t.Fatal(err)
	}
	if cs.TotalCommits != 2 {
		t.Errorf("TotalCommits=%d, want 2", cs.TotalCommits)
	}
	if cs.TotalOps != 4 {
		t.Errorf("TotalOps=%d, want 4", cs.TotalOps)
	}
	if cs.OldCommits != 0 || cs.OldOps != 0 {
		t.Errorf("zero cutoff but Old fields nonzero: %+v", cs)
	}

	// Future cutoff: every commit is "old."
	future := time.Now().Add(1 * time.Hour)
	cs, err = s.CommitStatsAt(future)
	if err != nil {
		t.Fatal(err)
	}
	if cs.OldCommits != 2 {
		t.Errorf("OldCommits=%d, want 2", cs.OldCommits)
	}
	if cs.OldOps != 4 {
		t.Errorf("OldOps=%d, want 4", cs.OldOps)
	}

	// NoAudit commits should NOT show up in commit stats.
	if err := w.Commit(ctx, Batch{
		Adds:    []Quad{{Subject: "c", Predicate: "p", Object: "1", Label: "source:t"}},
		NoAudit: true,
	}); err != nil {
		t.Fatal(err)
	}
	cs, err = s.CommitStatsAt(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if cs.TotalCommits != 2 {
		t.Errorf("after NoAudit Commit: TotalCommits=%d, still want 2", cs.TotalCommits)
	}
}

func TestMigrateToPebble(t *testing.T) {
	src := tempStore(t)
	dst := tempPebbleStoreT(t)
	ctx := context.Background()

	w, _ := src.Writer(ctx)
	if err := w.Commit(ctx, Batch{
		Adds: []Quad{
			{Subject: "alice", Predicate: "knows", Object: "bob", Label: "source:test"},
			{Subject: "bob", Predicate: "knows", Object: "carol", Label: "source:test"},
			{Subject: "alice", Predicate: "age", Object: "30", Label: "derived:demo"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	w.Close()

	stats, err := MigrateToPebble(ctx, src, dst, MigrateToPebbleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.QuadsCopied != 3 {
		t.Errorf("QuadsCopied=%d, want 3", stats.QuadsCopied)
	}

	// Confirm contents on the destination.
	n, err := dst.Reader().Count(ctx, Pattern{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("dst quad count=%d, want 3", n)
	}

	counts, err := dst.LabelCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts["source:test"] != 2 || counts["derived:demo"] != 1 {
		t.Errorf("dst LabelCounts wrong: %+v", counts)
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
