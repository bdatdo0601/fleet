// Package hooks extracts local transcript/git metadata and processes agent lifecycle hooks.
package hooks

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/memory"
	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/session"
)

const maxHookInputBytes = 1 << 20

// FormatUnread formats unread peer messages for hook injection or CLI wait output.
func FormatUnread(sessionID string, messages []agents.Message, sessionName, rearmHint string) string {
	label := sessionID
	if sessionName != "" {
		label = fmt.Sprintf("%s (%s)", sessionName, sessionID)
	}
	lines := []string{
		fmt.Sprintf("[Fleet Peer Messages for %s]", label),
		fmt.Sprintf("Reply with `fleet agents send <to> --session %s --reply-to <id> --thread <thread> --message ...` (see `fleet-agents` skill):", sessionID),
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
	var relayNote string
	switch harness {
	case "antigravity":
		relayNote = "Antigravity's `fleet` sidecar delivers incoming peer messages automatically via `agentapi`."
	case "claude":
		relayNote = "Fleet's local agent relay delivers incoming peer messages automatically via Claude's local messaging socket."
	case "codex":
		relayNote = "Fleet's local agent relay delivers incoming peer messages automatically via `codex queue`."
	}
	return fmt.Sprintf(
		"Fleet session: %s. %s Use the `fleet-agents`, `fleet-memory`, or `fleet-slack` skills (pass `--session %s` to `fleet` CLI).",
		idLabel, relayNote, sessionID,
	)
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

	SnakeEvent           string `json:"hook_event_name"`
	SessionID            string `json:"session_id"`
	SnakeTranscriptPath  string `json:"transcript_path"`
	Prompt               string `json:"prompt"`
	StopHookActive       bool   `json:"stop_hook_active"`
	LastAssistantMessage string `json:"last_assistant_message"`
	LastAssistantCamel   string `json:"lastAssistantMessage"`
	ToolName             string `json:"tool_name"`
	ToolInput            any    `json:"tool_input"`
	NotificationType     string `json:"notification_type"`
	Message              string `json:"message"`
	Title                string `json:"title"`
	CWD                  string `json:"cwd"`
	AgentID              string `json:"agent_id"`
	AgentIDCamel         string `json:"agentId"`
	ParentConvID         string `json:"parentConversationId"`
	ParentConvSnake      string `json:"parent_conversation_id"`
	IsSubagent           bool   `json:"isSubagent"`
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
	rawBytes, _ := io.ReadAll(io.LimitReader(stdin, maxHookInputBytes))
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
	pid := os.Getppid()
	if harness == "claude" {
		pid = ClaudeProcessPID()
	}
	return in, in.SnakeEvent, cmp.Or(in.SessionID, fmt.Sprintf("pid-%d", pid)), cmp.Or(in.CWD, cwdDefault), in.SnakeTranscriptPath, in.Prompt
}

func syncClaudeMessagingSocket(event, sessionID string, claudePID int) {
	if event == "SessionEnd" {
		RemoveClaudeSocket("", claudePID, sessionID)
		return
	}
	if sock := strings.TrimSpace(os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET")); sock != "" {
		_ = SaveClaudeSocket("", ClaudeSocketInfo{
			Socket:    sock,
			Token:     os.Getenv("CLAUDE_CODE_MESSAGING_TOKEN"),
			SessionID: sessionID,
			PID:       claudePID,
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}
}

func isSubagentHook(in *hookInput) bool {
	return in.AgentID != "" || in.AgentIDCamel != "" || in.ParentConvID != "" || in.ParentConvSnake != "" || in.IsSubagent
}

func maybeRecordSessionHook(harness string, in *hookInput, rawID, cwd, transcriptPath string) {
	if os.Getenv("FLEET_SESSION_TARGET") == "" {
		return
	}
	if isSubagentHook(in) {
		return
	}
	if rawID == "" || rawID == "agy" || strings.HasPrefix(rawID, "pid-") {
		return
	}
	session.RecordHookState(harness, rawID, cwd, transcriptPath)
}

func mergeLastAssistantMessage(tail []agents.TailItem, msg string, maxItems int) []agents.TailItem {
	text := strings.TrimSpace(msg)
	if text == "" {
		return tail
	}
	if maxItems <= 0 {
		maxItems = 20
	}
	truncated := agents.TruncateRunes(text, maxLastAssistantRunes)
	if len(tail) > 0 && tail[len(tail)-1].Role == "assistant" {
		existing := strings.TrimSpace(tail[len(tail)-1].Text)
		if existing == truncated || strings.HasPrefix(truncated, existing) || strings.HasPrefix(existing, truncated) {
			if len(truncated) > len(existing) {
				tail[len(tail)-1].Text = truncated
			}
			return tail
		}
	}
	tail = append(tail, agents.TailItem{Role: "assistant", Text: truncated})
	if len(tail) > maxItems {
		tail = tail[len(tail)-maxItems:]
	}
	normalizeTailLengths(tail)
	return tail
}

// EnvSessionName returns the Fleet-assigned session name from the environment, if present.
func EnvSessionName(harness string) string {
	if v := strings.TrimSpace(os.Getenv("FLEET_SESSION_NAME")); v != "" {
		return v
	}
	if harness == "antigravity" || harness == "agy" {
		if v := strings.TrimSpace(os.Getenv("AGY_SESSION_NAME")); v != "" {
			return v
		}
	}
	return ""
}

func resolveHeartbeatIdentity(harness, rawID, title string, isExplicitName, isSubagent bool) (titlePtr, namePtr *string, explicitName bool) {
	explicitName = isExplicitName
	if title != "" {
		titlePtr = &title
		if isExplicitName {
			namePtr = &title
		}
	}
	if namePtr == nil && !isSubagent {
		fallback := EnvSessionName(harness)
		if fallback == "" && harness == "antigravity" && rawID != "" {
			fallback = session.LookupAppSessionName(harness, rawID)
		}
		if fallback != "" {
			namePtr = &fallback
			explicitName = true
		}
	}
	return titlePtr, namePtr, explicitName
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
	maybeRecordSessionHook(harness, &in, rawID, cwd, transcriptPath)
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
	tail = mergeLastAssistantMessage(tail, cmp.Or(in.LastAssistantMessage, in.LastAssistantCamel), 20)

	summary := tailSummary
	if trimmed := strings.TrimSpace(prompt); trimmed != "" {
		summary = agents.TruncateRunes(trimmed, 160)
	}

	var sessionID, sessionName, memoryContext string
	var unread []agents.Message
	claudePID := 0
	if harness == "claude" {
		claudePID = ClaudeProcessPID()
	}
	if api, err := client.ResolveAPI(apiURL); err == nil {
		var pidPtr *int
		switch harness {
		case "claude":
			pidPtr = &claudePID
		case "codex":
			pidPtr = new(os.Getppid())
		}
		titlePtr, namePtr, explicitName := resolveHeartbeatIdentity(harness, rawID, title, isExplicitName, isSubagentHook(&in))
		var sumPtr *string
		if summary != "" {
			sumPtr = &summary
		}
		var resp agents.HeartbeatResult
		err := client.Call(api, "POST", "/v1/agents/heartbeat", agents.HeartbeatParams{
			Harness: harness, RawID: rawID, PID: pidPtr, CWD: cwd, Repo: repo, Branch: branch,
			State: state, PendingAction: pendingAction, Title: titlePtr, Name: namePtr,
			ExplicitName: explicitName, Summary: sumPtr, GitStatus: &gitStatus, Tail: tail, DrainInbox: canInject,
			IncludeMemory: event == "SessionStart", MemoryRepo: memRepo,
		}, 2*time.Second, nil, &resp)
		if err == nil {
			sessionID, sessionName, unread, memoryContext = resp.SessionID, resp.Name, resp.Unread, resp.MemoryContext
		}
	}
	if harness == "claude" {
		syncClaudeMessagingSocket(event, sessionID, claudePID)
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
