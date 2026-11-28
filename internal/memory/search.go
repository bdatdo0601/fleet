package memory

import (
	"database/sql"
	"strings"
	"unicode"
)

const maxQueryLimit = 200

func clampLimit(limit, def int) int {
	if limit <= 0 {
		return def
	}
	return min(limit, maxQueryLimit)
}

func buildFilterWhere(f SearchFilter) (string, []any, error) {
	clauses := []string{}
	args := []any{}
	if f.Scope != "" {
		ns, err := NormalizeScope(f.Scope)
		if err != nil {
			return "", nil, err
		}
		clauses = append(clauses, "m.scope = ?")
		args = append(args, ns)
	} else if len(f.Scopes) > 0 {
		ph := make([]string, 0, len(f.Scopes))
		for _, raw := range f.Scopes {
			if strings.TrimSpace(raw) == "" {
				continue
			}
			ns, err := NormalizeScope(raw)
			if err != nil {
				return "", nil, err
			}
			ph = append(ph, "?")
			args = append(args, ns)
		}
		if len(ph) > 0 {
			clauses = append(clauses, "m.scope IN ("+strings.Join(ph, ",")+")")
		}
	}
	if f.Kind != "" {
		nk, err := NormalizeKind(f.Kind)
		if err != nil {
			return "", nil, err
		}
		clauses = append(clauses, "m.kind = ?")
		args = append(args, nk)
	}
	if f.Tier != "" {
		nt, err := NormalizeTier(f.Tier, "", "")
		if err != nil {
			return "", nil, err
		}
		clauses = append(clauses, "m.tier = ?")
		args = append(args, nt)
	}
	if f.Status != "" {
		ns, err := NormalizeStatus(f.Status)
		if err != nil {
			return "", nil, err
		}
		clauses = append(clauses, "m.status = ?")
		args = append(args, ns)
	} else if !f.IncludeArchived {
		clauses = append(clauses, "m.status = 'active'")
	}
	if len(clauses) == 0 {
		return "1=1", args, nil
	}
	return strings.Join(clauses, " AND "), args, nil
}

func queryItems(db *sql.DB, q string, args []any, withScore bool) ([]Item, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Item{}
	for rows.Next() {
		it, err := scanItem(rows, withScore)
		if err != nil {
			return nil, err
		}
		if it != nil {
			out = append(out, *it)
		}
	}
	return out, rows.Err()
}

func tokenizeFTS(raw string) []string {
	var tokens []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, `"`+cur.String()+`"*`)
			cur.Reset()
		}
	}
	for _, r := range strings.ToLower(raw) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return tokens
}

func (s *Store) listLocked(db *sql.DB, f SearchFilter) ([]Item, error) {
	where, args, err := buildFilterWhere(f)
	if err != nil {
		return nil, err
	}
	limit := clampLimit(f.Limit, 20)
	args = append(args, limit)
	q := "SELECT " + itemSelectCols + " FROM memories m WHERE " + where + " ORDER BY m.updated_at DESC, m.id DESC LIMIT ?"
	return queryItems(db, q, args, false)
}

// List returns matching memories ordered by recency.
func (s *Store) List(f SearchFilter) ([]Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return nil, err
	}
	return s.listLocked(db, f)
}

func (s *Store) searchFTSLocked(db *sql.DB, f SearchFilter, matchExpr string, limit int) ([]Item, error) {
	where, filterArgs, err := buildFilterWhere(f)
	if err != nil {
		return nil, err
	}
	args := make([]any, 0, len(filterArgs)+2)
	args = append(args, matchExpr)
	args = append(args, filterArgs...)
	args = append(args, limit)
	q := `SELECT ` + itemSelectCols + `, -bm25(memories_fts, 1.0, 4.0, 1.5, 3.0, 1.0) AS score
		FROM memories_fts
		JOIN memories m ON m.id = memories_fts.rowid
		WHERE memories_fts MATCH ? AND ` + where + `
		ORDER BY bm25(memories_fts, 1.0, 4.0, 1.5, 3.0, 1.0) ASC, m.updated_at DESC
		LIMIT ?`
	return queryItems(db, q, args, true)
}

// Search performs FTS5 ranked search with AND-to-OR fallback, or lists when query is empty.
func (s *Store) Search(f SearchFilter) ([]Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(f.Query) == "" {
		return s.listLocked(db, f)
	}
	tokens := tokenizeFTS(f.Query)
	if len(tokens) == 0 {
		return s.listLocked(db, f)
	}
	limit := clampLimit(f.Limit, 20)
	items, err := s.searchFTSLocked(db, f, strings.Join(tokens, " "), limit)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 && len(tokens) > 1 {
		return s.searchFTSLocked(db, f, strings.Join(tokens, " OR "), limit)
	}
	return items, nil
}
