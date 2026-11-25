// Package hooks extracts local transcript/git metadata and processes agent lifecycle hooks.
package hooks

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/memory"
	"github.com/bdatdo0601/fleet/internal/paths"
)

const MaxTranscriptBytes = 256 * 1024

var userRequestRE = regexp.MustCompile(`(?s)<USER_REQUEST>\s*(.*?)\s*</USER_REQUEST>`)

// GitContext returns (repo, branch, shortStatus) for cwd without blocking longer than 1.5s.
func GitContext(cwd string) (string, string, string) {
	if cwd == "" {
		return "", "", ""
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return filepath.Base(cwd), "", ""
	}
	repo := filepath.Base(cwd)
	var branch, gitStatus string

	ctx1, cancel1 := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel1()
	topOut, err := exec.CommandContext(ctx1, "git", "-C", cwd, "rev-parse", "--show-toplevel", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return repo, "", ""
	}
	var lines []string
	for ln := range strings.SplitSeq(string(topOut), "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			lines = append(lines, t)
		}
	}
	if len(lines) > 0 {
		repo = filepath.Base(lines[0])
	}
	if len(lines) > 1 {
		branch = lines[1]
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel2()
	if stOut, err := exec.CommandContext(ctx2, "git", "-C", cwd, "status", "--short").Output(); err == nil && strings.TrimSpace(string(stOut)) != "" {
		stLines := strings.Split(string(stOut), "\n")
		if len(stLines) > 25 {
			stLines = stLines[:25]
		}
		gitStatus = agents.TruncateRunes(strings.Join(stLines, "\n"), 1500)
	}
	return repo, branch, gitStatus
}

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
			return []agents.TailItem{{Role: "user", Text: agents.TruncateRunes(text, 400)}}
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
			return []agents.TailItem{{Role: "assistant", Text: agents.TruncateRunes(strings.TrimSpace(s), 400)}}
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
				items = append(items, agents.TailItem{Role: "assistant", Text: agents.TruncateRunes(txt, 400)})
			}
		case "tool_use":
			items = append(items, agents.TailItem{Role: "tool", Text: agents.TruncateRunes(fmtTool(b.Name, b.Input), 400)})
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
		return []agents.TailItem{{Role: p.Role, Text: agents.TruncateRunes(text, 400)}}
	}
	if p.Type == "function_call" || p.Type == "custom_tool_call" || p.Type == "local_shell_call" {
		args := p.Arguments
		if args == nil {
			args = p.Input
		}
		return []agents.TailItem{{Role: "tool", Text: agents.TruncateRunes(fmtTool(cmp.Or(p.Name, p.Type), args), 400)}}
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
			return []agents.TailItem{{Role: "user", Text: agents.TruncateRunes(trimmed, 400)}}
		}
		return nil
	}
	if ev.Type != "PLANNER_RESPONSE" && ev.Type != "EXECUTOR_RESPONSE" && ev.Type != "MODEL_RESPONSE" && len(ev.ToolCalls) == 0 {
		return nil
	}
	var items []agents.TailItem
	if txt := strings.TrimSpace(ev.Content); txt != "" {
		items = append(items, agents.TailItem{Role: "assistant", Text: agents.TruncateRunes(txt, 400)})
	}
	if len(ev.ToolCalls) > 0 {
		names := make([]string, len(ev.ToolCalls))
		for i, tc := range ev.ToolCalls {
			names[i] = cmp.Or(tc.Name, tc.Tool, "tool")
		}
		items = append(items, agents.TailItem{Role: "tool", Text: agents.TruncateRunes(strings.Join(names, ", "), 400)})
	}
	return items
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

func readClaudeTitle(transcriptPath string) (string, bool) {
	if transcriptPath == "" || !isRegularFile(transcriptPath) {
		return "", false
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()

	var customTitle, agentName, aiTitle string
	br := bufio.NewReader(f)
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
	if exp := cmp.Or(customTitle, agentName); exp != "" {
		return exp, true
	}
	return aiTitle, false
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

	latest := ""
	needle := []byte(fullRawID)
	br := bufio.NewReader(f)
	for {
		rawLine, err := br.ReadBytes('\n')
		if len(rawLine) > 0 && bytes.Contains(rawLine, needle) {
			var obj struct {
				ID         string `json:"id"`
				ThreadName string `json:"thread_name"`
			}
			if json.Unmarshal(bytes.TrimSpace(rawLine), &obj) == nil && obj.ID == fullRawID {
				latest = cmp.Or(strings.TrimSpace(obj.ThreadName), latest)
			}
		}
		if err != nil {
			break
		}
	}
	return latest, false
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
		db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro", filepath.ToSlash(dbPath)))
		if err != nil {
			continue
		}
		_, _ = db.Exec("PRAGMA busy_timeout = 500")
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

// FormatUnread formats unread peer messages for hook injection or CLI wait output.
func FormatUnread(sessionID string, messages []agents.Message, sessionName, rearmHint string) string {
	label := sessionID
	if sessionName != "" {
		label = fmt.Sprintf("%s (%s)", sessionName, sessionID)
	}
	lines := []string{
		fmt.Sprintf("[Fleet Peer Messages for %s]", label),
		"Reply using `fleet_send_message` (include `reply_to` / `thread_id` and `session_id` below):",
	}
	for _, m := range messages {
		sender := m.FromSession
		if m.FromName != "" {
			sender = fmt.Sprintf("%s (%s)", m.FromName, m.FromSession)
		}
		lines = append(lines, fmt.Sprintf("- id=%d thread=%s from=%s: %s", m.ID, m.ThreadID, sender, m.Body))
	}
	if rearmHint != "" {
		lines = append(lines, "", rearmHint)
	}
	return strings.Join(lines, "\n")
}

func sessionStartMessage(harness, sessionID, sessionName string) string {
	idLabel := fmt.Sprintf("`%s`", sessionID)
	if sessionName != "" {
		idLabel = fmt.Sprintf("`%s` (name `%s`)", sessionID, sessionName)
	}
	lines := []string{
		fmt.Sprintf(
			"Your fleet agent session ID is %s. "+
				"Pass `%s` as `session_id` when calling `fleet_send_message`, `fleet_check_inbox`, "+
				"`fleet_ask_agent`, `fleet_set_name`, `fleet_listen`, or `fleet_memory_*` tools.",
			idLabel, sessionID,
		),
	}
	switch harness {
	case "antigravity":
		lines = append(lines, fmt.Sprintf(
			"To receive peer messages immediately while idle, on your first turn run "+
				"`fleet agents wait --session %s` in the background via `run_command` "+
				"(`WaitMsBeforeAsync=500`, `IsDaemon=false`), and re-run it after handling each incoming fleet message.",
			sessionID,
		))
	case "claude":
		lines = append(lines, fmt.Sprintf(
			"To receive peer messages immediately while idle, on your first turn run "+
				"`fleet agents wait --session %s` in the background via Bash "+
				"(`run_in_background=true`), and re-run it after handling each incoming fleet message.",
			sessionID,
		))
	case "codex":
		lines = append(lines,
			"The fleet MCP server automatically listens in the background and delivers incoming peer "+
				"messages to this session via `codex queue` (use `fleet_listen` to check status or pause/resume).",
		)
	}
	return strings.Join(lines, "\n")
}

func strVal(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func summarizeQuestions(toolName string, args any) string {
	if m, ok := args.(map[string]any); ok {
		if qs, ok := m["questions"].([]any); ok && len(qs) > 0 {
			var parts []string
			for _, raw := range qs {
				if q, ok := raw.(map[string]any); ok {
					if txt := strings.TrimSpace(cmp.Or(strVal(q["question"]), strVal(q["header"]))); txt != "" {
						parts = append(parts, txt)
					}
				}
			}
			if len(parts) > 0 {
				return agents.TruncateRunes(strings.Join(parts, " | "), 400)
			}
		}
		for _, k := range []string{"question", "prompt"} {
			if txt := strings.TrimSpace(strVal(m[k])); txt != "" {
				return agents.TruncateRunes(txt, 400)
			}
		}
	}
	return agents.TruncateRunes(fmtTool(toolName, args), 400)
}

func summarizePermissionMap(toolName string, m map[string]any) (string, bool) {
	_, hasAction := m["Action"]
	_, hasTarget := m["Target"]
	_, hasReason := m["Reason"]
	if toolName != "ask_permission" && !hasAction && !hasTarget && !hasReason {
		return "", false
	}
	head := strings.TrimSpace(strings.TrimSpace(strVal(m["Action"])) + " " + strings.TrimSpace(strVal(m["Target"])))
	reason := strings.TrimSpace(strVal(m["Reason"]))
	if head != "" && reason != "" {
		return agents.TruncateRunes(head+" — "+reason, 400), true
	}
	return agents.TruncateRunes(cmp.Or(head, reason), 400), head != "" || reason != ""
}

func summarizeToolDetail(toolName string, toolInput any) string {
	m, ok := toolInput.(map[string]any)
	if !ok {
		return agents.TruncateRunes(strings.TrimSpace(strVal(toolInput)), 400)
	}
	if s, ok := summarizePermissionMap(toolName, m); ok {
		return s
	}
	for _, k := range []string{"command", "CommandLine", "cmd"} {
		if xs, ok := m[k].([]any); ok {
			var parts []string
			for _, x := range xs {
				if x != nil {
					parts = append(parts, fmt.Sprint(x))
				}
			}
			if joined := strings.TrimSpace(strings.Join(parts, " ")); joined != "" {
				return agents.TruncateRunes(joined, 400)
			}
		} else if s := strings.TrimSpace(strVal(m[k])); s != "" {
			return agents.TruncateRunes(s, 400)
		}
	}
	for _, k := range []string{"file_path", "TargetFile", "path", "AbsolutePath", "description"} {
		if s := strings.TrimSpace(strVal(m[k])); s != "" {
			return agents.TruncateRunes(s, 400)
		}
	}
	return agents.TruncateRunes(fmtTool(toolName, m), 400)
}

func pendingActionMap(kind, tool, detail, event string) map[string]string {
	m := map[string]string{"kind": kind, "detail": detail, "event": event}
	if tool != "" {
		m["tool"] = tool
	}
	return m
}

type hookInput struct {
	HookEventName      string   `json:"hookEventName"`
	ConversationID     string   `json:"conversationId"`
	WorkspacePaths     []string `json:"workspacePaths"`
	TranscriptPath     string   `json:"transcriptPath"`
	LastUserInput      string   `json:"lastUserInput"`
	ContinuationActive bool     `json:"continuationActive"`
	TerminationReason  string   `json:"terminationReason"`
	ToolCall           *struct {
		Name string `json:"name"`
		Args any    `json:"args"`
	} `json:"toolCall"`

	SnakeEvent          string `json:"hook_event_name"`
	SessionID           string `json:"session_id"`
	SnakeTranscriptPath string `json:"transcript_path"`
	Prompt              string `json:"prompt"`
	StopHookActive      bool   `json:"stop_hook_active"`
	ToolName            string `json:"tool_name"`
	ToolInput           any    `json:"tool_input"`
	NotificationType    string `json:"notification_type"`
	Message             string `json:"message"`
	Title               string `json:"title"`
	CWD                 string `json:"cwd"`
}

func classifyAntigravityEvent(event string, in *hookInput) (string, map[string]string, bool) {
	switch event {
	case "Stop":
		term := cmp.Or(in.TerminationReason, "NO_TOOL_CALL")
		return "idle", nil, !in.ContinuationActive && term == "NO_TOOL_CALL"
	case "PreToolUse":
		if in.ToolCall == nil {
			return "working", nil, false
		}
		switch in.ToolCall.Name {
		case "ask_question":
			return "waiting_question", pendingActionMap("question", in.ToolCall.Name, summarizeQuestions(in.ToolCall.Name, in.ToolCall.Args), event), false
		case "ask_permission":
			return "waiting_permission", pendingActionMap("permission", in.ToolCall.Name, summarizeToolDetail(in.ToolCall.Name, in.ToolCall.Args), event), false
		default:
			return "working", nil, false
		}
	case "SessionStart", "PreInvocation":
		return "working", nil, true
	default:
		return "working", nil, false
	}
}

func classifyClaudeNotification(in *hookInput) (string, map[string]string, bool) {
	msg := agents.TruncateRunes(strings.TrimSpace(cmp.Or(in.Message, in.Title)), 400)
	switch in.NotificationType {
	case "permission_prompt":
		return "waiting_permission", pendingActionMap("permission", "", cmp.Or(msg, "Waiting for permission approval"), "Notification:"+in.NotificationType), false
	case "elicitation_dialog", "elicitation_url_dialog", "agent_needs_input":
		return "waiting_question", pendingActionMap("question", "", cmp.Or(msg, fmt.Sprintf("Waiting for user input (%s)", in.NotificationType)), "Notification:"+in.NotificationType), false
	case "idle_prompt":
		return "idle", nil, false
	default:
		return "", nil, false
	}
}

func classifyHookEvent(harness, event string, in *hookInput) (string, map[string]string, bool) {
	if harness == "antigravity" {
		return classifyAntigravityEvent(event, in)
	}
	if harness == "claude" {
		switch event {
		case "PermissionRequest", "PreToolUse":
			switch in.ToolName {
			case "AskUserQuestion":
				return "waiting_question", pendingActionMap("question", in.ToolName, summarizeQuestions(in.ToolName, in.ToolInput), event), false
			case "ExitPlanMode":
				return "waiting_question", pendingActionMap("question", in.ToolName, "Waiting for plan approval", event), false
			}
			if event == "PermissionRequest" {
				return "waiting_permission", pendingActionMap("permission", in.ToolName, summarizeToolDetail(in.ToolName, in.ToolInput), event), false
			}
			return "working", nil, false
		case "Notification":
			return classifyClaudeNotification(in)
		}
	}
	if harness == "codex" {
		switch event {
		case "PermissionRequest":
			return "waiting_permission", pendingActionMap("permission", in.ToolName, summarizeToolDetail(in.ToolName, in.ToolInput), event), false
		case "PreToolUse":
			if in.ToolName == "request_user_input" {
				return "waiting_question", pendingActionMap("question", in.ToolName, summarizeQuestions(in.ToolName, in.ToolInput), event), false
			}
			return "working", nil, false
		case "Interrupt":
			return "idle", nil, false
		}
	}
	switch event {
	case "Stop":
		return "idle", nil, !in.StopHookActive
	case "SessionEnd":
		return "exited", nil, false
	case "SessionStart", "UserPromptSubmit", "PostToolUse":
		return "working", nil, true
	default:
		return "working", nil, false
	}
}

func formatAntigravityOutput(event, contextText, unreadText string, canInject bool) map[string]any {
	switch event {
	case "PreToolUse":
		return map[string]any{"decision": "allow"}
	case "SessionStart", "PreInvocation":
		if contextText == "" {
			return map[string]any{}
		}
		return map[string]any{"injectSteps": []map[string]any{{"systemMessage": map[string]any{"systemMessage": contextText}}}}
	case "Stop":
		if unreadText != "" && canInject {
			return map[string]any{"decision": "continue", "reason": unreadText}
		}
		return map[string]any{"decision": "allow"}
	default:
		return map[string]any{}
	}
}

func formatHookOutput(harness, event, sessionID string, unread []agents.Message, canInject bool, sessionName, memoryContext string) map[string]any {
	unreadText := ""
	if len(unread) > 0 {
		unreadText = FormatUnread(sessionID, unread, sessionName, "")
	}
	var parts []string
	if event == "SessionStart" && sessionID != "" {
		parts = append(parts, sessionStartMessage(harness, sessionID, sessionName))
	}
	if event == "SessionStart" && memoryContext != "" {
		parts = append(parts, memoryContext)
	}
	if unreadText != "" {
		parts = append(parts, unreadText)
	}
	joined := strings.Join(parts, "\n\n")
	if harness == "antigravity" {
		return formatAntigravityOutput(event, joined, unreadText, canInject)
	}
	switch event {
	case "SessionStart", "UserPromptSubmit", "PostToolUse":
		if joined == "" {
			return map[string]any{}
		}
		return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": event, "additionalContext": joined}}
	case "Stop":
		if unreadText != "" && canInject {
			return map[string]any{"decision": "block", "reason": unreadText}
		}
		return map[string]any{}
	default:
		return map[string]any{}
	}
}

func parseHookInput(harness string, stdin io.Reader) (in hookInput, event, rawID, cwd, transcriptPath, prompt string) {
	rawBytes, _ := io.ReadAll(stdin)
	if trimmed := bytes.TrimSpace(rawBytes); len(trimmed) > 0 {
		_ = json.Unmarshal(trimmed, &in)
	}
	cwdDefault, _ := os.Getwd()
	if harness == "antigravity" {
		fallbackWS := ""
		if len(in.WorkspacePaths) > 0 {
			fallbackWS = in.WorkspacePaths[0]
		}
		return in, in.HookEventName, cmp.Or(in.ConversationID, "agy"), cmp.Or(in.CWD, fallbackWS, cwdDefault), in.TranscriptPath, in.LastUserInput
	}
	return in, in.SnakeEvent, cmp.Or(in.SessionID, fmt.Sprintf("pid-%d", os.Getppid())), cmp.Or(in.CWD, cwdDefault), in.SnakeTranscriptPath, in.Prompt
}

// RunHook processes a single lifecycle hook payload from stdin and writes harness JSON to stdout.
func RunHook(harness string, stdin io.Reader, stdout io.Writer, apiURL string) (int, error) {
	if !paths.ValidHarness(harness) {
		return 1, fmt.Errorf("unknown harness %q", harness)
	}
	if stdin == nil {
		stdin = os.Stdin
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	in, event, rawID, cwd, transcriptPath, prompt := parseHookInput(harness, stdin)
	state, pendingAction, canInject := classifyHookEvent(harness, event, &in)
	if state == "" {
		_, _ = io.WriteString(stdout, "{}\n")
		return 0, nil
	}

	var repo, branch, gitStatus, tailSummary, title, memRepo string
	var tail []agents.TailItem
	var isExplicitName bool
	var wg sync.WaitGroup
	wg.Go(func() { repo, branch, gitStatus = GitContext(cwd) })
	if event == "SessionStart" {
		wg.Go(func() { memRepo = memory.CanonicalRepo(cwd) })
	}
	wg.Go(func() { tailSummary, tail = ReadTranscriptTail(harness, transcriptPath, cwd, 20) })
	wg.Go(func() { title, isExplicitName = ReadSessionTitle(harness, rawID, transcriptPath, "", "") })
	wg.Wait()

	summary := tailSummary
	if trimmed := strings.TrimSpace(prompt); trimmed != "" {
		summary = agents.TruncateRunes(trimmed, 160)
	}

	var sessionID, sessionName, memoryContext string
	var unread []agents.Message
	if api, err := client.ResolveAPI(apiURL); err == nil {
		var pidPtr *int
		if harness != "antigravity" {
			pidPtr = new(os.Getppid())
		}
		var titlePtr, namePtr, sumPtr *string
		if title != "" {
			titlePtr = &title
			if isExplicitName {
				namePtr = &title
			}
		}
		if summary != "" {
			sumPtr = &summary
		}
		var resp agents.HeartbeatResult
		err := client.Call(api, "POST", "/v1/agents/heartbeat", agents.HeartbeatParams{
			Harness: harness, RawID: rawID, PID: pidPtr, CWD: cwd, Repo: repo, Branch: branch,
			State: state, PendingAction: pendingAction, Title: titlePtr, Name: namePtr,
			ExplicitName: isExplicitName, Summary: sumPtr, GitStatus: &gitStatus, Tail: tail, DrainInbox: canInject,
			IncludeMemory: event == "SessionStart", MemoryRepo: memRepo,
		}, 2*time.Second, nil, &resp)
		if err == nil {
			sessionID, sessionName, unread, memoryContext = resp.SessionID, resp.Name, resp.Unread, resp.MemoryContext
		}
	}
	if event == "SessionStart" {
		if memoryContext != "" {
			_ = memory.SaveContextCache(paths.CacheDir(), memRepo, memoryContext, nil, nil)
		} else {
			memoryContext = memory.LoadContextCache(paths.CacheDir(), memRepo)
		}
	}

	output := formatHookOutput(harness, event, sessionID, unread, canInject, sessionName, memoryContext)
	b, _ := json.Marshal(output)
	_, _ = stdout.Write(append(b, '\n'))
	return 0, nil
}
