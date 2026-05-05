// Package pebbleq is a Pebble-backed quadstore prototype. Lives at
// internal/ until we decide to expose it as a public backend
// (quadstore.OpenPebble); see docs/PEBBLE_VS_SQLITE.md and
// docs/RETHINK_TEST_PLAN.md for status.
//
// Feature parity with the SQLite-backed quadstore on the writer/
// reader/bulk-loader surface:
//
//   - Four sorted keyspaces (SPO/POS/OSP/LSP) plus C (commits) and
//     CO (commit_ops). Each quad is four key writes; each audited
//     commit is one C row + one CO row per quad.
//   - Label namespace validation: source: / derived: / human: /
//     meta: enforced at Writer.Commit (matches writer.go).
//   - Batch.NoAudit suppresses C+CO writes for hot paths.
//   - Reader.Find returns iter.Seq2[Quad, error] and routes to the
//     right keyspace based on which Pattern fields are bound.
//   - BulkLoader buffers, batches WriteBatches at NoSync, and
//     Flushes at Close for durability.
//
// What this prototype does NOT have:
//   - Partitioning (single Pebble dir per Store).
//   - Match/Path/Stats/CommitStats — the higher-level helpers.
//   - Migrate compatibility (separate concern).
package pebbleq

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"time"

	"github.com/cockroachdb/pebble/v2"
	"github.com/google/uuid"
)

// Keyspace prefix bytes. The single-byte separator from canonical
// data values is required to avoid label/predicate/object collisions
// (SPO subject="abc" must not collide with subject="ab" predicate="c").
const (
	prefSPO byte = 'q' // SPO: subject  | predicate | object   | label
	prefPOS byte = 'p' // POS: predicate | object   | subject | label
	prefOSP byte = 'o' // OSP: object   | subject  | predicate | label
	prefLSP byte = 'l' // LSP: label    | subject  | predicate | object
	prefC   byte = 'c' // commits:    'c' | commitID
	prefCO  byte = 'C' // commit_ops: 'C' | commitID | seq | op
	sep     byte = 0x00
)

// validLabelPrefixes mirrors writer.go to keep namespace
// enforcement identical across backends.
var validLabelPrefixes = []string{"source:", "derived:", "human:", "meta:"}

// MetaActor / MetaSource / MetaReason are documented Metadata keys.
// Mirrors quadstore package; redeclared so callers using only
// internal/pebbleq don't import both.
const (
	MetaActor  = "actor"
	MetaSource = "source"
	MetaReason = "reason"
)

// Sentinel errors. Names mirror the SQLite-backed quadstore.
var (
	ErrInvalidLabel = errors.New("pebbleq: invalid label namespace")
	ErrWriterClosed = errors.New("pebbleq: writer closed")
	ErrEmptyQuad    = errors.New("pebbleq: subject, predicate, object required")
)

// Quad mirrors quadstore.Quad.
type Quad struct {
	Subject, Predicate, Object, Label string
}

func (q Quad) valid() bool {
	return q.Subject != "" && q.Predicate != "" && q.Object != ""
}

// Batch mirrors quadstore.Batch (including NoAudit).
type Batch struct {
	Adds     []Quad
	Removes  []Quad
	Label    string
	Metadata map[string]string
	NoAudit  bool
}

// Pattern mirrors quadstore.Pattern. Empty fields are wildcards.
type Pattern struct {
	Subject, Predicate, Object, Label string
}

// Store is a Pebble-backed quadstore Store.
type Store struct {
	db     *pebble.DB
	closed bool
}

// quietLogger discards Pebble's startup chatter but panics on
// fatals so the runtime can react.
type quietLogger struct{}

func (quietLogger) Infof(string, ...interface{})  {}
func (quietLogger) Errorf(string, ...interface{}) {}
func (quietLogger) Fatalf(format string, args ...interface{}) {
	panic(fmt.Sprintf(format, args...))
}

// Open opens or creates a Pebble store at path. Defaults are
// out-of-the-box Pebble options other than a quiet logger.
func Open(path string) (*Store, error) {
	db, err := pebble.Open(path, &pebble.Options{
		Logger: quietLogger{},
	})
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close releases the Pebble handle. Idempotent.
func (s *Store) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}

// validateLabel mirrors writer.validateLabel.
func validateLabel(label string) error {
	if label == "" {
		return nil
	}
	for _, p := range validLabelPrefixes {
		if strings.HasPrefix(label, p) {
			return nil
		}
	}
	return fmt.Errorf("%w: %q must start with one of: %s",
		ErrInvalidLabel, label, strings.Join(validLabelPrefixes, ", "))
}

// ============================================================
// Key encoding
// ============================================================

// encodeKey writes a NUL-separated key in a one-byte keyspace.
// Caller is responsible for ensuring fields contain no NULs (true
// for synthetic test data and SecDek-shape production data).
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
// than any key starting with prefix. Bumps the last byte by 1.
// Caller guarantees the last byte != 0xFF.
func upperBoundForPrefix(prefix []byte) []byte {
	out := make([]byte, len(prefix))
	copy(out, prefix)
	out[len(prefix)-1]++
	return out
}

// decodeQuadKey decodes a key from one of the four data keyspaces
// back into a Quad. The keyspace is determined by the prefix byte;
// field order varies. Returns false if the key isn't a valid quad.
func decodeQuadKey(key []byte) (Quad, bool) {
	if len(key) < 1 {
		return Quad{}, false
	}
	parts := bytes.SplitN(key[1:], []byte{sep}, 4)
	if len(parts) != 4 {
		return Quad{}, false
	}
	switch key[0] {
	case prefSPO:
		return Quad{Subject: string(parts[0]), Predicate: string(parts[1]), Object: string(parts[2]), Label: string(parts[3])}, true
	case prefPOS:
		return Quad{Predicate: string(parts[0]), Object: string(parts[1]), Subject: string(parts[2]), Label: string(parts[3])}, true
	case prefOSP:
		return Quad{Object: string(parts[0]), Subject: string(parts[1]), Predicate: string(parts[2]), Label: string(parts[3])}, true
	case prefLSP:
		return Quad{Label: string(parts[0]), Subject: string(parts[1]), Predicate: string(parts[2]), Object: string(parts[3])}, true
	}
	return Quad{}, false
}

// fourKeysFor returns the four keyspace keys for a given quad.
// Used by Writer.Commit and BulkLoader.Add.
func fourKeysFor(q Quad) [4][]byte {
	return [4][]byte{
		encodeKey(prefSPO, q.Subject, q.Predicate, q.Object, q.Label),
		encodeKey(prefPOS, q.Predicate, q.Object, q.Subject, q.Label),
		encodeKey(prefOSP, q.Object, q.Subject, q.Predicate, q.Label),
		encodeKey(prefLSP, q.Label, q.Subject, q.Predicate, q.Object),
	}
}

// ============================================================
// Writer
// ============================================================

// Writer is a write handle. Pebble has no single-writer constraint,
// but we keep the type so the API matches the SQLite quadstore.
type Writer struct {
	store  *Store
	closed bool
}

// Writer returns a Writer.
func (s *Store) Writer(_ context.Context) (*Writer, error) {
	return &Writer{store: s}, nil
}

// Close marks the writer closed; subsequent Commit returns
// ErrWriterClosed.
func (w *Writer) Close() error {
	w.closed = true
	return nil
}

// Commit applies a Batch atomically. Label namespace validation
// runs before any writes; on validation failure no keys are
// touched. NoAudit suppresses the commits + commit_ops writes.
func (w *Writer) Commit(_ context.Context, b Batch) error {
	if w.closed {
		return ErrWriterClosed
	}

	// Validate Batch.Label and every Add/Remove label up front.
	if err := validateLabel(b.Label); err != nil {
		return err
	}
	for i, q := range b.Adds {
		if !q.valid() {
			return fmt.Errorf("%w: Adds[%d]", ErrEmptyQuad, i)
		}
		label := q.Label
		if label == "" {
			label = b.Label
		}
		if err := validateLabel(label); err != nil {
			return fmt.Errorf("Adds[%d]: %w", i, err)
		}
	}
	for i, q := range b.Removes {
		if !q.valid() {
			return fmt.Errorf("%w: Removes[%d]", ErrEmptyQuad, i)
		}
		// Removes with a label use it as-is (may be the empty
		// label for legacy data); no validation. Mirrors writer.go.
	}

	wb := w.store.db.NewBatch()
	defer wb.Close()

	// Commit row + commit_ops rows live under their own keyspaces
	// and are skipped when NoAudit is set.
	var (
		commitID  string
		commitNow int64
	)
	if !b.NoAudit {
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("pebbleq: generate commit id: %w", err)
		}
		commitID = id.String()
		commitNow = time.Now().Unix()

		// Encode the commit row value: ts | label | metadata-json.
		var meta []byte
		if len(b.Metadata) > 0 {
			meta, err = json.Marshal(b.Metadata)
			if err != nil {
				return fmt.Errorf("pebbleq: marshal metadata: %w", err)
			}
		}
		val := encodeCommitValue(commitNow, b.Label, meta)
		if err := wb.Set(encodeKey(prefC, commitID), val, nil); err != nil {
			return err
		}
	}

	var opSeq uint32
	for _, q := range b.Adds {
		label := q.Label
		if label == "" {
			label = b.Label
		}
		eff := Quad{Subject: q.Subject, Predicate: q.Predicate, Object: q.Object, Label: label}
		for _, k := range fourKeysFor(eff) {
			if err := wb.Set(k, nil, nil); err != nil {
				return err
			}
		}
		if !b.NoAudit {
			if err := wb.Set(commitOpKey(commitID, opSeq), commitOpValue("add", eff), nil); err != nil {
				return err
			}
			opSeq++
		}
	}
	for _, q := range b.Removes {
		for _, k := range fourKeysFor(q) {
			if err := wb.Delete(k, nil); err != nil {
				return err
			}
		}
		if !b.NoAudit {
			if err := wb.Set(commitOpKey(commitID, opSeq), commitOpValue("remove", q), nil); err != nil {
				return err
			}
			opSeq++
		}
	}

	// NoSync mirrors SQLite synchronous=NORMAL: WAL appended, not
	// fsynced per Commit. CommitSync is the strict-durability variant.
	return wb.Commit(pebble.NoSync)
}

// CommitSync is the per-Commit-fsync variant. ~1000× slower than
// Commit on M1 Pro; appropriate when you must not lose a committed
// write across a process crash. See docs/PEBBLE_VS_SQLITE.md
// "Durability matters."
func (w *Writer) CommitSync(ctx context.Context, b Batch) error {
	if err := w.Commit(ctx, b); err != nil {
		return err
	}
	// Force WAL fsync after the NoSync commit landed.
	return w.store.db.LogData(nil, pebble.Sync)
}

// encodeCommitValue packs (ts, label, metadata) into a byte value.
// Format:  varint(ts) | varint(len(label)) | label | metadata-json
// (json.Marshal returns nil bytes when metadata is empty).
func encodeCommitValue(ts int64, label string, metadataJSON []byte) []byte {
	const maxVarint = binary.MaxVarintLen64
	buf := make([]byte, 0, maxVarint+1+len(label)+len(metadataJSON)+1)
	var tmp [maxVarint]byte
	n := binary.PutVarint(tmp[:], ts)
	buf = append(buf, tmp[:n]...)
	n = binary.PutUvarint(tmp[:], uint64(len(label)))
	buf = append(buf, tmp[:n]...)
	buf = append(buf, label...)
	buf = append(buf, metadataJSON...)
	return buf
}

// commitOpKey builds a key for one operation row in the audit log:
//
//	'C' | commitID | sep | varint(seq)
func commitOpKey(commitID string, seq uint32) []byte {
	var seqBuf [binary.MaxVarintLen32]byte
	n := binary.PutUvarint(seqBuf[:], uint64(seq))
	out := make([]byte, 0, 2+len(commitID)+n)
	out = append(out, prefCO)
	out = append(out, commitID...)
	out = append(out, sep)
	out = append(out, seqBuf[:n]...)
	return out
}

// commitOpValue packs (op, subject, predicate, object, label) for
// the audit row's value column. Replay-friendly: every field is
// length-prefixed so even NUL bytes survive.
func commitOpValue(op string, q Quad) []byte {
	const maxVarint = binary.MaxVarintLen64
	totalLen := 1 // op byte: 'a' or 'r'
	for _, f := range []string{q.Subject, q.Predicate, q.Object, q.Label} {
		totalLen += maxVarint + len(f)
	}
	buf := make([]byte, 0, totalLen)
	switch op {
	case "add":
		buf = append(buf, 'a')
	case "remove":
		buf = append(buf, 'r')
	default:
		buf = append(buf, '?')
	}
	var tmp [maxVarint]byte
	for _, f := range []string{q.Subject, q.Predicate, q.Object, q.Label} {
		n := binary.PutUvarint(tmp[:], uint64(len(f)))
		buf = append(buf, tmp[:n]...)
		buf = append(buf, f...)
	}
	return buf
}

// ============================================================
// Reader
// ============================================================

// Reader is a read handle. Mirrors quadstore.Reader.
type Reader struct {
	store *Store
}

// Reader returns a Reader against the store.
func (s *Store) Reader() *Reader { return &Reader{store: s} }

// Find returns an iter.Seq2[Quad, error] of every quad matching
// the pattern. Routing rules (which keyspace to scan) are picked
// to minimize the scanned range:
//
//   - Subject set                  → SPO scan (subject prefix)
//   - Subject unset, Predicate set → POS scan (predicate prefix)
//   - Subject unset, Predicate unset, Object set → OSP scan
//   - Only Label set               → LSP scan
//   - Nothing set                  → full SPO scan
//
// Once the keyspace is chosen, in-row matching of the still-bound
// fields filters the iterator. Empty Label means "any label."
func (r *Reader) Find(_ context.Context, p Pattern) iter.Seq2[Quad, error] {
	return func(yield func(Quad, error) bool) {
		var lower, upper []byte

		// Choose keyspace by first bound field.
		switch {
		case p.Subject != "":
			// SPO. Build longest possible prefix from bound fields.
			parts := []string{p.Subject}
			if p.Predicate != "" {
				parts = append(parts, p.Predicate)
				if p.Object != "" {
					parts = append(parts, p.Object)
				}
			}
			pref := encodeKey(prefSPO, parts...)
			pref = append(pref, sep)
			lower = pref
			upper = upperBoundForPrefix(pref)
		case p.Predicate != "":
			// POS scan.
			parts := []string{p.Predicate}
			if p.Object != "" {
				parts = append(parts, p.Object)
			}
			pref := encodeKey(prefPOS, parts...)
			pref = append(pref, sep)
			lower = pref
			upper = upperBoundForPrefix(pref)
		case p.Object != "":
			// OSP scan.
			pref := encodeKey(prefOSP, p.Object)
			pref = append(pref, sep)
			lower = pref
			upper = upperBoundForPrefix(pref)
		case p.Label != "":
			// LSP scan.
			pref := encodeKey(prefLSP, p.Label)
			pref = append(pref, sep)
			lower = pref
			upper = upperBoundForPrefix(pref)
		default:
			// Full SPO scan.
			lower = []byte{prefSPO}
			upper = []byte{prefSPO + 1}
		}

		iter, err := r.store.db.NewIter(&pebble.IterOptions{
			LowerBound: lower,
			UpperBound: upper,
		})
		if err != nil {
			yield(Quad{}, err)
			return
		}
		defer iter.Close()

		for iter.First(); iter.Valid(); iter.Next() {
			q, ok := decodeQuadKey(iter.Key())
			if !ok {
				continue
			}
			// In-row filter: any bound field that wasn't part of the
			// keyspace prefix still has to match.
			if p.Subject != "" && q.Subject != p.Subject {
				continue
			}
			if p.Predicate != "" && q.Predicate != p.Predicate {
				continue
			}
			if p.Object != "" && q.Object != p.Object {
				continue
			}
			if p.Label != "" && q.Label != p.Label {
				continue
			}
			if !yield(q, nil) {
				return
			}
		}
		if err := iter.Error(); err != nil {
			yield(Quad{}, err)
			return
		}
	}
}

// Count returns the number of quads matching the pattern. Implemented
// as iter-and-count; production port could optimize via Pebble's
// per-sstable row counts but at our scale it's not the bottleneck.
func (r *Reader) Count(ctx context.Context, p Pattern) (int64, error) {
	var n int64
	for _, err := range r.Find(ctx, p) {
		if err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

// ============================================================
// BulkLoader
// ============================================================

// BulkStats mirrors quadstore.BulkStats. Added is the count of
// distinct (s,p,o,l) tuples written. Pebble's Set is upsert (no
// IGNORE semantics), so duplicates overwrite themselves; Added in
// the prototype tracks Add() calls, not unique-row insertions.
type BulkStats struct {
	Added     int64
	Attempted int64
	Flushes   int64
}

// BulkLoader is a write-optimized bulk-ingestion path. Buffers
// quads, flushes WriteBatches at NoSync, fsyncs at Close.
type BulkLoader struct {
	store     *Store
	batchSize int
	wb        *pebble.Batch
	bufRows   int
	label     string
	stats     BulkStats
	closed    bool
}

// BulkLoader opens a BulkLoader. defaultLabel becomes the label
// applied to quads whose own Label is empty. The default label is
// validated up front; namespace rules apply.
func (s *Store) BulkLoader(_ context.Context, defaultLabel string) (*BulkLoader, error) {
	if defaultLabel != "" {
		if err := validateLabel(defaultLabel); err != nil {
			return nil, err
		}
	}
	return &BulkLoader{
		store:     s,
		batchSize: 500,
		label:     defaultLabel,
		wb:        s.db.NewBatch(),
	}, nil
}

// BulkLoaderWithLabel is an alias for BulkLoader to mirror the
// quadstore API name.
func (s *Store) BulkLoaderWithLabel(ctx context.Context, defaultLabel string) (*BulkLoader, error) {
	return s.BulkLoader(ctx, defaultLabel)
}

// Add buffers one quad. Auto-flushes at batchSize. Idempotent on
// duplicate (s,p,o,l) — Pebble's Set is upsert.
func (b *BulkLoader) Add(q Quad) error {
	if b.closed {
		return errors.New("pebbleq: bulk loader closed")
	}
	if !q.valid() {
		return ErrEmptyQuad
	}
	label := q.Label
	if label == "" {
		label = b.label
	}
	eff := Quad{Subject: q.Subject, Predicate: q.Predicate, Object: q.Object, Label: label}
	for _, k := range fourKeysFor(eff) {
		if err := b.wb.Set(k, nil, nil); err != nil {
			return err
		}
	}
	b.stats.Added++
	b.stats.Attempted++
	b.bufRows++
	if b.bufRows >= b.batchSize {
		return b.flush()
	}
	return nil
}

// Flush writes any buffered quads immediately.
func (b *BulkLoader) Flush() error { return b.flush() }

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
	b.stats.Flushes++
	return nil
}

// Close flushes any buffered quads and forces a final memtable -> SST
// flush so the load is durable across a crash.
func (b *BulkLoader) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	if err := b.flush(); err != nil {
		return err
	}
	if b.wb != nil {
		b.wb.Close()
	}
	return b.store.db.Flush()
}

// Stats returns a snapshot of the loader's progress.
func (b *BulkLoader) Stats() BulkStats { return b.stats }
