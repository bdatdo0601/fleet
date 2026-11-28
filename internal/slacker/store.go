package slacker

import (
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bdatdo0601/fleet/internal/registry"
)

const slackerSchema = `
CREATE TABLE IF NOT EXISTS slacker_jobs (
  id               TEXT PRIMARY KEY,
  account          TEXT NOT NULL DEFAULT 'default',
  event_id         TEXT NOT NULL UNIQUE,
  channel_id       TEXT NOT NULL,
  channel_name     TEXT NOT NULL DEFAULT '',
  message_ts       TEXT NOT NULL,
  thread_ts        TEXT NOT NULL DEFAULT '',
  reaction         TEXT NOT NULL,
  user_id          TEXT NOT NULL,
  target_device    TEXT NOT NULL,
  repo_path        TEXT NOT NULL,
  use_tmux         INTEGER NOT NULL DEFAULT 0,
  policy           TEXT NOT NULL DEFAULT '',
  model            TEXT NOT NULL DEFAULT '',
  harness          TEXT NOT NULL,
  session_name     TEXT NOT NULL,
  prompt           TEXT NOT NULL,
  permalink        TEXT NOT NULL DEFAULT '',
  status           TEXT NOT NULL DEFAULT 'queued',
  attempt_count    INTEGER NOT NULL DEFAULT 0,
  claimed_by       TEXT NOT NULL DEFAULT '',
  claimed_at       TEXT NOT NULL DEFAULT '',
  lease_expires_at TEXT NOT NULL DEFAULT '',
  receipt_id       TEXT NOT NULL DEFAULT '',
  backend          TEXT NOT NULL DEFAULT '',
  error            TEXT NOT NULL DEFAULT '',
  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_slacker_jobs_device_status
  ON slacker_jobs(target_device, status, created_at);

CREATE TABLE IF NOT EXISTS slacker_runtime (
  role          TEXT PRIMARY KEY,
  pid           INTEGER NOT NULL DEFAULT 0,
  connected     INTEGER NOT NULL DEFAULT 0,
  last_poll_at  TEXT NOT NULL DEFAULT '',
  last_event_at TEXT NOT NULL DEFAULT '',
  last_error    TEXT NOT NULL DEFAULT '',
  updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS slacker_dispatch_state (
  scope       TEXT PRIMARY KEY,
  last_device TEXT NOT NULL DEFAULT '',
  updated_at  TEXT NOT NULL
);
`

var jobMigrations = []registry.ColumnMigration{
	{Col: "account", Def: "TEXT NOT NULL DEFAULT 'default'"},
	{Col: "policy", Def: "TEXT NOT NULL DEFAULT ''"},
	{Col: "model", Def: "TEXT NOT NULL DEFAULT ''"},
}

// Store manages persisted Slacker jobs and runtime status in ~/.fleet/slacker.db.
type Store struct {
	Path    string
	mu      sync.Mutex
	db      *sql.DB
	waitMu  sync.Mutex
	waiters map[string][]chan struct{}
}

// Open opens or initializes slacker.db at path.
func Open(path string) (*Store, error) {
	db, err := registry.OpenSQLite(path, slackerSchema, "slacker_jobs", jobMigrations)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_slacker_jobs_message ON slacker_jobs(account, channel_id, message_ts, status)`); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{
		Path:    path,
		db:      db,
		waiters: map[string][]chan struct{}{},
	}, nil
}

// Close closes the underlying SQLite connection and wakes all waiting workers.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.db == nil {
		s.mu.Unlock()
		return nil
	}
	err := s.db.Close()
	s.db = nil
	s.mu.Unlock()
	s.NotifyAll()
	return err
}

var errStoreClosed = errors.New("slacker store closed")

func (s *Store) dbLocked() (*sql.DB, error) {
	if s.db == nil {
		return nil, errStoreClosed
	}
	return s.db, nil
}

type immediateTx struct {
	ctx  context.Context
	conn *sql.Conn
	done bool
}

func (s *Store) beginImmediateLocked(ctx context.Context) (*immediateTx, error) {
	db, err := s.dbLocked()
	if err != nil {
		return nil, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &immediateTx{ctx: ctx, conn: conn}, nil
}

func (t *immediateTx) QueryRow(q string, args ...any) *sql.Row {
	return t.conn.QueryRowContext(t.ctx, q, args...)
}

func (t *immediateTx) Exec(q string, args ...any) (sql.Result, error) {
	return t.conn.ExecContext(t.ctx, q, args...)
}

func (t *immediateTx) Commit() error {
	if t.done {
		return nil
	}
	t.done = true
	_, err := t.conn.ExecContext(t.ctx, "COMMIT")
	cErr := t.conn.Close()
	if err != nil {
		return err
	}
	return cErr
}

func (t *immediateTx) Rollback(ctx context.Context) {
	if t.done {
		return
	}
	t.done = true
	_, _ = t.conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
	_ = t.conn.Close()
}

func newJobID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "job-" + hex.EncodeToString(b[:])
}

func isoAfter(d time.Duration) string {
	return time.Now().UTC().Add(d).Truncate(time.Second).Format("2006-01-02T15:04:05+00:00")
}

// NotifyDevice wakes any active long-poll waiters for device.
func (s *Store) NotifyDevice(device string) {
	s.waitMu.Lock()
	chans := s.waiters[device]
	delete(s.waiters, device)
	s.waitMu.Unlock()
	for _, ch := range chans {
		close(ch)
	}
}

// NotifyAll wakes all active long-poll waiters.
func (s *Store) NotifyAll() {
	s.waitMu.Lock()
	all := s.waiters
	s.waiters = map[string][]chan struct{}{}
	s.waitMu.Unlock()
	for _, chans := range all {
		for _, ch := range chans {
			close(ch)
		}
	}
}

func (s *Store) registerWaiter(device string) chan struct{} {
	ch := make(chan struct{})
	s.waitMu.Lock()
	s.waiters[device] = append(s.waiters[device], ch)
	s.waitMu.Unlock()
	return ch
}

func (s *Store) unregisterWaiter(device string, target chan struct{}) {
	s.waitMu.Lock()
	defer s.waitMu.Unlock()
	list := s.waiters[device]
	for i, ch := range list {
		if ch == target {
			s.waiters[device] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(s.waiters[device]) == 0 {
		delete(s.waiters, device)
	}
}

// HasEvent reports whether eventID has already been persisted in slacker_jobs.
func (s *Store) HasEvent(eventID string) (bool, error) {
	if eventID == "" {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return false, err
	}
	var n int
	err = db.QueryRow(`SELECT COUNT(*) FROM slacker_jobs WHERE event_id = ?`, eventID).Scan(&n)
	return n > 0, err
}

// HasMessageJob reports whether an active or launched job already exists for (account, channelID, messageTS).
func (s *Store) HasMessageJob(account, channelID, messageTS string) (bool, error) {
	if channelID == "" || messageTS == "" {
		return false, nil
	}
	acct := cmp.Or(account, DefaultAccountID)
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return false, err
	}
	var n int
	err = db.QueryRow(
		`SELECT COUNT(*) FROM slacker_jobs
		 WHERE account = ? AND channel_id = ? AND message_ts = ?
		   AND status IN ('queued', 'claimed', 'launched')`,
		acct, channelID, messageTS,
	).Scan(&n)
	return n > 0, err
}

const jobSelectCols = `id, account, event_id, channel_id, channel_name, message_ts, thread_ts, reaction, user_id,
  target_device, repo_path, use_tmux, policy, model, harness, session_name, prompt, permalink, status,
  attempt_count, claimed_by, claimed_at, lease_expires_at, receipt_id, backend, error, created_at, updated_at`

func scanJobRow(sc interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var useTmux int
	err := sc.Scan(
		&j.ID, &j.Account, &j.EventID, &j.ChannelID, &j.ChannelName, &j.MessageTS, &j.ThreadTS, &j.Reaction, &j.UserID,
		&j.TargetDevice, &j.RepoPath, &useTmux, &j.Policy, &j.Model, &j.Harness, &j.SessionName, &j.Prompt, &j.Permalink, &j.Status,
		&j.AttemptCount, &j.ClaimedBy, &j.ClaimedAt, &j.LeaseExpiresAt, &j.ReceiptID, &j.Backend, &j.Error,
		&j.CreatedAt, &j.UpdatedAt,
	)
	j.UseTmux = useTmux != 0
	return j, err
}

// EnqueueJob inserts a job deduplicated by event_id and active/launched message key (account, channel_id, message_ts).
// Returns (true, insertedJob, nil) on insert, or (false, existingJob, nil) when a matching job already exists.
func (s *Store) EnqueueJob(job Job) (bool, Job, error) {
	return s.EnqueueJobContext(context.Background(), job)
}

// EnqueueJobContext is the context-aware variant of EnqueueJob.
func (s *Store) EnqueueJobContext(ctx context.Context, job Job) (bool, Job, error) {
	if job.EventID == "" {
		return false, Job{}, newSlackerErr("event_id is required", 400)
	}
	now := registry.NowISO()
	job.ID = cmp.Or(job.ID, newJobID())
	job.Account = cmp.Or(job.Account, DefaultAccountID)
	job.Status = cmp.Or(job.Status, "queued")
	job.CreatedAt = cmp.Or(job.CreatedAt, now)
	job.UpdatedAt = cmp.Or(job.UpdatedAt, now)
	useTmuxInt := 0
	if job.UseTmux {
		useTmuxInt = 1
	}

	inserted, resJob, err := s.enqueueJobTx(ctx, job, useTmuxInt)
	if err != nil || !inserted {
		return false, resJob, err
	}
	s.NotifyDevice(resJob.TargetDevice)
	return true, resJob, nil
}

func (s *Store) enqueueJobTx(ctx context.Context, job Job, useTmuxInt int) (bool, Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.beginImmediateLocked(ctx)
	if err != nil {
		return false, Job{}, err
	}
	defer tx.Rollback(ctx)

	existing, err := scanJobRow(tx.QueryRow(`SELECT `+jobSelectCols+` FROM slacker_jobs WHERE event_id = ?`, job.EventID))
	if err == nil {
		return false, existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, Job{}, err
	}

	if job.ChannelID != "" && job.MessageTS != "" {
		existingMsg, mErr := scanJobRow(tx.QueryRow(
			`SELECT `+jobSelectCols+` FROM slacker_jobs
			 WHERE account = ? AND channel_id = ? AND message_ts = ?
			   AND status IN ('queued', 'claimed', 'launched')
			 ORDER BY created_at ASC, rowid ASC LIMIT 1`,
			job.Account, job.ChannelID, job.MessageTS,
		))
		if mErr == nil {
			return false, existingMsg, nil
		}
		if !errors.Is(mErr, sql.ErrNoRows) {
			return false, Job{}, mErr
		}
	}

	if _, err := tx.Exec(
		`INSERT INTO slacker_jobs(
			id, account, event_id, channel_id, channel_name, message_ts, thread_ts, reaction, user_id,
			target_device, repo_path, use_tmux, policy, model, harness, session_name, prompt, permalink, status,
			attempt_count, claimed_by, claimed_at, lease_expires_at, receipt_id, backend, error, created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', '', '', '', '', '', ?, ?)`,
		job.ID, job.Account, job.EventID, job.ChannelID, job.ChannelName, job.MessageTS, job.ThreadTS, job.Reaction, job.UserID,
		job.TargetDevice, job.RepoPath, useTmuxInt, job.Policy, job.Model, job.Harness, job.SessionName, job.Prompt, job.Permalink, job.Status,
		job.CreatedAt, job.UpdatedAt,
	); err != nil {
		return false, Job{}, err
	}
	if err := tx.Commit(); err != nil {
		return false, Job{}, err
	}
	return true, job, nil
}

// ClaimNextJob atomically claims the oldest queued or lease-expired job targeted at device.
func (s *Store) ClaimNextJob(device string, lease time.Duration) (*Job, error) {
	return s.claimNextJobCtx(context.Background(), device, lease)
}

func (s *Store) claimNextJobCtx(ctx context.Context, device string, lease time.Duration) (*Job, error) {
	if lease <= 0 {
		lease = 90 * time.Second
	}
	now := registry.NowISO()
	leaseExp := isoAfter(lease)

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.beginImmediateLocked(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(
		`SELECT `+jobSelectCols+` FROM slacker_jobs
		 WHERE target_device = ?
		   AND (status = 'queued' OR (status = 'claimed' AND lease_expires_at != '' AND lease_expires_at <= ?))
		 ORDER BY created_at ASC, rowid ASC
		 LIMIT 1`,
		device, now,
	)
	j, err := scanJobRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(
		`UPDATE slacker_jobs
		 SET status = 'claimed',
		     attempt_count = attempt_count + 1,
		     claimed_by = ?,
		     claimed_at = ?,
		     lease_expires_at = ?,
		     updated_at = ?
		 WHERE id = ?`,
		device, now, leaseExp, now, j.ID,
	); err != nil {
		return nil, err
	}
	j.Status = "claimed"
	j.AttemptCount++
	j.ClaimedBy = device
	j.ClaimedAt = now
	j.LeaseExpiresAt = leaseExp
	j.UpdatedAt = now
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &j, nil
}

// WaitForJob records worker presence and blocks until a job for device is available or timeout/ctx expires.
func (s *Store) WaitForJob(ctx context.Context, device string, timeout, lease time.Duration) (*Job, error) {
	if device == "" {
		return nil, newSlackerErr("device is required", 400)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	_ = s.UpsertWorkerRuntime(device, 0, true, "")

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		ch := s.registerWaiter(device)
		job, err := s.claimNextJobCtx(ctx, device, lease)
		if err != nil || job != nil {
			s.unregisterWaiter(device, ch)
			return job, err
		}
		select {
		case <-ch:
		case <-ticker.C:
			s.unregisterWaiter(device, ch)
		case <-deadline.C:
			s.unregisterWaiter(device, ch)
			_ = s.UpsertWorkerRuntime(device, 0, true, "")
			return nil, nil
		case <-ctx.Done():
			s.unregisterWaiter(device, ch)
			return nil, ctx.Err()
		}
	}
}

// RecordReceipt persists a target Mac worker's execution/reconciliation receipt for a job.
func (s *Store) RecordReceipt(device string, r Receipt) (Job, error) {
	return s.recordReceiptCtx(context.Background(), device, r)
}

func (s *Store) recordReceiptCtx(ctx context.Context, device string, r Receipt) (Job, error) {
	if r.JobID == "" {
		return Job{}, newSlackerErr("job_id is required", 400)
	}
	if r.Status != "launched" && r.Status != "failed" {
		return Job{}, newSlackerErr("status must be launched or failed", 400)
	}
	now := registry.NowISO()

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.beginImmediateLocked(ctx)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback(ctx)

	j, err := scanJobRow(tx.QueryRow(`SELECT `+jobSelectCols+` FROM slacker_jobs WHERE id = ?`, r.JobID))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, newSlackerErr("job not found", 404)
	}
	if err != nil {
		return Job{}, err
	}
	if j.TargetDevice != device {
		return Job{}, newSlackerErr(fmt.Sprintf("job %s belongs to %s, not %s", j.ID, j.TargetDevice, device), 403)
	}
	if j.Status == "launched" && r.Status == "launched" {
		return j, nil
	}

	if _, err := tx.Exec(
		`UPDATE slacker_jobs
		 SET status = ?, receipt_id = ?, backend = ?, error = ?, lease_expires_at = '', updated_at = ?
		 WHERE id = ?`,
		r.Status, r.ReceiptID, r.Backend, r.Error, now, r.JobID,
	); err != nil {
		return Job{}, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, err
	}
	j.Status = r.Status
	j.ReceiptID = r.ReceiptID
	j.Backend = r.Backend
	j.Error = r.Error
	j.LeaseExpiresAt = ""
	j.UpdatedAt = now
	return j, nil
}

// GetJob returns one job by id, or nil if absent.
func (s *Store) GetJob(id string) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return nil, err
	}
	j, err := scanJobRow(db.QueryRow(`SELECT `+jobSelectCols+` FROM slacker_jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// ListJobs returns recent jobs filtered optionally by device and status.
func (s *Store) ListJobs(device, status string, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 500)
	var where []string
	var args []any
	if device != "" {
		where = append(where, "target_device = ?")
		args = append(args, device)
	}
	if status != "" {
		where = append(where, "status = ?")
		args = append(args, status)
	}
	q := `SELECT ` + jobSelectCols + ` FROM slacker_jobs`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	q += ` ORDER BY created_at DESC, rowid DESC LIMIT ?`
	args = append(args, limit)

	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Job
	for rows.Next() {
		j, err := scanJobRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// RetryJob resets a failed, cancelled, or claimed job back to queued and wakes target device waiters.
func (s *Store) RetryJob(id string) (Job, error) {
	now := registry.NowISO()
	var retried Job

	err := func() error {
		s.mu.Lock()
		defer s.mu.Unlock()

		tx, err := s.beginImmediateLocked(context.Background())
		if err != nil {
			return err
		}
		defer tx.Rollback(context.Background())

		cur, err := scanJobRow(tx.QueryRow(`SELECT `+jobSelectCols+` FROM slacker_jobs WHERE id = ?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return newSlackerErr("job not found", 404)
		}
		if err != nil {
			return err
		}
		if cur.Status != "failed" && cur.Status != "cancelled" && cur.Status != "claimed" {
			return newSlackerErr(fmt.Sprintf("job %s is %s and cannot be retried", id, cur.Status), 409)
		}
		if cur.ChannelID != "" && cur.MessageTS != "" {
			var otherID string
			oErr := tx.QueryRow(
				`SELECT id FROM slacker_jobs
				 WHERE id != ? AND account = ? AND channel_id = ? AND message_ts = ?
				   AND status IN ('queued', 'claimed', 'launched')
				 LIMIT 1`,
				cur.ID, cmp.Or(cur.Account, DefaultAccountID), cur.ChannelID, cur.MessageTS,
			).Scan(&otherID)
			if oErr == nil {
				return newSlackerErr(fmt.Sprintf("message already has an active or launched job %s", otherID), 409)
			}
			if !errors.Is(oErr, sql.ErrNoRows) {
				return oErr
			}
		}

		res, err := tx.Exec(
			`UPDATE slacker_jobs
			 SET status = 'queued', claimed_by = '', claimed_at = '', lease_expires_at = '', error = '', updated_at = ?
			 WHERE id = ? AND status IN ('failed', 'cancelled', 'claimed')`,
			now, id,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return newSlackerErr(fmt.Sprintf("job %s is %s and cannot be retried", id, cur.Status), 409)
		}
		j, gErr := scanJobRow(tx.QueryRow(`SELECT `+jobSelectCols+` FROM slacker_jobs WHERE id = ?`, id))
		if gErr != nil {
			return gErr
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		retried = j
		return nil
	}()
	if err != nil {
		return Job{}, err
	}
	s.NotifyDevice(retried.TargetDevice)
	return retried, nil
}

// CancelJob marks a queued or claimed job as cancelled.
func (s *Store) CancelJob(id string) (Job, error) {
	now := registry.NowISO()
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.beginImmediateLocked(context.Background())
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback(context.Background())

	res, err := tx.Exec(
		`UPDATE slacker_jobs
		 SET status = 'cancelled', lease_expires_at = '', updated_at = ?
		 WHERE id = ? AND status IN ('queued', 'claimed')`,
		now, id,
	)
	if err != nil {
		return Job{}, err
	}
	n, _ := res.RowsAffected()
	j, gErr := scanJobRow(tx.QueryRow(`SELECT `+jobSelectCols+` FROM slacker_jobs WHERE id = ?`, id))
	if errors.Is(gErr, sql.ErrNoRows) {
		return Job{}, newSlackerErr("job not found", 404)
	}
	if gErr != nil {
		return Job{}, gErr
	}
	if n == 0 {
		return Job{}, newSlackerErr(fmt.Sprintf("job %s is %s and cannot be cancelled", id, j.Status), 409)
	}
	if err := tx.Commit(); err != nil {
		return Job{}, err
	}
	return j, nil
}

// UpsertRuntime records listener runtime status in slacker_runtime.
func (s *Store) UpsertRuntime(role string, pid int, connected bool, lastEventAt, lastErr string) error {
	now := registry.NowISO()
	connInt := 0
	if connected {
		connInt = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return errors.New("slacker store closed")
	}
	_, err := s.db.Exec(
		`INSERT INTO slacker_runtime(role, pid, connected, last_poll_at, last_event_at, last_error, updated_at)
		 VALUES(?, ?, ?, '', ?, ?, ?)
		 ON CONFLICT(role) DO UPDATE SET
		   pid = excluded.pid,
		   connected = excluded.connected,
		   last_event_at = CASE WHEN excluded.last_event_at != '' THEN excluded.last_event_at ELSE slacker_runtime.last_event_at END,
		   last_error = excluded.last_error,
		   updated_at = excluded.updated_at`,
		role, pid, connInt, lastEventAt, lastErr, now,
	)
	return err
}

// UpsertWorkerRuntime records a target Mac worker poll heartbeat under role="worker:<device>".
func (s *Store) UpsertWorkerRuntime(device string, pid int, connected bool, lastErr string) error {
	now := registry.NowISO()
	connInt := 0
	if connected {
		connInt = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return errors.New("slacker store closed")
	}
	_, err := s.db.Exec(
		`INSERT INTO slacker_runtime(role, pid, connected, last_poll_at, last_event_at, last_error, updated_at)
		 VALUES(?, ?, ?, ?, '', ?, ?)
		 ON CONFLICT(role) DO UPDATE SET
		   pid = CASE WHEN excluded.pid > 0 THEN excluded.pid ELSE slacker_runtime.pid END,
		   connected = excluded.connected,
		   last_poll_at = excluded.last_poll_at,
		   last_error = excluded.last_error,
		   updated_at = excluded.updated_at`,
		"worker:"+device, pid, connInt, now, lastErr, now,
	)
	return err
}

// ListRuntimes returns all recorded listener and worker runtime rows.
func (s *Store) ListRuntimes() ([]RuntimeStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(
		`SELECT role, pid, connected, last_poll_at, last_event_at, last_error, updated_at
		 FROM slacker_runtime ORDER BY role ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RuntimeStatus
	for rows.Next() {
		var r RuntimeStatus
		var conn int
		if err := rows.Scan(&r.Role, &r.PID, &conn, &r.LastPollAt, &r.LastEventAt, &r.LastError, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Connected = conn != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// JobCounts returns counts grouped by status.
func (s *Store) JobCounts() (map[string]int, error) {
	counts := map[string]int{
		"total":     0,
		"queued":    0,
		"claimed":   0,
		"launched":  0,
		"failed":    0,
		"cancelled": 0,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT status, COUNT(*) FROM slacker_jobs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		counts[st] = n
		counts["total"] += n
	}
	return counts, rows.Err()
}

// Cleanup deletes terminal jobs (launched, failed, cancelled) older than olderThan (or all terminal if allTerminal is true)
// and stale worker heartbeats. Queued and claimed jobs are never deleted.
func (s *Store) Cleanup(olderThan time.Duration, allTerminal bool) (int64, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return 0, 0, err
	}

	var jobRes sql.Result
	if allTerminal || olderThan <= 0 {
		jobRes, err = db.Exec(`DELETE FROM slacker_jobs WHERE status IN ('launched', 'failed', 'cancelled')`)
	} else {
		cutoff := time.Now().UTC().Add(-olderThan).Truncate(time.Second).Format("2006-01-02T15:04:05+00:00")
		jobRes, err = db.Exec(
			`DELETE FROM slacker_jobs WHERE status IN ('launched', 'failed', 'cancelled') AND updated_at <= ?`,
			cutoff,
		)
	}
	if err != nil {
		return 0, 0, err
	}
	prunedJobs, _ := jobRes.RowsAffected()

	var prunedWorkers int64
	if olderThan > 0 {
		cutoff := time.Now().UTC().Add(-olderThan).Truncate(time.Second).Format("2006-01-02T15:04:05+00:00")
		wRes, wErr := db.Exec(`DELETE FROM slacker_runtime WHERE role LIKE 'worker:%' AND updated_at <= ?`, cutoff)
		if wErr != nil {
			return prunedJobs, 0, wErr
		}
		prunedWorkers, _ = wRes.RowsAffected()
	}
	return prunedJobs, prunedWorkers, nil
}

// PendingJobCounts returns the count of queued or claimed jobs per target device.
func (s *Store) PendingJobCounts() (map[string]int, error) {
	counts := map[string]int{}
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(
		`SELECT target_device, COUNT(*) FROM slacker_jobs WHERE status IN ('queued', 'claimed') GROUP BY target_device`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var dev string
		var n int
		if err := rows.Scan(&dev, &n); err != nil {
			return nil, err
		}
		counts[dev] = n
	}
	return counts, rows.Err()
}

// OnlineWorkers returns devices whose Slacker worker reported connected within window of now.
func (s *Store) OnlineWorkers(now time.Time, window time.Duration) (map[string]bool, error) {
	online := map[string]bool{}
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(
		`SELECT role, last_poll_at, updated_at FROM slacker_runtime WHERE role LIKE 'worker:%' AND connected = 1`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var role, lastPoll, updatedAt string
		if err := rows.Scan(&role, &lastPoll, &updatedAt); err != nil {
			return nil, err
		}
		rawTS := cmp.Or(lastPoll, updatedAt)
		if rawTS == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339, rawTS)
		if err != nil {
			continue
		}
		if now.UTC().Sub(ts.UTC()) <= window {
			online[strings.TrimPrefix(role, "worker:")] = true
		}
	}
	return online, rows.Err()
}

// GetLastDevice returns the last dispatched target device for scope, or empty if unset.
func (s *Store) GetLastDevice(scope string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return "", err
	}
	var dev string
	err = db.QueryRow(`SELECT last_device FROM slacker_dispatch_state WHERE scope = ?`, scope).Scan(&dev)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return dev, err
}

// SetLastDevice persists the last dispatched target device for scope.
func (s *Store) SetLastDevice(scope, device string) error {
	now := registry.NowISO()
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.dbLocked()
	if err != nil {
		return err
	}
	_, err = db.Exec(
		`INSERT INTO slacker_dispatch_state(scope, last_device, updated_at)
		 VALUES(?, ?, ?)
		 ON CONFLICT(scope) DO UPDATE SET
		   last_device = excluded.last_device,
		   updated_at = excluded.updated_at`,
		scope, device, now,
	)
	return err
}
