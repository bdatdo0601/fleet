package hooks

import (
	"bufio"
	"bytes"
	"cmp"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	// Register pure-Go SQLite driver for reading Antigravity conversation_summaries.db.
	_ "modernc.org/sqlite"

	"github.com/bdatdo0601/fleet/internal/agents"
)

const (
	MaxTranscriptBytes    = 256 * 1024
	maxTailItemRunes      = 400
	maxLastAssistantRunes = 2000
	maxTitleScanBytes     = 8 << 20
	maxTitleCacheEntries  = 256
)

var (
	userRequestRE       = regexp.MustCompile(`(?s)<USER_REQUEST>\s*(.*?)\s*</USER_REQUEST>`)
	claudeProjectSlugRE = regexp.MustCompile(`[^a-zA-Z0-9-]`)
)

type claudeTitleCacheEntry struct {
	size     int64
	modNano  int64
	title    string
	explicit bool
}

type codexIndexCacheEntry struct {
	size    int64
	modNano int64
	titles  map[string]string
}

var (
	claudeTitleCacheMu sync.Mutex
	claudeTitleCache   = map[string]claudeTitleCacheEntry{}

	codexIndexCacheMu sync.Mutex
	codexIndexCache   = map[string]codexIndexCacheEntry{}
)

func firstLineCWDMatch(path, cwd string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	firstLine, err := bufio.NewReader(f).ReadString('\n')
	_ = f.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	trimmed := strings.TrimSpace(firstLine)
	if trimmed == "" {
		return false
	}
	var obj struct {
		Payload struct {
			CWD string `json:"cwd"`
		} `json:"payload"`
	}
	return json.Unmarshal([]byte(trimmed), &obj) == nil && obj.Payload.CWD == cwd
}

// FindCodexTranscript locates the most recent Codex rollout JSONL matching cwd.
func FindCodexTranscript(cwd, codexHome string) string {
	if cwd == "" {
		return ""
	}
	if codexHome == "" {
		h, _ := os.UserHomeDir()
		codexHome = filepath.Join(h, ".codex")
	}
	sessionsDir := filepath.Join(codexHome, "sessions")
	info, err := os.Stat(sessionsDir)
	if err != nil || !info.IsDir() {
		return ""
	}
	cutoff := time.Now().Add(-7200 * time.Second)
	type cand struct {
		mtime time.Time
		path  string
	}
	var cands []cand
	_ = filepath.WalkDir(sessionsDir, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr == nil && !d.IsDir() && strings.HasSuffix(d.Name(), ".jsonl") {
			if fi, fiErr := d.Info(); fiErr == nil && !fi.ModTime().Before(cutoff) {
				cands = append(cands, cand{mtime: fi.ModTime(), path: p})
			}
		}
		return nil
	})
	sort.Slice(cands, func(i, j int) bool { return cands[i].mtime.After(cands[j].mtime) })
	if len(cands) > 10 {
		cands = cands[:10]
	}
	for _, c := range cands {
		if firstLineCWDMatch(c.path, cwd) {
			return c.path
		}
	}
	return ""
}

// FindClaudeTranscript locates the Claude Code transcript JSONL for a session ID.
func FindClaudeTranscript(cwd, fullRawID, claudeHome string) string {
	if fullRawID == "" || strings.HasPrefix(fullRawID, "pid-") || strings.ContainsAny(fullRawID, `/\`) || strings.Contains(fullRawID, "..") {
		return ""
	}
	if claudeHome == "" {
		h, _ := os.UserHomeDir()
		claudeHome = filepath.Join(h, ".claude")
	}
	projectsDir := filepath.Join(claudeHome, "projects")
	filename := fullRawID + ".jsonl"
	if cwd != "" {
		slug := claudeProjectSlugRE.ReplaceAllString(cwd, "-")
		cand := filepath.Join(projectsDir, slug, filename)
		if isRegularFile(cand) {
			return cand
		}
	}
	matches, err := filepath.Glob(filepath.Join(projectsDir, "*", filename))
	if err != nil {
		return ""
	}
	for _, m := range matches {
		if isRegularFile(m) {
			return m
		}
	}
	return ""
}

// FindCodexTranscriptForSession locates a Codex rollout JSONL matching fullRawID, falling back to CWD matching.
func FindCodexTranscriptForSession(fullRawID, cwd, codexHome string) string {
	if codexHome == "" {
		h, _ := os.UserHomeDir()
		codexHome = filepath.Join(h, ".codex")
	}
	if fullRawID != "" && !strings.HasPrefix(fullRawID, "pid-") && !strings.ContainsAny(fullRawID, `/\`) && !strings.Contains(fullRawID, "..") {
		sessionsDir := filepath.Join(codexHome, "sessions")
		if info, err := os.Stat(sessionsDir); err == nil && info.IsDir() {
			exact := fullRawID + ".jsonl"
			suffix := "-" + exact
			var bestPath string
			var bestTime time.Time
			_ = filepath.WalkDir(sessionsDir, func(p string, d os.DirEntry, walkErr error) error {
				if walkErr == nil && !d.IsDir() {
					name := d.Name()
					if name == exact || strings.HasSuffix(name, suffix) {
						if fi, fiErr := d.Info(); fiErr == nil && (bestPath == "" || fi.ModTime().After(bestTime)) {
							bestPath = p
							bestTime = fi.ModTime()
						}
					}
				}
				return nil
			})
			if bestPath != "" {
				return bestPath
			}
		}
	}
	return FindCodexTranscript(cwd, codexHome)
}

// FindSessionTranscript resolves the local transcript file path for a live harness session.
func FindSessionTranscript(harness, fullRawID, cwd string) string {
	switch harness {
	case "claude":
		return FindClaudeTranscript(cwd, fullRawID, "")
	case "codex":
		return FindCodexTranscriptForSession(fullRawID, cwd, "")
	default:
		return ""
	}
}

func extractTextBlocks(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			return strings.TrimSpace(s)
		}
		return ""
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(trimmed, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if (b.Type == "text" || b.Type == "input_text" || b.Type == "output_text") && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, strings.TrimSpace(b.Text))
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func fmtTool(name string, args any) string {
	toolName := cmp.Or(name, "tool")
	if m, ok := args.(map[string]any); ok {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > 3 {
			keys = keys[:3]
		}
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%s", k, agents.TruncateRunes(fmt.Sprint(m[k]), 60)))
		}
		if len(parts) > 0 {
			return fmt.Sprintf("%s(%s)", toolName, strings.Join(parts, ", "))
		}
		return toolName
	}
	if s, ok := args.(string); ok && strings.TrimSpace(s) != "" {
		return fmt.Sprintf("%s(%s)", toolName, agents.TruncateRunes(strings.TrimSpace(s), 100))
	}
	return toolName
}

func parseClaudeLine(raw []byte) []agents.TailItem {
	var ev struct {
		Type    string `json:"type"`
		IsMeta  bool   `json:"isMeta"`
		Message *struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &ev) != nil || ev.Message == nil {
		return nil
	}
	if ev.Type == "user" && !ev.IsMeta {
		if text := extractTextBlocks(ev.Message.Content); text != "" {
			return []agents.TailItem{{Role: "user", Text: agents.TruncateRunes(text, maxTailItemRunes)}}
		}
		return nil
	}
	if ev.Type != "assistant" {
		return nil
	}
	trimmed := bytes.TrimSpace(ev.Message.Content)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil && strings.TrimSpace(s) != "" {
			return []agents.TailItem{{Role: "assistant", Text: agents.TruncateRunes(strings.TrimSpace(s), maxLastAssistantRunes)}}
		}
		return nil
	}
	var blocks []struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		Name  string `json:"name"`
		Input any    `json:"input"`
	}
	if json.Unmarshal(trimmed, &blocks) != nil {
		return nil
	}
	var items []agents.TailItem
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if txt := strings.TrimSpace(b.Text); txt != "" {
				items = append(items, agents.TailItem{Role: "assistant", Text: agents.TruncateRunes(txt, maxLastAssistantRunes)})
			}
		case "tool_use":
			items = append(items, agents.TailItem{Role: "tool", Text: agents.TruncateRunes(fmtTool(b.Name, b.Input), maxTailItemRunes)})
		}
	}
	return items
}

func parseCodexLine(raw []byte) []agents.TailItem {
	var ev struct {
		Type    string `json:"type"`
		Payload *struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			Name      string          `json:"name"`
			Arguments any             `json:"arguments"`
			Input     any             `json:"input"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &ev) != nil || ev.Type != "response_item" || ev.Payload == nil {
		return nil
	}
	p := ev.Payload
	if p.Type == "message" && (p.Role == "user" || p.Role == "assistant") {
		text := extractTextBlocks(p.Content)
		if text == "" || strings.HasPrefix(text, "<environment_context>") || strings.HasPrefix(text, "<permissions") {
			return nil
		}
		limit := maxTailItemRunes
		if p.Role == "assistant" {
			limit = maxLastAssistantRunes
		}
		return []agents.TailItem{{Role: p.Role, Text: agents.TruncateRunes(text, limit)}}
	}
	if p.Type == "function_call" || p.Type == "custom_tool_call" || p.Type == "local_shell_call" {
		args := p.Arguments
		if args == nil {
			args = p.Input
		}
		return []agents.TailItem{{Role: "tool", Text: agents.TruncateRunes(fmtTool(cmp.Or(p.Name, p.Type), args), maxTailItemRunes)}}
	}
	return nil
}

func parseAntigravityLine(raw []byte) []agents.TailItem {
	var ev struct {
		Type      string `json:"type"`
		Content   string `json:"content"`
		ToolCalls []struct {
			Name string `json:"name"`
			Tool string `json:"tool"`
		} `json:"tool_calls"`
	}
	if json.Unmarshal(raw, &ev) != nil {
		return nil
	}
	if ev.Type == "USER_INPUT" {
		text := ev.Content
		if m := userRequestRE.FindStringSubmatch(text); len(m) > 1 {
			text = m[1]
		}
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			return []agents.TailItem{{Role: "user", Text: agents.TruncateRunes(trimmed, maxTailItemRunes)}}
		}
		return nil
	}
	if ev.Type != "PLANNER_RESPONSE" && ev.Type != "EXECUTOR_RESPONSE" && ev.Type != "MODEL_RESPONSE" && len(ev.ToolCalls) == 0 {
		return nil
	}
	var items []agents.TailItem
	if txt := strings.TrimSpace(ev.Content); txt != "" {
		items = append(items, agents.TailItem{Role: "assistant", Text: agents.TruncateRunes(txt, maxLastAssistantRunes)})
	}
	if len(ev.ToolCalls) > 0 {
		names := make([]string, len(ev.ToolCalls))
		for i, tc := range ev.ToolCalls {
			names[i] = cmp.Or(tc.Name, tc.Tool, "tool")
		}
		items = append(items, agents.TailItem{Role: "tool", Text: agents.TruncateRunes(strings.Join(names, ", "), maxTailItemRunes)})
	}
	return items
}

func normalizeTailLengths(tail []agents.TailItem) {
	lastAssistantIdx := -1
	for i, it := range tail {
		if it.Role == "assistant" {
			lastAssistantIdx = i
		}
	}
	for i := range tail {
		limit := maxTailItemRunes
		if i == lastAssistantIdx {
			limit = maxLastAssistantRunes
		}
		tail[i].Text = agents.TruncateRunes(tail[i].Text, limit)
	}
}

// ReadTranscriptTail reads up to the last maxItems normalized events from a harness transcript JSONL.
func ReadTranscriptTail(harness, transcriptPath, cwd string, maxItems int) (string, []agents.TailItem) {
	if maxItems <= 0 {
		maxItems = 20
	}
	parsers := map[string]func([]byte) []agents.TailItem{
		"claude": parseClaudeLine, "codex": parseCodexLine, "antigravity": parseAntigravityLine,
	}
	parser := parsers[harness]
	resolved := transcriptPath
	if (resolved == "" || !isRegularFile(resolved)) && harness == "codex" && cwd != "" {
		resolved = FindCodexTranscript(cwd, "")
	}
	if parser == nil || resolved == "" || !isRegularFile(resolved) {
		return "", []agents.TailItem{}
	}
	f, err := os.Open(resolved)
	if err != nil {
		return "", []agents.TailItem{}
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return "", []agents.TailItem{}
	}
	start := max(fi.Size()-MaxTranscriptBytes, 0)
	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return "", []agents.TailItem{}
		}
	}
	br := bufio.NewReader(f)
	if start > 0 {
		_, _ = br.ReadBytes('\n') // discard partial first line
	}
	var items []agents.TailItem
	for {
		line, err := br.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			items = append(items, parser(trimmed)...)
		}
		if err != nil {
			break
		}
	}
	tail := items
	if len(tail) > maxItems {
		tail = tail[len(tail)-maxItems:]
	}
	if tail == nil {
		tail = []agents.TailItem{}
	}
	normalizeTailLengths(tail)
	summary := ""
	for _, it := range slices.Backward(items) {
		if it.Role == "user" && it.Text != "" {
			summary = agents.TruncateRunes(it.Text, 160)
			break
		}
	}
	return summary, tail
}

func isRegularFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

func openBoundedTailReader(f *os.File, size int64) (*bufio.Reader, error) {
	if size > maxTitleScanBytes {
		if _, err := f.Seek(size-maxTitleScanBytes, io.SeekStart); err != nil {
			return nil, err
		}
		br := bufio.NewReader(f)
		_, _ = br.ReadBytes('\n') // discard partial first line
		return br, nil
	}
	return bufio.NewReader(f), nil
}

func readClaudeTitle(transcriptPath string) (string, bool) {
	if transcriptPath == "" || !isRegularFile(transcriptPath) {
		return "", false
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return "", false
	}
	size, modNano := fi.Size(), fi.ModTime().UnixNano()
	claudeTitleCacheMu.Lock()
	if cached, ok := claudeTitleCache[transcriptPath]; ok && cached.size == size && cached.modNano == modNano {
		claudeTitleCacheMu.Unlock()
		return cached.title, cached.explicit
	}
	claudeTitleCacheMu.Unlock()

	br, err := openBoundedTailReader(f, size)
	if err != nil {
		return "", false
	}
	var customTitle, agentName, aiTitle string
	for {
		rawLine, err := br.ReadBytes('\n')
		if len(rawLine) > 0 && (bytes.Contains(rawLine, []byte(`"custom-title"`)) || bytes.Contains(rawLine, []byte(`"agent-name"`)) || bytes.Contains(rawLine, []byte(`"ai-title"`))) {
			var obj struct {
				Type        string `json:"type"`
				CustomTitle string `json:"customTitle"`
				AgentName   string `json:"agentName"`
				AITitle     string `json:"aiTitle"`
			}
			if json.Unmarshal(bytes.TrimSpace(rawLine), &obj) == nil {
				switch obj.Type {
				case "custom-title":
					customTitle = cmp.Or(strings.TrimSpace(obj.CustomTitle), customTitle)
				case "agent-name":
					agentName = cmp.Or(strings.TrimSpace(obj.AgentName), agentName)
				case "ai-title":
					aiTitle = cmp.Or(strings.TrimSpace(obj.AITitle), aiTitle)
				}
			}
		}
		if err != nil {
			break
		}
	}
	title, explicit := aiTitle, false
	if exp := cmp.Or(customTitle, agentName); exp != "" {
		title, explicit = exp, true
	}
	claudeTitleCacheMu.Lock()
	if len(claudeTitleCache) >= maxTitleCacheEntries {
		clear(claudeTitleCache)
	}
	claudeTitleCache[transcriptPath] = claudeTitleCacheEntry{
		size: size, modNano: modNano, title: title, explicit: explicit,
	}
	claudeTitleCacheMu.Unlock()
	return title, explicit
}

func readCodexTitle(fullRawID, codexHome string) (string, bool) {
	if fullRawID == "" || strings.HasPrefix(fullRawID, "pid-") {
		return "", false
	}
	if codexHome == "" {
		h, _ := os.UserHomeDir()
		codexHome = filepath.Join(h, ".codex")
	}
	indexPath := filepath.Join(codexHome, "session_index.jsonl")
	f, err := os.Open(indexPath)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return "", false
	}
	size, modNano := fi.Size(), fi.ModTime().UnixNano()
	codexIndexCacheMu.Lock()
	if cached, ok := codexIndexCache[indexPath]; ok && cached.size == size && cached.modNano == modNano {
		title := cached.titles[fullRawID]
		codexIndexCacheMu.Unlock()
		return title, false
	}
	codexIndexCacheMu.Unlock()

	br, err := openBoundedTailReader(f, size)
	if err != nil {
		return "", false
	}
	titles := make(map[string]string)
	for {
		rawLine, err := br.ReadBytes('\n')
		if len(rawLine) > 0 {
			var obj struct {
				ID         string `json:"id"`
				ThreadName string `json:"thread_name"`
			}
			if json.Unmarshal(bytes.TrimSpace(rawLine), &obj) == nil && obj.ID != "" {
				if tn := strings.TrimSpace(obj.ThreadName); tn != "" {
					titles[obj.ID] = tn
				}
			}
		}
		if err != nil {
			break
		}
	}
	codexIndexCacheMu.Lock()
	if len(codexIndexCache) >= maxTitleCacheEntries {
		clear(codexIndexCache)
	}
	codexIndexCache[indexPath] = codexIndexCacheEntry{
		size: size, modNano: modNano, titles: titles,
	}
	codexIndexCacheMu.Unlock()
	return titles[fullRawID], false
}

func readAntigravityTitle(fullRawID, transcriptPath, geminiHome string) (string, bool) {
	if fullRawID == "" || strings.HasPrefix(fullRawID, "pid-") {
		return "", false
	}
	var candidates []string
	seen := map[string]bool{}
	addCand := func(p string) {
		if isRegularFile(p) && !seen[p] {
			seen[p] = true
			candidates = append(candidates, p)
		}
	}
	if transcriptPath != "" {
		for cur := filepath.Dir(transcriptPath); ; cur = filepath.Dir(cur) {
			addCand(filepath.Join(cur, "conversation_summaries.db"))
			if filepath.Dir(cur) == cur {
				break
			}
		}
	}
	if geminiHome == "" {
		h, _ := os.UserHomeDir()
		geminiHome = filepath.Join(h, ".gemini")
	}
	for _, sub := range []string{"antigravity", "antigravity-cli", "antigravity-ide"} {
		addCand(filepath.Join(geminiHome, sub, "conversation_summaries.db"))
	}
	for _, dbPath := range candidates {
		dsn := (&url.URL{
			Scheme:   "file",
			Path:     filepath.ToSlash(dbPath),
			RawQuery: "mode=ro&_pragma=busy_timeout(500)",
		}).String()
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			continue
		}
		var title string
		err = db.QueryRow("SELECT title FROM conversation_summaries WHERE conversation_id = ?", fullRawID).Scan(&title)
		_ = db.Close()
		if err == nil && strings.TrimSpace(title) != "" {
			return strings.TrimSpace(title), false
		}
	}
	return "", false
}

// ReadSessionTitle discovers (title, isExplicitName) from local harness state.
func ReadSessionTitle(harness, fullRawID, transcriptPath, codexHome, geminiHome string) (string, bool) {
	switch harness {
	case "claude":
		return readClaudeTitle(transcriptPath)
	case "codex":
		return readCodexTitle(fullRawID, codexHome)
	case "antigravity":
		return readAntigravityTitle(fullRawID, transcriptPath, geminiHome)
	default:
		return "", false
	}
}
