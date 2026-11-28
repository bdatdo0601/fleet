package assistant

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bdatdo0601/fleet/internal/paths"
)

const (
	maxToolRounds = 14

	// FrontierEscalationTool is the synthetic HITL approval tool name used when gating frontier model escalation.
	FrontierEscalationTool = "frontier_escalation"
)

type frontierEscalationArgs struct {
	OriginalPrompt string  `json:"original_prompt"`
	FrontierModel  string  `json:"frontier_model"`
	FallbackModel  string  `json:"fallback_model"`
	Probability    float64 `json:"probability"`
	TopicHint      string  `json:"topic_hint"`
	DailySpendUSD  float64 `json:"daily_spend_usd,omitempty"`
	GrantMaxTurns  int     `json:"grant_max_turns,omitempty"`
	GrantMaxCost   float64 `json:"grant_max_cost,omitempty"`
}

type ephemeralToolResult struct {
	CallID   string
	ToolName string
	ArgsJSON string
	Content  string
}

type turnOptions struct {
	RetryReplaceMsgID   int64
	TrackGrant          bool
	AutoApprove         bool
	TriggerSource       string
	TriggerUserText     string
	ReplyToExternalID   string
	EphemeralToolResult *ephemeralToolResult
	InitialWidgets      []WidgetPayload
}

type turnRoute struct {
	Model          string
	CleanText      string
	BypassHITL     bool
	Escalate       *frontierEscalationArgs
	BorderlineProb float64
	UsedGrant      *FrontierGrant
}

// StreamEvent is pushed over SSE to open Fleet Console #chat sessions.
type StreamEvent struct {
	Type      string `json:"type"`
	ChannelID string `json:"channel_id,omitempty"`
	Data      any    `json:"data,omitempty"`
}

// Engine coordinates assistant storage, Vault, OpenRouter, Decisions, MCP, sandbox, timers, and SSE streaming.
type Engine struct {
	store       *Store
	configPath  string
	opTokenPath string
	vaultDir    string
	filesDir    string
	deps        FleetDeps
	llm         *openRouterClient
	mcp         *MCPManager
	skills      *SkillRegistry
	sandbox     *SandboxManager
	Probes      DoctorProbes

	opMu     sync.RWMutex
	opReader OPReaderFunc

	configWriteMu sync.Mutex
	memoryMu      sync.Mutex

	cfgMu      sync.RWMutex
	cfg        Config
	cachedKey  string
	cachedRef  string
	cachedAt   time.Time
	opVaults   []OPVaultMeta
	opVaultsAt time.Time
	stopCh     chan struct{}
	cancel     context.CancelFunc
	stopped    bool

	subMu       sync.Mutex
	subscribers map[chan StreamEvent]struct{}

	delMu                sync.RWMutex
	hasActiveDelegations bool
	notifier             Notifier

	discordMu             sync.RWMutex
	discordSyncMu         sync.Mutex
	discordConnected      bool
	discordBotUserID      string
	discordCategoryID     string
	discordAPIBase        string
	discordGatewayURL     string
	discordMaxUploadBytes int64
	discordReloadCh       chan struct{}

	notifyCh chan struct{}
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
	stateRoot := filepath.Dir(defaultVaultDir)
	if configPath != "" {
		stateRoot = filepath.Dir(configPath)
		opTokenPath = filepath.Join(stateRoot, "op-service-account.token")
	}
	opReader := NewOPReader(opTokenPath)
	mcpMgr := NewMCPManager(cfg.MCPServers, opReader)
	mcpMgr.SetOAuthStore(store)
	e := &Engine{
		store:           store,
		configPath:      configPath,
		opTokenPath:     opTokenPath,
		vaultDir:        defaultVaultDir,
		filesDir:        filepath.Join(stateRoot, "assistant-files"),
		deps:            deps,
		llm:             newOpenRouterClient(),
		mcp:             mcpMgr,
		skills:          NewSkillRegistry(),
		sandbox:         NewSandboxManager(filepath.Join(stateRoot, "sandbox"), NewSocketSandboxRunner(paths.SandboxSocketPath(stateRoot), NewAppleContainerRunner())),
		opReader:        opReader,
		cfg:             cfg,
		subscribers:     make(map[chan StreamEvent]struct{}),
		discordReloadCh: make(chan struct{}, 1),
		notifyCh:        make(chan struct{}, 1),
	}
	e.refreshDelegationsFlag()
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
	e.stopCh = make(chan struct{})
	e.stopped = false
	e.wg.Add(3)
	e.cfgMu.Unlock()

	go e.runWorker(ctx)
	go e.runScheduler(ctx)
	go e.runDiscordGateway(ctx)
}

func (e *Engine) trackWG(fn func()) {
	e.cfgMu.RLock()
	if e.stopped {
		e.cfgMu.RUnlock()
		return
	}
	e.wg.Add(1)
	e.cfgMu.RUnlock()
	go func() {
		defer e.wg.Done()
		fn()
	}()
}

func (e *Engine) spawnBG(fallbackCtx context.Context, timeout time.Duration, fn func(context.Context)) {
	e.cfgMu.RLock()
	if e.stopped {
		e.cfgMu.RUnlock()
		return
	}
	stopCh := e.stopCh
	e.wg.Add(1)
	e.cfgMu.RUnlock()

	base := context.WithoutCancel(fallbackCtx)
	var bgCtx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		bgCtx, cancel = context.WithTimeout(base, timeout)
	} else {
		bgCtx, cancel = context.WithCancel(base)
	}
	var watchWG sync.WaitGroup
	if stopCh != nil {
		watchWG.Go(func() {
			select {
			case <-stopCh:
				cancel()
			case <-bgCtx.Done():
			}
		})
	}
	go func() {
		defer e.wg.Done()
		defer watchWG.Wait()
		defer cancel()
		fn(bgCtx)
	}()
}

// Stop stops background goroutines and waits for them to exit.
func (e *Engine) Stop() {
	e.cfgMu.Lock()
	e.stopped = true
	cancel := e.cancel
	e.cancel = nil
	stopCh := e.stopCh
	e.stopCh = nil
	e.cfgMu.Unlock()
	if stopCh != nil {
		close(stopCh)
	}
	if cancel != nil {
		cancel()
	}
	e.wg.Wait()
	if e.mcp != nil {
		e.mcp.Close()
	}
	if e.sandbox != nil {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		e.sandbox.Close(stopCtx)
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
	return e.EnqueueUserMessageWithOptions(channelID, content, modelOverride, nil, false)
}

// EnqueueExternalMessage idempotently ingests a user message from an external bridge (e.g. Discord).
func (e *Engine) EnqueueExternalMessage(channelID, content, source, externalID string, fileIDs []string) (Message, InboxEvent, bool, error) {
	return e.enqueueMessageInternal(channelID, content, "", fileIDs, source, externalID, false)
}

// EnqueueUserMessageWithFiles stores a user message with optional uploaded file attachments and wakes the worker.
func (e *Engine) EnqueueUserMessageWithFiles(channelID, content, modelOverride string, fileIDs []string) (Message, InboxEvent, error) {
	return e.EnqueueUserMessageWithOptions(channelID, content, modelOverride, fileIDs, false)
}

// EnqueueUserMessageWithOptions stores a user message with optional attachments and per-turn auto-approve override.
func (e *Engine) EnqueueUserMessageWithOptions(channelID, content, modelOverride string, fileIDs []string, autoApprove bool) (Message, InboxEvent, error) {
	msg, ev, _, err := e.enqueueMessageInternal(channelID, content, modelOverride, fileIDs, "web", "", autoApprove)
	return msg, ev, err
}

func (e *Engine) enqueueMessageInternal(channelID, content, modelOverride string, fileIDs []string, source, externalID string, autoApprove bool) (Message, InboxEvent, bool, error) {
	channelID = cmp.Or(strings.TrimSpace(channelID), "general")
	content = strings.TrimSpace(content)
	cleanIDs := normalizeStringListOrdered(fileIDs)
	if content == "" && len(cleanIDs) == 0 {
		return Message{}, InboxEvent{}, false, newErr("message content is required", http.StatusBadRequest)
	}
	if externalID != "" && e.store.HasExternalMessage(source, externalID) {
		return Message{}, InboxEvent{}, false, nil
	}
	ch, err := e.store.GetChannel(channelID)
	if err != nil {
		return Message{}, InboxEvent{}, false, err
	}
	var widgets []WidgetPayload
	if len(cleanIDs) > 0 {
		entries := make([]any, 0, len(cleanIDs))
		for _, id := range cleanIDs {
			f, fErr := e.store.GetFile(id)
			if fErr != nil {
				return Message{}, InboxEvent{}, false, fErr
			}
			if f.ChannelID != channelID {
				return Message{}, InboxEvent{}, false, newErr(fmt.Sprintf("file %q belongs to channel #%s", id, f.ChannelID), http.StatusBadRequest)
			}
			entries = append(entries, fileWidgetEntry(*f))
		}
		widgets = append(widgets, WidgetPayload{
			Kind: "files",
			Data: map[string]any{"files": entries},
		})
		if content == "" {
			content = "Please inspect the attached file(s)."
		}
	}
	msg, err := e.store.AppendMessage(Message{
		ChannelID:    channelID,
		ContextEpoch: ch.ContextEpoch,
		Role:         "user",
		Content:      content,
		Widgets:      widgets,
		Source:       source,
		ExternalID:   externalID,
	})
	if err != nil {
		return Message{}, InboxEvent{}, false, err
	}
	if err := e.store.AttachFilesToMessage(channelID, msg.ID, cleanIDs); err != nil {
		return Message{}, InboxEvent{}, false, err
	}
	e.publishEvent(StreamEvent{Type: "message_created", ChannelID: channelID, Data: msg})
	_ = e.store.CancelPendingApprovalsByTool(channelID, FrontierEscalationTool)

	dedupe := ""
	if externalID != "" {
		dedupe = source + ":" + externalID
	}
	payload := map[string]any{
		"message_id": msg.ID,
		"content":    content,
		"source":     source,
	}
	if externalID != "" {
		payload["external_id"] = externalID
	}
	if len(cleanIDs) > 0 {
		payload["file_ids"] = cleanIDs
	}
	if autoApprove {
		payload["auto_approve"] = true
	}
	ev, _, err := e.store.EnqueueInbox(channelID, "chat", dedupe, payload, modelOverride)
	if err != nil {
		return Message{}, InboxEvent{}, false, err
	}
	e.notify()
	return msg, ev, true, nil
}

func normalizeStringListOrdered(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(items))
	var out []string
	for _, raw := range items {
		clean := strings.TrimSpace(raw)
		if clean == "" {
			continue
		}
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	return out
}

// DecideApproval resolves a pending HITL approval and queues worker resumption.
func (e *Engine) DecideApproval(id string, approve bool, note string) (Approval, error) {
	return e.DecideApprovalWithScope(id, approve, "", note)
}

// DecideApprovalWithScope resolves a pending HITL approval with an optional scope ("once" or "topic").
func (e *Engine) DecideApprovalWithScope(id string, approve bool, scope, note string) (Approval, error) {
	cleanScope := strings.ToLower(strings.TrimSpace(scope))
	if cleanScope != "topic" {
		cleanScope = "once"
	}
	storedNote := strings.TrimSpace(note)
	if approve && cleanScope == "topic" && storedNote == "" {
		storedNote = "scope:topic"
	}
	apr, err := e.store.DecideApproval(id, approve, storedNote)
	if err != nil {
		return Approval{}, err
	}
	e.publishEvent(StreamEvent{Type: "approval_decided", ChannelID: apr.ChannelID, Data: apr})
	_, _, err = e.store.EnqueueInbox(apr.ChannelID, "approval", "apr:"+apr.ID, map[string]any{
		"approval_id": apr.ID,
		"approved":    approve,
		"scope":       cleanScope,
	}, "")
	if err != nil {
		return Approval{}, err
	}
	e.notify()
	return apr, nil
}

// ActiveFrontierGrant returns the currently valid frontier topic grant for channelID, if any.
func (e *Engine) ActiveFrontierGrant(channelID string) *FrontierGrant {
	ch, err := e.store.GetChannel(cmp.Or(strings.TrimSpace(channelID), "general"))
	if err != nil {
		return nil
	}
	g, err := e.store.GetActiveFrontierGrant(ch.ID, ch.ContextEpoch, time.Now().UTC())
	if err != nil {
		return nil
	}
	return g
}

// RevokeFrontierGrant clears any active frontier topic grant on channelID and notifies SSE clients.
func (e *Engine) RevokeFrontierGrant(channelID string) error {
	clean := cmp.Or(strings.TrimSpace(channelID), "general")
	if err := e.store.DeleteFrontierGrant(clean); err != nil {
		return err
	}
	e.publishEvent(StreamEvent{Type: "grant_updated", ChannelID: clean})
	return nil
}

// RetryWithFrontier re-runs a borderline prompt directly on the configured frontier model, optionally granting the topic.
func (e *Engine) RetryWithFrontier(channelID string, replaceMsgID int64, prompt string, grantTopic bool) (InboxEvent, error) {
	channelID = cmp.Or(strings.TrimSpace(channelID), "general")
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return InboxEvent{}, newErr("prompt is required", http.StatusBadRequest)
	}
	ch, err := e.store.GetChannel(channelID)
	if err != nil {
		return InboxEvent{}, err
	}
	_ = e.store.CancelPendingApprovalsByTool(ch.ID, FrontierEscalationTool)
	cfg := e.Config()
	if grantTopic {
		exp := time.Now().UTC().Add(time.Duration(cfg.Routing.GrantIdleMinutes) * time.Minute).Format(time.RFC3339)
		if _, gErr := e.store.UpsertFrontierGrant(FrontierGrant{
			ChannelID:    ch.ID,
			ContextEpoch: ch.ContextEpoch,
			TopicSummary: truncateText(prompt, 160),
			TurnsUsed:    0,
			MaxTurns:     cfg.Routing.GrantMaxTurns,
			CostUSD:      0,
			MaxCostUSD:   cfg.Routing.GrantMaxCostUSD,
			ExpiresAt:    exp,
		}); gErr != nil {
			return InboxEvent{}, gErr
		}
		e.publishEvent(StreamEvent{Type: "grant_updated", ChannelID: ch.ID})
	}
	ev, _, err := e.store.EnqueueInbox(ch.ID, "chat", "", map[string]any{
		"content":              prompt,
		"retry_replace_msg_id": replaceMsgID,
		"use_grant":            grantTopic,
	}, cfg.Models.Frontier)
	if err != nil {
		return InboxEvent{}, err
	}
	e.notify()
	return ev, nil
}

// UpsertChannel creates or updates a Fleet channel and triggers Discord auto-sync if needed.
func (e *Engine) UpsertChannel(ctx context.Context, id, title, description, model string) (Channel, error) {
	ch, err := e.store.UpsertChannel(id, title, description, model)
	if err != nil {
		return Channel{}, err
	}
	e.publishEvent(StreamEvent{
		Type:      "channels_updated",
		ChannelID: ch.ID,
		Data:      map[string]any{"channel": ch},
	})
	d := e.Config().Discord
	if d.Enabled && (d.AutoSyncChannels == nil || *d.AutoSyncChannels) && d.Channels[ch.ID] == "" {
		e.spawnBG(ctx, 10*time.Second, func(syncCtx context.Context) {
			_, _ = e.SyncDiscordChannels(syncCtx)
		})
	}
	return ch, nil
}

// ResetChannel starts a fresh context epoch and optionally clears channel message history,
// leaving long-term Vault Observational Memory and Fleet shared memory untouched.
func (e *Engine) ResetChannel(id string, clearHistory bool) (Channel, error) {
	return e.resetChannelInternal(context.Background(), id, clearHistory, "web")
}

func (e *Engine) resetChannelInternal(ctx context.Context, id string, clearHistory bool, origin string) (Channel, error) {
	ch, err := e.store.ResetChannelContext(id, clearHistory)
	if err != nil {
		return Channel{}, err
	}
	e.publishEvent(StreamEvent{
		Type:      "channel_reset",
		ChannelID: ch.ID,
		Data:      map[string]any{"channel": ch, "cleared": clearHistory},
	})
	if origin != "discord" {
		cfg := e.Config()
		if cfg.Discord.Enabled && cfg.Discord.Channels[ch.ID] != "" {
			e.spawnBG(ctx, 15*time.Second, func(postCtx context.Context) {
				e.postDiscordContextCleared(postCtx, cfg, ch, origin, clearHistory)
			})
		}
	}
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
			if e.sandbox != nil {
				e.sandbox.ReapIdle(ctx, cfg.Sandbox.IdleMinutes)
			}
			if !cfg.Enabled || (strings.TrimSpace(cfg.APIKey) == "" && strings.TrimSpace(cfg.APIKeyOPRef) == "") {
				continue
			}
			e.reconcileActiveDelegations(now)
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
		prefix := ev.Source
		if ev.Source == "delegation" {
			prefix = "delegation_update"
		}
		_, _ = e.store.AppendMessage(Message{
			ChannelID:    ch.ID,
			ContextEpoch: ch.ContextEpoch,
			Role:         "user",
			Content:      fmt.Sprintf("[%s] %s", prefix, userText),
		})
	}
	var retryReplaceID int64
	if rawID, ok := payload["retry_replace_msg_id"].(float64); ok && rawID > 0 {
		retryReplaceID = int64(rawID)
	}
	useGrant, _ := payload["use_grant"].(bool)
	autoApprove, _ := payload["auto_approve"].(bool)
	trigSource := cmp.Or(strArg(payload, "source"), ev.Source)
	replyToExt := strArg(payload, "external_id")
	return e.runAgentTurn(ctx, cfg, *ch, ev.Source, userText, ev.ModelOverride, turnOptions{
		RetryReplaceMsgID: retryReplaceID,
		TrackGrant:        useGrant,
		AutoApprove:       autoApprove,
		TriggerSource:     trigSource,
		TriggerUserText:   userText,
		ReplyToExternalID: replyToExt,
	})
}

func (e *Engine) handleApprovalResume(ctx context.Context, cfg Config, ch Channel, payload map[string]any) error {
	aprID, _ := payload["approval_id"].(string)
	apr, err := e.store.GetApproval(aprID)
	if err != nil {
		return err
	}
	if apr.ToolName == FrontierEscalationTool {
		return e.handleFrontierEscalationResume(ctx, cfg, ch, apr, payload)
	}
	var toolSummary string
	var widgets []WidgetPayload
	var ephemeral *ephemeralToolResult
	if apr.Status == "approved" {
		res, execErr := e.executeTool(ctx, ToolCall{
			ID:       apr.ID,
			Type:     "function",
			Function: ToolCallFunction{Name: apr.ToolName, Arguments: apr.ArgsJSON},
		}, &toolExecContext{ChannelID: ch.ID, Source: "approval", Config: cfg, BypassHITL: true, Widgets: &widgets})
		if execErr != nil {
			_ = e.store.SetApprovalResult(apr.ID, "failed", execErr.Error())
			toolSummary = fmt.Sprintf("Approved action `%s` failed: %s", apr.ToolName, truncateText(execErr.Error(), 600))
		} else {
			_ = e.store.SetApprovalResult(apr.ID, "executed", res)
			shaped := e.formatToolResultForLLM(ch.ID, apr.ToolName, res)
			toolSummary = fmt.Sprintf("Approved action `%s` executed (%s; approval %s).", apr.ToolName, formatByteSize(int64(len(res))), apr.ID)
			ephemeral = &ephemeralToolResult{
				CallID:   apr.ID,
				ToolName: apr.ToolName,
				ArgsJSON: cmp.Or(apr.ArgsJSON, "{}"),
				Content:  shaped,
			}
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
	resumeModel := e.store.GetMessageModel(apr.MessageID)
	return e.runAgentTurn(ctx, cfg, ch, "approval", toolSummary, resumeModel, turnOptions{
		TriggerSource:       "approval",
		EphemeralToolResult: ephemeral,
		InitialWidgets:      widgets,
	})
}

func (e *Engine) handleFrontierEscalationResume(ctx context.Context, cfg Config, ch Channel, apr *Approval, payload map[string]any) error {
	var args frontierEscalationArgs
	_ = json.Unmarshal([]byte(apr.ArgsJSON), &args)
	fallback := cmp.Or(args.FallbackModel, concreteDefaultChat(cfg))
	if apr.Status != "approved" {
		return e.runAgentTurn(ctx, cfg, ch, "chat", args.OriginalPrompt, fallback, turnOptions{TriggerSource: "approval"})
	}
	scope, _ := payload["scope"].(string)
	isTopic := strings.EqualFold(scope, "topic") || strings.Contains(apr.DecisionNote, "scope:topic")
	frontierModel := cmp.Or(args.FrontierModel, cfg.Models.Frontier)
	if isTopic {
		exp := time.Now().UTC().Add(time.Duration(cfg.Routing.GrantIdleMinutes) * time.Minute).Format(time.RFC3339)
		_, _ = e.store.UpsertFrontierGrant(FrontierGrant{
			ChannelID:    ch.ID,
			ContextEpoch: ch.ContextEpoch,
			TopicSummary: cmp.Or(args.TopicHint, truncateText(args.OriginalPrompt, 160)),
			TurnsUsed:    0,
			MaxTurns:     cfg.Routing.GrantMaxTurns,
			CostUSD:      0,
			MaxCostUSD:   cfg.Routing.GrantMaxCostUSD,
			ExpiresAt:    exp,
		})
		e.publishEvent(StreamEvent{Type: "grant_updated", ChannelID: ch.ID})
	}
	_ = e.store.SetApprovalResult(apr.ID, "executed", "Approved model: "+frontierModel)
	return e.runAgentTurn(ctx, cfg, ch, "chat", args.OriginalPrompt, frontierModel, turnOptions{TrackGrant: isTopic, TriggerSource: "approval"})
}

func (e *Engine) loadTurnTail(ch Channel, opts turnOptions) ([]Message, error) {
	tail, err := e.store.ListContextMessages(ch.ID, ch.ContextEpoch, 40)
	if err != nil {
		return nil, err
	}
	if opts.RetryReplaceMsgID <= 0 {
		return tail, nil
	}
	filtered := make([]Message, 0, len(tail))
	for _, m := range tail {
		if m.ID < opts.RetryReplaceMsgID {
			filtered = append(filtered, m)
		}
	}
	return filtered, nil
}

func (e *Engine) buildRunStartData(chID, model string, route turnRoute, opts turnOptions) map[string]any {
	startData := map[string]any{"model": model}
	grantForMeta := route.UsedGrant
	if grantForMeta == nil && opts.TrackGrant {
		grantForMeta = e.ActiveFrontierGrant(chID)
	}
	if grantForMeta != nil {
		startData["grant_turn"] = grantForMeta.TurnsUsed + 1
		startData["grant_max_turns"] = grantForMeta.MaxTurns
	}
	return startData
}

func (e *Engine) runAgentTurn(ctx context.Context, cfg Config, ch Channel, source, userText, explicitModel string, opts turnOptions) error {
	var skills []Skill
	if v, err := e.vault(); err == nil && v != nil {
		skills, _ = v.ListSkills()
	}
	tail, err := e.loadTurnTail(ch, opts)
	if err != nil {
		return err
	}
	route := e.resolveTurnRoute(ctx, cfg, ch, source, userText, explicitModel, tail, skills, opts)
	if route.Escalate != nil {
		return e.pauseForFrontierEscalation(ctx, ch, route.Escalate)
	}
	model := route.Model
	msgs := e.buildPromptMessages(ctx, ch, tail, cfg.Models.Aliases)
	if opts.RetryReplaceMsgID > 0 && (len(msgs) <= 1 || msgs[len(msgs)-1].Role != "user") && strings.TrimSpace(userText) != "" {
		msgs = append(msgs, ChatMessage{Role: "user", Content: userText})
	}
	if ar := opts.EphemeralToolResult; ar != nil {
		msgs = append(msgs,
			ChatMessage{
				Role: "assistant",
				ToolCalls: []ToolCall{{
					ID:   ar.CallID,
					Type: "function",
					Function: ToolCallFunction{
						Name:      ar.ToolName,
						Arguments: cmp.Or(ar.ArgsJSON, "{}"),
					},
				}},
			},
			ChatMessage{
				Role:       "tool",
				ToolCallID: ar.CallID,
				Content:    ar.Content,
			},
		)
	}
	tools := e.buildToolSpecs(ctx)

	e.publishEvent(StreamEvent{Type: "run_start", ChannelID: ch.ID, Data: e.buildRunStartData(ch.ID, model, route, opts)})
	widgets := append([]WidgetPayload(nil), opts.InitialWidgets...)
	tctx := &toolExecContext{ChannelID: ch.ID, Source: source, Config: cfg, BypassHITL: route.BypassHITL, Widgets: &widgets}

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
			if route.BorderlineProb > 0 {
				widgets = append(widgets, buildFrontierRetryWidget(route.CleanText, cfg.Models.Frontier, route.BorderlineProb))
			}
			if err := e.finishTurnSuccess(ctx, cfg, ch, model, turn.Content, totalUsage, widgets, opts); err != nil {
				return err
			}
			e.recordTurnGrantUsage(cfg, ch.ID, totalUsage.CostUSD, route.UsedGrant != nil || opts.TrackGrant)
			return nil
		}
		msgs = append(msgs, ChatMessage{Role: "assistant", Content: turn.Content, ToolCalls: turn.ToolCalls})
		for _, tc := range turn.ToolCalls {
			e.publishEvent(StreamEvent{Type: "tool_start", ChannelID: ch.ID, Data: map[string]string{"name": tc.Function.Name}})
			outStr, execErr := e.executeTool(ctx, tc, tctx)
			if aprErr, ok := errors.AsType[*approvalRequiredError](execErr); ok {
				return e.pauseForApproval(ctx, ch, model, totalUsage, widgets, aprErr)
			}
			if execErr != nil {
				outStr = fmt.Sprintf(`{"error":%q}`, execErr.Error())
			} else {
				outStr = e.formatToolResultForLLM(ch.ID, tc.Function.Name, outStr)
			}
			e.publishEvent(StreamEvent{Type: "tool_done", ChannelID: ch.ID, Data: map[string]string{"name": tc.Function.Name}})
			msgs = append(msgs, ChatMessage{Role: "tool", ToolCallID: tc.ID, Content: outStr})
		}
	}
	if err := e.finishTurnSuccess(ctx, cfg, ch, model, "Stopped after reaching max tool rounds.", totalUsage, widgets, opts); err != nil {
		return err
	}
	e.recordTurnGrantUsage(cfg, ch.ID, totalUsage.CostUSD, route.UsedGrant != nil || opts.TrackGrant)
	return nil
}

func buildFrontierRetryWidget(prompt, frontierModel string, prob float64) WidgetPayload {
	return WidgetPayload{
		Kind:  "frontier_retry",
		Title: fmt.Sprintf("Borderline complexity (%.0f%%)", prob*100),
		Data: map[string]any{
			"prompt":         prompt,
			"frontier_model": frontierModel,
			"probability":    prob,
		},
	}
}

func (e *Engine) recordTurnGrantUsage(cfg Config, channelID string, costUSD float64, track bool) {
	if !track {
		return
	}
	exp := time.Now().UTC().Add(time.Duration(normalizeRoutingConfig(cfg.Routing).GrantIdleMinutes) * time.Minute).Format(time.RFC3339)
	if err := e.store.RecordFrontierGrantUsage(channelID, costUSD, exp); err == nil {
		e.publishEvent(StreamEvent{Type: "grant_updated", ChannelID: channelID})
	}
}

func (e *Engine) pauseForFrontierEscalation(ctx context.Context, ch Channel, esc *frontierEscalationArgs) error {
	rawArgs, err := json.Marshal(esc)
	if err != nil {
		return err
	}
	apr, err := e.store.CreateApproval(Approval{
		ChannelID: ch.ID,
		ToolName:  FrontierEscalationTool,
		ArgsJSON:  string(rawArgs),
		Summary:   fmt.Sprintf("Escalate to %s (%.0f%% complex)", esc.FrontierModel, esc.Probability*100),
		RiskLevel: "model",
	})
	if err != nil {
		return err
	}
	e.publishEvent(StreamEvent{Type: "approval", ChannelID: ch.ID, Data: apr})
	_ = e.sendApprovalToDiscord(ctx, e.Config(), ch, apr)
	return nil
}

func (e *Engine) finishTurnSuccess(ctx context.Context, cfg Config, ch Channel, model, content string, usage StreamUsage, widgets []WidgetPayload, opts turnOptions) error {
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
	e.mirrorTurnToDiscord(ctx, cfg, ch, opts, msg.Content, widgets)
	e.maybeAutoObserve(ctx, cfg, ch.ID)
	return nil
}

func (e *Engine) pauseForApproval(ctx context.Context, ch Channel, model string, usage StreamUsage, widgets []WidgetPayload, aprErr *approvalRequiredError) error {
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
	cfg := e.Config()
	sentInteractive := e.sendApprovalToDiscord(ctx, cfg, ch, apr)
	if !sentInteractive && cfg.Notifications.Enabled && (cfg.Notifications.NotifyApprovals == nil || *cfg.Notifications.NotifyApprovals) {
		e.spawnBG(ctx, 10*time.Second, func(nCtx context.Context) {
			_ = e.dispatchNotification(nCtx, Notification{
				Category:  "approval",
				ChannelID: ch.ID,
				Priority:  "urgent",
				Title:     "Approval needed: " + apr.ToolName,
				Summary:   apr.Summary,
				Fields: []NotificationField{
					{Name: "Channel", Value: "#" + ch.ID, Inline: true},
					{Name: "Risk", Value: apr.RiskLevel, Inline: true},
				},
			})
		})
	}
	return nil
}

var delegationSlugWordRE = regexp.MustCompile(`[a-z0-9]+`)

func slugifyDelegationName(text string) string {
	tokens := delegationSlugWordRE.FindAllString(strings.ToLower(text), -1)
	if len(tokens) == 0 {
		return ""
	}
	if len(tokens) > 5 {
		tokens = tokens[:5]
	}
	slug := strings.Join(tokens, "-")
	if len(slug) > 32 {
		slug = slug[:32]
	}
	return strings.TrimRight(slug, "-")
}

func delegationNameMatches(delegationName, heartbeatName string) bool {
	delClean := strings.TrimSpace(delegationName)
	hbClean := strings.TrimSpace(heartbeatName)
	if delClean == "" || hbClean == "" {
		return false
	}
	if strings.EqualFold(delClean, hbClean) {
		return true
	}
	delSlug := slugifyDelegationName(delClean)
	return delSlug != "" && delSlug == slugifyDelegationName(hbClean)
}

func (e *Engine) refreshDelegationsFlag() {
	active, err := e.store.ListActiveDelegations()
	e.delMu.Lock()
	e.hasActiveDelegations = err == nil && len(active) > 0
	e.delMu.Unlock()
}

// OnSlackerReceipt updates assistant delegation state when a Slacker spawn job launches or fails.
func (e *Engine) OnSlackerReceipt(jobID, device, sessionName, sessionID, status, errMsg string) {
	if e == nil || strings.TrimSpace(jobID) == "" {
		return
	}
	e.delMu.RLock()
	activeFlag := e.hasActiveDelegations
	e.delMu.RUnlock()
	if !activeFlag {
		return
	}
	dels, err := e.store.ListActiveDelegations()
	if err != nil {
		return
	}
	for _, d := range dels {
		matched := (d.JobID != "" && d.JobID == jobID) || (d.TargetDevice == device && delegationNameMatches(d.SessionName, sessionName))
		if !matched {
			continue
		}
		if status == "failed" {
			_ = e.store.UpdateDelegationState(d.ID, "", "failed", "failed")
			e.refreshDelegationsFlag()
			prompt := fmt.Sprintf("Delegated %s task %q on %s failed to launch: %s", d.Harness, d.SessionName, d.TargetDevice, cmp.Or(errMsg, "unknown error"))
			dedupe := fmt.Sprintf("del:%s:failed", d.ID)
			if _, ins, qErr := e.store.EnqueueInbox(d.ChannelID, "delegation", dedupe, map[string]any{"delegation_id": d.ID, "prompt": prompt}, ""); qErr == nil && ins {
				e.publishEvent(StreamEvent{Type: "delegation", ChannelID: d.ChannelID, Data: map[string]any{"id": d.ID, "status": "failed", "error": errMsg}})
				e.notify()
			}
			return
		}
		if status == "launched" {
			nextStatus := ""
			if d.Status == "queued" {
				nextStatus = "launched"
			}
			if nextStatus != "" || (sessionID != "" && d.SessionID == "") {
				_ = e.store.UpdateDelegationState(d.ID, sessionID, nextStatus, "")
				e.publishEvent(StreamEvent{Type: "delegation", ChannelID: d.ChannelID, Data: map[string]any{
					"id":         d.ID,
					"session_id": cmp.Or(sessionID, d.SessionID),
					"status":     cmp.Or(nextStatus, d.Status),
				}})
			}
		}
		return
	}
}

func shouldNotifyDelegationState(prevStatus, state string) bool {
	switch state {
	case "waiting_permission", "waiting_question":
		return true
	case "exited":
		return prevStatus != "idle"
	case "idle":
		return prevStatus == "queued" || prevStatus == "working" || prevStatus == "launched" || prevStatus == "waiting_permission" || prevStatus == "waiting_question"
	default:
		return false
	}
}

// OnAgentHeartbeat checks active delegations and wakes the assistant when a delegated agent blocks, idles, or exits.
func (e *Engine) OnAgentHeartbeat(device, harness, sessionID, name, state, summary, gitStatus string, pendingAction map[string]string) {
	if e == nil {
		return
	}
	e.delMu.RLock()
	activeFlag := e.hasActiveDelegations
	e.delMu.RUnlock()
	if !activeFlag {
		return
	}
	dels, err := e.store.ListActiveDelegations()
	if err != nil {
		return
	}
	for _, d := range dels {
		matched := (d.SessionID != "" && d.SessionID == sessionID) ||
			(d.TargetDevice == device && delegationNameMatches(d.SessionName, name))
		if !matched {
			continue
		}
		if !shouldNotifyDelegationState(d.Status, state) {
			if d.SessionID == "" || d.Status != state || (state == "working" && d.LastNotifiedState != "") {
				_ = e.store.UpdateDelegationState(d.ID, sessionID, state, "")
			}
			if state == "exited" {
				e.refreshDelegationsFlag()
			}
			continue
		}
		if d.LastNotifiedState == state {
			continue
		}
		e.notifyDelegationStateChange(d, device, harness, sessionID, name, state, summary, gitStatus, pendingAction)
	}
}

func (e *Engine) notifyDelegationStateChange(d Delegation, device, harness, sessionID, name, state, summary, gitStatus string, pendingAction map[string]string) {
	_ = e.store.UpdateDelegationState(d.ID, sessionID, state, state)
	if state == "exited" {
		e.refreshDelegationsFlag()
	}
	var details []string
	if summary != "" {
		details = append(details, "Summary: "+truncateText(summary, 300))
	}
	if len(pendingAction) > 0 {
		if rawPA, mErr := json.Marshal(pendingAction); mErr == nil {
			details = append(details, "Pending action: "+string(rawPA))
		}
	}
	if gitStatus != "" {
		details = append(details, "Git status: "+truncateText(gitStatus, 250))
	}
	prompt := fmt.Sprintf("Delegated %s session %s (%s on %s) transitioned to %s. Call `fleet_inspect_agent` to read the final output or blocker details, and give a concise update in #%s.",
		harness, cmp.Or(name, sessionID), sessionID, device, state, d.ChannelID)
	if len(details) > 0 {
		prompt += "\n" + strings.Join(details, "\n")
	}
	dedupe := fmt.Sprintf("del:%s:%s:%d", d.ID, state, time.Now().UTC().UnixNano())
	if _, ins, qErr := e.store.EnqueueInbox(d.ChannelID, "delegation", dedupe, map[string]any{"delegation_id": d.ID, "session_id": sessionID, "state": state, "prompt": prompt}, ""); qErr == nil && ins {
		e.publishEvent(StreamEvent{Type: "delegation", ChannelID: d.ChannelID, Data: map[string]any{"id": d.ID, "session_id": sessionID, "status": state}})
		e.notify()
	}
}

type agentSnapshot struct {
	SessionID     string            `json:"session_id"`
	Device        string            `json:"device"`
	Harness       string            `json:"harness"`
	Name          string            `json:"name"`
	State         string            `json:"state"`
	Live          *bool             `json:"live"`
	Summary       string            `json:"summary"`
	GitStatus     string            `json:"git_status"`
	PendingAction map[string]string `json:"pending_action"`
}

func (e *Engine) loadAgentSnapshots() ([]agentSnapshot, bool) {
	raw, err := e.deps.ListAgents("", "", "", "", true)
	if err != nil {
		return nil, false
	}
	if raw == nil {
		return nil, true
	}
	b, mErr := json.Marshal(raw)
	if mErr != nil {
		return nil, false
	}
	var snaps []agentSnapshot
	if uErr := json.Unmarshal(b, &snaps); uErr != nil {
		return nil, false
	}
	return snaps, true
}

func (e *Engine) reconcileActiveDelegations(now time.Time) {
	if e == nil || e.deps == nil {
		return
	}
	e.delMu.RLock()
	activeFlag := e.hasActiveDelegations
	e.delMu.RUnlock()
	if !activeFlag {
		return
	}
	dels, err := e.store.ListActiveDelegations()
	if err != nil {
		return
	}
	if len(dels) == 0 {
		e.refreshDelegationsFlag()
		return
	}
	snaps, ok := e.loadAgentSnapshots()
	if !ok {
		return
	}
	for _, d := range dels {
		e.reconcileOneDelegation(now, d, snaps)
	}
}

func findMatchingSnapshot(d Delegation, snaps []agentSnapshot) *agentSnapshot {
	for i := range snaps {
		s := &snaps[i]
		if (d.SessionID != "" && d.SessionID == s.SessionID) ||
			(d.TargetDevice == s.Device && delegationNameMatches(d.SessionName, s.Name)) {
			return s
		}
	}
	return nil
}

func (e *Engine) reconcileOneDelegation(now time.Time, d Delegation, snaps []agentSnapshot) {
	if s := findMatchingSnapshot(d, snaps); s != nil {
		effState := s.State
		if s.Live != nil && !*s.Live {
			effState = "exited"
		}
		needsSync := (d.SessionID == "" && s.SessionID != "") ||
			d.Status != effState ||
			(shouldNotifyDelegationState(d.Status, effState) && d.LastNotifiedState != effState)
		if effState != "" && needsSync {
			e.OnAgentHeartbeat(s.Device, s.Harness, s.SessionID, s.Name, effState, s.Summary, s.GitStatus, s.PendingAction)
		}
		return
	}
	e.reconcileMissingDelegation(now, d)
}

func (e *Engine) reconcileMissingDelegation(now time.Time, d Delegation) {
	if d.Status == "queued" && d.LastNotifiedState == "" {
		if createdAt, pErr := time.Parse(time.RFC3339, d.CreatedAt); pErr == nil && now.Sub(createdAt) >= 3*time.Minute {
			_ = e.store.UpdateDelegationState(d.ID, "", "", "queued_timeout")
			prompt := fmt.Sprintf("Delegated %s task %q on %s is still queued after 3 minutes (worker on %s may be offline or asleep). Check worker status or inform the user in #%s.",
				d.Harness, d.SessionName, d.TargetDevice, d.TargetDevice, d.ChannelID)
			dedupe := fmt.Sprintf("del:%s:queued_timeout", d.ID)
			if _, ins, qErr := e.store.EnqueueInbox(d.ChannelID, "delegation", dedupe, map[string]any{"delegation_id": d.ID, "status": "queued_timeout", "prompt": prompt}, ""); qErr == nil && ins {
				e.publishEvent(StreamEvent{Type: "delegation", ChannelID: d.ChannelID, Data: map[string]any{"id": d.ID, "status": "queued_timeout"}})
				e.notify()
			}
		}
		return
	}
	if d.Status == "idle" {
		if updatedAt, pErr := time.Parse(time.RFC3339, d.UpdatedAt); pErr == nil && now.Sub(updatedAt) >= 24*time.Hour {
			_ = e.store.UpdateDelegationState(d.ID, "", "exited", "")
			e.refreshDelegationsFlag()
		}
	}
}

func (e *Engine) activeNotifier(cfg NotificationsConfig) Notifier {
	e.delMu.RLock()
	custom := e.notifier
	e.delMu.RUnlock()
	if custom != nil {
		return custom
	}
	return NewDiscordWebhookNotifier(cfg, e.getOPReader(), nil)
}

func (e *Engine) dispatchNotification(ctx context.Context, n Notification) error {
	cfg := e.Config()
	if !cfg.Notifications.Enabled {
		return nil
	}
	return e.activeNotifier(cfg.Notifications).Send(ctx, n)
}

func (e *Engine) dispatchOutboxNotification(ctx context.Context, item OutboxItem) {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cfg := e.Config()
	if e.sendOutboxToDiscord(reqCtx, cfg, item) {
		_ = e.store.MarkOutboxDelivered(item.ID)
		return
	}
	if !cfg.Notifications.Enabled {
		return
	}
	if err := e.dispatchNotification(reqCtx, Notification{
		Category:  "outbox",
		ChannelID: item.ChannelID,
		Priority:  item.Priority,
		Title:     cmp.Or(item.Title, "Assistant Notification"),
		Summary:   item.Body,
	}); err == nil {
		_ = e.store.MarkOutboxDelivered(item.ID)
	}
}

// TestNotification verifies that configured Discord Webhook notifications resolve and deliver cleanly.
func (e *Engine) TestNotification(ctx context.Context) error {
	cfg := e.Config()
	return e.activeNotifier(cfg.Notifications).Send(ctx, Notification{
		Category:  "test",
		ChannelID: "general",
		Priority:  "normal",
		Title:     "Fleet Assistant Test Alert",
		Summary:   "Outbound-only Discord webhook notifications are connected and working.",
		Fields: []NotificationField{
			{Name: "Status", Value: "OK", Inline: true},
		},
	})
}
