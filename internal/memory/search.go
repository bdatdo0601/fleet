package memory

import (
	"database/sql"
	"maps"
	"sort"
	"strings"
	"unicode"
)

const (
	maxQueryLimit       = 200
	minCosineSimilarity = 0.25
	hybridRRFK          = 60.0
)

type vecScoreHit struct {
	id    int64
	score float64
}

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

func (s *Store) searchFTSWithFallbackLocked(db *sql.DB, f SearchFilter, tokens []string, limit int) ([]Item, error) {
	items, err := s.searchFTSLocked(db, f, strings.Join(tokens, " "), limit)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 && len(tokens) > 1 {
		return s.searchFTSLocked(db, f, strings.Join(tokens, " OR "), limit)
	}
	return items, nil
}

func (s *Store) ensureVectorCacheLocked(db *sql.DB, model string) (map[int64][]float32, error) {
	s.embedMu.RLock()
	valid := s.vecCacheValid && s.vecCacheModel == model
	cache := s.vecCache
	s.embedMu.RUnlock()
	if valid {
		return cache, nil
	}

	rows, err := db.Query(
		`SELECT id, embedding FROM memories WHERE status = 'active' AND embedding IS NOT NULL AND embedding_model = ?`,
		model,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	next := make(map[int64][]float32)
	for rows.Next() {
		var id int64
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, err
		}
		if vec := DecodeVector(blob); len(vec) > 0 {
			next[id] = vec
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	s.embedMu.Lock()
	s.vecCache = next
	s.vecCacheModel = model
	s.vecCacheValid = true
	s.embedMu.Unlock()
	return next, nil
}

func (s *Store) vectorCandidatesLocked(db *sql.DB, f SearchFilter, qVec []float32, model string, limit int) ([]vecScoreHit, error) {
	cache, err := s.ensureVectorCacheLocked(db, model)
	if err != nil || len(cache) == 0 {
		return nil, err
	}
	where, args, err := buildFilterWhere(f)
	if err != nil {
		return nil, err
	}
	args = append(args, model)
	rows, err := db.Query(
		`SELECT m.id FROM memories m WHERE `+where+` AND m.embedding IS NOT NULL AND m.embedding_model = ?`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var hits []vecScoreHit
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if vec, ok := cache[id]; ok {
			if sim := DotProduct(qVec, vec); sim >= minCosineSimilarity {
				hits = append(hits, vecScoreHit{id: id, score: sim})
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func (s *Store) fetchItemsByIDsLocked(db *sql.DB, ids []int64) (map[int64]Item, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	items, err := queryItems(db, "SELECT "+itemSelectCols+" FROM memories m WHERE m.id IN ("+strings.Join(ph, ",")+")", args, false)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]Item, len(items))
	for _, it := range items {
		byID[it.ID] = it
	}
	return byID, nil
}

func (s *Store) hybridFuseLocked(db *sql.DB, f SearchFilter, ftsItems []Item, qVec []float32, model string, limit int) ([]Item, error) {
	candLimit := max(limit*3, 40)
	vecHits, err := s.vectorCandidatesLocked(db, f, qVec, model, candLimit)
	if err != nil || len(vecHits) == 0 {
		if len(ftsItems) > limit {
			return ftsItems[:limit], nil
		}
		return ftsItems, nil
	}

	byID := make(map[int64]Item, len(ftsItems))
	scores := make(map[int64]float64, len(ftsItems)+len(vecHits))
	for i, it := range ftsItems {
		byID[it.ID] = it
		scores[it.ID] += 1.0 / (hybridRRFK + float64(i+1))
	}
	var missingIDs []int64
	for i, h := range vecHits {
		scores[h.id] += 1.0 / (hybridRRFK + float64(i+1))
		if _, ok := byID[h.id]; !ok {
			missingIDs = append(missingIDs, h.id)
		}
	}
	if len(missingIDs) > 0 {
		fetched, fErr := s.fetchItemsByIDsLocked(db, missingIDs)
		if fErr != nil {
			return nil, fErr
		}
		maps.Copy(byID, fetched)
	}
	merged := make([]Item, 0, len(scores))
	for id, score := range scores {
		if it, ok := byID[id]; ok {
			it.Score = score
			merged = append(merged, it)
		}
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Score != merged[j].Score {
			return merged[i].Score > merged[j].Score
		}
		return merged[i].UpdatedAt > merged[j].UpdatedAt
	})
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

// Search performs FTS5 + optional vector RRF hybrid search, or lists when query is empty.
func (s *Store) Search(f SearchFilter) ([]Item, error) {
	trimmed := strings.TrimSpace(f.Query)
	tokens := tokenizeFTS(f.Query)

	var qVec []float32
	var embedModel string
	if trimmed != "" {
		if fn := s.getQueryEmbedder(); fn != nil {
			if vec, model, err := fn(trimmed); err == nil && len(vec) > 0 && model != "" {
				qVec = vec
				embedModel = model
			}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return nil, err
	}
	if trimmed == "" || (len(tokens) == 0 && len(qVec) == 0) {
		return s.listLocked(db, f)
	}
	limit := clampLimit(f.Limit, 20)
	candLimit := limit
	if len(qVec) > 0 {
		candLimit = max(limit*3, 40)
	}
	var ftsItems []Item
	if len(tokens) > 0 {
		ftsItems, err = s.searchFTSWithFallbackLocked(db, f, tokens, candLimit)
		if err != nil {
			return nil, err
		}
	}
	if len(qVec) > 0 {
		return s.hybridFuseLocked(db, f, ftsItems, qVec, embedModel, limit)
	}
	return ftsItems, nil
}
