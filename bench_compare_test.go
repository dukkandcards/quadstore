package quadstore

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
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

// BenchmarkCompare_RawSQLite_BulkLoaderEquivalent emits 10k rows in
// 500-row batches via raw SQLite — apples-to-apples vs quadstore's
// BulkLoader (default batchSize=500).
func BenchmarkCompare_RawSQLite_BulkLoaderEquivalent(b *testing.B) {
	db := tempRawSQLiteBulkTuned(b)
	const total = 10000
	const batchSize = 500
	b.ResetTimer()
	for run := 0; run < b.N; run++ {
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
}

// quadstore-side counterpart to the BulkLoader-equivalent raw bench.
func BenchmarkCompare_Quadstore_BulkLoader10k(b *testing.B) {
	s := tempBenchStore(b)
	ctx := context.Background()
	const total = 10000
	b.ResetTimer()
	for run := 0; run < b.N; run++ {
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
	}
}
