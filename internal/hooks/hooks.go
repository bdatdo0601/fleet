// Package hooks extracts local transcript/git metadata and processes agent lifecycle hooks.
package hooks

import (
	"bufio"
	"bytes"
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
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/client"
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
	branch := ""
	gitStatus := ""

	ctx1, cancel1 := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel1()
	topOut, err := exec.CommandContext(ctx1, "git", "-C", cwd, "rev-parse", "--show-toplevel", "--abbrev-ref", "HEAD").Output()
	if err == nil {
		var lines []string
		for _, ln := range strings.Split(string(topOut), "\n") {
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
		stOut, err := exec.CommandContext(ctx2, "git", "-C", cwd, "status", "--short").Output()
		if err == nil && strings.TrimSpace(string(stOut)) != "" {
			stLines := strings.Split(string(stOut), "\n")
			if len(stLines) > 25 {
				stLines = stLines[:25]
			}
			gitStatus = agents.TruncateRunes(strings.Join(stLines, "\n"), 1500)
		}
	}
	return repo, branch, gitStatus
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
	_ = filepath.WalkDir(sessionsDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if !fi.ModTime().Before(cutoff) {
			cands = append(cands, cand{mtime: fi.ModTime(), path: p})
		}
		return nil
	})
	sort.Slice(cands, func(i, j int) bool {
		return cands[i].mtime.After(cands[j].mtime)
	})
	if len(cands) > 10 {
		cands = cands[:10]
	}
	for _, c := range cands {
		f, err := os.Open(c.path)
		if err != nil {
			continue
		}
		br := bufio.NewReader(f)
		firstLine, err := br.ReadString('\n')
		_ = f.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			continue
		}
		firstLine = strings.TrimSpace(firstLine)
		if firstLine == "" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(firstLine), &obj) != nil {
			continue
		}
		if payload, ok := obj["payload"].(map[string]any); ok {
			if pCWD, ok := payload["cwd"].(string); ok && pCWD == cwd {
				return c.path
			}
		}
	}
	return ""
}

func extractTextBlocks(content any) string {
	switch v := content.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		var parts []string
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			t, _ := m["type"].(string)
			if t == "text" || t == "input_text" || t == "output_text" {
				if txt, ok := m["text"].(string); ok && strings.TrimSpace(txt) != "" {
					parts = append(parts, strings.TrimSpace(txt))
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
	return ""
}

func fmtTool(name, args any) string {
	toolName := "tool"
	if s, ok := name.(string); ok && s != "" {
		toolName = s
	}
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

func parseClaudeLine(obj map[string]any) []agents.TailItem {
	kind, _ := obj["type"].(string)
	msg, ok := obj["message"].(map[string]any)
	if !ok {
		return nil
	}
	isMeta, _ := obj["isMeta"].(bool)
	if kind == "user" && !isMeta {
		text := extractTextBlocks(msg["content"])
		if text != "" {
			return []agents.TailItem{{Role: "user", Text: agents.TruncateRunes(text, 400)}}
		}
		return nil
	}
	if kind == "assistant" {
		if s, ok := msg["content"].(string); ok && strings.TrimSpace(s) != "" {
			return []agents.TailItem{{Role: "assistant", Text: agents.TruncateRunes(strings.TrimSpace(s), 400)}}
		}
		if blocks, ok := msg["content"].([]any); ok {
			var items []agents.TailItem
			for _, raw := range blocks {
				block, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				btype, _ := block["type"].(string)
				switch btype {
				case "text":
					if txt, ok := block["text"].(string); ok && strings.TrimSpace(txt) != "" {
						items = append(items, agents.TailItem{Role: "assistant", Text: agents.TruncateRunes(strings.TrimSpace(txt), 400)})
					}
				case "tool_use":
					items = append(items, agents.TailItem{
						Role: "tool",
						Text: agents.TruncateRunes(fmtTool(block["name"], block["input"]), 400),
					})
				}
			}
			return items
		}
	}
	return nil
}

func parseCodexLine(obj map[string]any) []agents.TailItem {
	if t, _ := obj["type"].(string); t != "response_item" {
		return nil
	}
	payload, ok := obj["payload"].(map[string]any)
	if !ok {
		return nil
	}
	ptype, _ := payload["type"].(string)
	if ptype == "message" {
		role, _ := payload["role"].(string)
		if role == "user" || role == "assistant" {
			text := extractTextBlocks(payload["content"])
			if text == "" || strings.HasPrefix(text, "<environment_context>") || strings.HasPrefix(text, "<permissions") {
				return nil
			}
			return []agents.TailItem{{Role: role, Text: agents.TruncateRunes(text, 400)}}
		}
	}
	if ptype == "function_call" || ptype == "custom_tool_call" || ptype == "local_shell_call" {
		name := payload["name"]
		if name == nil || name == "" {
			name = ptype
		}
		args := payload["arguments"]
		if args == nil {
			args = payload["input"]
		}
		return []agents.TailItem{{Role: "tool", Text: agents.TruncateRunes(fmtTool(name, args), 400)}}
	}
	return nil
}

func parseAntigravityLine(obj map[string]any) []agents.TailItem {
	kind, _ := obj["type"].(string)
	if kind == "USER_INPUT" {
		raw, ok := obj["content"].(string)
		if !ok {
			return nil
		}
		text := raw
		if m := userRequestRE.FindStringSubmatch(raw); len(m) > 1 {
			text = m[1]
		}
		text = strings.TrimSpace(text)
		if text != "" {
			return []agents.TailItem{{Role: "user", Text: agents.TruncateRunes(text, 400)}}
		}
		return nil
	}
	_, hasCalls := obj["tool_calls"]
	if kind == "PLANNER_RESPONSE" || kind == "EXECUTOR_RESPONSE" || kind == "MODEL_RESPONSE" || hasCalls {
		var items []agents.TailItem
		if raw, ok := obj["content"].(string); ok && strings.TrimSpace(raw) != "" {
			items = append(items, agents.TailItem{Role: "assistant", Text: agents.TruncateRunes(strings.TrimSpace(raw), 400)})
		}
		if calls, ok := obj["tool_calls"].([]any); ok && len(calls) > 0 {
			var names []string
			for _, rawC := range calls {
				if c, ok := rawC.(map[string]any); ok {
					n, _ := c["name"].(string)
					if n == "" {
						n, _ = c["tool"].(string)
					}
					if n == "" {
						n = "tool"
					}
					names = append(names, n)
				}
			}
			if len(names) > 0 {
				items = append(items, agents.TailItem{Role: "tool", Text: agents.TruncateRunes(strings.Join(names, ", "), 400)})
			}
		}
		return items
	}
	return nil
}

// ReadTranscriptTail reads up to the last maxItems normalized events from a harness transcript JSONL.
func ReadTranscriptTail(harness, transcriptPath, cwd string, maxItems int) (string, []agents.TailItem) {
	if maxItems <= 0 {
		maxItems = 20
	}
	resolved := transcriptPath
	if (resolved == "" || !isRegularFile(resolved)) && harness == "codex" && cwd != "" {
		resolved = FindCodexTranscript(cwd, "")
	}
	if resolved == "" || !isRegularFile(resolved) {
		return "", []agents.TailItem{}
	}

	var parser func(map[string]any) []agents.TailItem
	switch harness {
	case "claude":
		parser = parseClaudeLine
	case "codex":
		parser = parseCodexLine
	case "antigravity":
		parser = parseAntigravityLine
	default:
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
	start := fi.Size() - MaxTranscriptBytes
	if start < 0 {
		start = 0
	}
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
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			var obj map[string]any
			if json.Unmarshal(trimmed, &obj) == nil && obj != nil {
				items = append(items, parser(obj)...)
			}
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
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Role == "user" && items[i].Text != "" {
			summary = agents.TruncateRunes(items[i].Text, 160)
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
		if len(rawLine) > 0 &&
			(bytes.Contains(rawLine, []byte(`"custom-title"`)) ||
				bytes.Contains(rawLine, []byte(`"agent-name"`)) ||
				bytes.Contains(rawLine, []byte(`"ai-title"`))) {
			var obj map[string]any
			if json.Unmarshal(bytes.TrimSpace(rawLine), &obj) == nil {
				kind, _ := obj["type"].(string)
				switch kind {
				case "custom-title":
					if v, ok := obj["customTitle"].(string); ok && strings.TrimSpace(v) != "" {
						customTitle = strings.TrimSpace(v)
					}
				case "agent-name":
					if v, ok := obj["agentName"].(string); ok && strings.TrimSpace(v) != "" {
						agentName = strings.TrimSpace(v)
					}
				case "ai-title":
					if v, ok := obj["aiTitle"].(string); ok && strings.TrimSpace(v) != "" {
						aiTitle = strings.TrimSpace(v)
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	if customTitle != "" {
		return customTitle, true
	}
	if agentName != "" {
		return agentName, true
	}
	if aiTitle != "" {
		return aiTitle, false
	}
	return "", false
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
	if !isRegularFile(indexPath) {
		return "", false
	}
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
			var obj map[string]any
			if json.Unmarshal(bytes.TrimSpace(rawLine), &obj) == nil {
				if id, _ := obj["id"].(string); id == fullRawID {
					if tn, ok := obj["thread_name"].(string); ok && strings.TrimSpace(tn) != "" {
						latest = strings.TrimSpace(tn)
					}
				}
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
		cur := filepath.Dir(transcriptPath)
		for {
			addCand(filepath.Join(cur, "conversation_summaries.db"))
			parent := filepath.Dir(cur)
			if parent == cur {
				break
			}
			cur = parent
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
		dsn := fmt.Sprintf("file:%s?mode=ro", filepath.ToSlash(dbPath))
		db, err := sql.Open("sqlite", dsn)
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
				"`fleet_ask_agent`, `fleet_set_name`, or `fleet_listen`.",
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

func summarizeQuestions(toolName string, args any) string {
	if m, ok := args.(map[string]any); ok {
		if qs, ok := m["questions"].([]any); ok && len(qs) > 0 {
			var parts []string
			for _, raw := range qs {
				if q, ok := raw.(map[string]any); ok {
					txt, _ := q["question"].(string)
					if txt == "" {
						txt, _ = q["header"].(string)
					}
					if strings.TrimSpace(txt) != "" {
						parts = append(parts, strings.TrimSpace(txt))
					}
				}
			}
			if len(parts) > 0 {
				return agents.TruncateRunes(strings.Join(parts, " | "), 400)
			}
		}
		for _, k := range []string{"question", "prompt"} {
			if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
				return agents.TruncateRunes(strings.TrimSpace(v), 400)
			}
		}
	}
	return agents.TruncateRunes(fmtTool(toolName, args), 400)
}

func summarizeToolDetail(toolName string, toolInput any) string {
	if m, ok := toolInput.(map[string]any); ok {
		_, hasAction := m["Action"]
		_, hasTarget := m["Target"]
		_, hasReason := m["Reason"]
		if toolName == "ask_permission" || hasAction || hasTarget || hasReason {
			action := strings.TrimSpace(strVal(m["Action"]))
			target := strings.TrimSpace(strVal(m["Target"]))
			reason := strings.TrimSpace(strVal(m["Reason"]))
			head := strings.TrimSpace(action + " " + target)
			if head != "" && reason != "" {
				return agents.TruncateRunes(head+" — "+reason, 400)
			}
			if head != "" {
				return agents.TruncateRunes(head, 400)
			}
			if reason != "" {
				return agents.TruncateRunes(reason, 400)
			}
		}
		for _, k := range []string{"command", "CommandLine", "cmd"} {
			switch v := m[k].(type) {
			case []any:
				var parts []string
				for _, x := range v {
					if x != nil {
						parts = append(parts, fmt.Sprint(x))
					}
				}
				if joined := strings.TrimSpace(strings.Join(parts, " ")); joined != "" {
					return agents.TruncateRunes(joined, 400)
				}
			case string:
				if strings.TrimSpace(v) != "" {
					return agents.TruncateRunes(strings.TrimSpace(v), 400)
				}
			}
		}
		for _, k := range []string{"file_path", "TargetFile", "path", "AbsolutePath"} {
			if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
				return agents.TruncateRunes(strings.TrimSpace(v), 400)
			}
		}
		if d, ok := m["description"].(string); ok && strings.TrimSpace(d) != "" {
			return agents.TruncateRunes(strings.TrimSpace(d), 400)
		}
		return agents.TruncateRunes(fmtTool(toolName, m), 400)
	}
	if s, ok := toolInput.(string); ok && strings.TrimSpace(s) != "" {
		return agents.TruncateRunes(strings.TrimSpace(s), 400)
	}
	return ""
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

func classifyHookEvent(harness, event string, payload map[string]any, contActive bool, termReason string) (string, map[string]string, bool) {
	if harness == "antigravity" {
		switch event {
		case "Stop":
			canInject := !(contActive || termReason != "NO_TOOL_CALL")
			return "idle", nil, canInject
		case "PreToolUse":
			toolCall, _ := payload["toolCall"].(map[string]any)
			toolName := strVal(toolCall["name"])
			toolArgs := toolCall["args"]
			if toolName == "ask_question" {
				return "waiting_question", map[string]string{
					"kind":   "question",
					"tool":   toolName,
					"detail": summarizeQuestions(toolName, toolArgs),
					"event":  event,
				}, false
			}
			if toolName == "ask_permission" {
				return "waiting_permission", map[string]string{
					"kind":   "permission",
					"tool":   toolName,
					"detail": summarizeToolDetail(toolName, toolArgs),
					"event":  event,
				}, false
			}
			return "working", nil, false
		case "SessionStart", "PreInvocation":
			return "working", nil, true
		default:
			return "working", nil, false
		}
	}

	toolName := strVal(payload["tool_name"])
	toolInput := payload["tool_input"]

	if harness == "claude" {
		switch event {
		case "PermissionRequest":
			if toolName == "AskUserQuestion" {
				return "waiting_question", map[string]string{
					"kind":   "question",
					"tool":   toolName,
					"detail": summarizeQuestions(toolName, toolInput),
					"event":  event,
				}, false
			}
			if toolName == "ExitPlanMode" {
				return "waiting_question", map[string]string{
					"kind":   "question",
					"tool":   toolName,
					"detail": "Waiting for plan approval",
					"event":  event,
				}, false
			}
			return "waiting_permission", map[string]string{
				"kind":   "permission",
				"tool":   toolName,
				"detail": summarizeToolDetail(toolName, toolInput),
				"event":  event,
			}, false
		case "PreToolUse":
			if toolName == "AskUserQuestion" {
				return "waiting_question", map[string]string{
					"kind":   "question",
					"tool":   toolName,
					"detail": summarizeQuestions(toolName, toolInput),
					"event":  event,
				}, false
			}
			if toolName == "ExitPlanMode" {
				return "waiting_question", map[string]string{
					"kind":   "question",
					"tool":   toolName,
					"detail": "Waiting for plan approval",
					"event":  event,
				}, false
			}
			return "working", nil, false
		case "Notification":
			notifType := strVal(payload["notification_type"])
			notifMsg := strings.TrimSpace(strVal(payload["message"]))
			if notifMsg == "" {
				notifMsg = strings.TrimSpace(strVal(payload["title"]))
			}
			notifMsg = agents.TruncateRunes(notifMsg, 400)
			if notifType == "permission_prompt" {
				if notifMsg == "" {
					notifMsg = "Waiting for permission approval"
				}
				return "waiting_permission", map[string]string{
					"kind":   "permission",
					"detail": notifMsg,
					"event":  "Notification:" + notifType,
				}, false
			}
			if notifType == "elicitation_dialog" || notifType == "elicitation_url_dialog" || notifType == "agent_needs_input" {
				if notifMsg == "" {
					notifMsg = fmt.Sprintf("Waiting for user input (%s)", notifType)
				}
				return "waiting_question", map[string]string{
					"kind":   "question",
					"detail": notifMsg,
					"event":  "Notification:" + notifType,
				}, false
			}
			if notifType == "idle_prompt" {
				return "idle", nil, false
			}
			return "", nil, false
		case "Stop":
			return "idle", nil, !contActive
		case "SessionEnd":
			return "exited", nil, false
		case "SessionStart", "UserPromptSubmit", "PostToolUse":
			return "working", nil, true
		default:
			return "working", nil, false
		}
	}

	if harness == "codex" {
		switch event {
		case "PermissionRequest":
			return "waiting_permission", map[string]string{
				"kind":   "permission",
				"tool":   toolName,
				"detail": summarizeToolDetail(toolName, toolInput),
				"event":  event,
			}, false
		case "PreToolUse":
			if toolName == "request_user_input" {
				return "waiting_question", map[string]string{
					"kind":   "question",
					"tool":   toolName,
					"detail": summarizeQuestions(toolName, toolInput),
					"event":  event,
				}, false
			}
			return "working", nil, false
		case "Stop":
			return "idle", nil, !contActive
		case "Interrupt":
			return "idle", nil, false
		case "SessionEnd":
			return "exited", nil, false
		case "SessionStart", "UserPromptSubmit", "PostToolUse":
			return "working", nil, true
		default:
			return "working", nil, false
		}
	}
	return "working", nil, false
}

func formatHookOutput(harness, event, sessionID string, unread []agents.Message, canInject bool, sessionName string) map[string]any {
	unreadText := ""
	if len(unread) > 0 {
		unreadText = FormatUnread(sessionID, unread, sessionName, "")
	}
	if harness == "antigravity" {
		switch event {
		case "PreToolUse":
			return map[string]any{"decision": "allow"}
		case "PostToolUse":
			return map[string]any{}
		case "SessionStart":
			if sessionID == "" && unreadText == "" {
				return map[string]any{}
			}
			var parts []string
			if sessionID != "" {
				parts = append(parts, sessionStartMessage(harness, sessionID, sessionName))
			}
			if unreadText != "" {
				parts = append(parts, unreadText)
			}
			return map[string]any{
				"injectSteps": []map[string]any{
					{"systemMessage": map[string]any{"systemMessage": strings.Join(parts, "\n\n")}},
				},
			}
		case "PreInvocation":
			if unreadText == "" {
				return map[string]any{}
			}
			return map[string]any{
				"injectSteps": []map[string]any{
					{"systemMessage": map[string]any{"systemMessage": unreadText}},
				},
			}
		case "Stop":
			if unreadText != "" && canInject {
				return map[string]any{"decision": "continue", "reason": unreadText}
			}
			return map[string]any{"decision": "allow"}
		default:
			return map[string]any{}
		}
	}

	switch event {
	case "SessionStart":
		var parts []string
		if sessionID != "" {
			parts = append(parts, sessionStartMessage(harness, sessionID, sessionName))
		}
		if unreadText != "" {
			parts = append(parts, unreadText)
		}
		if len(parts) == 0 {
			return map[string]any{}
		}
		return map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     "SessionStart",
				"additionalContext": strings.Join(parts, "\n\n"),
			},
		}
	case "UserPromptSubmit", "PostToolUse":
		if unreadText == "" {
			return map[string]any{}
		}
		return map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     event,
				"additionalContext": unreadText,
			},
		}
	case "Stop":
		if unreadText != "" && canInject {
			return map[string]any{"decision": "block", "reason": unreadText}
		}
		return map[string]any{}
	default:
		return map[string]any{}
	}
}

func resolveAPI(apiURL string) (string, error) {
	if apiURL != "" {
		return apiURL, nil
	}
	data, err := os.ReadFile(paths.NodeFile())
	if err != nil {
		return "", err
	}
	var node struct {
		API string `json:"api"`
	}
	if err := json.Unmarshal(data, &node); err != nil {
		return "", err
	}
	if node.API == "" {
		return "", errors.New("missing api in fleet.json")
	}
	return node.API, nil
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
	rawBytes, _ := io.ReadAll(stdin)
	rawTrim := strings.TrimSpace(string(rawBytes))
	payload := map[string]any{}
	if rawTrim != "" {
		_ = json.Unmarshal([]byte(rawTrim), &payload)
		if payload == nil {
			payload = map[string]any{}
		}
	}

	var event, rawID, cwd, transcriptPath, prompt, termReason string
	var contActive bool
	cwdDefault, _ := os.Getwd()

	if harness == "antigravity" {
		event = strVal(payload["hookEventName"])
		rawID = strVal(payload["conversationId"])
		if rawID == "" {
			rawID = "agy"
		}
		fallbackWS := ""
		if ws, ok := payload["workspacePaths"].([]any); ok && len(ws) > 0 {
			fallbackWS = strVal(ws[0])
		}
		cwd = strVal(payload["cwd"])
		if cwd == "" {
			cwd = fallbackWS
		}
		if cwd == "" {
			cwd = cwdDefault
		}
		transcriptPath = strVal(payload["transcriptPath"])
		prompt = strVal(payload["lastUserInput"])
		contActive, _ = payload["continuationActive"].(bool)
		termReason = strVal(payload["terminationReason"])
		if termReason == "" {
			termReason = "NO_TOOL_CALL"
		}
	} else {
		event = strVal(payload["hook_event_name"])
		rawID = strVal(payload["session_id"])
		if rawID == "" {
			rawID = fmt.Sprintf("pid-%d", os.Getppid())
		}
		cwd = strVal(payload["cwd"])
		if cwd == "" {
			cwd = cwdDefault
		}
		transcriptPath = strVal(payload["transcript_path"])
		prompt = strVal(payload["prompt"])
		contActive, _ = payload["stop_hook_active"].(bool)
		termReason = "NO_TOOL_CALL"
	}

	state, pendingAction, canInject := classifyHookEvent(harness, event, payload, contActive, termReason)
	if state == "" {
		_, _ = io.WriteString(stdout, "{}\n")
		return 0, nil
	}

	var (
		repo, branch, gitStatus string
		tailSummary             string
		tail                    []agents.TailItem
		title                   string
		isExplicitName          bool
	)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		repo, branch, gitStatus = GitContext(cwd)
	}()
	go func() {
		defer wg.Done()
		tailSummary, tail = ReadTranscriptTail(harness, transcriptPath, cwd, 20)
	}()
	go func() {
		defer wg.Done()
		title, isExplicitName = ReadSessionTitle(harness, rawID, transcriptPath, "", "")
	}()
	wg.Wait()

	summary := tailSummary
	if strings.TrimSpace(prompt) != "" {
		summary = agents.TruncateRunes(strings.TrimSpace(prompt), 160)
	}

	var sessionID, sessionName string
	var unread []agents.Message

	if api, err := resolveAPI(apiURL); err == nil {
		hbPayload := map[string]any{
			"harness":       harness,
			"raw_id":        rawID,
			"cwd":           cwd,
			"repo":          repo,
			"branch":        branch,
			"state":         state,
			"explicit_name": isExplicitName,
			"git_status":    gitStatus,
			"tail":          tail,
			"drain_inbox":   canInject,
		}
		if harness != "antigravity" {
			hbPayload["pid"] = os.Getppid()
		} else {
			hbPayload["pid"] = nil
		}
		if title != "" {
			hbPayload["title"] = title
		} else {
			hbPayload["title"] = nil
		}
		if isExplicitName && title != "" {
			hbPayload["name"] = title
		} else {
			hbPayload["name"] = nil
		}
		if summary != "" {
			hbPayload["summary"] = summary
		} else {
			hbPayload["summary"] = nil
		}
		if pendingAction != nil {
			hbPayload["pending_action"] = pendingAction
		}
		var resp agents.HeartbeatResult
		if err := client.Call(api, "POST", "/v1/agents/heartbeat", hbPayload, 2*time.Second, nil, &resp); err == nil {
			sessionID = resp.SessionID
			sessionName = resp.Name
			unread = resp.Unread
		}
	}

	output := formatHookOutput(harness, event, sessionID, unread, canInject, sessionName)
	b, _ := json.Marshal(output)
	_, _ = stdout.Write(append(b, '\n'))
	return 0, nil
}
