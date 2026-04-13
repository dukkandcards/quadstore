package quadstore

import (
	"os"
	"path/filepath"
	"testing"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAddAndMatch(t *testing.T) {
	s := tempStore(t)

	q := Quad{Subject: "rob", Predicate: "assigned-to", Object: "matter-1"}
	if err := s.Add(q); err != nil {
		t.Fatal(err)
	}
	// Duplicate should be ignored.
	if err := s.Add(q); err != nil {
		t.Fatal(err)
	}

	quads, preds, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if quads != 1 {
		t.Errorf("expected 1 quad, got %d", quads)
	}
	if preds != 1 {
		t.Errorf("expected 1 predicate, got %d", preds)
	}

	// Match by subject.
	it := s.Match("rob", "", "", "")
	count := 0
	for it.Next() {
		count++
		if it.Quad().Object != "matter-1" {
			t.Errorf("unexpected object: %s", it.Quad().Object)
		}
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	it.Close()
	if count != 1 {
		t.Errorf("expected 1 match, got %d", count)
	}

	// Wildcard match (all quads).
	it = s.Match("", "", "", "")
	count = 0
	for it.Next() {
		count++
	}
	it.Close()
	if count != 1 {
		t.Errorf("expected 1 match from wildcard, got %d", count)
	}
}

func TestAddBatch(t *testing.T) {
	s := tempStore(t)

	quads := []Quad{
		{Subject: "rob", Predicate: "assigned-to", Object: "matter-1"},
		{Subject: "rob", Predicate: "assigned-to", Object: "matter-2"},
		{Subject: "lisa", Predicate: "assigned-to", Object: "matter-1"},
	}
	if err := s.AddBatch(quads); err != nil {
		t.Fatal(err)
	}

	n, _, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("expected 3 quads, got %d", n)
	}
}

func TestDelete(t *testing.T) {
	s := tempStore(t)

	q := Quad{Subject: "rob", Predicate: "assigned-to", Object: "matter-1"}
	s.Add(q)
	s.Delete(q)

	n, _, _ := s.Stats()
	if n != 0 {
		t.Errorf("expected 0 quads after delete, got %d", n)
	}
}

func TestPathOut(t *testing.T) {
	s := tempStore(t)
	s.AddBatch([]Quad{
		{Subject: "rob", Predicate: "assigned-to", Object: "matter-1"},
		{Subject: "rob", Predicate: "assigned-to", Object: "matter-2"},
		{Subject: "matter-1", Predicate: "has-deadline", Object: "2026-05-15"},
		{Subject: "matter-2", Predicate: "has-deadline", Object: "2026-06-01"},
	})

	// rob → assigned-to → matters → has-deadline → dates
	dates, err := s.From("rob").Out("assigned-to").Out("has-deadline").All()
	if err != nil {
		t.Fatal(err)
	}
	if len(dates) != 2 {
		t.Errorf("expected 2 dates, got %d: %v", len(dates), dates)
	}
}

func TestPathIn(t *testing.T) {
	s := tempStore(t)
	s.AddBatch([]Quad{
		{Subject: "rob", Predicate: "assigned-to", Object: "matter-1"},
		{Subject: "lisa", Predicate: "assigned-to", Object: "matter-1"},
	})

	// Who is assigned to matter-1?
	people, err := s.From("matter-1").In("assigned-to").All()
	if err != nil {
		t.Fatal(err)
	}
	if len(people) != 2 {
		t.Errorf("expected 2 people, got %d: %v", len(people), people)
	}
}

func TestPathHas(t *testing.T) {
	s := tempStore(t)
	s.AddBatch([]Quad{
		{Subject: "matter-1", Predicate: "type", Object: "patent"},
		{Subject: "matter-2", Predicate: "type", Object: "trademark"},
		{Subject: "rob", Predicate: "assigned-to", Object: "matter-1"},
		{Subject: "rob", Predicate: "assigned-to", Object: "matter-2"},
	})

	// Rob's matters that are patent type.
	patents, err := s.From("rob").Out("assigned-to").Has("type", "patent").All()
	if err != nil {
		t.Fatal(err)
	}
	if len(patents) != 1 || patents[0] != "matter-1" {
		t.Errorf("expected [matter-1], got %v", patents)
	}
}

func TestPathUnique(t *testing.T) {
	s := tempStore(t)
	s.AddBatch([]Quad{
		{Subject: "rob", Predicate: "assigned-to", Object: "matter-1"},
		{Subject: "rob", Predicate: "lead-on", Object: "matter-1"},
	})

	matters, err := s.From("rob").Out("assigned-to", "lead-on").Unique().All()
	if err != nil {
		t.Fatal(err)
	}
	if len(matters) != 1 {
		t.Errorf("expected 1 unique matter, got %d: %v", len(matters), matters)
	}
}

func TestPathCount(t *testing.T) {
	s := tempStore(t)
	s.AddBatch([]Quad{
		{Subject: "rob", Predicate: "assigned-to", Object: "matter-1"},
		{Subject: "rob", Predicate: "assigned-to", Object: "matter-2"},
	})

	n, err := s.From("rob").Out("assigned-to").Count()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("expected count 2, got %d", n)
	}
}

func TestPathFirst(t *testing.T) {
	s := tempStore(t)
	s.Add(Quad{Subject: "rob", Predicate: "assigned-to", Object: "matter-1"})

	first, err := s.From("rob").Out("assigned-to").First()
	if err != nil {
		t.Fatal(err)
	}
	if first != "matter-1" {
		t.Errorf("expected matter-1, got %s", first)
	}

	// Empty path.
	empty, err := s.From("nobody").Out("assigned-to").First()
	if err != nil {
		t.Fatal(err)
	}
	if empty != "" {
		t.Errorf("expected empty, got %s", empty)
	}
}

func TestShape(t *testing.T) {
	s := tempStore(t)
	s.AddBatch([]Quad{
		{Subject: "rob", Predicate: "assigned-to", Object: "matter-1"},
		{Subject: "matter-1", Predicate: "has-deadline", Object: "2026-05-15"},
	})

	shape, err := s.Shape()
	if err != nil {
		t.Fatal(err)
	}
	if shape.NodeCount != 3 {
		t.Errorf("expected 3 nodes, got %d", shape.NodeCount)
	}
	if len(shape.Predicates) != 2 {
		t.Errorf("expected 2 predicates, got %d", len(shape.Predicates))
	}
	if len(shape.Edges) != 2 {
		t.Errorf("expected 2 edges, got %d", len(shape.Edges))
	}
	// Predicates should be visible.
	for _, e := range shape.Edges {
		if e.Predicate == "" {
			t.Error("predicate should not be empty in shape")
		}
		// Tokens should be non-zero.
		if e.From == 0 || e.To == 0 {
			t.Error("tokens should be non-zero")
		}
	}
}

func TestLabelIsolation(t *testing.T) {
	s := tempStore(t)
	s.AddBatch([]Quad{
		{Subject: "rob", Predicate: "role", Object: "partner", Label: "firm-a"},
		{Subject: "rob", Predicate: "role", Object: "counsel", Label: "firm-b"},
	})

	// Match only firm-a.
	it := s.Match("", "", "", "firm-a")
	count := 0
	for it.Next() {
		count++
		if it.Quad().Object != "partner" {
			t.Errorf("expected partner, got %s", it.Quad().Object)
		}
	}
	it.Close()
	if count != 1 {
		t.Errorf("expected 1 match for firm-a, got %d", count)
	}
}

func TestIsolation(t *testing.T) {
	dir := t.TempDir()

	// Two separate stores — simulating two products.
	s1, err := Open(filepath.Join(dir, "lawdek.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()

	s2, err := Open(filepath.Join(dir, "igdek.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	s1.Add(Quad{Subject: "matter-1", Predicate: "type", Object: "patent"})
	s2.Add(Quad{Subject: "card-1", Predicate: "grade", Object: "PSA 10"})

	// Each store sees only its own data.
	n1, _, _ := s1.Stats()
	n2, _, _ := s2.Stats()
	if n1 != 1 || n2 != 1 {
		t.Errorf("expected 1 quad in each store, got %d and %d", n1, n2)
	}

	// No API exists to query across stores.
	it := s1.Match("card-1", "", "", "")
	if it.Next() {
		t.Error("lawdek store should not contain igdek data")
	}
	it.Close()
}

func TestInvalidQuad(t *testing.T) {
	s := tempStore(t)
	if err := s.Add(Quad{Subject: "", Predicate: "x", Object: "y"}); err == nil {
		t.Error("expected error for empty subject")
	}
	if err := s.AddBatch([]Quad{{Subject: "a", Predicate: "", Object: "y"}}); err == nil {
		t.Error("expected error for empty predicate in batch")
	}
}

func TestOpenNonexistentDir(t *testing.T) {
	_, err := Open(filepath.Join(os.TempDir(), "nonexistent-quadstore-dir-xyz", "test.db"))
	if err == nil {
		t.Error("expected error opening store in nonexistent directory")
	}
}
