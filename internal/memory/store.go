// Package memory implements lighthouse-hosted shared agent memory, FTS5 search,
// local Claude/Codex harvesting, offline context caching, and async LLM curation.
package memory

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/bdatdo0601/fleet/internal/registry"
)

const (
	MaxBodyBytes    = 32 * 1024
	MaxSummaryRunes = 240
	MaxContextBytes = 3500
	MaxSyncBatch    = 50
)

var (
	// ValidKinds lists the supported memory categories.
	ValidKinds = []string{"preference", "feedback", "project", "reference"}
	// ValidTiers lists the supported memory injection tiers.
	ValidTiers = []string{"always", "searchable"}
	// ValidStatuses lists the lifecycle states of a memory row.
	ValidStatuses = []string{"active", "archived", "superseded"}

	slugPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,79}$`)
	errStoreClosed = newMemoryErr("memory store closed", 503)
)

const memorySchema = `
CREATE TABLE IF NOT EXISTS memories (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  scope           TEXT NOT NULL,
  key             TEXT NOT NULL,
  kind            TEXT NOT NULL DEFAULT 'project',
  tier            TEXT NOT NULL DEFAULT 'searchable',
  status          TEXT NOT NULL DEFAULT 'active',
  summary         TEXT NOT NULL DEFAULT '',
  body            TEXT NOT NULL,
  content_hash    TEXT NOT NULL DEFAULT '',
  source          TEXT NOT NULL DEFAULT 'fleet',
  locked          INTEGER NOT NULL DEFAULT 0,
  device          TEXT NOT NULL DEFAULT '',
  harness         TEXT NOT NULL DEFAULT '',
  session_id      TEXT NOT NULL DEFAULT '',
  origin_path     TEXT NOT NULL DEFAULT '',
  superseded_by   TEXT NOT NULL DEFAULT '',
  revision        INTEGER NOT NULL DEFAULT 1,
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL,
  source_mtime    TEXT NOT NULL DEFAULT '',
  embedding       BLOB,
  embedding_model TEXT NOT NULL DEFAULT '',
  UNIQUE(scope, key)
);
CREATE INDEX IF NOT EXISTS idx_memories_scope_status_tier
  ON memories(scope, status, tier, updated_at DESC);

CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
  scope,
  key,
  kind,
  summary,
  body,
  content='memories',
  content_rowid='id',
  tokenize='porter unicode61'
);

CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
  INSERT INTO memories_fts(rowid, scope, key, kind, summary, body)
  VALUES (new.id, new.scope, new.key, new.kind, new.summary, new.body);
END;

CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
  INSERT INTO memories_fts(memories_fts, rowid, scope, key, kind, summary, body)
  VALUES ('delete', old.id, old.scope, old.key, old.kind, old.summary, old.body);
END;

CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN
  INSERT INTO memories_fts(memories_fts, rowid, scope, key, kind, summary, body)
  VALUES ('delete', old.id, old.scope, old.key, old.kind, old.summary, old.body);
  INSERT INTO memories_fts(rowid, scope, key, kind, summary, body)
  VALUES (new.id, new.scope, new.key, new.kind, new.summary, new.body);
END;

CREATE TABLE IF NOT EXISTS memory_revisions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  memory_id  INTEGER NOT NULL,
  scope      TEXT NOT NULL,
  key        TEXT NOT NULL,
  revision   INTEGER NOT NULL,
  kind       TEXT NOT NULL,
  tier       TEXT NOT NULL,
  status     TEXT NOT NULL,
  summary    TEXT NOT NULL,
  body       TEXT NOT NULL,
  source     TEXT NOT NULL,
  device     TEXT NOT NULL,
  harness    TEXT NOT NULL,
  session_id TEXT NOT NULL,
  reason     TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_memory_revisions_lookup
  ON memory_revisions(scope, key, revision DESC);

CREATE TABLE IF NOT EXISTS memory_proposals (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  action           TEXT NOT NULL,
  source_memory_id INTEGER NOT NULL DEFAULT 0,
  target_scope     TEXT NOT NULL,
  target_key       TEXT NOT NULL,
  kind             TEXT NOT NULL,
  tier             TEXT NOT NULL,
  summary          TEXT NOT NULL,
  body             TEXT NOT NULL,
  reason           TEXT NOT NULL,
  status           TEXT NOT NULL DEFAULT 'pending',
  model            TEXT NOT NULL DEFAULT '',
  created_at       TEXT NOT NULL,
  resolved_at      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_memory_proposals_status
  ON memory_proposals(status, created_at DESC);

CREATE TABLE IF NOT EXISTS curation_jobs (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  memory_id  INTEGER NOT NULL UNIQUE,
  status     TEXT NOT NULL DEFAULT 'pending',
  attempts   INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_curation_jobs_status
  ON curation_jobs(status, updated_at ASC);
`

// MemoryError is a caller-level error carrying an HTTP status code.
type MemoryError struct {
	Msg    string
	Status int
}

func (e *MemoryError) Error() string { return e.Msg }

// HTTPStatus returns the HTTP status code associated with this error.
func (e *MemoryError) HTTPStatus() int { return e.Status }

func newMemoryErr(msg string, status int) *MemoryError {
	if status == 0 {
		status = 400
	}
	return &MemoryError{Msg: msg, Status: status}
}

// Item represents a stored memory entry.
type Item struct {
	ID           int64   `json:"id"`
	Scope        string  `json:"scope"`
	Key          string  `json:"key"`
	Kind         string  `json:"kind"`
	Tier         string  `json:"tier"`
	Status       string  `json:"status"`
	Summary      string  `json:"summary"`
	Body         string  `json:"body"`
	ContentHash  string  `json:"content_hash"`
	Source       string  `json:"source"`
	Locked       bool    `json:"locked"`
	Device       string  `json:"device"`
	Harness      string  `json:"harness"`
	SessionID    string  `json:"session_id"`
	OriginPath   string  `json:"origin_path,omitempty"`
	SupersededBy string  `json:"superseded_by,omitempty"`
	Revision     int     `json:"revision"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
	SourceMtime  string  `json:"source_mtime,omitempty"`
	Score        float64 `json:"score,omitempty"`
}

// Revision captures one historical version of a memory item.
type Revision struct {
	ID        int64  `json:"id"`
	MemoryID  int64  `json:"memory_id"`
	Scope     string `json:"scope"`
	Key       string `json:"key"`
	Revision  int    `json:"revision"`
	Kind      string `json:"kind"`
	Tier      string `json:"tier"`
	Status    string `json:"status"`
	Summary   string `json:"summary"`
	Body      string `json:"body"`
	Source    string `json:"source"`
	Device    string `json:"device"`
	Harness   string `json:"harness"`
	SessionID string `json:"session_id"`
	Reason    string `json:"reason"`
	CreatedAt string `json:"created_at"`
}

// ItemDetail pairs a memory item with optional revision history.
type ItemDetail struct {
	Item    Item       `json:"memory"`
	History []Revision `json:"history,omitempty"`
}

// Proposal represents a curator-staged change awaiting review (e.g. global promotion).
type Proposal struct {
	ID             int64  `json:"id"`
	Action         string `json:"action"`
	SourceMemoryID int64  `json:"source_memory_id"`
	TargetScope    string `json:"target_scope"`
	TargetKey      string `json:"target_key"`
	Kind           string `json:"kind"`
	Tier           string `json:"tier"`
	Summary        string `json:"summary"`
	Body           string `json:"body"`
	Reason         string `json:"reason"`
	Status         string `json:"status"`
	Model          string `json:"model"`
	CreatedAt      string `json:"created_at"`
	ResolvedAt     string `json:"resolved_at,omitempty"`
}

// ProposalParams describes a new curator proposal to insert.
type ProposalParams struct {
	Action         string
	SourceMemoryID int64
	TargetScope    string
	TargetKey      string
	Kind           string
	Tier           string
	Summary        string
	Body           string
	Reason         string
	Model          string
}

// CurationJob tracks an async curation queue entry.
type CurationJob struct {
	ID        int64
	MemoryID  int64
	Status    string
	Attempts  int
	LastError string
	CreatedAt string
	UpdatedAt string
}

// UpsertParams holds fields for creating or updating a memory entry.
type UpsertParams struct {
	Scope         string `json:"scope"`
	Key           string `json:"key"`
	Kind          string `json:"kind"`
	Tier          string `json:"tier"`
	Status        string `json:"status,omitempty"`
	Summary       string `json:"summary"`
	Body          string `json:"body"`
	Source        string `json:"source,omitempty"`
	Locked        *bool  `json:"locked,omitempty"`
	Device        string `json:"device,omitempty"`
	Harness       string `json:"harness,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	OriginPath    string `json:"origin_path,omitempty"`
	SupersededBy  string `json:"superseded_by,omitempty"`
	SourceMtime   string `json:"source_mtime,omitempty"`
	Reason        string `json:"reason,omitempty"`
	EnqueueCurate *bool  `json:"enqueue_curate,omitempty"`
}

// UpsertResult reports the stored item and action taken.
type UpsertResult struct {
	Item   Item   `json:"memory"`
	Action string `json:"action"` // created, updated, unchanged, protected, stale
	Queued bool   `json:"queued"`
}

// SyncBatchResult summarizes a batch backup upload.
type SyncBatchResult struct {
	Created   int  `json:"created"`
	Updated   int  `json:"updated"`
	Unchanged int  `json:"unchanged"`
	Protected int  `json:"protected"`
	Stale     int  `json:"stale"`
	Queued    int  `json:"queued"`
	AnyQueued bool `json:"any_queued"`
}

// SearchFilter filters List and Search queries.
type SearchFilter struct {
	Query           string
	Scope           string
	Scopes          []string
	Kind            string
	Tier            string
	Status          string
	IncludeArchived bool
	Limit           int
}

// ContextResult contains rendered SessionStart prompt memory and scope counts.
type ContextResult struct {
	Scopes          []string `json:"scopes"`
	AlwaysCount     int      `json:"always_count"`
	SearchableCount int      `json:"searchable_count"`
	TotalCount      int      `json:"total_count"`
	Rendered        string   `json:"rendered"`
}

// Stats summarizes memory entry counts, revisions, curation queue, and scopes.
type Stats struct {
	Active           int      `json:"active"`
	Archived         int      `json:"archived"`
	Superseded       int      `json:"superseded"`
	AlwaysCount      int      `json:"always_count"`
	SearchableCount  int      `json:"searchable_count"`
	TotalRevisions   int      `json:"total_revisions"`
	PendingProposals int      `json:"pending_proposals"`
	PendingJobs      int      `json:"pending_jobs"`
	FailedJobs       int      `json:"failed_jobs"`
	Scopes           []string `json:"scopes"`
}

// Store wraps access to memory.db.
type Store struct {
	Path          string
	mu            sync.Mutex
	db            *sql.DB
	embedMu       sync.RWMutex
	queryEmbedder func(string) ([]float32, string, error)
	vecCacheModel string
	vecCache      map[int64][]float32
	vecCacheValid bool
}

// SetQueryEmbedder registers an optional query embedding callback for hybrid semantic search.
func (s *Store) SetQueryEmbedder(fn func(string) ([]float32, string, error)) {
	s.embedMu.Lock()
	s.queryEmbedder = fn
	s.embedMu.Unlock()
}

func (s *Store) getQueryEmbedder() func(string) ([]float32, string, error) {
	s.embedMu.RLock()
	fn := s.queryEmbedder
	s.embedMu.RUnlock()
	return fn
}

func (s *Store) invalidateVectorCache() {
	s.embedMu.Lock()
	s.vecCacheValid = false
	s.vecCache = nil
	s.vecCacheModel = ""
	s.embedMu.Unlock()
}

// Open opens or initializes memory.db at path.
func Open(path string) (*Store, error) {
	db, err := registry.OpenSQLite(path, memorySchema, "", nil)
	if err != nil {
		return nil, err
	}
	_, _ = db.Exec("UPDATE curation_jobs SET status = 'pending' WHERE status = 'running'")
	dedupePendingProposals(db)
	return &Store{Path: path, db: db}, nil
}

// Close closes the underlying SQLite connection.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *Store) dbLocked() (*sql.DB, error) {
	if s.db == nil {
		return nil, errStoreClosed
	}
	return s.db, nil
}

// TruncateRunes truncates s to at most maxRunes runes, appending an ellipsis when truncated.
func TruncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	if maxRunes <= 1 {
		return "…"
	}
	var b strings.Builder
	count := 0
	for _, r := range s {
		if count >= maxRunes-1 {
			break
		}
		b.WriteRune(r)
		count++
	}
	b.WriteRune('…')
	return b.String()
}

// truncateUTF8Bytes truncates s so len(out) <= maxBytes without splitting a multi-byte UTF-8 rune.
func truncateUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := s[:maxBytes]
	for len(cut) > 0 {
		r, size := utf8.DecodeLastRuneInString(cut)
		if r != utf8.RuneError || size > 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut
}

// NormalizeSlug normalizes an arbitrary string into a slug safe for scopes and keys.
func NormalizeSlug(raw string, maxLen int) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_':
			b.WriteRune(r)
			prevDash = false
		case r == '-' || r == '.' || r == '/' || unicode.IsSpace(r):
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-_")
	if maxLen > 0 && len(out) > maxLen {
		out = strings.TrimRight(out[:maxLen], "-_")
	}
	return out
}

// NormalizeScope validates and canonicalizes global, repo:<slug>, or device:<slug>.
func NormalizeScope(scope string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(scope))
	if s == "" {
		return "", newMemoryErr("scope is required", 400)
	}
	if s == "global" {
		return "global", nil
	}
	for _, prefix := range []string{"repo:", "device:"} {
		if after, ok := strings.CutPrefix(s, prefix); ok {
			slug := strings.TrimSpace(after)
			if !slugPattern.MatchString(slug) {
				return "", newMemoryErr(fmt.Sprintf("invalid %s scope slug %q", strings.TrimSuffix(prefix, ":"), slug), 400)
			}
			return prefix + slug, nil
		}
	}
	return "", newMemoryErr("scope must be global, repo:<name>, or device:<name>", 400)
}

// NormalizeKey converts key into a normalized slug up to 80 chars.
func NormalizeKey(key string) (string, error) {
	norm := NormalizeSlug(key, 80)
	if norm == "" {
		return "", newMemoryErr("key is required", 400)
	}
	return norm, nil
}

// NormalizeKind validates and normalizes a memory kind.
func NormalizeKind(kind string) (string, error) {
	k := strings.ToLower(strings.TrimSpace(kind))
	if k == "" {
		return "project", nil
	}
	if k == "user" {
		return "preference", nil
	}
	if !slices.Contains(ValidKinds, k) {
		return "", newMemoryErr("kind must be one of "+strings.Join(ValidKinds, ", "), 400)
	}
	return k, nil
}

// NormalizeTier validates and defaults a memory tier.
func NormalizeTier(tier, scope, kind string) (string, error) {
	t := strings.ToLower(strings.TrimSpace(tier))
	if t == "" {
		if scope == "global" && (kind == "preference" || kind == "feedback") {
			return "always", nil
		}
		return "searchable", nil
	}
	if !slices.Contains(ValidTiers, t) {
		return "", newMemoryErr("tier must be one of "+strings.Join(ValidTiers, ", "), 400)
	}
	return t, nil
}

// NormalizeStatus validates a memory status string.
func NormalizeStatus(status string) (string, error) {
	st := strings.ToLower(strings.TrimSpace(status))
	if st == "" {
		return "active", nil
	}
	if !slices.Contains(ValidStatuses, st) {
		return "", newMemoryErr("status must be one of "+strings.Join(ValidStatuses, ", "), 400)
	}
	return st, nil
}

// ContentHash returns a stable SHA-256 hex digest of summary and body.
func ContentHash(summary, body string) string {
	h := sha256.Sum256([]byte(strings.TrimSpace(summary) + "\n" + strings.TrimSpace(body)))
	return hex.EncodeToString(h[:])
}

// DeriveSummary produces a concise single-line summary from explicit summary or body text.
func DeriveSummary(summary, body string) string {
	s := strings.Join(strings.Fields(strings.TrimSpace(summary)), " ")
	if s != "" {
		return TruncateRunes(s, MaxSummaryRunes)
	}
	for ln := range strings.SplitSeq(body, "\n") {
		t := strings.TrimSpace(ln)
		t = strings.TrimLeft(t, "#-*•> ")
		t = strings.TrimSpace(t)
		if t != "" {
			return TruncateRunes(strings.Join(strings.Fields(t), " "), MaxSummaryRunes)
		}
	}
	return ""
}

func (s *Store) listScopesLocked(db *sql.DB) ([]string, error) {
	rows, err := db.Query(
		`SELECT DISTINCT scope FROM memories
		 ORDER BY CASE WHEN scope = 'global' THEN 0 ELSE 1 END, scope ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	scopes := []string{}
	for rows.Next() {
		var sc string
		if err := rows.Scan(&sc); err != nil {
			return nil, err
		}
		scopes = append(scopes, sc)
	}
	return scopes, rows.Err()
}

// ListScopes returns all distinct memory scopes with global first.
func (s *Store) ListScopes() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return nil, err
	}
	return s.listScopesLocked(db)
}

// Stats returns aggregate counts and scopes across memory.db.
func (s *Store) Stats() (Stats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return Stats{}, err
	}

	var st Stats
	if err := db.QueryRow(`
		SELECT
			COALESCE(SUM(CASE WHEN status = 'active' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'archived' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'superseded' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'active' AND tier = 'always' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'active' AND tier = 'searchable' THEN 1 ELSE 0 END), 0)
		FROM memories`,
	).Scan(&st.Active, &st.Archived, &st.Superseded, &st.AlwaysCount, &st.SearchableCount); err != nil {
		return Stats{}, err
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM memory_revisions").Scan(&st.TotalRevisions); err != nil {
		return Stats{}, err
	}
	pending, failed, proposals, err := s.jobCountsLocked(db)
	if err != nil {
		return Stats{}, err
	}
	st.PendingJobs = pending
	st.FailedJobs = failed
	st.PendingProposals = proposals
	scopes, err := s.listScopesLocked(db)
	if err != nil {
		return Stats{}, err
	}
	st.Scopes = scopes
	return st, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func isErrNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}
