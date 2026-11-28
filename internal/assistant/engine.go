package assistant

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maxToolRounds = 8

// StreamEvent is pushed over SSE to open Fleet Console #chat sessions.
type StreamEvent struct {
	Type      string `json:"type"`
	ChannelID string `json:"channel_id,omitempty"`
	Data      any    `json:"data,omitempty"`
}

// Engine coordinates assistant storage, Vault, OpenRouter, Decisions, MCP, timers, and SSE streaming.
type Engine struct {
	store       *Store
	configPath  string
	opTokenPath string
	vaultDir    string
	deps        FleetDeps
	llm         *openRouterClient
	mcp         *MCPManager
	opReader    OPReaderFunc
	Probes      DoctorProbes

	cfgMu      sync.RWMutex
	cfg        Config
	cachedKey  string
	cachedRef  string
	cachedAt   time.Time
	opVaults   []OPVaultMeta
	opVaultsAt time.Time

	subMu       sync.Mutex
	subscribers map[chan StreamEvent]struct{}

	notifyCh chan struct{}
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// Open initializes assistant.db, assistant.json, and the default Obsidian/assistant vault directory.
func Open(dbPath, configPath, defaultVaultDir string, deps FleetDeps) (*Engine, error) {
	store, err := OpenStore(dbPath)
	if err != nil {
		return nil, err
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	vaultRoot := ResolveVaultRoot(cfg.VaultPath, defaultVaultDir)
	if _, err := OpenVault(vaultRoot); err != nil {
		_ = store.Close()
		return nil, err
	}
	opTokenPath := ""
	if configPath != "" {
		opTokenPath = filepath.Join(filepath.Dir(configPath), "op-service-account.token")
	}
	opReader := NewOPReader(opTokenPath)
	e := &Engine{
		store:       store,
		configPath:  configPath,
		opTokenPath: opTokenPath,
		vaultDir:    defaultVaultDir,
		deps:        deps,
		llm:         newOpenRouterClient(),
		mcp:         NewMCPManager(cfg.MCPServers, opReader),
		opReader:    opReader,
		cfg:         cfg,
		subscribers: make(map[chan StreamEvent]struct{}),
		notifyCh:    make(chan struct{}, 1),
	}
	return e, nil
}

// Start launches the single inbox worker and 30s timer/heartbeat scheduler.
func (e *Engine) Start(parent context.Context) {
	e.cfgMu.Lock()
	if e.cancel != nil {
		e.cfgMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	e.cancel = cancel
	e.cfgMu.Unlock()

	e.wg.Add(2)
	go e.runWorker(ctx)
	go e.runScheduler(ctx)
}

// Stop stops background goroutines and waits for them to exit.
func (e *Engine) Stop() {
	e.cfgMu.Lock()
	cancel := e.cancel
	e.cancel = nil
	e.cfgMu.Unlock()
	if cancel != nil {
		cancel()
	}
	e.wg.Wait()
	if e.mcp != nil {
		e.mcp.Close()
	}
}

// Close stops workers and closes assistant.db.
func (e *Engine) Close() error {
	e.Stop()
	if e.store != nil {
		return e.store.Close()
	}
	return nil
}

// Store exposes the backing Store for HTTP/CLI reads.
func (e *Engine) Store() *Store { return e.store }

// Vault returns the active path-jailed Vault instance.
func (e *Engine) Vault() (*Vault, error) { return e.vault() }

func (e *Engine) vault() (*Vault, error) {
	cfg := e.Config()
	return OpenVault(ResolveVaultRoot(cfg.VaultPath, e.vaultDir))
}

// ResolveVault opens either the primary "agent" vault (default) or a mounted extra vault without scaffolding it.
func (e *Engine) ResolveVault(name string) (*Vault, VaultMount, error) {
	cfg := e.Config()
	target := strings.ToLower(strings.TrimSpace(name))
	if target == "" || target == PrimaryVaultName {
		v, err := e.vault()
		if err != nil {
			return nil, VaultMount{}, err
		}
		return v, VaultMount{
			Name:          PrimaryVaultName,
			Path:          v.Root,
			AutoWriteDirs: append([]string(nil), cfg.AutoWriteDirs...),
		}, nil
	}
	for _, m := range cfg.ExtraVaults {
		if m.Name == target {
			resolved := ResolveVaultRoot(m.Path, "")
			v, err := OpenVaultNoScaffold(resolved)
			if err != nil {
				return nil, VaultMount{}, err
			}
			return v, VaultMount{
				Name:          m.Name,
				Path:          v.Root,
				ReadOnly:      m.ReadOnly,
				AutoWriteDirs: append([]string(nil), m.AutoWriteDirs...),
			}, nil
		}
	}
	return nil, VaultMount{}, newErr(fmt.Sprintf("unknown vault %q", name), http.StatusNotFound)
}

// SearchVaults searches Markdown notes across a specific vault or all mounted vaults ("all" or empty).
func (e *Engine) SearchVaults(vaultName, query string, limit int) ([]VaultSearchHit, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	target := strings.ToLower(strings.TrimSpace(vaultName))
	if target != "" && target != "all" {
		v, mount, err := e.ResolveVault(target)
		if err != nil {
			return nil, err
		}
		hits, err := v.Search(query, limit)
		if err != nil {
			return nil, err
		}
		for i := range hits {
			hits[i].Vault = mount.Name
		}
		return hits, nil
	}
	var out []VaultSearchHit
	if pv, err := e.vault(); err == nil {
		if hits, sErr := pv.Search(query, limit); sErr == nil {
			for i := range hits {
				hits[i].Vault = PrimaryVaultName
			}
			out = append(out, hits...)
		}
	}
	for _, m := range e.Config().ExtraVaults {
		if len(out) >= limit {
			break
		}
		v, err := OpenVaultNoScaffold(ResolveVaultRoot(m.Path, ""))
		if err != nil {
			continue
		}
		hits, sErr := v.Search(query, limit-len(out))
		if sErr != nil {
			continue
		}
		for i := range hits {
			hits[i].Vault = m.Name
		}
		out = append(out, hits...)
	}
	return out, nil
}

// Config returns a copy of the current assistant Config.
func (e *Engine) Config() Config {
	e.cfgMu.RLock()
	defer e.cfgMu.RUnlock()
	return e.cfg
}

// ConfigView returns the safe non-secret ConfigView.
func (e *Engine) ConfigView() ConfigView {
	cfg := e.Config()
	return cfg.View(ResolveVaultRoot(cfg.VaultPath, e.vaultDir))
}

// Status returns combined config view and live queue counts.
func (e *Engine) Status() map[string]any {
	return map[string]any{
		"config":  e.ConfigView(),
		"summary": e.store.Summary(),
	}
}

// UpdateConfig validates, saves, and applies new assistant configuration.
func (e *Engine) UpdateConfig(next Config, clearAPIKey bool) (ConfigView, error) {
	cur := e.Config()
	if !clearAPIKey && strings.TrimSpace(next.APIKey) == "" && strings.TrimSpace(next.APIKeyOPRef) == "" {
		next.APIKey = cur.APIKey
		next.APIKeyOPRef = cur.APIKeyOPRef
	}
	if next.Credentials == nil {
		next.Credentials = append([]CredentialPolicy(nil), cur.Credentials...)
	}
	if next.MCPServers == nil {
		next.MCPServers = append([]MCPServerConfig(nil), cur.MCPServers...)
	} else {
		next.MCPServers = preserveMaskedMCPEnv(cur.MCPServers, next.MCPServers)
	}
	if next.ExtraVaults == nil {
		next.ExtraVaults = append([]VaultMount(nil), cur.ExtraVaults...)
	}
	next = NormalizeConfig(next)
	if err := validateExtraVaults(next.ExtraVaults); err != nil {
		return ConfigView{}, err
	}
	vaultRoot := ResolveVaultRoot(next.VaultPath, e.vaultDir)
	if _, err := OpenVault(vaultRoot); err != nil {
		return ConfigView{}, err
	}
	if err := SaveConfig(e.configPath, next); err != nil {
		return ConfigView{}, err
	}
	e.cfgMu.Lock()
	e.cfg = next
	e.cachedKey = ""
	e.cachedRef = ""
	e.cfgMu.Unlock()
	if e.mcp != nil {
		e.mcp.Reload(next.MCPServers)
	}
	e.notify()
	return next.View(vaultRoot), nil
}

// SaveOPServiceAccountToken persists or clears the 1Password service account token on disk (0600).
func (e *Engine) SaveOPServiceAccountToken(token string) error {
	if err := SaveOPToken(e.opTokenPath, token); err != nil {
		return err
	}
	e.cfgMu.Lock()
	e.cachedKey = ""
	e.cachedRef = ""
	e.opVaults = nil
	e.opVaultsAt = time.Time{}
	e.cfgMu.Unlock()
	return nil
}

func (e *Engine) listOPVaultsCached(ctx context.Context) ([]OPVaultMeta, error) {
	e.cfgMu.RLock()
	cached := e.opVaults
	at := e.opVaultsAt
	e.cfgMu.RUnlock()
	if len(cached) > 0 && time.Since(at) < 5*time.Minute {
		return cached, nil
	}
	lister := e.Probes.ListVaults
	if lister == nil {
		lister = ListOPVaults
	}
	vaults, err := lister(ctx, e.opTokenPath)
	if err != nil {
		return nil, err
	}
	e.cfgMu.Lock()
	e.opVaults = vaults
	e.opVaultsAt = time.Now()
	e.cfgMu.Unlock()
	return vaults, nil
}

func (e *Engine) resolveRuntimeConfig(ctx context.Context) (Config, error) {
	e.cfgMu.RLock()
	cfg := e.cfg
	cachedKey := e.cachedKey
	cachedRef := e.cachedRef
	cachedAt := e.cachedAt
	e.cfgMu.RUnlock()

	if strings.TrimSpace(cfg.APIKey) != "" || strings.TrimSpace(cfg.APIKeyOPRef) == "" {
		return cfg, nil
	}
	if cachedKey != "" && cachedRef == cfg.APIKeyOPRef && time.Since(cachedAt) < 5*time.Minute {
		cfg.APIKey = cachedKey
		return cfg, nil
	}
	reader := e.opReader
	if reader == nil {
		reader = defaultOPReader
	}
	secret, err := reader(ctx, cfg.APIKeyOPRef)
	if err != nil {
		return cfg, err
	}
	e.cfgMu.Lock()
	e.cachedKey = secret
	e.cachedRef = cfg.APIKeyOPRef
	e.cachedAt = time.Now()
	e.cfgMu.Unlock()
	cfg.APIKey = secret
	return cfg, nil
}

func preserveMaskedMCPEnv(prev, next []MCPServerConfig) []MCPServerConfig {
	prevByName := make(map[string]MCPServerConfig, len(prev))
	for _, s := range prev {
		prevByName[s.Name] = s
	}
	out := make([]MCPServerConfig, 0, len(next))
	for _, srv := range next {
		ps, ok := prevByName[srv.Name]
		if ok && len(srv.Env) > 0 && len(ps.Env) > 0 {
			restored := make(map[string]string, len(srv.Env))
			for k, v := range srv.Env {
				if oldVal, exists := ps.Env[k]; exists && v == maskSecret(oldVal) {
					restored[k] = oldVal
				} else {
					restored[k] = v
				}
			}
			srv.Env = restored
		}
		out = append(out, srv)
	}
	return out
}

// TestConnection verifies OpenRouter connectivity and model response.
func (e *Engine) TestConnection(ctx context.Context) error {
	cfg, err := e.resolveRuntimeConfig(ctx)
	if err != nil {
		return err
	}
	_, err = e.llm.CompleteText(ctx, cfg, cfg.Models.Fast, "Reply with OK.", "ping", 16)
	return err
}

// Subscribe registers an SSE subscriber channel.
func (e *Engine) Subscribe() chan StreamEvent {
	ch := make(chan StreamEvent, 64)
	e.subMu.Lock()
	e.subscribers[ch] = struct{}{}
	e.subMu.Unlock()
	return ch
}

// Unsubscribe removes an SSE subscriber channel.
func (e *Engine) Unsubscribe(ch chan StreamEvent) {
	e.subMu.Lock()
	delete(e.subscribers, ch)
	e.subMu.Unlock()
}

func (e *Engine) publishEvent(ev StreamEvent) {
	e.subMu.Lock()
	defer e.subMu.Unlock()
	for ch := range e.subscribers {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (e *Engine) notify() {
	select {
	case e.notifyCh <- struct{}{}:
	default:
	}
}

// EnqueueUserMessage stores a user message, queues a wakeup, and wakes the worker.
func (e *Engine) EnqueueUserMessage(channelID, content, modelOverride string) (Message, InboxEvent, error) {
	channelID = cmp.Or(strings.TrimSpace(channelID), "general")
	content = strings.TrimSpace(content)
	if content == "" {
		return Message{}, InboxEvent{}, newErr("message content is required", http.StatusBadRequest)
	}
	ch, err := e.store.GetChannel(channelID)
	if err != nil {
		return Message{}, InboxEvent{}, err
	}
	msg, err := e.store.AppendMessage(Message{
		ChannelID:    channelID,
		ContextEpoch: ch.ContextEpoch,
		Role:         "user",
		Content:      content,
	})
	if err != nil {
		return Message{}, InboxEvent{}, err
	}
	e.publishEvent(StreamEvent{Type: "message_created", ChannelID: channelID, Data: msg})

	ev, _, err := e.store.EnqueueInbox(channelID, "chat", "", map[string]any{
		"message_id": msg.ID,
		"content":    content,
	}, modelOverride)
	if err != nil {
		return Message{}, InboxEvent{}, err
	}
	e.notify()
	return msg, ev, nil
}

// DecideApproval resolves a pending HITL approval and queues worker resumption.
func (e *Engine) DecideApproval(id string, approve bool, note string) (Approval, error) {
	apr, err := e.store.DecideApproval(id, approve, note)
	if err != nil {
		return Approval{}, err
	}
	e.publishEvent(StreamEvent{Type: "approval_decided", ChannelID: apr.ChannelID, Data: apr})
	_, _, err = e.store.EnqueueInbox(apr.ChannelID, "approval", "apr:"+apr.ID, map[string]any{
		"approval_id": apr.ID,
		"approved":    approve,
	}, "")
	if err != nil {
		return Approval{}, err
	}
	e.notify()
	return apr, nil
}

// ResetChannel starts a fresh context epoch and optionally clears channel message history,
// leaving long-term Vault Observational Memory and Fleet shared memory untouched.
func (e *Engine) ResetChannel(id string, clearHistory bool) (Channel, error) {
	ch, err := e.store.ResetChannelContext(id, clearHistory)
	if err != nil {
		return Channel{}, err
	}
	e.publishEvent(StreamEvent{
		Type:      "channel_reset",
		ChannelID: ch.ID,
		Data:      map[string]any{"channel": ch, "cleared": clearHistory},
	})
	return ch, nil
}

func (e *Engine) runWorker(ctx context.Context) {
	defer e.wg.Done()
	for {
		if ctx.Err() != nil {
			return
		}
		ev, err := e.store.ClaimNextInbox()
		if err != nil || ev == nil {
			select {
			case <-ctx.Done():
				return
			case <-e.notifyCh:
			case <-time.After(2 * time.Second):
			}
			continue
		}
		runErr := e.processInboxEvent(ctx, *ev)
		if runErr != nil {
			_ = e.store.FinishInbox(ev.ID, "failed", runErr.Error())
			e.publishEvent(StreamEvent{Type: "error", ChannelID: ev.ChannelID, Data: map[string]string{"error": runErr.Error()}})
		} else {
			_ = e.store.FinishInbox(ev.ID, "done", "")
		}
	}
}

func (e *Engine) runScheduler(ctx context.Context) {
	defer e.wg.Done()
	lastHeartbeat := time.Now().Add(-time.Hour)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			cfg := e.Config()
			if !cfg.Enabled || (strings.TrimSpace(cfg.APIKey) == "" && strings.TrimSpace(cfg.APIKeyOPRef) == "") {
				continue
			}
			nowISO := now.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05+00:00")
			if due, err := e.store.DueTimers(nowISO); err == nil {
				for _, t := range due {
					dedupe := fmt.Sprintf("tmr:%s:%s", t.ID, nowISO)
					if _, ins, qErr := e.store.EnqueueInbox(t.ChannelID, "timer", dedupe, map[string]any{"timer_id": t.ID, "prompt": t.Prompt}, t.Model); qErr == nil && ins {
						_ = e.store.MarkTimerRun(t, now)
						e.notify()
					}
				}
			}
			if cfg.HeartbeatIntervalMinutes > 0 && now.Sub(lastHeartbeat) >= time.Duration(cfg.HeartbeatIntervalMinutes)*time.Minute {
				lastHeartbeat = now
				if v, vErr := e.vault(); vErr == nil {
					if hb, rErr := v.ReadFile("_agent/HEARTBEAT.md"); rErr == nil && NonEmptyBody(hb) {
						dedupe := fmt.Sprintf("hb:%s", now.UTC().Format("2006-01-02T15:04"))
						_, _, _ = e.store.EnqueueInbox("briefing", "heartbeat", dedupe, map[string]any{"prompt": hb}, cfg.Models.Heartbeat)
						e.notify()
					}
				}
			}
		}
	}
}

func (e *Engine) processInboxEvent(ctx context.Context, ev InboxEvent) error {
	cfg, err := e.resolveRuntimeConfig(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return newErr("assistant API key is not configured", http.StatusBadRequest)
	}
	ch, err := e.store.GetChannel(ev.ChannelID)
	if err != nil {
		return err
	}
	var payload map[string]any
	_ = json.Unmarshal([]byte(ev.PayloadJSON), &payload)

	if ev.Source == "approval" {
		return e.handleApprovalResume(ctx, cfg, *ch, payload)
	}
	userText, _ := payload["content"].(string)
	if userText == "" {
		userText, _ = payload["prompt"].(string)
	}
	if ev.Source != "chat" && userText != "" {
		_, _ = e.store.AppendMessage(Message{
			ChannelID:    ch.ID,
			ContextEpoch: ch.ContextEpoch,
			Role:         "user",
			Content:      fmt.Sprintf("[%s] %s", ev.Source, userText),
		})
	}
	return e.runAgentTurn(ctx, cfg, *ch, ev.Source, userText, ev.ModelOverride)
}

func (e *Engine) handleApprovalResume(ctx context.Context, cfg Config, ch Channel, payload map[string]any) error {
	aprID, _ := payload["approval_id"].(string)
	apr, err := e.store.GetApproval(aprID)
	if err != nil {
		return err
	}
	var toolSummary string
	if apr.Status == "approved" {
		var widgets []WidgetPayload
		res, execErr := e.executeTool(ctx, ToolCall{
			ID:       apr.ID,
			Type:     "function",
			Function: ToolCallFunction{Name: apr.ToolName, Arguments: apr.ArgsJSON},
		}, &toolExecContext{ChannelID: ch.ID, Source: "approval", Config: cfg, BypassHITL: true, Widgets: &widgets})
		if execErr != nil {
			_ = e.store.SetApprovalResult(apr.ID, "failed", execErr.Error())
			toolSummary = fmt.Sprintf("Approved action `%s` failed: %s", apr.ToolName, execErr.Error())
		} else {
			_ = e.store.SetApprovalResult(apr.ID, "executed", res)
			toolSummary = fmt.Sprintf("Approved action `%s` executed.\nResult: %s", apr.ToolName, truncateText(res, 1200))
		}
	} else {
		toolSummary = fmt.Sprintf("Action `%s` was denied by user (%s).", apr.ToolName, cmp.Or(apr.DecisionNote, "no note"))
	}
	_, _ = e.store.AppendMessage(Message{
		ChannelID:    ch.ID,
		ContextEpoch: ch.ContextEpoch,
		Role:         "user",
		Content:      "[approval_result] " + toolSummary,
	})
	return e.runAgentTurn(ctx, cfg, ch, "approval", toolSummary, "")
}

func parseSlashModelOverride(text string, aliases map[string]string) (model string, cleaned string) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/") {
		return "", text
	}
	first, rest, hasRest := strings.Cut(trimmed, " ")
	key := strings.ToLower(strings.TrimPrefix(first, "/"))
	if resolved, ok := aliases[key]; ok && hasRest && strings.TrimSpace(rest) != "" {
		return resolved, strings.TrimSpace(rest)
	}
	return "", text
}

func (e *Engine) resolveModel(ctx context.Context, cfg Config, ch Channel, source, userText, explicit string, skills []Skill) string {
	slashModel, cleanText := parseSlashModelOverride(userText, cfg.Models.Aliases)
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
		if source == "heartbeat" {
			chosen = cfg.Models.Heartbeat
		} else {
			chosen = cfg.Models.DefaultChat
		}
	}
	if alias, ok := cfg.Models.Aliases[strings.ToLower(chosen)]; ok {
		chosen = alias
	}
	if strings.EqualFold(chosen, "auto") {
		if ans, err := e.llm.Decide(ctx, cfg, cleanText, []DecisionQuestion{{
			ID:           "tier",
			Type:         "choice",
			Instructions: "Choose model capability tier needed for this user request.",
			Criteria: map[string]string{
				"fast":     "Simple question, greeting, reminder, or single-step status lookup",
				"balanced": "Normal assistant work, tool orchestration, summarization, or multi-step chat",
				"frontier": "Complex planning, deep reasoning, architecture design, or subtle analysis",
			},
		}}); err == nil {
			switch ans["tier"].Value {
			case "fast":
				return cfg.Models.Fast
			case "frontier":
				return cfg.Models.Frontier
			}
		}
		return cfg.Models.DefaultChat
	}
	return chosen
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
	if len(skills) > 0 {
		sysStatic.WriteString("\n## Available Skills (call skill_read for full instructions)\n")
		for _, sk := range skills {
			fmt.Fprintf(&sysStatic, "- %s: %s\n", sk.Name, cmp.Or(sk.Description, "(no description)"))
		}
	}
	if fleetCtx != "" {
		sysStatic.WriteString("\n## Fleet Standing Memory\n" + fleetCtx + "\n")
	}
	if vaultsSection := e.buildVaultsPromptSection(); vaultsSection != "" {
		sysStatic.WriteString(vaultsSection)
	}
	if opSection := e.buildOPPromptSection(ctx); opSection != "" {
		sysStatic.WriteString(opSection)
	}

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
			_, txt = parseSlashModelOverride(txt, slashAliases)
		}
		out = append(out, ChatMessage{Role: m.Role, Content: txt})
	}
	return out
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
	return b.String()
}

func (e *Engine) runAgentTurn(ctx context.Context, cfg Config, ch Channel, source, userText, explicitModel string) error {
	v, _ := e.vault()
	var skills []Skill
	if v != nil {
		skills, _ = v.ListSkills()
	}
	model := e.resolveModel(ctx, cfg, ch, source, userText, explicitModel, skills)
	tail, err := e.store.ListContextMessages(ch.ID, ch.ContextEpoch, 40)
	if err != nil {
		return err
	}
	msgs := e.buildPromptMessages(ctx, ch, tail, cfg.Models.Aliases)
	tools := e.buildToolSpecs(ctx)

	e.publishEvent(StreamEvent{Type: "run_start", ChannelID: ch.ID, Data: map[string]any{"model": model}})
	var widgets []WidgetPayload
	tctx := &toolExecContext{ChannelID: ch.ID, Source: source, Config: cfg, Widgets: &widgets}

	var totalUsage StreamUsage
	for range maxToolRounds {
		turn, err := e.llm.StreamChat(ctx, cfg, model, msgs, tools, func(delta string) {
			e.publishEvent(StreamEvent{Type: "delta", ChannelID: ch.ID, Data: map[string]string{"delta": delta}})
		})
		if err != nil {
			return err
		}
		totalUsage.PromptTokens += turn.Usage.PromptTokens
		totalUsage.CachedTokens += turn.Usage.CachedTokens
		totalUsage.CompletionTokens += turn.Usage.CompletionTokens
		totalUsage.CostUSD += turn.Usage.CostUSD

		if len(turn.ToolCalls) == 0 {
			return e.finishTurnSuccess(ctx, cfg, ch, model, turn.Content, totalUsage, widgets)
		}
		msgs = append(msgs, ChatMessage{Role: "assistant", Content: turn.Content, ToolCalls: turn.ToolCalls})
		for _, tc := range turn.ToolCalls {
			e.publishEvent(StreamEvent{Type: "tool_start", ChannelID: ch.ID, Data: map[string]string{"name": tc.Function.Name}})
			outStr, execErr := e.executeTool(ctx, tc, tctx)
			if aprErr, ok := errors.AsType[*approvalRequiredError](execErr); ok {
				return e.pauseForApproval(ch, model, totalUsage, widgets, aprErr)
			}
			if execErr != nil {
				outStr = fmt.Sprintf(`{"error":%q}`, execErr.Error())
			}
			e.publishEvent(StreamEvent{Type: "tool_done", ChannelID: ch.ID, Data: map[string]string{"name": tc.Function.Name}})
			msgs = append(msgs, ChatMessage{Role: "tool", ToolCallID: tc.ID, Content: outStr})
		}
	}
	return e.finishTurnSuccess(ctx, cfg, ch, model, "Stopped after reaching max tool rounds.", totalUsage, widgets)
}

func (e *Engine) finishTurnSuccess(ctx context.Context, cfg Config, ch Channel, model, content string, usage StreamUsage, widgets []WidgetPayload) error {
	msg, err := e.store.AppendMessage(Message{
		ChannelID:        ch.ID,
		ContextEpoch:     ch.ContextEpoch,
		Role:             "assistant",
		Content:          strings.TrimSpace(content),
		Model:            model,
		PromptTokens:     usage.PromptTokens,
		CachedTokens:     usage.CachedTokens,
		CompletionTokens: usage.CompletionTokens,
		CostUSD:          usage.CostUSD,
		Widgets:          widgets,
	})
	if err != nil {
		return err
	}
	e.publishEvent(StreamEvent{Type: "message_done", ChannelID: ch.ID, Data: msg})
	e.maybeAutoObserve(ctx, cfg, ch.ID)
	return nil
}

func (e *Engine) pauseForApproval(ch Channel, model string, usage StreamUsage, widgets []WidgetPayload, aprErr *approvalRequiredError) error {
	msg, err := e.store.AppendMessage(Message{
		ChannelID:        ch.ID,
		ContextEpoch:     ch.ContextEpoch,
		Role:             "assistant",
		Content:          "Paused awaiting your approval: " + aprErr.Summary,
		Model:            model,
		PromptTokens:     usage.PromptTokens,
		CachedTokens:     usage.CachedTokens,
		CompletionTokens: usage.CompletionTokens,
		CostUSD:          usage.CostUSD,
		Widgets:          widgets,
	})
	if err != nil {
		return err
	}
	apr, err := e.store.CreateApproval(Approval{
		ChannelID:  ch.ID,
		MessageID:  msg.ID,
		ToolName:   aprErr.ToolName,
		ServerName: aprErr.ServerName,
		ArgsJSON:   aprErr.ArgsJSON,
		Summary:    aprErr.Summary,
		RiskLevel:  aprErr.RiskLevel,
	})
	if err != nil {
		return err
	}
	e.publishEvent(StreamEvent{Type: "message_done", ChannelID: ch.ID, Data: msg})
	e.publishEvent(StreamEvent{Type: "approval", ChannelID: ch.ID, Data: apr})
	return nil
}

func (e *Engine) maybeAutoObserve(ctx context.Context, cfg Config, channelID string) {
	msgs, err := e.store.ListUnobservedMessages(channelID, 200)
	if err != nil || len(msgs) < 6 {
		return
	}
	var chars int
	for _, m := range msgs {
		chars += len(m.Content)
	}
	if chars/4 < cfg.ObserverThresholdTokens {
		return
	}
	_, _ = e.RunObserver(ctx, channelID)
}

// RunObserver distills unobserved messages into _agent/memory/OBSERVATIONS.md and triggers Reflector when large.
func (e *Engine) RunObserver(ctx context.Context, channelID string) (map[string]any, error) {
	cfg, err := e.resolveRuntimeConfig(ctx)
	if err != nil {
		return nil, err
	}
	msgs, err := e.store.ListUnobservedMessages(channelID, 200)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return map[string]any{"observed": 0, "reflected": false}, nil
	}
	var transcript strings.Builder
	var maxID int64
	for _, m := range msgs {
		if m.ID > maxID {
			maxID = m.ID
		}
		fmt.Fprintf(&transcript, "[%s #%s %s] %s\n", m.CreatedAt, m.ChannelID, m.Role, m.Content)
	}
	sys := `You are the Observational Memory Observer.
Convert the raw transcript into concise, dated Markdown bullet observations using priority markers:
- 🔴 critical decisions, blockers, explicit user instructions
- 🟡 ongoing tasks, preferences, context
- 🟢 minor FYI facts
Omit small talk. Output only Markdown bullets.`
	obsText, err := e.llm.CompleteText(ctx, cfg, cfg.Models.Observer, sys, transcript.String(), 1024)
	if err != nil {
		return nil, err
	}
	v, err := e.vault()
	if err != nil {
		return nil, err
	}
	header := fmt.Sprintf("\n### %s\n%s\n", time.Now().UTC().Format("2006-01-02 15:04 UTC"), strings.TrimSpace(obsText))
	if _, err := v.AppendFile("_agent/memory/OBSERVATIONS.md", header); err != nil {
		return nil, err
	}
	if err := e.store.MarkMessagesObserved(channelID, maxID); err != nil {
		return nil, err
	}
	v.MaybeGitCommit(cfg.GitAutoCommit, "assistant: observational memory pass")

	reflected := false
	if curObs, rErr := v.ReadFile("_agent/memory/OBSERVATIONS.md"); rErr == nil && len(curObs) > cfg.ReflectorThresholdBytes {
		if _, refErr := e.RunReflector(ctx); refErr == nil {
			reflected = true
		}
	}
	return map[string]any{"observed": len(msgs), "reflected": reflected}, nil
}

// RunReflector condenses _agent/memory/OBSERVATIONS.md into _agent/memory/REFLECTIONS.md and archives old observations.
func (e *Engine) RunReflector(ctx context.Context) (string, error) {
	cfg, err := e.resolveRuntimeConfig(ctx)
	if err != nil {
		return "", err
	}
	v, err := e.vault()
	if err != nil {
		return "", err
	}
	reflections, _ := v.ReadFile("_agent/memory/REFLECTIONS.md")
	observations, err := v.ReadFile("_agent/memory/OBSERVATIONS.md")
	if err != nil {
		return "", err
	}
	sys := `You are the Observational Memory Reflector.
Merge the existing REFLECTIONS.md with new OBSERVATIONS.md into an updated, compact REFLECTIONS.md.
Preserve durable preferences, active projects, open follow-ups, and key dates; drop resolved or obsolete items.
Output only the updated Markdown for REFLECTIONS.md.`
	prompt := "## Current REFLECTIONS.md\n" + reflections + "\n\n## New OBSERVATIONS.md\n" + observations
	nextRefl, err := e.llm.CompleteText(ctx, cfg, cfg.Models.Reflector, sys, prompt, 1500)
	if err != nil {
		return "", err
	}
	archiveRel := filepath.ToSlash(filepath.Join("_agent/memory/archive", time.Now().UTC().Format("2006-01")+".md"))
	_, _ = v.AppendFile(archiveRel, observations)
	if _, err := v.WriteFile("_agent/memory/REFLECTIONS.md", strings.TrimSpace(nextRefl)+"\n"); err != nil {
		return "", err
	}
	if _, err := v.WriteFile("_agent/memory/OBSERVATIONS.md", "# Observations\n_Append-only dated observation log maintained by the background Observer._\n"); err != nil {
		return "", err
	}
	v.MaybeGitCommit(cfg.GitAutoCommit, "assistant: reflector compaction")
	return nextRefl, nil
}

// SetOPReader overrides 1Password resolution (used by tests or custom brokers).
func (e *Engine) SetOPReader(fn OPReaderFunc) {
	e.opReader = fn
	if e.mcp != nil {
		e.mcp.reader = fn
	}
}
