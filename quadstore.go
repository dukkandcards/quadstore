// Package quadstore is a minimal embedded graph database for Go.
// Cayley-inspired first principles: subject-predicate-object-label,
// schema-on-read, one file (or directory) per product, path-based
// traversal.
//
// Two backends ship in the same package:
//
//   - quadstore.OpenPebble(path) → *PebbleStore — Pebble LSM
//     (CockroachDB lineage), pure Go, no CGo. The recommended
//     backend. 18-40× faster single-quad commit and ~10× smaller
//     on disk than the SQLite backend on cloud hardware. See
//     docs/PEBBLE_VS_SQLITE.md for measured deltas.
//   - quadstore.Open(path) → *Store — SQLite via modernc.org/sqlite,
//     pure Go, no CGo. Supported indefinitely. Pick this when you
//     want ~20 fewer transitive dependencies, smaller binaries, or
//     sqlite3-CLI access on the data file.
//
// Both backends share the same Quad / Batch / Pattern types, the
// same label namespace enforcement, and the same Writer / Reader /
// BulkLoader API shape. Cross-backend migration: see
// quadstore.MigrateToPebble.
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
//
// Partitioning: see OpenPartitioned and docs/PARTITIONING_DESIGN.md.
// A single-file Store is internally a one-partition Store. All public
// methods are partition-aware; legacy callers see no change in behavior.
package quadstore

import (
	"context"
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

// Store is a quad store. Backed by one SQLite file (Open) or several
// (OpenPartitioned). The Reader / Writer / Batch surface is identical
// across both shapes; routing is internal.
type Store struct {
	parts        []*partitionConn
	byName       map[Partition]*partitionConn
	defaultName  Partition // empty for single-file Stores
	routeLabel   LabelRouter
	routePattern PatternRouter
}

// Open creates or opens a single-file quad store at path. Applies schema
// migrations up to the current version; fails loudly if the on-disk
// schema is newer than the library.
//
// For multi-partition setups, see OpenPartitioned.
func Open(path string) (*Store, error) {
	db, err := openSQLite(path)
	if err != nil {
		return nil, fmt.Errorf("quadstore: open %s: %w", path, err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	conn := &partitionConn{
		name:       "", // sentinel for single-file mode
		db:         db,
		writerSlot: make(chan struct{}, 1),
	}
	return &Store{
		parts:       []*partitionConn{conn},
		byName:      map[Partition]*partitionConn{"": conn},
		defaultName: "",
		// routeLabel intentionally nil — partFor short-circuits on this.
	}, nil
}

// Close closes every partition's underlying SQL connection. Returns the
// first error encountered; subsequent partitions are still closed.
func (s *Store) Close() error {
	var firstErr error
	for _, p := range s.parts {
		if err := p.db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Add inserts a single quad. Duplicate quads are silently ignored.
// On a partitioned Store, the quad routes by its Label.
func (s *Store) Add(q Quad) error {
	if !q.valid() {
		return fmt.Errorf("quadstore: subject, predicate, and object are required")
	}
	conn := s.partFor(q.Label)
	_, err := conn.db.Exec(
		`INSERT OR IGNORE INTO quads (subject, predicate, object, label) VALUES (?, ?, ?, ?)`,
		q.Subject, q.Predicate, q.Object, q.Label,
	)
	return err
}

// AddBatch inserts multiple quads in a single transaction. On a
// partitioned Store all quads must route to the same partition;
// otherwise returns ErrCrossPartitionBatch and writes nothing.
// Single-file Stores are unaffected by this rule.
func (s *Store) AddBatch(quads []Quad) error {
	if len(quads) == 0 {
		return nil
	}
	conn, err := s.partForQuads(quads)
	if err != nil {
		return err
	}
	tx, err := conn.db.Begin()
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

// partForQuads validates that every quad in the slice routes to the
// same partition and returns that partition's connection. Returns
// ErrCrossPartitionBatch if quads disagree.
func (s *Store) partForQuads(quads []Quad) (*partitionConn, error) {
	if !s.partitioned() {
		return s.parts[0], nil
	}
	first := s.partFor(quads[0].Label)
	for _, q := range quads[1:] {
		if s.partFor(q.Label) != first {
			return nil, ErrCrossPartitionBatch
		}
	}
	return first, nil
}

// Delete removes a single quad. On a partitioned Store, routes by Label.
func (s *Store) Delete(q Quad) error {
	conn := s.partFor(q.Label)
	_, err := conn.db.Exec(
		`DELETE FROM quads WHERE subject=? AND predicate=? AND object=? AND label=?`,
		q.Subject, q.Predicate, q.Object, q.Label,
	)
	return err
}

// LabelCounts returns one row per distinct label with its quad count,
// summed across partitions on a partitioned Store. Uses the
// idx_lsp(label, subject, predicate) index, so it's orders of magnitude
// faster than Stats() on large DBs (~ms vs minutes for a 28 GB store).
//
// Empty-label entries are reported under the empty string. Use this for
// migration-tool planning, dashboards, or any "what fact families are
// in this DB" lookup.
func (s *Store) LabelCounts(ctx context.Context) (map[string]int64, error) {
	out := make(map[string]int64)
	for _, p := range s.parts {
		rows, err := p.db.QueryContext(ctx, `SELECT label, COUNT(*) FROM quads GROUP BY label`)
		if err != nil {
			return nil, fmt.Errorf("quadstore: label counts %s: %w", p.name, err)
		}
		for rows.Next() {
			var label string
			var n int64
			if err := rows.Scan(&label, &n); err != nil {
				rows.Close()
				return nil, err
			}
			out[label] += n
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// Stats returns counts of quads and distinct predicates summed across
// every partition. The DISTINCT predicate count is the union of distinct
// predicates seen in any partition.
//
// Stats reads the entire quads table — slow on multi-GB DBs. For a fast
// label-keyed breakdown use LabelCounts instead.
func (s *Store) Stats() (quads int64, predicates int64, err error) {
	preds := make(map[string]struct{})
	for _, p := range s.parts {
		var n int64
		if err = p.db.QueryRow(`SELECT COUNT(*) FROM quads`).Scan(&n); err != nil {
			return
		}
		quads += n
		rows, qerr := p.db.Query(`SELECT DISTINCT predicate FROM quads`)
		if qerr != nil {
			err = qerr
			return
		}
		for rows.Next() {
			var pred string
			if err = rows.Scan(&pred); err != nil {
				rows.Close()
				return
			}
			preds[pred] = struct{}{}
		}
		rows.Close()
	}
	predicates = int64(len(preds))
	return
}

// Vacuum runs SQLite VACUUM on every partition sequentially. Reclaims
// free pages after bulk deletes (e.g., after Writer.PruneOps). Blocks
// other connections to each partition for that partition's vacuum
// duration. Disk free space ~= the largest partition's size.
//
// For per-partition control (e.g. vacuum corpus during a quiet window
// without touching main), use Store.VacuumFor.
func (s *Store) Vacuum() error {
	for _, p := range s.parts {
		if _, err := p.db.Exec(`VACUUM`); err != nil {
			return fmt.Errorf("quadstore: vacuum %s: %w", p.name, err)
		}
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
// Sums across partitions on a partitioned Store.
func (s *Store) CommitStatsAt(cutoff time.Time) (CommitStats, error) {
	var cs CommitStats
	cutoffUnix := cutoff.Unix()
	hasCutoff := !cutoff.IsZero()
	for _, p := range s.parts {
		var n int64
		if err := p.db.QueryRow(`SELECT COUNT(*) FROM commits`).Scan(&n); err != nil {
			return cs, fmt.Errorf("quadstore: count commits %s: %w", p.name, err)
		}
		cs.TotalCommits += n
		if err := p.db.QueryRow(`SELECT COUNT(*) FROM commit_ops`).Scan(&n); err != nil {
			return cs, fmt.Errorf("quadstore: count commit_ops %s: %w", p.name, err)
		}
		cs.TotalOps += n
		if !hasCutoff {
			continue
		}
		if err := p.db.QueryRow(`SELECT COUNT(*) FROM commits WHERE created_at < ?`, cutoffUnix).Scan(&n); err != nil {
			return cs, fmt.Errorf("quadstore: count old commits %s: %w", p.name, err)
		}
		cs.OldCommits += n
		if err := p.db.QueryRow(
			`SELECT COUNT(*) FROM commit_ops WHERE commit_id IN (SELECT id FROM commits WHERE created_at < ?)`,
			cutoffUnix,
		).Scan(&n); err != nil {
			return cs, fmt.Errorf("quadstore: count old commit_ops %s: %w", p.name, err)
		}
		cs.OldOps += n
	}
	return cs, nil
}

// --- Pattern matching (legacy iterator API) ---

// Iterator walks matched quads lazily. On a partitioned Store it walks
// each partition's results in turn (configured iteration order).
type Iterator struct {
	parts []*partitionConn // partitions to walk in order
	query string
	args  []any
	cur   *sql.Rows
	idx   int // index into parts of the partition cur is reading from
	q     Quad
	err   error
}

// Next advances to the next quad. Returns false when exhausted or on
// error. Spans partitions transparently on a partitioned Store.
func (it *Iterator) Next() bool {
	for it.err == nil {
		if it.cur == nil {
			if it.idx >= len(it.parts) {
				return false
			}
			rows, err := it.parts[it.idx].db.Query(it.query, it.args...)
			if err != nil {
				it.err = err
				return false
			}
			it.cur = rows
			it.idx++
		}
		if it.cur.Next() {
			if err := it.cur.Scan(&it.q.Subject, &it.q.Predicate, &it.q.Object, &it.q.Label); err != nil {
				it.err = err
				return false
			}
			return true
		}
		// Partition exhausted; close and try the next.
		if err := it.cur.Err(); err != nil {
			it.err = err
			return false
		}
		it.cur.Close()
		it.cur = nil
	}
	return false
}

// Quad returns the current quad.
func (it *Iterator) Quad() Quad { return it.q }

// Err returns any error from iteration.
func (it *Iterator) Err() error { return it.err }

// Close releases the current cursor. Subsequent calls are no-ops.
func (it *Iterator) Close() error {
	if it.cur != nil {
		err := it.cur.Close()
		it.cur = nil
		return err
	}
	return nil
}

// Match returns quads matching a pattern. Empty string = wildcard.
// On a partitioned Store, scopes to one partition when the pattern's
// label routes to one; otherwise iterates all partitions.
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
	pattern := Pattern{Subject: subject, Predicate: predicate, Object: object, Label: label}
	parts := s.partsForPattern(pattern)
	return &Iterator{parts: parts, query: q, args: args}
}

// partsForPattern returns the slice of partitions a Pattern read should
// touch. One partition if the pattern routes deterministically, all
// partitions otherwise. Used by Match (legacy) and Reader.Find / Count.
func (s *Store) partsForPattern(p Pattern) []*partitionConn {
	if conn := s.partForPattern(p); conn != nil {
		return []*partitionConn{conn}
	}
	return s.parts
}

// --- Path traversal (the Cayley part) ---

// Path is a chainable, lazy graph traversal.
//
// On a partitioned Store, traversal queries each step against every
// partition and unions results. This is correct but conservative; a
// consumer that knows its traversal stays within one partition can
// scope by setting label hints in custom queries via Reader.Find.
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

// Has filters nodes that have an edge with the given predicate=value.
func (p *Path) Has(predicate, value string) *Path {
	p2 := p.clone()
	p2.steps = append(p2.steps, step{kind: stepHas, predicate: predicate, value: value})
	return p2
}

// Unique deduplicates nodes seen so far.
func (p *Path) Unique() *Path {
	p2 := p.clone()
	p2.steps = append(p2.steps, step{kind: stepUnique})
	return p2
}

func (p *Path) clone() *Path {
	cp := &Path{store: p.store, seeds: p.seeds}
	cp.steps = append(cp.steps, p.steps...)
	return cp
}

// Iterate runs the path and returns each terminal node. Materializes
// results in memory; for very large traversals consider Reader.Find.
func (p *Path) Iterate(ctx context.Context) ([]string, error) {
	if len(p.seeds) == 0 {
		return nil, nil
	}
	cur := append([]string(nil), p.seeds...)
	for _, st := range p.steps {
		next, err := p.applyStep(ctx, st, cur)
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}

// All runs the path and returns every reachable terminal node. Equivalent
// to Iterate(context.Background()) — kept for legacy callers.
func (p *Path) All() ([]string, error) {
	return p.Iterate(context.Background())
}

// Count runs the path and returns the number of terminal nodes.
func (p *Path) Count() (int, error) {
	out, err := p.Iterate(context.Background())
	if err != nil {
		return 0, err
	}
	return len(out), nil
}

// First runs the path and returns the first terminal node, if any.
// Returns "" when no nodes match.
func (p *Path) First() (string, error) {
	out, err := p.Iterate(context.Background())
	if err != nil {
		return "", err
	}
	if len(out) == 0 {
		return "", nil
	}
	return out[0], nil
}

func (p *Path) applyStep(ctx context.Context, st step, cur []string) ([]string, error) {
	if len(cur) == 0 {
		return cur, nil
	}
	switch st.kind {
	case stepOut, stepIn:
		var col, target string
		if st.kind == stepOut {
			col, target = "subject", "object"
		} else {
			col, target = "object", "subject"
		}
		placeholders := strings.Repeat("?,", len(cur))
		placeholders = placeholders[:len(placeholders)-1]
		predFilter := ""
		args := make([]any, 0, len(cur)+len(st.predicates))
		for _, n := range cur {
			args = append(args, n)
		}
		if len(st.predicates) > 0 {
			pp := strings.Repeat("?,", len(st.predicates))
			pp = pp[:len(pp)-1]
			predFilter = " AND predicate IN (" + pp + ")"
			for _, pr := range st.predicates {
				args = append(args, pr)
			}
		}
		q := fmt.Sprintf(`SELECT DISTINCT %s FROM quads WHERE %s IN (%s)%s`,
			target, col, placeholders, predFilter)
		var out []string
		seen := make(map[string]struct{})
		for _, conn := range p.store.parts {
			rows, err := conn.db.QueryContext(ctx, q, args...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var v string
				if err := rows.Scan(&v); err != nil {
					rows.Close()
					return nil, err
				}
				if _, dup := seen[v]; dup {
					continue
				}
				seen[v] = struct{}{}
				out = append(out, v)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return nil, err
			}
			rows.Close()
		}
		return out, nil
	case stepHas:
		var out []string
		seen := make(map[string]struct{})
		for _, conn := range p.store.parts {
			placeholders := strings.Repeat("?,", len(cur))
			placeholders = placeholders[:len(placeholders)-1]
			args := make([]any, 0, len(cur)+2)
			for _, n := range cur {
				args = append(args, n)
			}
			args = append(args, st.predicate, st.value)
			q := fmt.Sprintf(
				`SELECT DISTINCT subject FROM quads WHERE subject IN (%s) AND predicate = ? AND object = ?`,
				placeholders,
			)
			rows, err := conn.db.QueryContext(ctx, q, args...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var v string
				if err := rows.Scan(&v); err != nil {
					rows.Close()
					return nil, err
				}
				if _, dup := seen[v]; dup {
					continue
				}
				seen[v] = struct{}{}
				out = append(out, v)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return nil, err
			}
			rows.Close()
		}
		return out, nil
	case stepUnique:
		seen := make(map[string]struct{})
		out := cur[:0]
		for _, n := range cur {
			if _, dup := seen[n]; dup {
				continue
			}
			seen[n] = struct{}{}
			out = append(out, n)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("quadstore: unknown step kind %d", st.kind)
	}
}
