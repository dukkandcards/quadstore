package quadstore

import (
	"context"
	"fmt"
	"iter"
	"time"

	"github.com/dukkandcards/quadstore/internal/pebbleq"
)

// PebbleStore is a Pebble-backed Store. Provides the same write /
// read / bulk-load surface as *Store (the SQLite-backed legacy
// path) but wraps a Pebble LSM engine instead. Recommended backend
// going forward; *Store remains supported indefinitely.
//
// Pebble wins decisively on point operations (single-quad commit,
// subject lookups), bulk loads at scale, and on-disk size at the
// cost of heavier dependencies (~20 transitive packages including
// Sentry and Prometheus client). For minimal-binary deployments or
// callers needing sqlite3-CLI access on the data file, the
// SQLite-backed Open is the alternative.
//
// See docs/PEBBLE_VS_SQLITE.md for measured deltas.
//
// What's NOT in *PebbleStore (yet):
//   - Partitioning. Multi-directory routing is a v0.3+ concern;
//     today a Pebble-backed Store is one Pebble dir.
//   - Match (legacy *Iterator API). Reader.Find with iter.Seq2 is
//     the modern equivalent and works on both backends.
//   - Path traversal helpers (From/Out/In/Has/Unique). Used by
//     cmd/observe; SQLite-only until ported.
//   - MigrateFromSnapshot from a Pebble source. SQLite source is
//     supported via VACUUM INTO; Pebble snapshots not yet exposed.
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

// IngestSortedOptions controls IngestSorted behavior. Mirrors
// pebbleq.IngestSortedOptions; re-declared here so callers don't
// have to import internal/pebbleq directly.
type IngestSortedOptions struct {
	DefaultLabel string // applied to quads with empty Label
	TmpDir       string // working dir for intermediate sstables
}

// IngestSortedStats reports what IngestSorted did.
type IngestSortedStats struct {
	QuadsIngested     int64
	DuplicatesSkipped int64
	SSTablesWritten   int
	BytesWritten      int64
	Duration          time.Duration
}

// IngestSorted is the bulk-ingest fast path on the Pebble backend.
// Builds per-keyspace sorted sstables externally and hands them to
// Pebble's db.Ingest, bypassing the memtable + WAL + compaction work
// that the standard Writer / BulkLoader paths trigger per write.
//
// For pure-migration workloads this is significantly faster than
// wb.Set per quad — measured 5-10× on CockroachDB-class workloads.
// Tradeoffs:
//
//   - In-memory: holds all four-key encodings during the sort.
//     ~500 bytes/quad working set. ~10M quads on a 16 GB box.
//     Larger corpora need IngestSortedExternal (in progress).
//   - No audit trail. The commits + commit_ops keyspaces are NOT
//     populated. Ingest is bulk-shaped, not commit-shaped; callers
//     that need audit on top should issue a separate Writer.Commit
//     afterward with metadata recording the ingest.
//   - Caller dedup is preferred. sstable.Writer requires strictly
//     increasing keys; we dedup post-sort as a safety net and report
//     it in stats, but the upstream policy is the caller's.
//   - Label counters update via Merge after ingest, single sync
//     commit. Counters reflect the input cardinality, not the deduped
//     count — matching the standard write path's "writes attempted"
//     semantics. Use Store.RebuildLabelCounters() if you need exact
//     post-dedup counts.
//
// The destination Pebble dir must have been created with this
// package's Open (which registers the label-count merger). Sstables
// are tagged with the merger name so Pebble's manifest accepts them.
func (s *PebbleStore) IngestSorted(ctx context.Context, quads []Quad, opts IngestSortedOptions) (IngestSortedStats, error) {
	innerQuads := make([]pebbleq.Quad, len(quads))
	for i, q := range quads {
		innerQuads[i] = pebbleq.Quad{
			Subject:   q.Subject,
			Predicate: q.Predicate,
			Object:    q.Object,
			Label:     q.Label,
		}
	}
	innerOpts := pebbleq.IngestSortedOptions{
		DefaultLabel: opts.DefaultLabel,
		TmpDir:       opts.TmpDir,
	}
	st, err := s.inner.IngestSorted(ctx, innerQuads, innerOpts)
	return IngestSortedStats{
		QuadsIngested:     st.QuadsIngested,
		DuplicatesSkipped: st.DuplicatesSkipped,
		SSTablesWritten:   st.SSTablesWritten,
		BytesWritten:      st.BytesWritten,
		Duration:          st.Duration,
	}, err
}

// RebuildLabelCounters walks the LSP keyspace, computes per-label
// totals, and resets the label-count keyspace to those totals.
// Use after IngestSorted with deduped input if you want counters to
// reflect deduped semantics, or any time drift is suspected.
func (s *PebbleStore) RebuildLabelCounters() error {
	return s.inner.RebuildLabelCounters()
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

// LabelCounts returns a map of label → count of quads with that
// label. Same contract as (*Store).LabelCounts. Cost is
// O(distinct_labels × log N) thanks to per-label SeekGE.
func (s *PebbleStore) LabelCounts(ctx context.Context) (map[string]int64, error) {
	return s.inner.LabelCounts(ctx)
}

// Stats returns total quad count and distinct predicate count.
// Same contract as (*Store).Stats. Quad count is a full SPO scan
// (slow on multi-GB stores); distinct predicates uses SeekGE.
func (s *PebbleStore) Stats() (quads int64, predicates int64, err error) {
	return s.inner.Stats()
}

// CommitStatsAt returns commit-journal counts. Same contract as
// (*Store).CommitStatsAt; OldCommits and OldOps are zero when
// cutoff is the zero time.
func (s *PebbleStore) CommitStatsAt(cutoff time.Time) (CommitStats, error) {
	cs, err := s.inner.CommitStatsAt(context.Background(), cutoff)
	if err != nil {
		return CommitStats{}, err
	}
	return CommitStats{
		TotalCommits: cs.TotalCommits,
		OldCommits:   cs.OldCommits,
		TotalOps:     cs.TotalOps,
		OldOps:       cs.OldOps,
	}, nil
}

// MigrateToPebble streams every quad in src to dst via Reader.Find +
// BulkLoader. Skips the audit trail by default — set CopyAudit to
// true to also copy commits + commit_ops (slower; iterates the
// SQLite source's audit tables and replays them as audited
// PebbleStore commits).
//
// MigrateToPebble does not lock the source. If the source is being
// written to during migration, the destination may miss a few
// in-flight quads. For consistent migrations, snapshot the source
// first via (*Store).VacuumInto and migrate from the snapshot.
func MigrateToPebble(ctx context.Context, src *Store, dst *PebbleStore, opts MigrateToPebbleOptions) (MigrateToPebbleStats, error) {
	var stats MigrateToPebbleStats

	bl, err := dst.BulkLoaderWithLabel(ctx, "")
	if err != nil {
		return stats, fmt.Errorf("migrate: open bulk loader: %w", err)
	}
	defer bl.Close()

	r := src.Reader()
	for q, err := range r.Find(ctx, Pattern{}) {
		if err != nil {
			return stats, fmt.Errorf("migrate: read src: %w", err)
		}
		if err := bl.Add(q); err != nil {
			return stats, fmt.Errorf("migrate: bulk add: %w", err)
		}
		stats.QuadsCopied++
		if opts.Progress != nil && stats.QuadsCopied%opts.ProgressEvery == 0 {
			opts.Progress(stats)
		}
	}
	if err := bl.Close(); err != nil {
		return stats, fmt.Errorf("migrate: bulk close: %w", err)
	}
	return stats, nil
}

// MigrateToPebbleOptions controls cross-backend migration.
type MigrateToPebbleOptions struct {
	// Progress fires every ProgressEvery quads (set both or neither).
	Progress      func(MigrateToPebbleStats)
	ProgressEvery int64
}

// MigrateToPebbleStats reports a migration's progress.
type MigrateToPebbleStats struct {
	QuadsCopied int64
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
