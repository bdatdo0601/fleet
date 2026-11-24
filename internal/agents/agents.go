// Package agents implements the SQLite agent session registry and cross-node message broker (agents.db).
package agents

import (
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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

var sessionMigrations = []registry.ColumnMigration{
	{Col: "full_raw_id", Def: "TEXT NOT NULL DEFAULT ''"},
	{Col: "state_changed_at", Def: "TEXT NOT NULL DEFAULT ''"},
	{Col: "pending_action_json", Def: "TEXT NOT NULL DEFAULT ''"},
	{Col: "title", Def: "TEXT NOT NULL DEFAULT ''"},
	{Col: "name", Def: "TEXT NOT NULL DEFAULT ''"},
	{Col: "custom_name", Def: "INTEGER NOT NULL DEFAULT 0"},
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

// HTTPStatus returns the HTTP status code associated with this error.
func (e *AgentError) HTTPStatus() int { return e.Status }

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
	Device        string            `json:"-"`
	Harness       string            `json:"harness"`
	RawID         string            `json:"raw_id"`
	PID           *int              `json:"pid"`
	CWD           string            `json:"cwd"`
	Repo          string            `json:"repo"`
	Branch        string            `json:"branch"`
	State         string            `json:"state"`
	PendingAction map[string]string `json:"pending_action,omitempty"`
	Title         *string           `json:"title"`
	Name          *string           `json:"name"`
	ExplicitName  bool              `json:"explicit_name"`
	Summary       *string           `json:"summary"`
	GitStatus     *string           `json:"git_status"`
	Tail          []TailItem        `json:"tail"`
	DrainInbox    bool              `json:"drain_inbox"`
	IncludeMemory bool              `json:"include_memory,omitempty"`
	MemoryRepo    string            `json:"memory_repo,omitempty"`
}

// HeartbeatResult is returned by Heartbeat.
type HeartbeatResult struct {
	SessionID     string    `json:"session_id"`
	FullRawID     string    `json:"full_raw_id"`
	Name          string    `json:"name"`
	Title         string    `json:"title"`
	Unread        []Message `json:"unread"`
	MemoryContext string    `json:"memory_context,omitempty"`
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
	Sent      Message  `json:"sent"`
	Reply     *Message `json:"reply"`
	TimedOut  bool     `json:"timed_out"`
	Delivered bool     `json:"delivered"`
	Delivery  string   `json:"delivery"`
	Warning   *string  `json:"warning"`
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
	pid               *int
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
	if errors.Is(err, sql.ErrNoRows) {
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
	age := max(int(now.Sub(updated).Seconds()), 0)
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
		ws := max(int(now.Sub(parseTS(stateChangedAt)).Seconds()), 0)
		waitingSecs = &ws
	}
	isListening := listening && live && r.state != "exited"
	return Session{
		SessionID:      r.sessionID,
		Device:         r.device,
		Harness:        r.harness,
		FullRawID:      r.fullRawID,
		PID:            r.pid,
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

type subMap map[string]map[chan struct{}]struct{}

func (m subMap) subscribe(key string) chan struct{} {
	ch := make(chan struct{}, 1)
	set := m[key]
	if set == nil {
		set = map[chan struct{}]struct{}{}
		m[key] = set
	}
	set[ch] = struct{}{}
	return ch
}

func (m subMap) unsubscribe(key string, ch chan struct{}) {
	if set := m[key]; set != nil {
		delete(set, ch)
		if len(set) == 0 {
			delete(m, key)
		}
	}
}

func (m subMap) notify(key string) {
	for ch := range m[key] {
		select {
		case ch <- struct{}{}:
		default:
		}
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
	sessionSubs subMap
	threadSubs  subMap
	closed      bool
	doneCh      chan struct{}
}

// Open opens or initializes agents.db at path, creating its parent directory and running migrations.
func Open(path string) (*Hub, error) {
	db, err := registry.OpenSQLite(path, schema, "agent_sessions", sessionMigrations)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return &Hub{
		Path:        path,
		db:          db,
		waiters:     map[string]int{},
		waitSeenAt:  map[string]time.Time{},
		promotedTo:  map[string]string{},
		sessionSubs: subMap{},
		threadSubs:  subMap{},
		doneCh:      make(chan struct{}),
	}, nil
}

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
		h.sessionSubs.notify(sid)
	}
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
	h.sessionSubs.notify(oldID)
}

func (h *Hub) migrateSessionIdentityLocked(execer interface {
	Exec(string, ...any) (sql.Result, error)
}, oldID, targetID string,
) error {
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
	age := max(int(nowUTC.Sub(updated).Seconds()), 0)
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
	defer func() { _ = rows.Close() }()
	taken := map[string]bool{}
	for rows.Next() {
		var lname string
		if err := rows.Scan(&lname); err != nil {
			return "", err
		}
		taken[lname] = true
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	if !taken[strings.ToLower(base)] {
		return base, nil
	}
	for n := 2; ; n++ {
		suffix := fmt.Sprintf("-%d", n)
		prefixMax := max(32-len(suffix), 1)
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

type placeholderRow struct {
	sid        string
	pid        *int
	startedAt  string
	title      string
	name       string
	customName int
}

func loadPlaceholdersLocked(tx *sql.Tx, device, harness, cwd string) ([]placeholderRow, error) {
	cutoff := time.Now().UTC().Truncate(time.Second).Add(-time.Duration(paths.AgentActiveSeconds) * time.Second).Format("2006-01-02T15:04:05+00:00")
	rows, err := tx.Query(
		"SELECT session_id, pid, started_at, title, name, custom_name FROM agent_sessions "+
			"WHERE device = ? AND harness = ? AND cwd = ? AND raw_id LIKE 'pid-%' "+
			"AND state != 'exited' AND updated_at >= ? ORDER BY updated_at DESC",
		device, harness, cwd, cutoff,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var cands []placeholderRow
	for rows.Next() {
		var r placeholderRow
		if err := rows.Scan(&r.sid, &r.pid, &r.startedAt, &r.title, &r.name, &r.customName); err != nil {
			return nil, err
		}
		cands = append(cands, r)
	}
	return cands, rows.Err()
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
	cands, err := loadPlaceholdersLocked(tx, device, harness, cwd)
	if err != nil || len(cands) == 0 {
		return pid, "", "", "", 0, err
	}
	chosen := cands[0]
	if pid != nil {
		for _, c := range cands {
			if c.pid != nil && *c.pid == *pid {
				chosen = c
				break
			}
		}
	}
	if err := h.migrateSessionIdentityLocked(tx, chosen.sid, targetID); err != nil {
		return pid, "", "", "", 0, err
	}
	outPID := pid
	if outPID == nil {
		outPID = chosen.pid
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

const msgSelectSQL = "SELECT m.id, m.thread_id, m.reply_to, m.from_session, m.from_device, " +
	"COALESCE(s.name, '') AS from_name, COALESCE(s.title, '') AS from_title, " +
	"m.to_device, m.to_session, m.to_repo, m.body, m.created_at " +
	"FROM agent_messages m LEFT JOIN agent_sessions s ON s.session_id = m.from_session "

func (h *Hub) markReadLocked(sessionID string, ids ...int64) error {
	if len(ids) == 0 {
		return nil
	}
	readAt := registry.NowISO()
	tx, err := h.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, mid := range ids {
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO agent_reads(session_id, message_id, read_at) VALUES (?, ?, ?)",
			sessionID, mid, readAt,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (h *Hub) fetchInboxLocked(device, sessionID string, unreadOnly, markRead bool, limit int) ([]Message, error) {
	if !strings.HasPrefix(sessionID, device+":") {
		return nil, newAgentErr("session belongs to another device", 403)
	}
	limit = max(1, min(limit, 200))
	var repo, startedAt string
	err := h.db.QueryRow("SELECT repo, started_at FROM agent_sessions WHERE session_id = ?", sessionID).Scan(&repo, &startedAt)
	if errors.Is(err, sql.ErrNoRows) {
		repo = ""
		startedAt = "1970-01-01T00:00:00+00:00"
	} else if err != nil {
		return nil, err
	}
	unreadFlag := 0
	if unreadOnly {
		unreadFlag = 1
	}
	messages, err := h.queryMessagesLocked(
		msgSelectSQL+"WHERE m.from_session != ? "+
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
	if markRead && len(messages) > 0 {
		ids := make([]int64, len(messages))
		for i, m := range messages {
			ids[i] = m.ID
		}
		if err := h.markReadLocked(sessionID, ids...); err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func (h *Hub) queryMessagesLocked(query string, args ...any) ([]Message, error) {
	rows, err := h.db.Query(query, args...)
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
	return messages, rows.Err()
}

func scanMessage(scanner interface{ Scan(...any) error }) (Message, error) {
	var m Message
	err := scanner.Scan(
		&m.ID, &m.ThreadID, &m.ReplyTo, &m.FromSession, &m.FromDevice,
		&m.FromName, &m.FromTitle,
		&m.ToDevice, &m.ToSession, &m.ToRepo, &m.Body, &m.CreatedAt,
	)
	if err != nil {
		return Message{}, err
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

func resolvePendingAction(state string, incoming map[string]string, existing *sessionRow) (string, error) {
	if !IsWaitingState(state) {
		return "", nil
	}
	norm := normalizePendingAction(incoming)
	sameState := existing != nil && existing.state == state
	var existingPending map[string]string
	if sameState {
		existingPending = parsePendingAction(existing.pendingActionJSON)
	}
	if norm == nil || (sameState && existingPending != nil && existingPending["tool"] != "" && norm["tool"] == "") {
		if sameState {
			return existing.pendingActionJSON, nil
		}
		return "", nil
	}
	b, err := json.Marshal(norm)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func resolveStateChangedAt(pState, newPendingJSON, nowISO string, existing *sessionRow) string {
	if existing != nil && existing.state == pState && (!IsWaitingState(pState) || newPendingJSON == existing.pendingActionJSON) {
		return cmp.Or(existing.stateChangedAt, existing.updatedAt)
	}
	return nowISO
}

func baseIdentity(existing *sessionRow, oldTitle, oldName string, oldCustom int) (string, string, int) {
	baseTitle := oldTitle
	if existing != nil && existing.title != "" {
		baseTitle = existing.title
	}
	switch {
	case existing != nil && existing.customName != 0:
		return baseTitle, existing.name, 1
	case oldCustom != 0:
		return baseTitle, oldName, 1
	case existing != nil && existing.name != "":
		return baseTitle, existing.name, 0
	default:
		return baseTitle, oldName, 0
	}
}

func optionalTruncated(ptr *string, fallback string, maxRunes int) string {
	if ptr == nil {
		return fallback
	}
	return TruncateRunes(*ptr, maxRunes)
}

func (h *Hub) resolvePromotedLocked(sid string) string {
	for {
		next, ok := h.promotedTo[sid]
		if !ok {
			return sid
		}
		sid = next
	}
}

func (h *Hub) finishHeartbeatLocked(device, sessionID, fullRaw, name, title string, drain bool) (HeartbeatResult, error) {
	h.sessionSubs.notify(sessionID)
	unread := []Message{}
	if drain {
		var err error
		if unread, err = h.fetchInboxLocked(device, sessionID, true, true, 50); err != nil {
			return HeartbeatResult{}, err
		}
	}
	return HeartbeatResult{SessionID: sessionID, FullRawID: fullRaw, Name: name, Title: title, Unread: unread}, nil
}

func (h *Hub) heartbeatPromotedLocked(p HeartbeatParams, sessionID string, promoted *sessionRow, nowISO string) (HeartbeatResult, error) {
	targetID := promoted.sessionID
	effRepo := cmp.Or(p.Repo, promoted.repo)
	effTitle, effName, effCustom, err := h.resolveTitleAndNameLocked(
		h.db, p.Device, targetID, effRepo,
		p.Title, p.Name, p.ExplicitName,
		promoted.title, promoted.name, promoted.customName,
	)
	if err != nil {
		return HeartbeatResult{}, err
	}
	effTailJSON := promoted.tailJSON
	if p.Tail != nil {
		if effTailJSON, err = marshalTailSlice(p.Tail); err != nil {
			return HeartbeatResult{}, err
		}
	}
	tx, err := h.db.Begin()
	if err != nil {
		return HeartbeatResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := h.migrateSessionIdentityLocked(tx, sessionID, targetID); err != nil {
		return HeartbeatResult{}, err
	}
	newPID := promoted.pid
	if newPID == nil {
		newPID = p.PID
	}
	if _, err := tx.Exec(
		"UPDATE agent_sessions SET pid = ?, cwd = ?, repo = ?, branch = ?, "+
			"title = ?, name = ?, custom_name = ?, summary = ?, git_status = ?, "+
			"tail_json = ?, updated_at = ? WHERE session_id = ?",
		newPID, cmp.Or(p.CWD, promoted.cwd), effRepo, cmp.Or(p.Branch, promoted.branch),
		effTitle, effName, effCustom, optionalTruncated(p.Summary, promoted.summary, 160),
		optionalTruncated(p.GitStatus, promoted.gitStatus, 1500), effTailJSON, nowISO, targetID,
	); err != nil {
		return HeartbeatResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return HeartbeatResult{}, err
	}
	return h.finishHeartbeatLocked(p.Device, targetID, promoted.fullRawID, effName, effTitle, p.DrainInbox)
}

func upsertSessionTxLocked(
	tx *sql.Tx, p HeartbeatParams, existing *sessionRow,
	sessionID, shortID, fullRaw, effRepo, effTitle, effName string, effCustom int,
	newPendingJSON, newStateChangedAt, oldStarted, nowISO string, oldPID *int,
) error {
	if existing != nil {
		pidVal := oldPID
		if pidVal == nil {
			pidVal = existing.pid
		}
		effTail := existing.tailJSON
		if p.Tail != nil {
			var err error
			if effTail, err = marshalTailSlice(p.Tail); err != nil {
				return err
			}
		}
		_, err := tx.Exec(
			"UPDATE agent_sessions SET full_raw_id = ?, pid = ?, cwd = ?, repo = ?, branch = ?, "+
				"state = ?, state_changed_at = ?, pending_action_json = ?, title = ?, name = ?, "+
				"custom_name = ?, summary = ?, git_status = ?, tail_json = ?, started_at = ?, "+
				"updated_at = ? WHERE session_id = ?",
			cmp.Or(fullRaw, existing.fullRawID), pidVal, cmp.Or(p.CWD, existing.cwd), effRepo, cmp.Or(p.Branch, existing.branch),
			p.State, newStateChangedAt, newPendingJSON, effTitle, effName, effCustom,
			optionalTruncated(p.Summary, existing.summary, 160),
			optionalTruncated(p.GitStatus, existing.gitStatus, 1500),
			effTail, cmp.Or(oldStarted, existing.startedAt), nowISO, sessionID,
		)
		return err
	}
	tailJSON, err := marshalTailSlice(p.Tail)
	if err != nil {
		return err
	}
	_, err = tx.Exec(
		"INSERT INTO agent_sessions(session_id, device, harness, raw_id, full_raw_id, pid, cwd, repo, "+
			"branch, state, state_changed_at, pending_action_json, title, name, custom_name, summary, "+
			"git_status, tail_json, started_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		sessionID, p.Device, p.Harness, shortID, fullRaw, oldPID, p.CWD, p.Repo,
		p.Branch, p.State, newStateChangedAt, newPendingJSON, effTitle, effName, effCustom,
		optionalTruncated(p.Summary, "", 160), optionalTruncated(p.GitStatus, "", 1500),
		tailJSON, cmp.Or(oldStarted, nowISO), nowISO,
	)
	return err
}

// Heartbeat upserts session state and metadata, optionally draining unread inbox messages.
func (h *Hub) Heartbeat(p HeartbeatParams) (HeartbeatResult, error) {
	p.State = cmp.Or(p.State, "working")
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

	isPlaceholder := strings.HasPrefix(shortID, "pid-")
	if isPlaceholder && p.PID != nil {
		promoted, err := h.findPromotedSessionLocked(p.Device, p.Harness, *p.PID, p.CWD)
		if err != nil {
			return HeartbeatResult{}, err
		}
		if promoted != nil {
			return h.heartbeatPromotedLocked(p, sessionID, promoted, nowISO)
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
	if !isPlaceholder {
		oldPID, oldStarted, oldTitle, oldName, oldCustom, err = h.promotePlaceholderLocked(tx, p.Device, p.Harness, p.CWD, p.PID, sessionID)
		if err != nil {
			return HeartbeatResult{}, err
		}
	}
	existing, err := scanSessionRow(tx.QueryRow("SELECT "+sessionSelectCols+" FROM agent_sessions WHERE session_id = ?", sessionID))
	if err != nil {
		return HeartbeatResult{}, err
	}
	baseTitle, baseName, baseCustom := baseIdentity(existing, oldTitle, oldName, oldCustom)
	effRepo := p.Repo
	if effRepo == "" && existing != nil {
		effRepo = existing.repo
	}
	effTitle, effName, effCustom, err := h.resolveTitleAndNameLocked(tx, p.Device, sessionID, effRepo, p.Title, p.Name, p.ExplicitName, baseTitle, baseName, baseCustom)
	if err != nil {
		return HeartbeatResult{}, err
	}
	fullRaw := rawClean
	if isPlaceholder {
		fullRaw = ""
		if existing != nil {
			fullRaw = existing.fullRawID
		}
	}
	newPendingJSON, err := resolvePendingAction(p.State, p.PendingAction, existing)
	if err != nil {
		return HeartbeatResult{}, err
	}
	newStateChangedAt := resolveStateChangedAt(p.State, newPendingJSON, nowISO, existing)
	if err := upsertSessionTxLocked(tx, p, existing, sessionID, shortID, fullRaw, effRepo, effTitle, effName, effCustom, newPendingJSON, newStateChangedAt, oldStarted, nowISO, oldPID); err != nil {
		return HeartbeatResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return HeartbeatResult{}, err
	}
	return h.finishHeartbeatLocked(p.Device, sessionID, fullRaw, effName, effTitle, p.DrainInbox)
}

func validateSessionName(newName string) (string, error) {
	raw := strings.ToLower(strings.TrimSpace(newName))
	if strings.HasPrefix(raw, "repo:") || strings.HasPrefix(raw, "device:") {
		return "", newAgentErr("invalid session name", 400)
	}
	slug := SlugifyName(newName, 6, 40)
	if slug == "" || slug == "all" {
		return "", newAgentErr("session name must contain letters or digits and cannot be reserved", 400)
	}
	return slug, nil
}

func (h *Hub) renameSessionRow(sessionID, newName, deviceFilter string) (string, error) {
	slug, err := validateSessionName(newName)
	if err != nil {
		return "", err
	}
	nowISO := registry.NowISO()
	h.mu.Lock()
	defer h.mu.Unlock()
	var res sql.Result
	if deviceFilter != "" {
		res, err = h.db.Exec(
			"UPDATE agent_sessions SET name = ?, custom_name = 1, updated_at = ? WHERE device = ? AND session_id = ?",
			slug, nowISO, deviceFilter, sessionID,
		)
	} else {
		res, err = h.db.Exec(
			"UPDATE agent_sessions SET name = ?, custom_name = 1, updated_at = ? WHERE session_id = ?",
			slug, nowISO, sessionID,
		)
	}
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

// RenameSession sets an explicit custom friendly handle on a session belonging to device.
func (h *Hub) RenameSession(device, sessionID, newName string) (string, error) {
	if !strings.HasPrefix(sessionID, device+":") {
		return "", newAgentErr("session belongs to another device", 403)
	}
	return h.renameSessionRow(sessionID, newName, device)
}

// RenameSessionAny sets an explicit custom friendly handle on any session (for admin callers).
func (h *Hub) RenameSessionAny(sessionID, newName string) (string, error) {
	return h.renameSessionRow(sessionID, newName, "")
}

func (h *Hub) collectStringsLocked(query string, args ...any) ([]string, error) {
	rows, err := h.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
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
		h.sessionSubs.notify(*sessionID)
		return nil
	}
	if harness != nil && *harness != "" && pid != nil {
		sids, _ := h.collectStringsLocked(
			"SELECT session_id FROM agent_sessions WHERE device = ? AND harness = ? AND pid = ?",
			device, *harness, *pid,
		)
		if _, err := h.db.Exec(
			"UPDATE agent_sessions SET state = 'exited', state_changed_at = ?, pending_action_json = '', updated_at = ? WHERE device = ? AND harness = ? AND pid = ?",
			nowISO, nowISO, device, *harness, *pid,
		); err != nil {
			return err
		}
		for _, s := range sids {
			h.sessionSubs.notify(s)
		}
		return nil
	}
	return newAgentErr("session_id or (harness, pid) is required", 400)
}

// DeregisterAny marks any session as exited and wakes waiters (for admin callers).
func (h *Hub) DeregisterAny(sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return newAgentErr("session_id is required", 400)
	}
	nowISO := registry.NowISO()
	h.mu.Lock()
	defer h.mu.Unlock()
	res, err := h.db.Exec(
		"UPDATE agent_sessions SET state = 'exited', state_changed_at = ?, pending_action_json = '', updated_at = ? WHERE session_id = ?",
		nowISO, nowISO, sessionID,
	)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return newAgentErr(fmt.Sprintf("no agent session %q", sessionID), 404)
	}
	h.sessionSubs.notify(sessionID)
	return nil
}

type callerCand struct {
	sid string
	pid *int
	cwd string
}

func (h *Hub) listCallerCandidates(device, harness string) ([]callerCand, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rows, err := h.db.Query(
		"SELECT session_id, pid, cwd FROM agent_sessions WHERE device = ? AND harness = ? AND state != 'exited' ORDER BY updated_at DESC",
		device, harness,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var list []callerCand
	for rows.Next() {
		var c callerCand
		if err := rows.Scan(&c.sid, &c.pid, &c.cwd); err != nil {
			return nil, err
		}
		list = append(list, c)
	}
	return list, rows.Err()
}

// ResolveCallerSession resolves a caller's session_id from explicit ID, pid, cwd, or creates a placeholder.
func (h *Hub) ResolveCallerSession(device, harness string, sessionID *string, cwd string, pid *int) (string, error) {
	harness = cmp.Or(harness, "claude")
	if sessionID != nil && *sessionID != "" {
		sid := *sessionID
		if !strings.Contains(sid, ":") {
			resolved, _, err := MakeSessionID(device, harness, sid)
			return resolved, err
		}
		if !strings.HasPrefix(sid, device+":") {
			return "", newAgentErr("session belongs to another device", 403)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.resolvePromotedLocked(sid), nil
	}
	if !paths.ValidHarness(harness) {
		return "", newAgentErr(fmt.Sprintf("unknown harness %q", harness), 400)
	}
	list, err := h.listCallerCandidates(device, harness)
	if err != nil {
		return "", err
	}
	if pid != nil {
		for _, c := range list {
			if c.pid != nil && *c.pid == *pid {
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
	res, err := h.Heartbeat(HeartbeatParams{Device: device, Harness: harness, RawID: raw, PID: pid, CWD: cwd})
	return res.SessionID, err
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

func filterSessionRows(pool []*sessionRow, pred func(*sessionRow) bool) []*sessionRow {
	var out []*sessionRow
	for _, r := range pool {
		if pred(r) {
			out = append(out, r)
		}
	}
	return out
}

func appendKeyTiers(tiers [][]*sessionRow, pool []*sessionRow, key string, requireNonEmpty bool) [][]*sessionRow {
	keyLower := strings.ToLower(key)
	return append(tiers,
		filterSessionRows(pool, func(r *sessionRow) bool {
			return (!requireNonEmpty || r.name != "") && strings.ToLower(r.name) == keyLower
		}),
		filterSessionRows(pool, func(r *sessionRow) bool {
			return (!requireNonEmpty || r.title != "") && strings.ToLower(r.title) == keyLower
		}),
		filterSessionRows(pool, func(r *sessionRow) bool { return strings.HasSuffix(r.sessionID, ":"+key) }),
		filterSessionRows(pool, func(r *sessionRow) bool {
			return strings.Contains(strings.ToLower(r.name), keyLower) ||
				strings.Contains(strings.ToLower(r.title), keyLower) ||
				strings.Contains(r.sessionID, key)
		}),
	)
}

func (h *Hub) pickSessionCandidateLocked(target string, matches []*sessionRow, nowUTC, nowMono time.Time) (SessionDetail, bool, error) {
	if len(matches) == 0 {
		return SessionDetail{}, false, nil
	}
	pool := filterSessionRows(matches, func(r *sessionRow) bool {
		return isLiveRow(r.state, max(int(nowUTC.Sub(parseTS(r.updatedAt)).Seconds()), 0))
	})
	if len(pool) == 0 {
		pool = matches
	}
	if len(pool) == 1 {
		info := h.serializeSessionLocked(pool[0], nowUTC, nowMono)
		return SessionDetail{Session: info, Tail: parseTail(pool[0].tailJSON)}, true, nil
	}
	limit := min(len(pool), 5)
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
	allPtrs := make([]*sessionRow, len(allRows))
	for i := range allRows {
		allPtrs[i] = &allRows[i]
	}

	tiers := [][]*sessionRow{filterSessionRows(allPtrs, func(r *sessionRow) bool { return r.sessionID == target })}
	if devPart, keyPart, ok := strings.Cut(target, "/"); ok && !strings.Contains(target, ":") && keyPart != "" {
		devRows := filterSessionRows(allPtrs, func(r *sessionRow) bool { return r.device == devPart })
		tiers = appendKeyTiers(tiers, devRows, keyPart, false)
	}
	tiers = appendKeyTiers(tiers, allPtrs, target, true)

	nowUTC := time.Now().UTC().Truncate(time.Second)
	nowMono := time.Now()
	for _, matches := range tiers {
		found, ok, err := h.pickSessionCandidateLocked(target, matches, nowUTC, nowMono)
		if err != nil || ok {
			return found, err
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

func waitingActionDesc(sess *Session) string {
	kindLabel := "user input"
	if sess.State == "waiting_permission" {
		kindLabel = "permission approval"
	}
	tool := strings.TrimSpace(sess.PendingAction["tool"])
	detail := strings.TrimSpace(sess.PendingAction["detail"])
	if tool != "" && detail != "" {
		return fmt.Sprintf("%s (%s: %s)", kindLabel, tool, TruncateRunes(detail, 120))
	}
	if part := cmp.Or(tool, detail); part != "" {
		return fmt.Sprintf("%s (%s)", kindLabel, TruncateRunes(part, 120))
	}
	return kindLabel
}

func (h *Hub) directDeliveryReportLocked(sid string, nowUTC, nowMono time.Time) (DeliveryReport, error) {
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
		warning = new(fmt.Sprintf("Recipient %s is waiting for %s; message saved in inbox until the user responds.", label, waitingActionDesc(sess)))
	case delivery == "queued_idle":
		warning = new(fmt.Sprintf("Recipient %s is idle with no active listener; message queued until its next hook or user turn.", label))
	case delivery == "offline":
		warning = new(fmt.Sprintf("Recipient %s is offline/stale; message saved in inbox but will not trigger action.", label))
	}
	return DeliveryReport{Delivery: delivery, WokeWaiters: woke, Warning: warning}, nil
}

func broadcastWarning(recips *RecipientsMap) *string {
	if len(recips.Immediate) > 0 || len(recips.NextHook) > 0 {
		return nil
	}
	switch {
	case len(recips.WaitingUser) > 0 && len(recips.QueuedIdle) > 0:
		return new("All matching recipients are waiting for user input or idle with no active listener; message saved in inbox.")
	case len(recips.WaitingUser) > 0:
		return new("All matching recipients are waiting for user permission/input; message saved in inbox until the user responds.")
	case len(recips.QueuedIdle) > 0:
		return new("All matching recipients are idle with no active listener; message queued until next hook or user turn.")
	default:
		return new("No live matching recipients found; message saved in inbox.")
	}
}

func (h *Hub) deliveryReportLocked(msg Message) (DeliveryReport, error) {
	nowUTC := time.Now().UTC().Truncate(time.Second)
	nowMono := time.Now()
	if msg.ToSession != nil && *msg.ToSession != "" {
		return h.directDeliveryReportLocked(*msg.ToSession, nowUTC, nowMono)
	}
	rows, err := h.db.Query("SELECT "+sessionSelectCols+" FROM agent_sessions WHERE session_id != ? AND state != 'exited' ORDER BY updated_at DESC", msg.FromSession)
	if err != nil {
		return DeliveryReport{}, err
	}
	allRows, err := scanAllSessionRows(rows)
	if err != nil {
		return DeliveryReport{}, err
	}
	recips := &RecipientsMap{Immediate: []string{}, NextHook: []string{}, WaitingUser: []string{}, QueuedIdle: []string{}}
	for i := range allRows {
		r := &allRows[i]
		if (msg.ToDevice != nil && r.device != *msg.ToDevice) || (msg.ToRepo != nil && (r.repo == "" || r.repo != *msg.ToRepo)) {
			continue
		}
		if s := h.serializeSessionLocked(r, nowUTC, nowMono); s.Live {
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
	}
	return DeliveryReport{
		Delivery:    "broadcast",
		WokeWaiters: len(recips.Immediate),
		Recipients:  recips,
		Warning:     broadcastWarning(recips),
	}, nil
}

func (h *Hub) resolveTargetLocked(target string, knownDevices map[string]bool) (toDevice, toSession, toRepo *string, err error) {
	switch {
	case target == "*" || target == "all":
		return nil, nil, nil, nil
	case strings.HasPrefix(target, "repo:"):
		r := strings.TrimSpace(target[5:])
		if r == "" {
			return nil, nil, nil, newAgentErr("repo target cannot be empty", 400)
		}
		return nil, nil, &r, nil
	case strings.HasPrefix(target, "device:"):
		d := strings.TrimSpace(target[7:])
		if d == "" {
			return nil, nil, nil, newAgentErr("device target cannot be empty", 400)
		}
		return &d, nil, nil, nil
	case !strings.Contains(target, ":") && !strings.Contains(target, "/") && knownDevices[target]:
		d := target
		return &d, nil, nil, nil
	default:
		resolved, err := h.getSessionLocked(target)
		if err != nil {
			return nil, nil, nil, err
		}
		return &resolved.Device, &resolved.SessionID, nil, nil
	}
}

func (h *Hub) resolveThreadIDLocked(threadID *string, replyTo *int64) string {
	if threadID != nil && *threadID != "" {
		return *threadID
	}
	if replyTo != nil {
		var pt string
		if h.db.QueryRow("SELECT thread_id FROM agent_messages WHERE id = ?", *replyTo).Scan(&pt) == nil && pt != "" {
			return pt
		}
	}
	var buf [6]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

func (h *Hub) sendMessageAndReportLocked(device, fromSession, to, body string, threadID *string, replyTo *int64, knownDevices map[string]bool) (Message, DeliveryReport, error) {
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
	toDevice, toSession, toRepo, err := h.resolveTargetLocked(target, knownDevices)
	if err != nil {
		return Message{}, DeliveryReport{}, err
	}
	resolvedThread := h.resolveThreadIDLocked(threadID, replyTo)
	res, err := h.db.Exec(
		"INSERT INTO agent_messages(thread_id, reply_to, from_session, from_device, to_device, to_session, to_repo, body, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		resolvedThread, replyTo, fromSession, device, toDevice, toSession, toRepo, text, registry.NowISO(),
	)
	if err != nil {
		return Message{}, DeliveryReport{}, err
	}
	lastID, err := res.LastInsertId()
	if err != nil {
		return Message{}, DeliveryReport{}, err
	}
	msg, err := scanMessage(h.db.QueryRow(msgSelectSQL+"WHERE m.id = ?", lastID))
	if err != nil {
		return Message{}, DeliveryReport{}, err
	}
	report, err := h.deliveryReportLocked(msg)
	if err != nil {
		return Message{}, DeliveryReport{}, err
	}
	if toSession != nil && *toSession != "" {
		h.sessionSubs.notify(*toSession)
	} else {
		h.notifyBroadcastLocked(fromSession, toDevice)
	}
	h.threadSubs.notify(resolvedThread)
	return msg, report, nil
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

// ListMessages returns recent peer messages across the fleet, optionally filtered by session, thread, or device.
func (h *Hub) ListMessages(session, thread, device string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 50
	}
	limit = max(1, min(limit, 200))

	var clauses []string
	var args []any
	if s := strings.TrimSpace(session); s != "" {
		clauses = append(clauses, "(m.from_session = ? OR m.to_session = ?)")
		args = append(args, s, s)
	}
	if t := strings.TrimSpace(thread); t != "" {
		clauses = append(clauses, "m.thread_id = ?")
		args = append(args, t)
	}
	if d := strings.TrimSpace(device); d != "" {
		clauses = append(clauses, "(m.from_device = ? OR m.to_device = ?)")
		args = append(args, d, d)
	}
	q := msgSelectSQL
	if len(clauses) > 0 {
		q += "WHERE " + strings.Join(clauses, " AND ") + " "
	}
	q += "ORDER BY m.id DESC LIMIT ?"
	args = append(args, limit)

	h.mu.Lock()
	defer h.mu.Unlock()
	return h.queryMessagesLocked(q, args...)
}

// AckMessages marks messageIDs as read for sessionID.
func (h *Hub) AckMessages(device, sessionID string, messageIDs []int64) (int, error) {
	if !strings.HasPrefix(sessionID, device+":") {
		return 0, newAgentErr("session belongs to another device", 403)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.markReadLocked(sessionID, messageIDs...); err != nil {
		return 0, err
	}
	return len(messageIDs), nil
}

func (h *Hub) waitOnChannelLocked(ctx context.Context, ch <-chan struct{}, remaining time.Duration) error {
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
	return ctxErr
}

func (h *Hub) waiterSessionMetaLocked(sid string) (state, fullRawID, name, title string, exists bool, err error) {
	err = h.db.QueryRow(
		"SELECT state, full_raw_id, name, title FROM agent_sessions WHERE session_id = ?", sid,
	).Scan(&state, &fullRawID, &name, &title)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", "", false, nil
	}
	return state, fullRawID, name, title, err == nil, err
}

func (h *Hub) unregisterWaiterLocked(subSID string, subCh chan struct{}, registeredSID string) {
	if subCh != nil {
		h.sessionSubs.unsubscribe(subSID, subCh)
	}
	if registeredSID != "" {
		if c := h.waiters[registeredSID] - 1; c <= 0 {
			delete(h.waiters, registeredSID)
		} else {
			h.waiters[registeredSID] = c
		}
		h.waitSeenAt[registeredSID] = time.Now()
	}
}

func (h *Hub) syncWaiterSubscriptionLocked(currentSID string, registeredSID, subSID *string, subCh *chan struct{}, touchedOnce *bool) string {
	nextSID := h.resolvePromotedLocked(currentSID)
	if *registeredSID != "" && *registeredSID != nextSID {
		h.transferWaiterLocked(*registeredSID, nextSID)
		*registeredSID = nextSID
	}
	if *subCh == nil || *subSID != nextSID {
		if *subCh != nil {
			h.sessionSubs.unsubscribe(*subSID, *subCh)
		}
		*subSID = nextSID
		*subCh = h.sessionSubs.subscribe(nextSID)
		*touchedOnce = false
	}
	return nextSID
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
	var registeredSID, subSID string
	var subCh chan struct{}
	touchedOnce := false

	h.mu.Lock()
	defer func() {
		h.unregisterWaiterLocked(subSID, subCh, registeredSID)
		h.mu.Unlock()
	}()

	for {
		if h.closed {
			return WaitResult{SessionID: currentSID, Messages: []Message{}, TimedOut: true}, nil
		}
		currentSID = h.syncWaiterSubscriptionLocked(currentSID, &registeredSID, &subSID, &subCh, &touchedOnce)

		state, fullRawID, sessName, sessTitle, exists, err := h.waiterSessionMetaLocked(currentSID)
		if err != nil {
			return WaitResult{}, err
		}
		if exists && state == "exited" {
			return WaitResult{SessionID: currentSID, FullRawID: fullRawID, Name: sessName, Title: sessTitle, Messages: []Message{}, Exited: true}, nil
		}
		if exists && !touchedOnce {
			if _, err := h.db.Exec("UPDATE agent_sessions SET updated_at = ? WHERE session_id = ?", registry.NowISO(), currentSID); err != nil {
				return WaitResult{}, err
			}
			touchedOnce = true
		}
		if !p.RequireFullID || (fullRawID != "" && !strings.HasPrefix(fullRawID, "pid-")) {
			if registeredSID != currentSID {
				h.waiters[currentSID]++
				registeredSID = currentSID
			}
			msgs, err := h.fetchInboxLocked(p.Device, currentSID, true, p.MarkRead, limit)
			if err != nil || len(msgs) > 0 {
				return WaitResult{SessionID: currentSID, FullRawID: fullRawID, Name: sessName, Title: sessTitle, Messages: msgs}, err
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return WaitResult{SessionID: currentSID, FullRawID: fullRawID, Name: sessName, Title: sessTitle, Messages: []Message{}, TimedOut: true}, nil
		}
		if err := h.waitOnChannelLocked(ctx, subCh, remaining); err != nil {
			return WaitResult{}, err
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
	if sent.ToSession != nil && *sent.ToSession != "" &&
		(report.Delivery == "offline" || ((report.Delivery == "queued_idle" || report.Delivery == "waiting_user") && !p.WaitIfIdle)) {
		h.mu.Unlock()
		return AskResult{Sent: sent, Delivery: report.Delivery, Warning: report.Warning}, nil
	}

	subCh := h.threadSubs.subscribe(sent.ThreadID)
	defer func() {
		h.threadSubs.unsubscribe(sent.ThreadID, subCh)
		h.mu.Unlock()
	}()

	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	for {
		if h.closed {
			return AskResult{Sent: sent, TimedOut: true, Delivery: report.Delivery, Warning: report.Warning}, nil
		}
		reply, err := scanMessage(h.db.QueryRow(
			msgSelectSQL+"WHERE m.thread_id = ? AND m.id > ? AND m.from_session != ? "+
				"AND (m.to_session = ? OR (m.to_session IS NULL AND (m.to_device IS NULL OR m.to_device = ?))) ORDER BY m.id ASC LIMIT 1",
			sent.ThreadID, sent.ID, p.FromSession, p.FromSession, p.Device,
		))
		if err == nil {
			_ = h.markReadLocked(p.FromSession, reply.ID)
			return AskResult{Sent: sent, Reply: &reply, Delivered: true, Delivery: report.Delivery, Warning: report.Warning}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return AskResult{}, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return AskResult{Sent: sent, TimedOut: true, Delivery: report.Delivery, Warning: report.Warning}, nil
		}
		if err := h.waitOnChannelLocked(ctx, subCh, remaining); err != nil {
			return AskResult{}, err
		}
	}
}
