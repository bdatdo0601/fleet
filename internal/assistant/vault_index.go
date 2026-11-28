package assistant

import (
	"cmp"
	"context"
	"database/sql"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/bdatdo0601/fleet/internal/memory"
	"github.com/bdatdo0601/fleet/internal/registry"
)

const (
	defaultVaultChunkChars = 1600
	vaultMinCosine         = 0.25
	vaultRRFK              = 60.0
)

type vaultChunk struct {
	heading string
	text    string
}

type vaultFileState struct {
	mtimeNs   int64
	sizeBytes int64
}

type vaultChunkRow struct {
	id        int64
	vaultName string
	relPath   string
	heading   string
	text      string
}

type vaultChunkHit struct {
	vaultName string
	relPath   string
	heading   string
	text      string
	score     float64
}

type vaultChunkVec struct {
	id        int64
	vaultName string
	relPath   string
	heading   string
	text      string
	vec       []float32
}

func chunkMarkdown(text string, maxChars int) []vaultChunk {
	if maxChars <= 0 {
		maxChars = defaultVaultChunkChars
	}
	var sections []vaultChunk
	var h1, h2, h3 string
	var cur strings.Builder

	currentHeading := func() string {
		var parts []string
		for _, h := range []string{h1, h2, h3} {
			if h != "" {
				parts = append(parts, h)
			}
		}
		return strings.Join(parts, " > ")
	}
	flush := func() {
		body := strings.TrimSpace(cur.String())
		cur.Reset()
		if body == "" {
			return
		}
		h := currentHeading()
		for _, part := range splitChunkBody(body, maxChars) {
			sections = append(sections, vaultChunk{heading: h, text: part})
		}
	}

	for line := range strings.SplitSeq(text, "\n") {
		trim := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trim, "# "):
			flush()
			h1 = strings.TrimSpace(trim[2:])
			h2, h3 = "", ""
		case strings.HasPrefix(trim, "## "):
			flush()
			h2 = strings.TrimSpace(trim[3:])
			h3 = ""
		case strings.HasPrefix(trim, "### "):
			flush()
			h3 = strings.TrimSpace(trim[4:])
		default:
			cur.WriteString(line)
			cur.WriteByte('\n')
		}
	}
	flush()
	return sections
}

func splitChunkBody(body string, maxChars int) []string {
	if len(body) <= maxChars {
		return []string{body}
	}
	paras := strings.Split(body, "\n\n")
	var out []string
	var cur strings.Builder
	flushCur := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			out = append(out, t)
		}
		cur.Reset()
	}
	for _, p := range paras {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if cur.Len() > 0 && cur.Len()+2+len(p) > maxChars {
			flushCur()
		}
		if len(p) <= maxChars {
			if cur.Len() > 0 {
				cur.WriteString("\n\n")
			}
			cur.WriteString(p)
			continue
		}
		for len(p) > maxChars {
			cut := maxChars
			if idx := strings.LastIndexAny(p[:cut], " \n"); idx > maxChars/2 {
				cut = idx
			}
			if part := strings.TrimSpace(p[:cut]); part != "" {
				out = append(out, part)
			}
			p = strings.TrimSpace(p[cut:])
		}
		if p != "" {
			cur.WriteString(p)
		}
	}
	flushCur()
	return out
}

// VaultIndexFileStates returns tracked (mtime_ns, size_bytes) per rel_path for vaultName.
func (s *Store) VaultIndexFileStates(vaultName string) (map[string]vaultFileState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT rel_path, MAX(mtime_ns), MAX(size_bytes) FROM assistant_vault_chunks WHERE vault_name = ? GROUP BY rel_path`,
		vaultName,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]vaultFileState)
	for rows.Next() {
		var rel string
		var st vaultFileState
		if err := rows.Scan(&rel, &st.mtimeNs, &st.sizeBytes); err != nil {
			return nil, err
		}
		out[rel] = st
	}
	return out, rows.Err()
}

// ReplaceVaultFileChunks atomically replaces indexed chunks for one vault Markdown file.
func (s *Store) ReplaceVaultFileChunks(vaultName, relPath string, mtimeNs, sizeBytes int64, chunks []vaultChunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM assistant_vault_chunks WHERE vault_name = ? AND rel_path = ?`, vaultName, relPath); err != nil {
		return err
	}
	now := registry.NowISO()
	for idx, ch := range chunks {
		if _, err := tx.Exec(
			`INSERT INTO assistant_vault_chunks(vault_name, rel_path, mtime_ns, size_bytes, heading, chunk_idx, text, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			vaultName, relPath, mtimeNs, sizeBytes, ch.heading, idx, ch.text, now,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteVaultFileChunks removes all chunks for a deleted vault file.
func (s *Store) DeleteVaultFileChunks(vaultName, relPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM assistant_vault_chunks WHERE vault_name = ? AND rel_path = ?`, vaultName, relPath)
	return err
}

func (s *Store) listVaultChunksNeedingEmbedding(vaultName, model string, limit int) ([]vaultChunkRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 128 {
		limit = 32
	}
	rows, err := s.db.Query(
		`SELECT id, vault_name, rel_path, heading, text
		 FROM assistant_vault_chunks
		 WHERE vault_name = ? AND (embedding IS NULL OR embedding_model != ?)
		 ORDER BY id ASC LIMIT ?`,
		vaultName, model, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []vaultChunkRow
	for rows.Next() {
		var r vaultChunkRow
		if err := rows.Scan(&r.id, &r.vaultName, &r.relPath, &r.heading, &r.text); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) setVaultChunkEmbedding(id int64, model string, blob []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE assistant_vault_chunks SET embedding = ?, embedding_model = ? WHERE id = ?`, blob, model, id)
	return err
}

// VaultChunkCounts returns total indexed chunks and how many have embeddings for model.
func (s *Store) VaultChunkCounts(model string) (chunks, embedded int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM assistant_vault_chunks`).Scan(&chunks); err != nil {
		return 0, 0, err
	}
	if strings.TrimSpace(model) == "" {
		return chunks, 0, nil
	}
	if err = s.db.QueryRow(
		`SELECT COUNT(*) FROM assistant_vault_chunks WHERE embedding IS NOT NULL AND embedding_model = ?`,
		model,
	).Scan(&embedded); err != nil {
		return 0, 0, err
	}
	return chunks, embedded, nil
}

func (s *Store) searchVaultChunksFTS(vaultName, matchExpr string, limit int) ([]vaultChunkHit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows *sql.Rows
	var err error
	if vaultName != "" && vaultName != "all" {
		rows, err = s.db.Query(
			`SELECT c.vault_name, c.rel_path, c.heading, c.text, -bm25(assistant_vault_chunks_fts, 0.5, 2.0, 2.5, 1.0) AS score
			 FROM assistant_vault_chunks_fts
			 JOIN assistant_vault_chunks c ON c.id = assistant_vault_chunks_fts.rowid
			 WHERE assistant_vault_chunks_fts MATCH ? AND c.vault_name = ?
			 ORDER BY bm25(assistant_vault_chunks_fts, 0.5, 2.0, 2.5, 1.0) ASC
			 LIMIT ?`,
			matchExpr, vaultName, limit,
		)
	} else {
		rows, err = s.db.Query(
			`SELECT c.vault_name, c.rel_path, c.heading, c.text, -bm25(assistant_vault_chunks_fts, 0.5, 2.0, 2.5, 1.0) AS score
			 FROM assistant_vault_chunks_fts
			 JOIN assistant_vault_chunks c ON c.id = assistant_vault_chunks_fts.rowid
			 WHERE assistant_vault_chunks_fts MATCH ?
			 ORDER BY bm25(assistant_vault_chunks_fts, 0.5, 2.0, 2.5, 1.0) ASC
			 LIMIT ?`,
			matchExpr, limit,
		)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []vaultChunkHit
	for rows.Next() {
		var h vaultChunkHit
		if err := rows.Scan(&h.vaultName, &h.relPath, &h.heading, &h.text, &h.score); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) listVaultChunkVectors(vaultName, model string) ([]vaultChunkVec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows *sql.Rows
	var err error
	if vaultName != "" && vaultName != "all" {
		rows, err = s.db.Query(
			`SELECT id, vault_name, rel_path, heading, text, embedding
			 FROM assistant_vault_chunks
			 WHERE vault_name = ? AND embedding IS NOT NULL AND embedding_model = ?`,
			vaultName, model,
		)
	} else {
		rows, err = s.db.Query(
			`SELECT id, vault_name, rel_path, heading, text, embedding
			 FROM assistant_vault_chunks
			 WHERE embedding IS NOT NULL AND embedding_model = ?`,
			model,
		)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []vaultChunkVec
	for rows.Next() {
		var v vaultChunkVec
		var blob []byte
		if err := rows.Scan(&v.id, &v.vaultName, &v.relPath, &v.heading, &v.text, &blob); err != nil {
			return nil, err
		}
		if vec := memory.DecodeVector(blob); len(vec) > 0 {
			v.vec = vec
			out = append(out, v)
		}
	}
	return out, rows.Err()
}

func tokenizeVaultFTS(raw string) []string {
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

func (e *Engine) syncVaultFiles(ctx context.Context, vaultName, rootDir string) error {
	if strings.TrimSpace(rootDir) == "" {
		return nil
	}
	prevStates, err := e.store.VaultIndexFileStates(vaultName)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(prevStates))
	walkErr := filepath.WalkDir(rootDir, func(path string, d fs.DirEntry, wErr error) error {
		if wErr != nil || d == nil {
			return filepath.SkipDir
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == ".obsidian" || name == ".trash" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil
		}
		if rel, err := filepath.Rel(rootDir, path); err == nil {
			rel = filepath.ToSlash(rel)
			seen[rel] = struct{}{}
			if info, err := d.Info(); err == nil {
				mtimeNs := info.ModTime().UnixNano()
				sizeBytes := info.Size()
				if st, ok := prevStates[rel]; ok && st.mtimeNs == mtimeNs && st.sizeBytes == sizeBytes {
					return nil
				}
				if data, err := os.ReadFile(path); err == nil {
					chunks := chunkMarkdown(string(data), defaultVaultChunkChars)
					return e.store.ReplaceVaultFileChunks(vaultName, rel, mtimeNs, sizeBytes, chunks)
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	for oldRel := range prevStates {
		if _, ok := seen[oldRel]; !ok {
			_ = e.store.DeleteVaultFileChunks(vaultName, oldRel)
		}
	}
	return nil
}

func (e *Engine) embedSyncDue(vaultName string) bool {
	e.vaultSyncMu.Lock()
	defer e.vaultSyncMu.Unlock()
	if e.lastEmbedSync == nil {
		e.lastEmbedSync = make(map[string]time.Time)
	}
	if time.Since(e.lastEmbedSync[vaultName]) < 15*time.Second {
		return false
	}
	e.lastEmbedSync[vaultName] = time.Now()
	return true
}

func (e *Engine) recordVaultEmbedSync(vaultName string) {
	e.vaultSyncMu.Lock()
	if e.lastEmbedSync == nil {
		e.lastEmbedSync = make(map[string]time.Time)
	}
	e.lastEmbedSync[vaultName] = time.Now()
	e.vaultSyncMu.Unlock()
}

// SyncVaultIndex incrementally updates chunked FTS5 + embedding records for one Obsidian vault.
func (e *Engine) SyncVaultIndex(ctx context.Context, vaultName, rootDir string, embedCfg memory.EmbedConfig) error {
	if err := e.syncVaultFiles(ctx, vaultName, rootDir); err != nil {
		return err
	}
	if embedCfg.Enabled && strings.TrimSpace(embedCfg.Model) != "" {
		e.recordVaultEmbedSync(vaultName)
		return e.backfillVaultEmbeddings(ctx, vaultName, embedCfg, 32)
	}
	return nil
}

func (e *Engine) backfillVaultEmbeddings(ctx context.Context, vaultName string, embedCfg memory.EmbedConfig, limit int) error {
	rows, err := e.store.listVaultChunksNeedingEmbedding(vaultName, embedCfg.Model, limit)
	if err != nil || len(rows) == 0 {
		return err
	}
	texts := make([]string, len(rows))
	for i, r := range rows {
		texts[i] = truncateText(r.relPath+"\n"+r.heading+"\n"+r.text, 2000)
	}
	vecs, err := memory.EmbedTexts(ctx, &http.Client{Timeout: 30 * time.Second}, embedCfg, texts)
	if err != nil {
		return err
	}
	for i, r := range rows {
		if blob := memory.EncodeVector(vecs[i]); len(blob) > 0 {
			_ = e.store.setVaultChunkEmbedding(r.id, embedCfg.Model, blob)
		}
	}
	return nil
}

// SyncAllVaultIndexes incrementally indexes the primary agent vault and all mounted extra vaults.
func (e *Engine) SyncAllVaultIndexes(ctx context.Context) {
	cfg := e.Config()
	if pv, err := e.vault(); err == nil {
		_ = e.SyncVaultIndex(ctx, PrimaryVaultName, pv.Root, cfg.Embeddings)
	}
	for _, m := range cfg.ExtraVaults {
		if v, err := OpenVaultNoScaffold(ResolveVaultRoot(m.Path, "")); err == nil {
			_ = e.SyncVaultIndex(ctx, m.Name, v.Root, cfg.Embeddings)
		}
	}
}

func (e *Engine) maybeSyncVaultIndex(ctx context.Context, vaultName string) {
	cfg := e.Config()
	syncOne := func(name, root string) {
		if err := e.syncVaultFiles(ctx, name, root); err != nil {
			return
		}
		if cfg.Embeddings.Enabled && strings.TrimSpace(cfg.Embeddings.Model) != "" && e.embedSyncDue(name) {
			_ = e.backfillVaultEmbeddings(ctx, name, cfg.Embeddings, 32)
		}
	}
	target := strings.ToLower(strings.TrimSpace(vaultName))
	if target == "" || target == "all" {
		if pv, err := e.vault(); err == nil {
			syncOne(PrimaryVaultName, pv.Root)
		}
		for _, m := range cfg.ExtraVaults {
			if v, err := OpenVaultNoScaffold(ResolveVaultRoot(m.Path, "")); err == nil {
				syncOne(m.Name, v.Root)
			}
		}
		return
	}
	if v, mount, err := e.ResolveVault(target); err == nil {
		syncOne(mount.Name, v.Root)
	}
}

// SearchVaultIndex runs FTS5 + optional cosine vector RRF search across indexed vault chunks.
func (e *Engine) SearchVaultIndex(ctx context.Context, vaultName, query string, limit int, embedCfg memory.EmbedConfig) ([]VaultSearchHit, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return nil, nil
	}
	target := strings.ToLower(strings.TrimSpace(vaultName))
	candLimit := max(limit*3, 30)

	tokens := tokenizeVaultFTS(trimmed)
	var ftsHits []vaultChunkHit
	if len(tokens) > 0 {
		var err error
		ftsHits, err = e.store.searchVaultChunksFTS(target, strings.Join(tokens, " "), candLimit)
		if err != nil {
			return nil, err
		}
		if len(ftsHits) == 0 && len(tokens) > 1 {
			ftsHits, err = e.store.searchVaultChunksFTS(target, strings.Join(tokens, " OR "), candLimit)
			if err != nil {
				return nil, err
			}
		}
	}

	var vecHits []vaultChunkHit
	if embedCfg.Enabled && strings.TrimSpace(embedCfg.Model) != "" {
		vecHits = e.searchVaultVectors(ctx, target, trimmed, candLimit, embedCfg)
	}
	return fuseVaultChunkHits(ftsHits, vecHits, strings.Fields(strings.ToLower(trimmed)), limit), nil
}

func (e *Engine) searchVaultVectors(ctx context.Context, vaultName, query string, limit int, embedCfg memory.EmbedConfig) []vaultChunkHit {
	qCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	vecs, err := memory.EmbedTexts(qCtx, &http.Client{Timeout: 2 * time.Second}, embedCfg, []string{query})
	if err != nil || len(vecs) == 0 {
		return nil
	}
	qVec := memory.DecodeVector(memory.EncodeVector(vecs[0]))
	if len(qVec) == 0 {
		return nil
	}
	cands, err := e.store.listVaultChunkVectors(vaultName, embedCfg.Model)
	if err != nil || len(cands) == 0 {
		return nil
	}
	var hits []vaultChunkHit
	for _, c := range cands {
		if sim := memory.DotProduct(qVec, c.vec); sim >= vaultMinCosine {
			hits = append(hits, vaultChunkHit{
				vaultName: c.vaultName,
				relPath:   c.relPath,
				heading:   c.heading,
				text:      c.text,
				score:     sim,
			})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

func fuseVaultChunkHits(ftsHits, vecHits []vaultChunkHit, terms []string, limit int) []VaultSearchHit {
	type docBest struct {
		hit   vaultChunkHit
		score float64
	}
	byDoc := make(map[string]*docBest)
	addList := func(list []vaultChunkHit) {
		seenDoc := make(map[string]struct{}, len(list))
		rank := 0
		for _, ch := range list {
			key := ch.vaultName + "\x00" + ch.relPath
			if _, ok := seenDoc[key]; ok {
				continue
			}
			seenDoc[key] = struct{}{}
			rank++
			rrf := 1.0 / (vaultRRFK + float64(rank))
			cur, exists := byDoc[key]
			if !exists {
				byDoc[key] = &docBest{hit: ch, score: rrf}
			} else {
				cur.score += rrf
				if cur.hit.heading == "" && ch.heading != "" {
					cur.hit.heading = ch.heading
				}
			}
		}
	}
	addList(ftsHits)
	addList(vecHits)

	ranked := make([]*docBest, 0, len(byDoc))
	for _, d := range byDoc {
		ranked = append(ranked, d)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].hit.relPath < ranked[j].hit.relPath
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out := make([]VaultSearchHit, len(ranked))
	for i, d := range ranked {
		out[i] = VaultSearchHit{
			Vault:   d.hit.vaultName,
			Path:    d.hit.relPath,
			Title:   cmp.Or(strings.TrimSuffix(filepath.Base(d.hit.relPath), ".md"), d.hit.relPath),
			Heading: d.hit.heading,
			Snippet: firstMatchingSnippet(d.hit.text, terms),
			Score:   d.score,
		}
	}
	return out
}
