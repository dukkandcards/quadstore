package quadstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// BulkLoader is the fast path for initial ingestion or full re-imports.
//
// Trade-offs vs Writer.Commit:
//   - No commit_ops audit trail for data written via BulkLoader.
//   - synchronous=OFF + journal_mode=MEMORY during the load (a crash
//     mid-load loses the in-flight SQLite pages; on Close the file
//     returns to the library's defaults and on the next Open, WAL mode
//     resumes).
//   - Secondary indexes (idx_pos, idx_osp, idx_lsp) are dropped on
//     BulkLoader() and recreated at Close(). The UNIQUE(s,p,o,l)
//     constraint and idx_spo are kept — they are required for the
//     INSERT OR IGNORE dedupe path used by Add().
//   - Multi-row INSERT VALUES batching (bulkBatchRows rows per
//     statement) amortizes prepare/exec overhead.
//
// Holds the store's exclusive writer slot for its lifetime, so
// Writer.Commit() from another goroutine blocks until Close().
type BulkLoader struct {
	store        *Store
	conn         *partitionConn // partition this loader writes to
	ctx          context.Context
	origSync     string
	origJournal  string
	indexDropped bool
	buf          []Quad
	batchSize    int
	labelDefault string
	stats        BulkStats
	closed       bool
	stickyErr    error
}

// BulkStats captures what a BulkLoader wrote.
type BulkStats struct {
	// Added is the number of rows that won the INSERT OR IGNORE race
	// (new unique quads). Measured before the final index rebuild.
	Added int64

	// Attempted is every Add call made, including ones that were
	// ignored as duplicates. Attempted - Added = duplicates.
	Attempted int64

	// Flushes is how many multi-row INSERT statements were executed.
	Flushes int64
}

// bulkBatchRows is the multi-row INSERT VALUES batch size. SQLite's
// default SQLITE_MAX_COMPOUND_SELECT is 500 and SQLITE_MAX_VARIABLE_NUMBER
// is 999 on older builds, 32766 on newer. 500 rows × 4 cols = 2000 vars
// which is safe under both ceilings, and keeps the compiled statement
// reusable across batches.
const bulkBatchRows = 500

// BulkLoader opens a BulkLoader on the store. Blocks until the writer
// slot is free or ctx is cancelled.
//
// The caller MUST call Close() even on error paths — Close restores
// indexes and PRAGMAs and releases the writer slot.
//
// On a partitioned Store, writes go to the default partition; for a
// specific partition use BulkLoaderFor.
func (s *Store) BulkLoader(ctx context.Context) (*BulkLoader, error) {
	return s.BulkLoaderWithLabel(ctx, "")
}

// BulkLoaderFor opens a BulkLoader bound to a specific partition. Useful
// for migration tooling that routes quads externally and wants a direct
// write target without label-based routing. Returns ErrUnknownPartition
// for an unknown partition name.
func (s *Store) BulkLoaderFor(ctx context.Context, name Partition) (*BulkLoader, error) {
	conn, err := s.connFor(name)
	if err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case conn.writerSlot <- struct{}{}:
	}
	bl := &BulkLoader{
		store:     s,
		conn:      conn,
		ctx:       ctx,
		batchSize: bulkBatchRows,
		buf:       make([]Quad, 0, bulkBatchRows),
	}
	if err := bl.enterBulkMode(); err != nil {
		bl.Close()
		return nil, err
	}
	return bl, nil
}

// BulkLoaderWithLabel opens a BulkLoader with a default label applied
// to quads whose own Label is empty. The default must pass label
// validation (same rules as Writer.Commit).
//
// On a partitioned Store, the loader writes to the partition that the
// defaultLabel routes to (or the configured default partition when
// defaultLabel is ""). Every Add must therefore route to the same
// partition; cross-partition adds are rejected at Add time.
func (s *Store) BulkLoaderWithLabel(ctx context.Context, defaultLabel string) (*BulkLoader, error) {
	if defaultLabel != "" {
		if err := validateLabel(defaultLabel); err != nil {
			return nil, err
		}
	}
	conn := s.partFor(defaultLabel)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case conn.writerSlot <- struct{}{}:
	}

	bl := &BulkLoader{
		store:        s,
		conn:         conn,
		ctx:          ctx,
		batchSize:    bulkBatchRows,
		labelDefault: defaultLabel,
		buf:          make([]Quad, 0, bulkBatchRows),
	}

	if err := bl.enterBulkMode(); err != nil {
		bl.Close()
		return nil, err
	}
	return bl, nil
}

// Add buffers a quad. When the buffer reaches the batch size, flushes
// automatically. Returns an error sticky from any prior flush failure.
//
// On a partitioned Store, the quad's effective label must route to the
// loader's partition; cross-partition Adds return ErrCrossPartitionBatch
// without buffering.
func (b *BulkLoader) Add(q Quad) error {
	if b.closed {
		return errors.New("quadstore: bulk loader closed")
	}
	if b.stickyErr != nil {
		return b.stickyErr
	}
	if !q.valid() {
		return fmt.Errorf("quadstore: bulk add: subject/predicate/object required")
	}
	if q.Label == "" {
		q.Label = b.labelDefault
	}
	if b.store.partitioned() {
		if b.store.partFor(q.Label) != b.conn {
			return fmt.Errorf("%w: label %q routes to %q, loader holds %q",
				ErrCrossPartitionBatch, q.Label, b.store.partFor(q.Label).name, b.conn.name)
		}
	}
	b.buf = append(b.buf, q)
	b.stats.Attempted++
	if len(b.buf) >= b.batchSize {
		return b.Flush()
	}
	return nil
}

// Flush writes any buffered quads immediately. Safe to call any time;
// called automatically when buffer fills and on Close.
func (b *BulkLoader) Flush() error {
	if b.closed {
		return errors.New("quadstore: bulk loader closed")
	}
	if b.stickyErr != nil {
		return b.stickyErr
	}
	if len(b.buf) == 0 {
		return nil
	}
	err := b.flushBuf()
	if err != nil {
		b.stickyErr = err
	}
	return err
}

// Close flushes, recreates indexes, restores PRAGMAs, and releases the
// writer slot. Safe to call multiple times. Returns the first error
// encountered.
//
// IMPORTANT: the index rebuild after a large load can take minutes —
// SQLite has to build three B-trees across every row in the table.
// This is expected and part of the bulk trade.
func (b *BulkLoader) Close() error {
	if b.closed {
		return nil
	}
	defer func() {
		// Mark closed and release writer slot AFTER flush/exit so
		// Flush's own closed-check doesn't short-circuit us.
		b.closed = true
		<-b.conn.writerSlot
	}()

	var firstErr error
	if err := b.flushBuf(); err != nil {
		firstErr = err
		b.stickyErr = err
	}
	if err := b.exitBulkMode(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// Stats returns a snapshot of progress.
func (b *BulkLoader) Stats() BulkStats {
	return b.stats
}

// --- internals ---

func (b *BulkLoader) enterBulkMode() error {
	// Capture current PRAGMAs so we can restore on Close. Values come
	// back as ints from the driver; scan into int and format back.
	var syncVal int
	if err := b.conn.db.QueryRowContext(b.ctx, `PRAGMA synchronous`).Scan(&syncVal); err != nil {
		return fmt.Errorf("quadstore: read synchronous: %w", err)
	}
	b.origSync = fmt.Sprintf("%d", syncVal)

	if err := b.conn.db.QueryRowContext(b.ctx, `PRAGMA journal_mode`).Scan(&b.origJournal); err != nil {
		return fmt.Errorf("quadstore: read journal_mode: %w", err)
	}

	// Loosen durability. Any crash mid-load loses the in-flight pages;
	// acceptable because bulk load is idempotent-on-restart.
	stmts := []string{
		`PRAGMA synchronous = OFF`,
		`PRAGMA journal_mode = MEMORY`,
		`PRAGMA cache_size = -2000000`, // 2 GB page cache
		`PRAGMA temp_store = MEMORY`,
		// Drop secondary indexes. Keep UNIQUE(s,p,o,l) + idx_spo —
		// INSERT OR IGNORE needs UNIQUE for dedupe; idx_spo is the
		// hot read path.
		`DROP INDEX IF EXISTS idx_pos`,
		`DROP INDEX IF EXISTS idx_osp`,
		`DROP INDEX IF EXISTS idx_lsp`,
	}
	for _, s := range stmts {
		if _, err := b.conn.db.ExecContext(b.ctx, s); err != nil {
			return fmt.Errorf("quadstore: enterBulkMode %q: %w", s, err)
		}
	}
	b.indexDropped = true
	return nil
}

func (b *BulkLoader) exitBulkMode() error {
	var firstErr error

	if b.indexDropped {
		// Rebuild the secondary indexes. Expensive but one-shot.
		idx := []string{
			`CREATE INDEX IF NOT EXISTS idx_pos ON quads(predicate, object, subject)`,
			`CREATE INDEX IF NOT EXISTS idx_osp ON quads(object, subject, predicate)`,
			`CREATE INDEX IF NOT EXISTS idx_lsp ON quads(label, subject, predicate)`,
		}
		for _, s := range idx {
			if _, err := b.conn.db.ExecContext(b.ctx, s); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("quadstore: recreate index %q: %w", s, err)
			}
		}
		b.indexDropped = false
	}

	// Restore PRAGMAs. Errors here are best-effort.
	if b.origJournal != "" {
		_, _ = b.conn.db.ExecContext(b.ctx, `PRAGMA journal_mode = `+b.origJournal)
	}
	if b.origSync != "" {
		_, _ = b.conn.db.ExecContext(b.ctx, `PRAGMA synchronous = `+b.origSync)
	}

	return firstErr
}

func (b *BulkLoader) flushBuf() error {
	if len(b.buf) == 0 {
		return nil
	}

	// Build a single multi-row INSERT OR IGNORE.
	var sb strings.Builder
	sb.Grow(64 + len(b.buf)*12)
	sb.WriteString(`INSERT OR IGNORE INTO quads (subject, predicate, object, label) VALUES `)
	args := make([]any, 0, len(b.buf)*4)
	for i, q := range b.buf {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`(?,?,?,?)`)
		args = append(args, q.Subject, q.Predicate, q.Object, q.Label)
	}

	res, err := b.conn.db.ExecContext(b.ctx, sb.String(), args...)
	if err != nil {
		return fmt.Errorf("quadstore: bulk insert %d rows: %w", len(b.buf), err)
	}
	n, _ := res.RowsAffected()
	b.stats.Added += n
	b.stats.Flushes++
	b.buf = b.buf[:0]
	return nil
}
