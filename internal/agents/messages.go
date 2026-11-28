package agents

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/registry"
)

const msgSelectSQL = "SELECT m.id, m.thread_id, m.reply_to, m.from_session, m.from_device, " +
	"COALESCE(s.name, '') AS from_name, COALESCE(s.title, '') AS from_title, " +
	"m.to_device, m.to_session, m.to_repo, m.body, m.created_at " +
	"FROM agent_messages m LEFT JOIN agent_sessions s ON s.session_id = m.from_session "

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

func (h *Hub) fetchInboxFilteredLocked(device, sessionID string, unreadOnly, markRead, directOnly bool, limit int) ([]Message, error) {
	if !strings.HasPrefix(sessionID, device+":") {
		return nil, newAgentErr("session belongs to another device", 403)
	}
	limit = max(1, min(limit, 200))
	unreadFlag := 0
	if unreadOnly {
		unreadFlag = 1
	}
	var query string
	var args []any
	if directOnly {
		query = msgSelectSQL + "WHERE m.from_session != ? AND m.to_session = ? " +
			"AND (? = 0 OR m.id NOT IN (SELECT message_id FROM agent_reads WHERE session_id = ?)) " +
			"ORDER BY m.id ASC LIMIT ?"
		args = []any{sessionID, sessionID, unreadFlag, sessionID, limit}
	} else {
		var repo, startedAt string
		err := h.db.QueryRow("SELECT repo, started_at FROM agent_sessions WHERE session_id = ?", sessionID).Scan(&repo, &startedAt)
		if errors.Is(err, sql.ErrNoRows) {
			repo = ""
			startedAt = "1970-01-01T00:00:00+00:00"
		} else if err != nil {
			return nil, err
		}
		query = msgSelectSQL + "WHERE m.from_session != ? " +
			"AND (m.to_session = ? OR (m.to_session IS NULL " +
			"  AND (m.to_device IS NULL OR m.to_device = ?) " +
			"  AND (m.to_repo IS NULL OR (m.to_repo = ? AND ? != '')) " +
			"  AND m.created_at >= ?)) " +
			"AND (? = 0 OR m.id NOT IN (SELECT message_id FROM agent_reads WHERE session_id = ?)) " +
			"ORDER BY m.id ASC LIMIT ?"
		args = []any{sessionID, sessionID, device, repo, repo, startedAt, unreadFlag, sessionID, limit}
	}
	messages, err := h.queryMessagesLocked(query, args...)
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

func (h *Hub) fetchInboxLocked(device, sessionID string, unreadOnly, markRead bool, limit int) ([]Message, error) {
	return h.fetchInboxFilteredLocked(device, sessionID, unreadOnly, markRead, false, limit)
}

// FetchInbox returns incoming messages for sessionID.
func (h *Hub) FetchInbox(device, sessionID string, unreadOnly, markRead bool, limit int) ([]Message, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.checkOpenLocked(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	return h.fetchInboxLocked(device, sessionID, unreadOnly, markRead, limit)
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
	if strings.HasPrefix(sid, AssistantSessionPrefix) {
		return DeliveryReport{Delivery: "immediate", WokeWaiters: 1}, nil
	}
	row, err := scanSessionListRow(h.db.QueryRow("SELECT "+sessionListCols+" FROM agent_sessions WHERE session_id = ?", sid))
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
	if h.waiters[sid] > 0 || h.harnessRelays[deviceHarnessKey(sid)] > 0 {
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
	rows, err := h.db.Query("SELECT "+sessionListCols+" FROM agent_sessions WHERE session_id != ? AND state != 'exited' ORDER BY updated_at DESC", msg.FromSession)
	if err != nil {
		return DeliveryReport{}, err
	}
	allRows, err := scanAllSessionListRows(rows)
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
		if key := deviceHarnessKey(*toSession); key != "" {
			h.harnessSubs.notify(key)
		}
	} else {
		h.notifyBroadcastLocked(fromSession, toDevice)
	}
	h.threadSubs.notify(resolvedThread)
	return msg, report, nil
}

// SendMessageWithReport sends a message and returns the message along with delivery diagnostics.
func (h *Hub) SendMessageWithReport(device, fromSession, to, body string, threadID *string, replyTo *int64, knownDevices map[string]bool) (SendResult, error) {
	h.mu.Lock()
	if err := h.checkOpenLocked(); err != nil {
		h.mu.Unlock()
		return SendResult{}, err
	}
	msg, report, err := h.sendMessageAndReportLocked(device, fromSession, to, body, threadID, replyTo, knownDevices)
	cb := h.onAssistantMessage
	h.mu.Unlock()
	if err != nil {
		return SendResult{}, err
	}
	if cb != nil && msg.ToSession != nil && strings.HasPrefix(*msg.ToSession, AssistantSessionPrefix) {
		cb(msg)
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
	if err := h.checkOpenLocked(); err != nil {
		return nil, err
	}
	return h.queryMessagesLocked(q, args...)
}

// AckMessages marks messageIDs as read for sessionID.
func (h *Hub) AckMessages(device, sessionID string, messageIDs []int64) (int, error) {
	if !strings.HasPrefix(sessionID, device+":") {
		return 0, newAgentErr("session belongs to another device", 403)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.checkOpenLocked(); err != nil {
		return 0, err
	}
	if err := h.markReadLocked(sessionID, messageIDs...); err != nil {
		return 0, err
	}
	return len(messageIDs), nil
}
