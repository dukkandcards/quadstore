package quadstore

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// Side-by-side benchmarks: quadstore library vs hand-rolled raw SQLite
// (same modernc.org/sqlite driver, no library overhead). Establishes
// the cost of using the library over rolling your own quad table.
//
// These are intentionally apples-to-apples: same driver, same schema
// shape (quads with subject, predicate, object, label, primary index),
// same workload. Differences come from the library's commit-row writes,
// label validation, and per-Writer plumbing.
//
// Run with:
//
//	go test -bench='Compare' -benchtime=2s ./...

// Default raw SQLite PRAGMAs match quadstore's defaults (WAL + NORMAL):
// the realistic baseline for a careful Go author writing their own
// quad table on modernc.org/sqlite.
const rawSQLiteSchema = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
CREATE TABLE IF NOT EXISTS quads (
    subject   TEXT NOT NULL,
    predicate TEXT NOT NULL,
    object    TEXT NOT NULL,
    label     TEXT NOT NULL,
    PRIMARY KEY (label, subject, predicate, object)
);
CREATE INDEX IF NOT EXISTS idx_subject ON quads(subject);
`

// Bulk-tuned PRAGMAs mirror what quadstore's BulkLoader applies
// internally during a load (synchronous=OFF, journal=MEMORY, large
// cache). This is the upper-bound the driver itself can hit when
// durability is relaxed for the duration of a bulk import.
const rawSQLiteBulkPragmas = `
PRAGMA synchronous = OFF;
PRAGMA journal_mode = MEMORY;
PRAGMA cache_size = -2000000;
PRAGMA temp_store = MEMORY;
`

func tempRawSQLite(b *testing.B) *sql.DB {
	b.Helper()
	dir := b.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "raw.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	if _, err := db.Exec(rawSQLiteSchema); err != nil {
		b.Fatal(err)
	}
	return db
}

func tempRawSQLiteBulkTuned(b *testing.B) *sql.DB {
	b.Helper()
	db := tempRawSQLite(b)
	if _, err := db.Exec(rawSQLiteBulkPragmas); err != nil {
		b.Fatal(err)
	}
	return db
}

// Single-quad commit, raw SQLite (one INSERT per call).
func BenchmarkCompare_RawSQLite_SingleInsert(b *testing.B) {
	db := tempRawSQLite(b)
	stmt, err := db.Prepare(`INSERT OR IGNORE INTO quads(subject, predicate, object, label) VALUES (?, ?, ?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	defer stmt.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("s%d", i), "p", "o", "source:bench"); err != nil {
			b.Fatal(err)
		}
	}
}

// 1000-row batch insert via raw SQLite — single transaction, prepared
// stmt reused. This is the upper-bound throughput of the driver itself.
func BenchmarkCompare_RawSQLite_Batch1k(b *testing.B) {
	db := tempRawSQLite(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tx, err := db.Begin()
		if err != nil {
			b.Fatal(err)
		}
		stmt, err := tx.Prepare(`INSERT OR IGNORE INTO quads(subject, predicate, object, label) VALUES (?, ?, ?, ?)`)
		if err != nil {
			b.Fatal(err)
		}
		for j := 0; j < 1000; j++ {
			if _, err := stmt.Exec(fmt.Sprintf("s%d-%d", i, j), "p", "o", "source:bench"); err != nil {
				b.Fatal(err)
			}
		}
		stmt.Close()
		if err := tx.Commit(); err != nil {
			b.Fatal(err)
		}
	}
}

// Subject-indexed find via raw SQLite. 10k seeded quads across 100
// subjects; query for one subject's ~100 rows. Apples-to-apples with
// BenchmarkFind_BySubject.
func BenchmarkCompare_RawSQLite_FindBySubject(b *testing.B) {
	db := tempRawSQLite(b)

	// Seed.
	tx, _ := db.Begin()
	stmt, _ := tx.Prepare(`INSERT OR IGNORE INTO quads(subject, predicate, object, label) VALUES (?, ?, ?, ?)`)
	for i := 0; i < 10000; i++ {
		stmt.Exec(fmt.Sprintf("s%d", i%100), fmt.Sprintf("p%d", i%10), fmt.Sprintf("o%d", i), "source:bench")
	}
	stmt.Close()
	tx.Commit()

	q, err := db.Prepare(`SELECT subject, predicate, object, label FROM quads WHERE subject = ?`)
	if err != nil {
		b.Fatal(err)
	}
	defer q.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := q.Query("s50")
		if err != nil {
			b.Fatal(err)
		}
		n := 0
		for rows.Next() {
			var s, p, o, l string
			if err := rows.Scan(&s, &p, &o, &l); err != nil {
				rows.Close()
				b.Fatal(err)
			}
			n++
		}
		rows.Close()
		if n == 0 {
			b.Fatal("no results — index broken?")
		}
	}
}

// quadstore equivalents already exist as BenchmarkCommit_SingleQuad,
// BenchmarkCommit_Batch1k, BenchmarkFind_BySubject. Compare with the
// _ = context.Background() pattern below to keep imports symmetric.
var _ = context.Background

// rawSQLiteSchemaQuadstoreShape mirrors quadstore's schema: same
// quads table, same UNIQUE(s,p,o,l), same four secondary indexes.
// Used by the diagnostic bench below to isolate "is the overhead from
// the schema?" vs "is it from Go-side BulkLoader plumbing?".
const rawSQLiteSchemaQuadstoreShape = `
PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
CREATE TABLE IF NOT EXISTS quads (
    subject   TEXT NOT NULL,
    predicate TEXT NOT NULL,
    object    TEXT NOT NULL,
    label     TEXT NOT NULL DEFAULT '',
    UNIQUE(subject, predicate, object, label)
);
CREATE INDEX IF NOT EXISTS idx_spo ON quads(subject, predicate, object);
CREATE INDEX IF NOT EXISTS idx_pos ON quads(predicate, object, subject);
CREATE INDEX IF NOT EXISTS idx_osp ON quads(object, subject, predicate);
CREATE INDEX IF NOT EXISTS idx_lsp ON quads(label, subject, predicate);
`

func tempRawSQLiteQuadstoreShape(b *testing.B) *sql.DB {
	b.Helper()
	dir := b.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "shape.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	if _, err := db.Exec(rawSQLiteSchemaQuadstoreShape); err != nil {
		b.Fatal(err)
	}
	if _, err := db.Exec(rawSQLiteBulkPragmas); err != nil {
		b.Fatal(err)
	}
	return db
}

// BenchmarkCompare_RawSQLite_QuadstoreShape replicates BulkLoader's
// behavior in raw-SQLite terms: quadstore's schema, drop 3 indexes
// at start, multi-row INSERT in 500-row batches, rebuild indexes at
// end. If this matches quadstore's BulkLoader timing, the overhead
// is structural to the schema/index pattern; if it's faster, the
// overhead is Go-side BulkLoader plumbing we could optimize.
func BenchmarkCompare_RawSQLite_QuadstoreShape(b *testing.B) {
	for _, total := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("N=%d", total), func(b *testing.B) {
			b.ResetTimer()
			for run := 0; run < b.N; run++ {
				b.StopTimer()
				db := tempRawSQLiteQuadstoreShape(b)
				b.StartTimer()
				if _, err := db.Exec(`DROP INDEX idx_pos; DROP INDEX idx_osp; DROP INDEX idx_lsp;`); err != nil {
					b.Fatal(err)
				}
				const batchSize = 500
				args := make([]any, 0, batchSize*4)
				var sb strings.Builder
				for batchStart := 0; batchStart < total; batchStart += batchSize {
					end := batchStart + batchSize
					if end > total {
						end = total
					}
					sb.Reset()
					sb.WriteString(`INSERT OR IGNORE INTO quads (subject, predicate, object, label) VALUES `)
					args = args[:0]
					for i := batchStart; i < end; i++ {
						if i > batchStart {
							sb.WriteByte(',')
						}
						sb.WriteString(`(?,?,?,?)`)
						args = append(args, fmt.Sprintf("s%d-%d", run, i), "p", "o", "source:bench")
					}
					if _, err := db.Exec(sb.String(), args...); err != nil {
						b.Fatal(err)
					}
				}
				if _, err := db.Exec(`
					CREATE INDEX idx_pos ON quads(predicate, object, subject);
					CREATE INDEX idx_osp ON quads(object, subject, predicate);
					CREATE INDEX idx_lsp ON quads(label, subject, predicate);
				`); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				db.Close()
				b.StartTimer()
			}
		})
	}
}


// rawSQLiteBulkLoad emits `total` rows in 500-row transactions against
// a raw SQLite quad table (bulk-tuned PRAGMAs). Centralizes the loop so
// the parameterized benchmarks below stay readable.
func rawSQLiteBulkLoad(b *testing.B, db *sql.DB, run, total int) {
	const batchSize = 500
	tx, err := db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO quads(subject, predicate, object, label) VALUES (?, ?, ?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < total; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("s%d-%d", run, i), "p", "o", "source:bench"); err != nil {
			b.Fatal(err)
		}
		if (i+1)%batchSize == 0 {
			stmt.Close()
			if err := tx.Commit(); err != nil {
				b.Fatal(err)
			}
			tx, err = db.Begin()
			if err != nil {
				b.Fatal(err)
			}
			stmt, err = tx.Prepare(`INSERT OR IGNORE INTO quads(subject, predicate, object, label) VALUES (?, ?, ?, ?)`)
			if err != nil {
				b.Fatal(err)
			}
		}
	}
	stmt.Close()
	tx.Commit()
}

// BenchmarkCompare_RawSQLite_BulkLoad measures raw-SQLite bulk-load
// throughput across N. Apples-to-apples vs quadstore's BulkLoader at
// the same N below. The raw schema keeps its one secondary index
// inline; quadstore drops 3 indexes and rebuilds on Close, so the
// fixed-cost-vs-throughput tradeoff shifts with N.
//
// Each iteration uses a fresh DB so cumulative table growth doesn't
// distort per-iteration cost.
func BenchmarkCompare_RawSQLite_BulkLoad(b *testing.B) {
	for _, total := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("N=%d", total), func(b *testing.B) {
			b.ResetTimer()
			for run := 0; run < b.N; run++ {
				b.StopTimer()
				db := tempRawSQLiteBulkTuned(b)
				b.StartTimer()
				rawSQLiteBulkLoad(b, db, run, total)
				b.StopTimer()
				db.Close()
				b.StartTimer()
			}
		})
	}
}

// BenchmarkCompare_Quadstore_BulkLoader exercises quadstore's BulkLoader
// at the same N as the raw bench above. Each iteration uses a fresh
// store; the timed window covers BulkLoader open + Add loop + Close
// (which rebuilds the 3 secondary indexes).
func BenchmarkCompare_Quadstore_BulkLoader(b *testing.B) {
	ctx := context.Background()
	for _, total := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("N=%d", total), func(b *testing.B) {
			b.ResetTimer()
			for run := 0; run < b.N; run++ {
				b.StopTimer()
				s := tempBenchStore(b)
				b.StartTimer()
				bl, err := s.BulkLoaderWithLabel(ctx, "source:bench")
				if err != nil {
					b.Fatal(err)
				}
				for i := 0; i < total; i++ {
					if err := bl.Add(Quad{
						Subject:   fmt.Sprintf("s%d-%d", run, i),
						Predicate: "p",
						Object:    "o",
					}); err != nil {
						b.Fatal(err)
					}
				}
				if err := bl.Close(); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				s.Close()
				b.StartTimer()
			}
		})
	}
}
