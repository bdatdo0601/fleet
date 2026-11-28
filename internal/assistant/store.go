package assistant

import (
	"cmp"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bdatdo0601/fleet/internal/registry"
)

const assistantSchema = `
CREATE TABLE IF NOT EXISTS assistant_channels (
  id            TEXT PRIMARY KEY,
  title         TEXT NOT NULL,
  description   TEXT NOT NULL DEFAULT '',
  model         TEXT NOT NULL DEFAULT '',
  context_epoch INTEGER NOT NULL DEFAULT 1,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS assistant_messages (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  channel_id        TEXT NOT NULL,
  context_epoch     INTEGER NOT NULL DEFAULT 1,
  role              TEXT NOT NULL,
  content           TEXT NOT NULL DEFAULT '',
  model             TEXT NOT NULL DEFAULT '',
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  cached_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cost_usd          REAL NOT NULL DEFAULT 0,
  tool_calls_json   TEXT NOT NULL DEFAULT '',
  tool_call_id      TEXT NOT NULL DEFAULT '',
  widgets_json      TEXT NOT NULL DEFAULT '',
  observed          INTEGER NOT NULL DEFAULT 0,
  created_at        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_assistant_messages_channel
  ON assistant_messages(channel_id, id DESC);

CREATE TABLE IF NOT EXISTS assistant_inbox (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  channel_id     TEXT NOT NULL,
  source         TEXT NOT NULL,
  dedupe_key     TEXT NOT NULL UNIQUE,
  payload_json   TEXT NOT NULL DEFAULT '{}',
  model_override TEXT NOT NULL DEFAULT '',
  status         TEXT NOT NULL DEFAULT 'queued',
  error          TEXT NOT NULL DEFAULT '',
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_assistant_inbox_status
  ON assistant_inbox(status, id ASC);

CREATE TABLE IF NOT EXISTS assistant_timers (
  id               TEXT PRIMARY KEY,
  channel_id       TEXT NOT NULL,
  name             TEXT NOT NULL,
  prompt           TEXT NOT NULL,
  model            TEXT NOT NULL DEFAULT '',
  schedule_kind    TEXT NOT NULL,
  fire_at          TEXT NOT NULL DEFAULT '',
  interval_seconds INTEGER NOT NULL DEFAULT 0,
  cron_expr        TEXT NOT NULL DEFAULT '',
  enabled          INTEGER NOT NULL DEFAULT 1,
  last_run_at      TEXT NOT NULL DEFAULT '',
  next_run_at      TEXT NOT NULL DEFAULT '',
  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_assistant_timers_due
  ON assistant_timers(enabled, next_run_at);

CREATE TABLE IF NOT EXISTS assistant_approvals (
  id            TEXT PRIMARY KEY,
  channel_id    TEXT NOT NULL,
  message_id    INTEGER NOT NULL DEFAULT 0,
  tool_name     TEXT NOT NULL,
  server_name   TEXT NOT NULL DEFAULT '',
  args_json     TEXT NOT NULL DEFAULT '{}',
  summary       TEXT NOT NULL DEFAULT '',
  risk_level    TEXT NOT NULL DEFAULT 'write',
  status        TEXT NOT NULL DEFAULT 'pending',
  decision_note TEXT NOT NULL DEFAULT '',
  result_json   TEXT NOT NULL DEFAULT '',
  created_at    TEXT NOT NULL,
  decided_at    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_assistant_approvals_status
  ON assistant_approvals(status, created_at DESC);

CREATE TABLE IF NOT EXISTS assistant_outbox (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  channel_id   TEXT NOT NULL,
  priority     TEXT NOT NULL DEFAULT 'normal',
  title        TEXT NOT NULL DEFAULT '',
  body         TEXT NOT NULL,
  delivered    INTEGER NOT NULL DEFAULT 0,
  created_at   TEXT NOT NULL,
  delivered_at TEXT NOT NULL DEFAULT ''
);
`

// Channel represents one persistent assistant conversation stream.
type Channel struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	Model        string `json:"model"`
	ContextEpoch int    `json:"context_epoch"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// WidgetPayload represents either a typed native UI card or a sandboxed generative HTML widget.
type WidgetPayload struct {
	Kind  string `json:"kind"`
	Title string `json:"title,omitempty"`
	HTML  string `json:"html,omitempty"`
	Data  any    `json:"data,omitempty"`
}

// Message represents one stored user, assistant, or tool message.
type Message struct {
	ID               int64           `json:"id"`
	ChannelID        string          `json:"channel_id"`
	ContextEpoch     int             `json:"context_epoch"`
	Role             string          `json:"role"`
	Content          string          `json:"content"`
	Model            string          `json:"model,omitempty"`
	PromptTokens     int             `json:"prompt_tokens,omitempty"`
	CachedTokens     int             `json:"cached_tokens,omitempty"`
	CompletionTokens int             `json:"completion_tokens,omitempty"`
	CostUSD          float64         `json:"cost_usd,omitempty"`
	ToolCallsJSON    string          `json:"tool_calls_json,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	Widgets          []WidgetPayload `json:"widgets,omitempty"`
	Observed         bool            `json:"observed"`
	CreatedAt        string          `json:"created_at"`
}

// InboxEvent is a queued wakeup stimulus for the single assistant worker.
type InboxEvent struct {
	ID            int64  `json:"id"`
	ChannelID     string `json:"channel_id"`
	Source        string `json:"source"`
	DedupeKey     string `json:"dedupe_key"`
	PayloadJSON   string `json:"payload_json"`
	ModelOverride string `json:"model_override,omitempty"`
	Status        string `json:"status"`
	Error         string `json:"error,omitempty"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// Timer represents a one-shot or recurring scheduled wakeup.
type Timer struct {
	ID              string `json:"id"`
	ChannelID       string `json:"channel_id"`
	Name            string `json:"name"`
	Prompt          string `json:"prompt"`
	Model           string `json:"model,omitempty"`
	ScheduleKind    string `json:"schedule_kind"`
	FireAt          string `json:"fire_at,omitempty"`
	IntervalSeconds int    `json:"interval_seconds,omitempty"`
	CronExpr        string `json:"cron_expr,omitempty"`
	Enabled         bool   `json:"enabled"`
	LastRunAt       string `json:"last_run_at,omitempty"`
	NextRunAt       string `json:"next_run_at,omitempty"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

// Approval represents a deferred Human-in-the-Loop tool call authorization.
type Approval struct {
	ID           string `json:"id"`
	ChannelID    string `json:"channel_id"`
	MessageID    int64  `json:"message_id"`
	ToolName     string `json:"tool_name"`
	ServerName   string `json:"server_name,omitempty"`
	ArgsJSON     string `json:"args_json"`
	Summary      string `json:"summary"`
	RiskLevel    string `json:"risk_level"`
	Status       string `json:"status"`
	DecisionNote string `json:"decision_note,omitempty"`
	ResultJSON   string `json:"result_json,omitempty"`
	CreatedAt    string `json:"created_at"`
	DecidedAt    string `json:"decided_at,omitempty"`
}

// OutboxItem records a proactive assistant notification.
type OutboxItem struct {
	ID          int64  `json:"id"`
	ChannelID   string `json:"channel_id"`
	Priority    string `json:"priority"`
	Title       string `json:"title,omitempty"`
	Body        string `json:"body"`
	Delivered   bool   `json:"delivered"`
	CreatedAt   string `json:"created_at"`
	DeliveredAt string `json:"delivered_at,omitempty"`
}

// SummaryCounts reports live assistant queue/approval counts for overview UI.
type SummaryCounts struct {
	Channels         int `json:"channels"`
	PendingApprovals int `json:"pending_approvals"`
	EnabledTimers    int `json:"enabled_timers"`
	QueuedInbox      int `json:"queued_inbox"`
	OutboxPending    int `json:"outbox_pending"`
}

// Store wraps SQLite persistence in ~/.fleet/assistant.db.
type Store struct {
	Path string
	mu   sync.Mutex
	db   *sql.DB
}

func randomHexID(prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return prefix + "-" + hex.EncodeToString(b[:])
}

// OpenStore initializes assistant.db at path and seeds default channels.
func OpenStore(path string) (*Store, error) {
	db, err := registry.OpenSQLite(path, assistantSchema, "assistant_messages", nil)
	if err != nil {
		return nil, err
	}
	s := &Store{Path: path, db: db}
	if err := s.seedDefaultChannels(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
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

func (s *Store) seedDefaultChannels() error {
	now := registry.NowISO()
	defaults := []Channel{
		{ID: "general", Title: "#general", Description: "Everyday assistant chat across your fleet and second brain"},
		{ID: "fleet-ops", Title: "#fleet-ops", Description: "Live Fleet coding agents, sessions, memory, and Slack triage"},
		{ID: "briefing", Title: "#briefing", Description: "Scheduled heartbeats, morning briefings, and proactive alerts", Model: DefaultFastModel},
	}
	for _, ch := range defaults {
		if _, err := s.db.Exec(
			`INSERT OR IGNORE INTO assistant_channels(id, title, description, model, context_epoch, created_at, updated_at)
			 VALUES (?, ?, ?, ?, 1, ?, ?)`,
			ch.ID, ch.Title, ch.Description, ch.Model, now, now,
		); err != nil {
			return err
		}
	}
	return nil
}

// ListChannels returns all channels sorted with general first.
func (s *Store) ListChannels() ([]Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT id, title, description, model, context_epoch, created_at, updated_at
		FROM assistant_channels
		ORDER BY CASE id WHEN 'general' THEN 0 WHEN 'fleet-ops' THEN 1 WHEN 'briefing' THEN 2 ELSE 3 END, id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Channel
	for rows.Next() {
		var ch Channel
		if err := rows.Scan(&ch.ID, &ch.Title, &ch.Description, &ch.Model, &ch.ContextEpoch, &ch.CreatedAt, &ch.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, rows.Err()
}

// GetChannel returns one channel by ID.
func (s *Store) GetChannel(id string) (*Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, newErr("channel id is required", http.StatusBadRequest)
	}
	var ch Channel
	err := s.db.QueryRow(
		`SELECT id, title, description, model, context_epoch, created_at, updated_at FROM assistant_channels WHERE id = ?`, id,
	).Scan(&ch.ID, &ch.Title, &ch.Description, &ch.Model, &ch.ContextEpoch, &ch.CreatedAt, &ch.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, newErr(fmt.Sprintf("channel %q not found", id), http.StatusNotFound)
	}
	if err != nil {
		return nil, err
	}
	return &ch, nil
}

// UpsertChannel creates or updates a channel.
func (s *Store) UpsertChannel(id, title, description, model string) (Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(id, "#")))
	if id == "" {
		return Channel{}, newErr("channel id is required", http.StatusBadRequest)
	}
	for _, r := range id {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !valid {
			return Channel{}, newErr("channel id must contain only lowercase letters, digits, '-' or '_'", http.StatusBadRequest)
		}
	}
	now := registry.NowISO()
	title = cmp.Or(strings.TrimSpace(title), "#"+id)
	if _, err := s.db.Exec(
		`INSERT INTO assistant_channels(id, title, description, model, context_epoch, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   title = excluded.title,
		   description = excluded.description,
		   model = excluded.model,
		   updated_at = excluded.updated_at`,
		id, title, strings.TrimSpace(description), strings.TrimSpace(model), now, now,
	); err != nil {
		return Channel{}, err
	}
	var ch Channel
	if err := s.db.QueryRow(
		`SELECT id, title, description, model, context_epoch, created_at, updated_at FROM assistant_channels WHERE id = ?`, id,
	).Scan(&ch.ID, &ch.Title, &ch.Description, &ch.Model, &ch.ContextEpoch, &ch.CreatedAt, &ch.UpdatedAt); err != nil {
		return Channel{}, err
	}
	return ch, nil
}

// ResetChannelContext increments context_epoch so active LLM context starts fresh,
// and optionally clears channel scrollback & pending approvals without touching long-term memory.
func (s *Store) ResetChannelContext(id string, clearHistory bool) (Channel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cleanID := strings.TrimSpace(id)
	now := registry.NowISO()
	res, err := s.db.Exec(
		`UPDATE assistant_channels SET context_epoch = context_epoch + 1, updated_at = ? WHERE id = ?`,
		now, cleanID,
	)
	if err != nil {
		return Channel{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Channel{}, newErr(fmt.Sprintf("channel %q not found", id), http.StatusNotFound)
	}
	if clearHistory {
		if _, err := s.db.Exec(`DELETE FROM assistant_messages WHERE channel_id = ?`, cleanID); err != nil {
			return Channel{}, err
		}
		if _, err := s.db.Exec(
			`UPDATE assistant_approvals SET status = 'cancelled', decided_at = ? WHERE channel_id = ? AND status = 'pending'`,
			now, cleanID,
		); err != nil {
			return Channel{}, err
		}
	}
	var ch Channel
	if err := s.db.QueryRow(
		`SELECT id, title, description, model, context_epoch, created_at, updated_at FROM assistant_channels WHERE id = ?`, cleanID,
	).Scan(&ch.ID, &ch.Title, &ch.Description, &ch.Model, &ch.ContextEpoch, &ch.CreatedAt, &ch.UpdatedAt); err != nil {
		return Channel{}, err
	}
	return ch, nil
}

// AppendMessage persists a new message in a channel.
func (s *Store) AppendMessage(m Message) (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.CreatedAt == "" {
		m.CreatedAt = registry.NowISO()
	}
	widgetsRaw := ""
	if len(m.Widgets) > 0 {
		b, err := json.Marshal(m.Widgets)
		if err != nil {
			return Message{}, err
		}
		widgetsRaw = string(b)
	}
	obs := 0
	if m.Observed {
		obs = 1
	}
	res, err := s.db.Exec(
		`INSERT INTO assistant_messages(
			channel_id, context_epoch, role, content, model,
			prompt_tokens, cached_tokens, completion_tokens, cost_usd,
			tool_calls_json, tool_call_id, widgets_json, observed, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ChannelID, max(m.ContextEpoch, 1), m.Role, m.Content, m.Model,
		m.PromptTokens, m.CachedTokens, m.CompletionTokens, m.CostUSD,
		m.ToolCallsJSON, m.ToolCallID, widgetsRaw, obs, m.CreatedAt,
	)
	if err != nil {
		return Message{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Message{}, err
	}
	m.ID = id
	return m, nil
}

func scanMessage(rows *sql.Rows) (Message, error) {
	var m Message
	var widgetsRaw string
	var obs int
	if err := rows.Scan(
		&m.ID, &m.ChannelID, &m.ContextEpoch, &m.Role, &m.Content, &m.Model,
		&m.PromptTokens, &m.CachedTokens, &m.CompletionTokens, &m.CostUSD,
		&m.ToolCallsJSON, &m.ToolCallID, &widgetsRaw, &obs, &m.CreatedAt,
	); err != nil {
		return Message{}, err
	}
	m.Observed = obs != 0
	if widgetsRaw != "" {
		_ = json.Unmarshal([]byte(widgetsRaw), &m.Widgets)
	}
	return m, nil
}

// ListMessages returns chronological messages for a channel (capped by limit).
func (s *Store) ListMessages(channelID string, limit int) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(
		`SELECT id, channel_id, context_epoch, role, content, model,
		        prompt_tokens, cached_tokens, completion_tokens, cost_usd,
		        tool_calls_json, tool_call_id, widgets_json, observed, created_at
		 FROM assistant_messages
		 WHERE channel_id = ?
		 ORDER BY id DESC LIMIT ?`,
		channelID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var rev []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		rev = append(rev, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, nil
}

// ListContextMessages returns unobserved messages for the active (channel_id, context_epoch).
func (s *Store) ListContextMessages(channelID string, epoch, limit int) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		limit = 40
	}
	rows, err := s.db.Query(
		`SELECT id, channel_id, context_epoch, role, content, model,
		        prompt_tokens, cached_tokens, completion_tokens, cost_usd,
		        tool_calls_json, tool_call_id, widgets_json, observed, created_at
		 FROM assistant_messages
		 WHERE channel_id = ? AND context_epoch = ? AND observed = 0
		 ORDER BY id DESC LIMIT ?`,
		channelID, max(epoch, 1), limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var rev []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		rev = append(rev, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, nil
}

// ListUnobservedMessages returns unobserved user/assistant messages across channels for Observational Memory.
func (s *Store) ListUnobservedMessages(channelID string, limit int) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		limit = 200
	}
	query := `SELECT id, channel_id, context_epoch, role, content, model,
	        prompt_tokens, cached_tokens, completion_tokens, cost_usd,
	        tool_calls_json, tool_call_id, widgets_json, observed, created_at
	 FROM assistant_messages
	 WHERE observed = 0 AND role IN ('user', 'assistant')`
	var args []any
	if channelID != "" {
		query += " AND channel_id = ?"
		args = append(args, channelID)
	}
	query += " ORDER BY id ASC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MarkMessagesObserved marks message IDs <= maxID as observed.
func (s *Store) MarkMessagesObserved(channelID string, maxID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if channelID != "" {
		_, err := s.db.Exec(`UPDATE assistant_messages SET observed = 1 WHERE channel_id = ? AND id <= ? AND observed = 0`, channelID, maxID)
		return err
	}
	_, err := s.db.Exec(`UPDATE assistant_messages SET observed = 1 WHERE id <= ? AND observed = 0`, maxID)
	return err
}

// EnqueueInbox inserts an event into assistant_inbox (deduplicating on dedupe_key).
func (s *Store) EnqueueInbox(channelID, source, dedupeKey string, payload any, modelOverride string) (InboxEvent, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if dedupeKey == "" {
		dedupeKey = randomHexID("ev")
	}
	rawPayload := "{}"
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return InboxEvent{}, false, err
		}
		rawPayload = string(b)
	}
	now := registry.NowISO()
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO assistant_inbox(channel_id, source, dedupe_key, payload_json, model_override, status, error, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, 'queued', '', ?, ?)`,
		cmp.Or(channelID, "general"), source, dedupeKey, rawPayload, strings.TrimSpace(modelOverride), now, now,
	)
	if err != nil {
		return InboxEvent{}, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return InboxEvent{}, false, nil
	}
	id, _ := res.LastInsertId()
	return InboxEvent{
		ID:            id,
		ChannelID:     cmp.Or(channelID, "general"),
		Source:        source,
		DedupeKey:     dedupeKey,
		PayloadJSON:   rawPayload,
		ModelOverride: strings.TrimSpace(modelOverride),
		Status:        "queued",
		CreatedAt:     now,
		UpdatedAt:     now,
	}, true, nil
}

// ClaimNextInbox atomically claims the oldest queued inbox event.
func (s *Store) ClaimNextInbox() (*InboxEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ev InboxEvent
	err := s.db.QueryRow(
		`SELECT id, channel_id, source, dedupe_key, payload_json, model_override, status, error, created_at, updated_at
		 FROM assistant_inbox WHERE status = 'queued' ORDER BY id ASC LIMIT 1`,
	).Scan(&ev.ID, &ev.ChannelID, &ev.Source, &ev.DedupeKey, &ev.PayloadJSON, &ev.ModelOverride, &ev.Status, &ev.Error, &ev.CreatedAt, &ev.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now := registry.NowISO()
	if _, err := s.db.Exec(`UPDATE assistant_inbox SET status = 'running', updated_at = ? WHERE id = ?`, now, ev.ID); err != nil {
		return nil, err
	}
	ev.Status = "running"
	ev.UpdatedAt = now
	return &ev, nil
}

// FinishInbox marks an inbox event as done or failed.
func (s *Store) FinishInbox(id int64, status, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`UPDATE assistant_inbox SET status = ?, error = ?, updated_at = ? WHERE id = ?`,
		status, errMsg, registry.NowISO(), id,
	)
	return err
}

// UpsertTimer creates or updates a scheduled timer.
func (s *Store) UpsertTimer(t Timer) (Timer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := registry.NowISO()
	if t.ID == "" {
		t.ID = randomHexID("tmr")
	}
	t.ChannelID = cmp.Or(strings.TrimSpace(t.ChannelID), "briefing")
	t.ScheduleKind = cmp.Or(strings.TrimSpace(t.ScheduleKind), "interval")
	if t.NextRunAt == "" {
		switch {
		case t.ScheduleKind == "once" && t.FireAt != "":
			t.NextRunAt = t.FireAt
		case t.IntervalSeconds > 0:
			t.NextRunAt = time.Now().UTC().Add(time.Duration(t.IntervalSeconds) * time.Second).Truncate(time.Second).Format("2006-01-02T15:04:05+00:00")
		default:
			t.NextRunAt = now
		}
	}
	en := 0
	if t.Enabled {
		en = 1
	}
	t.CreatedAt = cmp.Or(t.CreatedAt, now)
	t.UpdatedAt = now
	if _, err := s.db.Exec(
		`INSERT INTO assistant_timers(
			id, channel_id, name, prompt, model, schedule_kind, fire_at,
			interval_seconds, cron_expr, enabled, last_run_at, next_run_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			channel_id = excluded.channel_id,
			name = excluded.name,
			prompt = excluded.prompt,
			model = excluded.model,
			schedule_kind = excluded.schedule_kind,
			fire_at = excluded.fire_at,
			interval_seconds = excluded.interval_seconds,
			cron_expr = excluded.cron_expr,
			enabled = excluded.enabled,
			next_run_at = excluded.next_run_at,
			updated_at = excluded.updated_at`,
		t.ID, t.ChannelID, t.Name, t.Prompt, t.Model, t.ScheduleKind, t.FireAt,
		t.IntervalSeconds, t.CronExpr, en, t.LastRunAt, t.NextRunAt, t.CreatedAt, t.UpdatedAt,
	); err != nil {
		return Timer{}, err
	}
	return t, nil
}

// ListTimers returns all timers.
func (s *Store) ListTimers() ([]Timer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT id, channel_id, name, prompt, model, schedule_kind, fire_at,
		        interval_seconds, cron_expr, enabled, last_run_at, next_run_at, created_at, updated_at
		 FROM assistant_timers ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Timer
	for rows.Next() {
		var t Timer
		var en int
		if err := rows.Scan(
			&t.ID, &t.ChannelID, &t.Name, &t.Prompt, &t.Model, &t.ScheduleKind, &t.FireAt,
			&t.IntervalSeconds, &t.CronExpr, &en, &t.LastRunAt, &t.NextRunAt, &t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, err
		}
		t.Enabled = en != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// DueTimers returns enabled timers whose next_run_at <= now.
func (s *Store) DueTimers(nowISO string) ([]Timer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT id, channel_id, name, prompt, model, schedule_kind, fire_at,
		        interval_seconds, cron_expr, enabled, last_run_at, next_run_at, created_at, updated_at
		 FROM assistant_timers WHERE enabled = 1 AND next_run_at != '' AND next_run_at <= ?`,
		nowISO,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Timer
	for rows.Next() {
		var t Timer
		var en int
		if err := rows.Scan(
			&t.ID, &t.ChannelID, &t.Name, &t.Prompt, &t.Model, &t.ScheduleKind, &t.FireAt,
			&t.IntervalSeconds, &t.CronExpr, &en, &t.LastRunAt, &t.NextRunAt, &t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, err
		}
		t.Enabled = en != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// MarkTimerRun updates last_run_at and next_run_at (or disables one-shot timers).
func (s *Store) MarkTimerRun(t Timer, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	nowISO := now.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05+00:00")
	enabled := 1
	nextRun := ""
	if t.ScheduleKind == "once" || t.IntervalSeconds <= 0 {
		enabled = 0
	} else {
		nextRun = now.UTC().Add(time.Duration(t.IntervalSeconds) * time.Second).Truncate(time.Second).Format("2006-01-02T15:04:05+00:00")
	}
	_, err := s.db.Exec(
		`UPDATE assistant_timers SET enabled = ?, last_run_at = ?, next_run_at = ?, updated_at = ? WHERE id = ?`,
		enabled, nowISO, nextRun, nowISO, t.ID,
	)
	return err
}

// DeleteTimer removes a timer by ID.
func (s *Store) DeleteTimer(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM assistant_timers WHERE id = ?`, strings.TrimSpace(id))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return newErr(fmt.Sprintf("timer %q not found", id), http.StatusNotFound)
	}
	return nil
}

// CreateApproval stores a new pending HITL tool approval.
func (s *Store) CreateApproval(a Approval) (Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.ID == "" {
		a.ID = randomHexID("apr")
	}
	a.Status = cmp.Or(a.Status, "pending")
	a.RiskLevel = cmp.Or(a.RiskLevel, "write")
	a.CreatedAt = cmp.Or(a.CreatedAt, registry.NowISO())
	_, err := s.db.Exec(
		`INSERT INTO assistant_approvals(
			id, channel_id, message_id, tool_name, server_name, args_json, summary,
			risk_level, status, decision_note, result_json, created_at, decided_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', ?, '')`,
		a.ID, a.ChannelID, a.MessageID, a.ToolName, a.ServerName, a.ArgsJSON, a.Summary,
		a.RiskLevel, a.Status, a.CreatedAt,
	)
	return a, err
}

// ListApprovals returns approvals filtered by status (all if empty).
func (s *Store) ListApprovals(status string, limit int) ([]Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		limit = 50
	}
	query := `SELECT id, channel_id, message_id, tool_name, server_name, args_json, summary,
		risk_level, status, decision_note, result_json, created_at, decided_at
		FROM assistant_approvals`
	var args []any
	if status != "" {
		query += " WHERE status = ?"
		args = append(args, status)
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Approval
	for rows.Next() {
		var a Approval
		if err := rows.Scan(
			&a.ID, &a.ChannelID, &a.MessageID, &a.ToolName, &a.ServerName, &a.ArgsJSON, &a.Summary,
			&a.RiskLevel, &a.Status, &a.DecisionNote, &a.ResultJSON, &a.CreatedAt, &a.DecidedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetApproval fetches one approval by ID.
func (s *Store) GetApproval(id string) (*Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var a Approval
	err := s.db.QueryRow(
		`SELECT id, channel_id, message_id, tool_name, server_name, args_json, summary,
		 risk_level, status, decision_note, result_json, created_at, decided_at
		 FROM assistant_approvals WHERE id = ?`, strings.TrimSpace(id),
	).Scan(
		&a.ID, &a.ChannelID, &a.MessageID, &a.ToolName, &a.ServerName, &a.ArgsJSON, &a.Summary,
		&a.RiskLevel, &a.Status, &a.DecisionNote, &a.ResultJSON, &a.CreatedAt, &a.DecidedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, newErr(fmt.Sprintf("approval %q not found", id), http.StatusNotFound)
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// DecideApproval transitions a pending approval to approved or denied.
func (s *Store) DecideApproval(id string, approve bool, note string) (Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := "denied"
	if approve {
		status = "approved"
	}
	now := registry.NowISO()
	res, err := s.db.Exec(
		`UPDATE assistant_approvals SET status = ?, decision_note = ?, decided_at = ?
		 WHERE id = ? AND status = 'pending'`,
		status, strings.TrimSpace(note), now, strings.TrimSpace(id),
	)
	if err != nil {
		return Approval{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Approval{}, newErr("approval not found or already decided", http.StatusConflict)
	}
	var a Approval
	if err := s.db.QueryRow(
		`SELECT id, channel_id, message_id, tool_name, server_name, args_json, summary,
		 risk_level, status, decision_note, result_json, created_at, decided_at
		 FROM assistant_approvals WHERE id = ?`, strings.TrimSpace(id),
	).Scan(
		&a.ID, &a.ChannelID, &a.MessageID, &a.ToolName, &a.ServerName, &a.ArgsJSON, &a.Summary,
		&a.RiskLevel, &a.Status, &a.DecisionNote, &a.ResultJSON, &a.CreatedAt, &a.DecidedAt,
	); err != nil {
		return Approval{}, err
	}
	return a, nil
}

// SetApprovalResult records execution result after an approved tool runs.
func (s *Store) SetApprovalResult(id, status, resultJSON string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`UPDATE assistant_approvals SET status = ?, result_json = ? WHERE id = ?`,
		status, resultJSON, id,
	)
	return err
}

// AddOutbox stores an outbound proactive notification.
func (s *Store) AddOutbox(channelID, priority, title, body string) (OutboxItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := registry.NowISO()
	res, err := s.db.Exec(
		`INSERT INTO assistant_outbox(channel_id, priority, title, body, delivered, created_at, delivered_at)
		 VALUES (?, ?, ?, ?, 0, ?, '')`,
		cmp.Or(channelID, "briefing"), cmp.Or(priority, "normal"), title, body, now,
	)
	if err != nil {
		return OutboxItem{}, err
	}
	id, _ := res.LastInsertId()
	return OutboxItem{
		ID:        id,
		ChannelID: cmp.Or(channelID, "briefing"),
		Priority:  cmp.Or(priority, "normal"),
		Title:     title,
		Body:      body,
		CreatedAt: now,
	}, nil
}

// Summary returns aggregate counts for Fleet overview.
func (s *Store) Summary() SummaryCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sc SummaryCounts
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM assistant_channels`).Scan(&sc.Channels)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM assistant_approvals WHERE status = 'pending'`).Scan(&sc.PendingApprovals)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM assistant_timers WHERE enabled = 1`).Scan(&sc.EnabledTimers)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM assistant_inbox WHERE status = 'queued'`).Scan(&sc.QueuedInbox)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM assistant_outbox WHERE delivered = 0`).Scan(&sc.OutboxPending)
	return sc
}
