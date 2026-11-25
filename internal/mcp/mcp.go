// Package mcp implements the stdio MCP JSON-RPC 2.0 server bridging local agent sessions to the lighthouse API.
package mcp

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/hooks"
	"github.com/bdatdo0601/fleet/internal/memory"
	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/version"
	"github.com/bdatdo0601/fleet/internal/waiter"
)

const HeartbeatIntervalSeconds = 45.0

type toolSchemaProp struct {
	Type        string   `json:"type"`
	Enum        []string `json:"enum,omitempty"`
	Description string   `json:"description"`
}

type toolInputSchema struct {
	Type       string                    `json:"type"`
	Properties map[string]toolSchemaProp `json:"properties"`
	Required   []string                  `json:"required,omitempty"`
}

// ToolDef defines one MCP tool schema returned by tools/list.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema toolInputSchema `json:"inputSchema"`
}

// Tools defines the 11 MCP tools exposed to agent harnesses.
var Tools = []ToolDef{
	{
		Name: "fleet_list_agents",
		Description: "List active AI agent sessions (claude, codex, antigravity) across the Nebula fleet, " +
			"including friendly names, titles, state (working, waiting_permission, waiting_question, idle), " +
			"waiting_seconds, pending_action, listening status, and delivery modes.",
		InputSchema: toolInputSchema{
			Type: "object",
			Properties: map[string]toolSchemaProp{
				"device":        {Type: "string", Description: "Optional fleet device name filter (e.g. mini, laptop)."},
				"repo":          {Type: "string", Description: "Optional repository name filter."},
				"harness":       {Type: "string", Enum: paths.Harnesses, Description: "Optional harness filter."},
				"state":         {Type: "string", Enum: []string{"working", "waiting", "waiting_permission", "waiting_question", "idle", "exited"}, Description: "Optional state filter ('waiting' matches waiting_permission and waiting_question)."},
				"include_stale": {Type: "boolean", Description: "Include recently inactive or exited sessions."},
			},
		},
	},
	{
		Name:        "fleet_inspect_agent",
		Description: "Inspect another agent session's cwd, git branch/status, pending permission/question prompts (pending_action, waiting_seconds), and recent conversation/tool activity.",
		InputSchema: toolInputSchema{
			Type: "object",
			Properties: map[string]toolSchemaProp{
				"agent_id":   {Type: "string", Description: "Target session name (or device/name), title, short ID, or full session_id."},
				"tail_limit": {Type: "integer", Description: "Optional maximum number of recent tail entries to return."},
			},
			Required: []string{"agent_id"},
		},
	},
	{
		Name:        "fleet_send_message",
		Description: "Send a message to a peer agent session, device (device:name), repo (repo:name), or all (*).",
		InputSchema: toolInputSchema{
			Type: "object",
			Properties: map[string]toolSchemaProp{
				"to":         {Type: "string", Description: "Recipient session name (or device/name), title, short id, full session_id, device name, device:<name>, repo:<name>, or '*'."},
				"message":    {Type: "string", Description: "Message text to deliver."},
				"session_id": {Type: "string", Description: "Caller session_id if explicitly provided."},
				"thread_id":  {Type: "string", Description: "Optional thread_id to continue an existing thread."},
				"reply_to":   {Type: "integer", Description: "Optional message id being replied to."},
			},
			Required: []string{"to", "message"},
		},
	},
	{
		Name:        "fleet_check_inbox",
		Description: "Check incoming peer messages for this agent session.",
		InputSchema: toolInputSchema{
			Type: "object",
			Properties: map[string]toolSchemaProp{
				"session_id":  {Type: "string", Description: "Caller session_id if explicitly provided."},
				"unread_only": {Type: "boolean", Description: "Return only unread messages (default true)."},
			},
		},
	},
	{
		Name:        "fleet_ask_agent",
		Description: "Send a question to another agent session and wait for its reply.",
		InputSchema: toolInputSchema{
			Type: "object",
			Properties: map[string]toolSchemaProp{
				"to":              {Type: "string", Description: "Target session name (or device/name), title, short id, or full session_id."},
				"question":        {Type: "string", Description: "Question to ask the peer agent."},
				"session_id":      {Type: "string", Description: "Caller session_id if explicitly provided."},
				"timeout_seconds": {Type: "number", Description: "Maximum seconds to wait for a reply (default 30, max 120)."},
				"wait_if_idle":    {Type: "boolean", Description: "Wait for a reply even if the target is currently idle with no listener (default false)."},
			},
			Required: []string{"to", "question"},
		},
	},
	{
		Name:        "fleet_set_name",
		Description: "Set a friendly name for this agent session so peers can address it easily.",
		InputSchema: toolInputSchema{
			Type: "object",
			Properties: map[string]toolSchemaProp{
				"name":       {Type: "string", Description: "Friendly handle for this session (e.g. auth-worker)."},
				"session_id": {Type: "string", Description: "Caller session_id if explicitly provided."},
			},
			Required: []string{"name"},
		},
	},
	{
		Name:        "fleet_listen",
		Description: "Check, start, or stop real-time background inbox listening for this session.",
		InputSchema: toolInputSchema{
			Type: "object",
			Properties: map[string]toolSchemaProp{
				"action":     {Type: "string", Enum: []string{"status", "start", "stop"}, Description: "Listener action (default status)."},
				"session_id": {Type: "string", Description: "Caller session_id if explicitly provided."},
			},
		},
	},
	{
		Name:        "fleet_memory_search",
		Description: "Search shared fleet memories across devices and harnesses using FTS5, or list recent memories. Defaults to global + current repository scopes.",
		InputSchema: toolInputSchema{
			Type: "object",
			Properties: map[string]toolSchemaProp{
				"query":      {Type: "string", Description: "Search keywords (empty lists recent active memories)."},
				"scope":      {Type: "string", Description: "Scope filter: 'current' (default: global + current repo), 'global', 'repo:<name>', 'device:<name>', or 'all'."},
				"kind":       {Type: "string", Enum: memory.ValidKinds, Description: "Optional memory kind filter."},
				"tier":       {Type: "string", Enum: memory.ValidTiers, Description: "Optional injection tier filter."},
				"limit":      {Type: "integer", Description: "Maximum results to return (default 20, max 100)."},
				"session_id": {Type: "string", Description: "Caller session_id if explicitly provided (used to resolve current repository scope when the MCP server cwd is not a git repo)."},
			},
		},
	},
	{
		Name:        "fleet_memory_get",
		Description: "Fetch the full body and optional revision history of a shared fleet memory entry.",
		InputSchema: toolInputSchema{
			Type: "object",
			Properties: map[string]toolSchemaProp{
				"scope":           {Type: "string", Description: "Memory scope ('global', 'repo:<name>', or 'device:<name>'). Defaults to 'global' (or parse '<scope>/<key>')."},
				"key":             {Type: "string", Description: "Memory key slug (or '<scope>/<key>')."},
				"include_history": {Type: "boolean", Description: "Include recent revision history."},
			},
			Required: []string{"key"},
		},
	},
	{
		Name:        "fleet_memory_set",
		Description: "Create or update a durable shared fleet memory on the lighthouse. Save standing cross-fleet preferences in scope='global' (tier='always'), or repo conventions/facts in scope='repo:<name>' (default). Never save ephemeral task state.",
		InputSchema: toolInputSchema{
			Type: "object",
			Properties: map[string]toolSchemaProp{
				"key":        {Type: "string", Description: "Short stable slug (e.g. 'prefer-few-prs')."},
				"body":       {Type: "string", Description: "Durable preference rule, convention, or reference text."},
				"scope":      {Type: "string", Description: "Target scope ('global', 'repo:<name>', or 'device:<name>'). Defaults to current repo (or 'global')."},
				"kind":       {Type: "string", Enum: memory.ValidKinds, Description: "Memory category: preference, feedback, project, or reference."},
				"tier":       {Type: "string", Enum: memory.ValidTiers, Description: "'always' (auto-injected at SessionStart) or 'searchable' (index + FTS5)."},
				"summary":    {Type: "string", Description: "Optional concise single-line summary (<=200 chars) for SessionStart injection."},
				"session_id": {Type: "string", Description: "Caller session_id if explicitly provided."},
			},
			Required: []string{"key", "body"},
		},
	},
	{
		Name:        "fleet_memory_delete",
		Description: "Archive (soft-delete) an obsolete or mistaken shared fleet memory entry.",
		InputSchema: toolInputSchema{
			Type: "object",
			Properties: map[string]toolSchemaProp{
				"scope":      {Type: "string", Description: "Memory scope ('global', 'repo:<name>', or 'device:<name>'). Defaults to current repo (or 'global')."},
				"key":        {Type: "string", Description: "Memory key slug to archive."},
				"session_id": {Type: "string", Description: "Caller session_id if explicitly provided."},
			},
			Required: []string{"key"},
		},
	},
}

// ServeOpts configures ServeStdio.
type ServeOpts struct {
	Harness           string
	Stdin             io.Reader
	Stdout            io.Writer
	APIURL            string
	CWD               string
	HeartbeatInterval float64
	AutoListen        *bool // nil defaults to true
	CodexRunQueueFn   waiter.RunQueueFn
}

func sendHeartbeat(apiURL, harness string, ppid int, cwd, knownFullID string) string {
	api, err := client.ResolveAPI(apiURL)
	if err != nil {
		return knownFullID
	}
	repo, branch, gitStatus := hooks.GitContext(cwd)
	summary, tail := hooks.ReadTranscriptTail(harness, "", cwd, 20)
	title, isExplicit := hooks.ReadSessionTitle(harness, knownFullID, "", "", "")
	var titlePtr, namePtr, sumPtr *string
	if title != "" {
		titlePtr = &title
		if isExplicit {
			namePtr = &title
		}
	}
	if summary != "" {
		sumPtr = &summary
	}
	var resp agents.HeartbeatResult
	err = client.Call(api, "POST", "/v1/agents/heartbeat", agents.HeartbeatParams{
		Harness:      harness,
		RawID:        fmt.Sprintf("pid-%d", ppid),
		PID:          &ppid,
		CWD:          cwd,
		Repo:         repo,
		Branch:       branch,
		State:        "working",
		Title:        titlePtr,
		Name:         namePtr,
		ExplicitName: isExplicit && title != "",
		Summary:      sumPtr,
		GitStatus:    &gitStatus,
		Tail:         tail,
	}, 2*time.Second, nil, &resp)
	if err == nil && resp.FullRawID != "" {
		return resp.FullRawID
	}
	return knownFullID
}

func deregister(apiURL, harness string, ppid int) {
	api, err := client.ResolveAPI(apiURL)
	if err != nil {
		return
	}
	_ = client.Call(api, "POST", "/v1/agents/deregister", map[string]any{"harness": harness, "pid": ppid}, 2*time.Second, nil, nil)
}

func resolveSIDNonDestructive(api, harness, cwd string, ppid int, explicitSID string) (string, error) {
	if explicitSID != "" {
		return explicitSID, nil
	}
	var resp struct {
		SessionID string `json:"session_id"`
	}
	if err := client.Call(api, "POST", "/v1/agents/inbox", map[string]any{
		"harness":     harness,
		"cwd":         cwd,
		"pid":         ppid,
		"unread_only": false,
		"mark_read":   false,
	}, 30*time.Second, nil, &resp); err != nil {
		return "", err
	}
	return resp.SessionID, nil
}

func fetchAgentSession(api, sid string) *agents.SessionDetail {
	if sid == "" {
		return nil
	}
	var resp struct {
		Agent agents.SessionDetail `json:"agent"`
	}
	if err := client.Call(api, "GET", "/v1/agents/"+url.PathEscape(sid), nil, 30*time.Second, nil, &resp); err != nil {
		return nil
	}
	return &resp.Agent
}

func decodeArgs[T any](raw json.RawMessage) (T, error) {
	var args T
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return args, nil
	}
	err := json.Unmarshal(raw, &args)
	return args, err
}

func callJSON(api, method, path string, body any, timeout time.Duration) (any, error) {
	var out map[string]any
	if err := client.Call(api, method, path, body, timeout, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func callListenTool(api, harness, cwd string, ppid int, listener *waiter.CodexQueueListener, action, explicitSID string) (any, error) {
	action = cmp.Or(action, "status")
	if action != "status" && action != "start" && action != "stop" {
		return nil, fmt.Errorf("invalid action %q", action)
	}
	if harness == "codex" {
		return callCodexListen(api, cwd, ppid, listener, action, explicitSID), nil
	}
	sid, err := resolveSIDNonDestructive(api, harness, cwd, ppid, explicitSID)
	if err != nil {
		return nil, err
	}
	sess := fetchAgentSession(api, sid)
	var sessName string
	var listening bool
	delivery := "offline"
	if sess != nil {
		sessName = sess.Name
		listening = sess.Listening
		delivery = cmp.Or(sess.Delivery, "offline")
	}
	hint := "Run this command in the background with run_command(WaitMsBeforeAsync=500, IsDaemon=false), and re-run it after handling each incoming message."
	if harness == "claude" {
		hint = "Run this command in the background with Bash(run_in_background=true), and re-run it after handling each incoming message."
	}
	return map[string]any{
		"harness": harness, "mode": "background_command", "action": action,
		"session_id": sid, "name": sessName, "listening": listening,
		"delivery": delivery, "command": "fleet agents wait --session " + sid, "instruction": hint,
	}, nil
}

func callCodexListen(api, cwd string, ppid int, listener *waiter.CodexQueueListener, action, explicitSID string) map[string]any {
	st := waiter.ListenerStatus{SessionID: explicitSID}
	if listener != nil {
		switch action {
		case "start":
			st = listener.Start()
		case "stop":
			st = listener.Pause()
		default:
			st = listener.Status()
		}
	}
	sid := cmp.Or(explicitSID, st.SessionID)
	if sid == "" {
		if resolved, err := resolveSIDNonDestructive(api, "codex", cwd, ppid, ""); err == nil {
			sid = resolved
		}
	}
	sess := fetchAgentSession(api, sid)
	var sessName string
	listening := st.Enabled
	delivery := "queued_idle"
	if st.Enabled {
		delivery = "immediate"
	}
	if sess != nil {
		sessName = sess.Name
		listening = sess.Listening
		delivery = cmp.Or(sess.Delivery, delivery)
	}
	return map[string]any{
		"harness": "codex", "mode": "mcp_codex_queue", "action": action,
		"running": st.Running, "enabled": st.Enabled, "session_id": sid,
		"full_raw_id": st.FullRawID, "delivered_count": st.DeliveredCount,
		"last_error": st.LastError, "name": sessName, "listening": listening, "delivery": delivery,
	}
}

func callListAgents(api string, rawArgs json.RawMessage) (any, error) {
	a, err := decodeArgs[struct {
		Device       string `json:"device"`
		Repo         string `json:"repo"`
		Harness      string `json:"harness"`
		State        string `json:"state"`
		IncludeStale bool   `json:"include_stale"`
	}](rawArgs)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	for k, v := range map[string]string{"device": a.Device, "repo": a.Repo, "harness": a.Harness, "state": a.State} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if a.IncludeStale {
		q.Set("include_stale", "true")
	}
	qs := ""
	if enc := q.Encode(); enc != "" {
		qs = "?" + enc
	}
	return callJSON(api, "GET", "/v1/agents"+qs, nil, 30*time.Second)
}

func callInspectAgent(api string, rawArgs json.RawMessage) (any, error) {
	a, err := decodeArgs[struct {
		AgentID   string `json:"agent_id"`
		TailLimit *int   `json:"tail_limit"`
	}](rawArgs)
	if err != nil {
		return nil, err
	}
	agentID := strings.TrimSpace(a.AgentID)
	if agentID == "" {
		return nil, errors.New("agent_id is required")
	}
	var out map[string]any
	if err := client.Call(api, "GET", "/v1/agents/"+url.PathEscape(agentID), nil, 30*time.Second, nil, &out); err != nil {
		return nil, err
	}
	if a.TailLimit != nil {
		lim := max(*a.TailLimit, 0)
		if agent, ok := out["agent"].(map[string]any); ok {
			if tail, ok := agent["tail"].([]any); ok && len(tail) > lim {
				agent["tail"] = tail[len(tail)-lim:]
			}
		}
	}
	return out, nil
}

func callAskAgent(api, harness, cwd string, ppid int, rawArgs json.RawMessage) (any, error) {
	a, err := decodeArgs[struct {
		To             string   `json:"to"`
		Question       string   `json:"question"`
		SessionID      string   `json:"session_id"`
		TimeoutSeconds *float64 `json:"timeout_seconds"`
		WaitIfIdle     bool     `json:"wait_if_idle"`
	}](rawArgs)
	if err != nil {
		return nil, err
	}
	timeoutSec := 30.0
	if a.TimeoutSeconds != nil {
		timeoutSec = *a.TimeoutSeconds
	}
	body := map[string]any{
		"to": a.To, "body": a.Question, "harness": harness, "cwd": cwd,
		"pid": ppid, "timeout": timeoutSec, "wait_if_idle": a.WaitIfIdle,
	}
	if a.SessionID != "" {
		body["session_id"] = a.SessionID
	}
	httpTimeout := max(min(timeoutSec, 120.0)+10.0, 5.0)
	return callJSON(api, "POST", "/v1/agents/ask", body, time.Duration(httpTimeout*float64(time.Second)))
}

func callTool(name string, rawArgs json.RawMessage, apiURL, harness string, ppid int, cwd string, listener *waiter.CodexQueueListener) (any, error) {
	api, err := client.ResolveAPI(apiURL)
	if err != nil {
		return nil, err
	}
	switch name {
	case "fleet_list_agents":
		return callListAgents(api, rawArgs)
	case "fleet_inspect_agent":
		return callInspectAgent(api, rawArgs)
	case "fleet_send_message":
		a, err := decodeArgs[struct {
			To        string `json:"to"`
			Message   string `json:"message"`
			SessionID string `json:"session_id"`
			ThreadID  string `json:"thread_id"`
			ReplyTo   *int64 `json:"reply_to"`
		}](rawArgs)
		if err != nil {
			return nil, err
		}
		body := map[string]any{"to": a.To, "body": a.Message, "harness": harness, "cwd": cwd, "pid": ppid}
		if a.SessionID != "" {
			body["session_id"] = a.SessionID
		}
		if a.ThreadID != "" {
			body["thread_id"] = a.ThreadID
		}
		if a.ReplyTo != nil {
			body["reply_to"] = *a.ReplyTo
		}
		return callJSON(api, "POST", "/v1/agents/send", body, 30*time.Second)

	case "fleet_check_inbox":
		a, err := decodeArgs[struct {
			SessionID  string `json:"session_id"`
			UnreadOnly *bool  `json:"unread_only"`
		}](rawArgs)
		if err != nil {
			return nil, err
		}
		body := map[string]any{"harness": harness, "cwd": cwd, "pid": ppid, "unread_only": a.UnreadOnly == nil || *a.UnreadOnly, "mark_read": true}
		if a.SessionID != "" {
			body["session_id"] = a.SessionID
		}
		return callJSON(api, "POST", "/v1/agents/inbox", body, 30*time.Second)

	case "fleet_ask_agent":
		return callAskAgent(api, harness, cwd, ppid, rawArgs)

	case "fleet_set_name":
		a, err := decodeArgs[struct {
			Name      string `json:"name"`
			SessionID string `json:"session_id"`
		}](rawArgs)
		if err != nil {
			return nil, err
		}
		body := map[string]any{"name": a.Name, "harness": harness, "cwd": cwd, "pid": ppid}
		if a.SessionID != "" {
			body["session_id"] = a.SessionID
		}
		return callJSON(api, "POST", "/v1/agents/rename", body, 30*time.Second)

	case "fleet_listen":
		a, err := decodeArgs[struct {
			Action    string `json:"action"`
			SessionID string `json:"session_id"`
		}](rawArgs)
		if err != nil {
			return nil, err
		}
		return callListenTool(api, harness, cwd, ppid, listener, a.Action, a.SessionID)

	case "fleet_memory_search", "fleet_memory_get", "fleet_memory_set", "fleet_memory_delete":
		return callMemoryTool(name, rawArgs, api, harness, cwd)

	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

type rpcRequest struct {
	ID     any             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolCallResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError"`
}

func dispatchRPC(req rpcRequest, opts ServeOpts, ppid int, cwd string, listener *waiter.CodexQueueListener) rpcResponse {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": cmp.Or(p.ProtocolVersion, "2024-11-05"),
				"capabilities":    map[string]any{"tools": struct{}{}},
				"serverInfo":      map[string]string{"name": "fleet", "version": version.Version},
			},
		}
	case "ping":
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: struct{}{}}
	case "tools/list":
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": Tools}}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		data, callErr := callTool(p.Name, p.Arguments, opts.APIURL, opts.Harness, ppid, cwd, listener)
		if callErr != nil {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: toolCallResult{
				Content: []textContent{{Type: "text", Text: callErr.Error()}},
				IsError: true,
			}}
		}
		pretty, _ := json.MarshalIndent(data, "", "  ")
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: toolCallResult{
			Content: []textContent{{Type: "text", Text: string(pretty)}},
			IsError: false,
		}}
	default:
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)}}
	}
}

func startBackgroundWorkers(opts ServeOpts, ppid int, cwd string, stopCh <-chan struct{}) (chan struct{}, *waiter.CodexQueueListener) {
	hbInterval := opts.HeartbeatInterval
	if hbInterval <= 0 {
		hbInterval = HeartbeatIntervalSeconds
	}
	var hbDone chan struct{}
	if opts.Harness == "claude" || opts.Harness == "codex" {
		hbDone = make(chan struct{})
		go func() {
			defer close(hbDone)
			knownFull := sendHeartbeat(opts.APIURL, opts.Harness, ppid, cwd, "")
			ticker := time.NewTicker(time.Duration(hbInterval * float64(time.Second)))
			defer ticker.Stop()
			for {
				select {
				case <-stopCh:
					return
				case <-ticker.C:
					knownFull = sendHeartbeat(opts.APIURL, opts.Harness, ppid, cwd, knownFull)
				}
			}
		}()
	}
	var listener *waiter.CodexQueueListener
	if opts.Harness == "codex" && (opts.AutoListen == nil || *opts.AutoListen) {
		listener = waiter.NewCodexQueueListener(waiter.CodexListenerOpts{
			APIURL: opts.APIURL, PPID: ppid, CWD: cwd, RunQueueFn: opts.CodexRunQueueFn,
		})
		listener.Start()
	}
	return hbDone, listener
}

// ServeStdio runs the newline-delimited JSON-RPC 2.0 MCP server on stdin/stdout.
func ServeStdio(opts ServeOpts) (int, error) {
	if !paths.ValidHarness(opts.Harness) {
		return 1, fmt.Errorf("unknown harness %q", opts.Harness)
	}
	stdin := opts.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	ppid := os.Getppid()
	cwd := opts.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	stopCh := make(chan struct{})
	hbDone, listener := startBackgroundWorkers(opts, ppid, cwd, stopCh)
	defer func() {
		close(stopCh)
		if listener != nil {
			listener.Stop()
		}
		if hbDone != nil {
			select {
			case <-hbDone:
			case <-time.After(1 * time.Second):
			}
			deregister(opts.APIURL, opts.Harness, ppid)
		}
	}()

	var writeMu sync.Mutex
	writeResp := func(v rpcResponse) {
		if b, err := json.Marshal(v); err == nil {
			writeMu.Lock()
			_, _ = stdout.Write(append(b, '\n'))
			writeMu.Unlock()
		}
	}

	br := bufio.NewReader(stdin)
	for {
		line, err := br.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			var req rpcRequest
			if json.Unmarshal(trimmed, &req) != nil {
				writeResp(rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
			} else if req.ID != nil {
				writeResp(dispatchRPC(req, opts, ppid, cwd, listener))
			}
		}
		if err != nil {
			break
		}
	}
	return 0, nil
}
