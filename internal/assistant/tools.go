package assistant

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// SpawnAgentRequest describes an assistant-initiated remote coding session launch.
type SpawnAgentRequest struct {
	Harness  string `json:"harness"`
	Name     string `json:"name,omitempty"`
	Device   string `json:"device,omitempty"`
	RepoPath string `json:"repo_path,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
	Model    string `json:"model,omitempty"`
	Policy   string `json:"policy,omitempty"`
	UseTmux  bool   `json:"use_tmux,omitempty"`
}

// SpawnAgentResult reports the queued Slacker job and target Mac worker availability.
type SpawnAgentResult struct {
	JobID        string `json:"job_id"`
	TargetDevice string `json:"target_device"`
	Harness      string `json:"harness"`
	SessionName  string `json:"session_name"`
	RepoPath     string `json:"repo_path"`
	Status       string `json:"status"`
	WorkerOnline bool   `json:"worker_online"`
}

// FleetDeps abstracts Fleet subsystem access so assistant does not import core/slacker/agents directly.
type FleetDeps interface {
	ListAgents(device, repo, harness, state string, includeStale bool) (any, error)
	InspectAgent(target string) (any, error)
	SendAgentMessage(fromDevice, to, body string) (any, error)
	ListPaths(device, query string, limit int) (any, error)
	SpawnAgent(ctx context.Context, req SpawnAgentRequest) (SpawnAgentResult, error)
	KillAgent(ctx context.Context, sessionID string) (any, error)
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
		makeToolSpec("fleet_list_agents", "List active Fleet AI agent sessions across Macs. state=\"idle\" means the agent finished its last turn and is waiting for input; state_changed_at/state_age_seconds show when it entered that state while updated_at/age_seconds reflect periodic liveness pings.", schemaObj(map[string]any{
			"device": strProp("Optional device filter"), "repo": strProp("Optional repo filter"),
			"harness": strProp("Optional harness filter"), "state": strProp("Optional state filter (working, waiting, idle)"),
			"include_stale": boolProp("Include recently exited/stale sessions"),
		})),
		makeToolSpec("fleet_inspect_agent", "Inspect one Fleet agent session's status, pending prompt, and tail. state=\"idle\" means the last turn finished; use state_changed_at/state_age_seconds to tell when it finished rather than updated_at/age_seconds (which only reflect periodic 60s liveness pings).", schemaObj(map[string]any{
			"agent_id": strProp("Session name, device/name, title, or session_id"),
		}, "agent_id")),
		makeToolSpec("fleet_send_message", "Send a message to a peer Fleet agent session or device.", schemaObj(map[string]any{
			"to": strProp("Target session name, device/name, repo:name, or *"), "message": strProp("Message text"),
		}, "to", "message")),
		makeToolSpec("fleet_list_paths", "List known project directories and repositories tracked across Fleet Macs.", schemaObj(map[string]any{
			"device": strProp("Optional device filter"), "query": strProp("Optional path/repo substring"), "limit": intProp("Max results"),
		})),
		makeToolSpec("fleet_spawn_agent", "Spawn a Claude, Codex, or Antigravity coding session on an awake Mac and watch for blockers/completion (approval-gated).", schemaObj(map[string]any{
			"harness": strProp("claude | codex | agy"), "device": strProp("Optional target Mac (e.g. laptop, office)"),
			"repo_path": strProp("Repo path, ~/path, or workspace subpath"), "prompt": strProp("Task prompt for the spawned coding agent"),
			"name": strProp("Optional friendly session name"), "model": strProp("Optional model"),
			"policy": strProp("Optional policy (auto|bypass|safe)"), "use_tmux": boolProp("Run inside tmux"),
		}, "harness", "prompt")),
		makeToolSpec("fleet_kill_agent", "Terminate a Fleet coding agent session (approval-gated).", schemaObj(map[string]any{
			"session_id": strProp("Session ID or target session"),
		}, "session_id")),
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
		makeToolSpec("vault_search", "Search Markdown notes across the primary agent vault and any mounted Obsidian vaults.", schemaObj(map[string]any{
			"query": strProp("Search query terms"), "limit": intProp("Max results"),
			"vault": strProp("Optional vault name (\"all\" searches agent + mounted vaults, or specify \"agent\" / mounted vault name)"),
		}, "query")),
		makeToolSpec("vault_read", "Read one Markdown file from the Obsidian vault.", schemaObj(map[string]any{
			"path":  strProp("Relative path inside vault"),
			"vault": strProp("Optional vault name (default \"agent\")"),
		}, "path")),
		makeToolSpec("vault_append", "Append text to a Markdown file in the vault (requires approval outside auto-write folders).", schemaObj(map[string]any{
			"path": strProp("Relative path inside vault"), "content": strProp("Markdown text to append"),
			"vault": strProp("Optional vault name (default \"agent\")"),
		}, "path", "content")),
		makeToolSpec("vault_write", "Write a Markdown file in the vault (requires approval outside auto-write folders).", schemaObj(map[string]any{
			"path": strProp("Relative path inside vault"), "content": strProp("Full Markdown content"),
			"vault": strProp("Optional vault name (default \"agent\")"),
		}, "path", "content")),
		makeToolSpec("timer_schedule", "Create a one-shot, interval, or 5-field cron timer wakeup for the assistant.", schemaObj(map[string]any{
			"name": strProp("Short timer name"), "channel_id": strProp("Target channel (default briefing)"),
			"prompt": strProp("Prompt to execute when timer fires"), "schedule_kind": strProp("once, interval, or cron"),
			"fire_at": strProp("RFC3339 UTC timestamp for once"), "interval_seconds": intProp("Repeat interval in seconds"),
			"cron_expr": strProp("5-field cron expression (e.g. '0 8 * * 1-5' for 8am weekdays)"), "model": strProp("Optional model override"),
		}, "name", "prompt")),
		makeToolSpec("timer_list", "List scheduled assistant timers.", schemaObj(map[string]any{})),
		makeToolSpec("timer_cancel", "Delete a scheduled timer by id.", schemaObj(map[string]any{"id": strProp("Timer ID")}, "id")),
		makeToolSpec("outbox_notify", "Queue a proactive notification card/outbox item.", schemaObj(map[string]any{
			"title": strProp("Short title"), "body": strProp("Notification body"), "priority": strProp("low|normal|urgent"),
		}, "body")),
		makeToolSpec("render_card", "Attach a typed native UI card to this reply. Prefer this over render_widget for structured mobile cards.", schemaObj(map[string]any{
			"kind":  strProp("choices | key_value | agents | agenda | markdown"),
			"title": strProp("Card title"),
			"data": map[string]any{
				"type":        "object",
				"description": "Payload matching kind: choices={choices:[{label,value}]}, key_value={key:val}, agents={agents:[{name,device,state}]}, agenda={items:[{time,title}]}, markdown={markdown}",
			},
		}, "kind", "data")),
		makeToolSpec("render_widget", "Attach an interactive sandboxed HTML/CSS/SVG Generative UI widget to this reply (no external URLs; use CSS vars --text/--muted/--border/--accent/--surface and data-fleet-action=\"...\" for clickable prompts).", schemaObj(map[string]any{
			"title": strProp("Widget title"), "html": strProp("Self-contained HTML snippet (inline <style>/<script>/<svg> only; no external URLs)"),
		}, "title", "html")),
		makeToolSpec("tool_output_read", "Read a slice of a truncated tool output saved earlier in this channel by output_id.", schemaObj(map[string]any{
			"output_id": strProp("Saved output ID (e.g. out-a1b2c3d4e5f6) from a truncation notice"),
			"offset":    intProp("Starting byte offset (default 0)"),
			"max_bytes": intProp("Max bytes to return (default 16384, max 32768)"),
		}, "output_id")),
	}
	specs = append(specs, credentialAndMCPToolSpecs()...)
	if e.Config().Sandbox.Enabled {
		specs = append(specs, sandboxToolSpecs()...)
	}
	if e.mcp != nil {
		for _, ext := range e.mcp.ListTools(ctx) {
			specs = append(specs, makeToolSpec(ext.FullName, ext.Description, ext.InputSchema))
		}
	}
	return specs
}

func credentialAndMCPToolSpecs() []ToolSpec {
	return []ToolSpec{
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
		makeToolSpec("mcp_list", "List configured external/internal MCP servers and their discovered tools.", schemaObj(map[string]any{})),
		makeToolSpec("mcp_test", "Run an initialize + tools/list handshake test on a configured MCP server.", schemaObj(map[string]any{
			"name": strProp("Configured MCP server name"),
		}, "name")),
		makeToolSpec("mcp_configure", "Add or update an external/internal stdio or HTTP/SSE MCP server in assistant.json (approval-gated; use op:// in env_refs/header_refs for secrets).", schemaObj(map[string]any{
			"name":                   strProp("Server slug ([a-z0-9][a-z0-9_-]*)"),
			"transport":              strProp("stdio or http"),
			"command":                strProp("Executable command for stdio transport"),
			"args":                   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Command arguments"},
			"env":                    map[string]any{"type": "object", "description": "Non-secret environment variables"},
			"env_refs":               map[string]any{"type": "object", "description": "Secret env var -> op://Vault/Item/field mapping"},
			"url":                    strProp("Endpoint URL for http/sse transport"),
			"headers":                map[string]any{"type": "object", "description": "Non-secret HTTP headers"},
			"header_refs":            map[string]any{"type": "object", "description": "Secret HTTP header -> op://Vault/Item/field mapping"},
			"enabled":                boolProp("Enable server (default true)"),
			"approval_policy":        strProp("Optional approval policy: all (gate every tool), writes_only (auto-allow tools declaring MCP annotations.readOnlyHint=true; gate writes/unannotated tools), or none (auto-allow unless listed in require_approval_tools)"),
			"require_approval_tools": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Tool names requiring HITL approval (defaults to [\"*\"] for new servers unless approval_policy is set)"},
		}, "name")),
		makeToolSpec("mcp_remove", "Remove a configured MCP server from assistant.json (approval-gated).", schemaObj(map[string]any{
			"name": strProp("MCP server name to remove"),
		}, "name")),
	}
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
	if name == "tool_output_read" {
		return e.execToolOutputRead(tctx.ChannelID, args)
	}
	if name == "credential_list_aliases" || name == "http_request_with_credential" || strings.HasPrefix(name, "onepassword_") {
		return e.dispatchCredentialTool(ctx, name, args, rawArgs, tctx)
	}
	if name == "mcp_list" || name == "mcp_test" || name == "mcp_configure" || name == "mcp_remove" {
		return e.dispatchMCPManageTool(ctx, name, args, rawArgs, tctx)
	}
	if strings.HasPrefix(name, "sandbox_") {
		return e.dispatchSandboxTool(ctx, name, args, rawArgs, tctx)
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
	case "fleet_list_paths":
		return e.deps.ListPaths(strArg(args, "device"), strArg(args, "query"), intArg(args, "limit", 30))
	case "fleet_spawn_agent":
		return e.execFleetSpawnTool(ctx, args, rawArgs, tctx)
	case "fleet_kill_agent":
		sid := strArg(args, "session_id")
		if !tctx.BypassHITL {
			return nil, &approvalRequiredError{
				ToolName:  name,
				Summary:   fmt.Sprintf("Terminate Fleet agent session %s", sid),
				RiskLevel: "agent_kill",
				ArgsJSON:  cmp.Or(rawArgs, "{}"),
			}
		}
		return e.deps.KillAgent(ctx, sid)
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
	switch name {
	case "skill_list":
		v, err := e.vault()
		if err != nil {
			return nil, err
		}
		return v.ListSkills()
	case "skill_read":
		v, err := e.vault()
		if err != nil {
			return nil, err
		}
		return v.GetSkill(strArg(args, "name"))
	case "skill_write":
		if tctx.Source != "chat" && tctx.Source != "approval" && !tctx.BypassHITL {
			return nil, newErr("skill_write is only allowed during direct user chat", http.StatusForbidden)
		}
		v, err := e.vault()
		if err != nil {
			return nil, err
		}
		sk, err := v.SaveSkill(strArg(args, "name"), strArg(args, "content"))
		if err != nil {
			return nil, err
		}
		v.MaybeGitCommit(tctx.Config.GitAutoCommit, "assistant: update skill "+sk.Name)
		return sk, nil
	case "vault_search":
		return e.SearchVaults(strArg(args, "vault"), strArg(args, "query"), intArg(args, "limit", 15))
	case "vault_read":
		v, mount, err := e.ResolveVault(strArg(args, "vault"))
		if err != nil {
			return nil, err
		}
		rel := strArg(args, "path")
		content, err := v.ReadFile(rel)
		if err != nil {
			return nil, err
		}
		return map[string]string{"vault": mount.Name, "path": rel, "content": content}, nil
	case "vault_append", "vault_write":
		return e.execVaultWriteTool(name, args, rawArgs, tctx)
	default:
		return nil, newErr("unknown vault tool "+name, http.StatusBadRequest)
	}
}

func (e *Engine) execVaultWriteTool(name string, args map[string]any, rawArgs string, tctx *toolExecContext) (any, error) {
	v, mount, err := e.ResolveVault(strArg(args, "vault"))
	if err != nil {
		return nil, err
	}
	if mount.ReadOnly {
		return nil, newErr(fmt.Sprintf("vault %q is read-only", mount.Name), http.StatusForbidden)
	}
	rel := strArg(args, "path")
	if !tctx.BypassHITL && !IsAutoWritable(rel, mount.AutoWriteDirs) {
		return nil, &approvalRequiredError{
			ToolName:  name,
			Summary:   fmt.Sprintf("Modify protected note %s:%s", mount.Name, rel),
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
	return map[string]string{"vault": mount.Name, "path": norm, "status": "saved"}, nil
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
			CronExpr:        strArg(args, "cron_expr"),
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
		if (tctx.Config.Discord.Enabled || tctx.Config.Notifications.Enabled) && priorityMeetsMin(item.Priority, tctx.Config.Notifications.MinPriority) {
			go e.dispatchOutboxNotification(item)
		}
		return item, nil
	default:
		return nil, newErr("unknown automation tool "+name, http.StatusBadRequest)
	}
}

var (
	reWidgetExternalScript = regexp.MustCompile(`(?is)<script[^>]*\bsrc\s*=\s*["']?\s*(?:https?:)?//`)
	reWidgetExternalLink   = regexp.MustCompile(`(?is)<link[^>]*\bhref\s*=\s*["']?\s*(?:https?:)?//`)
	reWidgetExternalImport = regexp.MustCompile(`(?is)@import\s+(?:url\(\s*)?["']?\s*(?:https?:)?//`)
)

func stripHTMLCodeFence(raw string) string {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	_, body, ok := strings.Cut(s, "\n")
	if !ok {
		return s
	}
	if idx := strings.LastIndex(body, "```"); idx >= 0 && strings.TrimSpace(body[idx+3:]) == "" {
		return strings.TrimSpace(body[:idx])
	}
	return s
}

func validateWidgetHTML(html string) error {
	if reWidgetExternalScript.MatchString(html) || reWidgetExternalLink.MatchString(html) || reWidgetExternalImport.MatchString(html) {
		return newErr("render_widget runs offline under strict CSP (default-src 'none') so external scripts/CDNs/stylesheets cannot load; rewrite the widget using self-contained inline <svg> or <canvas> and inline <script>", http.StatusBadRequest)
	}
	return nil
}

func dispatchUITool(name string, args map[string]any, tctx *toolExecContext) (any, error) {
	var wp WidgetPayload
	if name == "render_card" {
		kind := strings.ToLower(cmp.Or(strArg(args, "kind"), "markdown"))
		switch kind {
		case "agents", "agenda", "choices", "key_value", "markdown":
		default:
			return nil, newErr(fmt.Sprintf("unsupported render_card kind %q", kind), http.StatusBadRequest)
		}
		data := args["data"]
		if kind == "markdown" {
			if s, ok := data.(string); ok {
				data = map[string]any{"markdown": s}
			}
		}
		wp = WidgetPayload{
			Kind:  kind,
			Title: strArg(args, "title"),
			Data:  data,
		}
	} else {
		html := stripHTMLCodeFence(strArg(args, "html"))
		if html == "" {
			return nil, newErr("widget html is required", http.StatusBadRequest)
		}
		if err := validateWidgetHTML(html); err != nil {
			return nil, err
		}
		wp = WidgetPayload{
			Kind:  "html",
			Title: strArg(args, "title"),
			HTML:  html,
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

func (e *Engine) execFleetSpawnTool(ctx context.Context, args map[string]any, rawArgs string, tctx *toolExecContext) (any, error) {
	req := SpawnAgentRequest{
		Harness:  strArg(args, "harness"),
		Name:     strArg(args, "name"),
		Device:   strArg(args, "device"),
		RepoPath: strArg(args, "repo_path"),
		Prompt:   strArg(args, "prompt"),
		Model:    strArg(args, "model"),
		Policy:   strArg(args, "policy"),
		UseTmux:  boolArg(args, "use_tmux"),
	}
	if !tctx.BypassHITL {
		target := cmp.Or(req.Device, "auto")
		return nil, &approvalRequiredError{
			ToolName:  "fleet_spawn_agent",
			Summary:   fmt.Sprintf("Spawn %s session on %s (%s)", cmp.Or(req.Harness, "agent"), target, cmp.Or(req.RepoPath, "workspace")),
			RiskLevel: "agent_spawn",
			ArgsJSON:  cmp.Or(rawArgs, "{}"),
		}
	}
	res, err := e.deps.SpawnAgent(ctx, req)
	if err != nil {
		return nil, err
	}
	del, dErr := e.store.CreateDelegation(Delegation{
		JobID:        res.JobID,
		ChannelID:    tctx.ChannelID,
		TargetDevice: res.TargetDevice,
		Harness:      res.Harness,
		SessionName:  res.SessionName,
		RepoPath:     res.RepoPath,
		Prompt:       req.Prompt,
		Status:       "queued",
	})
	if dErr == nil {
		e.refreshDelegationsFlag()
		e.publishEvent(StreamEvent{Type: "delegation", ChannelID: tctx.ChannelID, Data: del})
	}
	return map[string]any{"spawn": res, "delegation": del}, nil
}

func strSliceArg(m map[string]any, key string) []string {
	raw, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, it := range raw {
		if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

func strMapArg(m map[string]any, key string) map[string]string {
	raw, ok := m[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if strings.TrimSpace(k) != "" {
			out[strings.TrimSpace(k)] = fmt.Sprint(v)
		}
	}
	return out
}

func parseMCPServerFromArgs(args map[string]any, existing *MCPServerConfig) MCPServerConfig {
	srv := MCPServerConfig{
		Name:                 strArg(args, "name"),
		Transport:            strArg(args, "transport"),
		Command:              strArg(args, "command"),
		Args:                 strSliceArg(args, "args"),
		Env:                  strMapArg(args, "env"),
		EnvRefs:              strMapArg(args, "env_refs"),
		URL:                  strArg(args, "url"),
		Headers:              strMapArg(args, "headers"),
		HeaderRefs:           strMapArg(args, "header_refs"),
		Enabled:              true,
		ApprovalPolicy:       strArg(args, "approval_policy"),
		RequireApprovalTools: strSliceArg(args, "require_approval_tools"),
	}
	if en, ok := args["enabled"].(bool); ok {
		srv.Enabled = en
	}
	if _, hasPolicy := args["approval_policy"]; !hasPolicy && existing != nil {
		srv.ApprovalPolicy = existing.ApprovalPolicy
	}
	if _, hasTools := args["require_approval_tools"]; !hasTools {
		if existing != nil {
			srv.RequireApprovalTools = append([]string(nil), existing.RequireApprovalTools...)
		} else if srv.ApprovalPolicy == "" {
			srv.RequireApprovalTools = []string{"*"}
		}
	}
	return NormalizeMCPServerConfig(srv)
}

func (e *Engine) dispatchMCPManageTool(ctx context.Context, name string, args map[string]any, rawArgs string, tctx *toolExecContext) (any, error) {
	cfg := e.Config()
	switch name {
	case "mcp_list":
		view := cfg.View("")
		var statuses []MCPServerStatus
		if e.mcp != nil {
			statuses = e.mcp.ListServerStatuses(ctx, cfg.MCPServers)
		}
		return map[string]any{"servers": view.MCPServers, "statuses": statuses}, nil
	case "mcp_test":
		srvName := strings.ToLower(strArg(args, "name"))
		for _, srv := range cfg.MCPServers {
			if srv.Name == srvName {
				if e.mcp == nil {
					return nil, newErr("MCP manager unavailable", http.StatusServiceUnavailable)
				}
				tools, err := e.mcp.TestServer(ctx, srv)
				if err != nil {
					return nil, err
				}
				names := make([]string, 0, len(tools))
				for _, t := range tools {
					names = append(names, t.RemoteName)
				}
				return map[string]any{"name": srv.Name, "transport": srv.Transport, "tool_count": len(tools), "tools": names}, nil
			}
		}
		return nil, newErr(fmt.Sprintf("MCP server %q not found", srvName), http.StatusNotFound)
	case "mcp_configure":
		return e.execMCPConfigureTool(ctx, args, rawArgs, tctx)
	case "mcp_remove":
		return e.execMCPRemoveTool(args, rawArgs, tctx)
	default:
		return nil, newErr("unknown MCP tool "+name, http.StatusBadRequest)
	}
}

func (e *Engine) execMCPConfigureTool(ctx context.Context, args map[string]any, rawArgs string, tctx *toolExecContext) (any, error) {
	if tctx.Source != "chat" && tctx.Source != "approval" && !tctx.BypassHITL {
		return nil, newErr("mcp_configure is only allowed during direct user chat", http.StatusForbidden)
	}
	cur := e.Config()
	name := strings.ToLower(strArg(args, "name"))
	var existing *MCPServerConfig
	for i := range cur.MCPServers {
		if cur.MCPServers[i].Name == name {
			existing = &cur.MCPServers[i]
			break
		}
	}
	srv := parseMCPServerFromArgs(args, existing)
	if err := ValidateMCPServerConfig(srv, true); err != nil {
		return nil, err
	}
	if !tctx.BypassHITL {
		target := cmp.Or(srv.URL, strings.TrimSpace(srv.Command+" "+strings.Join(srv.Args, " ")))
		return nil, &approvalRequiredError{
			ToolName:   "mcp_configure",
			ServerName: srv.Name,
			Summary:    fmt.Sprintf("Configure %s MCP server %q (%s)", srv.Transport, srv.Name, target),
			RiskLevel:  "mcp_config",
			ArgsJSON:   cmp.Or(rawArgs, "{}"),
		}
	}
	nextServers := make([]MCPServerConfig, 0, len(cur.MCPServers)+1)
	replaced := false
	for _, s := range cur.MCPServers {
		if s.Name == srv.Name {
			nextServers = append(nextServers, srv)
			replaced = true
		} else {
			nextServers = append(nextServers, s)
		}
	}
	if !replaced {
		nextServers = append(nextServers, srv)
	}
	cur.MCPServers = nextServers
	if _, err := e.UpdateConfig(cur, false); err != nil {
		return nil, err
	}
	tctx.Config = e.Config()
	res := map[string]any{"status": "saved", "server": redactMCPServers([]MCPServerConfig{srv})[0]}
	if srv.Enabled && e.mcp != nil {
		tCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if tools, tErr := e.mcp.TestServer(tCtx, srv); tErr != nil {
			res["test_error"] = tErr.Error()
		} else {
			names := make([]string, 0, len(tools))
			for _, t := range tools {
				names = append(names, t.RemoteName)
			}
			res["discovered_tools"] = names
		}
	}
	return res, nil
}

func (e *Engine) execMCPRemoveTool(args map[string]any, rawArgs string, tctx *toolExecContext) (any, error) {
	if tctx.Source != "chat" && tctx.Source != "approval" && !tctx.BypassHITL {
		return nil, newErr("mcp_remove is only allowed during direct user chat", http.StatusForbidden)
	}
	name := strings.ToLower(strArg(args, "name"))
	if name == "" {
		return nil, newErr("MCP server name is required", http.StatusBadRequest)
	}
	if !tctx.BypassHITL {
		return nil, &approvalRequiredError{
			ToolName:   "mcp_remove",
			ServerName: name,
			Summary:    fmt.Sprintf("Remove MCP server %q", name),
			RiskLevel:  "mcp_config",
			ArgsJSON:   cmp.Or(rawArgs, "{}"),
		}
	}
	cur := e.Config()
	idx := slices.IndexFunc(cur.MCPServers, func(s MCPServerConfig) bool { return s.Name == name })
	if idx < 0 {
		return nil, newErr(fmt.Sprintf("MCP server %q not found", name), http.StatusNotFound)
	}
	cur.MCPServers = slices.Delete(cur.MCPServers, idx, idx+1)
	if _, err := e.UpdateConfig(cur, false); err != nil {
		return nil, err
	}
	tctx.Config = e.Config()
	return map[string]any{"status": "removed", "name": name}, nil
}
