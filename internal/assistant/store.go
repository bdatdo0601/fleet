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
	"os"
	"strconv"
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
  source            TEXT NOT NULL DEFAULT '',
  external_id       TEXT NOT NULL DEFAULT '',
  observed          INTEGER NOT NULL DEFAULT 0,
  created_at        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_assistant_messages_channel
  ON assistant_messages(channel_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_assistant_messages_unobserved
  ON assistant_messages(channel_id, context_epoch, observed, id DESC);
CREATE INDEX IF NOT EXISTS idx_assistant_messages_created
  ON assistant_messages(created_at);

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

CREATE TABLE IF NOT EXISTS assistant_delegations (
  id                  TEXT PRIMARY KEY,
  job_id              TEXT NOT NULL DEFAULT '',
  channel_id          TEXT NOT NULL,
  target_device       TEXT NOT NULL,
  harness             TEXT NOT NULL,
  session_name        TEXT NOT NULL,
  session_id          TEXT NOT NULL DEFAULT '',
  repo_path           TEXT NOT NULL DEFAULT '',
  prompt              TEXT NOT NULL DEFAULT '',
  status              TEXT NOT NULL DEFAULT 'queued',
  last_notified_state TEXT NOT NULL DEFAULT '',
  created_at          TEXT NOT NULL,
  updated_at          TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_assistant_delegations_active
  ON assistant_delegations(status, target_device, session_name);

CREATE TABLE IF NOT EXISTS assistant_frontier_grants (
  channel_id    TEXT PRIMARY KEY,
  context_epoch INTEGER NOT NULL,
  topic_summary TEXT NOT NULL DEFAULT '',
  turns_used    INTEGER NOT NULL DEFAULT 0,
  max_turns     INTEGER NOT NULL DEFAULT 5,
  cost_usd      REAL NOT NULL DEFAULT 0,
  max_cost_usd  REAL NOT NULL DEFAULT 0.25,
  expires_at    TEXT NOT NULL,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS assistant_files (
  id           TEXT PRIMARY KEY,
  channel_id   TEXT NOT NULL,
  message_id   INTEGER NOT NULL DEFAULT 0,
  direction    TEXT NOT NULL,
  name         TEXT NOT NULL,
  mime_type    TEXT NOT NULL DEFAULT 'application/octet-stream',
  size_bytes   INTEGER NOT NULL DEFAULT 0,
  storage_path TEXT NOT NULL,
  created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_assistant_files_channel
  ON assistant_files(channel_id, created_at DESC);

CREATE TABLE IF NOT EXISTS assistant_discord_state (
  channel_id         TEXT PRIMARY KEY,
  discord_channel_id TEXT NOT NULL,
  last_message_id    TEXT NOT NULL DEFAULT '',
  updated_at         TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS assistant_mcp_oauth_tokens (
  server_name   TEXT PRIMARY KEY,
  access_token  TEXT NOT NULL DEFAULT '',
  refresh_token TEXT NOT NULL DEFAULT '',
  token_type    TEXT NOT NULL DEFAULT 'Bearer',
  scope         TEXT NOT NULL DEFAULT '',
  expires_at    TEXT NOT NULL DEFAULT '',
  token_url     TEXT NOT NULL DEFAULT '',
  updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS assistant_vault_chunks (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  vault_name      TEXT NOT NULL,
  rel_path        TEXT NOT NULL,
  mtime_ns        INTEGER NOT NULL,
  size_bytes      INTEGER NOT NULL,
  heading         TEXT NOT NULL DEFAULT '',
  chunk_idx       INTEGER NOT NULL,
  text            TEXT NOT NULL,
  embedding       BLOB,
  embedding_model TEXT NOT NULL DEFAULT '',
  updated_at      TEXT NOT NULL,
  UNIQUE(vault_name, rel_path, chunk_idx)
);
CREATE INDEX IF NOT EXISTS idx_vault_chunks_lookup
  ON assistant_vault_chunks(vault_name, rel_path);

CREATE VIRTUAL TABLE IF NOT EXISTS assistant_vault_chunks_fts USING fts5(
  vault_name,
  rel_path,
  heading,
  text,
  content='assistant_vault_chunks',
  content_rowid='id',
  tokenize='porter unicode61'
);

CREATE TRIGGER IF NOT EXISTS vault_chunks_ai AFTER INSERT ON assistant_vault_chunks BEGIN
  INSERT INTO assistant_vault_chunks_fts(rowid, vault_name, rel_path, heading, text)
  VALUES (new.id, new.vault_name, new.rel_path, new.heading, new.text);
END;

CREATE TRIGGER IF NOT EXISTS vault_chunks_ad AFTER DELETE ON assistant_vault_chunks BEGIN
  INSERT INTO assistant_vault_chunks_fts(assistant_vault_chunks_fts, rowid, vault_name, rel_path, heading, text)
  VALUES ('delete', old.id, old.vault_name, old.rel_path, old.heading, old.text);
END;

CREATE TRIGGER IF NOT EXISTS vault_chunks_au AFTER UPDATE ON assistant_vault_chunks BEGIN
  INSERT INTO assistant_vault_chunks_fts(assistant_vault_chunks_fts, rowid, vault_name, rel_path, heading, text)
  VALUES ('delete', old.id, old.vault_name, old.rel_path, old.heading, old.text);
  INSERT INTO assistant_vault_chunks_fts(rowid, vault_name, rel_path, heading, text)
  VALUES (new.id, new.vault_name, new.rel_path, new.heading, new.text);
END;
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
	Source           string          `json:"source,omitempty"`
	ExternalID       string          `json:"external_id,omitempty"`
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

// Delegation tracks a coding agent session spawned by the assistant for event-driven follow-ups.
type Delegation struct {
	ID                string `json:"id"`
	JobID             string `json:"job_id,omitempty"`
	ChannelID         string `json:"channel_id"`
	TargetDevice      string `json:"target_device"`
	Harness           string `json:"harness"`
	SessionName       string `json:"session_name"`
	SessionID         string `json:"session_id,omitempty"`
	RepoPath          string `json:"repo_path,omitempty"`
	Prompt            string `json:"prompt,omitempty"`
	Status            string `json:"status"`
	LastNotifiedState string `json:"last_notified_state,omitempty"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
}

// FrontierGrant represents a context-scoped auto-approval grant for frontier model turns on a channel topic.
type FrontierGrant struct {
	ChannelID    string  `json:"channel_id"`
	ContextEpoch int     `json:"context_epoch"`
	TopicSummary string  `json:"topic_summary"`
	TurnsUsed    int     `json:"turns_used"`
	MaxTurns     int     `json:"max_turns"`
	CostUSD      float64 `json:"cost_usd"`
	MaxCostUSD   float64 `json:"max_cost_usd"`
	ExpiresAt    string  `json:"expires_at"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
}

// SummaryCounts reports live assistant queue/approval counts for overview UI.
type SummaryCounts struct {
	Channels         int `json:"channels"`
	PendingApprovals int `json:"pending_approvals"`
	EnabledTimers    int `json:"enabled_timers"`
	QueuedInbox      int `json:"queued_inbox"`
	OutboxPending    int `json:"outbox_pending"`
}

// AssistantFile represents an uploaded or sandbox-generated file attached to a channel.
type AssistantFile struct {
	ID          string `json:"id"`
	ChannelID   string `json:"channel_id"`
	MessageID   int64  `json:"message_id,omitempty"`
	Direction   string `json:"direction"`
	Name        string `json:"name"`
	MimeType    string `json:"mime_type"`
	SizeBytes   int64  `json:"size_bytes"`
	StoragePath string `json:"-"`
	URL         string `json:"url,omitempty"`
	IsImage     bool   `json:"is_image,omitempty"`
	CreatedAt   string `json:"created_at"`
}

// MCPOAuthToken stores cached OAuth access/refresh tokens for an HTTP MCP server.
type MCPOAuthToken struct {
	ServerName   string `json:"server_name"`
	AccessToken  string `json:"-"`
	RefreshToken string `json:"-"`
	TokenType    string `json:"token_type,omitempty"`
	Scope        string `json:"scope,omitempty"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	TokenURL     string `json:"token_url,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
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

var messageMigrations = []registry.ColumnMigration{
	{Col: "source", Def: "TEXT NOT NULL DEFAULT ''"},
	{Col: "external_id", Def: "TEXT NOT NULL DEFAULT ''"},
}

// OpenStore initializes assistant.db at path and seeds default channels.
func OpenStore(path string) (*Store, error) {
	db, err := registry.OpenSQLite(path, assistantSchema, "assistant_messages", messageMigrations)
	if err != nil {
		return nil, err
	}
	if path != "" && path != ":memory:" && !strings.HasPrefix(path, "file:") {
		if err := os.Chmod(path, 0o600); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_assistant_messages_external ON assistant_messages(source, external_id) WHERE external_id != ''`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(`UPDATE assistant_inbox SET status = 'queued', updated_at = ? WHERE status = 'running'`, registry.NowISO()); err != nil {
		_ = db.Close()
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
	return s.db.Close()
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
	tx, err := s.db.Begin()
	if err != nil {
		return Channel{}, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(
		`UPDATE assistant_channels SET context_epoch = context_epoch + 1, updated_at = ? WHERE id = ?`,
		now, cleanID,
	)
	if err != nil {
		return Channel{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Channel{}, newErr(fmt.Sprintf("channel %q not found", id), http.StatusNotFound)
	}
	if _, err := tx.Exec(`DELETE FROM assistant_frontier_grants WHERE channel_id = ?`, cleanID); err != nil {
		return Channel{}, err
	}
	if clearHistory {
		if _, err := tx.Exec(`DELETE FROM assistant_messages WHERE channel_id = ?`, cleanID); err != nil {
			return Channel{}, err
		}
		if _, err := tx.Exec(
			`UPDATE assistant_approvals SET status = 'cancelled', decided_at = ? WHERE channel_id = ? AND status = 'pending'`,
			now, cleanID,
		); err != nil {
			return Channel{}, err
		}
	}
	var ch Channel
	if err := tx.QueryRow(
		`SELECT id, title, description, model, context_epoch, created_at, updated_at FROM assistant_channels WHERE id = ?`, cleanID,
	).Scan(&ch.ID, &ch.Title, &ch.Description, &ch.Model, &ch.ContextEpoch, &ch.CreatedAt, &ch.UpdatedAt); err != nil {
		return Channel{}, err
	}
	if err := tx.Commit(); err != nil {
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
			tool_calls_json, tool_call_id, widgets_json, source, external_id, observed, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ChannelID, max(m.ContextEpoch, 1), m.Role, m.Content, m.Model,
		m.PromptTokens, m.CachedTokens, m.CompletionTokens, m.CostUSD,
		m.ToolCallsJSON, m.ToolCallID, widgetsRaw, m.Source, m.ExternalID, obs, m.CreatedAt,
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

// UpdateMessageContent updates the text content of an existing stored message.
func (s *Store) UpdateMessageContent(id int64, content string) error {
	if id <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE assistant_messages SET content = ? WHERE id = ?`, content, id)
	return err
}

func scanMessage(rows *sql.Rows) (Message, error) {
	var m Message
	var widgetsRaw string
	var obs int
	if err := rows.Scan(
		&m.ID, &m.ChannelID, &m.ContextEpoch, &m.Role, &m.Content, &m.Model,
		&m.PromptTokens, &m.CachedTokens, &m.CompletionTokens, &m.CostUSD,
		&m.ToolCallsJSON, &m.ToolCallID, &widgetsRaw, &m.Source, &m.ExternalID, &obs, &m.CreatedAt,
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
		        tool_calls_json, tool_call_id, widgets_json, source, external_id, observed, created_at
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
		        tool_calls_json, tool_call_id, widgets_json, source, external_id, observed, created_at
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
	        tool_calls_json, tool_call_id, widgets_json, source, external_id, observed, created_at
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

func parseCronField(field string, minVal, maxVal int, wrapSeven bool) (map[int]bool, error) {
	allowed := make(map[int]bool)
	for part := range strings.SplitSeq(strings.TrimSpace(field), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty cron segment in %q", field)
		}
		base, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(strings.TrimSpace(stepStr))
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("invalid cron step %q", part)
			}
			step = n
		}
		lo, hi, err := parseCronRange(base, minVal, maxVal, wrapSeven)
		if err != nil {
			return nil, err
		}
		for v := lo; v <= hi; v += step {
			val := v
			if wrapSeven && val == 7 {
				val = 0
			}
			allowed[val] = true
		}
	}
	return allowed, nil
}

func parseCronRange(base string, minVal, maxVal int, wrapSeven bool) (int, int, error) {
	if base == "*" {
		return minVal, maxVal, nil
	}
	hiMax := maxVal
	if wrapSeven && maxVal < 7 {
		hiMax = 7
	}
	loStr, hiStr, isRange := strings.Cut(base, "-")
	lo, err := strconv.Atoi(strings.TrimSpace(loStr))
	if err != nil || lo < minVal || lo > hiMax {
		return 0, 0, fmt.Errorf("invalid cron value %q", base)
	}
	if !isRange {
		return lo, lo, nil
	}
	hi, err := strconv.Atoi(strings.TrimSpace(hiStr))
	if err != nil || hi < lo || hi > hiMax {
		return 0, 0, fmt.Errorf("invalid cron range %q", base)
	}
	return lo, hi, nil
}

// NextCronRun computes the next matching timestamp strictly after `after` (in after's location) for a 5-field cron expression.
func NextCronRun(expr string, after time.Time) (time.Time, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return time.Time{}, newErr("cron_expr must have 5 space-separated fields (min hour dom mon dow)", http.StatusBadRequest)
	}
	mins, err := parseCronField(fields[0], 0, 59, false)
	if err != nil {
		return time.Time{}, newErr(err.Error(), http.StatusBadRequest)
	}
	hrs, err := parseCronField(fields[1], 0, 23, false)
	if err != nil {
		return time.Time{}, newErr(err.Error(), http.StatusBadRequest)
	}
	doms, err := parseCronField(fields[2], 1, 31, false)
	if err != nil {
		return time.Time{}, newErr(err.Error(), http.StatusBadRequest)
	}
	mons, err := parseCronField(fields[3], 1, 12, false)
	if err != nil {
		return time.Time{}, newErr(err.Error(), http.StatusBadRequest)
	}
	dows, err := parseCronField(fields[4], 0, 6, true)
	if err != nil {
		return time.Time{}, newErr(err.Error(), http.StatusBadRequest)
	}
	cand := after.Truncate(time.Minute).Add(time.Minute)
	limit := cand.Add(366 * 24 * time.Hour)
	for !cand.After(limit) {
		if mons[int(cand.Month())] && doms[cand.Day()] && dows[int(cand.Weekday())] && hrs[cand.Hour()] && mins[cand.Minute()] {
			return cand.UTC(), nil
		}
		cand = cand.Add(time.Minute)
	}
	return time.Time{}, newErr("cron_expr did not match any date within 366 days", http.StatusBadRequest)
}

// UpsertTimer creates or updates a scheduled timer.
func (s *Store) UpsertTimer(t Timer) (Timer, error) {
	now := registry.NowISO()
	if t.ID == "" {
		t.ID = randomHexID("tmr")
	}
	t.ChannelID = cmp.Or(strings.TrimSpace(t.ChannelID), "briefing")
	t.ScheduleKind = cmp.Or(strings.TrimSpace(t.ScheduleKind), "interval")
	if t.ScheduleKind == "cron" {
		next, err := NextCronRun(t.CronExpr, time.Now())
		if err != nil {
			return Timer{}, err
		}
		t.NextRunAt = next.Format("2006-01-02T15:04:05+00:00")
	} else if t.NextRunAt == "" {
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

	s.mu.Lock()
	defer s.mu.Unlock()
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
	nowISO := now.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05+00:00")
	enabled := 1
	nextRun := ""
	switch {
	case t.ScheduleKind == "cron":
		next, err := NextCronRun(t.CronExpr, now.Local())
		if err != nil {
			enabled = 0
		} else {
			nextRun = next.Format("2006-01-02T15:04:05+00:00")
		}
	case t.ScheduleKind == "once" || t.IntervalSeconds <= 0:
		enabled = 0
	default:
		nextRun = now.UTC().Add(time.Duration(t.IntervalSeconds) * time.Second).Truncate(time.Second).Format("2006-01-02T15:04:05+00:00")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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

// CancelPendingApprovalsByTool cancels pending approvals for a given tool on a channel.
func (s *Store) CancelPendingApprovalsByTool(channelID, toolName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`UPDATE assistant_approvals SET status = 'cancelled', decided_at = ?
		 WHERE channel_id = ? AND tool_name = ? AND status = 'pending'`,
		registry.NowISO(), strings.TrimSpace(channelID), strings.TrimSpace(toolName),
	)
	return err
}

// GetMessageModel returns the recorded model slug for a stored message ID.
func (s *Store) GetMessageModel(id int64) string {
	if id <= 0 {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var model string
	_ = s.db.QueryRow(`SELECT model FROM assistant_messages WHERE id = ?`, id).Scan(&model)
	return strings.TrimSpace(model)
}

// DailySpendUSD returns total assistant message cost_usd recorded at or after sinceISO.
func (s *Store) DailySpendUSD(sinceISO string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total float64
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(cost_usd), 0) FROM assistant_messages WHERE created_at >= ?`, sinceISO).Scan(&total)
	return total
}

// UpsertFrontierGrant saves or replaces a topic-scoped frontier auto-approval grant for a channel.
func (s *Store) UpsertFrontierGrant(g FrontierGrant) (FrontierGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := registry.NowISO()
	g.ChannelID = cmp.Or(strings.TrimSpace(g.ChannelID), "general")
	g.ContextEpoch = max(g.ContextEpoch, 1)
	g.MaxTurns = max(g.MaxTurns, 1)
	if g.MaxCostUSD <= 0 {
		g.MaxCostUSD = DefaultFrontierGrantMaxCostUSD
	}
	g.CreatedAt = cmp.Or(g.CreatedAt, now)
	g.UpdatedAt = now
	if _, err := s.db.Exec(
		`INSERT INTO assistant_frontier_grants(
			channel_id, context_epoch, topic_summary, turns_used, max_turns,
			cost_usd, max_cost_usd, expires_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(channel_id) DO UPDATE SET
			context_epoch = excluded.context_epoch,
			topic_summary = excluded.topic_summary,
			turns_used = excluded.turns_used,
			max_turns = excluded.max_turns,
			cost_usd = excluded.cost_usd,
			max_cost_usd = excluded.max_cost_usd,
			expires_at = excluded.expires_at,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at`,
		g.ChannelID, g.ContextEpoch, g.TopicSummary, g.TurnsUsed, g.MaxTurns,
		g.CostUSD, g.MaxCostUSD, g.ExpiresAt, g.CreatedAt, g.UpdatedAt,
	); err != nil {
		return FrontierGrant{}, err
	}
	return g, nil
}

// GetActiveFrontierGrant returns the active frontier topic grant if valid for epoch, time, turns, and cost.
func (s *Store) GetActiveFrontierGrant(channelID string, epoch int, now time.Time) (*FrontierGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var g FrontierGrant
	err := s.db.QueryRow(
		`SELECT channel_id, context_epoch, topic_summary, turns_used, max_turns,
		        cost_usd, max_cost_usd, expires_at, created_at, updated_at
		 FROM assistant_frontier_grants WHERE channel_id = ?`,
		strings.TrimSpace(channelID),
	).Scan(
		&g.ChannelID, &g.ContextEpoch, &g.TopicSummary, &g.TurnsUsed, &g.MaxTurns,
		&g.CostUSD, &g.MaxCostUSD, &g.ExpiresAt, &g.CreatedAt, &g.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if g.ContextEpoch != max(epoch, 1) || g.TurnsUsed >= g.MaxTurns || g.CostUSD >= g.MaxCostUSD {
		return nil, nil
	}
	exp, pErr := time.Parse(time.RFC3339, g.ExpiresAt)
	if pErr != nil || !now.UTC().Before(exp.UTC()) {
		return nil, nil
	}
	return &g, nil
}

// RecordFrontierGrantUsage increments turns_used and cost_usd and slides expires_at forward.
func (s *Store) RecordFrontierGrantUsage(channelID string, addCostUSD float64, newExpiresAt string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`UPDATE assistant_frontier_grants
		 SET turns_used = turns_used + 1,
		     cost_usd = cost_usd + ?,
		     expires_at = CASE WHEN ? != '' THEN ? ELSE expires_at END,
		     updated_at = ?
		 WHERE channel_id = ?`,
		max(addCostUSD, 0.0), newExpiresAt, newExpiresAt, registry.NowISO(), strings.TrimSpace(channelID),
	)
	return err
}

// DeleteFrontierGrant removes any frontier topic grant for channelID.
func (s *Store) DeleteFrontierGrant(channelID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM assistant_frontier_grants WHERE channel_id = ?`, strings.TrimSpace(channelID))
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

// ListOutbox returns recent proactive notifications.
func (s *Store) ListOutbox(limit int, undeliveredOnly bool) ([]OutboxItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `SELECT id, channel_id, priority, title, body, delivered, created_at, delivered_at FROM assistant_outbox`
	if undeliveredOnly {
		query += ` WHERE delivered = 0`
	}
	query += ` ORDER BY id DESC LIMIT ?`
	rows, err := s.db.Query(query, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []OutboxItem
	for rows.Next() {
		var it OutboxItem
		var del int
		if err := rows.Scan(&it.ID, &it.ChannelID, &it.Priority, &it.Title, &it.Body, &del, &it.CreatedAt, &it.DeliveredAt); err != nil {
			return nil, err
		}
		it.Delivered = del != 0
		out = append(out, it)
	}
	return out, rows.Err()
}

// MarkOutboxDelivered marks an outbox notification as delivered.
func (s *Store) MarkOutboxDelivered(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE assistant_outbox SET delivered = 1, delivered_at = ? WHERE id = ?`, registry.NowISO(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return newErr(fmt.Sprintf("outbox item %d not found", id), http.StatusNotFound)
	}
	return nil
}

// CreateDelegation persists a newly spawned coding agent delegation.
func (s *Store) CreateDelegation(d Delegation) (Delegation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := registry.NowISO()
	if d.ID == "" {
		d.ID = randomHexID("del")
	}
	d.ChannelID = cmp.Or(strings.TrimSpace(d.ChannelID), "general")
	d.Status = cmp.Or(strings.TrimSpace(d.Status), "queued")
	d.CreatedAt = cmp.Or(d.CreatedAt, now)
	d.UpdatedAt = now
	_, err := s.db.Exec(
		`INSERT INTO assistant_delegations(
			id, job_id, channel_id, target_device, harness, session_name, session_id,
			repo_path, prompt, status, last_notified_state, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.JobID, d.ChannelID, d.TargetDevice, d.Harness, d.SessionName, d.SessionID,
		d.RepoPath, d.Prompt, d.Status, d.LastNotifiedState, d.CreatedAt, d.UpdatedAt,
	)
	return d, err
}

func scanDelegation(scanner interface{ Scan(...any) error }) (Delegation, error) {
	var d Delegation
	err := scanner.Scan(
		&d.ID, &d.JobID, &d.ChannelID, &d.TargetDevice, &d.Harness, &d.SessionName, &d.SessionID,
		&d.RepoPath, &d.Prompt, &d.Status, &d.LastNotifiedState, &d.CreatedAt, &d.UpdatedAt,
	)
	return d, err
}

// ListActiveDelegations returns non-terminal delegations (not exited/failed).
func (s *Store) ListActiveDelegations() ([]Delegation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT id, job_id, channel_id, target_device, harness, session_name, session_id,
		        repo_path, prompt, status, last_notified_state, created_at, updated_at
		 FROM assistant_delegations WHERE status NOT IN ('exited', 'failed') ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Delegation
	for rows.Next() {
		d, err := scanDelegation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListDelegations returns recent delegations ordered newest first.
func (s *Store) ListDelegations(limit int) ([]Delegation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT id, job_id, channel_id, target_device, harness, session_name, session_id,
		        repo_path, prompt, status, last_notified_state, created_at, updated_at
		 FROM assistant_delegations ORDER BY created_at DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Delegation
	for rows.Next() {
		d, err := scanDelegation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpdateDelegationState updates session_id, status, and last_notified_state on a delegation.
func (s *Store) UpdateDelegationState(id, sessionID, status, lastNotified string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`UPDATE assistant_delegations
		 SET session_id = CASE WHEN ? != '' THEN ? ELSE session_id END,
		     status = CASE WHEN ? != '' THEN ? ELSE status END,
		     last_notified_state = CASE
		         WHEN ? != '' THEN ?
		         WHEN ? = 'working' THEN ''
		         ELSE last_notified_state
		     END,
		     updated_at = ?
		 WHERE id = ?`,
		sessionID, sessionID, status, status, lastNotified, lastNotified, status, registry.NowISO(), id,
	)
	return err
}

// HasExternalMessage reports whether a message from source with externalID has already been ingested.
func (s *Store) HasExternalMessage(source, externalID string) bool {
	if strings.TrimSpace(source) == "" || strings.TrimSpace(externalID) == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM assistant_messages WHERE source = ? AND external_id = ?`, source, externalID).Scan(&n)
	return n > 0
}

// GetDiscordCursor returns the last processed Discord message snowflake for channelID.
func (s *Store) GetDiscordCursor(channelID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var lastID string
	err := s.db.QueryRow(`SELECT last_message_id FROM assistant_discord_state WHERE channel_id = ?`, channelID).Scan(&lastID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return lastID, err
}

// SetDiscordCursor saves the latest processed Discord message snowflake for channelID.
func (s *Store) SetDiscordCursor(channelID, discordChannelID, lastMessageID string) error {
	if strings.TrimSpace(channelID) == "" || strings.TrimSpace(lastMessageID) == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO assistant_discord_state(channel_id, discord_channel_id, last_message_id, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(channel_id) DO UPDATE SET
		   discord_channel_id = excluded.discord_channel_id,
		   last_message_id = excluded.last_message_id,
		   updated_at = excluded.updated_at`,
		channelID, discordChannelID, lastMessageID, registry.NowISO(),
	)
	return err
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

func decorateAssistantFile(f AssistantFile) AssistantFile {
	f.URL = "/v1/assistant/files/" + f.ID
	f.IsImage = isSafeInlineImageMIME(f.MimeType)
	return f
}

func scanAssistantFile(scanner interface{ Scan(...any) error }) (AssistantFile, error) {
	var f AssistantFile
	err := scanner.Scan(&f.ID, &f.ChannelID, &f.MessageID, &f.Direction, &f.Name, &f.MimeType, &f.SizeBytes, &f.StoragePath, &f.CreatedAt)
	if err != nil {
		return AssistantFile{}, err
	}
	return decorateAssistantFile(f), nil
}

// CreateFile persists an uploaded or generated file record in assistant_files.
func (s *Store) CreateFile(f AssistantFile) (AssistantFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.ID == "" {
		f.ID = randomHexID("file")
	}
	f.ChannelID = cmp.Or(strings.TrimSpace(f.ChannelID), "general")
	f.Direction = cmp.Or(strings.TrimSpace(f.Direction), "upload")
	f.MimeType = cmp.Or(strings.TrimSpace(f.MimeType), "application/octet-stream")
	if f.CreatedAt == "" {
		f.CreatedAt = registry.NowISO()
	}
	_, err := s.db.Exec(
		`INSERT INTO assistant_files(id, channel_id, message_id, direction, name, mime_type, size_bytes, storage_path, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.ID, f.ChannelID, f.MessageID, f.Direction, f.Name, f.MimeType, f.SizeBytes, f.StoragePath, f.CreatedAt,
	)
	if err != nil {
		return AssistantFile{}, err
	}
	return decorateAssistantFile(f), nil
}

// GetFile returns one AssistantFile by ID.
func (s *Store) GetFile(id string) (*AssistantFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := scanAssistantFile(s.db.QueryRow(
		`SELECT id, channel_id, message_id, direction, name, mime_type, size_bytes, storage_path, created_at
		 FROM assistant_files WHERE id = ?`, strings.TrimSpace(id),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, newErr(fmt.Sprintf("file %q not found", id), http.StatusNotFound)
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// ListChannelFiles returns recent files associated with channelID.
func (s *Store) ListChannelFiles(channelID string, limit int) ([]AssistantFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT id, channel_id, message_id, direction, name, mime_type, size_bytes, storage_path, created_at
		 FROM assistant_files WHERE channel_id = ? ORDER BY created_at DESC LIMIT ?`,
		cmp.Or(strings.TrimSpace(channelID), "general"), limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AssistantFile
	for rows.Next() {
		f, scanErr := scanAssistantFile(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// AttachFilesToMessage links pending uploaded file IDs to msgID.
func (s *Store) AttachFilesToMessage(channelID string, msgID int64, fileIDs []string) error {
	if msgID <= 0 || len(fileIDs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cleanCh := cmp.Or(strings.TrimSpace(channelID), "general")
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range fileIDs {
		cleanID := strings.TrimSpace(id)
		if cleanID == "" {
			continue
		}
		if _, err := tx.Exec(
			`UPDATE assistant_files SET message_id = ? WHERE id = ? AND channel_id = ? AND message_id = 0`,
			msgID, cleanID, cleanCh,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetMCPOAuthToken loads the stored OAuth token record for serverName.
func (s *Store) GetMCPOAuthToken(serverName string) (MCPOAuthToken, bool, error) {
	clean := strings.ToLower(strings.TrimSpace(serverName))
	if clean == "" {
		return MCPOAuthToken{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var tok MCPOAuthToken
	err := s.db.QueryRow(
		`SELECT server_name, access_token, refresh_token, token_type, scope, expires_at, token_url, updated_at
		 FROM assistant_mcp_oauth_tokens WHERE server_name = ?`,
		clean,
	).Scan(&tok.ServerName, &tok.AccessToken, &tok.RefreshToken, &tok.TokenType, &tok.Scope, &tok.ExpiresAt, &tok.TokenURL, &tok.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MCPOAuthToken{}, false, nil
	}
	if err != nil {
		return MCPOAuthToken{}, false, err
	}
	return tok, true, nil
}

// UpsertMCPOAuthToken saves or updates the OAuth token record for an HTTP MCP server.
func (s *Store) UpsertMCPOAuthToken(tok MCPOAuthToken) error {
	clean := strings.ToLower(strings.TrimSpace(tok.ServerName))
	if clean == "" {
		return errors.New("server_name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO assistant_mcp_oauth_tokens(server_name, access_token, refresh_token, token_type, scope, expires_at, token_url, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(server_name) DO UPDATE SET
		   access_token = excluded.access_token,
		   refresh_token = excluded.refresh_token,
		   token_type = excluded.token_type,
		   scope = excluded.scope,
		   expires_at = excluded.expires_at,
		   token_url = excluded.token_url,
		   updated_at = excluded.updated_at`,
		clean,
		strings.TrimSpace(tok.AccessToken),
		strings.TrimSpace(tok.RefreshToken),
		cmp.Or(strings.TrimSpace(tok.TokenType), "Bearer"),
		strings.TrimSpace(tok.Scope),
		strings.TrimSpace(tok.ExpiresAt),
		strings.TrimSpace(tok.TokenURL),
		registry.NowISO(),
	)
	return err
}

// DeleteMCPOAuthToken removes any stored OAuth token for serverName.
func (s *Store) DeleteMCPOAuthToken(serverName string) error {
	clean := strings.ToLower(strings.TrimSpace(serverName))
	if clean == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM assistant_mcp_oauth_tokens WHERE server_name = ?`, clean)
	return err
}
