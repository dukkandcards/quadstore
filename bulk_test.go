package quadstore

import (
	"context"
	"path/filepath"
	"testing"
)

func TestBulkLoader_BasicInsertAndDedupe(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "bulk.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	bl, err := s.BulkLoaderWithLabel(ctx, "source:test")
	if err != nil {
		t.Fatal(err)
	}

	// Insert 1200 quads with 200 duplicates.
	for i := 0; i < 1000; i++ {
		if err := bl.Add(Quad{Subject: "s", Predicate: "p", Object: fmtInt(i)}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 200; i++ {
		if err := bl.Add(Quad{Subject: "s", Predicate: "p", Object: fmtInt(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := bl.Close(); err != nil {
		t.Fatal(err)
	}

	got := bl.Stats()
	if got.Attempted != 1200 {
		t.Errorf("Attempted: got %d, want 1200", got.Attempted)
	}
	if got.Added != 1000 {
		t.Errorf("Added: got %d, want 1000 (200 should dedupe)", got.Added)
	}

	// Sanity: reads work after Close.
	n, _, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1000 {
		t.Errorf("store size: got %d, want 1000", n)
	}

	// Sanity: indexes were recreated — a predicate-filtered query works.
	var count int64
	if err := s.parts[0].db.QueryRow(`SELECT COUNT(*) FROM quads WHERE predicate = 'p'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1000 {
		t.Errorf("after-close index query: got %d, want 1000", count)
	}
}

func TestBulkLoader_ClosedRejects(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "bulk2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	bl, err := s.BulkLoader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := bl.Close(); err != nil {
		t.Fatal(err)
	}
	if err := bl.Add(Quad{Subject: "x", Predicate: "y", Object: "z"}); err == nil {
		t.Fatal("Add after Close must error")
	}
	if err := bl.Flush(); err == nil {
		t.Fatal("Flush after Close must error")
	}
}

func TestBulkLoader_DefaultLabelApplied(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "bulk3.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	bl, err := s.BulkLoaderWithLabel(ctx, "source:bulktest")
	if err != nil {
		t.Fatal(err)
	}
	if err := bl.Add(Quad{Subject: "s", Predicate: "p", Object: "o"}); err != nil {
		t.Fatal(err)
	}
	if err := bl.Close(); err != nil {
		t.Fatal(err)
	}

	var label string
	if err := s.parts[0].db.QueryRow(`SELECT label FROM quads WHERE subject='s'`).Scan(&label); err != nil {
		t.Fatal(err)
	}
	if label != "source:bulktest" {
		t.Errorf("label: got %q, want %q", label, "source:bulktest")
	}
}

func TestBulkLoader_InvalidLabelRejected(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "bulk4.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	_, err = s.BulkLoaderWithLabel(ctx, "not-a-valid-prefix")
	if err == nil {
		t.Fatal("BulkLoaderWithLabel accepted invalid label prefix")
	}
}

func fmtInt(i int) string {
	s := ""
	if i == 0 {
		return "0"
	}
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}
