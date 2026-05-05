// Migrate copies a single-file or partitioned Store into a partitioned
// Store, routing each quad / commit / commit_op via the destination's
// configured RouteLabel function.
//
// Designed for one-time upgrades from a legacy single-file Store to a
// partitioned layout — the typical use case is: open the legacy DB
// read-only, open the new partitioned DB, call Migrate, swap the
// consumer's open path. The source DB is never modified.

package quadstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// MigrateOptions configures a Migrate run.
type MigrateOptions struct {
	// ChunkSize is the number of quads buffered before flushing a
	// per-partition batch. Defaults to 10,000 if zero.
	ChunkSize int

	// CopyCommits selects whether the commits + commit_ops audit trail
	// is copied alongside the quads. Defaults true. Setting false skips
	// the audit copy and produces a quads-only destination — ~50% faster
	// but loses provenance for migrated data.
	CopyCommits bool

	// OnlySince, if non-zero, restricts migration to commits created at
	// or after the given time (and the quads attached to those commits).
	// Used for incremental top-up after an initial bulk migration.
	//
	// When OnlySince is non-zero, only quads that appear in `commit_ops`
	// for an in-window commit are copied; quads in the source that have
	// no commit_ops record (legacy Add / AddBatch / BulkLoader writes)
	// are skipped. For the first migration, leave OnlySince zero so
	// every quads-table row is copied regardless of audit history.
	OnlySince time.Time

	// Progress receives per-partition progress updates every ChunkSize
	// quads. Optional.
	Progress func(MigrateProgress)
}

// MigrateProgress is one progress event during Migrate.
type MigrateProgress struct {
	Partition       Partition
	QuadsCopiedSoFar int64
	Phase           string // "quads" | "commits" | "ops"
}

// MigrateStats is the result of a Migrate run.
type MigrateStats struct {
	QuadsCopied   int64
	CommitsCopied int64
	OpsCopied     int64
	Duration      time.Duration
	PerPartition  map[Partition]int64 // quads copied per destination partition
}

// ErrDestinationNotPartitioned is returned when Migrate is called with a
// non-partitioned destination Store.
var ErrDestinationNotPartitioned = errors.New("quadstore: migrate destination must be partitioned (use OpenPartitioned)")

// Migrate copies all quads (and optionally commits + commit_ops) from
// src into dst. dst must be a partitioned Store; src can be either
// single-file or partitioned (a re-partition is allowed if the routing
// changed). The src Store is read-only throughout — Migrate never writes
// to src.
//
// Pre-condition for a clean migration: dst's partitions should be empty.
// INSERT OR IGNORE makes re-runs safe but produces redundant work; for
// incremental top-ups use OnlySince.
//
// Migrate opens one BulkLoader per destination partition and streams
// from src in a single pass, routing each quad via dst's RouteLabel.
// The per-partition writer slots are independent, so the BulkLoaders
// can hold them concurrently.
func Migrate(ctx context.Context, src, dst *Store, opts MigrateOptions) (MigrateStats, error) {
	if !dst.partitioned() {
		return MigrateStats{}, ErrDestinationNotPartitioned
	}
	if dst.routeLabel == nil {
		return MigrateStats{}, ErrMissingRouter
	}
	if opts.ChunkSize <= 0 {
		opts.ChunkSize = 10000
	}
	stats := MigrateStats{
		PerPartition: make(map[Partition]int64),
	}
	t0 := time.Now()

	loaders := make(map[Partition]*BulkLoader, len(dst.parts))
	for _, p := range dst.parts {
		bl, err := dst.BulkLoaderFor(ctx, p.name)
		if err != nil {
			closeLoaders(loaders)
			return stats, fmt.Errorf("quadstore migrate: open loader %s: %w", p.name, err)
		}
		// Do NOT override bl.batchSize. BulkLoader's default
		// (bulkBatchRows = 500) is sized for SQLite's
		// SQLITE_MAX_VARIABLE_NUMBER ceiling: 500 rows × 4 cols = 2000
		// vars, well under the 32766 modernc.org/sqlite limit. A naive
		// override (e.g. ChunkSize = 10000) produces 40000 vars and
		// fails with `SQL logic error: too many SQL variables`.
		// opts.ChunkSize controls progress-reporting cadence, NOT
		// per-INSERT row count.
		loaders[p.name] = bl
	}
	// Ensure all loaders close on any exit path so SQLite indexes get
	// rebuilt and writer slots are released.
	defer func() {
		for _, bl := range loaders {
			_ = bl.Close()
		}
	}()

	hasSince := !opts.OnlySince.IsZero()
	sinceUnix := opts.OnlySince.Unix()

	// --- Phase 1: copy quads ---
	for _, srcConn := range src.parts {
		var (
			rows *sql.Rows
			err  error
		)
		if hasSince {
			// Only quads whose subject/predicate/object/label appears in
			// at least one commit_op for an in-window commit. Joining
			// against the (commit_id, op, subject, predicate, object,
			// label) shape of commit_ops with DISTINCT.
			rows, err = srcConn.db.QueryContext(ctx, `
				SELECT DISTINCT q.subject, q.predicate, q.object, q.label
				FROM quads q
				JOIN commit_ops co
				  ON co.subject = q.subject
				 AND co.predicate = q.predicate
				 AND co.object = q.object
				 AND co.label = q.label
				JOIN commits c
				  ON c.id = co.commit_id
				WHERE co.op = 'add'
				  AND c.created_at >= ?
			`, sinceUnix)
		} else {
			rows, err = srcConn.db.QueryContext(ctx,
				`SELECT subject, predicate, object, label FROM quads`)
		}
		if err != nil {
			return stats, fmt.Errorf("quadstore migrate: scan src quads %s: %w", srcConn.name, err)
		}
		if err := streamQuads(ctx, rows, dst, loaders, &stats, opts.Progress); err != nil {
			return stats, err
		}
	}

	// Flush all per-partition buffers before moving on so progress numbers
	// are accurate. Loaders stay open for any phase 2 / 3 inserts via
	// raw SQL on dst's connections (commits + commit_ops, below).
	for _, bl := range loaders {
		if err := bl.Flush(); err != nil {
			return stats, fmt.Errorf("quadstore migrate: flush loader %s: %w", bl.conn.name, err)
		}
	}

	// --- Phase 2: copy commits + commit_ops (optional) ---
	if opts.CopyCommits {
		if err := copyAudit(ctx, src, dst, opts, &stats); err != nil {
			return stats, err
		}
	}

	stats.Duration = time.Since(t0)
	return stats, nil
}

// streamQuads consumes rows of (subject, predicate, object, label) from
// src and routes each to the destination partition's BulkLoader.
func streamQuads(
	ctx context.Context,
	rows *sql.Rows,
	dst *Store,
	loaders map[Partition]*BulkLoader,
	stats *MigrateStats,
	progress func(MigrateProgress),
) error {
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var q Quad
		if err := rows.Scan(&q.Subject, &q.Predicate, &q.Object, &q.Label); err != nil {
			return fmt.Errorf("quadstore migrate: scan quad: %w", err)
		}
		target := dst.routeLabel(q.Label)
		if target == "" {
			target = dst.defaultName
		}
		bl, ok := loaders[target]
		if !ok {
			return fmt.Errorf("%w: %s", ErrUnknownPartition, target)
		}
		if err := bl.Add(q); err != nil {
			return fmt.Errorf("quadstore migrate: add to %s: %w", target, err)
		}
		stats.QuadsCopied++
		stats.PerPartition[target]++
		// Progress cadence is decoupled from BulkLoader.batchSize so
		// the SQLite-variable-limit fix above does not flood the log.
		// Default to a 10000-quad cadence for visibility on multi-
		// million-quad runs without spamming.
		const progressEvery = 10000
		if progress != nil && stats.QuadsCopied%progressEvery == 0 {
			progress(MigrateProgress{
				Partition:        target,
				QuadsCopiedSoFar: stats.QuadsCopied,
				Phase:            "quads",
			})
		}
	}
	return rows.Err()
}

// copyAudit copies commits + commit_ops from src to dst, routing each
// commit to a single destination partition by the LABEL on the commit
// itself (commits.label). This preserves provenance — the audit trail
// of any single commit lives in exactly one destination partition.
//
// Commits whose label is "" (no label set on the commit row) route to
// dst.defaultName.
func copyAudit(
	ctx context.Context,
	src, dst *Store,
	opts MigrateOptions,
	stats *MigrateStats,
) error {
	hasSince := !opts.OnlySince.IsZero()
	sinceUnix := opts.OnlySince.Unix()
	for _, srcConn := range src.parts {
		var (
			cRows *sql.Rows
			err   error
		)
		if hasSince {
			cRows, err = srcConn.db.QueryContext(ctx,
				`SELECT id, created_at, label, metadata FROM commits WHERE created_at >= ?`,
				sinceUnix)
		} else {
			cRows, err = srcConn.db.QueryContext(ctx,
				`SELECT id, created_at, label, metadata FROM commits`)
		}
		if err != nil {
			return fmt.Errorf("quadstore migrate: scan commits %s: %w", srcConn.name, err)
		}
		// Pre-collect commit IDs per partition so the commit_ops phase
		// can route them in a second pass without a second scan of commits.
		type commitRow struct {
			id        string
			createdAt int64
			label     string
			metadata  sql.NullString
		}
		commits := make(map[Partition][]commitRow)
		commitToPart := make(map[string]Partition)
		for cRows.Next() {
			var c commitRow
			if err := cRows.Scan(&c.id, &c.createdAt, &c.label, &c.metadata); err != nil {
				cRows.Close()
				return fmt.Errorf("quadstore migrate: scan commit row: %w", err)
			}
			target := dst.routeLabel(c.label)
			if target == "" {
				target = dst.defaultName
			}
			commits[target] = append(commits[target], c)
			commitToPart[c.id] = target
		}
		cRows.Close()
		if err := cRows.Err(); err != nil {
			return err
		}

		// Insert commits per partition.
		for partition, list := range commits {
			conn := dst.byName[partition]
			tx, err := conn.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("quadstore migrate: begin commits tx %s: %w", partition, err)
			}
			stmt, err := tx.PrepareContext(ctx,
				`INSERT OR IGNORE INTO commits (id, created_at, label, metadata) VALUES (?, ?, ?, ?)`)
			if err != nil {
				tx.Rollback()
				return fmt.Errorf("quadstore migrate: prepare commits %s: %w", partition, err)
			}
			for _, c := range list {
				var meta any
				if c.metadata.Valid {
					meta = c.metadata.String
				}
				if _, err := stmt.ExecContext(ctx, c.id, c.createdAt, c.label, meta); err != nil {
					stmt.Close()
					tx.Rollback()
					return fmt.Errorf("quadstore migrate: insert commit %s: %w", partition, err)
				}
				stats.CommitsCopied++
			}
			stmt.Close()
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("quadstore migrate: commit commits tx %s: %w", partition, err)
			}
		}

		// Stream commit_ops, route by commit_id.
		var opRows *sql.Rows
		if hasSince {
			opRows, err = srcConn.db.QueryContext(ctx, `
				SELECT co.commit_id, co.op, co.subject, co.predicate, co.object, co.label
				FROM commit_ops co
				JOIN commits c ON c.id = co.commit_id
				WHERE c.created_at >= ?
			`, sinceUnix)
		} else {
			opRows, err = srcConn.db.QueryContext(ctx,
				`SELECT commit_id, op, subject, predicate, object, label FROM commit_ops`)
		}
		if err != nil {
			return fmt.Errorf("quadstore migrate: scan commit_ops %s: %w", srcConn.name, err)
		}
		// Per-partition transaction with periodic checkpoints. Keep it
		// simple: one tx per partition for the whole stream, prepared
		// once. For a 1.87 M commit_ops table this is still manageable
		// inside one transaction.
		txByPart := make(map[Partition]*sql.Tx)
		stmtByPart := make(map[Partition]*sql.Stmt)
		closeAll := func() {
			for _, st := range stmtByPart {
				st.Close()
			}
			for _, tx := range txByPart {
				tx.Rollback() // no-op if already committed
			}
		}
		for opRows.Next() {
			if err := ctx.Err(); err != nil {
				opRows.Close()
				closeAll()
				return err
			}
			var commitID, op, subj, pred, obj, label string
			if err := opRows.Scan(&commitID, &op, &subj, &pred, &obj, &label); err != nil {
				opRows.Close()
				closeAll()
				return fmt.Errorf("quadstore migrate: scan op row: %w", err)
			}
			target, ok := commitToPart[commitID]
			if !ok {
				// Op references a commit we didn't pick up — out of
				// window or DB inconsistency. Skip silently; commits
				// table is the canonical record of what's in scope.
				continue
			}
			tx, ok := txByPart[target]
			if !ok {
				conn := dst.byName[target]
				tx, err = conn.db.BeginTx(ctx, nil)
				if err != nil {
					opRows.Close()
					closeAll()
					return fmt.Errorf("quadstore migrate: begin ops tx %s: %w", target, err)
				}
				stmt, err := tx.PrepareContext(ctx,
					`INSERT INTO commit_ops (commit_id, op, subject, predicate, object, label) VALUES (?, ?, ?, ?, ?, ?)`)
				if err != nil {
					opRows.Close()
					tx.Rollback()
					closeAll()
					return fmt.Errorf("quadstore migrate: prepare ops %s: %w", target, err)
				}
				txByPart[target] = tx
				stmtByPart[target] = stmt
			}
			if _, err := stmtByPart[target].ExecContext(ctx, commitID, op, subj, pred, obj, label); err != nil {
				opRows.Close()
				closeAll()
				return fmt.Errorf("quadstore migrate: insert op %s: %w", target, err)
			}
			stats.OpsCopied++
		}
		opRows.Close()
		if err := opRows.Err(); err != nil {
			closeAll()
			return err
		}
		// Commit per-partition op transactions.
		for partition, tx := range txByPart {
			stmtByPart[partition].Close()
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("quadstore migrate: commit ops tx %s: %w", partition, err)
			}
		}
	}
	return nil
}

// closeLoaders closes every BulkLoader in the map. Used for the
// failure-cleanup path in Migrate.
func closeLoaders(loaders map[Partition]*BulkLoader) {
	for _, bl := range loaders {
		_ = bl.Close()
	}
}

// SnapshotOptions configures MigrateFromSnapshot.
type SnapshotOptions struct {
	// SnapshotPath is where VACUUM INTO writes the consistent snapshot
	// of srcPath. Must not exist when MigrateFromSnapshot starts (the
	// SQLite VACUUM INTO statement refuses to overwrite an existing file).
	// Required.
	SnapshotPath string

	// KeepSnapshot, when true, leaves the snapshot file in place after
	// migration completes. Useful for verification or as an audit
	// artifact. When false (default), the snapshot is deleted after the
	// migration's post-verification step.
	KeepSnapshot bool

	// Migrate carries the per-quad migration options (ChunkSize,
	// CopyCommits, OnlySince, Progress). Forwarded to Migrate after the
	// snapshot is taken.
	Migrate MigrateOptions
}

// SnapshotStats reports on a MigrateFromSnapshot run.
type SnapshotStats struct {
	SnapshotDuration time.Duration // VACUUM INTO wall time
	SnapshotBytes    int64         // size of the snapshot file in bytes
	MigrateStats                   // embedded from the underlying Migrate call
}

// MigrateFromSnapshot is the race-safe counterpart to Migrate. It first
// takes a consistent point-in-time snapshot of srcPath using SQLite's
// `VACUUM INTO` statement, then migrates from that frozen snapshot into
// dst. Concurrent writers on srcPath are explicitly supported by
// SQLite — VACUUM INTO operates on a private working copy and does not
// require an exclusive lock on the source.
//
// Why this matters: a long-running Migrate against a live source can
// observe a torn snapshot when concurrent writers add/remove rows
// mid-stream. With INSERT OR IGNORE on dst this is non-fatal but can
// undercount derived state — e.g., if the source's daily refresh job
// `[remove]`s a derived label and is mid-rebuild when Migrate scans,
// the migration captures fewer rows than expected. MigrateFromSnapshot
// removes that surface by decoupling the read from any writer activity.
//
// Pure-Go: VACUUM INTO is a SQL statement supported by
// modernc.org/sqlite. No external sqlite3 CLI tool required.
//
// srcPath must be a single-file Store (not a partitioned root). For
// re-partitioning a partitioned source, use Migrate directly with both
// stores opened simultaneously — partitioned-to-partitioned migration
// is a separate operation and would require per-partition snapshots.
//
// The destination dst must be a partitioned Store (same precondition as
// Migrate).
func MigrateFromSnapshot(
	ctx context.Context,
	srcPath string,
	dst *Store,
	opts SnapshotOptions,
) (SnapshotStats, error) {
	if opts.SnapshotPath == "" {
		return SnapshotStats{}, errors.New("quadstore: SnapshotPath is required")
	}
	if !dst.partitioned() {
		return SnapshotStats{}, ErrDestinationNotPartitioned
	}

	var stats SnapshotStats
	t0 := time.Now()

	// Phase 1: VACUUM INTO produces a consistent snapshot file.
	// Concurrent writers on srcPath are fine per SQLite docs:
	// https://sqlite.org/lang_vacuum.html
	if err := vacuumInto(ctx, srcPath, opts.SnapshotPath); err != nil {
		return stats, fmt.Errorf("quadstore: snapshot %s -> %s: %w", srcPath, opts.SnapshotPath, err)
	}
	stats.SnapshotDuration = time.Since(t0)

	// Capture snapshot file size for reporting.
	if fi, err := os.Stat(opts.SnapshotPath); err == nil {
		stats.SnapshotBytes = fi.Size()
	}

	// Phase 2: open the snapshot read-only and migrate from it.
	snapshot, err := Open(opts.SnapshotPath)
	if err != nil {
		return stats, fmt.Errorf("quadstore: open snapshot: %w", err)
	}
	defer snapshot.Close()

	migrateStats, err := Migrate(ctx, snapshot, dst, opts.Migrate)
	stats.MigrateStats = migrateStats
	if err != nil {
		return stats, err
	}

	// Phase 3: cleanup unless caller wants to keep the snapshot.
	if !opts.KeepSnapshot {
		snapshot.Close()
		if err := os.Remove(opts.SnapshotPath); err != nil {
			return stats, fmt.Errorf("quadstore: remove snapshot: %w", err)
		}
	}

	return stats, nil
}

// vacuumInto runs `VACUUM INTO 'dest'` on srcPath. Opens its own
// connection to srcPath so callers don't need an open *Store handle.
func vacuumInto(ctx context.Context, srcPath, dstPath string) error {
	db, err := sql.Open("sqlite", srcPath+"?_pragma=busy_timeout(60000)")
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer db.Close()

	// Escape single quotes in dstPath for the SQL string literal. SQLite
	// has no parameterized form for VACUUM INTO; the path is part of the
	// SQL syntax, not a value binding.
	quoted := "'" + strings.ReplaceAll(dstPath, "'", "''") + "'"
	_, err = db.ExecContext(ctx, "VACUUM INTO "+quoted)
	if err != nil {
		return fmt.Errorf("VACUUM INTO: %w", err)
	}
	return nil
}
