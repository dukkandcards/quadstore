// Package quadstore is a minimal quad store backed by SQLite.
// Cayley-inspired first principles: subject-predicate-object-label,
// schema-on-read, one file per product, path-based traversal.
//
// Label namespace for quads written via Writer.Commit (enforced):
//
//	source:*   raw ingest (source:msgraph, source:psa-api, source:book-ocr)
//	derived:*  computed signals (derived:cluster, derived:contention, derived:tfidf)
//	human:*    attention / decision triples (human:selected, human:overridden)
//	meta:*     provenance and schema (meta:schema-version)
//
// Legacy write methods (Add, AddBatch, Delete) are permissive — they
// accept any label string, including pre-namespace labels ("reference",
// "generated") used by existing corpora. New callers should use
// Writer.Commit; legacy callers can migrate at their own pace. Migration
// mapping: reference → source:reference; generated → derived:generated.
package quadstore

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Quad is the only data structure.
type Quad struct {
	Subject   string
	Predicate string
	Object    string
	Label     string // optional graph/context partition within a store
}

func (q Quad) valid() bool {
	return q.Subject != "" && q.Predicate != "" && q.Object != ""
}

// Store is one SQLite file — one product's data.
type Store struct {
	db         *sql.DB
	writerSlot chan struct{} // Rung 1: single-writer queue, capacity 1
}

// Open creates or opens a quad store at path. Applies schema migrations
// up to the current version; fails loudly if the on-disk schema is newer
// than the library (downgrade refused).
func Open(path string) (*Store, error) {
	// modernc.org/sqlite honors _pragma=key(value) in the DSN; the legacy
	// _journal_mode / _busy_timeout shortcuts are silently ignored (verified
	// 2026-04-20 — live SecDek DB had been in rollback-journal mode since
	// project start). All five PRAGMAs go through _pragma=. cache_size=-262144
	// is 256 MB (negative = kibibytes). synchronous=NORMAL is safe under WAL.
	// journal_size_limit=500MB caps WAL growth: SQLite checkpoints on schedule
	// but with the default -1 (no limit) the WAL high-water mark is never
	// truncated, so on long-lived writers the WAL file grows unbounded across
	// bulk-ingest spikes (verified 2026-05-01 on live SecDek — WAL hit 1.5 GB
	// after a 798K-quad forward-ingest run, recovered only via manual
	// PRAGMA wal_checkpoint(TRUNCATE)).
	dsn := path + "?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=cache_size(-262144)" +
		"&_pragma=journal_size_limit(500000000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("quadstore: open %s: %w", path, err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{
		db:         db,
		writerSlot: make(chan struct{}, 1),
	}, nil
}

// Close closes the store.
func (s *Store) Close() error {
	return s.db.Close()
}

// Add inserts a single quad. Duplicate quads are silently ignored.
func (s *Store) Add(q Quad) error {
	if !q.valid() {
		return fmt.Errorf("quadstore: subject, predicate, and object are required")
	}
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO quads (subject, predicate, object, label) VALUES (?, ?, ?, ?)`,
		q.Subject, q.Predicate, q.Object, q.Label,
	)
	return err
}

// AddBatch inserts multiple quads in a single transaction.
func (s *Store) AddBatch(quads []Quad) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(
		`INSERT OR IGNORE INTO quads (subject, predicate, object, label) VALUES (?, ?, ?, ?)`,
	)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, q := range quads {
		if !q.valid() {
			tx.Rollback()
			return fmt.Errorf("quadstore: subject, predicate, and object are required")
		}
		if _, err := stmt.Exec(q.Subject, q.Predicate, q.Object, q.Label); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// Delete removes a single quad.
func (s *Store) Delete(q Quad) error {
	_, err := s.db.Exec(
		`DELETE FROM quads WHERE subject=? AND predicate=? AND object=? AND label=?`,
		q.Subject, q.Predicate, q.Object, q.Label,
	)
	return err
}

// Stats returns counts of quads and distinct predicates.
func (s *Store) Stats() (quads int64, predicates int64, err error) {
	err = s.db.QueryRow(`SELECT COUNT(*) FROM quads`).Scan(&quads)
	if err != nil {
		return
	}
	err = s.db.QueryRow(`SELECT COUNT(DISTINCT predicate) FROM quads`).Scan(&predicates)
	return
}

// Vacuum runs SQLite VACUUM to reclaim free pages after bulk deletes
// (e.g., after Writer.PruneOps). Blocks all other connections to the DB
// for its duration. Requires free disk space approximately equal to the
// current DB size (VACUUM rewrites the whole file).
func (s *Store) Vacuum() error {
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return fmt.Errorf("quadstore: vacuum: %w", err)
	}
	return nil
}

// CommitStats reports commit-journal scale relative to a cutoff, used by
// retention tooling to preview or verify a PruneOps sweep.
type CommitStats struct {
	TotalCommits int64 // all rows in commits
	OldCommits   int64 // commits.created_at < cutoff.Unix()
	TotalOps     int64 // all rows in commit_ops
	OldOps       int64 // commit_ops rows whose commit is older than cutoff
}

// CommitStatsAt returns commit-journal counts with `old` fields measured
// against the given cutoff. A zero cutoff yields 0 for both `Old` fields.
func (s *Store) CommitStatsAt(cutoff time.Time) (CommitStats, error) {
	var cs CommitStats
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM commits`).Scan(&cs.TotalCommits); err != nil {
		return cs, fmt.Errorf("quadstore: count commits: %w", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM commit_ops`).Scan(&cs.TotalOps); err != nil {
		return cs, fmt.Errorf("quadstore: count commit_ops: %w", err)
	}
	if cutoff.IsZero() {
		return cs, nil
	}
	cutoffUnix := cutoff.Unix()
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM commits WHERE created_at < ?`, cutoffUnix).Scan(&cs.OldCommits); err != nil {
		return cs, fmt.Errorf("quadstore: count old commits: %w", err)
	}
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM commit_ops WHERE commit_id IN (SELECT id FROM commits WHERE created_at < ?)`,
		cutoffUnix,
	).Scan(&cs.OldOps); err != nil {
		return cs, fmt.Errorf("quadstore: count old commit_ops: %w", err)
	}
	return cs, nil
}

// --- Pattern matching ---

// Iterator walks matched quads lazily.
type Iterator struct {
	rows *sql.Rows
	cur  Quad
	err  error
}

// Next advances to the next quad. Returns false when exhausted or on error.
func (it *Iterator) Next() bool {
	if it.rows == nil {
		return false
	}
	if !it.rows.Next() {
		it.err = it.rows.Err()
		return false
	}
	it.err = it.rows.Scan(&it.cur.Subject, &it.cur.Predicate, &it.cur.Object, &it.cur.Label)
	return it.err == nil
}

// Quad returns the current quad.
func (it *Iterator) Quad() Quad { return it.cur }

// Err returns any error from iteration.
func (it *Iterator) Err() error { return it.err }

// Close releases the underlying resources.
func (it *Iterator) Close() error {
	if it.rows == nil {
		return nil
	}
	return it.rows.Close()
}

// Match returns quads matching a pattern. Empty string = wildcard.
func (s *Store) Match(subject, predicate, object, label string) *Iterator {
	var where []string
	var args []any
	if subject != "" {
		where = append(where, "subject=?")
		args = append(args, subject)
	}
	if predicate != "" {
		where = append(where, "predicate=?")
		args = append(args, predicate)
	}
	if object != "" {
		where = append(where, "object=?")
		args = append(args, object)
	}
	if label != "" {
		where = append(where, "label=?")
		args = append(args, label)
	}
	q := "SELECT subject, predicate, object, label FROM quads"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return &Iterator{err: err}
	}
	return &Iterator{rows: rows}
}

// --- Path traversal (the Cayley part) ---

// Path is a chainable, lazy graph traversal.
type Path struct {
	store *Store
	steps []step
	seeds []string
}

type stepKind int

const (
	stepOut stepKind = iota
	stepIn
	stepHas
	stepUnique
)

type step struct {
	kind       stepKind
	predicates []string // for out/in
	predicate  string   // for has
	value      string   // for has
}

// From starts a path from one or more known nodes.
func (s *Store) From(nodes ...string) *Path {
	return &Path{store: s, seeds: nodes}
}

// Out follows edges forward through the given predicates.
func (p *Path) Out(predicates ...string) *Path {
	p2 := p.clone()
	p2.steps = append(p2.steps, step{kind: stepOut, predicates: predicates})
	return p2
}

// In follows edges backward through the given predicates.
func (p *Path) In(predicates ...string) *Path {
	p2 := p.clone()
	p2.steps = append(p2.steps, step{kind: stepIn, predicates: predicates})
	return p2
}

// Has filters to nodes that have the given predicate-value pair.
func (p *Path) Has(predicate, value string) *Path {
	p2 := p.clone()
	p2.steps = append(p2.steps, step{kind: stepHas, predicate: predicate, value: value})
	return p2
}

// Unique deduplicates the current node set.
func (p *Path) Unique() *Path {
	p2 := p.clone()
	p2.steps = append(p2.steps, step{kind: stepUnique})
	return p2
}

func (p *Path) clone() *Path {
	steps := make([]step, len(p.steps))
	copy(steps, p.steps)
	seeds := make([]string, len(p.seeds))
	copy(seeds, p.seeds)
	return &Path{store: p.store, steps: steps, seeds: seeds}
}

// All materializes the path into all result nodes.
func (p *Path) All() ([]string, error) {
	nodes := p.seeds
	for _, s := range p.steps {
		var err error
		nodes, err = p.applyStep(nodes, s)
		if err != nil {
			return nil, err
		}
		if len(nodes) == 0 {
			return nil, nil
		}
	}
	return nodes, nil
}

// Count returns the number of result nodes.
func (p *Path) Count() (int64, error) {
	nodes, err := p.All()
	if err != nil {
		return 0, err
	}
	return int64(len(nodes)), nil
}

// First returns the first result node, or "" if empty.
func (p *Path) First() (string, error) {
	nodes, err := p.All()
	if err != nil {
		return "", err
	}
	if len(nodes) == 0 {
		return "", nil
	}
	return nodes[0], nil
}

func (p *Path) applyStep(nodes []string, s step) ([]string, error) {
	switch s.kind {
	case stepOut:
		return p.walkOut(nodes, s.predicates)
	case stepIn:
		return p.walkIn(nodes, s.predicates)
	case stepHas:
		return p.filterHas(nodes, s.predicate, s.value)
	case stepUnique:
		return unique(nodes), nil
	}
	return nodes, nil
}

func (p *Path) walkOut(nodes []string, predicates []string) ([]string, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	ph := placeholders(len(nodes))
	args := stringsToAny(nodes)

	q := "SELECT object FROM quads WHERE subject IN (" + ph + ")"
	if len(predicates) > 0 {
		q += " AND predicate IN (" + placeholders(len(predicates)) + ")"
		args = append(args, stringsToAny(predicates)...)
	}
	return p.queryStrings(q, args)
}

func (p *Path) walkIn(nodes []string, predicates []string) ([]string, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	ph := placeholders(len(nodes))
	args := stringsToAny(nodes)

	q := "SELECT subject FROM quads WHERE object IN (" + ph + ")"
	if len(predicates) > 0 {
		q += " AND predicate IN (" + placeholders(len(predicates)) + ")"
		args = append(args, stringsToAny(predicates)...)
	}
	return p.queryStrings(q, args)
}

func (p *Path) filterHas(nodes []string, predicate, value string) ([]string, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	ph := placeholders(len(nodes))
	args := stringsToAny(nodes)
	args = append(args, predicate, value)

	q := "SELECT subject FROM quads WHERE subject IN (" + ph + ") AND predicate=? AND object=?"
	return p.queryStrings(q, args)
}

func (p *Path) queryStrings(q string, args []any) ([]string, error) {
	rows, err := p.store.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

func stringsToAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func unique(ss []string) []string {
	seen := make(map[string]struct{}, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}
