// IngestSortedExternal is the bounded-memory bulk-ingest path.
// External merge sort: read input in chunks, sort each chunk in
// memory, flush sorted "runs" to disk, then k-way-merge the runs
// into a single sorted sstable per keyspace and hand to db.Ingest.
//
// Working set per chunk: ~400 bytes/quad × ChunkSize quads.
// At ChunkSize=500_000 (default) that's ~200 MB — fits comfortably
// on a 16 GB host even with other workloads. Total disk during run:
// ~size of source data spread across run files; freed at merge time.
//
// Right for SlideDek-class consumers (133M+ quads) where the
// in-memory IngestSorted variant won't fit in RAM. For smaller
// corpora the in-memory variant is faster (no run-file write/read
// overhead).
//
// Caller pulls input from a channel; close the channel when done.
// Library blocks on chunk-fill, sorts the chunk, flushes a run, and
// continues. After the input channel closes, library does the k-way
// merge phase and ingests.
//
// Run file format:
//
//	<u32 LE: key length><key bytes><u32 LE: 0xFFFFFFFF as EOF marker>
//
// Length-prefixed sequences. EOF marker simplifies detection without
// needing trusted file size or a footer block.
package pebbleq

import (
	"bufio"
	"bytes"
	"container/heap"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/objstorage/objstorageprovider"
	"github.com/cockroachdb/pebble/v2/sstable"
	"github.com/cockroachdb/pebble/v2/vfs"
)

// IngestSortedExternalOptions controls IngestSortedExternal.
type IngestSortedExternalOptions struct {
	// DefaultLabel applied to quads with empty Label.
	DefaultLabel string

	// TmpDir for run files + final sstables. Default: os.TempDir().
	// Run files are deleted after merge succeeds; sstables after ingest.
	TmpDir string

	// ChunkSize is the number of input quads sorted in memory per run.
	// Default: 500_000. Larger = fewer runs (faster merge), more RAM.
	// Smaller = more runs, less RAM, slightly slower merge.
	ChunkSize int
}

// IngestSortedExternalStats reports what IngestSortedExternal did.
type IngestSortedExternalStats struct {
	QuadsIngested     int64
	DuplicatesSkipped int64 // dropped during merge (cross-run duplicates)
	SSTablesWritten   int
	BytesWritten      int64
	RunsCreated       int // intermediate run files
	Duration          time.Duration
}

// IngestSortedExternal reads quads from in until the channel is
// closed, then bulk-ingests them via external merge sort. Bounded
// memory regardless of corpus size.
//
// See package doc for working-set sizing and run file format.
func (s *Store) IngestSortedExternal(ctx context.Context, in <-chan Quad, opts IngestSortedExternalOptions) (IngestSortedExternalStats, error) {
	if s.closed {
		return IngestSortedExternalStats{}, errors.New("pebbleq: store closed")
	}
	if err := validateLabel(opts.DefaultLabel); err != nil {
		return IngestSortedExternalStats{}, err
	}
	if opts.ChunkSize <= 0 {
		opts.ChunkSize = 500_000
	}
	tmpDir := opts.TmpDir
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}
	workDir, err := os.MkdirTemp(tmpDir, "quadstore-ingest-sorted-external-")
	if err != nil {
		return IngestSortedExternalStats{}, fmt.Errorf("mkdir tmp: %w", err)
	}
	defer os.RemoveAll(workDir)

	t0 := time.Now()
	stats := IngestSortedExternalStats{}

	// Phase 1: chunked sort + run flush.
	// Per-keyspace run paths grouped by keyspace name.
	runPaths := map[string][]string{
		"spo": {},
		"pos": {},
		"osp": {},
		"lsp": {},
	}
	keyspaces := []string{"spo", "pos", "osp", "lsp"}

	chunk := make([]Quad, 0, opts.ChunkSize)
	labelDeltas := make(map[string]int64, 4)
	var totalRead int64

	flushChunk := func() error {
		if len(chunk) == 0 {
			return nil
		}
		runIdx := stats.RunsCreated
		// Build per-keyspace key arrays from this chunk.
		spo := make([][]byte, 0, len(chunk))
		pos := make([][]byte, 0, len(chunk))
		osp := make([][]byte, 0, len(chunk))
		lsp := make([][]byte, 0, len(chunk))
		for _, q := range chunk {
			label := q.Label
			if label == "" {
				label = opts.DefaultLabel
			}
			if err := validateLabel(label); err != nil {
				return fmt.Errorf("validate label %q: %w", label, err)
			}
			eff := Quad{Subject: q.Subject, Predicate: q.Predicate, Object: q.Object, Label: label}
			keys := fourKeysFor(eff)
			spo = append(spo, keys[0])
			pos = append(pos, keys[1])
			osp = append(osp, keys[2])
			lsp = append(lsp, keys[3])
			labelDeltas[label]++
		}
		sortByteSlices(spo)
		sortByteSlices(pos)
		sortByteSlices(osp)
		sortByteSlices(lsp)

		// Write each keyspace's run file.
		for _, kspace := range []struct {
			name string
			keys [][]byte
		}{
			{"spo", spo},
			{"pos", pos},
			{"osp", osp},
			{"lsp", lsp},
		} {
			path := filepath.Join(workDir, fmt.Sprintf("run-%06d-%s.dat", runIdx, kspace.name))
			if err := writeRunFile(path, kspace.keys); err != nil {
				return fmt.Errorf("write run %d %s: %w", runIdx, kspace.name, err)
			}
			runPaths[kspace.name] = append(runPaths[kspace.name], path)
		}
		stats.RunsCreated++
		// Reset chunk for next iteration. Keep capacity.
		chunk = chunk[:0]
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return stats, ctx.Err()
		case q, ok := <-in:
			if !ok {
				// Drain done — flush final partial chunk.
				if err := flushChunk(); err != nil {
					return stats, err
				}
				goto mergePhase
			}
			if !q.valid() {
				return stats, fmt.Errorf("%w: from input channel", ErrEmptyQuad)
			}
			chunk = append(chunk, q)
			totalRead++
			if len(chunk) >= opts.ChunkSize {
				if err := flushChunk(); err != nil {
					return stats, err
				}
			}
		}
	}

mergePhase:
	// Phase 2: k-way merge each keyspace's runs into a final sstable.
	tableFormat := s.db.TableFormat()
	writerOpts := sstable.WriterOptions{
		Comparer:    pebble.DefaultComparer,
		MergerName:  labelCountMergerName,
		TableFormat: tableFormat,
	}
	sstablePaths := []string{}
	for _, ks := range keyspaces {
		runs := runPaths[ks]
		sstPath := filepath.Join(workDir, ks+".sst")
		written, deduped, err := mergeRunsToSSTable(sstPath, runs, writerOpts)
		if err != nil {
			return stats, fmt.Errorf("merge %s: %w", ks, err)
		}
		sstablePaths = append(sstablePaths, sstPath)
		stats.BytesWritten += written
		// Deduped is the same number across all keyspaces (per-quad
		// dedup); record once from the SPO keyspace.
		if ks == "spo" {
			stats.DuplicatesSkipped = int64(deduped)
			stats.QuadsIngested = totalRead - int64(deduped)
		}
		// Run files for this keyspace are no longer needed.
		for _, r := range runs {
			_ = os.Remove(r)
		}
	}
	stats.SSTablesWritten = len(sstablePaths)

	// Phase 3: hand sstables to Pebble.
	if err := s.db.Ingest(ctx, sstablePaths); err != nil {
		return stats, fmt.Errorf("pebble ingest: %w", err)
	}

	// Phase 4: label-count deltas via Merge. Sync commit.
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

// ---- run file write/read ----

const runEOFMarker = uint32(0xFFFFFFFF)

// writeRunFile encodes a sorted [][]byte as length-prefixed records
// followed by an EOF marker.
func writeRunFile(path string, keys [][]byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20) // 1 MiB buffer
	var hdr [4]byte
	for _, k := range keys {
		if uint64(len(k)) >= uint64(runEOFMarker) {
			f.Close()
			return fmt.Errorf("key too large for run encoding: %d bytes", len(k))
		}
		binary.LittleEndian.PutUint32(hdr[:], uint32(len(k)))
		if _, err := w.Write(hdr[:]); err != nil {
			f.Close()
			return err
		}
		if _, err := w.Write(k); err != nil {
			f.Close()
			return err
		}
	}
	binary.LittleEndian.PutUint32(hdr[:], runEOFMarker)
	if _, err := w.Write(hdr[:]); err != nil {
		f.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// runReader streams keys back from a run file.
type runReader struct {
	f   *os.File
	buf *bufio.Reader
	cur []byte
	eof bool
	err error
}

func openRunReader(path string) (*runReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r := &runReader{
		f:   f,
		buf: bufio.NewReaderSize(f, 1<<20),
	}
	r.advance() // load first key
	return r, nil
}

func (r *runReader) advance() {
	if r.eof || r.err != nil {
		return
	}
	var hdr [4]byte
	if _, err := io.ReadFull(r.buf, hdr[:]); err != nil {
		r.err = err
		return
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n == runEOFMarker {
		r.eof = true
		r.cur = nil
		return
	}
	key := make([]byte, n)
	if _, err := io.ReadFull(r.buf, key); err != nil {
		r.err = err
		return
	}
	r.cur = key
}

func (r *runReader) Close() error {
	if r.f != nil {
		return r.f.Close()
	}
	return nil
}

// ---- k-way merge ----

// mergeRunsToSSTable opens all run files (each holding a sorted
// sequence of keys), k-way-merges them through a min-heap, dedups
// across runs, and writes the merged stream to a single sstable
// at sstPath. Returns (bytesWritten, duplicatesDropped, err).
func mergeRunsToSSTable(sstPath string, runPaths []string, opts sstable.WriterOptions) (int64, int, error) {
	// Open readers.
	readers := make([]*runReader, 0, len(runPaths))
	for _, p := range runPaths {
		r, err := openRunReader(p)
		if err != nil {
			for _, rr := range readers {
				rr.Close()
			}
			return 0, 0, fmt.Errorf("open run %s: %w", p, err)
		}
		readers = append(readers, r)
	}
	defer func() {
		for _, r := range readers {
			r.Close()
		}
	}()

	// Build min-heap from all readers that have at least one key.
	h := &runHeap{}
	for _, r := range readers {
		if r.err != nil {
			return 0, 0, fmt.Errorf("run read: %w", r.err)
		}
		if !r.eof {
			heap.Push(h, r)
		}
	}

	// Open the destination sstable.
	f, err := vfs.Default.Create(sstPath, vfs.WriteCategoryUnspecified)
	if err != nil {
		return 0, 0, err
	}
	w := sstable.NewWriter(objstorageprovider.NewFileWritable(f), opts)

	var lastEmitted []byte
	deduped := 0
	for h.Len() > 0 {
		top := heap.Pop(h).(*runReader)
		key := top.cur
		// Dedup against last emitted.
		if lastEmitted != nil && bytes.Equal(key, lastEmitted) {
			deduped++
		} else {
			if err := w.Set(key, nil); err != nil {
				_ = w.Close()
				return 0, 0, fmt.Errorf("sstable Set: %w", err)
			}
			lastEmitted = key
		}
		top.advance()
		if top.err != nil {
			_ = w.Close()
			return 0, 0, fmt.Errorf("run read: %w", top.err)
		}
		if !top.eof {
			heap.Push(h, top)
		}
	}

	if err := w.Close(); err != nil {
		return 0, 0, err
	}
	fi, err := os.Stat(sstPath)
	if err != nil {
		return 0, 0, err
	}
	return fi.Size(), deduped, nil
}

// runHeap is a min-heap of *runReader keyed by current key bytes.
type runHeap []*runReader

func (h runHeap) Len() int           { return len(h) }
func (h runHeap) Less(i, j int) bool { return bytes.Compare(h[i].cur, h[j].cur) < 0 }
func (h runHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *runHeap) Push(x interface{}) {
	*h = append(*h, x.(*runReader))
}
func (h *runHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// (sortByteSlices is defined in ingest_sorted.go and shared.)
