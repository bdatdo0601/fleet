package agents

import (
	"cmp"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/registry"
)

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
	turnCount         int
	startedAt         string
	updatedAt         string
}

const sessionListCols = "session_id, device, harness, raw_id, full_raw_id, pid, cwd, repo, branch, " +
	"state, state_changed_at, pending_action_json, title, name, custom_name, summary, git_status, " +
	"CASE WHEN json_valid(tail_json) THEN COALESCE(json_array_length(tail_json), 0) ELSE 0 END, started_at, updated_at"

const sessionDetailCols = "session_id, device, harness, raw_id, full_raw_id, pid, cwd, repo, branch, " +
	"state, state_changed_at, pending_action_json, title, name, custom_name, summary, git_status, tail_json, started_at, updated_at"

func scanSessionListRow(scanner interface{ Scan(...any) error }) (*sessionRow, error) {
	var r sessionRow
	err := scanner.Scan(
		&r.sessionID, &r.device, &r.harness, &r.rawID, &r.fullRawID,
		&r.pid, &r.cwd, &r.repo, &r.branch, &r.state,
		&r.stateChangedAt, &r.pendingActionJSON, &r.title, &r.name,
		&r.customName, &r.summary, &r.gitStatus, &r.turnCount, &r.startedAt, &r.updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func scanAllSessionListRows(rows *sql.Rows) ([]sessionRow, error) {
	defer func() { _ = rows.Close() }()
	var out []sessionRow
	for rows.Next() {
		r, err := scanSessionListRow(rows)
		if err != nil {
			return nil, err
		}
		if r != nil {
			out = append(out, *r)
		}
	}
	return out, rows.Err()
}

func scanSessionDetailRow(scanner interface{ Scan(...any) error }) (*sessionRow, error) {
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
	r.turnCount = len(parseTail(r.tailJSON))
	return &r, nil
}

func scanAllSessionDetailRows(rows *sql.Rows) ([]sessionRow, error) {
	defer func() { _ = rows.Close() }()
	var out []sessionRow
	for rows.Next() {
		r, err := scanSessionDetailRow(rows)
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
	stateChangedAt := r.stateChangedAt
	if stateChangedAt == "" {
		stateChangedAt = r.updatedAt
	}
	stateAge := max(int(now.Sub(parseTS(stateChangedAt)).Seconds()), 0)
	var pendingAction map[string]string
	var waitingSecs *int
	if IsWaitingState(r.state) {
		pendingAction = parsePendingAction(r.pendingActionJSON)
		ws := stateAge
		waitingSecs = &ws
	}
	isListening := listening && live && r.state != "exited"
	return Session{
		SessionID:       r.sessionID,
		Device:          r.device,
		Harness:         r.harness,
		FullRawID:       r.fullRawID,
		PID:             r.pid,
		CWD:             r.cwd,
		Repo:            r.repo,
		Branch:          r.branch,
		State:           r.state,
		StateChangedAt:  stateChangedAt,
		StateAgeSeconds: stateAge,
		WaitingSeconds:  waitingSecs,
		PendingAction:   pendingAction,
		Live:            live,
		Listening:       isListening,
		Delivery:        DeliveryMode(live, r.state, isListening),
		AgeSeconds:      age,
		Title:           r.title,
		Name:            r.name,
		Summary:         r.summary,
		GitStatus:       r.gitStatus,
		TurnCount:       r.turnCount,
		StartedAt:       r.startedAt,
		UpdatedAt:       r.updatedAt,
	}
}

func (h *Hub) serializeSessionLocked(r *sessionRow, nowUTC time.Time, nowMono time.Time) Session {
	updated := parseTS(r.updatedAt)
	age := max(int(nowUTC.Sub(updated).Seconds()), 0)
	live := isLiveRow(r.state, age)
	listening := h.isListeningLocked(r.sessionID, r.state, live, nowMono)
	return rowToSession(r, nowUTC, listening)
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
	if _, err := execer.Exec("UPDATE agent_watches SET target_session = ? WHERE target_session = ?", targetID, oldID); err != nil {
		return err
	}
	if _, err := execer.Exec("UPDATE agent_watches SET watcher_session = ? WHERE watcher_session = ?", targetID, oldID); err != nil {
		return err
	}
	return nil
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
) (promotedFrom string, outPID *int, startedAt, title, name string, customName int, err error) {
	if cwd == "" {
		return "", pid, "", "", "", 0, nil
	}
	cands, err := loadPlaceholdersLocked(tx, device, harness, cwd)
	if err != nil || len(cands) == 0 {
		return "", pid, "", "", "", 0, err
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
		return "", pid, "", "", "", 0, err
	}
	outPID = pid
	if outPID == nil {
		outPID = chosen.pid
	}
	return chosen.sid, outPID, chosen.startedAt, chosen.title, chosen.name, chosen.customName, nil
}

func (h *Hub) findPromotedSessionLocked(device, harness string, pid int, cwd string) (*sessionRow, error) {
	now := time.Now().UTC().Truncate(time.Second)
	cutoff := now.Add(-time.Duration(paths.AgentActiveSeconds) * time.Second).Format("2006-01-02T15:04:05+00:00")
	waitingCutoff := now.Add(-time.Duration(paths.AgentWaitingActiveSeconds) * time.Second).Format("2006-01-02T15:04:05+00:00")
	byPID, err := scanSessionDetailRow(h.db.QueryRow(
		"SELECT "+sessionDetailCols+" FROM agent_sessions "+
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
			"SELECT "+sessionDetailCols+" FROM agent_sessions "+
				"WHERE device = ? AND harness = ? AND cwd = ? AND pid IS NULL AND raw_id NOT LIKE 'pid-%' "+
				"AND state != 'exited' "+
				"AND (updated_at >= ? OR (state IN ('waiting_permission', 'waiting_question') AND updated_at >= ?)) "+
				"ORDER BY updated_at DESC",
			device, harness, cwd, cutoff, waitingCutoff,
		)
		if err != nil {
			return nil, err
		}
		list, err := scanAllSessionDetailRows(rows)
		if err != nil {
			return nil, err
		}
		if len(list) == 1 {
			return &list[0], nil
		}
	}
	return nil, nil
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

func (h *Hub) finishHeartbeatLocked(device, sessionID, fullRaw, name, title string, drain bool) (HeartbeatResult, error) {
	h.sessionSubs.notify(sessionID)
	if key := deviceHarnessKey(sessionID); key != "" {
		h.harnessSubs.notify(key)
	}
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
	effCWD := cmp.Or(p.CWD, promoted.cwd)
	if _, err := tx.Exec(
		"UPDATE agent_sessions SET pid = ?, cwd = ?, repo = ?, branch = ?, "+
			"title = ?, name = ?, custom_name = ?, summary = ?, git_status = ?, "+
			"tail_json = ?, updated_at = ? WHERE session_id = ?",
		newPID, effCWD, effRepo, cmp.Or(p.Branch, promoted.branch),
		effTitle, effName, effCustom, optionalTruncated(p.Summary, promoted.summary, 160),
		optionalTruncated(p.GitStatus, promoted.gitStatus, 1500), effTailJSON, nowISO, targetID,
	); err != nil {
		return HeartbeatResult{}, err
	}
	bumpPath := p.CWD != "" && NormalizeTrackedPath(p.CWD) != NormalizeTrackedPath(promoted.cwd)
	if err := recordPathExecLocked(tx, p.Device, effCWD, effRepo, "heartbeat", nowISO, bumpPath); err != nil {
		return HeartbeatResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return HeartbeatResult{}, err
	}
	h.transferWaiterLocked(sessionID, targetID)
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

func resolveHeartbeatIdentity(existing *sessionRow, isPlaceholder bool, rawClean string) string {
	if !isPlaceholder {
		return rawClean
	}
	if existing != nil {
		return existing.fullRawID
	}
	return ""
}

func resolveHeartbeatRepo(pRepo string, existing *sessionRow) string {
	if pRepo != "" {
		return pRepo
	}
	if existing != nil {
		return existing.repo
	}
	return ""
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
	res, tr, triggers, trCb, watchCb, hErr := h.heartbeatLocked(p, sessionID, shortID, rawClean, nowISO)
	h.mu.Unlock()
	if hErr != nil {
		return HeartbeatResult{}, hErr
	}
	h.fireTransitionCallbacks(trCb, watchCb, tr, triggers)
	return res, nil
}

func (h *Hub) heartbeatLocked(
	p HeartbeatParams, sessionID, shortID, rawClean, nowISO string,
) (HeartbeatResult, StateTransition, []watchTrigger, func(StateTransition), func(Watch, StateTransition), error) {
	if err := h.checkOpenLocked(); err != nil {
		return HeartbeatResult{}, StateTransition{}, nil, nil, nil, err
	}
	if err := h.pruneLocked(false); err != nil {
		return HeartbeatResult{}, StateTransition{}, nil, nil, nil, err
	}

	isPlaceholder := strings.HasPrefix(shortID, "pid-")
	if isPlaceholder && p.PID != nil {
		promoted, err := h.findPromotedSessionLocked(p.Device, p.Harness, *p.PID, p.CWD)
		if err != nil {
			return HeartbeatResult{}, StateTransition{}, nil, nil, nil, err
		}
		if promoted != nil {
			res, err := h.heartbeatPromotedLocked(p, sessionID, promoted, nowISO)
			return res, StateTransition{}, nil, nil, nil, err
		}
	}

	tx, err := h.db.Begin()
	if err != nil {
		return HeartbeatResult{}, StateTransition{}, nil, nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()

	oldPID := p.PID
	var promotedFrom, oldStarted, oldTitle, oldName string
	var oldCustom int
	if !isPlaceholder {
		promotedFrom, oldPID, oldStarted, oldTitle, oldName, oldCustom, err = h.promotePlaceholderLocked(tx, p.Device, p.Harness, p.CWD, p.PID, sessionID)
		if err != nil {
			return HeartbeatResult{}, StateTransition{}, nil, nil, nil, err
		}
	}
	existing, err := scanSessionDetailRow(tx.QueryRow("SELECT "+sessionDetailCols+" FROM agent_sessions WHERE session_id = ?", sessionID))
	if err != nil {
		return HeartbeatResult{}, StateTransition{}, nil, nil, nil, err
	}
	baseTitle, baseName, baseCustom := baseIdentity(existing, oldTitle, oldName, oldCustom)
	effRepo := resolveHeartbeatRepo(p.Repo, existing)
	effTitle, effName, effCustom, err := h.resolveTitleAndNameLocked(tx, p.Device, sessionID, effRepo, p.Title, p.Name, p.ExplicitName, baseTitle, baseName, baseCustom)
	if err != nil {
		return HeartbeatResult{}, StateTransition{}, nil, nil, nil, err
	}
	fullRaw := resolveHeartbeatIdentity(existing, isPlaceholder, rawClean)
	newPendingJSON, err := resolvePendingAction(p.State, p.PendingAction, existing)
	if err != nil {
		return HeartbeatResult{}, StateTransition{}, nil, nil, nil, err
	}
	newStateChangedAt := resolveStateChangedAt(p.State, newPendingJSON, nowISO, existing)
	if err := upsertSessionTxLocked(tx, p, existing, sessionID, shortID, fullRaw, effRepo, effTitle, effName, effCustom, newPendingJSON, newStateChangedAt, oldStarted, nowISO, oldPID); err != nil {
		return HeartbeatResult{}, StateTransition{}, nil, nil, nil, err
	}
	if err := recordHeartbeatSessionPathLocked(tx, p, existing, effRepo, nowISO); err != nil {
		return HeartbeatResult{}, StateTransition{}, nil, nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return HeartbeatResult{}, StateTransition{}, nil, nil, nil, err
	}
	if promotedFrom != "" {
		h.transferWaiterLocked(promotedFrom, sessionID)
	}
	res, err := h.finishHeartbeatLocked(p.Device, sessionID, fullRaw, effName, effTitle, p.DrainInbox)
	if err != nil {
		return HeartbeatResult{}, StateTransition{}, nil, nil, nil, err
	}
	tr := buildHeartbeatTransition(p, existing, sessionID, effTitle, effName, newPendingJSON, newStateChangedAt)
	triggers := h.handleTransitionLocked(tr)
	return res, tr, triggers, h.onTransition, h.onWatchTriggered, nil
}

func recordHeartbeatSessionPathLocked(tx *sql.Tx, p HeartbeatParams, existing *sessionRow, effRepo, nowISO string) error {
	effCWD := p.CWD
	if effCWD == "" && existing != nil {
		effCWD = existing.cwd
	}
	bumpPath := existing == nil || (p.CWD != "" && NormalizeTrackedPath(p.CWD) != NormalizeTrackedPath(existing.cwd))
	return recordPathExecLocked(tx, p.Device, effCWD, effRepo, "heartbeat", nowISO, bumpPath)
}

func (h *Hub) renameSessionRow(sessionID, newName, deviceFilter string) (string, error) {
	slug, err := validateSessionName(newName)
	if err != nil {
		return "", err
	}
	nowISO := registry.NowISO()
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.checkOpenLocked(); err != nil {
		return "", err
	}
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

// ExitedSession describes a session transitioned to exited by DeregisterWithInfo or DeregisterAnyWithInfo.
type ExitedSession struct {
	SessionID string
	Device    string
	Harness   string
	Name      string
	Summary   string
	GitStatus string
}

func (h *Hub) collectExitedRowsLocked(query string, args ...any) ([]*sessionRow, error) {
	rows, err := h.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	list, err := scanAllSessionDetailRows(rows)
	if err != nil {
		return nil, err
	}
	out := make([]*sessionRow, len(list))
	for i := range list {
		out[i] = &list[i]
	}
	return out, nil
}

func (h *Hub) emitExitedRowsLocked(rows []*sessionRow, nowISO string) ([]ExitedSession, []StateTransition, [][]watchTrigger) {
	exited := make([]ExitedSession, 0, len(rows))
	trs := make([]StateTransition, 0, len(rows))
	trigs := make([][]watchTrigger, 0, len(rows))
	for _, r := range rows {
		exited = append(exited, ExitedSession{
			SessionID: r.sessionID,
			Device:    r.device,
			Harness:   r.harness,
			Name:      r.name,
			Summary:   r.summary,
			GitStatus: r.gitStatus,
		})
		tr := buildExitTransition(r, nowISO)
		trs = append(trs, tr)
		trigs = append(trigs, h.handleTransitionLocked(tr))
	}
	return exited, trs, trigs
}

// Deregister marks a session (or sessions matching harness+pid) as exited and wakes waiters.
func (h *Hub) Deregister(device string, sessionID, harness *string, pid *int) error {
	_, err := h.DeregisterWithInfo(device, sessionID, harness, pid)
	return err
}

// DeregisterWithInfo marks matching sessions as exited, wakes waiters, and returns sessions that transitioned from non-exited.
func (h *Hub) DeregisterWithInfo(device string, sessionID, harness *string, pid *int) ([]ExitedSession, error) {
	if sessionID != nil && *sessionID != "" && !strings.HasPrefix(*sessionID, device+":") {
		return nil, newAgentErr("session belongs to another device", 403)
	}
	nowISO := registry.NowISO()
	h.mu.Lock()
	if err := h.checkOpenLocked(); err != nil {
		h.mu.Unlock()
		return nil, err
	}
	var exitedRows []*sessionRow
	switch {
	case sessionID != nil && *sessionID != "":
		exitedRows, _ = h.collectExitedRowsLocked(
			"SELECT "+sessionDetailCols+" FROM agent_sessions WHERE device = ? AND session_id = ? AND state != 'exited'",
			device, *sessionID,
		)
		if _, err := h.db.Exec(
			"UPDATE agent_sessions SET state = 'exited', state_changed_at = ?, pending_action_json = '', updated_at = ? WHERE device = ? AND session_id = ?",
			nowISO, nowISO, device, *sessionID,
		); err != nil {
			h.mu.Unlock()
			return nil, err
		}
		h.sessionSubs.notify(*sessionID)
	case harness != nil && *harness != "" && pid != nil:
		exitedRows, _ = h.collectExitedRowsLocked(
			"SELECT "+sessionDetailCols+" FROM agent_sessions WHERE device = ? AND harness = ? AND pid = ? AND state != 'exited'",
			device, *harness, *pid,
		)
		sids, _ := h.collectStringsLocked(
			"SELECT session_id FROM agent_sessions WHERE device = ? AND harness = ? AND pid = ?",
			device, *harness, *pid,
		)
		if _, err := h.db.Exec(
			"UPDATE agent_sessions SET state = 'exited', state_changed_at = ?, pending_action_json = '', updated_at = ? WHERE device = ? AND harness = ? AND pid = ?",
			nowISO, nowISO, device, *harness, *pid,
		); err != nil {
			h.mu.Unlock()
			return nil, err
		}
		for _, s := range sids {
			h.sessionSubs.notify(s)
		}
	default:
		h.mu.Unlock()
		return nil, newAgentErr("session_id or (harness, pid) is required", 400)
	}
	exited, trs, trigs := h.emitExitedRowsLocked(exitedRows, nowISO)
	trCb, watchCb := h.onTransition, h.onWatchTriggered
	h.mu.Unlock()

	for i, tr := range trs {
		h.fireTransitionCallbacks(trCb, watchCb, tr, trigs[i])
	}
	return exited, nil
}

// DeregisterAny marks any session as exited and wakes waiters (for admin callers).
func (h *Hub) DeregisterAny(sessionID string) error {
	_, err := h.DeregisterAnyWithInfo(sessionID)
	return err
}

// DeregisterAnyWithInfo marks any session as exited, wakes waiters, and returns its metadata if it transitioned from non-exited.
func (h *Hub) DeregisterAnyWithInfo(sessionID string) (*ExitedSession, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, newAgentErr("session_id is required", 400)
	}
	nowISO := registry.NowISO()
	h.mu.Lock()
	if err := h.checkOpenLocked(); err != nil {
		h.mu.Unlock()
		return nil, err
	}
	exitedRows, _ := h.collectExitedRowsLocked(
		"SELECT "+sessionDetailCols+" FROM agent_sessions WHERE session_id = ? AND state != 'exited'",
		sessionID,
	)
	res, err := h.db.Exec(
		"UPDATE agent_sessions SET state = 'exited', state_changed_at = ?, pending_action_json = '', updated_at = ? WHERE session_id = ?",
		nowISO, nowISO, sessionID,
	)
	if err != nil {
		h.mu.Unlock()
		return nil, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		h.mu.Unlock()
		return nil, err
	}
	if affected == 0 {
		h.mu.Unlock()
		return nil, newAgentErr(fmt.Sprintf("no agent session %q", sessionID), 404)
	}
	h.sessionSubs.notify(sessionID)
	exitedList, trs, trigs := h.emitExitedRowsLocked(exitedRows, nowISO)
	trCb, watchCb := h.onTransition, h.onWatchTriggered
	h.mu.Unlock()

	for i, tr := range trs {
		h.fireTransitionCallbacks(trCb, watchCb, tr, trigs[i])
	}
	if len(exitedList) > 0 {
		return &exitedList[0], nil
	}
	return nil, nil
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
	if err := h.checkOpenLocked(); err != nil {
		h.mu.Unlock()
		return nil, err
	}
	rows, err := h.db.Query(
		"SELECT "+sessionListCols+" FROM agent_sessions "+
			"WHERE (? IS NULL OR device = ?) AND (? IS NULL OR repo = ?) AND (? IS NULL OR harness = ?) "+
			"ORDER BY updated_at DESC",
		devArg, devArg, repoArg, repoArg, harArg, harArg,
	)
	if err != nil {
		h.mu.Unlock()
		return nil, err
	}
	rawRows, err := scanAllSessionListRows(rows)
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
