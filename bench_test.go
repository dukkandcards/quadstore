package quadstore

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func tempBenchStore(b *testing.B) *Store {
	b.Helper()
	dir := b.TempDir()
	s, err := Open(filepath.Join(dir, "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	return s
}

// Per-commit overhead with single-quad batches.
// Watch for commit-path latency regressions.
func BenchmarkCommit_SingleQuad(b *testing.B) {
	s := tempBenchStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w, err := s.Writer(ctx)
		if err != nil {
			b.Fatal(err)
		}
		if err := w.Commit(ctx, Batch{
			Adds: []Quad{{
				Subject:   fmt.Sprintf("s%d", i),
				Predicate: "p",
				Object:    "o",
				Label:     "source:bench",
			}},
		}); err != nil {
			b.Fatal(err)
		}
		w.Close()
	}
}

// Per-commit overhead WITHOUT audit rows — the high-throughput path
// for callers who don't need the commit_ops journal. Should approach
// raw modernc.org/sqlite single-INSERT cost.
func BenchmarkCommit_SingleQuad_NoAudit(b *testing.B) {
	s := tempBenchStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w, err := s.Writer(ctx)
		if err != nil {
			b.Fatal(err)
		}
		if err := w.Commit(ctx, Batch{
			Adds: []Quad{{
				Subject:   fmt.Sprintf("s%d", i),
				Predicate: "p",
				Object:    "o",
				Label:     "source:bench",
			}},
			NoAudit: true,
		}); err != nil {
			b.Fatal(err)
		}
		w.Close()
	}
}

// Batched-commit throughput — ingest pipelines take this path.
// Watch for per-triple-in-batch costs.
func BenchmarkCommit_Batch1k(b *testing.B) {
	s := tempBenchStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		batch := Batch{Label: "source:bench", Adds: make([]Quad, 0, 1000)}
		for j := 0; j < 1000; j++ {
			batch.Adds = append(batch.Adds, Quad{
				Subject:   fmt.Sprintf("s%d-%d", i, j),
				Predicate: "p",
				Object:    "o",
			})
		}
		w, _ := s.Writer(ctx)
		if err := w.Commit(ctx, batch); err != nil {
			b.Fatal(err)
		}
		w.Close()
	}
}

// Find on indexed subject lookup, 10k seed.
// Watch for read-path regressions.
func BenchmarkFind_BySubject(b *testing.B) {
	s := tempBenchStore(b)
	ctx := context.Background()

	// Seed — 10k quads across 100 subjects.
	w, _ := s.Writer(ctx)
	batch := Batch{Label: "source:bench", Adds: make([]Quad, 0, 10000)}
	for i := 0; i < 10000; i++ {
		batch.Adds = append(batch.Adds, Quad{
			Subject:   fmt.Sprintf("s%d", i%100),
			Predicate: fmt.Sprintf("p%d", i%10),
			Object:    fmt.Sprintf("o%d", i),
		})
	}
	if err := w.Commit(ctx, batch); err != nil {
		b.Fatal(err)
	}
	w.Close()

	r := s.Reader()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		for _, err := range r.Find(ctx, Pattern{Subject: "s50"}) {
			if err != nil {
				b.Fatal(err)
			}
			n++
		}
		if n == 0 {
			b.Fatal("no results — index broken?")
		}
	}
}
