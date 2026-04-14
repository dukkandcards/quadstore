package quadstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWriterCommit_AddsAndMetadata(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	w, err := s.Writer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	err = w.Commit(ctx, Batch{
		Adds: []Quad{
			{Subject: "event:abc", Predicate: "scheduled-at", Object: "2026-05-01"},
		},
		Label:    "source:msgraph",
		Metadata: map[string]string{MetaActor: "delta-sync", MetaSource: "msgraph"},
	})
	if err != nil {
		t.Fatal(err)
	}

	n, err := s.Reader().Count(ctx, Pattern{Subject: "event:abc"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected 1 quad, got %d", n)
	}

	var commits int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM commits`).Scan(&commits); err != nil {
		t.Fatal(err)
	}
	if commits != 1 {
		t.Errorf("expected 1 commit row, got %d", commits)
	}

	var ops int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM commit_ops WHERE op='add'`).Scan(&ops); err != nil {
		t.Fatal(err)
	}
	if ops != 1 {
		t.Errorf("expected 1 add op, got %d", ops)
	}

	var meta string
	if err := s.db.QueryRow(`SELECT metadata FROM commits`).Scan(&meta); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(meta, `"actor":"delta-sync"`) {
		t.Errorf("metadata missing actor: %s", meta)
	}
}

func TestWriterCommit_Removes(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	w, err := s.Writer(ctx)
	if err != nil {
		t.Fatal(err)
	}

	q := Quad{Subject: "x", Predicate: "y", Object: "z", Label: "source:test"}
	if err := w.Commit(ctx, Batch{Adds: []Quad{q}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(ctx, Batch{Removes: []Quad{q}}); err != nil {
		t.Fatal(err)
	}
	w.Close()

	n, _ := s.Reader().Count(ctx, Pattern{})
	if n != 0 {
		t.Errorf("expected 0 quads after remove, got %d", n)
	}

	var adds, removes int
	s.db.QueryRow(`SELECT COUNT(*) FROM commit_ops WHERE op='add'`).Scan(&adds)
	s.db.QueryRow(`SELECT COUNT(*) FROM commit_ops WHERE op='remove'`).Scan(&removes)
	if adds != 1 || removes != 1 {
		t.Errorf("expected 1 add + 1 remove in commit_ops, got %d + %d", adds, removes)
	}
}

func TestWriterCommit_LabelValidation(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	w, err := s.Writer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Bogus label rejected.
	err = w.Commit(ctx, Batch{
		Adds: []Quad{{Subject: "a", Predicate: "b", Object: "c", Label: "bogus"}},
	})
	if err == nil {
		t.Error("expected error for bogus label")
	}

	// Each valid prefix accepted.
	for _, label := range []string{"source:x", "derived:y", "human:z", "meta:w", ""} {
		err := w.Commit(ctx, Batch{
			Adds: []Quad{{Subject: "a-" + label, Predicate: "b", Object: "c", Label: label}},
		})
		if err != nil {
			t.Errorf("expected no error for label %q, got %v", label, err)
		}
	}

	// Batch.Label applied to empty Quad.Label.
	err = w.Commit(ctx, Batch{
		Adds:  []Quad{{Subject: "batch-default", Predicate: "p", Object: "o"}},
		Label: "source:batch",
	})
	if err != nil {
		t.Fatal(err)
	}
	var label string
	s.db.QueryRow(`SELECT label FROM quads WHERE subject='batch-default'`).Scan(&label)
	if label != "source:batch" {
		t.Errorf("expected label source:batch, got %q", label)
	}

	// Batch.Label itself invalid.
	err = w.Commit(ctx, Batch{
		Adds:  []Quad{{Subject: "x", Predicate: "p", Object: "o"}},
		Label: "nope",
	})
	if err == nil {
		t.Error("expected error for invalid Batch.Label")
	}
}

func TestWriter_SingleActive_CtxCancel(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	w1, err := s.Writer(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Second Writer with a short deadline — should time out.
	ctx2, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err = s.Writer(ctx2)
	if err == nil {
		t.Error("expected timeout error while slot held")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}

	// Release first writer — second acquire should now succeed.
	w1.Close()
	w2, err := s.Writer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w2.Close()
}

func TestWriter_CommitAfterClose(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	w, _ := s.Writer(ctx)
	w.Close()

	err := w.Commit(ctx, Batch{Adds: []Quad{{Subject: "a", Predicate: "b", Object: "c"}}})
	if err == nil {
		t.Error("expected error committing on closed writer")
	}
}

func TestWriter_RetryAfterError(t *testing.T) {
	// A validation error should leave the Writer usable for retry.
	s := tempStore(t)
	ctx := context.Background()
	w, _ := s.Writer(ctx)
	defer w.Close()

	// First attempt: bad label → error.
	err := w.Commit(ctx, Batch{
		Adds: []Quad{{Subject: "a", Predicate: "b", Object: "c", Label: "nope"}},
	})
	if err == nil {
		t.Fatal("expected validation error")
	}

	// Retry with valid label on the same writer — should succeed.
	err = w.Commit(ctx, Batch{
		Adds: []Quad{{Subject: "a", Predicate: "b", Object: "c", Label: "source:ok"}},
	})
	if err != nil {
		t.Errorf("retry failed: %v", err)
	}
}
