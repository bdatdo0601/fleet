// Package events implements the authoritative Fleet event log and pub/sub bus (events.db).
package events

import (
	"cmp"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bdatdo0601/fleet/internal/registry"
)

const schema = `
CREATE TABLE IF NOT EXISTS fleet_events (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  type         TEXT NOT NULL,
  severity     TEXT NOT NULL DEFAULT 'info',
  source       TEXT NOT NULL,
  actor        TEXT NOT NULL DEFAULT '',
  subject      TEXT NOT NULL DEFAULT '',
  channel_hint TEXT NOT NULL DEFAULT '',
  dedupe_key   TEXT NOT NULL UNIQUE,
  title        TEXT NOT NULL DEFAULT '',
  summary      TEXT NOT NULL DEFAULT '',
  payload_json TEXT NOT NULL DEFAULT '{}',
  created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_fleet_events_type ON fleet_events(type, id DESC);
CREATE INDEX IF NOT EXISTS idx_fleet_events_created ON fleet_events(created_at);
`

const (
	// RetentionDays is the age after which historical events are pruned from events.db.
	RetentionDays = 14
	pruneInterval = time.Hour
)

var (
	validSeverities = []string{"info", "warn", "error", "critical"}
	errBusClosed    = errors.New("events bus is closed")
)

// Event represents one authoritative Fleet lifecycle, ops, or external event.
type Event struct {
	ID          int64          `json:"id"`
	Type        string         `json:"type"`
	Severity    string         `json:"severity"`
	Source      string         `json:"source"`
	Actor       string         `json:"actor,omitempty"`
	Subject     string         `json:"subject,omitempty"`
	ChannelHint string         `json:"channel_hint,omitempty"`
	DedupeKey   string         `json:"dedupe_key"`
	Title       string         `json:"title,omitempty"`
	Summary     string         `json:"summary,omitempty"`
	PayloadJSON string         `json:"-"`
	Payload     map[string]any `json:"payload,omitempty"`
	CreatedAt   string         `json:"created_at"`
}

// Filter scopes Bus.List queries.
type Filter struct {
	TypePrefix string
	Severity   string
	Source     string
	Subject    string
	SinceID    int64
	Limit      int
}

// Bus persists authoritative events in SQLite and fans out new events to live subscribers.
type Bus struct {
	Path      string
	mu        sync.Mutex
	db        *sql.DB
	lastPrune time.Time

	subMu       sync.Mutex
	subscribers map[chan Event]struct{}
}

// Open initializes events.db at path with WAL mode and 0600 file permissions.
func Open(path string) (*Bus, error) {
	db, err := registry.OpenSQLite(path, schema, "", nil)
	if err != nil {
		return nil, err
	}
	if path != "" && path != ":memory:" && !strings.HasPrefix(path, "file:") {
		if err := os.Chmod(path, 0o600); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return &Bus{
		Path:        path,
		db:          db,
		subscribers: make(map[chan Event]struct{}),
	}, nil
}

// Close closes the event database and drops subscriber registrations.
func (b *Bus) Close() error {
	b.mu.Lock()
	if b.db == nil {
		b.mu.Unlock()
		return nil
	}
	err := b.db.Close()
	b.db = nil
	b.mu.Unlock()

	b.subMu.Lock()
	clear(b.subscribers)
	b.subMu.Unlock()
	return err
}

func randomDedupeKey() string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return "ev-" + hex.EncodeToString(buf[:])
}

// NormalizeSeverity maps raw severity strings into info|warn|error|critical.
func NormalizeSeverity(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if slices.Contains(validSeverities, s) {
		return s
	}
	if s == "warning" {
		return "warn"
	}
	return "info"
}

func normalizeEvent(ev Event) (Event, error) {
	ev.Type = strings.TrimSpace(ev.Type)
	if ev.Type == "" {
		return Event{}, errors.New("event type is required")
	}
	ev.Severity = NormalizeSeverity(ev.Severity)
	ev.Source = cmp.Or(strings.TrimSpace(ev.Source), "fleet")
	ev.Actor = strings.TrimSpace(ev.Actor)
	ev.Subject = strings.TrimSpace(ev.Subject)
	ev.ChannelHint = strings.TrimSpace(ev.ChannelHint)
	ev.DedupeKey = cmp.Or(strings.TrimSpace(ev.DedupeKey), randomDedupeKey())
	ev.Title = strings.TrimSpace(ev.Title)
	ev.Summary = strings.TrimSpace(ev.Summary)
	ev.CreatedAt = cmp.Or(strings.TrimSpace(ev.CreatedAt), registry.NowISO())
	switch {
	case ev.Payload != nil:
		raw, err := json.Marshal(ev.Payload)
		if err != nil {
			return Event{}, fmt.Errorf("marshal event payload: %w", err)
		}
		ev.PayloadJSON = string(raw)
	case strings.TrimSpace(ev.PayloadJSON) == "":
		ev.PayloadJSON = "{}"
	default:
		var m map[string]any
		if json.Unmarshal([]byte(ev.PayloadJSON), &m) == nil {
			ev.Payload = m
		}
	}
	return ev, nil
}

func (b *Bus) pruneOldLocked(now time.Time) {
	if !b.lastPrune.IsZero() && now.Sub(b.lastPrune) < pruneInterval {
		return
	}
	b.lastPrune = now
	cutoff := now.UTC().Add(-RetentionDays * 24 * time.Hour).Truncate(time.Second).Format("2006-01-02T15:04:05+00:00")
	_, _ = b.db.Exec(`DELETE FROM fleet_events WHERE created_at < ?`, cutoff)
}

// Publish persists ev (deduplicating on dedupe_key) and notifies subscribers when newly inserted.
func (b *Bus) Publish(ev Event) (Event, bool, error) {
	norm, err := normalizeEvent(ev)
	if err != nil {
		return Event{}, false, err
	}

	b.mu.Lock()
	if b.db == nil {
		b.mu.Unlock()
		return Event{}, false, errBusClosed
	}
	b.pruneOldLocked(time.Now())
	res, err := b.db.Exec(
		`INSERT OR IGNORE INTO fleet_events(
			type, severity, source, actor, subject, channel_hint,
			dedupe_key, title, summary, payload_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		norm.Type, norm.Severity, norm.Source, norm.Actor, norm.Subject, norm.ChannelHint,
		norm.DedupeKey, norm.Title, norm.Summary, norm.PayloadJSON, norm.CreatedAt,
	)
	if err != nil {
		b.mu.Unlock()
		return Event{}, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		b.mu.Unlock()
		return Event{}, false, nil
	}
	id, _ := res.LastInsertId()
	norm.ID = id
	b.mu.Unlock()

	b.broadcast(norm)
	return norm, true, nil
}

func (b *Bus) broadcast(ev Event) {
	b.subMu.Lock()
	defer b.subMu.Unlock()
	for ch := range b.subscribers {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Subscribe registers a buffered channel that receives newly published events.
func (b *Bus) Subscribe() chan Event {
	ch := make(chan Event, 128)
	b.subMu.Lock()
	b.subscribers[ch] = struct{}{}
	b.subMu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber channel registered via Subscribe.
func (b *Bus) Unsubscribe(ch chan Event) {
	if ch == nil {
		return
	}
	b.subMu.Lock()
	delete(b.subscribers, ch)
	b.subMu.Unlock()
}

func scanEvent(scanner interface{ Scan(...any) error }) (Event, error) {
	var ev Event
	if err := scanner.Scan(
		&ev.ID, &ev.Type, &ev.Severity, &ev.Source, &ev.Actor, &ev.Subject,
		&ev.ChannelHint, &ev.DedupeKey, &ev.Title, &ev.Summary, &ev.PayloadJSON, &ev.CreatedAt,
	); err != nil {
		return Event{}, err
	}
	if strings.TrimSpace(ev.PayloadJSON) != "" && ev.PayloadJSON != "{}" {
		var m map[string]any
		if json.Unmarshal([]byte(ev.PayloadJSON), &m) == nil && len(m) > 0 {
			ev.Payload = m
		}
	}
	return ev, nil
}

// List returns stored events matching f. When SinceID > 0, events are returned oldest-first; otherwise newest-first.
func (b *Bus) List(f Filter) ([]Event, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 200)

	var clauses []string
	var args []any
	if tp := strings.TrimSpace(f.TypePrefix); tp != "" {
		clauses = append(clauses, "type LIKE ?")
		args = append(args, tp+"%")
	}
	if sev := strings.TrimSpace(f.Severity); sev != "" {
		clauses = append(clauses, "severity = ?")
		args = append(args, NormalizeSeverity(sev))
	}
	if src := strings.TrimSpace(f.Source); src != "" {
		clauses = append(clauses, "source = ?")
		args = append(args, src)
	}
	if sub := strings.TrimSpace(f.Subject); sub != "" {
		clauses = append(clauses, "subject = ?")
		args = append(args, sub)
	}
	order := "DESC"
	if f.SinceID > 0 {
		clauses = append(clauses, "id > ?")
		args = append(args, f.SinceID)
		order = "ASC"
	}
	query := `SELECT id, type, severity, source, actor, subject, channel_hint, dedupe_key, title, summary, payload_json, created_at FROM fleet_events`
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY id " + order + " LIMIT ?"
	args = append(args, limit)

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.db == nil {
		return nil, errBusClosed
	}
	rows, err := b.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Event{}
	for rows.Next() {
		ev, sErr := scanEvent(rows)
		if sErr != nil {
			return nil, sErr
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
