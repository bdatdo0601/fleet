package mcp

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/waiter"
)

func callSendMessageTool(ctx context.Context, api, harness, cwd string, ppid int, rawArgs json.RawMessage) (any, error) {
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
	return callJSON(ctx, api, "POST", "/v1/agents/send", body, 30*time.Second)
}

func callListenArgTool(ctx context.Context, api, harness, cwd string, ppid int, listener waiter.SessionListener, rawArgs json.RawMessage) (any, error) {
	a, err := decodeArgs[struct {
		Action    string `json:"action"`
		SessionID string `json:"session_id"`
	}](rawArgs)
	if err != nil {
		return nil, err
	}
	return callListenTool(ctx, api, harness, cwd, ppid, listener, a.Action, a.SessionID)
}

type dynamicToolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

var personalVaultTools = []dynamicToolDef{
	{
		Name:        "personal_vault_search",
		Description: "Search Obsidian vaults explicitly exposed to personal_devices.",
		InputSchema: toolInputSchema{
			Type: "object",
			Properties: map[string]toolSchemaProp{
				"query": {Type: "string", Description: "Search query terms."},
				"vault": {Type: "string", Description: "Optional vault name ('all', 'agent', or mounted vault name)."},
				"limit": {Type: "integer", Description: "Max results (default 20)."},
			},
			Required: []string{"query"},
		},
	},
	{
		Name:        "personal_vault_read",
		Description: "Read one Markdown note from a personal_devices-exposed Obsidian vault.",
		InputSchema: toolInputSchema{
			Type: "object",
			Properties: map[string]toolSchemaProp{
				"path":  {Type: "string", Description: "Relative Markdown note path."},
				"vault": {Type: "string", Description: "Optional vault name (default: 'agent')."},
			},
			Required: []string{"path"},
		},
	},
	{
		Name:        "personal_vault_capture",
		Description: "Write or append a note inside an auto-writable directory (e.g. Inbox/ or Daily/) of a personal_devices-exposed Obsidian vault.",
		InputSchema: toolInputSchema{
			Type: "object",
			Properties: map[string]toolSchemaProp{
				"path":    {Type: "string", Description: "Relative Markdown note path inside auto-write directory."},
				"content": {Type: "string", Description: "Markdown content to write or append."},
				"vault":   {Type: "string", Description: "Optional vault name (default: 'agent')."},
				"append":  {Type: "boolean", Description: "Append instead of overwriting (default false)."},
			},
			Required: []string{"path", "content"},
		},
	},
}

func listPersonalProfileTools(ctx context.Context, apiURL string) ([]dynamicToolDef, error) {
	api, err := client.ResolveAPI(apiURL)
	if err != nil {
		return nil, err
	}
	var res struct {
		Tools []struct {
			FullName    string         `json:"full_name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"input_schema"`
		} `json:"tools"`
	}
	if err := client.CallContext(ctx, api, "GET", "/v1/personal/mcp/tools", nil, 15*time.Second, nil, &res); err != nil {
		return nil, err
	}
	out := make([]dynamicToolDef, 0, len(personalVaultTools)+len(res.Tools))
	out = append(out, personalVaultTools...)
	for _, t := range res.Tools {
		schema := cmp.Or[any](t.InputSchema, map[string]any{"type": "object", "properties": map[string]any{}})
		out = append(out, dynamicToolDef{
			Name:        t.FullName,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	return out, nil
}

func callPersonalProfileTool(ctx context.Context, name string, rawArgs json.RawMessage, apiURL string) (any, error) {
	api, err := client.ResolveAPI(apiURL)
	if err != nil {
		return nil, err
	}
	switch name {
	case "personal_vault_search":
		a, err := decodeArgs[struct {
			Query string `json:"query"`
			Vault string `json:"vault"`
			Limit int    `json:"limit"`
		}](rawArgs)
		if err != nil {
			return nil, err
		}
		q := url.Values{"q": {strings.TrimSpace(a.Query)}}
		if a.Vault != "" {
			q.Set("vault", a.Vault)
		}
		if a.Limit > 0 {
			q.Set("limit", strconv.Itoa(a.Limit))
		}
		return callJSON(ctx, api, "GET", "/v1/personal/vault/search?"+q.Encode(), nil, 30*time.Second)
	case "personal_vault_read":
		a, err := decodeArgs[struct {
			Path  string `json:"path"`
			Vault string `json:"vault"`
		}](rawArgs)
		if err != nil {
			return nil, err
		}
		q := url.Values{"path": {strings.TrimSpace(a.Path)}}
		if a.Vault != "" {
			q.Set("vault", a.Vault)
		}
		return callJSON(ctx, api, "GET", "/v1/personal/vault/read?"+q.Encode(), nil, 30*time.Second)
	case "personal_vault_capture":
		a, err := decodeArgs[struct {
			Path    string `json:"path"`
			Content string `json:"content"`
			Vault   string `json:"vault"`
			Append  bool   `json:"append"`
		}](rawArgs)
		if err != nil {
			return nil, err
		}
		return callJSON(ctx, api, "POST", "/v1/personal/vault/capture", map[string]any{
			"path":    a.Path,
			"content": a.Content,
			"vault":   a.Vault,
			"append":  a.Append,
		}, 30*time.Second)
	default:
		if strings.HasPrefix(name, "mcp__") {
			args := map[string]any{}
			if len(rawArgs) > 0 && string(rawArgs) != "null" {
				if err := json.Unmarshal(rawArgs, &args); err != nil {
					return nil, err
				}
			}
			return callJSON(ctx, api, "POST", "/v1/personal/mcp/call", map[string]any{
				"tool":      name,
				"arguments": args,
			}, 60*time.Second)
		}
		return nil, fmt.Errorf("unknown tool %q in personal profile", name)
	}
}

func callWatchTool(ctx context.Context, api, harness, cwd string, ppid int, rawArgs json.RawMessage) (any, error) {
	a, err := decodeArgs[struct {
		Action       string   `json:"action"`
		Target       string   `json:"target"`
		TargetDevice string   `json:"target_device"`
		TargetName   string   `json:"target_name"`
		Events       []string `json:"events"`
		Once         bool     `json:"once"`
		AllowPending bool     `json:"allow_pending"`
		WatchID      string   `json:"watch_id"`
		SessionID    string   `json:"session_id"`
	}](rawArgs)
	if err != nil {
		return nil, err
	}
	action := strings.ToLower(cmp.Or(strings.TrimSpace(a.Action), "create"))
	switch action {
	case "list":
		q := url.Values{}
		if a.Target != "" {
			q.Set("target", a.Target)
		}
		qs := ""
		if enc := q.Encode(); enc != "" {
			qs = "?" + enc
		}
		return callJSON(ctx, api, "GET", "/v1/agents/watches"+qs, nil, 30*time.Second)
	case "delete":
		id := cmp.Or(strings.TrimSpace(a.WatchID), "by-target")
		q := url.Values{}
		if id == "by-target" && a.Target != "" {
			q.Set("target", a.Target)
		}
		qs := ""
		if enc := q.Encode(); enc != "" {
			qs = "?" + enc
		}
		return callJSON(ctx, api, "DELETE", "/v1/agents/watches/"+url.PathEscape(id)+qs, nil, 30*time.Second)
	case "create":
		body := map[string]any{
			"harness":       harness,
			"cwd":           cwd,
			"pid":           ppid,
			"target":        a.Target,
			"target_device": a.TargetDevice,
			"target_name":   a.TargetName,
			"events":        a.Events,
			"once":          a.Once,
			"allow_pending": a.AllowPending,
		}
		if a.SessionID != "" {
			body["session_id"] = a.SessionID
		}
		return callJSON(ctx, api, "POST", "/v1/agents/watches", body, 30*time.Second)
	default:
		return nil, fmt.Errorf("unknown fleet_watch_agent action %q", action)
	}
}

func callEventsTool(ctx context.Context, name string, rawArgs json.RawMessage, api string) (any, error) {
	if name == "fleet_events_emit" {
		a, err := decodeArgs[struct {
			Type        string `json:"type"`
			Severity    string `json:"severity"`
			Subject     string `json:"subject"`
			ChannelHint string `json:"channel_hint"`
			DedupeKey   string `json:"dedupe_key"`
			Title       string `json:"title"`
			Summary     string `json:"summary"`
		}](rawArgs)
		if err != nil {
			return nil, err
		}
		return callJSON(ctx, api, "POST", "/v1/events", map[string]any{
			"type":         a.Type,
			"severity":     a.Severity,
			"subject":      a.Subject,
			"channel_hint": a.ChannelHint,
			"dedupe_key":   a.DedupeKey,
			"title":        a.Title,
			"summary":      a.Summary,
		}, 30*time.Second)
	}
	a, err := decodeArgs[struct {
		Type     string `json:"type"`
		Severity string `json:"severity"`
		Source   string `json:"source"`
		Subject  string `json:"subject"`
		AfterID  int64  `json:"after_id"`
		Limit    int    `json:"limit"`
	}](rawArgs)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	for k, v := range map[string]string{"type": a.Type, "severity": a.Severity, "source": a.Source, "subject": a.Subject} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if a.AfterID > 0 {
		q.Set("after_id", strconv.FormatInt(a.AfterID, 10))
	}
	if a.Limit > 0 {
		q.Set("limit", strconv.Itoa(a.Limit))
	}
	qs := ""
	if enc := q.Encode(); enc != "" {
		qs = "?" + enc
	}
	return callJSON(ctx, api, "GET", "/v1/events"+qs, nil, 30*time.Second)
}
