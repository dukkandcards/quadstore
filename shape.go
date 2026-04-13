package quadstore

import "fmt"

// Token is an opaque reference to a value — for cross-product
// shape analysis without exposing actual data.
type Token uint64

// TokenEdge represents a directed edge between tokenized nodes.
type TokenEdge struct {
	From      Token
	Predicate string
	To        Token
}

// Shape is a graph's topology with subjects/objects replaced by opaque
// tokens. Predicates remain visible — they're schema, not data.
type Shape struct {
	NodeCount int
	Predicates []string
	Edges      []TokenEdge
}

// Shape exports the graph's topology with values replaced by tokens.
// Predicates are visible (they describe structure). Subjects and objects
// are opaque (they are data). Two stores produce comparable shapes
// without revealing either product's content.
func (s *Store) Shape() (*Shape, error) {
	// Collect distinct predicates.
	predRows, err := s.db.Query(`SELECT DISTINCT predicate FROM quads ORDER BY predicate`)
	if err != nil {
		return nil, fmt.Errorf("quadstore shape: predicates: %w", err)
	}
	var predicates []string
	for predRows.Next() {
		var p string
		if err := predRows.Scan(&p); err != nil {
			predRows.Close()
			return nil, err
		}
		predicates = append(predicates, p)
	}
	predRows.Close()
	if err := predRows.Err(); err != nil {
		return nil, err
	}

	// Tokenize all values and build edges.
	tokenMap := make(map[string]Token)
	var nextToken Token = 1

	tokenize := func(val string) Token {
		if t, ok := tokenMap[val]; ok {
			return t
		}
		t := nextToken
		nextToken++
		tokenMap[val] = t
		return t
	}

	rows, err := s.db.Query(`SELECT subject, predicate, object FROM quads`)
	if err != nil {
		return nil, fmt.Errorf("quadstore shape: quads: %w", err)
	}
	defer rows.Close()

	var edges []TokenEdge
	for rows.Next() {
		var subj, pred, obj string
		if err := rows.Scan(&subj, &pred, &obj); err != nil {
			return nil, err
		}
		edges = append(edges, TokenEdge{
			From:      tokenize(subj),
			Predicate: pred,
			To:        tokenize(obj),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return &Shape{
		NodeCount:  len(tokenMap),
		Predicates: predicates,
		Edges:      edges,
	}, nil
}
