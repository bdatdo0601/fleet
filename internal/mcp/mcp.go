// Package mcp implements the stdio MCP JSON-RPC 2.0 server bridging local agent sessions to the lighthouse API.
package mcp

import (
	"bufio"
	"bytes"
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
	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/version"
	"github.com/bdatdo0601/fleet/internal/waiter"
)

const HeartbeatIntervalSeconds = 45.0

// Tools defines the 7 MCP tools exposed to agent harnesses.
var Tools = []map[string]any{
	{
		"name": "fleet_list_agents",
		"description": "List active AI agent sessions (claude, codex, antigravity) across the Nebula fleet, " +
			"including friendly names, titles, state (working, waiting_permission, waiting_question, idle), " +
			"waiting_seconds, pending_action, listening status, and delivery modes.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"device":        map[string]any{"type": "string", "description": "Optional fleet device name filter (e.g. mini, laptop)."},
				"repo":          map[string]any{"type": "string", "description": "Optional repository name filter."},
				"harness":       map[string]any{"type": "string", "enum": paths.Harnesses, "description": "Optional harness filter."},
				"state":         map[string]any{"type": "string", "enum": []string{"working", "waiting", "waiting_permission", "waiting_question", "idle", "exited"}, "description": "Optional state filter ('waiting' matches waiting_permission and waiting_question)."},
				"include_stale": map[string]any{"type": "boolean", "description": "Include recently inactive or exited sessions."},
			},
		},
	},
	{
		"name":        "fleet_inspect_agent",
		"description": "Inspect another agent session's cwd, git branch/status, pending permission/question prompts (pending_action, waiting_seconds), and recent conversation/tool activity.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent_id":   map[string]any{"type": "string", "description": "Target session name (or device/name), title, short ID, or full session_id."},
				"tail_limit": map[string]any{"type": "integer", "description": "Optional maximum number of recent tail entries to return."},
			},
			"required": []string{"agent_id"},
		},
	},
	{
		"name":        "fleet_send_message",
		"description": "Send a message to a peer agent session, device (device:name), repo (repo:name), or all (*).",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to":         map[string]any{"type": "string", "description": "Recipient session name (or device/name), title, short id, full session_id, device name, device:<name>, repo:<name>, or '*'."},
				"message":    map[string]any{"type": "string", "description": "Message text to deliver."},
				"session_id": map[string]any{"type": "string", "description": "Caller session_id if explicitly provided."},
				"thread_id":  map[string]any{"type": "string", "description": "Optional thread_id to continue an existing thread."},
				"reply_to":   map[string]any{"type": "integer", "description": "Optional message id being replied to."},
			},
			"required": []string{"to", "message"},
		},
	},
	{
		"name":        "fleet_check_inbox",
		"description": "Check incoming peer messages for this agent session.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"session_id":  map[string]any{"type": "string", "description": "Caller session_id if explicitly provided."},
				"unread_only": map[string]any{"type": "boolean", "description": "Return only unread messages (default true)."},
			},
		},
	},
	{
		"name":        "fleet_ask_agent",
		"description": "Send a question to another agent session and wait for its reply.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to":              map[string]any{"type": "string", "description": "Target session name (or device/name), title, short id, or full session_id."},
				"question":        map[string]any{"type": "string", "description": "Question to ask the peer agent."},
				"session_id":      map[string]any{"type": "string", "description": "Caller session_id if explicitly provided."},
				"timeout_seconds": map[string]any{"type": "number", "description": "Maximum seconds to wait for a reply (default 30, max 120)."},
				"wait_if_idle":    map[string]any{"type": "boolean", "description": "Wait for a reply even if the target is currently idle with no listener (default false)."},
			},
			"required": []string{"to", "question"},
		},
	},
	{
		"name":        "fleet_set_name",
		"description": "Set a friendly name for this agent session so peers can address it easily.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":       map[string]any{"type": "string", "description": "Friendly handle for this session (e.g. auth-worker)."},
				"session_id": map[string]any{"type": "string", "description": "Caller session_id if explicitly provided."},
			},
			"required": []string{"name"},
		},
	},
	{
		"name":        "fleet_listen",
		"description": "Check, start, or stop real-time background inbox listening for this session.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":     map[string]any{"type": "string", "enum": []string{"status", "start", "stop"}, "description": "Listener action (default status)."},
				"session_id": map[string]any{"type": "string", "description": "Caller session_id if explicitly provided."},
			},
		},
	},
}

// ServeOpts configures ServeStdio.
type ServeOpts struct {
	Harness           string
	Stdin             io.Reader
	Stdout            io.Writer
	APIURL            string
	HeartbeatInterval float64
	AutoListen        *bool // nil defaults to true
	CodexRunQueueFn   waiter.RunQueueFn
}

func resolveAPI(apiURL string) (string, error) {
	if apiURL != "" {
		return apiURL, nil
	}
	data, err := os.ReadFile(paths.NodeFile())
	if err != nil {
		return "", &client.ApiError{Msg: "this machine hasn't joined a fleet (fleet join NAME)"}
	}
	var node struct {
		API string `json:"api"`
	}
	if err := json.Unmarshal(data, &node); err != nil || node.API == "" {
		return "", &client.ApiError{Msg: "invalid fleet.json"}
	}
	return node.API, nil
}

func sendHeartbeat(apiURL, harness string, ppid int, cwd, knownFullID string) string {
	api, err := resolveAPI(apiURL)
	if err != nil {
		return knownFullID
	}
	repo, branch, gitStatus := hooks.GitContext(cwd)
	summary, tail := hooks.ReadTranscriptTail(harness, "", cwd, 20)
	title, isExplicit := hooks.ReadSessionTitle(harness, knownFullID, "", "", "")
	payload := map[string]any{
		"harness":     harness,
		"raw_id":      fmt.Sprintf("pid-%d", ppid),
		"pid":         ppid,
		"cwd":         cwd,
		"repo":        repo,
		"branch":      branch,
		"state":       "working",
		"git_status":  gitStatus,
		"drain_inbox": false,
	}
	if title != "" {
		payload["title"] = title
	}
	if isExplicit && title != "" {
		payload["name"] = title
		payload["explicit_name"] = true
	}
	if summary != "" {
		payload["summary"] = summary
	}
	if len(tail) > 0 {
		payload["tail"] = tail
	}
	var resp agents.HeartbeatResult
	if err := client.Call(api, "POST", "/v1/agents/heartbeat", payload, 2*time.Second, nil, &resp); err == nil && resp.FullRawID != "" {
		return resp.FullRawID
	}
	return knownFullID
}

func deregister(apiURL, harness string, ppid int) {
	api, err := resolveAPI(apiURL)
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

func argString(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func argBool(args map[string]any, key string, dflt bool) bool {
	v, ok := args[key]
	if !ok || v == nil {
		return dflt
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return dflt
}

func argFloat(args map[string]any, key string, dflt float64) float64 {
	v, ok := args[key]
	if !ok || v == nil {
		return dflt
	}
	if f, ok := v.(float64); ok {
		return f
	}
	return dflt
}

func argInt(args map[string]any, key string) (int, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

func callTool(
	name string,
	args map[string]any,
	apiURL, harness string,
	ppid int,
	cwd string,
	listener *waiter.CodexQueueListener,
) (any, error) {
	api, err := resolveAPI(apiURL)
	if err != nil {
		return nil, err
	}
	switch name {
	case "fleet_list_agents":
		q := url.Values{}
		for _, k := range []string{"device", "repo", "harness", "state"} {
			if val := argString(args, k); val != "" {
				q.Set(k, val)
			}
		}
		if argBool(args, "include_stale", false) {
			q.Set("include_stale", "true")
		}
		qs := ""
		if enc := q.Encode(); enc != "" {
			qs = "?" + enc
		}
		var out map[string]any
		if err := client.Call(api, "GET", "/v1/agents"+qs, nil, 30*time.Second, nil, &out); err != nil {
			return nil, err
		}
		return out, nil

	case "fleet_inspect_agent":
		agentID := strings.TrimSpace(argString(args, "agent_id"))
		if agentID == "" {
			return nil, errors.New("agent_id is required")
		}
		var out map[string]any
		if err := client.Call(api, "GET", "/v1/agents/"+url.PathEscape(agentID), nil, 30*time.Second, nil, &out); err != nil {
			return nil, err
		}
		if lim, ok := argInt(args, "tail_limit"); ok {
			if lim < 0 {
				lim = 0
			}
			if agent, ok := out["agent"].(map[string]any); ok {
				if tail, ok := agent["tail"].([]any); ok {
					if lim == 0 {
						agent["tail"] = []any{}
					} else if len(tail) > lim {
						agent["tail"] = tail[len(tail)-lim:]
					}
				}
			}
		}
		return out, nil

	case "fleet_send_message":
		body := map[string]any{
			"to":      argString(args, "to"),
			"body":    argString(args, "message"),
			"harness": harness,
			"cwd":     cwd,
			"pid":     ppid,
		}
		if sid := argString(args, "session_id"); sid != "" {
			body["session_id"] = sid
		}
		if tid := argString(args, "thread_id"); tid != "" {
			body["thread_id"] = tid
		}
		if rt, ok := argInt(args, "reply_to"); ok {
			body["reply_to"] = rt
		}
		var out map[string]any
		if err := client.Call(api, "POST", "/v1/agents/send", body, 30*time.Second, nil, &out); err != nil {
			return nil, err
		}
		return out, nil

	case "fleet_check_inbox":
		body := map[string]any{
			"harness":     harness,
			"cwd":         cwd,
			"pid":         ppid,
			"unread_only": argBool(args, "unread_only", true),
			"mark_read":   true,
		}
		if sid := argString(args, "session_id"); sid != "" {
			body["session_id"] = sid
		}
		var out map[string]any
		if err := client.Call(api, "POST", "/v1/agents/inbox", body, 30*time.Second, nil, &out); err != nil {
			return nil, err
		}
		return out, nil

	case "fleet_ask_agent":
		timeoutSec := argFloat(args, "timeout_seconds", 30.0)
		body := map[string]any{
			"to":           argString(args, "to"),
			"body":         argString(args, "question"),
			"harness":      harness,
			"cwd":          cwd,
			"pid":          ppid,
			"timeout":      timeoutSec,
			"wait_if_idle": argBool(args, "wait_if_idle", false),
		}
		if sid := argString(args, "session_id"); sid != "" {
			body["session_id"] = sid
		}
		httpTimeout := timeoutSec
		if httpTimeout > 120.0 {
			httpTimeout = 120.0
		}
		httpTimeout += 10.0
		if httpTimeout < 5.0 {
			httpTimeout = 5.0
		}
		var out map[string]any
		if err := client.Call(api, "POST", "/v1/agents/ask", body, time.Duration(httpTimeout*float64(time.Second)), nil, &out); err != nil {
			return nil, err
		}
		return out, nil

	case "fleet_set_name":
		body := map[string]any{
			"name":    argString(args, "name"),
			"harness": harness,
			"cwd":     cwd,
			"pid":     ppid,
		}
		if sid := argString(args, "session_id"); sid != "" {
			body["session_id"] = sid
		}
		var out map[string]any
		if err := client.Call(api, "POST", "/v1/agents/rename", body, 30*time.Second, nil, &out); err != nil {
			return nil, err
		}
		return out, nil

	case "fleet_listen":
		action := argString(args, "action")
		if action == "" {
			action = "status"
		}
		if action != "status" && action != "start" && action != "stop" {
			return nil, fmt.Errorf("invalid action %q", action)
		}
		explicitSID := argString(args, "session_id")
		if harness == "codex" {
			var st waiter.ListenerStatus
			if listener != nil {
				switch action {
				case "start":
					st = listener.Start()
				case "stop":
					st = listener.Pause()
				default:
					st = listener.Status()
				}
			} else {
				st = waiter.ListenerStatus{SessionID: explicitSID}
			}
			sid := explicitSID
			if sid == "" {
				sid = st.SessionID
			}
			if sid == "" {
				if resolved, err := resolveSIDNonDestructive(api, harness, cwd, ppid, ""); err == nil {
					sid = resolved
				}
			}
			var sess map[string]any
			if sid != "" {
				var resp map[string]any
				if err := client.Call(api, "GET", "/v1/agents/"+url.PathEscape(sid), nil, 30*time.Second, nil, &resp); err == nil {
					sess, _ = resp["agent"].(map[string]any)
				}
			}
			sessName, _ := sess["name"].(string)
			listening, hasL := sess["listening"].(bool)
			if !hasL {
				listening = st.Enabled
			}
			delivery, _ := sess["delivery"].(string)
			if delivery == "" {
				if st.Enabled {
					delivery = "immediate"
				} else {
					delivery = "queued_idle"
				}
			}
			return map[string]any{
				"harness":         "codex",
				"mode":            "mcp_codex_queue",
				"action":          action,
				"running":         st.Running,
				"enabled":         st.Enabled,
				"session_id":      sid,
				"full_raw_id":     st.FullRawID,
				"delivered_count": st.DeliveredCount,
				"last_error":      st.LastError,
				"name":            sessName,
				"listening":       listening,
				"delivery":        delivery,
			}, nil
		}

		sid, err := resolveSIDNonDestructive(api, harness, cwd, ppid, explicitSID)
		if err != nil {
			return nil, err
		}
		var sess map[string]any
		var resp map[string]any
		if err := client.Call(api, "GET", "/v1/agents/"+url.PathEscape(sid), nil, 30*time.Second, nil, &resp); err == nil {
			sess, _ = resp["agent"].(map[string]any)
		}
		sessName, _ := sess["name"].(string)
		listening, _ := sess["listening"].(bool)
		delivery, _ := sess["delivery"].(string)
		if delivery == "" {
			delivery = "offline"
		}
		cmd := "fleet agents wait --session " + sid
		hint := "Run this command in the background with run_command(WaitMsBeforeAsync=500, IsDaemon=false), and re-run it after handling each incoming message."
		if harness == "claude" {
			hint = "Run this command in the background with Bash(run_in_background=true), and re-run it after handling each incoming message."
		}
		return map[string]any{
			"harness":     harness,
			"mode":        "background_command",
			"action":      action,
			"session_id":  sid,
			"name":        sessName,
			"listening":   listening,
			"delivery":    delivery,
			"command":     cmd,
			"instruction": hint,
		}, nil

	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

func jsonrpcResult(reqID, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": reqID, "result": result}
}

func jsonrpcError(reqID any, code int, message string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      reqID,
		"error":   map[string]any{"code": code, "message": message},
	}
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
	hbInterval := opts.HeartbeatInterval
	if hbInterval <= 0 {
		hbInterval = HeartbeatIntervalSeconds
	}
	autoListen := true
	if opts.AutoListen != nil {
		autoListen = *opts.AutoListen
	}
	ppid := os.Getppid()
	cwd, _ := os.Getwd()

	stopCh := make(chan struct{})
	var hbDone chan struct{}
	var listener *waiter.CodexQueueListener

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

	if opts.Harness == "codex" && autoListen {
		listener = waiter.NewCodexQueueListener(waiter.CodexListenerOpts{
			APIURL:     opts.APIURL,
			PPID:       ppid,
			CWD:        cwd,
			RunQueueFn: opts.CodexRunQueueFn,
		})
		listener.Start()
	}

	var writeMu sync.Mutex
	writeResp := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		writeMu.Lock()
		_, _ = stdout.Write(append(b, '\n'))
		writeMu.Unlock()
	}

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

	br := bufio.NewReader(stdin)
	for {
		line, err := br.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			var raw any
			if jErr := json.Unmarshal(trimmed, &raw); jErr != nil {
				writeResp(jsonrpcError(nil, -32700, "parse error"))
			} else if req, ok := raw.(map[string]any); ok {
				reqID, hasID := req["id"]
				if hasID && reqID != nil {
					method, _ := req["method"].(string)
					params, _ := req["params"].(map[string]any)
					if params == nil {
						params = map[string]any{}
					}
					switch method {
					case "initialize":
						proto, _ := params["protocolVersion"].(string)
						if proto == "" {
							proto = "2024-11-05"
						}
						writeResp(jsonrpcResult(reqID, map[string]any{
							"protocolVersion": proto,
							"capabilities":    map[string]any{"tools": map[string]any{}},
							"serverInfo":      map[string]any{"name": "fleet", "version": version.Version},
						}))
					case "ping":
						writeResp(jsonrpcResult(reqID, map[string]any{}))
					case "tools/list":
						writeResp(jsonrpcResult(reqID, map[string]any{"tools": Tools}))
					case "tools/call":
						toolName, _ := params["name"].(string)
						toolArgs, _ := params["arguments"].(map[string]any)
						if toolArgs == nil {
							toolArgs = map[string]any{}
						}
						data, callErr := callTool(toolName, toolArgs, opts.APIURL, opts.Harness, ppid, cwd, listener)
						if callErr != nil {
							writeResp(jsonrpcResult(reqID, map[string]any{
								"content": []map[string]any{{"type": "text", "text": callErr.Error()}},
								"isError": true,
							}))
						} else {
							pretty, _ := json.MarshalIndent(data, "", "  ")
							writeResp(jsonrpcResult(reqID, map[string]any{
								"content": []map[string]any{{"type": "text", "text": string(pretty)}},
								"isError": false,
							}))
						}
					default:
						writeResp(jsonrpcError(reqID, -32601, fmt.Sprintf("method not found: %s", method)))
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	return 0, nil
}
