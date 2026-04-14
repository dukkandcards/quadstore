package quadstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Partition is an opaque routing key. Today unused (single backing
// file); reserved for per-partition backing stores (Rung 2 of the
// concurrent-writer evolution ladder).
type Partition string

// Batch is the atomic unit of Writer.Commit. Adds and Removes apply
// together or not at all. Label is the default label applied to Adds
// whose Quad.Label is empty. Metadata is commit-level provenance; see
// well-known keys (MetaActor, MetaSource, MetaReason). Any additional
// keys are accepted.
type Batch struct {
	Adds     []Quad
	Removes  []Quad
	Label    string
	Metadata map[string]string
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

// Writer is a single-producer write handle. Obtained from Store.Writer
// or Store.WriterFor; one active at a time per Store. A failed Commit
// rolls back and leaves the Writer usable for retry. Close releases
// the writer slot.
type Writer struct {
	store  *Store
	closed bool
}

// Writer returns a Writer, blocking until the writer slot is free or
// ctx is cancelled.
func (s *Store) Writer(ctx context.Context) (*Writer, error) {
	return s.WriterFor(ctx, "")
}

// WriterFor returns a Writer for the given Partition. Today p is ignored
// (single backing file); reserved for partitioned routing (Rung 2).
func (s *Store) WriterFor(ctx context.Context, _ Partition) (*Writer, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case s.writerSlot <- struct{}{}:
		return &Writer{store: s}, nil
	}
}

// Close releases the writer slot. Safe to call multiple times.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	<-w.store.writerSlot
	return nil
}

// Commit applies a Batch atomically. On error, the transaction is
// rolled back and the Writer remains usable for retry. Commit after
// Close returns an error.
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
	}
	for i, q := range b.Removes {
		if !q.valid() {
			return fmt.Errorf("quadstore: Removes[%d] missing subject/predicate/object", i)
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

	tx, err := w.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after tx.Commit succeeds

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO commits (id, created_at, label, metadata) VALUES (?, ?, ?, ?)`,
		commitID.String(), time.Now().Unix(), b.Label, metadataJSON,
	); err != nil {
		return fmt.Errorf("quadstore: insert commit: %w", err)
	}

	addQuad, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO quads (subject, predicate, object, label) VALUES (?, ?, ?, ?)`,
	)
	if err != nil {
		return err
	}
	defer addQuad.Close()

	logOp, err := tx.PrepareContext(ctx,
		`INSERT INTO commit_ops (commit_id, op, subject, predicate, object, label) VALUES (?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return err
	}
	defer logOp.Close()

	removeQuad, err := tx.PrepareContext(ctx,
		`DELETE FROM quads WHERE subject = ? AND predicate = ? AND object = ? AND label = ?`,
	)
	if err != nil {
		return err
	}
	defer removeQuad.Close()

	for _, q := range b.Adds {
		label := q.Label
		if label == "" {
			label = b.Label
		}
		if _, err := addQuad.ExecContext(ctx, q.Subject, q.Predicate, q.Object, label); err != nil {
			return fmt.Errorf("quadstore: add quad: %w", err)
		}
		if _, err := logOp.ExecContext(ctx, commitID.String(), "add", q.Subject, q.Predicate, q.Object, label); err != nil {
			return fmt.Errorf("quadstore: log add op: %w", err)
		}
	}
	for _, q := range b.Removes {
		if _, err := removeQuad.ExecContext(ctx, q.Subject, q.Predicate, q.Object, q.Label); err != nil {
			return fmt.Errorf("quadstore: remove quad: %w", err)
		}
		if _, err := logOp.ExecContext(ctx, commitID.String(), "remove", q.Subject, q.Predicate, q.Object, q.Label); err != nil {
			return fmt.Errorf("quadstore: log remove op: %w", err)
		}
	}

	return tx.Commit()
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
