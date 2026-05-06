// Per-label counter for the Pebble backend. Maintains a counter
// keyspace ('L' | label → 8-byte LE int64) updated on every Writer.Commit
// and BulkLoader flush via Pebble's Merge operator. This turns
// Reader.Count(Pattern{Label: X}) into a single 8-byte Get instead of an
// O(N) range scan over the LSP keyspace.
//
// Why Merge: Pebble's Merge operator is associative and combines fragments
// at compaction or read time. Multiple concurrent Writers calling Merge on
// the same counter key are safe — Pebble accumulates fragments and the
// merger sums them. No write-side serialization needed.
//
// Drift surface: the counter assumes that every Add to a label is "new
// data" (the LSP key didn't already exist). If a caller commits the same
// quad twice, the LSP keyspace dedups but the counter would over-count.
// In practice SecDek-shape workloads don't double-commit; for callers that
// might, RebuildLabelCounters() walks LSP and resets to truth.
package pebbleq

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/cockroachdb/pebble/v2"
)

// labelCountMergerName is persisted in the Pebble manifest. Bumping this
// constant is a breaking change — existing Pebble dirs created with an
// older name will not open with the new name.
const labelCountMergerName = "quadstore.label-count.v1"

// labelCountMerger is the Pebble.Merger registered at Open() time. The
// Merge function below initialises a ValueMerger for one operand. Pebble
// later calls MergeNewer/MergeOlder for additional operands and Finish to
// produce the final stored value.
//
// Only the 'L' (label-count) keyspace is ever Merge'd — every other key
// in the quadstore Pebble layout is written via Set. If a non-counter key
// were Merge'd somehow, the merger still expects 8-byte values and errors
// loudly otherwise (loud is preferable to silent corruption).
var labelCountMerger = &pebble.Merger{
	Name: labelCountMergerName,
	Merge: func(_, value []byte) (pebble.ValueMerger, error) {
		if len(value) != 8 {
			return nil, fmt.Errorf("pebbleq label-count merger: expected 8-byte value, got %d", len(value))
		}
		return &labelCountValueMerger{
			sum: int64(binary.LittleEndian.Uint64(value)),
		}, nil
	},
}

// labelCountValueMerger accumulates int64 deltas (signed) into a running
// sum. Both MergeNewer and MergeOlder add — addition is associative so
// fragment order doesn't matter.
type labelCountValueMerger struct {
	sum int64
}

func (m *labelCountValueMerger) MergeNewer(value []byte) error {
	if len(value) != 8 {
		return fmt.Errorf("pebbleq label-count merger: expected 8-byte value, got %d", len(value))
	}
	m.sum += int64(binary.LittleEndian.Uint64(value))
	return nil
}

func (m *labelCountValueMerger) MergeOlder(value []byte) error {
	if len(value) != 8 {
		return fmt.Errorf("pebbleq label-count merger: expected 8-byte value, got %d", len(value))
	}
	m.sum += int64(binary.LittleEndian.Uint64(value))
	return nil
}

func (m *labelCountValueMerger) Finish(_ bool) ([]byte, io.Closer, error) {
	out := make([]byte, 8)
	binary.LittleEndian.PutUint64(out, uint64(m.sum))
	return out, nil, nil
}

// encodeLabelCountKey is 'L' | label.
func encodeLabelCountKey(label string) []byte {
	out := make([]byte, 1+len(label))
	out[0] = prefLabelCount
	copy(out[1:], label)
	return out
}

// encodeLabelDelta encodes a signed int64 as 8 bytes little-endian for
// use as a Merge operand value.
func encodeLabelDelta(delta int64) []byte {
	out := make([]byte, 8)
	binary.LittleEndian.PutUint64(out, uint64(delta))
	return out
}

// decodeLabelCount reads an int64 from a stored counter value.
func decodeLabelCount(value []byte) (int64, error) {
	if len(value) != 8 {
		return 0, fmt.Errorf("pebbleq: label-count value must be 8 bytes, got %d", len(value))
	}
	return int64(binary.LittleEndian.Uint64(value)), nil
}

// readLabelCount returns the current count for a label. Missing key → 0.
func (s *Store) readLabelCount(label string) (int64, error) {
	val, closer, err := s.db.Get(encodeLabelCountKey(label))
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return 0, nil
		}
		return 0, err
	}
	defer closer.Close()
	return decodeLabelCount(val)
}

// aggregateLabelDeltas computes net per-label deltas from Adds + Removes
// in a Batch. Adds whose Label is empty inherit batchLabel. Removes use
// the Quad's own Label (which mirrors how Removes work in Commit — they
// can target any pre-existing label).
func aggregateLabelDeltas(adds, removes []Quad, batchLabel string) map[string]int64 {
	deltas := make(map[string]int64, 4)
	for _, q := range adds {
		label := q.Label
		if label == "" {
			label = batchLabel
		}
		deltas[label]++
	}
	for _, q := range removes {
		label := q.Label
		if label == "" {
			label = batchLabel
		}
		deltas[label]--
	}
	return deltas
}

// applyLabelDeltas issues one Merge per non-zero entry in deltas onto the
// given batch. Caller is responsible for committing the batch.
func applyLabelDeltas(wb *pebble.Batch, deltas map[string]int64) error {
	for label, d := range deltas {
		if d == 0 {
			continue
		}
		if err := wb.Merge(encodeLabelCountKey(label), encodeLabelDelta(d), nil); err != nil {
			return err
		}
	}
	return nil
}

// RebuildLabelCounters walks the LSP keyspace, computes per-label totals,
// and resets the 'L' (label-count) keyspace to those totals. Use after a
// migration that bypassed Merge writes, or any time you suspect drift
// (callers double-committing, partial crash recovery, etc.).
//
// Concurrent writers will see eventual consistency: Merge fragments
// applied during the rebuild's read phase are dropped on DeleteRange,
// then re-emerge as new Merges on top of the rebuilt base. Operationally
// the safe pattern is "pause writers, rebuild, resume."
func (s *Store) RebuildLabelCounters() error {
	if s.closed {
		return errors.New("pebbleq: store closed")
	}

	// Phase 1: count by scanning LSP.
	counts := make(map[string]int64, 32)
	iter, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{prefLSP},
		UpperBound: []byte{prefLSP + 1},
	})
	if err != nil {
		return err
	}
	for iter.First(); iter.Valid(); iter.Next() {
		k := iter.Key()
		if len(k) < 2 || k[0] != prefLSP {
			continue
		}
		rest := k[1:]
		idx := bytes.IndexByte(rest, sep)
		if idx < 0 {
			continue
		}
		counts[string(rest[:idx])]++
	}
	if err := iter.Error(); err != nil {
		iter.Close()
		return err
	}
	iter.Close()

	// Phase 2: replace the entire 'L' keyspace atomically. DeleteRange
	// erases anything previously persisted (including stale Merge fragments)
	// and the Sets establish the fresh base values.
	wb := s.db.NewBatch()
	defer wb.Close()
	if err := wb.DeleteRange([]byte{prefLabelCount}, []byte{prefLabelCount + 1}, nil); err != nil {
		return err
	}
	for label, n := range counts {
		if err := wb.Set(encodeLabelCountKey(label), encodeLabelDelta(n), nil); err != nil {
			return err
		}
	}
	return wb.Commit(pebble.Sync)
}
