package quadstore

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/dukkandcards/quadstore/internal/pebbleq"
)

// Comparative benchmarks: SQLite-backed quadstore vs Pebble-backed
// prototype, same workload. The Pebble side has known limitations
// (no audit, no partitioning, no namespace validation — see
// internal/pebbleq/store.go) so it represents the upper-bound speed
// of dropping Pebble in. A production-tuned port would be a few %
// slower for the audit + validation work.
//
// Run with:
//
//	go test -bench='Pebble|Compare_RawSQLite|Commit_SingleQuad|Commit_Batch1k|Find_BySubject' -benchtime=2s ./...

func tempPebbleStore(b *testing.B) *pebbleq.Store {
	b.Helper()
	dir := b.TempDir()
	s, err := pebbleq.Open(filepath.Join(dir, "pebble"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	return s
}

// BenchmarkPebble_SingleQuad — audited single-quad Commit on the
// Pebble backend. Mirror of BenchmarkCommit_SingleQuad on SQLite:
// commits + commit_ops audit rows are written.
func BenchmarkPebble_SingleQuad(b *testing.B) {
	s := tempPebbleStore(b)
	ctx := context.Background()
	w, err := s.Writer(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := w.Commit(ctx, pebbleq.Batch{
			Adds: []pebbleq.Quad{{
				Subject:   fmt.Sprintf("s%d", i),
				Predicate: "p",
				Object:    "o",
				Label:     "source:bench",
			}},
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPebble_SingleQuad_NoAudit — same as above but with
// Batch.NoAudit suppressing the audit rows. Mirror of
// BenchmarkCommit_SingleQuad_NoAudit on SQLite.
func BenchmarkPebble_SingleQuad_NoAudit(b *testing.B) {
	s := tempPebbleStore(b)
	ctx := context.Background()
	w, err := s.Writer(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := w.Commit(ctx, pebbleq.Batch{
			Adds: []pebbleq.Quad{{
				Subject:   fmt.Sprintf("s%d", i),
				Predicate: "p",
				Object:    "o",
				Label:     "source:bench",
			}},
			NoAudit: true,
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPebble_Batch1k — Pebble-side of BenchmarkCommit_Batch1k.
// 1000 quads per Commit, single WriteBatch.
func BenchmarkPebble_Batch1k(b *testing.B) {
	s := tempPebbleStore(b)
	ctx := context.Background()
	w, err := s.Writer(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		batch := pebbleq.Batch{Label: "source:bench", Adds: make([]pebbleq.Quad, 0, 1000)}
		for j := 0; j < 1000; j++ {
			batch.Adds = append(batch.Adds, pebbleq.Quad{
				Subject:   fmt.Sprintf("s%d-%d", i, j),
				Predicate: "p",
				Object:    "o",
			})
		}
		if err := w.Commit(ctx, batch); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPebble_FindBySubject — Pebble-side of BenchmarkFind_BySubject.
// Seeds 10K quads across 100 subjects, queries one subject's ~100 rows.
func BenchmarkPebble_FindBySubject(b *testing.B) {
	s := tempPebbleStore(b)
	ctx := context.Background()

	// Seed via BulkLoader.
	bl, err := s.BulkLoader(ctx, "source:bench")
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 10000; i++ {
		if err := bl.Add(pebbleq.Quad{
			Subject:   fmt.Sprintf("s%d", i%100),
			Predicate: fmt.Sprintf("p%d", i%10),
			Object:    fmt.Sprintf("o%d", i),
		}); err != nil {
			b.Fatal(err)
		}
	}
	if err := bl.Close(); err != nil {
		b.Fatal(err)
	}

	r := s.Reader()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n, err := r.Count(ctx, pebbleq.Pattern{Subject: "s50"})
		if err != nil {
			b.Fatal(err)
		}
		if n == 0 {
			b.Fatal("no results — index broken?")
		}
	}
}

// BenchmarkPebble_BulkLoader — Pebble-side of
// BenchmarkCompare_Quadstore_BulkLoader. Same N=1k/10k/100k curve.
func BenchmarkPebble_BulkLoader(b *testing.B) {
	ctx := context.Background()
	for _, total := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("N=%d", total), func(b *testing.B) {
			b.ResetTimer()
			for run := 0; run < b.N; run++ {
				b.StopTimer()
				s := tempPebbleStore(b)
				b.StartTimer()
				bl, err := s.BulkLoader(ctx, "source:bench")
				if err != nil {
					b.Fatal(err)
				}
				for i := 0; i < total; i++ {
					if err := bl.Add(pebbleq.Quad{
						Subject:   fmt.Sprintf("s%d-%d", run, i),
						Predicate: "p",
						Object:    "o",
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
		})
	}
}
