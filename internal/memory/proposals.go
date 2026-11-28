package memory

import (
	"cmp"
	"database/sql"
	"fmt"
	"strings"

	"github.com/bdatdo0601/fleet/internal/registry"
)

const proposalSelectCols = "id, action, source_memory_id, target_scope, target_key, kind, tier, summary, body, reason, status, model, created_at, resolved_at"

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
	db, err := s.dbLocked()
	if err != nil {
		return Proposal{}, err
	}
	tx, err := db.Begin()
	if err != nil {
		return Proposal{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var existingID int64
	err = tx.QueryRow(
		`SELECT id FROM memory_proposals
		 WHERE status = 'pending'
		   AND ((? > 0 AND source_memory_id = ? AND action = ? AND target_scope = ?)
		        OR (target_scope = ? AND target_key = ?))
		 ORDER BY id DESC LIMIT 1`,
		p.SourceMemoryID, p.SourceMemoryID, action, ts, ts, tk,
	).Scan(&existingID)
	if err != nil && !isErrNoRows(err) {
		return Proposal{}, err
	}

	if existingID > 0 {
		if _, err := tx.Exec(
			`UPDATE memory_proposals
			 SET action = ?, source_memory_id = ?, target_scope = ?, target_key = ?, kind = ?, tier = ?, summary = ?, body = ?, reason = ?, model = ?, created_at = ?
			 WHERE id = ?`,
			action, p.SourceMemoryID, ts, tk, kind, tier, summary, body, p.Reason, p.Model, now, existingID,
		); err != nil {
			return Proposal{}, err
		}
		if err := tx.Commit(); err != nil {
			return Proposal{}, err
		}
		return Proposal{
			ID: existingID, Action: action, SourceMemoryID: p.SourceMemoryID,
			TargetScope: ts, TargetKey: tk, Kind: kind, Tier: tier,
			Summary: summary, Body: body, Reason: p.Reason, Status: "pending",
			Model: p.Model, CreatedAt: now,
		}, nil
	}

	res, err := tx.Exec(
		`INSERT INTO memory_proposals
		 (action, source_memory_id, target_scope, target_key, kind, tier, summary, body, reason, status, model, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
		action, p.SourceMemoryID, ts, tk, kind, tier, summary, body, p.Reason, p.Model, now,
	)
	if err != nil {
		return Proposal{}, err
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return Proposal{}, err
	}
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
	db, err := s.dbLocked()
	if err != nil {
		return Proposal{}, err
	}
	p, err := scanProposal(db.QueryRow("SELECT "+proposalSelectCols+" FROM memory_proposals WHERE id = ?", id))
	if isErrNoRows(err) {
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
	db, err := s.dbLocked()
	if err != nil {
		return nil, err
	}
	limit = clampLimit(limit, 50)
	var rows *sql.Rows
	if status != "" {
		rows, err = db.Query(
			"SELECT "+proposalSelectCols+" FROM memory_proposals WHERE status = ? ORDER BY id DESC LIMIT ?",
			status, limit,
		)
	} else {
		rows, err = db.Query(
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

// ResolveProposal approves or rejects a pending proposal atomically in one transaction.
func (s *Store) ResolveProposal(id int64, approve bool, device string) (Proposal, *Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return Proposal{}, nil, err
	}
	tx, err := db.Begin()
	if err != nil {
		return Proposal{}, nil, err
	}
	defer func() { _ = tx.Rollback() }()

	p, err := scanProposal(tx.QueryRow("SELECT "+proposalSelectCols+" FROM memory_proposals WHERE id = ?", id))
	if isErrNoRows(err) {
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
		if _, err := tx.Exec("UPDATE memory_proposals SET status = 'rejected', resolved_at = ? WHERE id = ?", now, id); err != nil {
			return Proposal{}, nil, err
		}
		if err := tx.Commit(); err != nil {
			return Proposal{}, nil, err
		}
		p.Status = "rejected"
		p.ResolvedAt = now
		return p, nil, nil
	}

	var applied *Item
	switch p.Action {
	case "archive":
		existing, gErr := getByScopeKey(tx, p.TargetScope, p.TargetKey)
		if gErr != nil {
			return Proposal{}, nil, gErr
		}
		if existing != nil {
			ur, uErr := upsertTx(tx, UpsertParams{
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
		ur, uErr := upsertTx(tx, UpsertParams{
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

	if _, err := tx.Exec("UPDATE memory_proposals SET status = 'approved', resolved_at = ? WHERE id = ?", now, id); err != nil {
		return Proposal{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return Proposal{}, nil, err
	}
	p.Status = "approved"
	p.ResolvedAt = now
	return p, applied, nil
}
