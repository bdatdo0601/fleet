package agents

import (
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/registry"
)

var defaultWatchEvents = []string{"idle", "waiting_permission", "waiting_question", "exited"}

// StateTransition describes a lifecycle state change observed for an agent session.
type StateTransition struct {
	SessionID     string            `json:"session_id"`
	Device        string            `json:"device"`
	Harness       string            `json:"harness"`
	Name          string            `json:"name"`
	Title         string            `json:"title"`
	OldState      string            `json:"old_state"`
	NewState      string            `json:"new_state"`
	StateChanged  bool              `json:"state_changed"`
	PendingAction map[string]string `json:"pending_action,omitempty"`
	Summary       string            `json:"summary,omitempty"`
	GitStatus     string            `json:"git_status,omitempty"`
	LastAssistant string            `json:"last_assistant,omitempty"`
	ChangedAt     string            `json:"changed_at"`
}

// Watch represents a persistent subscription pushing state transitions to a watching session's inbox.
type Watch struct {
	ID             string   `json:"id"`
	WatcherSession string   `json:"watcher_session"`
	WatcherDevice  string   `json:"watcher_device"`
	TargetSession  string   `json:"target_session"`
	TargetDevice   string   `json:"target_device"`
	TargetName     string   `json:"target_name"`
	Events         []string `json:"events"`
	Once           bool     `json:"once"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
}

// CreateWatchParams configures CreateWatch.
type CreateWatchParams struct {
	WatcherDevice  string
	WatcherSession string
	Target         string
	TargetDevice   string
	TargetName     string
	Events         []string
	Once           bool
	AllowPending   bool
}

// WatchWaitParams configures WaitForSessionEvent.
type WatchWaitParams struct {
	Target         string
	TargetDevice   string
	TargetName     string
	Events         []string
	Timeout        float64
	IncludeCurrent bool
	AllowPending   bool
}

// WatchWaitResult is returned by WaitForSessionEvent.
type WatchWaitResult struct {
	Matched  bool             `json:"matched"`
	TimedOut bool             `json:"timed_out"`
	Event    *StateTransition `json:"event,omitempty"`
}

// SetOnTransition registers an optional callback invoked outside Hub's lock on state transitions.
func (h *Hub) SetOnTransition(fn func(StateTransition)) {
	h.mu.Lock()
	h.onTransition = fn
	h.mu.Unlock()
}

// ParseWatchEvents validates and normalizes watch event names, expanding "waiting" and defaulting when empty.
func ParseWatchEvents(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return slices.Clone(defaultWatchEvents), nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(ev string) {
		if !seen[ev] {
			seen[ev] = true
			out = append(out, ev)
		}
	}
	for _, item := range raw {
		for tok := range strings.SplitSeq(item, ",") {
			ev := strings.ToLower(strings.TrimSpace(tok))
			if ev == "" {
				continue
			}
			switch ev {
			case "waiting":
				add("waiting_permission")
				add("waiting_question")
			case "working", "idle", "waiting_permission", "waiting_question", "exited":
				add(ev)
			default:
				return nil, newAgentErr(fmt.Sprintf("unknown watch event %q (expected working, idle, waiting, waiting_permission, waiting_question, or exited)", ev), 400)
			}
		}
	}
	if len(out) == 0 {
		return slices.Clone(defaultWatchEvents), nil
	}
	return out, nil
}

func randomWatchID() string {
	var buf [6]byte
	_, _ = rand.Read(buf[:])
	return "w-" + hex.EncodeToString(buf[:])
}

func lastAssistantFromTail(tail []TailItem) string {
	for _, it := range slices.Backward(tail) {
		if it.Role == "assistant" && strings.TrimSpace(it.Text) != "" {
			return strings.TrimSpace(it.Text)
		}
	}
	return ""
}

func buildHeartbeatTransition(p HeartbeatParams, existing *sessionRow, sessionID, effTitle, effName, newPendingJSON, newStateChangedAt string) StateTransition {
	prevState, prevSummary, prevGitStatus := "", "", ""
	var prevTail []TailItem
	if existing != nil {
		prevState = existing.state
		prevSummary = existing.summary
		prevGitStatus = existing.gitStatus
		prevTail = parseTail(existing.tailJSON)
	}
	effTail := p.Tail
	if effTail == nil {
		effTail = prevTail
	}
	stateChanged := existing == nil || existing.state != p.State || existing.name != effName || (IsWaitingState(p.State) && newPendingJSON != existing.pendingActionJSON)
	return StateTransition{
		SessionID:     sessionID,
		Device:        p.Device,
		Harness:       p.Harness,
		Name:          effName,
		Title:         effTitle,
		OldState:      prevState,
		NewState:      p.State,
		StateChanged:  stateChanged,
		PendingAction: parsePendingAction(newPendingJSON),
		Summary:       optionalTruncated(p.Summary, prevSummary, 160),
		GitStatus:     optionalTruncated(p.GitStatus, prevGitStatus, 1500),
		LastAssistant: lastAssistantFromTail(effTail),
		ChangedAt:     newStateChangedAt,
	}
}

func buildExitTransition(row *sessionRow, nowISO string) StateTransition {
	return StateTransition{
		SessionID:     row.sessionID,
		Device:        row.device,
		Harness:       row.harness,
		Name:          row.name,
		Title:         row.title,
		OldState:      row.state,
		NewState:      "exited",
		StateChanged:  true,
		Summary:       row.summary,
		GitStatus:     row.gitStatus,
		LastAssistant: lastAssistantFromTail(parseTail(row.tailJSON)),
		ChangedAt:     nowISO,
	}
}

func parseWatchEventsJSON(raw string) []string {
	var out []string
	if json.Unmarshal([]byte(raw), &out) != nil || len(out) == 0 {
		return slices.Clone(defaultWatchEvents)
	}
	return out
}

type watchRow struct {
	Watch
	lastNotifiedKey string
}

func scanWatchRow(scanner interface{ Scan(...any) error }) (watchRow, error) {
	var w watchRow
	var eventsJSON string
	var onceInt int
	err := scanner.Scan(
		&w.ID, &w.WatcherSession, &w.WatcherDevice, &w.TargetSession, &w.TargetDevice,
		&w.TargetName, &eventsJSON, &onceInt, &w.lastNotifiedKey, &w.CreatedAt, &w.UpdatedAt,
	)
	if err != nil {
		return watchRow{}, err
	}
	w.Events = parseWatchEventsJSON(eventsJSON)
	w.Once = onceInt != 0
	return w, nil
}

const watchSelectCols = "id, watcher_session, watcher_device, target_session, target_device, target_name, events_json, once, last_notified_key, created_at, updated_at"

func scanAllWatches(rows *sql.Rows) ([]watchRow, error) {
	defer func() { _ = rows.Close() }()
	var out []watchRow
	for rows.Next() {
		w, err := scanWatchRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (h *Hub) resolveWatchTargetLocked(target, targetDevice, targetName string, allowPending bool) (sid, dev, name string, err error) {
	t := strings.TrimSpace(target)
	td := strings.TrimSpace(targetDevice)
	tn := strings.TrimSpace(targetName)
	lookup := t
	if lookup == "" && tn != "" {
		lookup = tn
		if td != "" {
			lookup = td + "/" + tn
		}
	}
	if lookup == "" {
		return "", "", "", newAgentErr("watch target or target_name is required", 400)
	}
	if det, gErr := h.getSessionLocked(lookup); gErr == nil {
		return det.SessionID, det.Device, cmp.Or(det.Name, tn), nil
	} else if !allowPending {
		return "", "", "", gErr
	}
	if td == "" && tn == "" {
		if dPart, nPart, ok := strings.Cut(t, "/"); ok && !strings.Contains(t, ":") && nPart != "" {
			td, tn = strings.TrimSpace(dPart), strings.TrimSpace(nPart)
		} else if !strings.Contains(t, ":") {
			tn = t
		}
	}
	if tn == "" && !strings.Contains(t, ":") {
		return "", "", "", newAgentErr("target_name is required for a pending watch", 400)
	}
	if strings.Contains(t, ":") && tn == "" {
		return t, td, "", nil
	}
	return "", td, tn, nil
}

// CreateWatch registers or updates a state-transition subscription for a watching session.
func (h *Hub) CreateWatch(p CreateWatchParams) (Watch, error) {
	if p.WatcherSession == "" || !strings.HasPrefix(p.WatcherSession, p.WatcherDevice+":") {
		return Watch{}, newAgentErr("watcher_session must belong to caller device", 403)
	}
	events, err := ParseWatchEvents(p.Events)
	if err != nil {
		return Watch{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.checkOpenLocked(); err != nil {
		return Watch{}, err
	}

	targetSID, targetDev, targetName, err := h.resolveWatchTargetLocked(p.Target, p.TargetDevice, p.TargetName, p.AllowPending)
	if err != nil {
		return Watch{}, err
	}
	if targetSID != "" && targetSID == p.WatcherSession {
		return Watch{}, newAgentErr("session cannot watch itself", 400)
	}

	eventsBytes, _ := json.Marshal(events)
	onceInt := 0
	if p.Once {
		onceInt = 1
	}
	nowISO := registry.NowISO()

	var existingID string
	if targetSID != "" {
		_ = h.db.QueryRow(
			"SELECT id FROM agent_watches WHERE watcher_session = ? AND target_session = ? LIMIT 1",
			p.WatcherSession, targetSID,
		).Scan(&existingID)
	} else {
		_ = h.db.QueryRow(
			"SELECT id FROM agent_watches WHERE watcher_session = ? AND target_session = '' AND target_device = ? AND LOWER(target_name) = LOWER(?) LIMIT 1",
			p.WatcherSession, targetDev, targetName,
		).Scan(&existingID)
	}
	if existingID != "" {
		if _, err := h.db.Exec(
			"UPDATE agent_watches SET target_device = ?, target_name = ?, events_json = ?, once = ?, updated_at = ? WHERE id = ?",
			targetDev, targetName, string(eventsBytes), onceInt, nowISO, existingID,
		); err != nil {
			return Watch{}, err
		}
		wr, err := scanWatchRow(h.db.QueryRow("SELECT "+watchSelectCols+" FROM agent_watches WHERE id = ?", existingID))
		return wr.Watch, err
	}

	id := randomWatchID()
	if _, err := h.db.Exec(
		"INSERT INTO agent_watches(id, watcher_session, watcher_device, target_session, target_device, target_name, events_json, once, last_notified_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?)",
		id, p.WatcherSession, p.WatcherDevice, targetSID, targetDev, targetName, string(eventsBytes), onceInt, nowISO, nowISO,
	); err != nil {
		return Watch{}, err
	}
	wr, err := scanWatchRow(h.db.QueryRow("SELECT "+watchSelectCols+" FROM agent_watches WHERE id = ?", id))
	return wr.Watch, err
}

// ListWatches returns active watches filtered optionally by watcherDevice, watcherSession, or target.
func (h *Hub) ListWatches(watcherDevice, watcherSession, target string) ([]Watch, error) {
	var clauses []string
	var args []any
	if d := strings.TrimSpace(watcherDevice); d != "" {
		clauses = append(clauses, "watcher_device = ?")
		args = append(args, d)
	}
	if ws := strings.TrimSpace(watcherSession); ws != "" {
		clauses = append(clauses, "watcher_session = ?")
		args = append(args, ws)
	}
	if t := strings.TrimSpace(target); t != "" {
		clauses = append(clauses, "(target_session = ? OR LOWER(target_name) = LOWER(?))")
		args = append(args, t, t)
	}
	q := "SELECT " + watchSelectCols + " FROM agent_watches "
	if len(clauses) > 0 {
		q += "WHERE " + strings.Join(clauses, " AND ") + " "
	}
	q += "ORDER BY created_at DESC"

	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.checkOpenLocked(); err != nil {
		return nil, err
	}
	rows, err := h.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	list, err := scanAllWatches(rows)
	if err != nil {
		return nil, err
	}
	out := make([]Watch, len(list))
	for i := range list {
		out[i] = list[i].Watch
	}
	return out, nil
}

// DeleteWatch cancels watches matching id or (watcherSession, target).
func (h *Hub) DeleteWatch(device string, admin bool, id, watcherSession, target string) (int, error) {
	id = strings.TrimSpace(id)
	watcherSession = strings.TrimSpace(watcherSession)
	target = strings.TrimSpace(target)
	if id == "" && target == "" {
		return 0, newAgentErr("watch id or target is required", 400)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.checkOpenLocked(); err != nil {
		return 0, err
	}

	var clauses []string
	var args []any
	if !admin {
		clauses = append(clauses, "watcher_device = ?")
		args = append(args, device)
	}
	if id != "" {
		clauses = append(clauses, "id = ?")
		args = append(args, id)
	} else {
		if watcherSession != "" {
			clauses = append(clauses, "watcher_session = ?")
			args = append(args, watcherSession)
		}
		if det, gErr := h.getSessionLocked(target); gErr == nil {
			clauses = append(clauses, "(target_session = ? OR LOWER(target_name) = LOWER(?))")
			args = append(args, det.SessionID, target)
		} else {
			clauses = append(clauses, "(target_session = ? OR LOWER(target_name) = LOWER(?))")
			args = append(args, target, target)
		}
	}
	res, err := h.db.Exec("DELETE FROM agent_watches WHERE "+strings.Join(clauses, " AND "), args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func watchDedupeKey(ev StateTransition) string {
	if IsWaitingState(ev.NewState) {
		return ev.NewState + "|" + ev.PendingAction["tool"] + "|" + ev.PendingAction["detail"]
	}
	return ev.NewState
}

func formatWatchMessage(ev StateTransition) string {
	label := ev.SessionID
	if ev.Name != "" {
		label = fmt.Sprintf("%s (%s)", ev.Name, ev.SessionID)
	}
	var b strings.Builder
	if ev.OldState != "" {
		fmt.Fprintf(&b, "[Fleet Watch] %s transitioned %s -> %s (%s on %s)", label, ev.OldState, ev.NewState, ev.Harness, ev.Device)
	} else {
		fmt.Fprintf(&b, "[Fleet Watch] %s is now %s (%s on %s)", label, ev.NewState, ev.Harness, ev.Device)
	}
	if len(ev.PendingAction) > 0 {
		tool := strings.TrimSpace(ev.PendingAction["tool"])
		detail := strings.TrimSpace(ev.PendingAction["detail"])
		switch {
		case tool != "" && detail != "":
			fmt.Fprintf(&b, "\nPending (%s): %s", tool, detail)
		case tool != "" || detail != "":
			fmt.Fprintf(&b, "\nPending: %s", cmp.Or(tool, detail))
		}
	}
	if ev.LastAssistant != "" {
		fmt.Fprintf(&b, "\nLast response: %s", TruncateRunes(ev.LastAssistant, 800))
	}
	if ev.GitStatus != "" {
		fmt.Fprintf(&b, "\nGit status: %s", TruncateRunes(strings.ReplaceAll(ev.GitStatus, "\n", "; "), 250))
	}
	return b.String()
}

func (h *Hub) bindPendingWatchesLocked(ev StateTransition) {
	if ev.Name == "" || ev.SessionID == "" {
		return
	}
	_, _ = h.db.Exec(
		"UPDATE agent_watches SET target_session = ?, updated_at = ? "+
			"WHERE target_session = '' AND (target_device = '' OR target_device = ?) AND LOWER(target_name) = LOWER(?)",
		ev.SessionID, ev.ChangedAt, ev.Device, ev.Name,
	)
}

func (h *Hub) handleTransitionLocked(ev StateTransition) {
	h.bindPendingWatchesLocked(ev)
	if !ev.StateChanged {
		return
	}
	for ch := range h.watchSubs {
		select {
		case ch <- ev:
		default:
		}
	}
	if ev.NewState == "working" {
		_, _ = h.db.Exec("UPDATE agent_watches SET last_notified_key = '' WHERE target_session = ?", ev.SessionID)
	}

	rows, err := h.db.Query("SELECT "+watchSelectCols+" FROM agent_watches WHERE target_session = ?", ev.SessionID)
	if err != nil {
		return
	}
	watches, err := scanAllWatches(rows)
	if err != nil || len(watches) == 0 {
		return
	}
	dedupeKey := watchDedupeKey(ev)
	nowISO := registry.NowISO()
	for _, w := range watches {
		if w.WatcherSession == ev.SessionID {
			continue
		}
		if !slices.Contains(w.Events, ev.NewState) {
			if ev.NewState == "exited" {
				_, _ = h.db.Exec("DELETE FROM agent_watches WHERE id = ?", w.ID)
			}
			continue
		}
		if w.lastNotifiedKey == dedupeKey {
			continue
		}
		body := formatWatchMessage(ev)
		threadID := "watch-" + w.ID
		if _, err := h.db.Exec(
			"INSERT INTO agent_messages(thread_id, reply_to, from_session, from_device, to_device, to_session, to_repo, body, created_at) VALUES (?, NULL, ?, ?, ?, ?, NULL, ?, ?)",
			threadID, ev.SessionID, ev.Device, w.WatcherDevice, w.WatcherSession, body, nowISO,
		); err != nil {
			continue
		}
		h.sessionSubs.notify(w.WatcherSession)
		if key := deviceHarnessKey(w.WatcherSession); key != "" {
			h.harnessSubs.notify(key)
		}
		if w.Once || ev.NewState == "exited" {
			_, _ = h.db.Exec("DELETE FROM agent_watches WHERE id = ?", w.ID)
		} else {
			_, _ = h.db.Exec("UPDATE agent_watches SET last_notified_key = ?, updated_at = ? WHERE id = ?", dedupeKey, nowISO, w.ID)
		}
	}
}

func transitionFromDetail(det SessionDetail) StateTransition {
	return StateTransition{
		SessionID:     det.SessionID,
		Device:        det.Device,
		Harness:       det.Harness,
		Name:          det.Name,
		Title:         det.Title,
		NewState:      det.State,
		StateChanged:  true,
		PendingAction: det.PendingAction,
		Summary:       det.Summary,
		GitStatus:     det.GitStatus,
		LastAssistant: lastAssistantFromTail(det.Tail),
		ChangedAt:     cmp.Or(det.StateChangedAt, det.UpdatedAt),
	}
}

func (h *Hub) waitForTransitionLocked(ctx context.Context, subCh <-chan StateTransition, remaining time.Duration) (StateTransition, bool, error) {
	done := h.doneCh
	h.mu.Unlock()
	timer := time.NewTimer(remaining)
	var ev StateTransition
	var gotEv bool
	var ctxErr error
	select {
	case ev = <-subCh:
		gotEv = true
	case <-timer.C:
	case <-done:
	case <-ctx.Done():
		ctxErr = ctx.Err()
	}
	timer.Stop()
	h.mu.Lock()
	return ev, gotEv, ctxErr
}

func matchWatchTransition(ev StateTransition, targetSID *string, targetDev, targetName string, events []string) bool {
	matched := (*targetSID != "" && ev.SessionID == *targetSID) ||
		(*targetSID == "" && (targetDev == "" || ev.Device == targetDev) && targetName != "" && strings.EqualFold(ev.Name, targetName))
	if !matched {
		return false
	}
	if *targetSID == "" {
		*targetSID = ev.SessionID
	}
	return slices.Contains(events, ev.NewState)
}

// WaitForSessionEvent blocks until a watched session enters one of the requested states, or timeout elapses.
func (h *Hub) WaitForSessionEvent(ctx context.Context, p WatchWaitParams) (WatchWaitResult, error) {
	events, err := ParseWatchEvents(p.Events)
	if err != nil {
		return WatchWaitResult{}, err
	}
	timeout := p.Timeout
	if timeout == 0 {
		timeout = 55.0
	}
	timeout = math.Max(0.05, math.Min(timeout, 60.0))
	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))

	h.mu.Lock()
	if err := h.checkOpenLocked(); err != nil {
		h.mu.Unlock()
		return WatchWaitResult{}, err
	}
	subCh := make(chan StateTransition, 16)
	h.watchSubs[subCh] = struct{}{}
	defer func() {
		delete(h.watchSubs, subCh)
		h.mu.Unlock()
	}()

	targetSID, targetDev, targetName, err := h.resolveWatchTargetLocked(p.Target, p.TargetDevice, p.TargetName, p.AllowPending)
	if err != nil {
		return WatchWaitResult{}, err
	}
	if targetSID != "" {
		if det, gErr := h.getSessionLocked(targetSID); gErr == nil {
			if (p.IncludeCurrent || det.State == "exited") && slices.Contains(events, det.State) {
				ev := transitionFromDetail(det)
				return WatchWaitResult{Matched: true, Event: &ev}, nil
			}
		}
	}

	for {
		if h.closed {
			return WatchWaitResult{TimedOut: true}, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return WatchWaitResult{TimedOut: true}, nil
		}
		ev, gotEv, ctxErr := h.waitForTransitionLocked(ctx, subCh, remaining)
		if ctxErr != nil {
			return WatchWaitResult{}, ctxErr
		}
		if gotEv && matchWatchTransition(ev, &targetSID, targetDev, targetName, events) {
			evCopy := ev
			return WatchWaitResult{Matched: true, Event: &evCopy}, nil
		}
	}
}
