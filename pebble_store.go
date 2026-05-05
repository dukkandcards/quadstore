package quadstore

import (
	"context"
	"iter"
	"time"

	"github.com/dukkandcards/quadstore/internal/pebbleq"
)

// PebbleStore is a Pebble-backed Store. Provides the same write /
// read / bulk-load surface as *Store (the SQLite-backed default)
// but wraps a Pebble LSM engine instead.
//
// Pebble wins decisively on point operations (single-quad commit,
// subject lookups) and large bulk loads at the cost of much heavier
// dependencies (~20 transitive packages including Sentry and
// Prometheus client). For most workloads the perf wins justify the
// cost; for minimal-binary deployments the SQLite-backed Open is
// still the default.
//
// See docs/PEBBLE_VS_SQLITE.md for measured deltas.
//
// What's NOT in *PebbleStore (yet):
//   - Partitioning. Multi-directory routing is a v0.3+ concern;
//     today a Pebble-backed Store is one Pebble dir.
//   - Match / Path / Stats / CommitStats. Higher-level helpers
//     remain SQLite-only until ported.
//   - Migrate compatibility (separate concern).
type PebbleStore struct {
	inner *pebbleq.Store
}

// OpenPebble opens or creates a Pebble-backed Store at path. path is
// a directory; Pebble manages the WAL, sstables, and metadata files
// inside it.
//
// Defaults are out-of-the-box Pebble options other than a quiet
// logger. Pebble's pebble.NoSync is used for Writer.Commit so
// durability matches SQLite's default `synchronous=NORMAL` (lazy
// fsync). Callers needing strict per-commit durability use
// (*PebbleWriter).CommitSync.
func OpenPebble(path string) (*PebbleStore, error) {
	s, err := pebbleq.Open(path)
	if err != nil {
		return nil, err
	}
	return &PebbleStore{inner: s}, nil
}

// Close releases the Pebble handle. Idempotent.
func (s *PebbleStore) Close() error { return s.inner.Close() }

// Writer returns a *PebbleWriter for this store.
func (s *PebbleStore) Writer(ctx context.Context) (*PebbleWriter, error) {
	w, err := s.inner.Writer(ctx)
	if err != nil {
		return nil, err
	}
	return &PebbleWriter{inner: w}, nil
}

// Reader returns a *PebbleReader for this store.
func (s *PebbleStore) Reader() *PebbleReader {
	return &PebbleReader{inner: s.inner.Reader()}
}

// BulkLoader opens a *PebbleBulkLoader against the store. Wraps
// pebbleq.Store.BulkLoader.
func (s *PebbleStore) BulkLoader(ctx context.Context) (*PebbleBulkLoader, error) {
	return s.BulkLoaderWithLabel(ctx, "")
}

// BulkLoaderWithLabel opens a *PebbleBulkLoader with a default label.
func (s *PebbleStore) BulkLoaderWithLabel(ctx context.Context, defaultLabel string) (*PebbleBulkLoader, error) {
	bl, err := s.inner.BulkLoaderWithLabel(ctx, defaultLabel)
	if err != nil {
		return nil, err
	}
	return &PebbleBulkLoader{inner: bl}, nil
}

// PebbleWriter is the Pebble-backed Writer. Same surface as *Writer
// (Commit, Close); under the hood writes go through pebbleq.
type PebbleWriter struct {
	inner *pebbleq.Writer
}

// Close releases the writer. Idempotent.
func (w *PebbleWriter) Close() error { return w.inner.Close() }

// Commit applies a Batch atomically. Lazy-fsync durability — see
// CommitSync for strict per-commit fsync.
func (w *PebbleWriter) Commit(ctx context.Context, b Batch) error {
	return w.inner.Commit(ctx, toPebbleBatch(b))
}

// CommitSync is the per-Commit-fsync variant. ~1000× slower than
// Commit on M1 Pro. Use when you must not lose a committed write
// across a crash.
func (w *PebbleWriter) CommitSync(ctx context.Context, b Batch) error {
	return w.inner.CommitSync(ctx, toPebbleBatch(b))
}

// PebbleReader is the Pebble-backed Reader.
type PebbleReader struct {
	inner *pebbleq.Reader
}

// Find returns iter.Seq2[Quad, error] of every quad matching the
// pattern. Routing rules — see internal/pebbleq.(*Reader).Find godoc
// for the full table.
func (r *PebbleReader) Find(ctx context.Context, p Pattern) iter.Seq2[Quad, error] {
	pp := pebbleq.Pattern{
		Subject:   p.Subject,
		Predicate: p.Predicate,
		Object:    p.Object,
		Label:     p.Label,
	}
	innerSeq := r.inner.Find(ctx, pp)
	return func(yield func(Quad, error) bool) {
		for pq, err := range innerSeq {
			q := Quad{
				Subject:   pq.Subject,
				Predicate: pq.Predicate,
				Object:    pq.Object,
				Label:     pq.Label,
			}
			if !yield(q, err) {
				return
			}
			if err != nil {
				return
			}
		}
	}
}

// Count returns the number of quads matching the pattern.
func (r *PebbleReader) Count(ctx context.Context, p Pattern) (int64, error) {
	pp := pebbleq.Pattern{
		Subject:   p.Subject,
		Predicate: p.Predicate,
		Object:    p.Object,
		Label:     p.Label,
	}
	return r.inner.Count(ctx, pp)
}

// PebbleBulkLoader is the Pebble-backed bulk loader.
type PebbleBulkLoader struct {
	inner *pebbleq.BulkLoader
}

// Add buffers one quad.
func (b *PebbleBulkLoader) Add(q Quad) error {
	return b.inner.Add(pebbleq.Quad{
		Subject:   q.Subject,
		Predicate: q.Predicate,
		Object:    q.Object,
		Label:     q.Label,
	})
}

// Flush writes any buffered quads.
func (b *PebbleBulkLoader) Flush() error { return b.inner.Flush() }

// Close flushes any buffered quads and forces a final memtable ->
// sstable flush so the load is durable.
func (b *PebbleBulkLoader) Close() error { return b.inner.Close() }

// Stats returns a snapshot of the loader's progress.
func (b *PebbleBulkLoader) Stats() BulkStats {
	s := b.inner.Stats()
	return BulkStats{
		Added:     s.Added,
		Attempted: s.Attempted,
		Flushes:   s.Flushes,
	}
}

// toPebbleBatch converts the public Batch into pebbleq.Batch. Free
// struct copies — Quad fields are identical between the two types.
func toPebbleBatch(b Batch) pebbleq.Batch {
	pb := pebbleq.Batch{
		Label:    b.Label,
		Metadata: b.Metadata,
		NoAudit:  b.NoAudit,
	}
	if len(b.Adds) > 0 {
		pb.Adds = make([]pebbleq.Quad, len(b.Adds))
		for i, q := range b.Adds {
			pb.Adds[i] = pebbleq.Quad{
				Subject:   q.Subject,
				Predicate: q.Predicate,
				Object:    q.Object,
				Label:     q.Label,
			}
		}
	}
	if len(b.Removes) > 0 {
		pb.Removes = make([]pebbleq.Quad, len(b.Removes))
		for i, q := range b.Removes {
			pb.Removes[i] = pebbleq.Quad{
				Subject:   q.Subject,
				Predicate: q.Predicate,
				Object:    q.Object,
				Label:     q.Label,
			}
		}
	}
	return pb
}

// avoid unused import in pruned configs
var _ = time.Now
