package quadstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Batch is the atomic unit of Writer.Commit. Adds and Removes apply
// together or not at all. Label is the default label applied to Adds
// whose Quad.Label is empty. Metadata is commit-level provenance; see
// well-known keys (MetaActor, MetaSource, MetaReason). Any additional
// keys are accepted.
//
// On a partitioned Store, every quad in a Batch must route to the same
// partition; otherwise Writer.Commit returns ErrCrossPartitionBatch.
//
// **Adds-then-Removes ordering** — within a single Commit, Adds are
// applied BEFORE Removes (the SQLite path uses INSERT OR IGNORE then
// DELETE; the Pebble path mirrors this). The consequence: if the same
// (subject, predicate, object, label) tuple appears in BOTH the Adds
// and Removes lists, the net effect is REMOVE (the Add is INSERT OR
// IGNORE'd as a duplicate, then the Remove deletes the existing row).
// If the intent is "replace stale set with current set", the caller
// must diff first and only put quads that are leaving in Removes:
//
//	keep := map[string]bool{}
//	for _, q := range newQuads {
//	    keep[key(q)] = true
//	}
//	for _, q := range existing {
//	    if !keep[key(q)] {
//	        removes = append(removes, q)
//	    }
//	}
//
// Surfaced 2026-05-10 in secdek's force-reemit backfill where every
// surviving slug got deleted because it was naively included in both
// lists. Caller-side diff is the right fix; flipping the apply order
// inside the writer would silently break any caller that relies on
// the current ordering.
//
// NoAudit, when true, suppresses the per-Commit `commits` row and the
// per-quad `commit_ops` rows. Label validation, partition routing, and
// the actual `quads` writes still happen, and the whole batch is still
// atomic. Use this for high-throughput workloads (~3× faster on
// single-quad commits) where you don't need the audit trail. The
// tradeoff: writes performed with NoAudit are invisible to
// Reader.Commits, do not appear in Migrate(CopyCommits=true) output,
// and cannot be tailed via the commits table. If unsure, leave it
// false — the audit trail is the point of using Writer.Commit over
// BulkLoader.
type Batch struct {
	Adds     []Quad
	Removes  []Quad
	Label    string
	Metadata map[string]string
	NoAudit  bool
}

// Well-known Metadata keys. Callers may add any keys; these are
// documented conventions, not enforcement.
const (
	MetaActor  = "actor"
	MetaSource = "source"
	MetaReason = "reason"
)

// validLabelPrefixes are the enforced label namespaces on Writer.Commit.
// Legacy Add/AddBatch/Delete remain permissive. Empty label is also valid.
var validLabelPrefixes = []string{"source:", "derived:", "human:", "meta:"}

// Writer is a single-producer write handle for one partition. Obtained
// from Store.Writer (default partition) or Store.WriterFor (named
// partition). One active Writer at a time per partition; concurrent
// Writers across different partitions are allowed and independent.
//
// A failed Commit rolls back and leaves the Writer usable for retry.
// Close releases the writer slot.
type Writer struct {
	store  *Store
	conn   *partitionConn // partition this Writer owns the slot of
	closed bool
}

// Writer returns a Writer for the default partition, blocking until the
// default partition's writer slot is free or ctx is cancelled.
func (s *Store) Writer(ctx context.Context) (*Writer, error) {
	return s.WriterFor(ctx, "")
}

// WriterFor returns a Writer for the named partition, blocking until that
// partition's writer slot is free or ctx is cancelled.
//
// An empty Partition routes to the default partition. An unknown name
// returns ErrUnknownPartition immediately without blocking.
func (s *Store) WriterFor(ctx context.Context, p Partition) (*Writer, error) {
	conn, err := s.connFor(p)
	if err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case conn.writerSlot <- struct{}{}:
		return &Writer{store: s, conn: conn}, nil
	}
}

// connFor resolves a partition name to its connection. Empty name maps
// to the default. Used by WriterFor and admin commands.
func (s *Store) connFor(p Partition) (*partitionConn, error) {
	if p == "" {
		// Single-file Store has parts[0] with name "" — unique mapping.
		// Multi-partition Store has a configured Default.
		if !s.partitioned() {
			return s.parts[0], nil
		}
		return s.byName[s.defaultName], nil
	}
	if conn, ok := s.byName[p]; ok {
		return conn, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrUnknownPartition, p)
}

// Partition returns the partition name this Writer owns the slot of.
// "" for single-file Stores.
func (w *Writer) Partition() Partition {
	return w.conn.name
}

// Close releases the writer slot. Safe to call multiple times.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	<-w.conn.writerSlot
	return nil
}

// Commit applies a Batch atomically to the Writer's partition. On error,
// the transaction is rolled back and the Writer remains usable for retry.
//
// On a partitioned Store, validates every quad routes to this Writer's
// partition. Returns ErrCrossPartitionBatch if any quad routes elsewhere;
// the caller splits the batch by partition and acquires the right Writer
// for each.
//
// Commit after Close returns an error.
func (w *Writer) Commit(ctx context.Context, b Batch) error {
	if w.closed {
		return errors.New("quadstore: writer closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Validate before touching the database.
	if b.Label != "" {
		if err := validateLabel(b.Label); err != nil {
			return err
		}
	}
	for i, q := range b.Adds {
		label := q.Label
		if label == "" {
			label = b.Label
		}
		if err := validateLabel(label); err != nil {
			return fmt.Errorf("quadstore: Adds[%d]: %w", i, err)
		}
		if !q.valid() {
			return fmt.Errorf("quadstore: Adds[%d] missing subject/predicate/object", i)
		}
		if w.store.partitioned() {
			if w.store.partFor(label) != w.conn {
				return fmt.Errorf("%w: Adds[%d] label %q routes to partition %q, writer holds %q",
					ErrCrossPartitionBatch, i, label, w.store.partFor(label).name, w.conn.name)
			}
		}
	}
	for i, q := range b.Removes {
		if !q.valid() {
			return fmt.Errorf("quadstore: Removes[%d] missing subject/predicate/object", i)
		}
		if w.store.partitioned() {
			if w.store.partFor(q.Label) != w.conn {
				return fmt.Errorf("%w: Removes[%d] label %q routes to partition %q, writer holds %q",
					ErrCrossPartitionBatch, i, q.Label, w.store.partFor(q.Label).name, w.conn.name)
			}
		}
	}

	commitID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("quadstore: generate commit id: %w", err)
	}
	var metadataJSON any
	if len(b.Metadata) > 0 {
		buf, err := json.Marshal(b.Metadata)
		if err != nil {
			return fmt.Errorf("quadstore: marshal metadata: %w", err)
		}
		metadataJSON = string(buf)
	}

	tx, err := w.conn.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after tx.Commit succeeds

	if !b.NoAudit {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO commits (id, created_at, label, metadata) VALUES (?, ?, ?, ?)`,
			commitID.String(), time.Now().Unix(), b.Label, metadataJSON,
		); err != nil {
			return fmt.Errorf("quadstore: insert commit: %w", err)
		}
	}

	// Only prepare statements we'll actually use. A common shape is
	// "Adds-only, no Removes" — preparing the DELETE in that case is
	// pure waste. Saves ~5-10 µs per Commit on the hot single-add path.
	// logOp is only needed when audit is on AND we have writes.
	var (
		addQuad    *sql.Stmt
		removeQuad *sql.Stmt
		logOp      *sql.Stmt
	)
	if !b.NoAudit && (len(b.Adds) > 0 || len(b.Removes) > 0) {
		logOp, err = tx.PrepareContext(ctx,
			`INSERT INTO commit_ops (commit_id, op, subject, predicate, object, label) VALUES (?, ?, ?, ?, ?, ?)`,
		)
		if err != nil {
			return err
		}
		defer logOp.Close()
	}
	if len(b.Adds) > 0 {
		addQuad, err = tx.PrepareContext(ctx,
			`INSERT OR IGNORE INTO quads (subject, predicate, object, label) VALUES (?, ?, ?, ?)`,
		)
		if err != nil {
			return err
		}
		defer addQuad.Close()
	}
	if len(b.Removes) > 0 {
		removeQuad, err = tx.PrepareContext(ctx,
			`DELETE FROM quads WHERE subject = ? AND predicate = ? AND object = ? AND label = ?`,
		)
		if err != nil {
			return err
		}
		defer removeQuad.Close()
	}

	for _, q := range b.Adds {
		label := q.Label
		if label == "" {
			label = b.Label
		}
		if _, err := addQuad.ExecContext(ctx, q.Subject, q.Predicate, q.Object, label); err != nil {
			return fmt.Errorf("quadstore: add quad: %w", err)
		}
		if logOp != nil {
			if _, err := logOp.ExecContext(ctx, commitID.String(), "add", q.Subject, q.Predicate, q.Object, label); err != nil {
				return fmt.Errorf("quadstore: log add op: %w", err)
			}
		}
	}
	for _, q := range b.Removes {
		if _, err := removeQuad.ExecContext(ctx, q.Subject, q.Predicate, q.Object, q.Label); err != nil {
			return fmt.Errorf("quadstore: remove quad: %w", err)
		}
		if logOp != nil {
			if _, err := logOp.ExecContext(ctx, commitID.String(), "remove", q.Subject, q.Predicate, q.Object, q.Label); err != nil {
				return fmt.Errorf("quadstore: log remove op: %w", err)
			}
		}
	}

	return tx.Commit()
}

// PruneOps deletes rows from commit_ops for commits whose created_at is
// strictly before olderThan, on this Writer's partition. The commits
// rows themselves are preserved.
//
// On a partitioned Store, this only prunes the partition the Writer
// owns. To prune all partitions, iterate Store.Partitions and acquire
// a Writer for each. (Sequential admin commands avoid IOPS contention
// on a shared disk.)
func (w *Writer) PruneOps(ctx context.Context, olderThan time.Time) (int64, error) {
	if w.closed {
		return 0, errors.New("quadstore: writer closed")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	cutoff := olderThan.Unix()
	res, err := w.conn.db.ExecContext(ctx,
		`DELETE FROM commit_ops
		 WHERE commit_id IN (SELECT id FROM commits WHERE created_at < ?)`,
		cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("quadstore: prune commit_ops: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("quadstore: prune rows affected: %w", err)
	}
	return n, nil
}

func validateLabel(label string) error {
	if label == "" {
		return nil
	}
	for _, prefix := range validLabelPrefixes {
		if strings.HasPrefix(label, prefix) {
			return nil
		}
	}
	return fmt.Errorf(
		"quadstore: label %q must start with one of: %s",
		label, strings.Join(validLabelPrefixes, ", "),
	)
}
