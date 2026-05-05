// Package pebbleq is a comparative-benchmark prototype: a quadstore
// backed by github.com/cockroachdb/pebble/v2 instead of SQLite.
//
// This is NOT a production-ready backend. It exists so we can run
// head-to-head benchmarks against the SQLite-backed library and
// answer the question "would Pebble actually be faster for our
// workload?" with measured numbers rather than intuition.
//
// Limitations vs the production quadstore:
//   - No commit_ops audit trail
//   - No partition routing (single keyspace cluster per Store)
//   - No label namespace validation
//   - No Pattern shapes beyond subject-prefix lookup
//   - Null-byte separated keys (synthetic test data is null-free)
//
// Four keyspaces hold one row per quad each, distinguished by a
// single prefix byte. Each quad is written to all four; reads pick
// the keyspace whose leading column is bound by the Pattern.
//
//	SPO: 'q' | subject   '\0' predicate '\0' object    '\0' label
//	POS: 'p' | predicate '\0' object    '\0' subject   '\0' label
//	OSP: 'o' | object    '\0' subject   '\0' predicate '\0' label
//	LSP: 'l' | label     '\0' subject   '\0' predicate '\0' object
package pebbleq

import (
	"bytes"
	"context"
	"fmt"

	"github.com/cockroachdb/pebble/v2"
)

const (
	prefSPO byte = 'q'
	prefPOS byte = 'p'
	prefOSP byte = 'o'
	prefLSP byte = 'l'
	sep     byte = 0x00
)

// Quad mirrors quadstore.Quad for benchmark parity.
type Quad struct {
	Subject, Predicate, Object, Label string
}

// Batch mirrors quadstore.Batch for benchmark parity. Audit trail is
// not modeled in this prototype.
type Batch struct {
	Adds  []Quad
	Label string
}

// Pattern mirrors quadstore.Pattern for benchmark parity.
type Pattern struct {
	Subject, Predicate, Object, Label string
}

// Store is the Pebble-backed Store equivalent.
type Store struct {
	db     *pebble.DB
	closed bool
}

// quietLogger discards Pebble's startup chatter so it doesn't
// pollute benchmark output. We don't suppress fatals (Pebble panics
// on those, so the test runner sees them anyway).
type quietLogger struct{}

func (quietLogger) Infof(string, ...interface{})  {}
func (quietLogger) Errorf(string, ...interface{}) {}
func (quietLogger) Fatalf(format string, args ...interface{}) {
	panic(fmt.Sprintf(format, args...))
}

// Open opens or creates a Pebble-backed Store at path. Defaults are
// Pebble's out-of-box settings; we don't tune for this prototype so
// the comparison reflects "drop in Pebble, don't tune" — which is
// the honest worst-case for the engine.
func Open(path string) (*Store, error) {
	db, err := pebble.Open(path, &pebble.Options{
		Logger: quietLogger{},
	})
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close flushes pending writes and releases the Pebble handle.
// Idempotent: subsequent calls are no-ops.
func (s *Store) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}

// encodeKey builds a key in the given keyspace by concatenating the
// fields with NUL separators after a one-byte prefix.
func encodeKey(prefix byte, fields ...string) []byte {
	size := 1
	for i, f := range fields {
		size += len(f)
		if i < len(fields)-1 {
			size++
		}
	}
	buf := make([]byte, 0, size)
	buf = append(buf, prefix)
	for i, f := range fields {
		buf = append(buf, f...)
		if i < len(fields)-1 {
			buf = append(buf, sep)
		}
	}
	return buf
}

// upperBoundForPrefix returns the smallest key strictly greater
// than any key starting with prefix. Used as Pebble's exclusive
// UpperBound for prefix scans. We bump the last byte by 1 (assumes
// it's not 0xFF — true for our NUL-separated layouts since the
// last byte of the lower bound is always sep=0x00).
func upperBoundForPrefix(prefix []byte) []byte {
	out := make([]byte, len(prefix))
	copy(out, prefix)
	out[len(prefix)-1]++
	return out
}

// Writer is a write handle. Pebble has no single-writer constraint;
// concurrent Writers can land batches in parallel. The handle exists
// to mirror quadstore.Writer's API shape for the benchmark.
type Writer struct {
	store *Store
}

// Writer returns a Writer. Always succeeds; ctx is accepted for
// API parity with quadstore.Store.Writer.
func (s *Store) Writer(_ context.Context) (*Writer, error) {
	return &Writer{store: s}, nil
}

// Close releases the writer. No-op for this prototype.
func (w *Writer) Close() error { return nil }

// Commit applies a Batch atomically via a single Pebble WriteBatch
// across all four keyspaces. Uses pebble.NoSync to mirror SQLite's
// `synchronous=NORMAL` default — WAL is appended but not fsynced
// per Commit. This matches the durability semantics of the
// SQLite-backed quadstore. Callers who need strict per-commit
// durability should call Sync() after Commit, or open a Sync-mode
// Writer (not exposed in this prototype).
func (w *Writer) Commit(_ context.Context, b Batch) error {
	wb := w.store.db.NewBatch()
	defer wb.Close()
	for _, q := range b.Adds {
		label := q.Label
		if label == "" {
			label = b.Label
		}
		if err := wb.Set(encodeKey(prefSPO, q.Subject, q.Predicate, q.Object, label), nil, nil); err != nil {
			return err
		}
		if err := wb.Set(encodeKey(prefPOS, q.Predicate, q.Object, q.Subject, label), nil, nil); err != nil {
			return err
		}
		if err := wb.Set(encodeKey(prefOSP, q.Object, q.Subject, q.Predicate, label), nil, nil); err != nil {
			return err
		}
		if err := wb.Set(encodeKey(prefLSP, label, q.Subject, q.Predicate, q.Object), nil, nil); err != nil {
			return err
		}
	}
	return wb.Commit(pebble.NoSync)
}

// CommitSync is the strict-durability variant: fsyncs the WAL on
// every commit. ~100× slower per call but no committed write is lost
// across a process crash. Provided for the comparison bench.
func (w *Writer) CommitSync(ctx context.Context, b Batch) error {
	wb := w.store.db.NewBatch()
	defer wb.Close()
	for _, q := range b.Adds {
		label := q.Label
		if label == "" {
			label = b.Label
		}
		if err := wb.Set(encodeKey(prefSPO, q.Subject, q.Predicate, q.Object, label), nil, nil); err != nil {
			return err
		}
		if err := wb.Set(encodeKey(prefPOS, q.Predicate, q.Object, q.Subject, label), nil, nil); err != nil {
			return err
		}
		if err := wb.Set(encodeKey(prefOSP, q.Object, q.Subject, q.Predicate, label), nil, nil); err != nil {
			return err
		}
		if err := wb.Set(encodeKey(prefLSP, label, q.Subject, q.Predicate, q.Object), nil, nil); err != nil {
			return err
		}
	}
	return wb.Commit(pebble.Sync)
}

// Reader is a read handle. Like Writer, exists for API parity.
type Reader struct {
	store *Store
}

// Reader returns a Reader against the store.
func (s *Store) Reader() *Reader { return &Reader{store: s} }

// FindBySubject returns every quad whose subject matches. Uses the
// SPO keyspace prefix scan. Returns a materialized slice (the
// production library uses iter.Seq2; for the benchmark we only need
// to count rows).
func (r *Reader) FindBySubject(_ context.Context, subject string) ([]Quad, error) {
	prefix := encodeKey(prefSPO, subject)
	prefix = append(prefix, sep) // include the separator so we don't match prefixes like "subject1" when looking for "subject"
	upper := upperBoundForPrefix(prefix)
	iter, err := r.store.db.NewIter(&pebble.IterOptions{
		LowerBound: prefix,
		UpperBound: upper,
	})
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	var out []Quad
	for iter.First(); iter.Valid(); iter.Next() {
		key := iter.Key()
		// key layout after the 1-byte prefix: subject\0predicate\0object\0label
		parts := bytes.SplitN(key[1:], []byte{sep}, 4)
		if len(parts) != 4 {
			continue
		}
		out = append(out, Quad{
			Subject:   string(parts[0]),
			Predicate: string(parts[1]),
			Object:    string(parts[2]),
			Label:     string(parts[3]),
		})
	}
	return out, iter.Error()
}

// CountBySubject returns the number of quads whose subject matches.
// Faster than FindBySubject when the caller only wants a count;
// avoids decoding keys.
func (r *Reader) CountBySubject(_ context.Context, subject string) (int, error) {
	prefix := encodeKey(prefSPO, subject)
	prefix = append(prefix, sep)
	upper := upperBoundForPrefix(prefix)
	iter, err := r.store.db.NewIter(&pebble.IterOptions{
		LowerBound: prefix,
		UpperBound: upper,
	})
	if err != nil {
		return 0, err
	}
	defer iter.Close()
	n := 0
	for iter.First(); iter.Valid(); iter.Next() {
		n++
	}
	return n, iter.Error()
}

// BulkLoader is a write-optimized bulk-ingestion path. Buffers
// quads, applies in batched WriteBatches with Pebble.NoSync, then
// fsyncs once on Close. Mirrors the quadstore BulkLoader shape.
type BulkLoader struct {
	store     *Store
	batchSize int
	buf       []Quad
	label     string
	wb        *pebble.Batch
	bufRows   int
}

// BulkLoader opens a BulkLoader. label becomes the default label
// applied to quads with no Label of their own.
func (s *Store) BulkLoader(_ context.Context, label string) (*BulkLoader, error) {
	return &BulkLoader{
		store:     s,
		batchSize: 500,
		label:     label,
		wb:        s.db.NewBatch(),
	}, nil
}

// Add buffers one quad. Flushes when the buffer reaches batchSize.
func (b *BulkLoader) Add(q Quad) error {
	label := q.Label
	if label == "" {
		label = b.label
	}
	if err := b.wb.Set(encodeKey(prefSPO, q.Subject, q.Predicate, q.Object, label), nil, nil); err != nil {
		return err
	}
	if err := b.wb.Set(encodeKey(prefPOS, q.Predicate, q.Object, q.Subject, label), nil, nil); err != nil {
		return err
	}
	if err := b.wb.Set(encodeKey(prefOSP, q.Object, q.Subject, q.Predicate, label), nil, nil); err != nil {
		return err
	}
	if err := b.wb.Set(encodeKey(prefLSP, label, q.Subject, q.Predicate, q.Object), nil, nil); err != nil {
		return err
	}
	b.bufRows++
	if b.bufRows >= b.batchSize {
		return b.flush()
	}
	return nil
}

// flush commits the current WriteBatch with NoSync (loads are
// idempotent on restart in the production library; we mirror that
// durability tradeoff here) and starts a fresh batch.
func (b *BulkLoader) flush() error {
	if b.bufRows == 0 {
		return nil
	}
	if err := b.wb.Commit(pebble.NoSync); err != nil {
		return err
	}
	b.wb.Close()
	b.wb = b.store.db.NewBatch()
	b.bufRows = 0
	return nil
}

// Close flushes any buffered quads and forces a final fsync via
// Pebble's Flush + Sync paths.
func (b *BulkLoader) Close() error {
	if err := b.flush(); err != nil {
		return err
	}
	b.wb.Close()
	// Final fsync: trigger a memtable flush so all writes hit disk.
	return b.store.db.Flush()
}

// avoid unused import warning when go vet runs without bench
var _ = bytes.SplitN
