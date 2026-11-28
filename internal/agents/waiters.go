package agents

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/registry"
)

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

func (h *Hub) isListeningLocked(sessionID, state string, live bool, now time.Time) bool {
	if !live || state == "exited" {
		return false
	}
	if h.waiters[sessionID] > 0 {
		return true
	}
	if seen, ok := h.waitSeenAt[sessionID]; ok && state == "working" && now.Sub(seen).Seconds() <= WaiterRearmGraceSeconds {
		return true
	}
	if key := deviceHarnessKey(sessionID); key != "" {
		if h.harnessRelays[key] > 0 {
			return true
		}
		if seen, ok := h.harnessRelaySeenAt[key]; ok && now.Sub(seen).Seconds() <= WaiterRearmGraceSeconds {
			return true
		}
	}
	return false
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
		if h.closed || h.db == nil {
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

type harnessBatchCand struct {
	sessionID string
	fullRawID string
	pid       *int
	name      string
	title     string
	state     string
	updatedAt string
}

func (h *Hub) listHarnessBatchCandsLocked(p HarnessWaitParams) ([]harnessBatchCand, error) {
	rows, err := h.db.Query(
		"SELECT session_id, full_raw_id, pid, name, title, state, updated_at FROM agent_sessions "+
			"WHERE device = ? AND harness = ? AND state != 'exited' ORDER BY updated_at DESC",
		p.Device, p.Harness,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var cands []harnessBatchCand
	for rows.Next() {
		var c harnessBatchCand
		if sErr := rows.Scan(&c.sessionID, &c.fullRawID, &c.pid, &c.name, &c.title, &c.state, &c.updatedAt); sErr != nil {
			return nil, sErr
		}
		cands = append(cands, c)
	}
	return cands, rows.Err()
}

func (h *Hub) collectHarnessBatchesLocked(p HarnessWaitParams, excluded map[string]bool, limit int) ([]SessionMessages, error) {
	cands, err := h.listHarnessBatchCandsLocked(p)
	if err != nil {
		return nil, err
	}

	nowUTC := time.Now().UTC().Truncate(time.Second)
	var batches []SessionMessages
	for _, c := range cands {
		if excluded[c.sessionID] {
			continue
		}
		if p.RequireFullID && (c.fullRawID == "" || strings.HasPrefix(c.fullRawID, "pid-")) {
			continue
		}
		age := max(int(nowUTC.Sub(parseTS(c.updatedAt)).Seconds()), 0)
		live := isLiveRow(c.state, age)
		msgs, err := h.fetchInboxFilteredLocked(p.Device, c.sessionID, true, p.MarkRead, !live, limit)
		if err != nil {
			return nil, err
		}
		if len(msgs) > 0 {
			batches = append(batches, SessionMessages{
				SessionID: c.sessionID, FullRawID: c.fullRawID, PID: c.pid, Name: c.name, Title: c.title, Messages: msgs,
			})
		}
	}
	return batches, nil
}

// WaitForHarnessMessages blocks until unread messages arrive for any session matching (Device, Harness),
// timeout elapses, ctx is cancelled, or the hub closes.
func (h *Hub) WaitForHarnessMessages(ctx context.Context, p HarnessWaitParams) (HarnessWaitResult, error) {
	if !paths.ValidHarness(p.Harness) {
		return HarnessWaitResult{}, newAgentErr(fmt.Sprintf("unknown harness %q", p.Harness), 400)
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
	excluded := make(map[string]bool, len(p.ExcludeIDs))
	for _, id := range p.ExcludeIDs {
		if id != "" {
			excluded[id] = true
		}
	}
	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	key := p.Device + ":" + p.Harness

	h.mu.Lock()
	h.harnessRelays[key]++
	subCh := h.harnessSubs.subscribe(key)
	defer func() {
		h.harnessSubs.unsubscribe(key, subCh)
		if c := h.harnessRelays[key] - 1; c <= 0 {
			delete(h.harnessRelays, key)
		} else {
			h.harnessRelays[key] = c
		}
		h.harnessRelaySeenAt[key] = time.Now()
		h.mu.Unlock()
	}()

	for {
		if h.closed || h.db == nil {
			return HarnessWaitResult{Device: p.Device, Harness: p.Harness, Batches: []SessionMessages{}, TimedOut: true}, nil
		}
		batches, err := h.collectHarnessBatchesLocked(p, excluded, limit)
		if err != nil {
			return HarnessWaitResult{}, err
		}
		if len(batches) > 0 {
			return HarnessWaitResult{Device: p.Device, Harness: p.Harness, Batches: batches}, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return HarnessWaitResult{Device: p.Device, Harness: p.Harness, Batches: []SessionMessages{}, TimedOut: true}, nil
		}
		if err := h.waitOnChannelLocked(ctx, subCh, remaining); err != nil {
			return HarnessWaitResult{}, err
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
	if err := h.checkOpenLocked(); err != nil {
		h.mu.Unlock()
		return AskResult{}, err
	}
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
	cb := h.onAssistantMessage
	if cb != nil && sent.ToSession != nil && strings.HasPrefix(*sent.ToSession, AssistantSessionPrefix) {
		h.mu.Unlock()
		cb(sent)
		h.mu.Lock()
	}
	defer func() {
		h.threadSubs.unsubscribe(sent.ThreadID, subCh)
		h.mu.Unlock()
	}()

	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	for {
		if h.closed || h.db == nil {
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
