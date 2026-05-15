package quadstore

import (
	"context"
	"fmt"
	"testing"

	"github.com/dukkandcards/quadstore/internal/pebbleq"
)

// Scale benchmarks: 1M-quad workloads, exercising the LSM /
// B-tree shape at sizes where small-N behavior may not extrapolate.
// Run with:
//
//	go test -bench='BenchmarkScale_' -benchtime=1x -run=^$ ./...
//
// (b.N=1 forced because each iteration is expensive; we want one
// deterministic measurement, not the auto-scaled b.N loop.)
//
// Predicate cardinality is set to 100 distinct values to approximate
// the SecDek-class shape (~140 predicates across 133M quads).

const (
	scaleN          = 1_000_000
	scalePredicates = 100
	scaleSubjects   = 10_000 // 1M quads / 10k subjects = 100 quads/subject avg
)

// BenchmarkScale_BulkLoad1M_SQLite — 1M-quad load on the SQLite
// backend. Uses BulkLoader's drop-and-rebuild pattern.
func BenchmarkScale_BulkLoad1M_SQLite(b *testing.B) {
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		s := tempBenchStore(b)
		b.StartTimer()
		bl, err := s.BulkLoaderWithLabel(ctx, "source:bench")
		if err != nil {
			b.Fatal(err)
		}
		for j := 0; j < scaleN; j++ {
			if err := bl.Add(Quad{
				Subject:   fmt.Sprintf("s%d-%d", i, j%scaleSubjects),
				Predicate: fmt.Sprintf("p%d", j%scalePredicates),
				Object:    fmt.Sprintf("o%d", j),
			}); err != nil {
				b.Fatal(err)
			}
		}
		if err := bl.Close(); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		s.Close()
		b.StartTimer()
	}
}

// BenchmarkScale_BulkLoad1M_Pebble — 1M-quad load on the Pebble
// backend. Uses the prototype's WriteBatch shape.
func BenchmarkScale_BulkLoad1M_Pebble(b *testing.B) {
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		s, err := pebbleq.Open(b.TempDir() + "/pebble")
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		bl, err := s.BulkLoaderWithLabel(ctx, "source:bench")
		if err != nil {
			b.Fatal(err)
		}
		for j := 0; j < scaleN; j++ {
			if err := bl.Add(pebbleq.Quad{
				Subject:   fmt.Sprintf("s%d-%d", i, j%scaleSubjects),
				Predicate: fmt.Sprintf("p%d", j%scalePredicates),
				Object:    fmt.Sprintf("o%d", j),
			}); err != nil {
				b.Fatal(err)
			}
		}
		if err := bl.Close(); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		s.Close()
		b.StartTimer()
	}
}

// scaleSeedSQLite seeds a SQLite store with 1M quads (run once,
// outside the timer, for read-after-load benches).
func scaleSeedSQLite(b *testing.B) *Store {
	b.Helper()
	s := tempBenchStore(b)
	ctx := context.Background()
	bl, err := s.BulkLoaderWithLabel(ctx, "source:bench")
	if err != nil {
		b.Fatal(err)
	}
	for j := 0; j < scaleN; j++ {
		if err := bl.Add(Quad{
			Subject:   fmt.Sprintf("s%d", j%scaleSubjects),
			Predicate: fmt.Sprintf("p%d", j%scalePredicates),
			Object:    fmt.Sprintf("o%d", j),
		}); err != nil {
			b.Fatal(err)
		}
	}
	if err := bl.Close(); err != nil {
		b.Fatal(err)
	}
	return s
}

func scaleSeedPebble(b *testing.B) *pebbleq.Store {
	b.Helper()
	s, err := pebbleq.Open(b.TempDir() + "/pebble")
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	bl, err := s.BulkLoaderWithLabel(ctx, "source:bench")
	if err != nil {
		b.Fatal(err)
	}
	for j := 0; j < scaleN; j++ {
		if err := bl.Add(pebbleq.Quad{
			Subject:   fmt.Sprintf("s%d", j%scaleSubjects),
			Predicate: fmt.Sprintf("p%d", j%scalePredicates),
			Object:    fmt.Sprintf("o%d", j),
		}); err != nil {
			b.Fatal(err)
		}
	}
	if err := bl.Close(); err != nil {
		b.Fatal(err)
	}
	return s
}

// BenchmarkScale_FindBySubject_After1M_SQLite — after a 1M seed,
// time how long Find by subject takes. Returns ~100 rows (1M/10K
// subjects).
func BenchmarkScale_FindBySubject_After1M_SQLite(b *testing.B) {
	s := scaleSeedSQLite(b)
	defer s.Close()
	ctx := context.Background()
	r := s.Reader()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n, err := r.Count(ctx, Pattern{Subject: fmt.Sprintf("s%d", i%scaleSubjects)})
		if err != nil {
			b.Fatal(err)
		}
		if n == 0 {
			b.Fatal("no results — seed broken?")
		}
	}
}

// BenchmarkScale_FindBySubject_After1M_Pebble — same shape, Pebble
// backend. The Bloom-filter / sstable advantage shows up here.
func BenchmarkScale_FindBySubject_After1M_Pebble(b *testing.B) {
	s := scaleSeedPebble(b)
	defer s.Close()
	ctx := context.Background()
	r := s.Reader()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n, err := r.Count(ctx, pebbleq.Pattern{Subject: fmt.Sprintf("s%d", i%scaleSubjects)})
		if err != nil {
			b.Fatal(err)
		}
		if n == 0 {
			b.Fatal("no results — seed broken?")
		}
	}
}

// BenchmarkScale_FindByPredicate_After1M_SQLite — predicate scan
// (~10k rows, since 1M quads / 100 predicates = 10k each).
func BenchmarkScale_FindByPredicate_After1M_SQLite(b *testing.B) {
	s := scaleSeedSQLite(b)
	defer s.Close()
	ctx := context.Background()
	r := s.Reader()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n, err := r.Count(ctx, Pattern{Predicate: fmt.Sprintf("p%d", i%scalePredicates)})
		if err != nil {
			b.Fatal(err)
		}
		if n == 0 {
			b.Fatal("no results")
		}
	}
}

// BenchmarkScale_FindByPredicate_After1M_Pebble — same on Pebble.
func BenchmarkScale_FindByPredicate_After1M_Pebble(b *testing.B) {
	s := scaleSeedPebble(b)
	defer s.Close()
	ctx := context.Background()
	r := s.Reader()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n, err := r.Count(ctx, pebbleq.Pattern{Predicate: fmt.Sprintf("p%d", i%scalePredicates)})
		if err != nil {
			b.Fatal(err)
		}
		if n == 0 {
			b.Fatal("no results")
		}
	}
}
