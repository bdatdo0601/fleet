package assistant

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// FleetDeps abstracts Fleet subsystem access so assistant does not import core/slacker/agents directly.
type FleetDeps interface {
	ListAgents(device, repo, harness, state string, includeStale bool) (any, error)
	InspectAgent(target string) (any, error)
	SendAgentMessage(fromDevice, to, body string) (any, error)
	MemorySearch(query, scope, kind, tier string, limit int) (any, error)
	MemoryGet(scope, key string) (any, error)
	MemorySet(scope, key, kind, tier, summary, body string) (any, error)
	MemoryContext(repo, device string) (string, error)
	SlackChannels(ctx context.Context, account string, includeAll bool, limit int) (any, error)
	SlackRead(ctx context.Context, account, channel, threadTS, query string, limit int) (any, error)
	SlackSend(ctx context.Context, account, channel, text, threadTS string) (any, error)
}

type toolExecContext struct {
	ChannelID  string
	Source     string
	Config     Config
	BypassHITL bool
	Widgets    *[]WidgetPayload
}

type approvalRequiredError struct {
	ToolName   string
	ServerName string
	Summary    string
	RiskLevel  string
	ArgsJSON   string
}

func (e *approvalRequiredError) Error() string { return "approval required: " + e.Summary }

func schemaObj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func makeToolSpec(name, desc string, params map[string]any) ToolSpec {
	return ToolSpec{
		Type:     "function",
		Function: ToolFunctionSpec{Name: name, Description: desc, Parameters: params},
	}
}

func (e *Engine) buildToolSpecs(ctx context.Context) []ToolSpec {
	specs := []ToolSpec{
		makeToolSpec("fleet_list_agents", "List active Fleet AI agent sessions across Macs.", schemaObj(map[string]any{
			"device": strProp("Optional device filter"), "repo": strProp("Optional repo filter"),
			"harness": strProp("Optional harness filter"), "state": strProp("Optional state filter (working, waiting, idle)"),
			"include_stale": boolProp("Include recently exited/stale sessions"),
		})),
		makeToolSpec("fleet_inspect_agent", "Inspect one Fleet agent session's status, pending prompt, and tail.", schemaObj(map[string]any{
			"agent_id": strProp("Session name, device/name, title, or session_id"),
		}, "agent_id")),
		makeToolSpec("fleet_send_message", "Send a message to a peer Fleet agent session or device.", schemaObj(map[string]any{
			"to": strProp("Target session name, device/name, repo:name, or *"), "message": strProp("Message text"),
		}, "to", "message")),
		makeToolSpec("fleet_memory_search", "Search shared cross-device Fleet memories.", schemaObj(map[string]any{
			"query": strProp("Search keywords"), "scope": strProp("global, repo:<name>, device:<name>, or all"),
			"kind": strProp("preference|feedback|project|reference"), "tier": strProp("always|searchable"), "limit": intProp("Max results"),
		})),
		makeToolSpec("fleet_memory_get", "Read full body of one shared Fleet memory.", schemaObj(map[string]any{
			"scope": strProp("Memory scope"), "key": strProp("Memory key"),
		}, "scope", "key")),
		makeToolSpec("fleet_memory_set", "Save or update a shared cross-device Fleet memory.", schemaObj(map[string]any{
			"scope": strProp("Target scope (global, repo:<name>, device:<name>)"), "key": strProp("Slug key"),
			"kind": strProp("preference|feedback|project|reference"), "tier": strProp("always|searchable"),
			"summary": strProp("Concise 1-line summary"), "body": strProp("Full markdown body"),
		}, "scope", "key", "body")),
		makeToolSpec("fleet_slack_channels", "List configured Slack accounts and mapped channels.", schemaObj(map[string]any{
			"account": strProp("Optional Slack account"), "include_all": boolProp("Query Slack API for all channels"), "limit": intProp("Limit"),
		})),
		makeToolSpec("fleet_slack_read", "Read messages or threads from a Slack channel.", schemaObj(map[string]any{
			"account": strProp("Optional account"), "channel": strProp("Channel ID"), "thread_ts": strProp("Thread timestamp"),
			"query": strProp("Search query"), "limit": intProp("Max messages"),
		})),
		makeToolSpec("fleet_slack_send", "Post a Slack message or thread reply (requires human approval).", schemaObj(map[string]any{
			"account": strProp("Optional account"), "channel": strProp("Channel ID"), "text": strProp("Message text"), "thread_ts": strProp("Optional thread_ts"),
		}, "channel", "text")),
		makeToolSpec("skill_list", "List available SKILL.md playbooks in the Obsidian vault.", schemaObj(map[string]any{})),
		makeToolSpec("skill_read", "Read the full SKILL.md instructions for a named skill.", schemaObj(map[string]any{
			"name": strProp("Skill slug"),
		}, "name")),
		makeToolSpec("skill_write", "Create or update a SKILL.md playbook (direct user chat only).", schemaObj(map[string]any{
			"name": strProp("Skill slug (lowercase alphanumeric, - or _)"), "content": strProp("Full SKILL.md markdown including YAML frontmatter"),
		}, "name", "content")),
		makeToolSpec("vault_search", "Search Markdown notes across the Obsidian vault.", schemaObj(map[string]any{
			"query": strProp("Search query terms"), "limit": intProp("Max results"),
		}, "query")),
		makeToolSpec("vault_read", "Read one Markdown file from the Obsidian vault.", schemaObj(map[string]any{
			"path": strProp("Relative path inside vault"),
		}, "path")),
		makeToolSpec("vault_append", "Append text to a Markdown file in the vault (requires approval outside auto-write folders).", schemaObj(map[string]any{
			"path": strProp("Relative path inside vault"), "content": strProp("Markdown text to append"),
		}, "path", "content")),
		makeToolSpec("vault_write", "Write a Markdown file in the vault (requires approval outside auto-write folders).", schemaObj(map[string]any{
			"path": strProp("Relative path inside vault"), "content": strProp("Full Markdown content"),
		}, "path", "content")),
		makeToolSpec("timer_schedule", "Create a one-shot or recurring timer wakeup for the assistant.", schemaObj(map[string]any{
			"name": strProp("Short timer name"), "channel_id": strProp("Target channel (default briefing)"),
			"prompt": strProp("Prompt to execute when timer fires"), "schedule_kind": strProp("once or interval"),
			"fire_at": strProp("RFC3339 UTC timestamp for once"), "interval_seconds": intProp("Repeat interval in seconds"),
			"model": strProp("Optional model override"),
		}, "name", "prompt")),
		makeToolSpec("timer_list", "List scheduled assistant timers.", schemaObj(map[string]any{})),
		makeToolSpec("timer_cancel", "Delete a scheduled timer by id.", schemaObj(map[string]any{"id": strProp("Timer ID")}, "id")),
		makeToolSpec("outbox_notify", "Queue a proactive notification card/outbox item.", schemaObj(map[string]any{
			"title": strProp("Short title"), "body": strProp("Notification body"), "priority": strProp("low|normal|urgent"),
		}, "body")),
		makeToolSpec("render_card", "Attach a typed native UI card (agents, agenda, choices, key_value, markdown) to this reply.", schemaObj(map[string]any{
			"kind": strProp("agents|agenda|choices|key_value|markdown"), "title": strProp("Card title"),
			"data": map[string]any{"type": "object", "description": "Structured card payload"},
		}, "kind", "data")),
		makeToolSpec("render_widget", "Attach an interactive sandboxed HTML/CSS/SVG Generative UI widget to this reply.", schemaObj(map[string]any{
			"title": strProp("Widget title"), "html": strProp("Self-contained HTML snippet (inline <style>/<script> allowed; no external URLs)"),
		}, "title", "html")),
		makeToolSpec("credential_list_aliases", "List configured 1Password credential aliases and allowlisted hosts (no secrets returned).", schemaObj(map[string]any{})),
		makeToolSpec("onepassword_list_vaults", "List 1Password vaults accessible to the assistant Service Account.", schemaObj(map[string]any{})),
		makeToolSpec("onepassword_list_items", "List items in a 1Password vault (titles, categories, URLs, tags — no secrets).", schemaObj(map[string]any{
			"vault": strProp("Optional vault name or ID"),
		})),
		makeToolSpec("onepassword_get_item_meta", "Get field labels, URLs, and op:// references for a 1Password item (zero secrets returned).", schemaObj(map[string]any{
			"item": strProp("Item title or ID"), "vault": strProp("Optional vault name or ID"),
		}, "item")),
		makeToolSpec("onepassword_store_item", "Create a new credential item in a scoped 1Password vault (approval-gated).", schemaObj(map[string]any{
			"vault": strProp("Target 1Password vault"), "title": strProp("Item title"),
			"category": strProp("Category (default API Credential)"), "url": strProp("Associated API/website URL"),
			"fields": map[string]any{"type": "object", "description": "Field label->value pairs"},
		}, "vault", "title")),
		makeToolSpec("http_request_with_credential", "Perform an HTTP request injecting a 1Password secret (via configured alias or op_ref+item) into an allowlisted host without exposing the secret.", schemaObj(map[string]any{
			"alias": strProp("Configured credential alias"), "op_ref": strProp("op://Vault/Item/field reference"),
			"item": strProp("1Password item name (required with op_ref to enforce URL host allowlist)"), "vault": strProp("Optional 1Password vault"),
			"method": strProp("HTTP method"), "url": strProp("Target URL"),
			"body": strProp("Optional request body"),
		}, "url")),
	}
	if e.mcp != nil {
		for _, ext := range e.mcp.ListTools(ctx) {
			specs = append(specs, makeToolSpec(ext.FullName, ext.Description, ext.InputSchema))
		}
	}
	return specs
}

func decodeToolArgs(raw string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, err
	}
	return m, nil
}

func strArg(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return strings.TrimSpace(v)
}

func boolArg(m map[string]any, key string) bool {
	v, _ := m[key].(bool)
	return v
}

func intArg(m map[string]any, key string, defVal int) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return defVal
	}
}

func (e *Engine) executeTool(ctx context.Context, tc ToolCall, tctx *toolExecContext) (string, error) {
	args, err := decodeToolArgs(tc.Function.Arguments)
	if err != nil {
		return "", fmt.Errorf("invalid arguments JSON: %w", err)
	}
	res, err := e.dispatchTool(ctx, tc.Function.Name, args, tc.Function.Arguments, tctx)
	if err != nil {
		return "", err
	}
	b, mErr := json.Marshal(res)
	if mErr != nil {
		return "", mErr
	}
	return string(b), nil
}

func (e *Engine) dispatchTool(ctx context.Context, name string, args map[string]any, rawArgs string, tctx *toolExecContext) (any, error) {
	if strings.HasPrefix(name, "fleet_") {
		return e.dispatchFleetTool(ctx, name, args, rawArgs, tctx)
	}
	if strings.HasPrefix(name, "skill_") || strings.HasPrefix(name, "vault_") {
		return e.dispatchVaultTool(name, args, rawArgs, tctx)
	}
	if strings.HasPrefix(name, "timer_") || name == "outbox_notify" {
		return e.dispatchAutomationTool(name, args, tctx)
	}
	if name == "render_card" || name == "render_widget" {
		return dispatchUITool(name, args, tctx)
	}
	if name == "credential_list_aliases" || name == "http_request_with_credential" || strings.HasPrefix(name, "onepassword_") {
		return e.dispatchCredentialTool(ctx, name, args, rawArgs, tctx)
	}
	if strings.HasPrefix(name, "mcp__") && e.mcp != nil {
		ext, ok := e.mcp.FindTool(ctx, name)
		if !ok {
			return nil, newErr(fmt.Sprintf("external MCP tool %q not found", name), http.StatusNotFound)
		}
		if ext.RequireApproval && !tctx.BypassHITL {
			return nil, &approvalRequiredError{
				ToolName:   name,
				ServerName: ext.ServerName,
				Summary:    fmt.Sprintf("Execute external MCP tool %s", name),
				RiskLevel:  "external",
				ArgsJSON:   cmp.Or(rawArgs, "{}"),
			}
		}
		return e.mcp.CallTool(ctx, name, args)
	}
	return nil, newErr(fmt.Sprintf("unknown tool %q", name), http.StatusBadRequest)
}

func (e *Engine) dispatchFleetTool(ctx context.Context, name string, args map[string]any, rawArgs string, tctx *toolExecContext) (any, error) {
	if e.deps == nil {
		return nil, newErr("fleet dependencies unavailable", http.StatusServiceUnavailable)
	}
	switch name {
	case "fleet_list_agents":
		return e.deps.ListAgents(strArg(args, "device"), strArg(args, "repo"), strArg(args, "harness"), strArg(args, "state"), boolArg(args, "include_stale"))
	case "fleet_inspect_agent":
		return e.deps.InspectAgent(strArg(args, "agent_id"))
	case "fleet_send_message":
		return e.deps.SendAgentMessage("assistant", strArg(args, "to"), strArg(args, "message"))
	case "fleet_memory_search":
		return e.deps.MemorySearch(strArg(args, "query"), cmp.Or(strArg(args, "scope"), "all"), strArg(args, "kind"), strArg(args, "tier"), intArg(args, "limit", 20))
	case "fleet_memory_get":
		return e.deps.MemoryGet(strArg(args, "scope"), strArg(args, "key"))
	case "fleet_memory_set":
		return e.deps.MemorySet(strArg(args, "scope"), strArg(args, "key"), strArg(args, "kind"), strArg(args, "tier"), strArg(args, "summary"), strArg(args, "body"))
	case "fleet_slack_channels":
		return e.deps.SlackChannels(ctx, strArg(args, "account"), boolArg(args, "include_all"), intArg(args, "limit", 50))
	case "fleet_slack_read":
		return e.deps.SlackRead(ctx, strArg(args, "account"), strArg(args, "channel"), strArg(args, "thread_ts"), strArg(args, "query"), intArg(args, "limit", 20))
	case "fleet_slack_send":
		if !tctx.BypassHITL {
			return nil, &approvalRequiredError{
				ToolName:  name,
				Summary:   fmt.Sprintf("Send Slack message to %s", strArg(args, "channel")),
				RiskLevel: "external_write",
				ArgsJSON:  cmp.Or(rawArgs, "{}"),
			}
		}
		return e.deps.SlackSend(ctx, strArg(args, "account"), strArg(args, "channel"), strArg(args, "text"), strArg(args, "thread_ts"))
	default:
		return nil, newErr("unknown fleet tool "+name, http.StatusBadRequest)
	}
}

func (e *Engine) dispatchVaultTool(name string, args map[string]any, rawArgs string, tctx *toolExecContext) (any, error) {
	v, err := e.vault()
	if err != nil {
		return nil, err
	}
	switch name {
	case "skill_list":
		return v.ListSkills()
	case "skill_read":
		return v.GetSkill(strArg(args, "name"))
	case "skill_write":
		if tctx.Source != "chat" && tctx.Source != "approval" && !tctx.BypassHITL {
			return nil, newErr("skill_write is only allowed during direct user chat", http.StatusForbidden)
		}
		sk, err := v.SaveSkill(strArg(args, "name"), strArg(args, "content"))
		if err != nil {
			return nil, err
		}
		v.MaybeGitCommit(tctx.Config.GitAutoCommit, "assistant: update skill "+sk.Name)
		return sk, nil
	case "vault_search":
		return v.Search(strArg(args, "query"), intArg(args, "limit", 15))
	case "vault_read":
		content, err := v.ReadFile(strArg(args, "path"))
		if err != nil {
			return nil, err
		}
		return map[string]string{"path": strArg(args, "path"), "content": content}, nil
	case "vault_append", "vault_write":
		rel := strArg(args, "path")
		if !tctx.BypassHITL && !IsAutoWritable(rel, tctx.Config.AutoWriteDirs) {
			return nil, &approvalRequiredError{
				ToolName:  name,
				Summary:   fmt.Sprintf("Modify protected vault note %s", rel),
				RiskLevel: "vault_write",
				ArgsJSON:  cmp.Or(rawArgs, "{}"),
			}
		}
		var norm string
		var wErr error
		if name == "vault_append" {
			norm, wErr = v.AppendFile(rel, strArg(args, "content"))
		} else {
			norm, wErr = v.WriteFile(rel, strArg(args, "content"))
		}
		if wErr != nil {
			return nil, wErr
		}
		v.MaybeGitCommit(tctx.Config.GitAutoCommit, "assistant: update "+norm)
		return map[string]string{"path": norm, "status": "saved"}, nil
	default:
		return nil, newErr("unknown vault tool "+name, http.StatusBadRequest)
	}
}

func (e *Engine) dispatchAutomationTool(name string, args map[string]any, tctx *toolExecContext) (any, error) {
	switch name {
	case "timer_schedule":
		return e.store.UpsertTimer(Timer{
			ChannelID:       cmp.Or(strArg(args, "channel_id"), "briefing"),
			Name:            strArg(args, "name"),
			Prompt:          strArg(args, "prompt"),
			Model:           strArg(args, "model"),
			ScheduleKind:    cmp.Or(strArg(args, "schedule_kind"), "interval"),
			FireAt:          strArg(args, "fire_at"),
			IntervalSeconds: intArg(args, "interval_seconds", 0),
			Enabled:         true,
		})
	case "timer_list":
		return e.store.ListTimers()
	case "timer_cancel":
		if err := e.store.DeleteTimer(strArg(args, "id")); err != nil {
			return nil, err
		}
		return map[string]bool{"ok": true}, nil
	case "outbox_notify":
		item, err := e.store.AddOutbox(tctx.ChannelID, cmp.Or(strArg(args, "priority"), "normal"), strArg(args, "title"), strArg(args, "body"))
		if err != nil {
			return nil, err
		}
		e.publishEvent(StreamEvent{Type: "outbox", ChannelID: tctx.ChannelID, Data: item})
		return item, nil
	default:
		return nil, newErr("unknown automation tool "+name, http.StatusBadRequest)
	}
}

func dispatchUITool(name string, args map[string]any, tctx *toolExecContext) (any, error) {
	var wp WidgetPayload
	if name == "render_card" {
		wp = WidgetPayload{
			Kind:  cmp.Or(strArg(args, "kind"), "markdown"),
			Title: strArg(args, "title"),
			Data:  args["data"],
		}
	} else {
		wp = WidgetPayload{
			Kind:  "html",
			Title: strArg(args, "title"),
			HTML:  strArg(args, "html"),
		}
	}
	if tctx.Widgets != nil {
		*tctx.Widgets = append(*tctx.Widgets, wp)
	}
	return map[string]any{"attached": true, "kind": wp.Kind, "title": wp.Title}, nil
}

func (e *Engine) dispatchCredentialTool(ctx context.Context, name string, args map[string]any, rawArgs string, tctx *toolExecContext) (any, error) {
	switch name {
	case "credential_list_aliases":
		return tctx.Config.View("").Credentials, nil
	case "onepassword_list_vaults":
		return e.listOPVaultsCached(ctx)
	case "onepassword_list_items":
		return ListOPItems(ctx, e.opTokenPath, strArg(args, "vault"))
	case "onepassword_get_item_meta":
		return GetOPItemMeta(ctx, e.opTokenPath, strArg(args, "item"), strArg(args, "vault"))
	case "onepassword_store_item":
		if !tctx.BypassHITL {
			return nil, &approvalRequiredError{
				ToolName:  name,
				Summary:   fmt.Sprintf("Create 1Password item %q in vault %q", strArg(args, "title"), strArg(args, "vault")),
				RiskLevel: "credential_write",
				ArgsJSON:  cmp.Or(rawArgs, "{}"),
			}
		}
		fields := map[string]string{}
		if rawFields, ok := args["fields"].(map[string]any); ok {
			for k, v := range rawFields {
				fields[k] = fmt.Sprint(v)
			}
		}
		return StoreOPItem(ctx, e.opTokenPath, strArg(args, "vault"), strArg(args, "title"), strArg(args, "category"), strArg(args, "url"), fields)
	case "http_request_with_credential":
		return e.execCredentialHTTPTool(ctx, args, rawArgs, tctx)
	default:
		return nil, newErr("unknown credential tool "+name, http.StatusBadRequest)
	}
}

func (e *Engine) execCredentialHTTPTool(ctx context.Context, args map[string]any, rawArgs string, tctx *toolExecContext) (any, error) {
	alias := strArg(args, "alias")
	opRef := strArg(args, "op_ref")
	if opRef != "" {
		item := strArg(args, "item")
		if item == "" {
			return nil, newErr("item is required when using op_ref", http.StatusBadRequest)
		}
		if !tctx.BypassHITL {
			return nil, &approvalRequiredError{
				ToolName:  "http_request_with_credential",
				Summary:   fmt.Sprintf("Authenticated HTTP %s %s (1Password ref %s)", cmp.Or(strArg(args, "method"), "GET"), strArg(args, "url"), opRef),
				RiskLevel: "credential_http",
				ArgsJSON:  cmp.Or(rawArgs, "{}"),
			}
		}
		meta, err := GetOPItemMeta(ctx, e.opTokenPath, item, strArg(args, "vault"))
		if err != nil {
			return nil, err
		}
		var allowed []string
		for _, rawURL := range meta.URLs {
			if u, uErr := url.Parse(rawURL); uErr == nil && u.Hostname() != "" {
				allowed = append(allowed, u.Hostname())
			}
		}
		if len(allowed) == 0 {
			return nil, newErr(fmt.Sprintf("1Password item %q has no URLs configured for host allowlist", item), http.StatusForbidden)
		}
		return ExecuteCredentialHTTP(ctx, tctx.Config, e.opReader, nil, CredentialHTTPRequest{
			OPRef:        opRef,
			AllowedHosts: allowed,
			Method:       strArg(args, "method"),
			URL:          strArg(args, "url"),
			Body:         strArg(args, "body"),
		})
	}
	cp, ok := findCredentialPolicy(tctx.Config, alias)
	if !ok {
		return nil, newErr(fmt.Sprintf("unknown credential alias %q", alias), http.StatusNotFound)
	}
	if cp.RequireApproval && !tctx.BypassHITL {
		return nil, &approvalRequiredError{
			ToolName:  "http_request_with_credential",
			Summary:   fmt.Sprintf("Authenticated HTTP %s %s (credential %s)", cmp.Or(strArg(args, "method"), "GET"), strArg(args, "url"), alias),
			RiskLevel: "credential_http",
			ArgsJSON:  cmp.Or(rawArgs, "{}"),
		}
	}
	return ExecuteCredentialHTTP(ctx, tctx.Config, e.opReader, nil, CredentialHTTPRequest{
		Alias:  alias,
		Method: strArg(args, "method"),
		URL:    strArg(args, "url"),
		Body:   strArg(args, "body"),
	})
}
