package pebbleq

import (
	"context"
	"path/filepath"
	"testing"
)

// TestLabelCounter_CommitAdds verifies the per-label counter increments
// match the number of Adds under each label across a series of Commits.
func TestLabelCounter_CommitAdds(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	w := newWriter(t, s)
	defer w.Close()

	ctx := context.Background()

	// Commit 30 quads under source:a, 12 under source:b, 5 under source:c.
	mustCommit(t, w, ctx, Batch{
		Label: "source:a",
		Adds:  makeQuads("source:a", 30),
	})
	mustCommit(t, w, ctx, Batch{
		Label: "source:b",
		Adds:  makeQuads("source:b", 12),
	})
	mustCommit(t, w, ctx, Batch{
		Label: "source:c",
		Adds:  makeQuads("source:c", 5),
	})

	r := s.Reader()
	cases := []struct {
		label string
		want  int64
	}{
		{"source:a", 30},
		{"source:b", 12},
		{"source:c", 5},
		{"source:nonexistent", 0},
	}
	for _, c := range cases {
		got, err := r.Count(ctx, Pattern{Label: c.label})
		if err != nil {
			t.Errorf("Count(%q): %v", c.label, err)
			continue
		}
		if got != c.want {
			t.Errorf("Count(%q): got %d, want %d", c.label, got, c.want)
		}
	}
}

// TestLabelCounter_AddsAndRemoves verifies removes decrement.
func TestLabelCounter_AddsAndRemoves(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	w := newWriter(t, s)
	defer w.Close()
	ctx := context.Background()

	// 10 adds, then remove 3.
	adds := makeQuads("source:x", 10)
	mustCommit(t, w, ctx, Batch{Label: "source:x", Adds: adds})

	rem := adds[:3]
	mustCommit(t, w, ctx, Batch{Label: "source:x", Removes: rem})

	got, err := s.Reader().Count(ctx, Pattern{Label: "source:x"})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 7 {
		t.Errorf("after 10 adds + 3 removes: got %d, want 7", got)
	}
}

// TestLabelCounter_BulkLoader verifies BulkLoader updates the counter.
func TestLabelCounter_BulkLoader(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	bl, err := s.BulkLoader(ctx, "source:bulk")
	if err != nil {
		t.Fatalf("bulkloader: %v", err)
	}
	for _, q := range makeQuads("source:bulk", 1500) {
		if err := bl.Add(q); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	if err := bl.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, err := s.Reader().Count(ctx, Pattern{Label: "source:bulk"})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 1500 {
		t.Errorf("bulk: got %d, want 1500", got)
	}
}

// TestLabelCounter_FastPathMatchesSlowPath confirms the O(1) Count
// fast path returns the same value as iterating the LSP keyspace.
func TestLabelCounter_FastPathMatchesSlowPath(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	w := newWriter(t, s)
	defer w.Close()
	ctx := context.Background()

	mustCommit(t, w, ctx, Batch{Label: "source:fp", Adds: makeQuads("source:fp", 47)})

	// Fast path: Pattern{Label: ...} only.
	fast, err := s.Reader().Count(ctx, Pattern{Label: "source:fp"})
	if err != nil {
		t.Fatalf("fast count: %v", err)
	}

	// Slow path: Pattern{Label, Predicate} forces iter-and-count.
	slow, err := s.Reader().Count(ctx, Pattern{Label: "source:fp", Predicate: "p"})
	if err != nil {
		t.Fatalf("slow count: %v", err)
	}

	if fast != slow {
		t.Errorf("fast=%d slow=%d (should match — every quad has predicate=p)", fast, slow)
	}
	if fast != 47 {
		t.Errorf("fast=%d, want 47", fast)
	}
}

// TestLabelCounter_RebuildResetsCounters confirms a freshly built counter
// matches LSP truth even when prior state was missing or wrong.
func TestLabelCounter_RebuildResetsCounters(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	w := newWriter(t, s)
	ctx := context.Background()

	mustCommit(t, w, ctx, Batch{Label: "source:r", Adds: makeQuads("source:r", 25)})
	w.Close()

	// Force a 'wrong' counter manually via direct Set bypassing Merge.
	if err := s.db.Set(encodeLabelCountKey("source:r"), encodeLabelDelta(999), nil); err != nil {
		t.Fatalf("force-set: %v", err)
	}
	if got, _ := s.Reader().Count(ctx, Pattern{Label: "source:r"}); got != 999 {
		t.Fatalf("force-set didn't take: got %d", got)
	}

	if err := s.RebuildLabelCounters(); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if got, _ := s.Reader().Count(ctx, Pattern{Label: "source:r"}); got != 25 {
		t.Errorf("after rebuild: got %d, want 25", got)
	}
}

// TestLabelCounter_PersistsAcrossOpen verifies the counter survives close+reopen.
func TestLabelCounter_PersistsAcrossOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	w := newWriter(t, s)
	ctx := context.Background()
	mustCommit(t, w, ctx, Batch{Label: "source:persist", Adds: makeQuads("source:persist", 9)})
	w.Close()
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	got, err := s2.Reader().Count(ctx, Pattern{Label: "source:persist"})
	if err != nil {
		t.Fatalf("reopen count: %v", err)
	}
	if got != 9 {
		t.Errorf("after reopen: got %d, want 9", got)
	}
}

// ---- helpers ----

func newWriter(t *testing.T, s *Store) *Writer {
	t.Helper()
	w, err := s.Writer(context.Background())
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	return w
}

func mustCommit(t *testing.T, w *Writer, ctx context.Context, b Batch) {
	t.Helper()
	if err := w.Commit(ctx, b); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func makeQuads(label string, n int) []Quad {
	out := make([]Quad, n)
	for i := range out {
		out[i] = Quad{
			Subject:   subjectN(i),
			Predicate: "p",
			Object:    objectN(i),
			Label:     label,
		}
	}
	return out
}

func subjectN(i int) string { return "s" + itoa(i) }
func objectN(i int) string  { return "o" + itoa(i) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
