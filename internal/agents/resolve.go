package agents

import (
	"cmp"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bdatdo0601/fleet/internal/paths"
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

func deviceHarnessKey(sessionID string) string {
	dev, rest, ok := strings.Cut(sessionID, ":")
	if !ok || dev == "" {
		return ""
	}
	har, _, ok := strings.Cut(rest, ":")
	if !ok || har == "" {
		return ""
	}
	return dev + ":" + har
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

func (h *Hub) resolvePromotedLocked(sid string) string {
	seen := map[string]bool{sid: true}
	for range 16 {
		next, ok := h.promotedTo[sid]
		if !ok || next == "" || seen[next] {
			return sid
		}
		seen[next] = true
		sid = next
	}
	return sid
}

type callerCand struct {
	sid string
	pid *int
	cwd string
}

func (h *Hub) listCallerCandidates(device, harness string) ([]callerCand, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.checkOpenLocked(); err != nil {
		return nil, err
	}
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
	row, err := scanSessionDetailRow(h.db.QueryRow("SELECT "+sessionDetailCols+" FROM agent_sessions WHERE session_id = ?", target))
	if err != nil {
		return SessionDetail{}, err
	}
	if row != nil {
		nowUTC := time.Now().UTC().Truncate(time.Second)
		nowMono := time.Now()
		info := h.serializeSessionLocked(row, nowUTC, nowMono)
		return SessionDetail{Session: info, Tail: parseTail(row.tailJSON)}, nil
	}

	rows, err := h.db.Query("SELECT " + sessionDetailCols + " FROM agent_sessions ORDER BY updated_at DESC")
	if err != nil {
		return SessionDetail{}, err
	}
	allRows, err := scanAllSessionDetailRows(rows)
	if err != nil {
		return SessionDetail{}, err
	}
	allPtrs := make([]*sessionRow, len(allRows))
	for i := range allRows {
		allPtrs[i] = &allRows[i]
	}

	var tiers [][]*sessionRow
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
	if err := h.checkOpenLocked(); err != nil {
		return SessionDetail{}, err
	}
	return h.getSessionLocked(target)
}

// AssistantSessionID returns the virtual peer session_id for an assistant channel.
func AssistantSessionID(channelID string) string {
	ch := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(channelID)), "#")
	if ch == "" {
		ch = "general"
	}
	return AssistantSessionPrefix + ch
}

// ParseAssistantTarget parses "assistant", "assistant:<channel>", or "assistant:channel:<channel>".
func ParseAssistantTarget(target string) (string, bool) {
	clean := strings.ToLower(strings.TrimSpace(target))
	switch {
	case clean == AssistantDevice || clean == "@"+AssistantDevice:
		return "general", true
	case strings.HasPrefix(clean, "assistant:channel:"):
		ch := strings.TrimPrefix(strings.TrimPrefix(clean, "assistant:channel:"), "#")
		return cmp.Or(ch, "general"), true
	case strings.HasPrefix(clean, AssistantSessionPrefix):
		ch := strings.TrimPrefix(strings.TrimPrefix(clean, AssistantSessionPrefix), "#")
		return cmp.Or(ch, "general"), true
	default:
		return "", false
	}
}

func (h *Hub) resolveTargetLocked(target string, knownDevices map[string]bool) (toDevice, toSession, toRepo *string, err error) {
	if ch, ok := ParseAssistantTarget(target); ok {
		d := AssistantDevice
		sid := AssistantSessionID(ch)
		return &d, &sid, nil, nil
	}
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
