// Package memory implements lighthouse-hosted shared agent memory, FTS5 search,
// local Claude/Codex harvesting, offline context caching, and async LLM curation.
package memory

import (
	"cmp"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
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

	slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,79}$`)
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

// Store wraps access to memory.db.
type Store struct {
	Path string
	mu   sync.Mutex
	db   *sql.DB
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

func dedupePendingProposals(db *sql.DB) {
	_, _ = db.Exec(`DELETE FROM memory_proposals
		WHERE status = 'pending' AND source_memory_id > 0 AND id NOT IN (
			SELECT MAX(id) FROM memory_proposals
			WHERE status = 'pending' AND source_memory_id > 0
			GROUP BY source_memory_id, action, target_scope
		)`)
	_, _ = db.Exec(`DELETE FROM memory_proposals
		WHERE status = 'pending' AND id NOT IN (
			SELECT MAX(id) FROM memory_proposals
			WHERE status = 'pending'
			GROUP BY target_scope, target_key
		)`)
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

const itemSelectCols = `m.id, m.scope, m.key, m.kind, m.tier, m.status, m.summary, m.body,
  m.content_hash, m.source, m.locked, m.device, m.harness, m.session_id, m.origin_path,
  m.superseded_by, m.revision, m.created_at, m.updated_at, m.source_mtime`

func scanItem(scanner interface{ Scan(...any) error }, withScore bool) (*Item, error) {
	var it Item
	var lockedInt int
	dest := []any{
		&it.ID, &it.Scope, &it.Key, &it.Kind, &it.Tier, &it.Status, &it.Summary, &it.Body,
		&it.ContentHash, &it.Source, &lockedInt, &it.Device, &it.Harness, &it.SessionID,
		&it.OriginPath, &it.SupersededBy, &it.Revision, &it.CreatedAt, &it.UpdatedAt, &it.SourceMtime,
	}
	if withScore {
		dest = append(dest, &it.Score)
	}
	if err := scanner.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	it.Locked = lockedInt != 0
	return &it, nil
}

func (s *Store) getByScopeKeyLocked(scope, key string) (*Item, error) {
	return scanItem(s.db.QueryRow(
		"SELECT "+itemSelectCols+" FROM memories m WHERE m.scope = ? AND m.key = ?",
		scope, key,
	), false)
}

func (s *Store) getByIDLocked(id int64) (*Item, error) {
	return scanItem(s.db.QueryRow(
		"SELECT "+itemSelectCols+" FROM memories m WHERE m.id = ?",
		id,
	), false)
}

func isBackupSource(src string) bool {
	return src == "claude_backup" || src == "codex_backup"
}

func parseFlexTime(raw string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, raw)
}

func isOlderMtime(incoming, existing string) bool {
	if incoming == "" || existing == "" {
		return false
	}
	t1, err1 := parseFlexTime(incoming)
	t2, err2 := parseFlexTime(existing)
	if err1 == nil && err2 == nil {
		return t1.Before(t2)
	}
	return incoming < existing
}

func checkBackupProtection(p UpsertParams, existing *Item, incomingHash string) (string, bool) {
	if existing == nil || !isBackupSource(p.Source) {
		return "", false
	}
	if existing.Locked || existing.Source == "human" || existing.Source == "fleet" {
		return "protected", true
	}
	if existing.ContentHash == incomingHash {
		return "unchanged", true
	}
	if isOlderMtime(p.SourceMtime, existing.SourceMtime) {
		return "stale", true
	}
	return "", false
}

func shouldQueueCuration(p UpsertParams, kind string) bool {
	if p.EnqueueCurate != nil {
		return *p.EnqueueCurate
	}
	switch p.Source {
	case "fleet":
		return true
	case "claude_backup", "codex_backup":
		return kind == "preference" || kind == "feedback"
	default:
		return false
	}
}

func insertRevisionTx(tx *sql.Tx, memID int64, scope, key string, rev int, kind, tier, status, summary, body, source, device, harness, sessionID, reason, now string) error {
	_, err := tx.Exec(
		`INSERT INTO memory_revisions
		 (memory_id, scope, key, revision, kind, tier, status, summary, body, source, device, harness, session_id, reason, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		memID, scope, key, rev, kind, tier, status, summary, body, source, device, harness, sessionID, reason, now,
	)
	return err
}

func enqueueJobTx(tx *sql.Tx, memID int64, now string) error {
	_, err := tx.Exec(
		`INSERT INTO curation_jobs(memory_id, status, attempts, last_error, created_at, updated_at)
		 VALUES (?, 'pending', 0, '', ?, ?)
		 ON CONFLICT(memory_id) DO UPDATE SET
		   status = 'pending', attempts = 0, last_error = '', updated_at = excluded.updated_at`,
		memID, now, now,
	)
	return err
}

func (s *Store) normalizeUpsert(p UpsertParams, existing *Item) (UpsertParams, error) {
	scope, err := NormalizeScope(p.Scope)
	if err != nil {
		return p, err
	}
	key, err := NormalizeKey(p.Key)
	if err != nil {
		return p, err
	}
	rawKind := p.Kind
	if rawKind == "" && existing != nil {
		rawKind = existing.Kind
	}
	kind, err := NormalizeKind(rawKind)
	if err != nil {
		return p, err
	}
	rawTier := p.Tier
	if rawTier == "" && existing != nil && existing.Scope == scope {
		rawTier = existing.Tier
	}
	tier, err := NormalizeTier(rawTier, scope, kind)
	if err != nil {
		return p, err
	}
	status, err := NormalizeStatus(p.Status)
	if err != nil {
		return p, err
	}
	body := strings.TrimSpace(p.Body)
	if body == "" {
		return p, newMemoryErr("body is required", 400)
	}
	if len(body) > MaxBodyBytes {
		return p, newMemoryErr(fmt.Sprintf("body exceeds %d bytes", MaxBodyBytes), 400)
	}
	p.Scope = scope
	p.Key = key
	p.Kind = kind
	p.Tier = tier
	p.Status = status
	p.Body = body
	p.Summary = DeriveSummary(p.Summary, body)
	p.Source = cmp.Or(strings.TrimSpace(p.Source), "fleet")
	return p, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) writeMemoryTx(existing *Item, p UpsertParams, hash string, locked bool) (int64, string, bool, error) {
	now := registry.NowISO()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, "", false, err
	}
	defer func() { _ = tx.Rollback() }()

	action := "created"
	rev := 1
	var memID int64
	if existing == nil {
		res, execErr := tx.Exec(
			`INSERT INTO memories
			 (scope, key, kind, tier, status, summary, body, content_hash, source, locked, device, harness, session_id, origin_path, superseded_by, revision, created_at, updated_at, source_mtime)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`,
			p.Scope, p.Key, p.Kind, p.Tier, p.Status, p.Summary, p.Body, hash, p.Source, boolToInt(locked),
			p.Device, p.Harness, p.SessionID, p.OriginPath, p.SupersededBy, now, now, p.SourceMtime,
		)
		if execErr != nil {
			return 0, "", false, execErr
		}
		memID, _ = res.LastInsertId()
	} else {
		action = "updated"
		rev = existing.Revision + 1
		memID = existing.ID
		if _, execErr := tx.Exec(
			`UPDATE memories
			 SET kind = ?, tier = ?, status = ?, summary = ?, body = ?, content_hash = ?,
			     source = ?, locked = ?, device = ?, harness = ?, session_id = ?, origin_path = ?,
			     superseded_by = ?, revision = ?, updated_at = ?, source_mtime = ?
			 WHERE id = ?`,
			p.Kind, p.Tier, p.Status, p.Summary, p.Body, hash,
			p.Source, boolToInt(locked), p.Device, p.Harness, p.SessionID, cmp.Or(p.OriginPath, existing.OriginPath),
			p.SupersededBy, rev, now, cmp.Or(p.SourceMtime, existing.SourceMtime), memID,
		); execErr != nil {
			return 0, "", false, execErr
		}
	}
	reason := cmp.Or(p.Reason, action)
	if err := insertRevisionTx(tx, memID, p.Scope, p.Key, rev, p.Kind, p.Tier, p.Status, p.Summary, p.Body, p.Source, p.Device, p.Harness, p.SessionID, reason, now); err != nil {
		return 0, "", false, err
	}
	queued := shouldQueueCuration(p, p.Kind)
	if queued {
		if err := enqueueJobTx(tx, memID, now); err != nil {
			return 0, "", false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, "", false, err
	}
	return memID, action, queued, nil
}

func (s *Store) upsertLocked(p UpsertParams) (UpsertResult, error) {
	scope, err := NormalizeScope(p.Scope)
	if err != nil {
		return UpsertResult{}, err
	}
	key, err := NormalizeKey(p.Key)
	if err != nil {
		return UpsertResult{}, err
	}
	existing, err := s.getByScopeKeyLocked(scope, key)
	if err != nil {
		return UpsertResult{}, err
	}
	p, err = s.normalizeUpsert(p, existing)
	if err != nil {
		return UpsertResult{}, err
	}
	hash := ContentHash(p.Summary, p.Body)
	if action, skip := checkBackupProtection(p, existing, hash); skip {
		return UpsertResult{Item: *existing, Action: action}, nil
	}

	locked := existing != nil && existing.Locked
	if p.Locked != nil {
		locked = *p.Locked
	}
	if existing != nil && existing.Kind == p.Kind && existing.Tier == p.Tier &&
		existing.Status == p.Status && existing.Summary == p.Summary &&
		existing.Body == p.Body && existing.Locked == locked &&
		existing.SupersededBy == p.SupersededBy {
		return UpsertResult{Item: *existing, Action: "unchanged"}, nil
	}

	memID, action, queued, err := s.writeMemoryTx(existing, p, hash, locked)
	if err != nil {
		return UpsertResult{}, err
	}
	saved, err := s.getByIDLocked(memID)
	if err != nil {
		return UpsertResult{}, err
	}
	if saved == nil {
		return UpsertResult{}, newMemoryErr("memory missing after upsert", 500)
	}
	return UpsertResult{Item: *saved, Action: action, Queued: queued}, nil
}

// Upsert creates or updates a memory entry and records a revision snapshot.
func (s *Store) Upsert(p UpsertParams) (UpsertResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upsertLocked(p)
}

// SyncBatch applies a batch of backup/sync upserts up to MaxSyncBatch items.
func (s *Store) SyncBatch(device string, items []UpsertParams) (SyncBatchResult, error) {
	if len(items) > MaxSyncBatch {
		return SyncBatchResult{}, newMemoryErr(fmt.Sprintf("batch exceeds %d items", MaxSyncBatch), 400)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var res SyncBatchResult
	for _, it := range items {
		if it.Device == "" {
			it.Device = device
		}
		if it.Source == "" {
			it.Source = "claude_backup"
		}
		ur, err := s.upsertLocked(it)
		if err != nil {
			return res, err
		}
		switch ur.Action {
		case "created":
			res.Created++
		case "updated":
			res.Updated++
		case "unchanged":
			res.Unchanged++
		case "protected":
			res.Protected++
		case "stale":
			res.Stale++
		}
		if ur.Queued {
			res.Queued++
			res.AnyQueued = true
		}
	}
	return res, nil
}

// Get retrieves a memory entry by scope and key, optionally including revision history.
func (s *Store) Get(scope, key string, includeHistory bool) (ItemDetail, error) {
	normScope, err := NormalizeScope(scope)
	if err != nil {
		return ItemDetail{}, err
	}
	normKey, err := NormalizeKey(key)
	if err != nil {
		return ItemDetail{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	it, err := s.getByScopeKeyLocked(normScope, normKey)
	if err != nil {
		return ItemDetail{}, err
	}
	if it == nil {
		return ItemDetail{}, newMemoryErr(fmt.Sprintf("memory %s/%s not found", normScope, normKey), 404)
	}
	detail := ItemDetail{Item: *it}
	if includeHistory {
		hist, err := s.historyLocked(normScope, normKey, 20)
		if err != nil {
			return ItemDetail{}, err
		}
		detail.History = hist
	}
	return detail, nil
}

// GetByID retrieves a memory entry by numeric ID, or nil if missing.
func (s *Store) GetByID(id int64) (*Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getByIDLocked(id)
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

func clampLimit(limit, def, maxVal int) int {
	if limit <= 0 {
		return def
	}
	return min(limit, maxVal)
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

func (s *Store) listLocked(f SearchFilter) ([]Item, error) {
	where, args, err := buildFilterWhere(f)
	if err != nil {
		return nil, err
	}
	limit := clampLimit(f.Limit, 20, 100)
	args = append(args, limit)
	q := "SELECT " + itemSelectCols + " FROM memories m WHERE " + where + " ORDER BY m.updated_at DESC, m.id DESC LIMIT ?"
	return queryItems(s.db, q, args, false)
}

// List returns matching memories ordered by recency.
func (s *Store) List(f SearchFilter) ([]Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked(f)
}

func (s *Store) searchFTSLocked(f SearchFilter, matchExpr string, limit int) ([]Item, error) {
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
	return queryItems(s.db, q, args, true)
}

// Search performs FTS5 ranked search with AND-to-OR fallback, or lists when query is empty.
func (s *Store) Search(f SearchFilter) ([]Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(f.Query) == "" {
		return s.listLocked(f)
	}
	tokens := tokenizeFTS(f.Query)
	if len(tokens) == 0 {
		return s.listLocked(f)
	}
	limit := clampLimit(f.Limit, 20, 100)
	items, err := s.searchFTSLocked(f, strings.Join(tokens, " "), limit)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 && len(tokens) > 1 {
		return s.searchFTSLocked(f, strings.Join(tokens, " OR "), limit)
	}
	return items, nil
}

// Delete soft-archives (or hard-deletes) a memory entry by scope and key.
func (s *Store) Delete(scope, key string, hard bool, device, harness, sessionID, reason string) error {
	normScope, err := NormalizeScope(scope)
	if err != nil {
		return err
	}
	normKey, err := NormalizeKey(key)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, err := s.getByScopeKeyLocked(normScope, normKey)
	if err != nil {
		return err
	}
	if existing == nil {
		return newMemoryErr(fmt.Sprintf("memory %s/%s not found", normScope, normKey), 404)
	}
	now := registry.NowISO()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if hard {
		if err := insertRevisionTx(tx, existing.ID, existing.Scope, existing.Key, existing.Revision+1, existing.Kind, existing.Tier, "deleted", existing.Summary, existing.Body, "human", device, harness, sessionID, cmp.Or(reason, "hard_delete"), now); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM curation_jobs WHERE memory_id = ?", existing.ID); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM memories WHERE id = ?", existing.ID); err != nil {
			return err
		}
		return tx.Commit()
	}

	rev := existing.Revision + 1
	src := "fleet"
	if harness == "" || harness == "cli" {
		src = "human"
		if _, err := tx.Exec(
			"UPDATE memories SET status = 'archived', source = 'human', locked = 1, revision = ?, updated_at = ? WHERE id = ?",
			rev, now, existing.ID,
		); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(
			"UPDATE memories SET status = 'archived', revision = ?, updated_at = ? WHERE id = ?",
			rev, now, existing.ID,
		); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("DELETE FROM curation_jobs WHERE memory_id = ?", existing.ID); err != nil {
		return err
	}
	if err := insertRevisionTx(tx, existing.ID, existing.Scope, existing.Key, rev, existing.Kind, existing.Tier, "archived", existing.Summary, existing.Body, src, device, harness, sessionID, cmp.Or(reason, "archived"), now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) historyLocked(scope, key string, limit int) ([]Revision, error) {
	limit = clampLimit(limit, 30, 200)
	var rows *sql.Rows
	var err error
	if scope != "" && key != "" {
		rows, err = s.db.Query(
			`SELECT id, memory_id, scope, key, revision, kind, tier, status, summary, body, source, device, harness, session_id, reason, created_at
			 FROM memory_revisions WHERE scope = ? AND key = ? ORDER BY revision DESC, id DESC LIMIT ?`,
			scope, key, limit,
		)
	} else {
		rows, err = s.db.Query(
			`SELECT id, memory_id, scope, key, revision, kind, tier, status, summary, body, source, device, harness, session_id, reason, created_at
			 FROM memory_revisions ORDER BY id DESC LIMIT ?`,
			limit,
		)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Revision{}
	for rows.Next() {
		var r Revision
		if err := rows.Scan(
			&r.ID, &r.MemoryID, &r.Scope, &r.Key, &r.Revision, &r.Kind, &r.Tier, &r.Status,
			&r.Summary, &r.Body, &r.Source, &r.Device, &r.Harness, &r.SessionID, &r.Reason, &r.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// History returns revision entries for one key or recent revisions across the store.
func (s *Store) History(scope, key string, limit int) ([]Revision, error) {
	var ns, nk string
	if scope != "" || key != "" {
		var err error
		if ns, err = NormalizeScope(scope); err != nil {
			return nil, err
		}
		if nk, err = NormalizeKey(key); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.historyLocked(ns, nk, limit)
}

// Revert restores a memory entry from a historical revision as a new human-authored revision.
func (s *Store) Revert(scope, key string, revision int, device string) (Item, error) {
	ns, err := NormalizeScope(scope)
	if err != nil {
		return Item{}, err
	}
	nk, err := NormalizeKey(key)
	if err != nil {
		return Item{}, err
	}
	if revision <= 0 {
		return Item{}, newMemoryErr("revision must be positive", 400)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var rev Revision
	err = s.db.QueryRow(
		`SELECT id, memory_id, scope, key, revision, kind, tier, status, summary, body
		 FROM memory_revisions WHERE scope = ? AND key = ? AND revision = ? ORDER BY id DESC LIMIT 1`,
		ns, nk, revision,
	).Scan(&rev.ID, &rev.MemoryID, &rev.Scope, &rev.Key, &rev.Revision, &rev.Kind, &rev.Tier, &rev.Status, &rev.Summary, &rev.Body)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, newMemoryErr(fmt.Sprintf("revision %d of %s/%s not found", revision, ns, nk), 404)
	}
	if err != nil {
		return Item{}, err
	}
	status := rev.Status
	if status == "deleted" {
		status = "active"
	}
	ur, err := s.upsertLocked(UpsertParams{
		Scope:         ns,
		Key:           nk,
		Kind:          rev.Kind,
		Tier:          rev.Tier,
		Status:        status,
		Summary:       rev.Summary,
		Body:          rev.Body,
		Source:        "human",
		Device:        device,
		Reason:        fmt.Sprintf("revert_to_r%d", revision),
		EnqueueCurate: new(false),
	})
	if err != nil {
		return Item{}, err
	}
	return ur.Item, nil
}

// Promote copies or moves a memory entry to a new scope/key/tier (defaulting to global/always).
func (s *Store) Promote(fromScope, fromKey, toScope, toKey, tier, summary, device string, lock bool) (Item, error) {
	fs, err := NormalizeScope(fromScope)
	if err != nil {
		return Item{}, err
	}
	fk, err := NormalizeKey(fromKey)
	if err != nil {
		return Item{}, err
	}
	ts, err := NormalizeScope(cmp.Or(toScope, "global"))
	if err != nil {
		return Item{}, err
	}
	tk, err := NormalizeKey(cmp.Or(toKey, fk))
	if err != nil {
		return Item{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	src, err := s.getByScopeKeyLocked(fs, fk)
	if err != nil {
		return Item{}, err
	}
	if src == nil {
		return Item{}, newMemoryErr(fmt.Sprintf("memory %s/%s not found", fs, fk), 404)
	}
	targetTier := cmp.Or(tier, "always")
	var lockPtr *bool
	if lock {
		lockPtr = new(true)
	}
	ur, err := s.upsertLocked(UpsertParams{
		Scope:         ts,
		Key:           tk,
		Kind:          src.Kind,
		Tier:          targetTier,
		Status:        "active",
		Summary:       cmp.Or(summary, src.Summary),
		Body:          src.Body,
		Source:        "human",
		Locked:        lockPtr,
		Device:        device,
		Reason:        fmt.Sprintf("promoted_from_%s/%s", fs, fk),
		EnqueueCurate: new(false),
	})
	if err != nil {
		return Item{}, err
	}
	return ur.Item, nil
}

const proposalSelectCols = "id, action, source_memory_id, target_scope, target_key, kind, tier, summary, body, reason, status, model, created_at, resolved_at"

func scanProposal(scanner interface{ Scan(...any) error }) (Proposal, error) {
	var p Proposal
	err := scanner.Scan(
		&p.ID, &p.Action, &p.SourceMemoryID, &p.TargetScope, &p.TargetKey,
		&p.Kind, &p.Tier, &p.Summary, &p.Body, &p.Reason, &p.Status, &p.Model, &p.CreatedAt, &p.ResolvedAt,
	)
	return p, err
}

// CreateProposal stores (or updates an existing pending) curator proposal for human review.
func (s *Store) CreateProposal(p ProposalParams) (Proposal, error) {
	ts, err := NormalizeScope(p.TargetScope)
	if err != nil {
		return Proposal{}, err
	}
	tk, err := NormalizeKey(p.TargetKey)
	if err != nil {
		return Proposal{}, err
	}
	kind, err := NormalizeKind(p.Kind)
	if err != nil {
		return Proposal{}, err
	}
	tier, err := NormalizeTier(p.Tier, ts, kind)
	if err != nil {
		return Proposal{}, err
	}
	now := registry.NowISO()
	action := cmp.Or(p.Action, "promote_global")
	body := strings.TrimSpace(p.Body)
	summary := DeriveSummary(p.Summary, body)

	s.mu.Lock()
	defer s.mu.Unlock()

	var existingID int64
	err = s.db.QueryRow(
		`SELECT id FROM memory_proposals
		 WHERE status = 'pending'
		   AND ((? > 0 AND source_memory_id = ? AND action = ? AND target_scope = ?)
		        OR (target_scope = ? AND target_key = ?))
		 ORDER BY id DESC LIMIT 1`,
		p.SourceMemoryID, p.SourceMemoryID, action, ts, ts, tk,
	).Scan(&existingID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Proposal{}, err
	}

	if existingID > 0 {
		if _, err := s.db.Exec(
			`UPDATE memory_proposals
			 SET target_scope = ?, target_key = ?, kind = ?, tier = ?, summary = ?, body = ?, reason = ?, model = ?, created_at = ?
			 WHERE id = ?`,
			ts, tk, kind, tier, summary, body, p.Reason, p.Model, now, existingID,
		); err != nil {
			return Proposal{}, err
		}
		return Proposal{
			ID: existingID, Action: action, SourceMemoryID: p.SourceMemoryID,
			TargetScope: ts, TargetKey: tk, Kind: kind, Tier: tier,
			Summary: summary, Body: body, Reason: p.Reason, Status: "pending",
			Model: p.Model, CreatedAt: now,
		}, nil
	}

	res, err := s.db.Exec(
		`INSERT INTO memory_proposals
		 (action, source_memory_id, target_scope, target_key, kind, tier, summary, body, reason, status, model, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
		action, p.SourceMemoryID, ts, tk, kind, tier, summary, body, p.Reason, p.Model, now,
	)
	if err != nil {
		return Proposal{}, err
	}
	id, _ := res.LastInsertId()
	return Proposal{
		ID: id, Action: action, SourceMemoryID: p.SourceMemoryID,
		TargetScope: ts, TargetKey: tk, Kind: kind, Tier: tier,
		Summary: summary, Body: body, Reason: p.Reason, Status: "pending",
		Model: p.Model, CreatedAt: now,
	}, nil
}

// GetProposal returns a single curator proposal by ID.
func (s *Store) GetProposal(id int64) (Proposal, error) {
	if id <= 0 {
		return Proposal{}, newMemoryErr("invalid proposal id", 400)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := scanProposal(s.db.QueryRow("SELECT "+proposalSelectCols+" FROM memory_proposals WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Proposal{}, newMemoryErr(fmt.Sprintf("proposal %d not found", id), 404)
	}
	if err != nil {
		return Proposal{}, err
	}
	return p, nil
}

// ListProposals returns curator proposals filtered by status (default all if empty).
func (s *Store) ListProposals(status string, limit int) ([]Proposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit = clampLimit(limit, 50, 200)
	var rows *sql.Rows
	var err error
	if status != "" {
		rows, err = s.db.Query(
			"SELECT "+proposalSelectCols+" FROM memory_proposals WHERE status = ? ORDER BY id DESC LIMIT ?",
			status, limit,
		)
	} else {
		rows, err = s.db.Query(
			"SELECT "+proposalSelectCols+" FROM memory_proposals ORDER BY id DESC LIMIT ?",
			limit,
		)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Proposal{}
	for rows.Next() {
		p, scanErr := scanProposal(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ResolveProposal approves or rejects a pending proposal, applying it when approved.
func (s *Store) ResolveProposal(id int64, approve bool, device string) (Proposal, *Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := scanProposal(s.db.QueryRow("SELECT "+proposalSelectCols+" FROM memory_proposals WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Proposal{}, nil, newMemoryErr(fmt.Sprintf("proposal %d not found", id), 404)
	}
	if err != nil {
		return Proposal{}, nil, err
	}
	if p.Status != "pending" {
		return Proposal{}, nil, newMemoryErr(fmt.Sprintf("proposal %d is already %s", id, p.Status), 409)
	}

	now := registry.NowISO()
	if !approve {
		if _, err := s.db.Exec("UPDATE memory_proposals SET status = 'rejected', resolved_at = ? WHERE id = ?", now, id); err != nil {
			return Proposal{}, nil, err
		}
		p.Status = "rejected"
		p.ResolvedAt = now
		return p, nil, nil
	}

	var applied *Item
	switch p.Action {
	case "archive":
		existing, gErr := s.getByScopeKeyLocked(p.TargetScope, p.TargetKey)
		if gErr != nil {
			return Proposal{}, nil, gErr
		}
		if existing != nil {
			ur, uErr := s.upsertLocked(UpsertParams{
				Scope: p.TargetScope, Key: p.TargetKey, Kind: existing.Kind, Tier: existing.Tier,
				Status: "archived", Summary: existing.Summary, Body: existing.Body,
				Source: "human", Device: device, Reason: fmt.Sprintf("proposal_%d_archive", id), EnqueueCurate: new(false),
			})
			if uErr != nil {
				return Proposal{}, nil, uErr
			}
			applied = &ur.Item
		}
	default:
		src := "curator"
		if p.TargetScope == "global" {
			src = "human"
		}
		ur, uErr := s.upsertLocked(UpsertParams{
			Scope:         p.TargetScope,
			Key:           p.TargetKey,
			Kind:          p.Kind,
			Tier:          p.Tier,
			Status:        "active",
			Summary:       p.Summary,
			Body:          p.Body,
			Source:        src,
			Device:        device,
			Reason:        fmt.Sprintf("proposal_%d_%s", id, p.Action),
			EnqueueCurate: new(false),
		})
		if uErr != nil {
			return Proposal{}, nil, uErr
		}
		applied = &ur.Item
	}

	if _, err := s.db.Exec("UPDATE memory_proposals SET status = 'approved', resolved_at = ? WHERE id = ?", now, id); err != nil {
		return Proposal{}, nil, err
	}
	p.Status = "approved"
	p.ResolvedAt = now
	return p, applied, nil
}

func contextScopes(repo, device string) ([]string, string) {
	scopes := []string{"global"}
	var repoScope string
	if r := NormalizeSlug(repo, 80); r != "" {
		if ns, err := NormalizeScope("repo:" + r); err == nil {
			scopes = append(scopes, ns)
			repoScope = ns
		}
	}
	if d := NormalizeSlug(device, 80); d != "" {
		if ns, err := NormalizeScope("device:" + d); err == nil && !slices.Contains(scopes, ns) {
			scopes = append(scopes, ns)
		}
	}
	return scopes, repoScope
}

func (s *Store) queryAlwaysForContext(scopes []string) ([]Item, error) {
	ph := strings.Repeat(",?", len(scopes))[1:]
	args := make([]any, 0, len(scopes))
	for _, sc := range scopes {
		args = append(args, sc)
	}
	q := "SELECT " + itemSelectCols + ` FROM memories m
		WHERE m.status = 'active' AND m.tier = 'always' AND m.scope IN (` + ph + `)
		ORDER BY CASE
		  WHEN m.scope = 'global' THEN 0
		  WHEN m.scope LIKE 'repo:%' THEN 1
		  ELSE 2
		END, CASE WHEN m.kind IN ('preference', 'feedback') THEN 0 ELSE 1 END, m.updated_at DESC`
	return queryItems(s.db, q, args, false)
}

func (s *Store) querySearchableForContext(repoScope string) ([]Item, error) {
	scopes := []string{"global"}
	if repoScope != "" {
		scopes = append(scopes, repoScope)
	}
	ph := strings.Repeat(",?", len(scopes))[1:]
	args := make([]any, 0, len(scopes))
	for _, sc := range scopes {
		args = append(args, sc)
	}
	q := "SELECT " + itemSelectCols + ` FROM memories m
		WHERE m.status = 'active' AND m.tier = 'searchable' AND m.scope IN (` + ph + `)
		ORDER BY CASE WHEN m.scope = 'global' THEN 0 ELSE 1 END,
		         CASE WHEN m.kind IN ('preference', 'feedback') THEN 0 ELSE 1 END,
		         m.updated_at DESC
		LIMIT 12`
	return queryItems(s.db, q, args, false)
}

func formatAlwaysLine(it Item) string {
	text := cmp.Or(it.Summary, DeriveSummary("", it.Body))
	return fmt.Sprintf("- [%s/%s] %s", it.Scope, it.Key, TruncateRunes(text, MaxSummaryRunes))
}

func formatIndexLine(it Item) string {
	text := cmp.Or(it.Summary, DeriveSummary("", it.Body))
	return fmt.Sprintf("- %s/%s (%s): %s", it.Scope, it.Key, it.Kind, TruncateRunes(text, 140))
}

// RenderContext renders the SessionStart fleet memory prompt block capped at MaxContextBytes.
func (s *Store) RenderContext(repo, device string) (ContextResult, error) {
	scopes, repoScope := contextScopes(repo, device)

	s.mu.Lock()
	always, err := s.queryAlwaysForContext(scopes)
	if err != nil {
		s.mu.Unlock()
		return ContextResult{}, err
	}
	searchable, err := s.querySearchableForContext(repoScope)
	s.mu.Unlock()
	if err != nil {
		return ContextResult{}, err
	}

	saveScope := cmp.Or(repoScope, "global")
	header := fmt.Sprintf("[Fleet Memory] (scopes: %s)", strings.Join(scopes, ", "))
	footer := fmt.Sprintf(
		"Memory tools: `fleet_memory_search` / `fleet_memory_get` for deeper repo/fleet notes; "+
			"`fleet_memory_set` to save standing user preferences (`scope=\"global\"`, `tier=\"always\"`) "+
			"or durable repo conventions (`scope=%q`, default). "+
			"Never save ephemeral task state or code facts derivable from git; update existing keys instead of duplicating.",
		saveScope,
	)

	var body strings.Builder
	body.WriteString(header)
	reserved := len(footer) + 64
	budget := MaxContextBytes - reserved

	truncated := false
	appendLine := func(ln string) bool {
		if body.Len()+len(ln)+1 > budget {
			truncated = true
			return false
		}
		body.WriteByte('\n')
		body.WriteString(ln)
		return true
	}

	if len(always) > 0 {
		if appendLine("Standing preferences:") {
			for _, it := range always {
				if !appendLine(formatAlwaysLine(it)) {
					break
				}
			}
		}
	}
	if len(searchable) > 0 {
		if appendLine("Indexed memories:") {
			for _, it := range searchable {
				if !appendLine(formatIndexLine(it)) {
					break
				}
			}
		}
	}
	if truncated {
		body.WriteString("\n... (truncated; use `fleet_memory_search`)")
	}
	body.WriteString("\n")
	body.WriteString(footer)

	rendered := body.String()
	if len(rendered) > MaxContextBytes {
		rendered = TruncateRunes(rendered, MaxContextBytes)
	}
	return ContextResult{
		Scopes:          scopes,
		AlwaysCount:     len(always),
		SearchableCount: len(searchable),
		TotalCount:      len(always) + len(searchable),
		Rendered:        rendered,
	}, nil
}

// ClaimNextJob returns and marks the next pending (or retryable failed) curation job as running.
func (s *Store) ClaimNextJob() (*CurationJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var job CurationJob
	err := s.db.QueryRow(
		`SELECT id, memory_id, status, attempts, last_error, created_at, updated_at
		 FROM curation_jobs
		 WHERE status = 'pending' OR (status = 'failed' AND attempts < 3)
		 ORDER BY CASE WHEN status = 'pending' THEN 0 ELSE 1 END, updated_at ASC, id ASC
		 LIMIT 1`,
	).Scan(&job.ID, &job.MemoryID, &job.Status, &job.Attempts, &job.LastError, &job.CreatedAt, &job.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now := registry.NowISO()
	if _, err := s.db.Exec(
		"UPDATE curation_jobs SET status = 'running', attempts = attempts + 1, updated_at = ? WHERE id = ?",
		now, job.ID,
	); err != nil {
		return nil, err
	}
	job.Status = "running"
	job.Attempts++
	job.UpdatedAt = now
	return &job, nil
}

// CompleteJob removes a completed curation job from the queue.
func (s *Store) CompleteJob(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("DELETE FROM curation_jobs WHERE id = ?", id)
	return err
}

// FailJob marks a curation job failed with the given error message.
func (s *Store) FailJob(id int64, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		"UPDATE curation_jobs SET status = 'failed', last_error = ?, updated_at = ? WHERE id = ?",
		TruncateRunes(errMsg, 400), registry.NowISO(), id,
	)
	return err
}

// EnqueueAllActive enqueues active memories matching kinds (or all active if kinds is empty).
func (s *Store) EnqueueAllActive(kinds []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := registry.NowISO()
	var rows *sql.Rows
	var err error
	if len(kinds) > 0 {
		ph := strings.Repeat(",?", len(kinds))[1:]
		args := make([]any, 0, len(kinds))
		for _, k := range kinds {
			args = append(args, k)
		}
		rows, err = s.db.Query("SELECT id FROM memories WHERE status = 'active' AND kind IN ("+ph+")", args...)
	} else {
		rows, err = s.db.Query("SELECT id FROM memories WHERE status = 'active'")
	}
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if scanErr := rows.Scan(&id); scanErr != nil {
			_ = rows.Close()
			return 0, scanErr
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		if err := enqueueJobTx(tx, id, now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// ResetFailedJobs resets failed curation jobs back to pending so manual runs can retry them.
func (s *Store) ResetFailedJobs() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		"UPDATE curation_jobs SET status = 'pending', attempts = 0, last_error = '', updated_at = ? WHERE status = 'failed'",
		registry.NowISO(),
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// JobCounts returns counts of pending jobs, exhausted/failed jobs, and pending proposals.
func (s *Store) JobCounts() (pending, failed, proposals int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.db.QueryRow("SELECT COUNT(*) FROM curation_jobs WHERE status IN ('pending', 'running') OR (status = 'failed' AND attempts < 3)").Scan(&pending); err != nil {
		return 0, 0, 0, err
	}
	if err = s.db.QueryRow("SELECT COUNT(*) FROM curation_jobs WHERE status = 'failed' AND attempts >= 3").Scan(&failed); err != nil {
		return 0, 0, 0, err
	}
	if err = s.db.QueryRow("SELECT COUNT(*) FROM memory_proposals WHERE status = 'pending'").Scan(&proposals); err != nil {
		return 0, 0, 0, err
	}
	return pending, failed, proposals, nil
}
