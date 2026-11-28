package memory

import (
	"cmp"
	"database/sql"
	"strings"

	"github.com/bdatdo0601/fleet/internal/registry"
)

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

// ClaimNextJob returns and marks the next pending (or retryable failed) curation job as running.
func (s *Store) ClaimNextJob() (*CurationJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return nil, err
	}

	var job CurationJob
	err = db.QueryRow(
		`SELECT id, memory_id, status, attempts, last_error, created_at, updated_at
		 FROM curation_jobs
		 WHERE status = 'pending' OR (status = 'failed' AND attempts < 3)
		 ORDER BY CASE WHEN status = 'pending' THEN 0 ELSE 1 END, updated_at ASC, id ASC
		 LIMIT 1`,
	).Scan(&job.ID, &job.MemoryID, &job.Status, &job.Attempts, &job.LastError, &job.CreatedAt, &job.UpdatedAt)
	if isErrNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now := registry.NowISO()
	if _, err := db.Exec(
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
	db, err := s.dbLocked()
	if err != nil {
		return err
	}
	_, err = db.Exec("DELETE FROM curation_jobs WHERE id = ?", id)
	return err
}

// FailJob marks a curation job failed with the given error message.
func (s *Store) FailJob(id int64, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return err
	}
	_, err = db.Exec(
		"UPDATE curation_jobs SET status = 'failed', last_error = ?, updated_at = ? WHERE id = ?",
		TruncateRunes(errMsg, 400), registry.NowISO(), id,
	)
	return err
}

// EnqueueAllActive enqueues active memories matching kinds (or all active if kinds is empty).
func (s *Store) EnqueueAllActive(kinds []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return 0, err
	}

	now := registry.NowISO()
	var rows *sql.Rows
	if len(kinds) > 0 {
		ph := strings.Repeat(",?", len(kinds))[1:]
		args := make([]any, 0, len(kinds))
		for _, k := range kinds {
			args = append(args, k)
		}
		rows, err = db.Query("SELECT id FROM memories WHERE status = 'active' AND kind IN ("+ph+")", args...)
	} else {
		rows, err = db.Query("SELECT id FROM memories WHERE status = 'active'")
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

	tx, err := db.Begin()
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
	db, err := s.dbLocked()
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(
		"UPDATE curation_jobs SET status = 'pending', attempts = 0, last_error = '', updated_at = ? WHERE status = 'failed'",
		registry.NowISO(),
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s *Store) jobCountsLocked(db *sql.DB) (pending, failed, proposals int, err error) {
	if err = db.QueryRow("SELECT COUNT(*) FROM curation_jobs WHERE status IN ('pending', 'running') OR (status = 'failed' AND attempts < 3)").Scan(&pending); err != nil {
		return 0, 0, 0, err
	}
	if err = db.QueryRow("SELECT COUNT(*) FROM curation_jobs WHERE status = 'failed' AND attempts >= 3").Scan(&failed); err != nil {
		return 0, 0, 0, err
	}
	if err = db.QueryRow("SELECT COUNT(*) FROM memory_proposals WHERE status = 'pending'").Scan(&proposals); err != nil {
		return 0, 0, 0, err
	}
	return pending, failed, proposals, nil
}

// JobCounts returns counts of pending jobs, exhausted/failed jobs, and pending proposals.
func (s *Store) JobCounts() (pending, failed, proposals int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return 0, 0, 0, err
	}
	return s.jobCountsLocked(db)
}

// QueuedCandidates returns active memory items currently queued in curation_jobs without claiming them.
func (s *Store) QueuedCandidates(limit int) ([]Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return nil, err
	}
	limit = clampLimit(limit, 20)
	rows, err := db.Query(
		`SELECT `+itemSelectCols+` FROM curation_jobs j
		 JOIN memories m ON m.id = j.memory_id
		 WHERE m.status = 'active' AND (j.status = 'pending' OR (j.status = 'failed' AND j.attempts < 3))
		 ORDER BY j.updated_at ASC, j.id ASC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows, false)
		if err != nil {
			return nil, err
		}
		if it != nil {
			out = append(out, *it)
		}
	}
	return out, rows.Err()
}

// CurateMutationResult reports the outcome of a transactional curator mutation.
type CurateMutationResult struct {
	Item          Item
	Target        *Item
	Applied       bool
	Protected     bool
	Stale         bool
	Skipped       bool
	TargetMissing bool
	OptedOut      bool
	Reason        string
}

func checkCurateCandidateTx(tx *sql.Tx, candID int64, expectedRev int) (*Item, *CurateMutationResult, error) {
	cand, err := getByID(tx, candID)
	if err != nil {
		return nil, nil, err
	}
	if cand == nil || cand.Status != "active" {
		return nil, &CurateMutationResult{Skipped: true, Reason: "candidate no longer active"}, nil
	}
	if cand.Locked || cand.Source == "human" {
		return cand, &CurateMutationResult{Item: *cand, Protected: true, Reason: "source entry is locked/human-edited"}, nil
	}
	if expectedRev > 0 && cand.Revision != expectedRev {
		return cand, &CurateMutationResult{Item: *cand, Stale: true, Reason: "candidate modified concurrently"}, nil
	}
	return cand, nil, nil
}

// ApplyCuratorUpdate verifies candidate protection/revision and updates or archives cand atomically.
func (s *Store) ApplyCuratorUpdate(candID int64, expectedRev int, kind, tier, status, summary, body, reason string) (CurateMutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return CurateMutationResult{}, err
	}
	tx, err := db.Begin()
	if err != nil {
		return CurateMutationResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	cand, blocked, err := checkCurateCandidateTx(tx, candID, expectedRev)
	if err != nil || blocked != nil {
		if blocked != nil {
			return *blocked, nil
		}
		return CurateMutationResult{}, err
	}

	nextKind := cmp.Or(kind, cand.Kind)
	nextTier := cmp.Or(tier, cand.Tier)
	nextSummary := cmp.Or(summary, cand.Summary)
	nextBody := cmp.Or(body, cand.Body)
	if status == "archived" {
		nextKind, nextTier, nextSummary, nextBody = cand.Kind, cand.Tier, cand.Summary, cand.Body
	} else if (cand.Kind == "project" || cand.Kind == "reference") && len(nextBody) < len(cand.Body)/2 {
		nextBody = cand.Body
	}

	ur, err := upsertTx(tx, UpsertParams{
		Scope: cand.Scope, Key: cand.Key, Kind: nextKind, Tier: nextTier, Status: cmp.Or(status, "active"),
		Summary: nextSummary, Body: nextBody, Source: "curator",
		Reason: cmp.Or(reason, "curator_update"), EnqueueCurate: new(false),
	})
	if err != nil {
		return CurateMutationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return CurateMutationResult{}, err
	}
	return CurateMutationResult{Item: ur.Item, Applied: ur.Action == "updated"}, nil
}

// ApplyCuratorMerge atomically merges cand into target after re-verifying protection and revision under s.mu.
func (s *Store) ApplyCuratorMerge(candID int64, expectedRev int, targetScope, targetKey, summary, body, reason string) (CurateMutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return CurateMutationResult{}, err
	}
	tx, err := db.Begin()
	if err != nil {
		return CurateMutationResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	cand, blocked, err := checkCurateCandidateTx(tx, candID, expectedRev)
	if err != nil || blocked != nil {
		if blocked != nil {
			return *blocked, nil
		}
		return CurateMutationResult{}, err
	}

	target, err := getByScopeKey(tx, targetScope, targetKey)
	if err != nil {
		return CurateMutationResult{}, err
	}
	if target == nil {
		return CurateMutationResult{Item: *cand, TargetMissing: true}, nil
	}
	if target.Locked || target.Source == "human" {
		return CurateMutationResult{Item: *cand, Target: target, Protected: true, Reason: "target entry is locked/human-edited"}, nil
	}

	merged, err := upsertTx(tx, UpsertParams{
		Scope: target.Scope, Key: target.Key, Kind: target.Kind, Tier: target.Tier,
		Summary: summary, Body: body, Source: "curator",
		Reason: "curator_merge_from_" + cand.Key, EnqueueCurate: new(false),
	})
	if err != nil {
		return CurateMutationResult{}, err
	}
	if _, err := upsertTx(tx, UpsertParams{
		Scope: cand.Scope, Key: cand.Key, Kind: cand.Kind, Tier: cand.Tier, Status: "superseded",
		Summary: cand.Summary, Body: cand.Body, Source: "curator",
		SupersededBy: target.Scope + "/" + target.Key,
		Reason:       cmp.Or(reason, "merged into "+target.Key), EnqueueCurate: new(false),
	}); err != nil {
		return CurateMutationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return CurateMutationResult{}, err
	}
	return CurateMutationResult{Item: *cand, Target: &merged.Item, Applied: true}, nil
}

// ApplyCuratorSupersede atomically marks target superseded and updates cand after re-verifying protection and revision.
func (s *Store) ApplyCuratorSupersede(candID int64, expectedRev int, targetScope, targetKey, kind, tier, summary, body, reason string) (CurateMutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return CurateMutationResult{}, err
	}
	tx, err := db.Begin()
	if err != nil {
		return CurateMutationResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	cand, blocked, err := checkCurateCandidateTx(tx, candID, expectedRev)
	if err != nil || blocked != nil {
		if blocked != nil {
			return *blocked, nil
		}
		return CurateMutationResult{}, err
	}

	target, err := getByScopeKey(tx, targetScope, targetKey)
	if err != nil {
		return CurateMutationResult{}, err
	}
	if target == nil {
		return CurateMutationResult{Item: *cand, TargetMissing: true}, nil
	}
	if target.Locked || target.Source == "human" {
		return CurateMutationResult{Item: *cand, Target: target, Protected: true, Reason: "target entry is locked/human-edited"}, nil
	}

	if _, err := upsertTx(tx, UpsertParams{
		Scope: target.Scope, Key: target.Key, Kind: target.Kind, Tier: target.Tier, Status: "superseded",
		Summary: target.Summary, Body: target.Body, Source: "curator",
		SupersededBy: cand.Scope + "/" + cand.Key,
		Reason:       cmp.Or(reason, "superseded by "+cand.Key), EnqueueCurate: new(false),
	}); err != nil {
		return CurateMutationResult{}, err
	}
	updated, err := upsertTx(tx, UpsertParams{
		Scope: cand.Scope, Key: cand.Key, Kind: cmp.Or(kind, cand.Kind), Tier: cmp.Or(tier, cand.Tier),
		Summary: summary, Body: body, Source: "curator",
		Reason: cmp.Or(reason, "supersedes "+target.Key), EnqueueCurate: new(false),
	})
	if err != nil {
		return CurateMutationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return CurateMutationResult{}, err
	}
	return CurateMutationResult{Item: updated.Item, Target: target, Applied: true}, nil
}

// ApplyCuratorPromoteGlobal atomically promotes cand to global/always after re-verifying protection and revision.
func (s *Store) ApplyCuratorPromoteGlobal(candID int64, expectedRev int, targetKey, kind, summary, body, reason string) (CurateMutationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return CurateMutationResult{}, err
	}
	tx, err := db.Begin()
	if err != nil {
		return CurateMutationResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	cand, blocked, err := checkCurateCandidateTx(tx, candID, expectedRev)
	if err != nil || blocked != nil {
		if blocked != nil {
			return *blocked, nil
		}
		return CurateMutationResult{}, err
	}

	target, err := getByScopeKey(tx, "global", targetKey)
	if err != nil {
		return CurateMutationResult{}, err
	}
	if target != nil && (target.Locked || target.Source == "human") {
		if target.Status == "archived" {
			return CurateMutationResult{Item: *cand, Target: target, OptedOut: true, Reason: "opted out (archived by human)"}, nil
		}
		return CurateMutationResult{Item: *cand, Target: target, Protected: true, Reason: "target global entry is locked/human-edited"}, nil
	}

	ur, err := upsertTx(tx, UpsertParams{
		Scope:         "global",
		Key:           targetKey,
		Kind:          kind,
		Tier:          "always",
		Summary:       summary,
		Body:          body,
		Source:        "curator",
		Reason:        cmp.Or(reason, "curator_promote_global"),
		EnqueueCurate: new(false),
	})
	if err != nil {
		return CurateMutationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return CurateMutationResult{}, err
	}
	return CurateMutationResult{Item: *cand, Target: &ur.Item, Applied: true}, nil
}
