package quadstore

import (
	"context"
	"testing"
)

func TestReader_FindAndCount(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	s.AddBatch([]Quad{
		{Subject: "rob", Predicate: "assigned-to", Object: "matter-1"},
		{Subject: "rob", Predicate: "assigned-to", Object: "matter-2"},
		{Subject: "lisa", Predicate: "assigned-to", Object: "matter-1"},
	})

	r := s.Reader()

	n, err := r.Count(ctx, Pattern{Subject: "rob"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("expected 2, got %d", n)
	}

	var seen int
	for q, err := range r.Find(ctx, Pattern{Predicate: "assigned-to"}) {
		if err != nil {
			t.Fatal(err)
		}
		if q.Predicate != "assigned-to" {
			t.Errorf("wrong predicate: %s", q.Predicate)
		}
		seen++
	}
	if seen != 3 {
		t.Errorf("expected 3 rows, got %d", seen)
	}

	// Early termination via break.
	seen = 0
	for range r.Find(ctx, Pattern{Predicate: "assigned-to"}) {
		seen++
		break
	}
	if seen != 1 {
		t.Errorf("expected early termination at 1, got %d", seen)
	}
}

func TestReader_Find_CtxCancel(t *testing.T) {
	s := tempStore(t)
	s.AddBatch([]Quad{
		{Subject: "a", Predicate: "p", Object: "1"},
		{Subject: "b", Predicate: "p", Object: "2"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before iterating

	var gotErr error
	for _, err := range s.Reader().Find(ctx, Pattern{}) {
		if err != nil {
			gotErr = err
			break
		}
	}
	if gotErr == nil {
		t.Error("expected error from cancelled ctx")
	}
}
