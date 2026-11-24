// Package agents implements the SQLite agent session registry and cross-node message broker (agents.db).
package agents

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/registry"
)

const schema = `
CREATE TABLE IF NOT EXISTS agent_sessions (
  session_id          TEXT PRIMARY KEY,
  device              TEXT NOT NULL,
  harness             TEXT NOT NULL,
  raw_id              TEXT NOT NULL,
  full_raw_id         TEXT NOT NULL DEFAULT '',
  pid                 INTEGER,
  cwd                 TEXT NOT NULL DEFAULT '',
  repo                TEXT NOT NULL DEFAULT '',
  branch              TEXT NOT NULL DEFAULT '',
  state               TEXT NOT NULL DEFAULT 'working',
  state_changed_at    TEXT NOT NULL DEFAULT '',
  pending_action_json TEXT NOT NULL DEFAULT '',
  title               TEXT NOT NULL DEFAULT '',
  name                TEXT NOT NULL DEFAULT '',
  custom_name         INTEGER NOT NULL DEFAULT 0,
  summary             TEXT NOT NULL DEFAULT '',
  git_status          TEXT NOT NULL DEFAULT '',
  tail_json           TEXT NOT NULL DEFAULT '[]',
  started_at          TEXT NOT NULL,
  updated_at          TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_agent_sessions_lookup
  ON agent_sessions(device, harness, cwd, updated_at);

CREATE TABLE IF NOT EXISTS agent_messages (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  thread_id    TEXT NOT NULL,
  reply_to     INTEGER,
  from_session TEXT NOT NULL,
  from_device  TEXT NOT NULL,
  to_device    TEXT,
  to_session   TEXT,
  to_repo      TEXT,
  body         TEXT NOT NULL,
  created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_agent_messages_inbox
  ON agent_messages(to_session, to_device, id);

CREATE TABLE IF NOT EXISTS agent_reads (
  session_id  TEXT NOT NULL,
  message_id  INTEGER NOT NULL,
  read_at     TEXT NOT NULL,
  PRIMARY KEY (session_id, message_id)
);
`

var sessionMigrations = []struct {
	col string
	def string
}{
	{"full_raw_id", "TEXT NOT NULL DEFAULT ''"},
	{"state_changed_at", "TEXT NOT NULL DEFAULT ''"},
	{"pending_action_json", "TEXT NOT NULL DEFAULT ''"},
	{"title", "TEXT NOT NULL DEFAULT ''"},
	{"name", "TEXT NOT NULL DEFAULT ''"},
	{"custom_name", "INTEGER NOT NULL DEFAULT 0"},
}

const (
	MaxMessageBytes         = 32 * 1024
	WaiterRearmGraceSeconds = 20.0
)

var (
	rawIDBadRE = regexp.MustCompile(`[^a-zA-Z0-9_-]`)
	slugWordRE = regexp.MustCompile(`[a-z0-9]+`)
)

// IsWaitingState reports whether state is a user-blocked waiting state.
func IsWaitingState(state string) bool {
	return state == "waiting_permission" || state == "waiting_question"
}

// IsValidState reports whether state is a valid session state.
func IsValidState(state string) bool {
	switch state {
	case "working", "waiting_permission", "waiting_question", "idle", "exited":
		return true
	}
	return false
}

// IsValidStateFilter reports whether state is a valid list filter.
func IsValidStateFilter(state string) bool {
	return IsValidState(state) || state == "waiting"
}

// AgentError is a caller-level error carrying an HTTP status code.
type AgentError struct {
	Msg    string
	Status int
}

func (e *AgentError) Error() string { return e.Msg }

func newAgentErr(msg string, status int) *AgentError {
	if status == 0 {
		status = 400
	}
	return &AgentError{Msg: msg, Status: status}
}

// TailItem is a normalized transcript entry stored on a session.
type TailItem struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// Session represents a serialized agent session in list/report responses.
type Session struct {
	SessionID      string            `json:"session_id"`
	Device         string            `json:"device"`
	Harness        string            `json:"harness"`
	FullRawID      string            `json:"full_raw_id"`
	PID            *int              `json:"pid"`
	CWD            string            `json:"cwd"`
	Repo           string            `json:"repo"`
	Branch         string            `json:"branch"`
	State          string            `json:"state"`
	StateChangedAt string            `json:"state_changed_at"`
	WaitingSeconds *int              `json:"waiting_seconds"`
	PendingAction  map[string]string `json:"pending_action"`
	Live           bool              `json:"live"`
	Listening      bool              `json:"listening"`
	Delivery       string            `json:"delivery"`
	AgeSeconds     int               `json:"age_seconds"`
	Title          string            `json:"title"`
	Name           string            `json:"name"`
	Summary        string            `json:"summary"`
	GitStatus      string            `json:"git_status"`
	TurnCount      int               `json:"turn_count"`
	StartedAt      string            `json:"started_at"`
	UpdatedAt      string            `json:"updated_at"`
}

// SessionDetail extends Session with the recent transcript tail.
type SessionDetail struct {
	Session
	Tail []TailItem `json:"tail"`
}

// Message represents a peer message in agents.db.
type Message struct {
	ID          int64   `json:"id"`
	ThreadID    string  `json:"thread_id"`
	ReplyTo     *int64  `json:"reply_to"`
	FromSession string  `json:"from_session"`
	FromDevice  string  `json:"from_device"`
	FromName    string  `json:"from_name"`
	FromTitle   string  `json:"from_title"`
	ToDevice    *string `json:"to_device"`
	ToSession   *string `json:"to_session"`
	ToRepo      *string `json:"to_repo"`
	Body        string  `json:"body"`
	CreatedAt   string  `json:"created_at"`
}

// RecipientsMap groups live broadcast recipients by delivery channel.
type RecipientsMap struct {
	Immediate   []string `json:"immediate"`
	NextHook    []string `json:"next_hook"`
	WaitingUser []string `json:"waiting_user"`
	QueuedIdle  []string `json:"queued_idle"`
}

// DeliveryReport describes how a sent message is expected to reach its recipient(s).
type DeliveryReport struct {
	Delivery    string         `json:"delivery"`
	WokeWaiters int            `json:"woke_waiters"`
	Recipients  *RecipientsMap `json:"recipients"`
	Warning     *string        `json:"warning"`
}

// SendResult is returned by SendMessageWithReport.
type SendResult struct {
	Message Message `json:"message"`
	DeliveryReport
}

// HeartbeatParams contains fields accepted by Heartbeat.
type HeartbeatParams struct {
	Device        string
	Harness       string
	RawID         string
	PID           *int
	CWD           string
	Repo          string
	Branch        string
	State         string
	PendingAction map[string]string
	Title         *string
	Name          *string
	ExplicitName  bool
	Summary       *string
	GitStatus     *string
	Tail          []TailItem
	DrainInbox    bool
}

// HeartbeatResult is returned by Heartbeat.
type HeartbeatResult struct {
	SessionID string    `json:"session_id"`
	FullRawID string    `json:"full_raw_id"`
	Name      string    `json:"name"`
	Title     string    `json:"title"`
	Unread    []Message `json:"unread"`
}

// WaitParams controls WaitForMessages.
type WaitParams struct {
	Device        string
	SessionID     string
	Timeout       float64
	MarkRead      bool
	Limit         int
	RequireFullID bool
}

// WaitResult is returned by WaitForMessages.
type WaitResult struct {
	SessionID string    `json:"session_id"`
	FullRawID string    `json:"full_raw_id"`
	Name      string    `json:"name"`
	Title     string    `json:"title"`
	Messages  []Message `json:"messages"`
	TimedOut  bool      `json:"timed_out"`
	Exited    bool      `json:"exited"`
}

// AskParams controls Ask.
type AskParams struct {
	Device       string
	FromSession  string
	To           string
	Body         string
	Timeout      float64
	WaitIfIdle   bool
	KnownDevices map[string]bool
}

// AskResult is returned by Ask.
type AskResult struct {
	Sent     Message  `json:"sent"`
	Reply    *Message `json:"reply"`
	TimedOut bool     `json:"timed_out"`
	Delivered bool    `json:"delivered"`
	Delivery string   `json:"delivery"`
	Warning  *string  `json:"warning"`
}

// TruncateRunes truncates s to at most maxRunes Unicode code points.
func TruncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	n := 0
	for i := range s {
		if n == maxRunes {
			return s[:i]
		}
		n++
	}
	return s
}

// NormalizeRawID sanitizes a harness raw session id and truncates non-placeholder IDs to 12 chars.
func NormalizeRawID(rawID string) (string, error) {
	cleaned := rawIDBadRE.ReplaceAllString(rawID, "")
	if cleaned == "" {
		return "", newAgentErr("session raw_id must not be empty", 400)
	}
	if strings.HasPrefix(cleaned, "pid-") {
		return cleaned, nil
	}
	if len(cleaned) > 12 {
		cleaned = cleaned[:12]
	}
	return cleaned, nil
}

// MakeSessionID constructs `<device>:<harness>:<short_id>`.
func MakeSessionID(device, harness, rawID string) (string, string, error) {
	if !paths.ValidHarness(harness) {
		return "", "", newAgentErr(fmt.Sprintf("unknown harness %q", harness), 400)
	}
	short, err := NormalizeRawID(rawID)
	if err != nil {
		return "", "", err
	}
	return fmt.Sprintf("%s:%s:%s", device, harness, short), short, nil
}

// SlugifyName converts arbitrary title/name text into a hyphenated lowercase handle.
func SlugifyName(text string, maxWords, maxLen int) string {
	if maxWords <= 0 {
		maxWords = 5
	}
	if maxLen <= 0 {
		maxLen = 32
	}
	tokens := slugWordRE.FindAllString(strings.ToLower(text), -1)
	if len(tokens) == 0 {
		return ""
	}
	if len(tokens) > maxWords {
		tokens = tokens[:maxWords]
	}
	slug := strings.Join(tokens, "-")
	if len(slug) > maxLen {
		slug = slug[:maxLen]
	}
	return strings.TrimRight(slug, "-")
}

func isLiveRow(state string, age int) bool {
	if state == "exited" {
		return false
	}
	limit := paths.AgentActiveSeconds
	if IsWaitingState(state) {
		limit = paths.AgentWaitingActiveSeconds
	}
	return age <= limit
}

func normalizePendingAction(action map[string]string) map[string]string {
	if len(action) == 0 {
		return nil
	}
	limits := []struct {
		key    string
		maxLen int
	}{
		{"kind", 32},
		{"tool", 120},
		{"detail", 1000},
		{"event", 64},
	}
	cleaned := map[string]string{}
	for _, spec := range limits {
		if raw, ok := action[spec.key]; ok {
			text := TruncateRunes(strings.TrimSpace(raw), spec.maxLen)
			if text != "" {
				cleaned[spec.key] = text
			}
		}
	}
	if len(cleaned) == 0 {
		return nil
	}
	return cleaned
}

func parsePendingAction(raw string) map[string]string {
	if raw == "" {
		return nil
	}
	var obj map[string]string
	if err := json.Unmarshal([]byte(raw), &obj); err != nil || len(obj) == 0 {
		return nil
	}
	return obj
}

// DeliveryMode classifies how messages to a session are delivered.
func DeliveryMode(live bool, state string, listening bool) string {
	if !live || state == "exited" {
		return "offline"
	}
	if IsWaitingState(state) {
		return "waiting_user"
	}
	if listening {
		return "immediate"
	}
	if state == "working" {
		return "next_hook"
	}
	return "queued_idle"
}

type sessionRow struct {
	sessionID         string
	device            string
	harness           string
	rawID             string
	fullRawID         string
	pid               sql.NullInt64
	cwd               string
	repo              string
	branch            string
	state             string
	stateChangedAt    string
	pendingActionJSON string
	title             string
	name              string
	customName        int
	summary           string
	gitStatus         string
	tailJSON          string
	startedAt         string
	updatedAt         string
}

const sessionSelectCols = "session_id, device, harness, raw_id, full_raw_id, pid, cwd, repo, branch, " +
	"state, state_changed_at, pending_action_json, title, name, custom_name, summary, git_status, tail_json, started_at, updated_at"

func scanSessionRow(scanner interface{ Scan(...any) error }) (*sessionRow, error) {
	var r sessionRow
	err := scanner.Scan(
		&r.sessionID, &r.device, &r.harness, &r.rawID, &r.fullRawID,
		&r.pid, &r.cwd, &r.repo, &r.branch, &r.state,
		&r.stateChangedAt, &r.pendingActionJSON, &r.title, &r.name,
		&r.customName, &r.summary, &r.gitStatus, &r.tailJSON, &r.startedAt, &r.updatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func scanAllSessionRows(rows *sql.Rows) ([]sessionRow, error) {
	defer func() { _ = rows.Close() }()
	var out []sessionRow
	for rows.Next() {
		r, err := scanSessionRow(rows)
		if err != nil {
			return nil, err
		}
		if r != nil {
			out = append(out, *r)
		}
	}
	return out, rows.Err()
}

func parseTail(raw string) []TailItem {
	if raw == "" {
		return []TailItem{}
	}
	var tail []TailItem
	if err := json.Unmarshal([]byte(raw), &tail); err != nil || tail == nil {
		return []TailItem{}
	}
	return tail
}

func parseTS(iso string) time.Time {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func rowToSession(r *sessionRow, now time.Time, listening bool) Session {
	updated := parseTS(r.updatedAt)
	age := int(now.Sub(updated).Seconds())
	if age < 0 {
		age = 0
	}
	live := isLiveRow(r.state, age)
	tail := parseTail(r.tailJSON)
	stateChangedAt := r.stateChangedAt
	if stateChangedAt == "" {
		stateChangedAt = r.updatedAt
	}
	var pendingAction map[string]string
	var waitingSecs *int
	if IsWaitingState(r.state) {
		pendingAction = parsePendingAction(r.pendingActionJSON)
		ws := int(now.Sub(parseTS(stateChangedAt)).Seconds())
		if ws < 0 {
			ws = 0
		}
		waitingSecs = &ws
	}
	isListening := listening && live && r.state != "exited"
	var pidPtr *int
	if r.pid.Valid {
		p := int(r.pid.Int64)
		pidPtr = &p
	}
	return Session{
		SessionID:      r.sessionID,
		Device:         r.device,
		Harness:        r.harness,
		FullRawID:      r.fullRawID,
		PID:            pidPtr,
		CWD:            r.cwd,
		Repo:           r.repo,
		Branch:         r.branch,
		State:          r.state,
		StateChangedAt: stateChangedAt,
		WaitingSeconds: waitingSecs,
		PendingAction:  pendingAction,
		Live:           live,
		Listening:      isListening,
		Delivery:       DeliveryMode(live, r.state, isListening),
		AgeSeconds:     age,
		Title:          r.title,
		Name:           r.name,
		Summary:        r.summary,
		GitStatus:      r.gitStatus,
		TurnCount:      len(tail),
		StartedAt:      r.startedAt,
		UpdatedAt:      r.updatedAt,
	}
}

// Hub is the SQLite agent session registry and cross-node message broker.
type Hub struct {
	Path        string
	mu          sync.Mutex
	db          *sql.DB
	lastPrune   time.Time
	waiters     map[string]int
	waitSeenAt  map[string]time.Time
	promotedTo  map[string]string
	sessionSubs map[string]map[chan struct{}]struct{}
	threadSubs  map[string]map[chan struct{}]struct{}
	closed      bool
	doneCh      chan struct{}
}

// Open opens or initializes agents.db at path, creating its parent directory and running migrations.
func Open(path string) (*Hub, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open agents db %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := os.Stat(path); err == nil {
		_ = os.Chmod(path, 0o600)
	}
	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("agents %s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init agents schema: %w", err)
	}
	if err := migrateSessions(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return &Hub{
		Path:        path,
		db:          db,
		waiters:     map[string]int{},
		waitSeenAt:  map[string]time.Time{},
		promotedTo:  map[string]string{},
		sessionSubs: map[string]map[chan struct{}]struct{}{},
		threadSubs:  map[string]map[chan struct{}]struct{}{},
		doneCh:      make(chan struct{}),
	}, nil
}

func migrateSessions(db *sql.DB) error {
	rows, err := db.Query("PRAGMA table_info(agent_sessions)")
	if err != nil {
		return err
	}
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return err
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, m := range sessionMigrations {
		if !cols[m.col] {
			if _, err := db.Exec(fmt.Sprintf("ALTER TABLE agent_sessions ADD COLUMN %s %s", m.col, m.def)); err != nil {
				return fmt.Errorf("migrate agent_sessions.%s: %w", m.col, err)
			}
		}
	}
	return nil
}

// DB exposes the raw *sql.DB for testing.
func (h *Hub) DB() *sql.DB { return h.db }

// Close shuts down waiters and closes agents.db.
func (h *Hub) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	close(h.doneCh)
	if h.db == nil {
		return nil
	}
	err := h.db.Close()
	h.db = nil
	return err
}

func (h *Hub) subscribeSessionLocked(sid string) chan struct{} {
	ch := make(chan struct{}, 1)
	m := h.sessionSubs[sid]
	if m == nil {
		m = map[chan struct{}]struct{}{}
		h.sessionSubs[sid] = m
	}
	m[ch] = struct{}{}
	return ch
}

func (h *Hub) unsubscribeSessionLocked(sid string, ch chan struct{}) {
	if m := h.sessionSubs[sid]; m != nil {
		delete(m, ch)
		if len(m) == 0 {
			delete(h.sessionSubs, sid)
		}
	}
}

func (h *Hub) notifySessionLocked(sid string) {
	for ch := range h.sessionSubs[sid] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (h *Hub) subscribeThreadLocked(threadID string) chan struct{} {
	ch := make(chan struct{}, 1)
	m := h.threadSubs[threadID]
	if m == nil {
		m = map[chan struct{}]struct{}{}
		h.threadSubs[threadID] = m
	}
	m[ch] = struct{}{}
	return ch
}

func (h *Hub) unsubscribeThreadLocked(threadID string, ch chan struct{}) {
	if m := h.threadSubs[threadID]; m != nil {
		delete(m, ch)
		if len(m) == 0 {
			delete(h.threadSubs, threadID)
		}
	}
}

func (h *Hub) notifyThreadLocked(threadID string) {
	for ch := range h.threadSubs[threadID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (h *Hub) notifyBroadcastLocked(fromSession string, toDevice *string) {
	for sid := range h.sessionSubs {
		if sid == fromSession {
			continue
		}
		if toDevice != nil {
			dev, _, _ := strings.Cut(sid, ":")
			if dev != *toDevice {
				continue
			}
		}
		h.notifySessionLocked(sid)
	}
}

// Prune deletes expired sessions, messages, and read receipts.
func (h *Hub) Prune(force bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pruneLocked(force)
}

func (h *Hub) pruneLocked(force bool) error {
	now := time.Now()
	if !force && !h.lastPrune.IsZero() && now.Sub(h.lastPrune) < 60*time.Second {
		return nil
	}
	h.lastPrune = now
	cutoff := now.UTC().Truncate(time.Second).Add(-time.Duration(paths.AgentRetentionSeconds) * time.Second).Format("2006-01-02T15:04:05+00:00")
	tx, err := h.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("DELETE FROM agent_sessions WHERE updated_at < ?", cutoff); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM agent_messages WHERE created_at < ?", cutoff); err != nil {
		return err
	}
	if _, err := tx.Exec(
		"DELETE FROM agent_reads WHERE read_at < ? OR message_id NOT IN (SELECT id FROM agent_messages)",
		cutoff,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (h *Hub) transferWaiterLocked(oldID, targetID string) {
	if oldID == targetID {
		return
	}
	if c, ok := h.waiters[oldID]; ok {
		delete(h.waiters, oldID)
		h.waiters[targetID] += c
	}
	if ts, ok := h.waitSeenAt[oldID]; ok {
		delete(h.waitSeenAt, oldID)
		if cur, exists := h.waitSeenAt[targetID]; !exists || ts.After(cur) {
			h.waitSeenAt[targetID] = ts
		}
	}
	h.promotedTo[oldID] = targetID
	h.notifySessionLocked(oldID)
}

func (h *Hub) migrateSessionIdentityLocked(execer interface {
	Exec(string, ...any) (sql.Result, error)
}, oldID, targetID string) error {
	if oldID == targetID {
		return nil
	}
	if _, err := execer.Exec("UPDATE agent_messages SET to_session = ? WHERE to_session = ?", targetID, oldID); err != nil {
		return err
	}
	if _, err := execer.Exec("UPDATE agent_messages SET from_session = ? WHERE from_session = ?", targetID, oldID); err != nil {
		return err
	}
	if _, err := execer.Exec(
		"INSERT OR IGNORE INTO agent_reads(session_id, message_id, read_at) "+
			"SELECT ?, message_id, read_at FROM agent_reads WHERE session_id = ?",
		targetID, oldID,
	); err != nil {
		return err
	}
	if _, err := execer.Exec("DELETE FROM agent_reads WHERE session_id = ?", oldID); err != nil {
		return err
	}
	if _, err := execer.Exec("DELETE FROM agent_sessions WHERE session_id = ?", oldID); err != nil {
		return err
	}
	h.transferWaiterLocked(oldID, targetID)
	return nil
}

func (h *Hub) isListeningLocked(sessionID, state string, live bool, now time.Time) bool {
	if !live || state == "exited" {
		return false
	}
	if h.waiters[sessionID] > 0 {
		return true
	}
	seen, ok := h.waitSeenAt[sessionID]
	if !ok {
		return false
	}
	return state == "working" && now.Sub(seen).Seconds() <= WaiterRearmGraceSeconds
}

func (h *Hub) serializeSessionLocked(r *sessionRow, nowUTC time.Time, nowMono time.Time) Session {
	updated := parseTS(r.updatedAt)
	age := int(nowUTC.Sub(updated).Seconds())
	if age < 0 {
		age = 0
	}
	live := isLiveRow(r.state, age)
	listening := h.isListeningLocked(r.sessionID, r.state, live, nowMono)
	return rowToSession(r, nowUTC, listening)
}

type querier interface {
	Query(string, ...any) (*sql.Rows, error)
}

func (h *Hub) uniqueAutoNameLocked(q querier, base, device, excludeSID string) (string, error) {
	if base == "" {
		return "", nil
	}
	now := time.Now().UTC().Truncate(time.Second)
	cutoff := now.Add(-time.Duration(paths.AgentActiveSeconds) * time.Second).Format("2006-01-02T15:04:05+00:00")
	waitingCutoff := now.Add(-time.Duration(paths.AgentWaitingActiveSeconds) * time.Second).Format("2006-01-02T15:04:05+00:00")
	rows, err := q.Query(
		"SELECT LOWER(name) AS lname FROM agent_sessions "+
			"WHERE device = ? AND state != 'exited' "+
			"AND (updated_at >= ? OR (state IN ('waiting_permission', 'waiting_question') AND updated_at >= ?)) "+
			"AND session_id != ? AND name != ''",
		device, cutoff, waitingCutoff, excludeSID,
	)
	if err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for rows.Next() {
		var lname string
		if err := rows.Scan(&lname); err != nil {
			_ = rows.Close()
			return "", err
		}
		taken[lname] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return "", err
	}
	_ = rows.Close()

	if !taken[strings.ToLower(base)] {
		return base, nil
	}
	for n := 2; ; n++ {
		suffix := fmt.Sprintf("-%d", n)
		prefixMax := 32 - len(suffix)
		if prefixMax < 1 {
			prefixMax = 1
		}
		pref := base
		if len(pref) > prefixMax {
			pref = pref[:prefixMax]
		}
		pref = strings.TrimRight(pref, "-")
		candidate := pref + suffix
		if !taken[strings.ToLower(candidate)] {
			return candidate, nil
		}
	}
}

func (h *Hub) resolveTitleAndNameLocked(
	q querier,
	device, sessionID, repo string,
	title, name *string,
	explicitName bool,
	baseTitle, baseName string,
	baseCustom int,
) (string, string, int, error) {
	effTitle := baseTitle
	if title != nil && strings.TrimSpace(*title) != "" {
		effTitle = TruncateRunes(strings.TrimSpace(*title), 160)
	}
	if baseCustom != 0 {
		return effTitle, baseName, 1, nil
	}
	if name != nil && strings.TrimSpace(*name) != "" {
		slug := SlugifyName(*name, 5, 32)
		if slug != "" {
			if explicitName {
				return effTitle, slug, 0, nil
			}
			uniq, err := h.uniqueAutoNameLocked(q, slug, device, sessionID)
			return effTitle, uniq, 0, err
		}
	}
	if effTitle != "" && (effTitle != baseTitle || baseName == "") {
		slug := SlugifyName(effTitle, 5, 32)
		if slug != "" {
			uniq, err := h.uniqueAutoNameLocked(q, slug, device, sessionID)
			return effTitle, uniq, 0, err
		}
	}
	if baseName == "" && repo != "" {
		slug := SlugifyName(repo, 5, 32)
		if slug != "" {
			uniq, err := h.uniqueAutoNameLocked(q, slug, device, sessionID)
			return effTitle, uniq, 0, err
		}
	}
	return effTitle, baseName, 0, nil
}

func (h *Hub) promotePlaceholderLocked(
	tx *sql.Tx,
	device, harness, cwd string,
	pid *int,
	targetID string,
) (*int, string, string, string, int, error) {
	if cwd == "" {
		return pid, "", "", "", 0, nil
	}
	cutoff := time.Now().UTC().Truncate(time.Second).Add(-time.Duration(paths.AgentActiveSeconds) * time.Second).Format("2006-01-02T15:04:05+00:00")
	rows, err := tx.Query(
		"SELECT session_id, pid, started_at, title, name, custom_name FROM agent_sessions "+
			"WHERE device = ? AND harness = ? AND cwd = ? AND raw_id LIKE 'pid-%' "+
			"AND state != 'exited' AND updated_at >= ? ORDER BY updated_at DESC",
		device, harness, cwd, cutoff,
	)
	if err != nil {
		return pid, "", "", "", 0, err
	}
	type phRow struct {
		sid        string
		pid        sql.NullInt64
		startedAt  string
		title      string
		name       string
		customName int
	}
	var cands []phRow
	for rows.Next() {
		var r phRow
		if err := rows.Scan(&r.sid, &r.pid, &r.startedAt, &r.title, &r.name, &r.customName); err != nil {
			_ = rows.Close()
			return pid, "", "", "", 0, err
		}
		cands = append(cands, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return pid, "", "", "", 0, err
	}
	_ = rows.Close()
	if len(cands) == 0 {
		return pid, "", "", "", 0, nil
	}
	chosen := cands[0]
	if pid != nil {
		for _, c := range cands {
			if c.pid.Valid && int(c.pid.Int64) == *pid {
				chosen = c
				break
			}
		}
	}
	if err := h.migrateSessionIdentityLocked(tx, chosen.sid, targetID); err != nil {
		return pid, "", "", "", 0, err
	}
	outPID := pid
	if outPID == nil && chosen.pid.Valid {
		v := int(chosen.pid.Int64)
		outPID = &v
	}
	return outPID, chosen.startedAt, chosen.title, chosen.name, chosen.customName, nil
}

func (h *Hub) findPromotedSessionLocked(device, harness string, pid int, cwd string) (*sessionRow, error) {
	now := time.Now().UTC().Truncate(time.Second)
	cutoff := now.Add(-time.Duration(paths.AgentActiveSeconds) * time.Second).Format("2006-01-02T15:04:05+00:00")
	waitingCutoff := now.Add(-time.Duration(paths.AgentWaitingActiveSeconds) * time.Second).Format("2006-01-02T15:04:05+00:00")
	byPID, err := scanSessionRow(h.db.QueryRow(
		"SELECT "+sessionSelectCols+" FROM agent_sessions "+
			"WHERE device = ? AND harness = ? AND pid = ? AND raw_id NOT LIKE 'pid-%' "+
			"AND state != 'exited' "+
			"AND (updated_at >= ? OR (state IN ('waiting_permission', 'waiting_question') AND updated_at >= ?)) "+
			"ORDER BY updated_at DESC LIMIT 1",
		device, harness, pid, cutoff, waitingCutoff,
	))
	if err != nil {
		return nil, err
	}
	if byPID != nil {
		return byPID, nil
	}
	if cwd != "" {
		rows, err := h.db.Query(
			"SELECT "+sessionSelectCols+" FROM agent_sessions "+
				"WHERE device = ? AND harness = ? AND cwd = ? AND pid IS NULL AND raw_id NOT LIKE 'pid-%' "+
				"AND state != 'exited' "+
				"AND (updated_at >= ? OR (state IN ('waiting_permission', 'waiting_question') AND updated_at >= ?)) "+
				"ORDER BY updated_at DESC",
			device, harness, cwd, cutoff, waitingCutoff,
		)
		if err != nil {
			return nil, err
		}
		list, err := scanAllSessionRows(rows)
		if err != nil {
			return nil, err
		}
		if len(list) == 1 {
			return &list[0], nil
		}
	}
	return nil, nil
}

func (h *Hub) fetchInboxLocked(device, sessionID string, unreadOnly, markRead bool, limit int) ([]Message, error) {
	if !strings.HasPrefix(sessionID, device+":") {
		return nil, newAgentErr("session belongs to another device", 403)
	}
	if limit < 1 {
		limit = 1
	} else if limit > 200 {
		limit = 200
	}
	var repo, startedAt string
	err := h.db.QueryRow("SELECT repo, started_at FROM agent_sessions WHERE session_id = ?", sessionID).Scan(&repo, &startedAt)
	if err == sql.ErrNoRows {
		repo = ""
		startedAt = "1970-01-01T00:00:00+00:00"
	} else if err != nil {
		return nil, err
	}
	unreadFlag := 0
	if unreadOnly {
		unreadFlag = 1
	}
	rows, err := h.db.Query(
		"SELECT m.id, m.thread_id, m.reply_to, m.from_session, m.from_device, "+
			"COALESCE(s.name, '') AS from_name, COALESCE(s.title, '') AS from_title, "+
			"m.to_device, m.to_session, m.to_repo, m.body, m.created_at "+
			"FROM agent_messages m "+
			"LEFT JOIN agent_sessions s ON s.session_id = m.from_session "+
			"WHERE m.from_session != ? "+
			"AND (m.to_session = ? OR (m.to_session IS NULL "+
			"  AND (m.to_device IS NULL OR m.to_device = ?) "+
			"  AND (m.to_repo IS NULL OR (m.to_repo = ? AND ? != '')) "+
			"  AND m.created_at >= ?)) "+
			"AND (? = 0 OR m.id NOT IN (SELECT message_id FROM agent_reads WHERE session_id = ?)) "+
			"ORDER BY m.id ASC LIMIT ?",
		sessionID, sessionID, device, repo, repo, startedAt, unreadFlag, sessionID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	messages := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = rows.Close()

	if markRead && len(messages) > 0 {
		readAt := registry.NowISO()
		tx, err := h.db.Begin()
		if err != nil {
			return nil, err
		}
		defer func() { _ = tx.Rollback() }()
		for _, m := range messages {
			if _, err := tx.Exec(
				"INSERT OR IGNORE INTO agent_reads(session_id, message_id, read_at) VALUES (?, ?, ?)",
				sessionID, m.ID, readAt,
			); err != nil {
				return nil, err
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func scanMessage(scanner interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var replyTo sql.NullInt64
	var toDevice, toSession, toRepo sql.NullString
	err := scanner.Scan(
		&m.ID, &m.ThreadID, &replyTo, &m.FromSession, &m.FromDevice,
		&m.FromName, &m.FromTitle,
		&toDevice, &toSession, &toRepo, &m.Body, &m.CreatedAt,
	)
	if err != nil {
		return Message{}, err
	}
	if replyTo.Valid {
		v := replyTo.Int64
		m.ReplyTo = &v
	}
	if toDevice.Valid {
		s := toDevice.String
		m.ToDevice = &s
	}
	if toSession.Valid {
		s := toSession.String
		m.ToSession = &s
	}
	if toRepo.Valid {
		s := toRepo.String
		m.ToRepo = &s
	}
	return m, nil
}

// FetchInbox returns incoming messages for sessionID.
func (h *Hub) FetchInbox(device, sessionID string, unreadOnly, markRead bool, limit int) ([]Message, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if limit <= 0 {
		limit = 50
	}
	return h.fetchInboxLocked(device, sessionID, unreadOnly, markRead, limit)
}

func marshalTailSlice(tail []TailItem) (string, error) {
	if len(tail) > 25 {
		tail = tail[len(tail)-25:]
	}
	if tail == nil {
		tail = []TailItem{}
	}
	b, err := json.Marshal(tail)
	if err != nil {
		return "[]", err
	}
	return string(b), nil
}

// Heartbeat upserts session state and metadata, optionally draining unread inbox messages.
func (h *Hub) Heartbeat(p HeartbeatParams) (HeartbeatResult, error) {
	if p.State == "" {
		p.State = "working"
	}
	if !IsValidState(p.State) {
		return HeartbeatResult{}, newAgentErr(fmt.Sprintf("invalid state %q", p.State), 400)
	}
	sessionID, shortID, err := MakeSessionID(p.Device, p.Harness, p.RawID)
	if err != nil {
		return HeartbeatResult{}, err
	}
	rawClean := TruncateRunes(strings.TrimSpace(p.RawID), 128)
	nowISO := registry.NowISO()

	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.pruneLocked(false); err != nil {
		return HeartbeatResult{}, err
	}

	if strings.HasPrefix(shortID, "pid-") && p.PID != nil {
		promoted, err := h.findPromotedSessionLocked(p.Device, p.Harness, *p.PID, p.CWD)
		if err != nil {
			return HeartbeatResult{}, err
		}
		if promoted != nil {
			targetID := promoted.sessionID
			effRepo := p.Repo
			if effRepo == "" {
				effRepo = promoted.repo
			}
			effTitle, effName, effCustom, err := h.resolveTitleAndNameLocked(
				h.db,
				p.Device, targetID, effRepo,
				p.Title, p.Name, p.ExplicitName,
				promoted.title, promoted.name, promoted.customName,
			)
			if err != nil {
				return HeartbeatResult{}, err
			}
			tx, err := h.db.Begin()
			if err != nil {
				return HeartbeatResult{}, err
			}
			defer func() { _ = tx.Rollback() }()
			if err := h.migrateSessionIdentityLocked(tx, sessionID, targetID); err != nil {
				return HeartbeatResult{}, err
			}
			var newPID any = *p.PID
			if promoted.pid.Valid {
				newPID = promoted.pid.Int64
			}
			effCWD := p.CWD
			if effCWD == "" {
				effCWD = promoted.cwd
			}
			effBranch := p.Branch
			if effBranch == "" {
				effBranch = promoted.branch
			}
			effSummary := promoted.summary
			if p.Summary != nil {
				effSummary = TruncateRunes(*p.Summary, 160)
			}
			effGit := promoted.gitStatus
			if p.GitStatus != nil {
				effGit = TruncateRunes(*p.GitStatus, 1500)
			}
			effTailJSON := promoted.tailJSON
			if p.Tail != nil {
				effTailJSON, err = marshalTailSlice(p.Tail)
				if err != nil {
					return HeartbeatResult{}, err
				}
			}
			if _, err := tx.Exec(
				"UPDATE agent_sessions SET pid = ?, cwd = ?, repo = ?, branch = ?, "+
					"title = ?, name = ?, custom_name = ?, summary = ?, git_status = ?, "+
					"tail_json = ?, updated_at = ? WHERE session_id = ?",
				newPID, effCWD, effRepo, effBranch,
				effTitle, effName, effCustom, effSummary, effGit,
				effTailJSON, nowISO, targetID,
			); err != nil {
				return HeartbeatResult{}, err
			}
			if err := tx.Commit(); err != nil {
				return HeartbeatResult{}, err
			}
			h.notifySessionLocked(targetID)
			unread := []Message{}
			if p.DrainInbox {
				unread, err = h.fetchInboxLocked(p.Device, targetID, true, true, 50)
				if err != nil {
					return HeartbeatResult{}, err
				}
			}
			return HeartbeatResult{
				SessionID: targetID,
				FullRawID: promoted.fullRawID,
				Name:      effName,
				Title:     effTitle,
				Unread:    unread,
			}, nil
		}
	}

	tx, err := h.db.Begin()
	if err != nil {
		return HeartbeatResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	oldPID := p.PID
	var oldStarted, oldTitle, oldName string
	var oldCustom int
	if !strings.HasPrefix(shortID, "pid-") {
		oldPID, oldStarted, oldTitle, oldName, oldCustom, err = h.promotePlaceholderLocked(
			tx, p.Device, p.Harness, p.CWD, p.PID, sessionID,
		)
		if err != nil {
			return HeartbeatResult{}, err
		}
	}

	existing, err := scanSessionRow(tx.QueryRow(
		"SELECT "+sessionSelectCols+" FROM agent_sessions WHERE session_id = ?", sessionID,
	))
	if err != nil {
		return HeartbeatResult{}, err
	}

	baseTitle := oldTitle
	if existing != nil && existing.title != "" {
		baseTitle = existing.title
	}
	var baseName string
	var baseCustom int
	if existing != nil && existing.customName != 0 {
		baseName = existing.name
		baseCustom = 1
	} else if oldCustom != 0 {
		baseName = oldName
		baseCustom = 1
	} else {
		baseName = oldName
		if existing != nil && existing.name != "" {
			baseName = existing.name
		}
		baseCustom = 0
	}

	effRepo := p.Repo
	if effRepo == "" && existing != nil {
		effRepo = existing.repo
	}
	effTitle, effName, effCustom, err := h.resolveTitleAndNameLocked(
		tx,
		p.Device, sessionID, effRepo,
		p.Title, p.Name, p.ExplicitName,
		baseTitle, baseName, baseCustom,
	)
	if err != nil {
		return HeartbeatResult{}, err
	}

	fullRaw := rawClean
	if strings.HasPrefix(shortID, "pid-") {
		if existing != nil {
			fullRaw = existing.fullRawID
		} else {
			fullRaw = ""
		}
	}

	var newPendingJSON string
	if IsWaitingState(p.State) {
		normAction := normalizePendingAction(p.PendingAction)
		existingSameState := existing != nil && existing.state == p.State
		var existingPending map[string]string
		if existingSameState {
			existingPending = parsePendingAction(existing.pendingActionJSON)
		}
		if normAction == nil {
			if existingSameState {
				newPendingJSON = existing.pendingActionJSON
			}
		} else if existingSameState && existingPending != nil && existingPending["tool"] != "" && normAction["tool"] == "" {
			newPendingJSON = existing.pendingActionJSON
		} else {
			b, err := json.Marshal(normAction)
			if err != nil {
				return HeartbeatResult{}, err
			}
			newPendingJSON = string(b)
		}
	}

	var newStateChangedAt string
	if existing != nil && existing.state == p.State && (!IsWaitingState(p.State) || newPendingJSON == existing.pendingActionJSON) {
		newStateChangedAt = existing.stateChangedAt
		if newStateChangedAt == "" {
			newStateChangedAt = existing.updatedAt
		}
	} else {
		newStateChangedAt = nowISO
	}

	if existing != nil {
		effFullRaw := fullRaw
		if effFullRaw == "" {
			effFullRaw = existing.fullRawID
		}
		var pidVal any
		if oldPID != nil {
			pidVal = *oldPID
		} else if existing.pid.Valid {
			pidVal = existing.pid.Int64
		}
		effCWD := p.CWD
		if effCWD == "" {
			effCWD = existing.cwd
		}
		effBranch := p.Branch
		if effBranch == "" {
			effBranch = existing.branch
		}
		effSummary := existing.summary
		if p.Summary != nil {
			effSummary = TruncateRunes(*p.Summary, 160)
		}
		effGit := existing.gitStatus
		if p.GitStatus != nil {
			effGit = TruncateRunes(*p.GitStatus, 1500)
		}
		effTail := existing.tailJSON
		if p.Tail != nil {
			effTail, err = marshalTailSlice(p.Tail)
			if err != nil {
				return HeartbeatResult{}, err
			}
		}
		effStarted := oldStarted
		if effStarted == "" {
			effStarted = existing.startedAt
		}
		if _, err := tx.Exec(
			"UPDATE agent_sessions SET full_raw_id = ?, pid = ?, cwd = ?, repo = ?, branch = ?, "+
				"state = ?, state_changed_at = ?, pending_action_json = ?, title = ?, name = ?, "+
				"custom_name = ?, summary = ?, git_status = ?, tail_json = ?, started_at = ?, "+
				"updated_at = ? WHERE session_id = ?",
			effFullRaw, pidVal, effCWD, effRepo, effBranch,
			p.State, newStateChangedAt, newPendingJSON, effTitle, effName,
			effCustom, effSummary, effGit, effTail, effStarted,
			nowISO, sessionID,
		); err != nil {
			return HeartbeatResult{}, err
		}
	} else {
		var pidVal any
		if oldPID != nil {
			pidVal = *oldPID
		}
		var sumVal, gitVal string
		if p.Summary != nil {
			sumVal = TruncateRunes(*p.Summary, 160)
		}
		if p.GitStatus != nil {
			gitVal = TruncateRunes(*p.GitStatus, 1500)
		}
		tailJSON, err := marshalTailSlice(p.Tail)
		if err != nil {
			return HeartbeatResult{}, err
		}
		effStarted := oldStarted
		if effStarted == "" {
			effStarted = nowISO
		}
		if _, err := tx.Exec(
			"INSERT INTO agent_sessions(session_id, device, harness, raw_id, full_raw_id, pid, cwd, repo, "+
				"branch, state, state_changed_at, pending_action_json, title, name, custom_name, summary, "+
				"git_status, tail_json, started_at, updated_at) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			sessionID, p.Device, p.Harness, shortID, fullRaw, pidVal, p.CWD, p.Repo,
			p.Branch, p.State, newStateChangedAt, newPendingJSON, effTitle, effName, effCustom, sumVal,
			gitVal, tailJSON, effStarted, nowISO,
		); err != nil {
			return HeartbeatResult{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return HeartbeatResult{}, err
	}
	h.notifySessionLocked(sessionID)

	unread := []Message{}
	if p.DrainInbox {
		unread, err = h.fetchInboxLocked(p.Device, sessionID, true, true, 50)
		if err != nil {
			return HeartbeatResult{}, err
		}
	}
	return HeartbeatResult{
		SessionID: sessionID,
		FullRawID: fullRaw,
		Name:      effName,
		Title:     effTitle,
		Unread:    unread,
	}, nil
}

// RenameSession sets an explicit custom friendly handle on a session belonging to device.
func (h *Hub) RenameSession(device, sessionID, newName string) (string, error) {
	if !strings.HasPrefix(sessionID, device+":") {
		return "", newAgentErr("session belongs to another device", 403)
	}
	raw := strings.ToLower(strings.TrimSpace(newName))
	if strings.HasPrefix(raw, "repo:") || strings.HasPrefix(raw, "device:") {
		return "", newAgentErr("invalid session name", 400)
	}
	slug := SlugifyName(newName, 6, 40)
	if slug == "" || slug == "all" {
		return "", newAgentErr("session name must contain letters or digits and cannot be reserved", 400)
	}
	nowISO := registry.NowISO()
	h.mu.Lock()
	defer h.mu.Unlock()
	res, err := h.db.Exec(
		"UPDATE agent_sessions SET name = ?, custom_name = 1, updated_at = ? WHERE device = ? AND session_id = ?",
		slug, nowISO, device, sessionID,
	)
	if err != nil {
		return "", err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if affected == 0 {
		return "", newAgentErr(fmt.Sprintf("no agent session %q", sessionID), 404)
	}
	return slug, nil
}

// Deregister marks a session (or sessions matching harness+pid) as exited and wakes waiters.
func (h *Hub) Deregister(device string, sessionID, harness *string, pid *int) error {
	if sessionID != nil && *sessionID != "" && !strings.HasPrefix(*sessionID, device+":") {
		return newAgentErr("session belongs to another device", 403)
	}
	nowISO := registry.NowISO()
	h.mu.Lock()
	defer h.mu.Unlock()
	if sessionID != nil && *sessionID != "" {
		if _, err := h.db.Exec(
			"UPDATE agent_sessions SET state = 'exited', state_changed_at = ?, pending_action_json = '', updated_at = ? WHERE device = ? AND session_id = ?",
			nowISO, nowISO, device, *sessionID,
		); err != nil {
			return err
		}
		h.notifySessionLocked(*sessionID)
		return nil
	}
	if harness != nil && *harness != "" && pid != nil {
		rows, err := h.db.Query(
			"SELECT session_id FROM agent_sessions WHERE device = ? AND harness = ? AND pid = ?",
			device, *harness, *pid,
		)
		var sids []string
		if err == nil {
			for rows.Next() {
				var s string
				if rows.Scan(&s) == nil {
					sids = append(sids, s)
				}
			}
			_ = rows.Close()
		}
		if _, err := h.db.Exec(
			"UPDATE agent_sessions SET state = 'exited', state_changed_at = ?, pending_action_json = '', updated_at = ? WHERE device = ? AND harness = ? AND pid = ?",
			nowISO, nowISO, device, *harness, *pid,
		); err != nil {
			return err
		}
		for _, s := range sids {
			h.notifySessionLocked(s)
		}
		return nil
	}
	return newAgentErr("session_id or (harness, pid) is required", 400)
}

// ResolveCallerSession resolves a caller's session_id from explicit ID, pid, cwd, or creates a placeholder.
func (h *Hub) ResolveCallerSession(device, harness string, sessionID *string, cwd string, pid *int) (string, error) {
	if harness == "" {
		harness = "claude"
	}
	if sessionID != nil && *sessionID != "" {
		sid := *sessionID
		if strings.Contains(sid, ":") {
			if !strings.HasPrefix(sid, device+":") {
				return "", newAgentErr("session belongs to another device", 403)
			}
			h.mu.Lock()
			cur := sid
			for {
				next, ok := h.promotedTo[cur]
				if !ok {
					break
				}
				cur = next
			}
			h.mu.Unlock()
			return cur, nil
		}
		resolved, _, err := MakeSessionID(device, harness, sid)
		return resolved, err
	}
	if !paths.ValidHarness(harness) {
		return "", newAgentErr(fmt.Sprintf("unknown harness %q", harness), 400)
	}
	h.mu.Lock()
	rows, err := h.db.Query(
		"SELECT session_id, pid, cwd FROM agent_sessions WHERE device = ? AND harness = ? AND state != 'exited' ORDER BY updated_at DESC",
		device, harness,
	)
	if err != nil {
		h.mu.Unlock()
		return "", err
	}
	type cand struct {
		sid string
		pid sql.NullInt64
		cwd string
	}
	var list []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.sid, &c.pid, &c.cwd); err != nil {
			_ = rows.Close()
			h.mu.Unlock()
			return "", err
		}
		list = append(list, c)
	}
	_ = rows.Close()
	h.mu.Unlock()

	if pid != nil {
		for _, c := range list {
			if c.pid.Valid && int(c.pid.Int64) == *pid {
				return c.sid, nil
			}
		}
	}
	if cwd != "" {
		for _, c := range list {
			if c.cwd == cwd {
				return c.sid, nil
			}
		}
	}
	if len(list) > 0 {
		return list[0].sid, nil
	}
	raw := "default"
	if pid != nil {
		raw = fmt.Sprintf("pid-%d", *pid)
	}
	res, err := h.Heartbeat(HeartbeatParams{
		Device:  device,
		Harness: harness,
		RawID:   raw,
		PID:     pid,
		CWD:     cwd,
	})
	if err != nil {
		return "", err
	}
	return res.SessionID, nil
}

// ListSessions returns active (or all) agent sessions filtered by optional criteria.
func (h *Hub) ListSessions(device, repo, harness, state string, includeStale bool) ([]Session, error) {
	if state != "" && !IsValidStateFilter(state) {
		return nil, newAgentErr(fmt.Sprintf("invalid state filter %q", state), 400)
	}
	var devArg, repoArg, harArg any
	if device != "" {
		devArg = device
	}
	if repo != "" {
		repoArg = repo
	}
	if harness != "" {
		harArg = harness
	}
	h.mu.Lock()
	rows, err := h.db.Query(
		"SELECT "+sessionSelectCols+" FROM agent_sessions "+
			"WHERE (? IS NULL OR device = ?) AND (? IS NULL OR repo = ?) AND (? IS NULL OR harness = ?) "+
			"ORDER BY updated_at DESC",
		devArg, devArg, repoArg, repoArg, harArg, harArg,
	)
	if err != nil {
		h.mu.Unlock()
		return nil, err
	}
	rawRows, err := scanAllSessionRows(rows)
	if err != nil {
		h.mu.Unlock()
		return nil, err
	}
	nowUTC := time.Now().UTC().Truncate(time.Second)
	nowMono := time.Now()
	sessions := make([]Session, 0, len(rawRows))
	for i := range rawRows {
		sessions = append(sessions, h.serializeSessionLocked(&rawRows[i], nowUTC, nowMono))
	}
	h.mu.Unlock()

	filtered := make([]Session, 0, len(sessions))
	for _, s := range sessions {
		if !includeStale && !s.Live {
			continue
		}
		if state != "" {
			if state == "waiting" {
				if !IsWaitingState(s.State) {
					continue
				}
			} else if s.State != state {
				continue
			}
		}
		filtered = append(filtered, s)
	}
	return filtered, nil
}

func (h *Hub) getSessionLocked(target string) (SessionDetail, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return SessionDetail{}, newAgentErr("session target is required", 400)
	}
	rows, err := h.db.Query("SELECT " + sessionSelectCols + " FROM agent_sessions ORDER BY updated_at DESC")
	if err != nil {
		return SessionDetail{}, err
	}
	allRows, err := scanAllSessionRows(rows)
	if err != nil {
		return SessionDetail{}, err
	}
	nowUTC := time.Now().UTC().Truncate(time.Second)
	nowMono := time.Now()

	isLive := func(r *sessionRow) bool {
		age := int(nowUTC.Sub(parseTS(r.updatedAt)).Seconds())
		if age < 0 {
			age = 0
		}
		return isLiveRow(r.state, age)
	}
	pick := func(matches []*sessionRow) (SessionDetail, bool, error) {
		if len(matches) == 0 {
			return SessionDetail{}, false, nil
		}
		var liveMatches []*sessionRow
		for _, r := range matches {
			if isLive(r) {
				liveMatches = append(liveMatches, r)
			}
		}
		pool := liveMatches
		if len(pool) == 0 {
			pool = matches
		}
		if len(pool) == 1 {
			info := h.serializeSessionLocked(pool[0], nowUTC, nowMono)
			return SessionDetail{Session: info, Tail: parseTail(pool[0].tailJSON)}, true, nil
		}
		limit := len(pool)
		if limit > 5 {
			limit = 5
		}
		cands := make([]string, 0, limit)
		for _, r := range pool[:limit] {
			if r.name != "" {
				cands = append(cands, fmt.Sprintf("%s (%s)", r.name, r.sessionID))
			} else {
				cands = append(cands, r.sessionID)
			}
		}
		return SessionDetail{}, false, newAgentErr(fmt.Sprintf("ambiguous session %q: %s", target, strings.Join(cands, ", ")), 400)
	}

	filter := func(pool []*sessionRow, pred func(*sessionRow) bool) []*sessionRow {
		var out []*sessionRow
		for _, r := range pool {
			if pred(r) {
				out = append(out, r)
			}
		}
		return out
	}

	allPtrs := make([]*sessionRow, len(allRows))
	for i := range allRows {
		allPtrs[i] = &allRows[i]
	}

	var tiers [][]*sessionRow
	tiers = append(tiers, filter(allPtrs, func(r *sessionRow) bool { return r.sessionID == target }))
	targetLower := strings.ToLower(target)

	if strings.Contains(target, "/") && !strings.Contains(target, ":") {
		devPart, keyPart, _ := strings.Cut(target, "/")
		keyLower := strings.ToLower(keyPart)
		devRows := filter(allPtrs, func(r *sessionRow) bool { return r.device == devPart })
		if keyPart != "" {
			tiers = append(tiers,
				filter(devRows, func(r *sessionRow) bool { return strings.ToLower(r.name) == keyLower }),
				filter(devRows, func(r *sessionRow) bool { return strings.ToLower(r.title) == keyLower }),
				filter(devRows, func(r *sessionRow) bool { return strings.HasSuffix(r.sessionID, ":"+keyPart) }),
				filter(devRows, func(r *sessionRow) bool {
					return strings.Contains(strings.ToLower(r.name), keyLower) ||
						strings.Contains(strings.ToLower(r.title), keyLower) ||
						strings.Contains(r.sessionID, keyPart)
				}),
			)
		}
	}

	tiers = append(tiers,
		filter(allPtrs, func(r *sessionRow) bool { return r.name != "" && strings.ToLower(r.name) == targetLower }),
		filter(allPtrs, func(r *sessionRow) bool { return r.title != "" && strings.ToLower(r.title) == targetLower }),
		filter(allPtrs, func(r *sessionRow) bool { return strings.HasSuffix(r.sessionID, ":"+target) }),
		filter(allPtrs, func(r *sessionRow) bool {
			return strings.Contains(strings.ToLower(r.name), targetLower) ||
				strings.Contains(strings.ToLower(r.title), targetLower) ||
				strings.Contains(r.sessionID, target)
		}),
	)

	for _, matches := range tiers {
		found, ok, err := pick(matches)
		if err != nil {
			return SessionDetail{}, err
		}
		if ok {
			return found, nil
		}
	}
	return SessionDetail{}, newAgentErr(fmt.Sprintf("no agent session matching %q", target), 404)
}

// GetSession resolves target (session_id, short id, name, device/name, or title) and returns session details + tail.
func (h *Hub) GetSession(target string) (SessionDetail, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.getSessionLocked(target)
}

func (h *Hub) deliveryReportLocked(msg Message) (DeliveryReport, error) {
	nowUTC := time.Now().UTC().Truncate(time.Second)
	nowMono := time.Now()
	if msg.ToSession != nil && *msg.ToSession != "" {
		sid := *msg.ToSession
		row, err := scanSessionRow(h.db.QueryRow("SELECT "+sessionSelectCols+" FROM agent_sessions WHERE session_id = ?", sid))
		if err != nil {
			return DeliveryReport{}, err
		}
		delivery := "offline"
		label := sid
		var sess *Session
		if row != nil {
			s := h.serializeSessionLocked(row, nowUTC, nowMono)
			sess = &s
			delivery = s.Delivery
			if s.Name != "" {
				label = fmt.Sprintf("%s (%s)", s.Name, s.SessionID)
			} else {
				label = s.SessionID
			}
		}
		woke := 0
		if h.waiters[sid] > 0 {
			woke = 1
		}
		var warning *string
		switch {
		case delivery == "waiting_user" && sess != nil:
			kindLabel := "user input"
			if sess.State == "waiting_permission" {
				kindLabel = "permission approval"
			}
			tool := strings.TrimSpace(sess.PendingAction["tool"])
			detail := strings.TrimSpace(sess.PendingAction["detail"])
			waitDesc := kindLabel
			if tool != "" && detail != "" {
				waitDesc = fmt.Sprintf("%s (%s: %s)", kindLabel, tool, TruncateRunes(detail, 120))
			} else if tool != "" || detail != "" {
				part := tool
				if part == "" {
					part = detail
				}
				waitDesc = fmt.Sprintf("%s (%s)", kindLabel, TruncateRunes(part, 120))
			}
			w := fmt.Sprintf("Recipient %s is waiting for %s; message saved in inbox until the user responds.", label, waitDesc)
			warning = &w
		case delivery == "queued_idle":
			w := fmt.Sprintf("Recipient %s is idle with no active listener; message queued until its next hook or user turn.", label)
			warning = &w
		case delivery == "offline":
			w := fmt.Sprintf("Recipient %s is offline/stale; message saved in inbox but will not trigger action.", label)
			warning = &w
		}
		return DeliveryReport{
			Delivery:    delivery,
			WokeWaiters: woke,
			Recipients:  nil,
			Warning:     warning,
		}, nil
	}

	rows, err := h.db.Query(
		"SELECT "+sessionSelectCols+" FROM agent_sessions WHERE session_id != ? AND state != 'exited' ORDER BY updated_at DESC",
		msg.FromSession,
	)
	if err != nil {
		return DeliveryReport{}, err
	}
	allRows, err := scanAllSessionRows(rows)
	if err != nil {
		return DeliveryReport{}, err
	}
	recips := &RecipientsMap{
		Immediate:   []string{},
		NextHook:    []string{},
		WaitingUser: []string{},
		QueuedIdle:  []string{},
	}
	for i := range allRows {
		r := &allRows[i]
		if msg.ToDevice != nil && r.device != *msg.ToDevice {
			continue
		}
		if msg.ToRepo != nil && (r.repo == "" || r.repo != *msg.ToRepo) {
			continue
		}
		s := h.serializeSessionLocked(r, nowUTC, nowMono)
		if !s.Live {
			continue
		}
		switch s.Delivery {
		case "immediate":
			recips.Immediate = append(recips.Immediate, s.SessionID)
		case "next_hook":
			recips.NextHook = append(recips.NextHook, s.SessionID)
		case "waiting_user":
			recips.WaitingUser = append(recips.WaitingUser, s.SessionID)
		case "queued_idle":
			recips.QueuedIdle = append(recips.QueuedIdle, s.SessionID)
		}
	}
	woke := len(recips.Immediate)
	var warning *string
	if len(recips.Immediate) == 0 && len(recips.NextHook) == 0 {
		var w string
		switch {
		case len(recips.WaitingUser) > 0 && len(recips.QueuedIdle) > 0:
			w = "All matching recipients are waiting for user input or idle with no active listener; message saved in inbox."
		case len(recips.WaitingUser) > 0:
			w = "All matching recipients are waiting for user permission/input; message saved in inbox until the user responds."
		case len(recips.QueuedIdle) > 0:
			w = "All matching recipients are idle with no active listener; message queued until next hook or user turn."
		default:
			w = "No live matching recipients found; message saved in inbox."
		}
		warning = &w
	}
	return DeliveryReport{
		Delivery:    "broadcast",
		WokeWaiters: woke,
		Recipients:  recips,
		Warning:     warning,
	}, nil
}

// DeliveryReport computes the delivery report for an already-stored message.
func (h *Hub) DeliveryReport(msg Message) (DeliveryReport, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.deliveryReportLocked(msg)
}

func (h *Hub) sendMessageAndReportLocked(
	device, fromSession, to, body string,
	threadID *string,
	replyTo *int64,
	knownDevices map[string]bool,
) (Message, DeliveryReport, error) {
	if !strings.HasPrefix(fromSession, device+":") {
		return Message{}, DeliveryReport{}, newAgentErr("session belongs to another device", 403)
	}
	text := strings.TrimSpace(body)
	if text == "" {
		return Message{}, DeliveryReport{}, newAgentErr("message body is required", 400)
	}
	if len(text) > MaxMessageBytes {
		return Message{}, DeliveryReport{}, newAgentErr("message too large (max 32 KB)", 413)
	}
	target := strings.TrimSpace(to)
	if target == "" {
		return Message{}, DeliveryReport{}, newAgentErr("recipient `to` is required", 400)
	}

	var toDevice, toSession, toRepo *string
	switch {
	case target == "*" || target == "all":
	case strings.HasPrefix(target, "repo:"):
		r := strings.TrimSpace(target[5:])
		if r == "" {
			return Message{}, DeliveryReport{}, newAgentErr("repo target cannot be empty", 400)
		}
		toRepo = &r
	case strings.HasPrefix(target, "device:"):
		d := strings.TrimSpace(target[7:])
		if d == "" {
			return Message{}, DeliveryReport{}, newAgentErr("device target cannot be empty", 400)
		}
		toDevice = &d
	case !strings.Contains(target, ":") && !strings.Contains(target, "/") && knownDevices[target]:
		d := target
		toDevice = &d
	default:
		resolved, err := h.getSessionLocked(target)
		if err != nil {
			return Message{}, DeliveryReport{}, err
		}
		sid := resolved.SessionID
		dev := resolved.Device
		toSession = &sid
		toDevice = &dev
	}

	resolvedThread := ""
	if threadID != nil {
		resolvedThread = *threadID
	}
	if resolvedThread == "" && replyTo != nil {
		var pt string
		if err := h.db.QueryRow("SELECT thread_id FROM agent_messages WHERE id = ?", *replyTo).Scan(&pt); err == nil {
			resolvedThread = pt
		}
	}
	if resolvedThread == "" {
		var buf [6]byte
		_, _ = rand.Read(buf[:])
		resolvedThread = hex.EncodeToString(buf[:])
	}

	nowISO := registry.NowISO()
	var replyArg, toDevArg, toSessArg, toRepoArg any
	if replyTo != nil {
		replyArg = *replyTo
	}
	if toDevice != nil {
		toDevArg = *toDevice
	}
	if toSession != nil {
		toSessArg = *toSession
	}
	if toRepo != nil {
		toRepoArg = *toRepo
	}

	res, err := h.db.Exec(
		"INSERT INTO agent_messages(thread_id, reply_to, from_session, from_device, to_device, to_session, to_repo, body, created_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		resolvedThread, replyArg, fromSession, device, toDevArg, toSessArg, toRepoArg, text, nowISO,
	)
	if err != nil {
		return Message{}, DeliveryReport{}, err
	}
	lastID, err := res.LastInsertId()
	if err != nil {
		return Message{}, DeliveryReport{}, err
	}
	msg, err := scanMessage(h.db.QueryRow(
		"SELECT m.id, m.thread_id, m.reply_to, m.from_session, m.from_device, "+
			"COALESCE(s.name, '') AS from_name, COALESCE(s.title, '') AS from_title, "+
			"m.to_device, m.to_session, m.to_repo, m.body, m.created_at "+
			"FROM agent_messages m LEFT JOIN agent_sessions s ON s.session_id = m.from_session "+
			"WHERE m.id = ?",
		lastID,
	))
	if err != nil {
		return Message{}, DeliveryReport{}, err
	}
	report, err := h.deliveryReportLocked(msg)
	if err != nil {
		return Message{}, DeliveryReport{}, err
	}

	if toSession != nil && *toSession != "" {
		h.notifySessionLocked(*toSession)
	} else {
		h.notifyBroadcastLocked(fromSession, toDevice)
	}
	h.notifyThreadLocked(resolvedThread)

	return msg, report, nil
}

// SendMessage inserts a message and wakes any matching recipients/threads.
func (h *Hub) SendMessage(device, fromSession, to, body string, threadID *string, replyTo *int64, knownDevices map[string]bool) (Message, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	msg, _, err := h.sendMessageAndReportLocked(device, fromSession, to, body, threadID, replyTo, knownDevices)
	return msg, err
}

// SendMessageWithReport sends a message and returns the message along with delivery diagnostics.
func (h *Hub) SendMessageWithReport(device, fromSession, to, body string, threadID *string, replyTo *int64, knownDevices map[string]bool) (SendResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	msg, report, err := h.sendMessageAndReportLocked(device, fromSession, to, body, threadID, replyTo, knownDevices)
	if err != nil {
		return SendResult{}, err
	}
	return SendResult{Message: msg, DeliveryReport: report}, nil
}

// AckMessages marks messageIDs as read for sessionID.
func (h *Hub) AckMessages(device, sessionID string, messageIDs []int64) (int, error) {
	if !strings.HasPrefix(sessionID, device+":") {
		return 0, newAgentErr("session belongs to another device", 403)
	}
	if len(messageIDs) == 0 {
		return 0, nil
	}
	readAt := registry.NowISO()
	h.mu.Lock()
	defer h.mu.Unlock()
	tx, err := h.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, mid := range messageIDs {
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO agent_reads(session_id, message_id, read_at) VALUES (?, ?, ?)",
			sessionID, mid, readAt,
		); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(messageIDs), nil
}

// WaitForMessages blocks until unread messages arrive for SessionID, the session exits,
// timeout elapses, ctx is cancelled, or the hub closes.
func (h *Hub) WaitForMessages(ctx context.Context, p WaitParams) (WaitResult, error) {
	if !strings.HasPrefix(p.SessionID, p.Device+":") {
		return WaitResult{}, newAgentErr("session belongs to another device", 403)
	}
	timeout := p.Timeout
	if timeout == 0 {
		timeout = 55.0
	}
	timeout = math.Max(0.05, math.Min(timeout, 60.0))
	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}
	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	currentSID := p.SessionID
	var registeredSID string
	var subSID string
	var subCh chan struct{}
	touchedOnce := false

	h.mu.Lock()
	defer func() {
		if subCh != nil {
			h.unsubscribeSessionLocked(subSID, subCh)
		}
		if registeredSID != "" {
			if c := h.waiters[registeredSID] - 1; c <= 0 {
				delete(h.waiters, registeredSID)
			} else {
				h.waiters[registeredSID] = c
			}
			h.waitSeenAt[registeredSID] = time.Now()
		}
		h.mu.Unlock()
	}()

	for {
		if h.closed {
			return WaitResult{
				SessionID: currentSID,
				Messages:  []Message{},
				TimedOut:  true,
			}, nil
		}
		for {
			next, ok := h.promotedTo[currentSID]
			if !ok {
				break
			}
			currentSID = next
		}
		if registeredSID != "" && registeredSID != currentSID {
			h.transferWaiterLocked(registeredSID, currentSID)
			registeredSID = currentSID
		}
		if subCh == nil || subSID != currentSID {
			if subCh != nil {
				h.unsubscribeSessionLocked(subSID, subCh)
			}
			subSID = currentSID
			subCh = h.subscribeSessionLocked(subSID)
			touchedOnce = false
		}

		var state, fullRawID, sessName, sessTitle string
		err := h.db.QueryRow(
			"SELECT state, full_raw_id, name, title FROM agent_sessions WHERE session_id = ?",
			currentSID,
		).Scan(&state, &fullRawID, &sessName, &sessTitle)
		if err != nil && err != sql.ErrNoRows {
			return WaitResult{}, err
		}
		exists := err == nil
		if exists && state == "exited" {
			return WaitResult{
				SessionID: currentSID,
				FullRawID: fullRawID,
				Name:      sessName,
				Title:     sessTitle,
				Messages:  []Message{},
				Exited:    true,
			}, nil
		}
		if exists && !touchedOnce {
			if _, err := h.db.Exec("UPDATE agent_sessions SET updated_at = ? WHERE session_id = ?", registry.NowISO(), currentSID); err != nil {
				return WaitResult{}, err
			}
			touchedOnce = true
		}

		ready := (!p.RequireFullID) || (fullRawID != "" && !strings.HasPrefix(fullRawID, "pid-"))
		if ready {
			if registeredSID != currentSID {
				h.waiters[currentSID]++
				registeredSID = currentSID
			}
			msgs, err := h.fetchInboxLocked(p.Device, currentSID, true, p.MarkRead, limit)
			if err != nil {
				return WaitResult{}, err
			}
			if len(msgs) > 0 {
				return WaitResult{
					SessionID: currentSID,
					FullRawID: fullRawID,
					Name:      sessName,
					Title:     sessTitle,
					Messages:  msgs,
				}, nil
			}
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return WaitResult{
				SessionID: currentSID,
				FullRawID: fullRawID,
				Name:      sessName,
				Title:     sessTitle,
				Messages:  []Message{},
				TimedOut:  true,
			}, nil
		}

		// Capture channels while locked, then release lock to wait.
		ch := subCh
		done := h.doneCh
		h.mu.Unlock()

		timer := time.NewTimer(remaining)
		var ctxErr error
		select {
		case <-ch:
		case <-timer.C:
		case <-done:
		case <-ctx.Done():
			ctxErr = ctx.Err()
		}
		timer.Stop()
		h.mu.Lock()
		if ctxErr != nil {
			return WaitResult{}, ctxErr
		}
	}
}

// Ask sends a question to To and waits for the first reply on the new thread (unless failing fast).
func (h *Hub) Ask(ctx context.Context, p AskParams) (AskResult, error) {
	timeout := p.Timeout
	if timeout == 0 {
		timeout = 30.0
	}
	timeout = math.Max(0.1, math.Min(timeout, 120.0))

	h.mu.Lock()
	sent, report, err := h.sendMessageAndReportLocked(p.Device, p.FromSession, p.To, p.Body, nil, nil, p.KnownDevices)
	if err != nil {
		h.mu.Unlock()
		return AskResult{}, err
	}
	if sent.ToSession != nil && *sent.ToSession != "" {
		if report.Delivery == "offline" || ((report.Delivery == "queued_idle" || report.Delivery == "waiting_user") && !p.WaitIfIdle) {
			h.mu.Unlock()
			return AskResult{
				Sent:      sent,
				Reply:     nil,
				TimedOut:  false,
				Delivered: false,
				Delivery:  report.Delivery,
				Warning:   report.Warning,
			}, nil
		}
	}

	subCh := h.subscribeThreadLocked(sent.ThreadID)
	defer func() {
		h.unsubscribeThreadLocked(sent.ThreadID, subCh)
		h.mu.Unlock()
	}()

	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	for {
		if h.closed {
			return AskResult{
				Sent:     sent,
				TimedOut: true,
				Delivery: report.Delivery,
				Warning:  report.Warning,
			}, nil
		}
		row := h.db.QueryRow(
			"SELECT m.id, m.thread_id, m.reply_to, m.from_session, m.from_device, "+
				"COALESCE(s.name, '') AS from_name, COALESCE(s.title, '') AS from_title, "+
				"m.to_device, m.to_session, m.to_repo, m.body, m.created_at "+
				"FROM agent_messages m LEFT JOIN agent_sessions s ON s.session_id = m.from_session "+
				"WHERE m.thread_id = ? AND m.id > ? AND m.from_session != ? "+
				"AND (m.to_session = ? OR (m.to_session IS NULL AND (m.to_device IS NULL OR m.to_device = ?))) "+
				"ORDER BY m.id ASC LIMIT 1",
			sent.ThreadID, sent.ID, p.FromSession, p.FromSession, p.Device,
		)
		reply, err := scanMessage(row)
		if err == nil {
			_, _ = h.db.Exec(
				"INSERT OR IGNORE INTO agent_reads(session_id, message_id, read_at) VALUES (?, ?, ?)",
				p.FromSession, reply.ID, registry.NowISO(),
			)
			return AskResult{
				Sent:      sent,
				Reply:     &reply,
				Delivered: true,
				Delivery:  report.Delivery,
				Warning:   report.Warning,
			}, nil
		}
		if err != sql.ErrNoRows {
			return AskResult{}, err
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return AskResult{
				Sent:     sent,
				TimedOut: true,
				Delivery: report.Delivery,
				Warning:  report.Warning,
			}, nil
		}

		done := h.doneCh
		h.mu.Unlock()
		timer := time.NewTimer(remaining)
		var ctxErr error
		select {
		case <-subCh:
		case <-timer.C:
		case <-done:
		case <-ctx.Done():
			ctxErr = ctx.Err()
		}
		timer.Stop()
		h.mu.Lock()
		if ctxErr != nil {
			return AskResult{}, ctxErr
		}
	}
}
