// IngestSorted is the bulk-ingest fast path on the Pebble backend.
// Builds per-keyspace sorted sstables externally and hands them to
// Pebble's db.Ingest, bypassing the memtable + WAL + compaction work
// that the standard Writer / BulkLoader paths trigger per write. For
// pure-migration workloads this is 5-10× faster than wb.Set per quad
// (CockroachDB's bulk-restore documents the same speedup pattern).
//
// Tradeoffs vs the standard write path:
//   - Memory: in-memory variant holds all 4 keyspaces' encodings
//     in RAM during the sort. Bounded at ~120 bytes/quad × 4 = ~500
//     bytes/quad of working set. ~10M quads fits comfortably on a
//     16 GB box. Larger corpora need IngestSortedExternal.
//   - No audit trail: the commits + commit_ops keyspaces are NOT
//     populated. This is bulk ingest, not commit-shaped writes; the
//     audit trail belongs to per-Commit semantics. Callers that need
//     audit on top of an ingest should issue a separate one-row
//     Writer.Commit afterward with metadata recording the ingest.
//   - Quads slice must be deduplicated by the caller. sstable.Writer
//     requires strictly increasing keys; duplicates will return an
//     error. We dedup post-sort as a safety net and report it in the
//     stats, but the caller is responsible for the upstream policy.
//   - Label counters are updated via Merge after ingest (single
//     batch, sync commit). Same semantics as the standard write path.
//
// The current implementation is the in-memory variant (option 1 of
// the three-level ladder in TODO.md). External merge sort and
// per-corpus driver pattern come next.
package pebbleq

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/objstorage/objstorageprovider"
	"github.com/cockroachdb/pebble/v2/sstable"
	"github.com/cockroachdb/pebble/v2/vfs"
)

// IngestSortedOptions controls IngestSorted behavior.
type IngestSortedOptions struct {
	// DefaultLabel is the label applied to quads whose own Label is
	// empty. Validated against the namespace prefixes if non-empty.
	DefaultLabel string

	// TmpDir is where intermediate sstables are written before being
	// handed to Pebble. Default: os.TempDir(). Sstables are deleted
	// after ingest succeeds (Pebble takes ownership during the call).
	TmpDir string
}

// IngestSortedStats reports what an IngestSorted call did.
type IngestSortedStats struct {
	QuadsIngested   int64
	DuplicatesSkipped int64
	SSTablesWritten int
	BytesWritten    int64
	Duration        time.Duration
}

// IngestSorted writes the given quads as four sorted sstables (one
// per keyspace) plus a fifth for any new label counters, then ingests
// them via db.Ingest. Quads need not be pre-sorted; this method sorts
// internally.
//
// Returns ErrInvalidLabel if any quad's effective label is invalid.
// Returns the underlying Pebble error if ingest fails.
//
// The Pebble store must use this package's Open (which registers the
// label-count merger). Sstables are tagged with the merger name so
// Pebble's manifest accepts them.
func (s *Store) IngestSorted(ctx context.Context, quads []Quad, opts IngestSortedOptions) (IngestSortedStats, error) {
	if s.closed {
		return IngestSortedStats{}, errors.New("pebbleq: store closed")
	}

	// 1. Validate up front — reject the whole batch on any label or
	//    empty-quad violation.
	if err := validateLabel(opts.DefaultLabel); err != nil {
		return IngestSortedStats{}, err
	}
	for i, q := range quads {
		if !q.valid() {
			return IngestSortedStats{}, fmt.Errorf("%w: quads[%d]", ErrEmptyQuad, i)
		}
		label := q.Label
		if label == "" {
			label = opts.DefaultLabel
		}
		if err := validateLabel(label); err != nil {
			return IngestSortedStats{}, fmt.Errorf("quads[%d]: %w", i, err)
		}
	}

	t0 := time.Now()

	// 2. Build per-keyspace key arrays + per-label deltas in one pass.
	n := len(quads)
	spo := make([][]byte, 0, n)
	pos := make([][]byte, 0, n)
	osp := make([][]byte, 0, n)
	lsp := make([][]byte, 0, n)
	labelDeltas := make(map[string]int64, 4)
	for _, q := range quads {
		label := q.Label
		if label == "" {
			label = opts.DefaultLabel
		}
		eff := Quad{Subject: q.Subject, Predicate: q.Predicate, Object: q.Object, Label: label}
		keys := fourKeysFor(eff)
		spo = append(spo, keys[0])
		pos = append(pos, keys[1])
		osp = append(osp, keys[2])
		lsp = append(lsp, keys[3])
		labelDeltas[label]++
	}

	// 3. Sort + dedup each keyspace. sstable.Writer requires strictly
	//    increasing keys; duplicate (s,p,o,l) tuples produce duplicate
	//    keys in all four keyspaces by construction. dedup once on
	//    the SPO axis, then drop the matching positions from the
	//    other three to keep them aligned. (Or just dedup each
	//    keyspace independently — same result since the input is
	//    deterministic by quad.)
	sortByteSlices(spo)
	sortByteSlices(pos)
	sortByteSlices(osp)
	sortByteSlices(lsp)

	uniqueSPO, dupCount := dedupSorted(spo)
	uniquePOS, _ := dedupSorted(pos)
	uniqueOSP, _ := dedupSorted(osp)
	uniqueLSP, _ := dedupSorted(lsp)

	// 4. Build the sstables under a temp dir.
	tmpDir := opts.TmpDir
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}
	workDir, err := os.MkdirTemp(tmpDir, "quadstore-ingest-sorted-")
	if err != nil {
		return IngestSortedStats{}, fmt.Errorf("mkdir tmp: %w", err)
	}
	defer os.RemoveAll(workDir)

	tableFormat := s.db.TableFormat()
	writerOpts := sstable.WriterOptions{
		Comparer:    pebble.DefaultComparer,
		MergerName:  labelCountMergerName,
		TableFormat: tableFormat,
	}

	stats := IngestSortedStats{
		QuadsIngested:     int64(len(uniqueSPO)),
		DuplicatesSkipped: int64(dupCount),
	}

	paths := []string{}
	for _, kspace := range []struct {
		name string
		keys [][]byte
	}{
		{"spo", uniqueSPO},
		{"pos", uniquePOS},
		{"osp", uniqueOSP},
		{"lsp", uniqueLSP},
	} {
		path := filepath.Join(workDir, kspace.name+".sst")
		written, err := writeKeyOnlySSTable(path, kspace.keys, writerOpts)
		if err != nil {
			return stats, fmt.Errorf("write %s sstable: %w", kspace.name, err)
		}
		paths = append(paths, path)
		stats.BytesWritten += written
	}
	stats.SSTablesWritten = len(paths)

	// 5. Hand the sstables to Pebble. db.Ingest is atomic across the
	//    set: either all sstables land or none do.
	if err := s.db.Ingest(ctx, paths); err != nil {
		return stats, fmt.Errorf("pebble ingest: %w", err)
	}

	// 6. Apply per-label counter deltas via Merge. Sync commit so
	//    the counters are durable when IngestSorted returns.
	if len(labelDeltas) > 0 {
		wb := s.db.NewBatch()
		defer wb.Close()
		if err := applyLabelDeltas(wb, labelDeltas); err != nil {
			return stats, err
		}
		if err := wb.Commit(pebble.Sync); err != nil {
			return stats, err
		}
	}

	stats.Duration = time.Since(t0)
	return stats, nil
}

// writeKeyOnlySSTable writes a sorted set of byte-keys (with empty
// values, matching the SPO/POS/OSP/LSP keyspace convention) as a
// single Pebble-compatible sstable at path. Returns bytes-written
// for stats reporting.
func writeKeyOnlySSTable(path string, keys [][]byte, opts sstable.WriterOptions) (int64, error) {
	f, err := vfs.Default.Create(path, vfs.WriteCategoryUnspecified)
	if err != nil {
		return 0, err
	}
	w := sstable.NewWriter(objstorageprovider.NewFileWritable(f), opts)
	for _, k := range keys {
		if err := w.Set(k, nil); err != nil {
			_ = w.Close()
			return 0, fmt.Errorf("sstable Set: %w", err)
		}
	}
	if err := w.Close(); err != nil {
		return 0, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// sortByteSlices sorts a slice of byte slices lexicographically.
// Stable sort is not required — Pebble's keyspace order is total.
func sortByteSlices(s [][]byte) {
	sort.Slice(s, func(i, j int) bool {
		return bytes.Compare(s[i], s[j]) < 0
	})
}

// dedupSorted removes consecutive byte-identical entries from a
// pre-sorted slice. Returns the deduplicated slice (sharing the
// input's backing array) and the number of duplicates dropped.
func dedupSorted(s [][]byte) ([][]byte, int) {
	if len(s) <= 1 {
		return s, 0
	}
	out := s[:1]
	dups := 0
	for i := 1; i < len(s); i++ {
		if bytes.Equal(s[i], out[len(out)-1]) {
			dups++
			continue
		}
		out = append(out, s[i])
	}
	return out, dups
}
