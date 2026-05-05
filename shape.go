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
	// On a partitioned Store the shape is the union of all partitions'
	// shapes — predicates are deduped across partitions, edges are
	// concatenated. Tokenization is global (so a value seen in two
	// partitions gets the same Token).
	predSet := make(map[string]struct{})
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

	var edges []TokenEdge
	for _, conn := range s.parts {
		predRows, err := conn.db.Query(`SELECT DISTINCT predicate FROM quads`)
		if err != nil {
			return nil, fmt.Errorf("quadstore shape: predicates %s: %w", conn.name, err)
		}
		for predRows.Next() {
			var p string
			if err := predRows.Scan(&p); err != nil {
				predRows.Close()
				return nil, err
			}
			predSet[p] = struct{}{}
		}
		predRows.Close()
		if err := predRows.Err(); err != nil {
			return nil, err
		}

		rows, err := conn.db.Query(`SELECT subject, predicate, object FROM quads`)
		if err != nil {
			return nil, fmt.Errorf("quadstore shape: quads %s: %w", conn.name, err)
		}
		for rows.Next() {
			var subj, pred, obj string
			if err := rows.Scan(&subj, &pred, &obj); err != nil {
				rows.Close()
				return nil, err
			}
			edges = append(edges, TokenEdge{
				From:      tokenize(subj),
				Predicate: pred,
				To:        tokenize(obj),
			})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	predicates := make([]string, 0, len(predSet))
	for p := range predSet {
		predicates = append(predicates, p)
	}
	sortStrings(predicates)

	return &Shape{
		NodeCount:  len(tokenMap),
		Predicates: predicates,
		Edges:      edges,
	}, nil
}

// sortStrings is a tiny dependency-free sort to keep Shape's predicate
// list stable across runs without pulling in sort just for this file.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
