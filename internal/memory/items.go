package memory

import (
	"cmp"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/registry"
)

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
		if isErrNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	it.Locked = lockedInt != 0
	return &it, nil
}

type queryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

func getByScopeKey(qr queryRower, scope, key string) (*Item, error) {
	return scanItem(qr.QueryRow(
		"SELECT "+itemSelectCols+" FROM memories m WHERE m.scope = ? AND m.key = ?",
		scope, key,
	), false)
}

func getByID(qr queryRower, id int64) (*Item, error) {
	return scanItem(qr.QueryRow(
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

func normalizeUpsert(p UpsertParams, existing *Item) (UpsertParams, error) {
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

func writeMemoryInTx(tx *sql.Tx, existing *Item, p UpsertParams, hash string, locked bool) (int64, string, bool, error) {
	now := registry.NowISO()
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
	return memID, action, queued, nil
}

func upsertTx(tx *sql.Tx, p UpsertParams) (UpsertResult, error) {
	scope, err := NormalizeScope(p.Scope)
	if err != nil {
		return UpsertResult{}, err
	}
	key, err := NormalizeKey(p.Key)
	if err != nil {
		return UpsertResult{}, err
	}
	existing, err := getByScopeKey(tx, scope, key)
	if err != nil {
		return UpsertResult{}, err
	}
	p, err = normalizeUpsert(p, existing)
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

	memID, action, queued, err := writeMemoryInTx(tx, existing, p, hash, locked)
	if err != nil {
		return UpsertResult{}, err
	}
	saved, err := getByID(tx, memID)
	if err != nil {
		return UpsertResult{}, err
	}
	if saved == nil {
		return UpsertResult{}, newMemoryErr("memory missing after upsert", 500)
	}
	return UpsertResult{Item: *saved, Action: action, Queued: queued}, nil
}

func (s *Store) upsertLocked(db *sql.DB, p UpsertParams) (UpsertResult, error) {
	tx, err := db.Begin()
	if err != nil {
		return UpsertResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := upsertTx(tx, p)
	if err != nil {
		return UpsertResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return UpsertResult{}, err
	}
	return res, nil
}

// Upsert creates or updates a memory entry and records a revision snapshot.
func (s *Store) Upsert(p UpsertParams) (UpsertResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return UpsertResult{}, err
	}
	return s.upsertLocked(db, p)
}

// SyncBatch applies a batch of backup/sync upserts up to MaxSyncBatch items in one transaction.
func (s *Store) SyncBatch(device string, items []UpsertParams) (SyncBatchResult, error) {
	if len(items) > MaxSyncBatch {
		return SyncBatchResult{}, newMemoryErr(fmt.Sprintf("batch exceeds %d items", MaxSyncBatch), 400)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return SyncBatchResult{}, err
	}
	tx, err := db.Begin()
	if err != nil {
		return SyncBatchResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var res SyncBatchResult
	for _, it := range items {
		if it.Device == "" {
			it.Device = device
		}
		if it.Source == "" {
			it.Source = "claude_backup"
		}
		ur, uErr := upsertTx(tx, it)
		if uErr != nil {
			return SyncBatchResult{}, uErr
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
	if err := tx.Commit(); err != nil {
		return SyncBatchResult{}, err
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
	db, err := s.dbLocked()
	if err != nil {
		return ItemDetail{}, err
	}
	it, err := getByScopeKey(db, normScope, normKey)
	if err != nil {
		return ItemDetail{}, err
	}
	if it == nil {
		return ItemDetail{}, newMemoryErr(fmt.Sprintf("memory %s/%s not found", normScope, normKey), 404)
	}
	detail := ItemDetail{Item: *it}
	if includeHistory {
		hist, err := s.historyLocked(db, normScope, normKey, 20)
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
	db, err := s.dbLocked()
	if err != nil {
		return nil, err
	}
	return getByID(db, id)
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
	db, err := s.dbLocked()
	if err != nil {
		return err
	}
	existing, err := getByScopeKey(db, normScope, normKey)
	if err != nil {
		return err
	}
	if existing == nil {
		return newMemoryErr(fmt.Sprintf("memory %s/%s not found", normScope, normKey), 404)
	}
	now := registry.NowISO()
	tx, err := db.Begin()
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

func (s *Store) historyLocked(db *sql.DB, scope, key string, limit int) ([]Revision, error) {
	limit = clampLimit(limit, 30)
	var rows *sql.Rows
	var err error
	if scope != "" && key != "" {
		rows, err = db.Query(
			`SELECT id, memory_id, scope, key, revision, kind, tier, status, summary, body, source, device, harness, session_id, reason, created_at
			 FROM memory_revisions WHERE scope = ? AND key = ? ORDER BY revision DESC, id DESC LIMIT ?`,
			scope, key, limit,
		)
	} else {
		rows, err = db.Query(
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
	db, err := s.dbLocked()
	if err != nil {
		return nil, err
	}
	return s.historyLocked(db, ns, nk, limit)
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
	db, err := s.dbLocked()
	if err != nil {
		return Item{}, err
	}

	var rev Revision
	err = db.QueryRow(
		`SELECT id, memory_id, scope, key, revision, kind, tier, status, summary, body
		 FROM memory_revisions WHERE scope = ? AND key = ? AND revision = ? ORDER BY id DESC LIMIT 1`,
		ns, nk, revision,
	).Scan(&rev.ID, &rev.MemoryID, &rev.Scope, &rev.Key, &rev.Revision, &rev.Kind, &rev.Tier, &rev.Status, &rev.Summary, &rev.Body)
	if isErrNoRows(err) {
		return Item{}, newMemoryErr(fmt.Sprintf("revision %d of %s/%s not found", revision, ns, nk), 404)
	}
	if err != nil {
		return Item{}, err
	}
	status := rev.Status
	if status == "deleted" {
		status = "active"
	}
	ur, err := s.upsertLocked(db, UpsertParams{
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
	db, err := s.dbLocked()
	if err != nil {
		return Item{}, err
	}
	src, err := getByScopeKey(db, fs, fk)
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
	ur, err := s.upsertLocked(db, UpsertParams{
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
