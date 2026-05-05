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
//
// On a partitioned Store, Find scopes to a single partition when the
// pattern routes there deterministically (Pattern.Label resolves via
// LabelRouter, or PartitionedConfig.RoutePattern returns a partition);
// otherwise Find fans out across every partition. Order across
// partitions is unspecified.
func (r *Reader) Find(ctx context.Context, p Pattern) iter.Seq2[Quad, error] {
	query, args := buildMatchQuery(p, false)
	parts := r.store.partsForPattern(p)
	return func(yield func(Quad, error) bool) {
		for _, conn := range parts {
			rows, err := conn.db.QueryContext(ctx, query, args...)
			if err != nil {
				yield(Quad{}, err)
				return
			}
			for rows.Next() {
				var q Quad
				if err := rows.Scan(&q.Subject, &q.Predicate, &q.Object, &q.Label); err != nil {
					rows.Close()
					yield(Quad{}, err)
					return
				}
				if !yield(q, nil) {
					rows.Close()
					return
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				yield(Quad{}, err)
				return
			}
			rows.Close()
		}
	}
}

// Count returns the number of quads matching the pattern. On a
// partitioned Store, sums across partitions if the read fans out.
func (r *Reader) Count(ctx context.Context, p Pattern) (int64, error) {
	query, args := buildMatchQuery(p, true)
	parts := r.store.partsForPattern(p)
	var total int64
	for _, conn := range parts {
		var n int64
		if err := conn.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
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
