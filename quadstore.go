// Package quadstore is a minimal quad store backed by SQLite.
// Cayley-inspired first principles: subject-predicate-object-label,
// schema-on-read, one file per product, path-based traversal.
package quadstore

import (
	"database/sql"
	"fmt"
	"strings"

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
	db *sql.DB
}

// Open creates or opens a quad store at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("quadstore: open %s: %w", path, err)
	}
	if err := createSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the store.
func (s *Store) Close() error {
	return s.db.Close()
}

func createSchema(db *sql.DB) error {
	_, err := db.Exec(`
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
	`)
	return err
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
