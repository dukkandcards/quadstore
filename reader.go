package quadstore

import (
	"context"
	"iter"
	"strings"
)

// Pattern is a match specification; empty fields are wildcards.
type Pattern struct {
	Subject, Predicate, Object, Label string
}

// Reader provides concurrent read access. Cheap to create; safe to share.
// Many Readers per Store is fine.
type Reader struct {
	store *Store
}

// Reader returns a Reader for this Store.
func (s *Store) Reader() *Reader {
	return &Reader{store: s}
}

// Find returns an iter.Seq2 of matching quads. Errors surface mid-stream
// via the second value and terminate iteration.
func (r *Reader) Find(ctx context.Context, p Pattern) iter.Seq2[Quad, error] {
	return func(yield func(Quad, error) bool) {
		query, args := buildMatchQuery(p, false)
		rows, err := r.store.db.QueryContext(ctx, query, args...)
		if err != nil {
			yield(Quad{}, err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var q Quad
			if err := rows.Scan(&q.Subject, &q.Predicate, &q.Object, &q.Label); err != nil {
				yield(Quad{}, err)
				return
			}
			if !yield(q, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(Quad{}, err)
		}
	}
}

// Count returns the number of quads matching the pattern.
func (r *Reader) Count(ctx context.Context, p Pattern) (int64, error) {
	query, args := buildMatchQuery(p, true)
	var n int64
	err := r.store.db.QueryRowContext(ctx, query, args...).Scan(&n)
	return n, err
}

func buildMatchQuery(p Pattern, count bool) (string, []any) {
	var where []string
	var args []any
	if p.Subject != "" {
		where = append(where, "subject = ?")
		args = append(args, p.Subject)
	}
	if p.Predicate != "" {
		where = append(where, "predicate = ?")
		args = append(args, p.Predicate)
	}
	if p.Object != "" {
		where = append(where, "object = ?")
		args = append(args, p.Object)
	}
	if p.Label != "" {
		where = append(where, "label = ?")
		args = append(args, p.Label)
	}
	var q string
	if count {
		q = "SELECT COUNT(*) FROM quads"
	} else {
		q = "SELECT subject, predicate, object, label FROM quads"
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	return q, args
}
