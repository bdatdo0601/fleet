// Package agents implements the SQLite agent session registry and cross-node message broker (agents.db).
package agents

import (
	"database/sql"
	"os"
	"strings"
	"sync"
	"time"

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

CREATE TABLE IF NOT EXISTS harness_usage (
  harness      TEXT PRIMARY KEY,
  device       TEXT NOT NULL DEFAULT '',
  source       TEXT NOT NULL DEFAULT '',
  plan         TEXT NOT NULL DEFAULT '',
  status       TEXT NOT NULL DEFAULT 'ok',
  detail       TEXT NOT NULL DEFAULT '',
  windows_json TEXT NOT NULL DEFAULT '[]',
  metrics_json TEXT NOT NULL DEFAULT '{}',
  updated_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS agent_paths (
  device     TEXT NOT NULL,
  path       TEXT NOT NULL,
  repo       TEXT NOT NULL DEFAULT '',
  source     TEXT NOT NULL DEFAULT '',
  use_count  INTEGER NOT NULL DEFAULT 1,
  first_seen TEXT NOT NULL,
  last_seen  TEXT NOT NULL,
  PRIMARY KEY (device, path)
);
CREATE INDEX IF NOT EXISTS idx_agent_paths_last_seen
  ON agent_paths(last_seen DESC);
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

// ErrHubClosed is returned when operations are invoked on a closed Hub.
var ErrHubClosed error = &AgentError{Msg: "agents hub is closed", Status: 503}

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
	SessionID       string            `json:"session_id"`
	Device          string            `json:"device"`
	Harness         string            `json:"harness"`
	FullRawID       string            `json:"full_raw_id"`
	PID             *int              `json:"pid"`
	CWD             string            `json:"cwd"`
	Repo            string            `json:"repo"`
	Branch          string            `json:"branch"`
	State           string            `json:"state"`
	StateChangedAt  string            `json:"state_changed_at"`
	StateAgeSeconds int               `json:"state_age_seconds"`
	WaitingSeconds  *int              `json:"waiting_seconds"`
	PendingAction   map[string]string `json:"pending_action"`
	Live            bool              `json:"live"`
	Listening       bool              `json:"listening"`
	Delivery        string            `json:"delivery"`
	AgeSeconds      int               `json:"age_seconds"`
	Title           string            `json:"title"`
	Name            string            `json:"name"`
	Summary         string            `json:"summary"`
	GitStatus       string            `json:"git_status"`
	TurnCount       int               `json:"turn_count"`
	StartedAt       string            `json:"started_at"`
	UpdatedAt       string            `json:"updated_at"`
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

// HarnessWaitParams controls WaitForHarnessMessages.
type HarnessWaitParams struct {
	Device        string
	Harness       string
	Timeout       float64
	MarkRead      bool
	Limit         int
	RequireFullID bool
	ExcludeIDs    []string
}

// SessionMessages groups unread messages for a single session returned by WaitForHarnessMessages.
type SessionMessages struct {
	SessionID string    `json:"session_id"`
	FullRawID string    `json:"full_raw_id"`
	PID       *int      `json:"pid,omitempty"`
	Name      string    `json:"name"`
	Title     string    `json:"title"`
	Messages  []Message `json:"messages"`
}

// HarnessWaitResult is returned by WaitForHarnessMessages.
type HarnessWaitResult struct {
	Device   string            `json:"device"`
	Harness  string            `json:"harness"`
	Batches  []SessionMessages `json:"batches"`
	TimedOut bool              `json:"timed_out"`
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
	Path               string
	mu                 sync.Mutex
	db                 *sql.DB
	lastPrune          time.Time
	waiters            map[string]int
	waitSeenAt         map[string]time.Time
	harnessRelays      map[string]int
	harnessRelaySeenAt map[string]time.Time
	promotedTo         map[string]string
	sessionSubs        subMap
	harnessSubs        subMap
	threadSubs         subMap
	closed             bool
	doneCh             chan struct{}
}

// Open opens or initializes agents.db at path, creating its parent directory and running migrations.
func Open(path string) (*Hub, error) {
	db, err := registry.OpenSQLite(path, schema, "agent_sessions", sessionMigrations)
	if err != nil {
		return nil, err
	}
	if err := backfillPathsFromSessions(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return &Hub{
		Path:               path,
		db:                 db,
		waiters:            map[string]int{},
		waitSeenAt:         map[string]time.Time{},
		harnessRelays:      map[string]int{},
		harnessRelaySeenAt: map[string]time.Time{},
		promotedTo:         map[string]string{},
		sessionSubs:        subMap{},
		harnessSubs:        subMap{},
		threadSubs:         subMap{},
		doneCh:             make(chan struct{}),
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

func (h *Hub) checkOpenLocked() error {
	if h.closed || h.db == nil {
		return ErrHubClosed
	}
	return nil
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
	for key := range h.harnessSubs {
		if toDevice != nil {
			dev, _, _ := strings.Cut(key, ":")
			if dev != *toDevice {
				continue
			}
		}
		h.harnessSubs.notify(key)
	}
}

func (h *Hub) pruneMemoryMapsLocked(now time.Time) {
	grace := time.Duration(WaiterRearmGraceSeconds*2) * time.Second
	for sid, ts := range h.waitSeenAt {
		if h.waiters[sid] == 0 && now.Sub(ts) > grace {
			delete(h.waitSeenAt, sid)
		}
	}
	for key, ts := range h.harnessRelaySeenAt {
		if h.harnessRelays[key] == 0 && now.Sub(ts) > grace {
			delete(h.harnessRelaySeenAt, key)
		}
	}
	if len(h.promotedTo) == 0 || h.db == nil {
		return
	}
	rows, err := h.db.Query("SELECT session_id FROM agent_sessions")
	if err != nil {
		return
	}
	defer func() { _ = rows.Close() }()
	liveIDs := make(map[string]bool)
	for rows.Next() {
		var sid string
		if rows.Scan(&sid) == nil {
			liveIDs[sid] = true
		}
	}
	if rows.Err() != nil {
		return
	}
	for oldID := range h.promotedTo {
		if !liveIDs[h.resolvePromotedLocked(oldID)] {
			delete(h.promotedTo, oldID)
		}
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
	if err := tx.Commit(); err != nil {
		return err
	}
	h.pruneMemoryMapsLocked(now)
	return nil
}
