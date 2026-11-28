package assistant

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"
)

func isAutoApproveDirective(key string) bool {
	return key == "auto-approve" || key == "autoapprove" || key == "yolo"
}

func parseTurnDirectives(text string, aliases map[string]string) (model string, autoApprove bool, cleaned string) {
	cur := strings.TrimSpace(text)
	matched := false
	for range 2 {
		if !strings.HasPrefix(cur, "/") {
			break
		}
		first, rest, hasRest := strings.Cut(cur, " ")
		rest = strings.TrimSpace(rest)
		if !hasRest || rest == "" {
			break
		}
		key := strings.ToLower(strings.TrimPrefix(first, "/"))
		if isAutoApproveDirective(key) && !autoApprove {
			autoApprove = true
			cur = rest
			matched = true
			continue
		}
		if resolved, ok := aliases[key]; ok && model == "" {
			model = resolved
			cur = rest
			matched = true
			continue
		}
		break
	}
	if !matched {
		return "", false, text
	}
	return model, autoApprove, cur
}

func (e *Engine) resolveTurnRoute(ctx context.Context, cfg Config, ch Channel, source, userText, explicit string, tail []Message, skills []Skill, opts turnOptions) turnRoute {
	slashModel, slashAutoApprove, cleanText := parseTurnDirectives(userText, cfg.Models.Aliases)
	bypassHITL := cfg.AutoApprove || opts.AutoApprove || slashAutoApprove || (opts.TriggerSource == "discord" && cfg.Discord.AutoApprove)
	chosen := cmp.Or(slashModel, strings.TrimSpace(explicit))
	if chosen == "" {
		for _, sk := range skills {
			if sk.Model != "" && strings.Contains(strings.ToLower(cleanText), strings.ToLower(sk.Name)) {
				chosen = sk.Model
				break
			}
		}
	}
	if chosen == "" {
		chosen = strings.TrimSpace(ch.Model)
	}
	if chosen == "" {
		switch {
		case source == "heartbeat":
			chosen = cfg.Models.Heartbeat
		case source == "chat" && cfg.Routing.Mode != RoutingModeOff:
			chosen = "auto"
		default:
			chosen = concreteDefaultChat(cfg)
		}
	}
	if alias, ok := cfg.Models.Aliases[strings.ToLower(chosen)]; ok {
		chosen = alias
	}
	if !strings.EqualFold(chosen, "auto") {
		return turnRoute{Model: chosen, CleanText: cleanText, BypassHITL: bypassHITL}
	}
	if source != "chat" {
		return turnRoute{Model: e.decideLegacyTier(ctx, cfg, cleanText), CleanText: cleanText, BypassHITL: bypassHITL}
	}
	return e.routeInteractiveChat(ctx, cfg, ch, cleanText, userText, tail, bypassHITL)
}

func (e *Engine) decideLegacyTier(ctx context.Context, cfg Config, cleanText string) string {
	fallback := concreteDefaultChat(cfg)
	ans, err := e.llm.Decide(ctx, cfg, cleanText, []DecisionQuestion{{
		ID:           "tier",
		Type:         "choice",
		Instructions: "Choose model capability tier needed for this user request.",
		Criteria: map[string]string{
			"fast":     "Simple question, greeting, reminder, or single-step status lookup",
			"balanced": "Normal assistant work, tool orchestration, summarization, or multi-step chat",
			"frontier": "Complex planning, deep reasoning, architecture design, or subtle analysis",
		},
	}})
	if err != nil {
		return fallback
	}
	switch ans["tier"].Value {
	case "fast":
		return cfg.Models.Fast
	case "frontier":
		return cfg.Models.Frontier
	default:
		return fallback
	}
}

func buildRoutingState(chID, cleanText, rawUserText string, tail []Message, activeGrant *FrontierGrant) map[string]any {
	state := map[string]any{
		"input":        cleanText,
		"user_message": cleanText,
		"channel":      chID,
	}
	hist := tail
	if len(hist) > 0 && hist[len(hist)-1].Role == "user" {
		lastContent := strings.TrimSpace(hist[len(hist)-1].Content)
		if lastContent == strings.TrimSpace(rawUserText) || lastContent == strings.TrimSpace(cleanText) {
			hist = hist[:len(hist)-1]
		}
	}
	if len(hist) > 4 {
		hist = hist[len(hist)-4:]
	}
	if len(hist) > 0 {
		recent := make([]string, 0, len(hist))
		for _, m := range hist {
			if m.Role != "user" && m.Role != "assistant" {
				continue
			}
			recent = append(recent, m.Role+": "+truncateText(m.Content, 300))
		}
		if len(recent) > 0 {
			state["recent_context"] = recent
		}
	}
	if activeGrant != nil && strings.TrimSpace(activeGrant.TopicSummary) != "" {
		state["active_frontier_topic"] = activeGrant.TopicSummary
	}
	return state
}

func (e *Engine) routeInteractiveChat(ctx context.Context, cfg Config, ch Channel, cleanText, rawUserText string, tail []Message, bypassHITL bool) turnRoute {
	fallback := concreteDefaultChat(cfg)
	routing := normalizeRoutingConfig(cfg.Routing)
	effMode := routing.Mode
	if effMode == RoutingModeOff {
		effMode = RoutingModeAuto
	}
	now := time.Now().UTC()
	activeGrant, _ := e.store.GetActiveFrontierGrant(ch.ID, ch.ContextEpoch, now)
	questions := []DecisionQuestion{{
		ID:   "needs_frontier",
		Type: "noul",
		Instructions: "Does this turn require a frontier reasoning model (complex architecture, subtle debugging, multi-step planning, deep trade-off analysis) " +
			"rather than a fast balanced assistant model (routine questions, status checks, simple edits, summarization)?",
		Criteria: map[string]string{
			"true":  "Requires deep reasoning, complex planning, architecture design, or subtle analysis",
			"false": "Routine chat, lookups, greetings, simple tool calls, or straightforward summarization",
		},
	}}
	if activeGrant != nil {
		questions = append(questions, DecisionQuestion{
			ID:           "same_topic",
			Type:         "noul",
			Instructions: "Is the latest user message continuing the same task/topic as active_frontier_topic and recent_context, rather than pivoting to a new unrelated topic?",
			Criteria: map[string]string{
				"true":  "Continuation or follow-up on the active frontier topic",
				"false": "New or unrelated topic pivot",
			},
		})
	}
	state := buildRoutingState(ch.ID, cleanText, rawUserText, tail, activeGrant)
	ans, err := e.llm.DecideState(ctx, cfg, state, questions)
	if err != nil {
		return turnRoute{Model: fallback, CleanText: cleanText, BypassHITL: bypassHITL}
	}
	pFrontier := ans["needs_frontier"].Prob("true")
	if _, hasNF := ans["needs_frontier"]; !hasNF {
		if tAns, hasTier := ans["tier"]; hasTier {
			pFrontier = tAns.Prob("frontier")
		}
	}
	if activeGrant != nil {
		if ans["same_topic"].Prob("true") < DefaultSameTopicProb {
			_ = e.store.DeleteFrontierGrant(ch.ID)
			e.publishEvent(StreamEvent{Type: "grant_updated", ChannelID: ch.ID})
			activeGrant = nil
		}
	}
	if pFrontier >= routing.GateProb {
		if bypassHITL || effMode == RoutingModeAuto {
			return turnRoute{Model: cfg.Models.Frontier, CleanText: cleanText, BypassHITL: bypassHITL}
		}
		todayStart := now.Format("2006-01-02") + "T00:00:00+00:00"
		dailySpend := e.store.DailySpendUSD(todayStart)
		if activeGrant != nil && (routing.DailySoftCapUSD <= 0 || dailySpend < routing.DailySoftCapUSD) {
			return turnRoute{Model: cfg.Models.Frontier, CleanText: cleanText, BypassHITL: bypassHITL, UsedGrant: activeGrant}
		}
		return turnRoute{
			CleanText:  cleanText,
			BypassHITL: bypassHITL,
			Escalate: &frontierEscalationArgs{
				OriginalPrompt: cleanText,
				FrontierModel:  cfg.Models.Frontier,
				FallbackModel:  fallback,
				Probability:    pFrontier,
				TopicHint:      truncateText(cleanText, 160),
				DailySpendUSD:  dailySpend,
				GrantMaxTurns:  routing.GrantMaxTurns,
				GrantMaxCost:   routing.GrantMaxCostUSD,
			},
		}
	}
	if effMode == RoutingModeHybrid && pFrontier >= routing.BorderlineProb {
		return turnRoute{Model: fallback, CleanText: cleanText, BypassHITL: bypassHITL, BorderlineProb: pFrontier}
	}
	return turnRoute{Model: fallback, CleanText: cleanText, BypassHITL: bypassHITL}
}

func (e *Engine) buildPromptMessages(ctx context.Context, ch Channel, tail []Message, slashAliases map[string]string) []ChatMessage {
	v, _ := e.vault()
	var soul, reflections, observations, fleetCtx string
	var skills []Skill
	if v != nil {
		soul, _ = v.ReadFile("_agent/SOUL.md")
		reflections, _ = v.ReadFile("_agent/memory/REFLECTIONS.md")
		observations, _ = v.ReadFile("_agent/memory/OBSERVATIONS.md")
		skills, _ = v.ListSkills()
	}
	if e.deps != nil {
		fleetCtx, _ = e.deps.MemoryContext("", "lighthouse")
	}
	var sysStatic strings.Builder
	sysStatic.WriteString(soul)
	sysStatic.WriteString("\nCurrent UTC time: " + time.Now().UTC().Format(time.RFC3339) + "\n")
	sysStatic.WriteString("Active channel: #" + ch.ID + "\n")
	sysStatic.WriteString("\n## Skills Catalog\n")
	if len(skills) > 0 {
		for _, sk := range skills {
			extra := ""
			if len(sk.Files) > 0 {
				extra = fmt.Sprintf(" (+%d files)", len(sk.Files))
			}
			fmt.Fprintf(&sysStatic, "- %s%s: %s\n", sk.Name, extra, cmp.Or(sk.Description, "(no description)"))
		}
	} else {
		sysStatic.WriteString("- (no installed skills yet)\n")
	}
	sysStatic.WriteString("Use `skill_read` (with optional `file`) to load `SKILL.md` and companion files; use `skill_search` + `skill_fetch` to discover/preview `skills.sh` or GitHub (`owner/repo@skill`) playbooks, `skill_install` (approval-gated, direct chat only) to install bundles, `skill_write` to author or adapt skills to Fleet tools, and `skill_remove` to uninstall. Bundled skill scripts are non-executable on the host and must only be inspected or run inside `sandbox_exec` if needed.\n")
	if fleetCtx != "" {
		sysStatic.WriteString("\n## Fleet Standing Memory\n" + fleetCtx + "\n")
	}
	if vaultsSection := e.buildVaultsPromptSection(); vaultsSection != "" {
		sysStatic.WriteString(vaultsSection)
	}
	if opSection := e.buildOPPromptSection(ctx); opSection != "" {
		sysStatic.WriteString(opSection)
	}
	if sbSection := buildSandboxPromptSection(e.Config()); sbSection != "" {
		sysStatic.WriteString(sbSection)
	}
	if lhSection := e.buildLighthouseWorkerPromptSection(); lhSection != "" {
		sysStatic.WriteString(lhSection)
	}
	sysStatic.WriteString(buildGenerativeUIPromptSection())

	var memPrefix strings.Builder
	if reflections != "" {
		memPrefix.WriteString("## Reflections\n" + reflections + "\n\n")
	}
	if observations != "" {
		memPrefix.WriteString("## Observations\n" + observations + "\n")
	}

	parts := []PromptPart{{Type: "text", Text: sysStatic.String()}}
	if memPrefix.Len() > 0 {
		parts = append(parts, PromptPart{
			Type:         "text",
			Text:         memPrefix.String(),
			CacheControl: map[string]any{"type": "ephemeral"},
		})
	} else {
		parts[0].CacheControl = map[string]any{"type": "ephemeral"}
	}
	out := []ChatMessage{{Role: "system", Content: parts}}
	for _, m := range tail {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		txt := m.Content
		if m.Role == "user" {
			_, _, txt = parseTurnDirectives(txt, slashAliases)
			if hint := formatAttachedFilesHint(m.Widgets); hint != "" {
				txt += "\n" + hint
			}
		}
		out = append(out, ChatMessage{Role: m.Role, Content: txt})
	}
	return out
}

func formatAttachedFilesHint(widgets []WidgetPayload) string {
	var parts []string
	for _, w := range widgets {
		if w.Kind != "files" {
			continue
		}
		dataMap, ok := w.Data.(map[string]any)
		if !ok {
			continue
		}
		rawFiles, ok := dataMap["files"].([]any)
		if !ok {
			continue
		}
		for _, item := range rawFiles {
			fm, ok := item.(map[string]any)
			if !ok {
				continue
			}
			name, _ := fm["name"].(string)
			mimeType, _ := fm["mime_type"].(string)
			var sizeBytes int64
			switch v := fm["size_bytes"].(type) {
			case float64:
				sizeBytes = int64(v)
			case int64:
				sizeBytes = v
			}
			if name != "" {
				parts = append(parts, fmt.Sprintf("%s (%s, %s)", name, formatByteSize(sizeBytes), cmp.Or(mimeType, "application/octet-stream")))
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "[Attached files staged in sandbox /work/in/: " + strings.Join(parts, ", ") + "]"
}

func (e *Engine) buildVaultsPromptSection() string {
	cfg := e.Config()
	var b strings.Builder
	b.WriteString("\n## Mounted Obsidian Vaults\n")
	fmt.Fprintf(&b, "- %s (primary): _agent/, _skills/, Observational Memory; auto-write: [%s]; approval required elsewhere\n", PrimaryVaultName, strings.Join(cfg.AutoWriteDirs, ", "))
	for _, m := range cfg.ExtraVaults {
		var policy string
		switch {
		case m.ReadOnly:
			policy = "read-only"
		case len(m.AutoWriteDirs) > 0:
			policy = fmt.Sprintf("auto-write: [%s]; approval required elsewhere", strings.Join(m.AutoWriteDirs, ", "))
		default:
			policy = "read anywhere; all writes require human approval"
		}
		fmt.Fprintf(&b, "- %s (%s): %s\n", m.Name, m.Path, policy)
	}
	b.WriteString("Use `vault_search` (with optional `vault`: \"all\", \"agent\", or a mounted vault name) and pass optional `vault` to `vault_read`, `vault_write`, and `vault_append`.\n")
	return b.String()
}

func (e *Engine) buildOPPromptSection(ctx context.Context) string {
	hasTok := ResolveOPToken(e.opTokenPath) != ""
	if !hasTok && e.Probes.ListVaults == nil {
		return ""
	}
	vaults, err := e.listOPVaultsCached(ctx)
	if (err != nil || len(vaults) == 0) && !hasTok {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## 1Password Agent Vault (Zero-Exposure)\n")
	if len(vaults) > 0 {
		names := make([]string, 0, len(vaults))
		for _, v := range vaults {
			names = append(names, v.Name)
		}
		b.WriteString("Accessible vaults: " + strings.Join(names, ", ") + "\n")
	}
	b.WriteString("Use `onepassword_list_items`, `onepassword_get_item_meta`, `http_request_with_credential`, and `onepassword_store_item` to inspect item metadata/op:// references and inject credentials at runtime without ever requesting or displaying plaintext secrets.\n")
	b.WriteString("For external or internal MCP setup, use `mcp_list`, `mcp_configure` (approval-gated; pass secrets via `op://` in `env_refs`, `header_refs`, or `oauth.client_secret_op_ref`, never plaintext; for official Google Workspace MCPs set `oauth: {\"enabled\":true,\"provider\":\"google\",\"client_id\":\"...\",\"client_secret_op_ref\":\"op://...\",\"scopes\":[...]}`), `mcp_oauth_connect` (attaches an interactive sign-in card in `#chat` whenever OAuth sign-in or re-authorization is needed), and `mcp_test`.\n")
	return b.String()
}

func (e *Engine) buildLighthouseWorkerPromptSection() string {
	if e.deps == nil {
		return ""
	}
	lh := e.deps.LighthouseWorkerStatus()
	if lh.Device == "" {
		return ""
	}
	var b strings.Builder
	if lh.WorkerOnline {
		fmt.Fprintf(&b, "\n## Lighthouse Host Worker (%s, online)\n", lh.Device)
		fmt.Fprintf(&b, "- `%s` has a local Fleet worker online for lighthouse host self-configuration (e.g. installing `uv`, `node`, `brew` packages, or troubleshooting stdio MCP servers) via `fleet_spawn_agent(device=%q, harness=\"agy\", repo_path=\"~\", ...)` (`harness` defaults to `agy` on `%s` unless the user specifies another harness) — call `skill_read(\"lighthouse-self-config\")` before spawning for the playbook. Always requires explicit user approval.\n", lh.Device, lh.Device, lh.Device)
		b.WriteString("- Keep using `sandbox_*` for document/data/file tasks and other awake Macs (`laptop`/`office`) for normal software repo coding.\n")
	} else {
		fmt.Fprintf(&b, "\n## Lighthouse Host Worker (%s, offline)\n", lh.Device)
		fmt.Fprintf(&b, "- `%s`'s host worker is currently offline; if host self-configuration is needed, tell the user they can enable it once with `fleet assistant worker-install` on `%s` (using Rex or tmux).\n", lh.Device, lh.Device)
	}
	return b.String()
}

func buildGenerativeUIPromptSection() string {
	return strings.ReplaceAll(`
## Generative UI (~render_card~ & ~render_widget~)
- Prefer ~render_card~ for structured mobile-friendly UI and quick follow-ups; use ~render_widget~ for interactive or Generative UI charts, graphs, diagrams, and custom visualizations (call ~render_widget~ directly — do NOT paste raw HTML/SVG into Markdown text, and do NOT use ~sandbox_exec~ unless the user asked for a downloadable PNG/PDF file).
- ~render_card~ ~data~ schemas by ~kind~:
  - ~choices~: ~{"choices":[{"label":"Button label","value":"Prompt sent on tap"}]}~
  - ~key_value~: ~{"Field":"Value"}~
  - ~agents~: ~{"agents":[{"name":"...","device":"...","state":"..."}]}~
  - ~agenda~: ~{"items":[{"time":"09:00","title":"..."}]}~
  - ~markdown~: ~{"markdown":"..."}~
  - ~mcp_oauth~: ~{"server":"..."}~ (prefer calling ~mcp_oauth_connect~ so server metadata and scopes are populated automatically)
- ~render_widget~ rules (sandboxed iframe, strict CSP ~default-src 'none'~):
  - Self-contained HTML with inline ~<style>~, ~<svg viewBox="...">~ or ~<canvas>~, and inline ~<script>~ only (never wrap ~html~ in markdown code fences); external URLs/CDNs/Chart.js/D3/Plotly/fonts are blocked and rejected (only ~data:~ images work).
  - Style with host CSS variables (~--text~, ~--muted~, ~--border~, ~--accent~, ~--surface~) so light and dark themes adapt cleanly.
  - Add ~data-fleet-action="<prompt text>"~ to clickable elements to send follow-up prompts into ~#chat~; keep layouts mobile-friendly and compact (<= 480px tall).
`, "~", "`")
}
