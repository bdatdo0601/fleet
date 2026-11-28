package memory

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/registry"
)

const (
	DefaultOpenRouterURL   = "https://openrouter.ai/api/v1"
	DefaultOpenRouterModel = "openai/gpt-6-luna"
	DefaultOpenAIURL       = "https://api.openai.com/v1"
	DefaultOpenAIModel     = "gpt-5-nano"
	DefaultOllamaURL       = "http://127.0.0.1:11434/v1"
	DefaultOllamaModel     = "qwen2.5:3b"
)

// CuratorConfig persists the lighthouse memory curator LLM configuration.
type CuratorConfig struct {
	Enabled        bool   `json:"enabled"`
	Provider       string `json:"provider"`
	BaseURL        string `json:"base_url"`
	Model          string `json:"model"`
	APIKey         string `json:"api_key,omitempty"`
	ZDR            bool   `json:"zdr,omitempty"`
	DataCollection string `json:"data_collection,omitempty"`
	ReviewGlobal   bool   `json:"review_global,omitempty"`
	KeepAlive      string `json:"keep_alive,omitempty"`
	NumCtx         int    `json:"num_ctx,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"`
}

// CuratorStatus reports non-secret curator configuration, queue depths, and runtime health.
type CuratorStatus struct {
	Enabled          bool   `json:"enabled"`
	Configured       bool   `json:"configured"`
	Provider         string `json:"provider"`
	BaseURL          string `json:"base_url"`
	Model            string `json:"model"`
	APIKeyHint       string `json:"api_key_hint"`
	ZDR              bool   `json:"zdr"`
	DataCollection   string `json:"data_collection,omitempty"`
	AutoGlobal       bool   `json:"auto_global"`
	ReviewGlobal     bool   `json:"review_global"`
	KeepAlive        string `json:"keep_alive,omitempty"`
	NumCtx           int    `json:"num_ctx,omitempty"`
	PendingJobs      int    `json:"pending_jobs"`
	FailedJobs       int    `json:"failed_jobs"`
	PendingProposals int    `json:"pending_proposals"`
	LastRunAt        string `json:"last_run_at,omitempty"`
	LastError        string `json:"last_error,omitempty"`
	ProcessedCount   int    `json:"processed_count"`
}

// CuratorDecision represents the structured JSON decision returned by the curator model.
type CuratorDecision struct {
	Action    string `json:"action"`
	TargetKey string `json:"target_key"`
	Scope     string `json:"scope"`
	Kind      string `json:"kind"`
	Tier      string `json:"tier"`
	Summary   string `json:"summary"`
	Body      string `json:"body"`
	Reason    string `json:"reason"`
}

// CurateActionReport describes the outcome of curating one memory item.
type CurateActionReport struct {
	MemoryScope      string `json:"memory_scope"`
	MemoryKey        string `json:"memory_key"`
	Action           string `json:"action"`
	TargetScope      string `json:"target_scope,omitempty"`
	TargetKey        string `json:"target_key,omitempty"`
	QueuedProposalID int64  `json:"queued_proposal_id,omitempty"`
	Applied          bool   `json:"applied"`
	Reason           string `json:"reason,omitempty"`
	Error            string `json:"error,omitempty"`
}

var decisionJSONSchema = map[string]any{
	"type": "json_schema",
	"json_schema": map[string]any{
		"name":   "memory_curation_decision",
		"strict": true,
		"schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type": "string",
					"enum": []string{"keep", "update", "merge", "supersede", "archive", "promote_global"},
				},
				"target_key": map[string]any{"type": "string"},
				"scope":      map[string]any{"type": "string"},
				"kind": map[string]any{
					"type": "string",
					"enum": []string{"preference", "feedback", "project", "reference"},
				},
				"tier": map[string]any{
					"type": "string",
					"enum": []string{"always", "searchable"},
				},
				"summary": map[string]any{"type": "string"},
				"body":    map[string]any{"type": "string"},
				"reason":  map[string]any{"type": "string"},
			},
			"required":             []string{"action", "target_key", "scope", "kind", "tier", "summary", "body", "reason"},
			"additionalProperties": false,
		},
	},
}

// MaskAPIKey returns a redacted hint for an API key without exposing secret material.
func MaskAPIKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if len(key) >= 16 {
		return key[:8] + "..." + key[len(key)-4:]
	}
	return "***"
}

func inferProvider(baseURL, provider string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p != "" {
		return p
	}
	u := strings.ToLower(strings.TrimSpace(baseURL))
	switch {
	case strings.Contains(u, "openrouter.ai"):
		return "openrouter"
	case strings.Contains(u, "api.openai.com"):
		return "openai"
	case strings.Contains(u, ":11434"):
		return "ollama"
	case u != "":
		return "custom"
	default:
		return "openrouter"
	}
}

func normalizeCuratorConfig(cfg, prev CuratorConfig) (CuratorConfig, error) {
	cfg.Provider = inferProvider(cfg.BaseURL, cfg.Provider)
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.DataCollection = strings.ToLower(strings.TrimSpace(cfg.DataCollection))
	cfg.KeepAlive = strings.TrimSpace(cfg.KeepAlive)

	switch cfg.Provider {
	case "openrouter":
		cfg.BaseURL = cmp.Or(cfg.BaseURL, DefaultOpenRouterURL)
		cfg.Model = cmp.Or(cfg.Model, DefaultOpenRouterModel)
		cfg.DataCollection = cmp.Or(cfg.DataCollection, "deny")
	case "openai":
		cfg.BaseURL = cmp.Or(cfg.BaseURL, DefaultOpenAIURL)
		cfg.Model = cmp.Or(cfg.Model, DefaultOpenAIModel)
	case "ollama":
		cfg.BaseURL = cmp.Or(cfg.BaseURL, DefaultOllamaURL)
		cfg.Model = cmp.Or(cfg.Model, DefaultOllamaModel)
		cfg.KeepAlive = cmp.Or(cfg.KeepAlive, "2m")
		if cfg.NumCtx <= 0 {
			cfg.NumCtx = 2048
		}
	case "custom":
		if cfg.BaseURL == "" {
			return cfg, newMemoryErr("base_url is required for custom provider", 400)
		}
		if cfg.Model == "" {
			return cfg, newMemoryErr("model is required for custom provider", 400)
		}
	default:
		return cfg, newMemoryErr(fmt.Sprintf("unknown curator provider %q", cfg.Provider), 400)
	}

	if !strings.HasPrefix(cfg.BaseURL, "http://") && !strings.HasPrefix(cfg.BaseURL, "https://") {
		return cfg, newMemoryErr("base_url must start with http:// or https://", 400)
	}
	if cfg.APIKey == "" && prev.APIKey != "" && (prev.Provider == cfg.Provider || prev.BaseURL == cfg.BaseURL) {
		cfg.APIKey = prev.APIKey
	}
	if cfg.Enabled && (cfg.Provider == "openrouter" || cfg.Provider == "openai") && cfg.APIKey == "" {
		return cfg, newMemoryErr("api_key is required for "+cfg.Provider, 400)
	}
	cfg.UpdatedAt = registry.NowISO()
	return cfg, nil
}

// Curator manages async and on-demand LLM curation for memories on the lighthouse.
type Curator struct {
	store      *Store
	configPath string
	httpClient *http.Client

	mu        sync.Mutex
	drainMu   sync.Mutex
	cfg       CuratorConfig
	lastRunAt string
	lastError string
	processed int

	notifyCh chan struct{}
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	running  bool
}

// NewCurator constructs a Curator and loads configPath if it exists.
func NewCurator(store *Store, configPath string) *Curator {
	c := &Curator{
		store:      store,
		configPath: configPath,
		httpClient: &http.Client{Timeout: 45 * time.Second},
		notifyCh:   make(chan struct{}, 1),
	}
	if configPath != "" {
		if raw, err := os.ReadFile(configPath); err == nil && len(bytes.TrimSpace(raw)) > 0 {
			var loaded CuratorConfig
			if json.Unmarshal(raw, &loaded) == nil {
				c.cfg = loaded
			}
		}
	}
	return c
}

func (c *Curator) saveConfigLocked() error {
	if c.configPath == "" {
		return nil
	}
	data, err := json.MarshalIndent(c.cfg, "", "  ")
	if err != nil {
		return err
	}
	_, err = paths.WriteFileAtomic(c.configPath, append(data, '\n'), 0o600, nil, nil)
	return err
}

// Configure validates, optionally preflights, and persists curator configuration.
func (c *Curator) Configure(ctx context.Context, cfg CuratorConfig, testFirst bool) (CuratorStatus, error) {
	c.mu.Lock()
	prev := c.cfg
	c.mu.Unlock()

	norm, err := normalizeCuratorConfig(cfg, prev)
	if err != nil {
		return CuratorStatus{}, err
	}
	if norm.Enabled && testFirst {
		if err := c.TestConnection(ctx, norm); err != nil {
			return CuratorStatus{}, err
		}
	}

	c.mu.Lock()
	c.cfg = norm
	c.lastError = ""
	saveErr := c.saveConfigLocked()
	c.mu.Unlock()
	if saveErr != nil {
		return CuratorStatus{}, saveErr
	}
	if norm.Enabled {
		c.Notify()
	}
	return c.Status(), nil
}

// Disable turns off background curation while preserving the non-secret endpoint metadata.
func (c *Curator) Disable() (CuratorStatus, error) {
	c.mu.Lock()
	c.cfg.Enabled = false
	c.cfg.UpdatedAt = registry.NowISO()
	saveErr := c.saveConfigLocked()
	c.mu.Unlock()
	if saveErr != nil {
		return CuratorStatus{}, saveErr
	}
	return c.Status(), nil
}

// Status returns the current non-secret curator configuration and queue metrics.
func (c *Curator) Status() CuratorStatus {
	c.mu.Lock()
	cfg := c.cfg
	lastRun := c.lastRunAt
	lastErr := c.lastError
	proc := c.processed
	c.mu.Unlock()

	var pending, failed, proposals int
	if c.store != nil {
		pending, failed, proposals, _ = c.store.JobCounts()
	}
	return CuratorStatus{
		Enabled:          cfg.Enabled,
		Configured:       cfg.BaseURL != "" && cfg.Model != "",
		Provider:         cfg.Provider,
		BaseURL:          cfg.BaseURL,
		Model:            cfg.Model,
		APIKeyHint:       MaskAPIKey(cfg.APIKey),
		ZDR:              cfg.ZDR,
		DataCollection:   cfg.DataCollection,
		AutoGlobal:       !cfg.ReviewGlobal,
		ReviewGlobal:     cfg.ReviewGlobal,
		KeepAlive:        cfg.KeepAlive,
		NumCtx:           cfg.NumCtx,
		PendingJobs:      pending,
		FailedJobs:       failed,
		PendingProposals: proposals,
		LastRunAt:        lastRun,
		LastError:        lastErr,
		ProcessedCount:   proc,
	}
}

// Start launches the background curation worker goroutine if not already running.
func (c *Curator) Start(parent context.Context) {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	c.cancel = cancel
	c.running = true
	c.wg.Add(1)
	c.mu.Unlock()

	go c.loop(ctx)
	c.Notify()
}

// Stop signals the background worker to exit and waits for completion.
func (c *Curator) Stop() {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return
	}
	cancel := c.cancel
	c.cancel = nil
	c.running = false
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	c.wg.Wait()
}

// Notify wakes the background worker to process queued curation jobs.
func (c *Curator) Notify() {
	select {
	case c.notifyCh <- struct{}{}:
	default:
	}
}

func (c *Curator) loop(ctx context.Context) {
	defer c.wg.Done()
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.notifyCh:
			// Debounce bursts of incoming memory writes or sync batches.
			select {
			case <-ctx.Done():
				return
			case <-time.After(1500 * time.Millisecond):
			}
			c.drainQueue(ctx, 20)
		case <-ticker.C:
			c.drainQueue(ctx, 10)
		}
	}
}

func isOpenRouterConfig(cfg CuratorConfig) bool {
	return cfg.Provider == "openrouter" || strings.Contains(strings.ToLower(cfg.BaseURL), "openrouter.ai")
}

func isOllamaConfig(cfg CuratorConfig) bool {
	return cfg.Provider == "ollama" || strings.Contains(strings.ToLower(cfg.BaseURL), ":11434")
}

func buildChatPayload(cfg CuratorConfig, messages []map[string]string, responseFormat any, maxTokens int) map[string]any {
	payload := map[string]any{
		"model":       cfg.Model,
		"messages":    messages,
		"temperature": 0,
	}
	if maxTokens > 0 {
		payload["max_tokens"] = maxTokens
	}
	if responseFormat != nil {
		payload["response_format"] = responseFormat
	}
	if isOpenRouterConfig(cfg) {
		prov := map[string]any{
			"data_collection": cmp.Or(cfg.DataCollection, "deny"),
		}
		if _, isSchema := responseFormat.(map[string]any)["json_schema"]; isSchema {
			prov["require_parameters"] = true
		}
		if cfg.ZDR {
			prov["zdr"] = true
		}
		payload["provider"] = prov
	}
	if isOllamaConfig(cfg) {
		if cfg.KeepAlive != "" {
			payload["keep_alive"] = cfg.KeepAlive
		}
		if cfg.NumCtx > 0 {
			payload["options"] = map[string]any{"num_ctx": cfg.NumCtx}
		}
	}
	return payload
}

const decisionMaxTokens = 4096

type chatResult struct {
	content      string
	finishReason string
	status       int
	rawBody      string
}

func (c *Curator) doChatRequest(ctx context.Context, cfg CuratorConfig, payload map[string]any) (chatResult, error) {
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return chatResult{}, err
	}
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return chatResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	if isOpenRouterConfig(cfg) {
		req.Header.Set("HTTP-Referer", "https://github.com/bdatdo0601/fleet")
		req.Header.Set("X-Title", "fleet-memory-curator")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return chatResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return chatResult{status: resp.StatusCode}, err
	}
	raw := string(respBytes)
	if resp.StatusCode >= 400 {
		return chatResult{status: resp.StatusCode, rawBody: raw}, fmt.Errorf("curator HTTP %d: %s", resp.StatusCode, TruncateRunes(strings.TrimSpace(raw), 240))
	}

	var parsed struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return chatResult{status: resp.StatusCode, rawBody: raw}, fmt.Errorf("decode curator response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return chatResult{status: resp.StatusCode, rawBody: raw}, errors.New("curator returned no choices")
	}
	return chatResult{
		content:      parsed.Choices[0].Message.Content,
		finishReason: parsed.Choices[0].FinishReason,
		status:       resp.StatusCode,
		rawBody:      raw,
	}, nil
}

// TestConnection verifies credentials, endpoint reachability, and model JSON support.
func (c *Curator) TestConnection(ctx context.Context, cfg CuratorConfig) error {
	msgs := []map[string]string{
		{"role": "system", "content": "Return valid json only."},
		{"role": "user", "content": `Reply with valid json: {"ok":true}`},
	}
	payload := buildChatPayload(cfg, msgs, map[string]any{"type": "json_object"}, 128)
	res, err := c.doChatRequest(ctx, cfg, payload)
	if err != nil && (res.status == 400 || res.status == 404) && strings.Contains(strings.ToLower(res.rawBody), "response_format") {
		payloadNoRF := buildChatPayload(cfg, msgs, nil, 128)
		_, err = c.doChatRequest(ctx, cfg, payloadNoRF)
	}
	if err != nil {
		return newMemoryErr("curator preflight failed: "+err.Error(), 400)
	}
	return nil
}

func stripCodeFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if idx := strings.IndexByte(s, '\n'); idx >= 0 {
			s = s[idx+1:]
		}
		if end := strings.LastIndex(s, "```"); end >= 0 {
			s = s[:end]
		}
	}
	return strings.TrimSpace(s)
}

func (c *Curator) requestDecision(ctx context.Context, cfg CuratorConfig, msgs []map[string]string) (chatResult, error) {
	payload := buildChatPayload(cfg, msgs, decisionJSONSchema, decisionMaxTokens)
	res, err := c.doChatRequest(ctx, cfg, payload)
	if err != nil && (res.status == 400 || res.status == 404) {
		low := strings.ToLower(res.rawBody)
		if strings.Contains(low, "response_format") || strings.Contains(low, "json_schema") || strings.Contains(low, "provider") || strings.Contains(low, "parameters") {
			fallbackPayload := buildChatPayload(cfg, msgs, map[string]any{"type": "json_object"}, decisionMaxTokens)
			return c.doChatRequest(ctx, cfg, fallbackPayload)
		}
	}
	return res, err
}

func (c *Curator) callDecisionLLM(ctx context.Context, cfg CuratorConfig, prompt string) (CuratorDecision, error) {
	sys := `You curate a fleet-wide engineering memory store shared by multiple coding agents.
Decide how to handle the CANDIDATE memory relative to EXISTING matches and STANDING global rules:
- "archive": ONLY if candidate is clearly ephemeral task progress, branch scratch, or zero reusable value. Never archive durable technical project/reference notes.
- "merge": candidate duplicates or extends an existing memory in the SAME scope -> set target_key to that existing key and merge all nuances into summary and body.
- "supersede": candidate directly replaces/contradicts an obsolete memory in the SAME scope -> set target_key to that obsolete key.
- "promote_global": candidate is non-global preference/feedback expressing a cross-repo workflow rule -> set scope="global", tier="always", target_key=short slug, summary, body.
- "update": keep in current scope, optionally refining concise summary (<=200 chars) or kind/tier; preserve detailed body for project/reference notes.
- "keep": already clean as-is.
For "keep", "archive", or "update" when the existing body does not need rewriting, set "body": "" (the server preserves the existing body). Keep "body" concise when actually rewriting, merging, or generalizing for "promote_global".
Return valid JSON matching the schema.`

	msgs := []map[string]string{
		{"role": "system", "content": sys},
		{"role": "user", "content": prompt},
	}
	res, err := c.requestDecision(ctx, cfg, msgs)
	if err == nil && (res.finishReason == "length" || strings.TrimSpace(res.content) == "") {
		msgs = append(msgs, map[string]string{
			"role":    "user",
			"content": `Previous output hit length limit; return compact valid JSON with body="" unless rewriting.`,
		})
		res, err = c.requestDecision(ctx, cfg, msgs)
	}
	if err != nil {
		return CuratorDecision{}, err
	}
	if strings.TrimSpace(res.content) == "" {
		return CuratorDecision{}, fmt.Errorf("curator returned empty response (finish_reason=%s)", cmp.Or(res.finishReason, "unknown"))
	}

	var dec CuratorDecision
	cleaned := stripCodeFences(res.content)
	if err := json.Unmarshal([]byte(cleaned), &dec); err != nil {
		return CuratorDecision{}, fmt.Errorf("parse curator json (finish_reason=%s): %w", cmp.Or(res.finishReason, "unknown"), err)
	}
	dec.Action = cmp.Or(strings.ToLower(strings.TrimSpace(dec.Action)), "keep")
	return dec, nil
}

func (c *Curator) buildCandidatePrompt(cand Item) (string, error) {
	seen := map[int64]bool{cand.ID: true}
	var related []Item

	if cand.Summary != "" || cand.Key != "" {
		matches, err := c.store.Search(SearchFilter{
			Query: cand.Key + " " + cand.Summary,
			Scope: cand.Scope,
			Limit: 6,
		})
		if err != nil {
			return "", err
		}
		for _, m := range matches {
			if !seen[m.ID] && len(related) < 5 {
				seen[m.ID] = true
				related = append(related, m)
			}
		}
	}
	if cand.Scope != "global" && len(related) < 5 {
		gMatches, err := c.store.Search(SearchFilter{
			Query: cand.Key + " " + cand.Summary,
			Scope: "global",
			Limit: 3,
		})
		if err == nil {
			for _, m := range gMatches {
				if !seen[m.ID] && len(related) < 5 {
					seen[m.ID] = true
					related = append(related, m)
				}
			}
		}
	}
	alwaysList, _ := c.store.List(SearchFilter{Tier: "always", Limit: 15})

	var b strings.Builder
	fmt.Fprintf(&b, "CANDIDATE:\nscope: %s\nkey: %s\nkind: %s\ntier: %s\nsummary: %s\nbody:\n%s\n",
		cand.Scope, cand.Key, cand.Kind, cand.Tier, cand.Summary, TruncateRunes(cand.Body, 4000))

	if len(related) > 0 {
		b.WriteString("\nSIMILAR EXISTING MEMORIES:\n")
		for _, r := range related {
			fmt.Fprintf(&b, "- %s/%s (%s, %s, source=%s): %s\n  body: %s\n",
				r.Scope, r.Key, r.Kind, r.Tier, r.Source, r.Summary, TruncateRunes(r.Body, 500))
		}
	}
	if len(alwaysList) > 0 {
		b.WriteString("\nSTANDING ALWAYS MEMORIES:\n")
		for _, a := range alwaysList {
			if a.ID == cand.ID {
				continue
			}
			fmt.Fprintf(&b, "- %s/%s: %s\n", a.Scope, a.Key, a.Summary)
		}
	}
	return b.String(), nil
}

func (c *Curator) queueProposal(cand Item, action, targetScope, targetKey, kind, tier, summary, body, reason, model string) (CurateActionReport, error) {
	prop, err := c.store.CreateProposal(ProposalParams{
		Action:         action,
		SourceMemoryID: cand.ID,
		TargetScope:    targetScope,
		TargetKey:      targetKey,
		Kind:           kind,
		Tier:           tier,
		Summary:        summary,
		Body:           body,
		Reason:         reason,
		Model:          model,
	})
	if err != nil {
		return CurateActionReport{}, err
	}
	return CurateActionReport{
		MemoryScope:      cand.Scope,
		MemoryKey:        cand.Key,
		Action:           action,
		TargetScope:      prop.TargetScope,
		TargetKey:        prop.TargetKey,
		QueuedProposalID: prop.ID,
		Applied:          false,
		Reason:           reason,
	}, nil
}

func isProtectedTarget(it *Item) bool {
	return it != nil && (it.Locked || it.Source == "human")
}

func normalizeDecisionFields(cand Item, dec CuratorDecision) (targetScope, targetKey, kind, tier, summary, body string) {
	targetScope = cand.Scope
	if ns, err := NormalizeScope(dec.Scope); err == nil {
		targetScope = ns
	}
	targetKey = cand.Key
	if nk, err := NormalizeKey(dec.TargetKey); err == nil {
		targetKey = nk
	}
	kind = cand.Kind
	if nk, err := NormalizeKind(dec.Kind); err == nil && dec.Kind != "" {
		kind = nk
	}
	tier = cand.Tier
	if nt, err := NormalizeTier(dec.Tier, targetScope, kind); err == nil && dec.Tier != "" {
		tier = nt
	}
	summary = cmp.Or(strings.TrimSpace(dec.Summary), cand.Summary)
	body = cmp.Or(strings.TrimSpace(dec.Body), cand.Body)
	return targetScope, targetKey, kind, tier, summary, body
}

func (c *Curator) applyPromoteGlobal(cand Item, cfg CuratorConfig, dec CuratorDecision, targetKey, kind, summary, body string) (CurateActionReport, error) {
	if cfg.ReviewGlobal {
		return c.queueProposal(cand, "promote_global", "global", targetKey, kind, "always", summary, body, cmp.Or(dec.Reason, "cross-fleet preference candidate"), cfg.Model)
	}
	res, err := c.store.ApplyCuratorPromoteGlobal(cand.ID, cand.Revision, targetKey, kind, summary, body, dec.Reason)
	if err != nil {
		return CurateActionReport{}, err
	}
	if res.Skipped || res.Stale {
		return CurateActionReport{
			MemoryScope: cand.Scope, MemoryKey: cand.Key, Action: "keep", Applied: false, Reason: res.Reason,
		}, nil
	}
	if res.OptedOut {
		return CurateActionReport{
			MemoryScope: cand.Scope, MemoryKey: cand.Key, Action: "keep",
			TargetScope: "global", TargetKey: targetKey, Applied: false, Reason: res.Reason,
		}, nil
	}
	if res.Protected {
		return c.queueProposal(cand, "promote_global", "global", targetKey, kind, "always", summary, body, res.Reason, cfg.Model)
	}
	return CurateActionReport{
		MemoryScope: cand.Scope, MemoryKey: cand.Key, Action: "promote_global",
		TargetScope: res.Target.Scope, TargetKey: res.Target.Key, Applied: true, Reason: dec.Reason,
	}, nil
}

func (c *Curator) applyMergeOrSupersede(cand Item, cfg CuratorConfig, dec CuratorDecision, targetKey, kind, tier, summary, body string) (CurateActionReport, error) {
	if targetKey == "" || targetKey == cand.Key {
		return c.applyUpdate(cand, cfg, dec, cand.Kind, cand.Tier, "active", summary, body)
	}

	var res CurateMutationResult
	var err error
	if dec.Action == "merge" {
		res, err = c.store.ApplyCuratorMerge(cand.ID, cand.Revision, cand.Scope, targetKey, summary, body, dec.Reason)
	} else {
		res, err = c.store.ApplyCuratorSupersede(cand.ID, cand.Revision, cand.Scope, targetKey, kind, tier, summary, body, dec.Reason)
	}
	if err != nil {
		return CurateActionReport{}, err
	}
	if res.TargetMissing {
		return c.applyUpdate(cand, cfg, dec, kind, tier, "active", summary, body)
	}
	if res.Skipped || res.Stale {
		return CurateActionReport{
			MemoryScope: cand.Scope, MemoryKey: cand.Key, Action: "keep", Applied: false, Reason: res.Reason,
		}, nil
	}
	if res.Protected {
		if res.Target != nil {
			return c.queueProposal(cand, dec.Action, res.Target.Scope, res.Target.Key, res.Target.Kind, res.Target.Tier, summary, body, res.Reason, cfg.Model)
		}
		return c.queueProposal(cand, dec.Action, cand.Scope, cand.Key, kind, tier, summary, body, res.Reason, cfg.Model)
	}
	return CurateActionReport{
		MemoryScope: cand.Scope, MemoryKey: cand.Key, Action: dec.Action,
		TargetScope: res.Target.Scope, TargetKey: res.Target.Key, Applied: true, Reason: dec.Reason,
	}, nil
}

func (c *Curator) applyUpdate(cand Item, cfg CuratorConfig, dec CuratorDecision, kind, tier, status, summary, body string) (CurateActionReport, error) {
	action := "update"
	reason := cmp.Or(dec.Reason, "curator_update")
	if status == "archived" {
		action = "archive"
		reason = cmp.Or(dec.Reason, "curator_archive")
	}
	res, err := c.store.ApplyCuratorUpdate(cand.ID, cand.Revision, kind, tier, status, summary, body, reason)
	if err != nil {
		return CurateActionReport{}, err
	}
	if res.Skipped || res.Stale {
		return CurateActionReport{
			MemoryScope: cand.Scope, MemoryKey: cand.Key, Action: "keep", Applied: false, Reason: res.Reason,
		}, nil
	}
	if res.Protected {
		return c.queueProposal(cand, action, cand.Scope, cand.Key, kind, tier, summary, body, res.Reason, cfg.Model)
	}
	return CurateActionReport{
		MemoryScope: cand.Scope, MemoryKey: cand.Key, Action: action,
		Applied: res.Applied, Reason: dec.Reason,
	}, nil
}

func (c *Curator) applyDecision(cand Item, cfg CuratorConfig, dec CuratorDecision) (CurateActionReport, error) {
	targetScope, targetKey, kind, tier, summary, body := normalizeDecisionFields(cand, dec)

	if dec.Action == "keep" {
		if cand.Summary == "" && summary != "" && !isProtectedTarget(&cand) {
			return c.applyUpdate(cand, cfg, dec, cand.Kind, cand.Tier, "active", summary, cand.Body)
		}
		return CurateActionReport{MemoryScope: cand.Scope, MemoryKey: cand.Key, Action: "keep", Reason: dec.Reason}, nil
	}

	if isProtectedTarget(&cand) {
		propScope := cand.Scope
		propKey := cand.Key
		if dec.Action == "promote_global" {
			propScope = "global"
			propKey = targetKey
		}
		return c.queueProposal(cand, dec.Action, propScope, propKey, kind, tier, summary, body, "source entry is locked/human-edited", cfg.Model)
	}

	if dec.Action == "promote_global" || (targetScope == "global" && cand.Scope != "global") {
		return c.applyPromoteGlobal(cand, cfg, dec, targetKey, kind, summary, body)
	}

	switch dec.Action {
	case "archive":
		return c.applyUpdate(cand, cfg, dec, cand.Kind, cand.Tier, "archived", cand.Summary, cand.Body)
	case "merge", "supersede":
		return c.applyMergeOrSupersede(cand, cfg, dec, targetKey, kind, tier, summary, body)
	default:
		return c.applyUpdate(cand, cfg, dec, kind, tier, "active", summary, body)
	}
}

func (c *Curator) recordRunStats(processed int, lastErr string) {
	c.mu.Lock()
	c.lastRunAt = registry.NowISO()
	c.lastError = lastErr
	c.processed += processed
	c.mu.Unlock()
}

func (c *Curator) currentConfig() CuratorConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg
}

func (c *Curator) drainQueue(ctx context.Context, limit int) {
	c.drainMu.Lock()
	defer c.drainMu.Unlock()
	_, _ = c.drainQueueLocked(ctx, limit)
}

func (c *Curator) drainQueueLocked(ctx context.Context, limit int) ([]CurateActionReport, error) {
	cfg := c.currentConfig()
	if !cfg.Enabled || cfg.BaseURL == "" || cfg.Model == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	var reports []CurateActionReport
	processed := 0
	var lastErr string

	for range limit {
		if ctx.Err() != nil {
			break
		}
		job, err := c.store.ClaimNextJob()
		if err != nil {
			lastErr = err.Error()
			break
		}
		if job == nil {
			break
		}
		cand, err := c.store.GetByID(job.MemoryID)
		if err != nil {
			_ = c.store.FailJob(job.ID, err.Error())
			lastErr = err.Error()
			continue
		}
		if cand == nil || cand.Status != "active" {
			_ = c.store.CompleteJob(job.ID)
			continue
		}
		prompt, err := c.buildCandidatePrompt(*cand)
		if err != nil {
			_ = c.store.FailJob(job.ID, err.Error())
			lastErr = err.Error()
			continue
		}
		dec, err := c.callDecisionLLM(ctx, cfg, prompt)
		if err != nil {
			_ = c.store.FailJob(job.ID, err.Error())
			lastErr = err.Error()
			reports = append(reports, CurateActionReport{MemoryScope: cand.Scope, MemoryKey: cand.Key, Action: "error", Error: err.Error()})
			continue
		}
		rep, err := c.applyDecision(*cand, cfg, dec)
		if err != nil {
			_ = c.store.FailJob(job.ID, err.Error())
			lastErr = err.Error()
			reports = append(reports, CurateActionReport{MemoryScope: cand.Scope, MemoryKey: cand.Key, Action: "error", Error: err.Error()})
			continue
		}
		_ = c.store.CompleteJob(job.ID)
		processed++
		reports = append(reports, rep)
	}
	if processed > 0 || lastErr != "" {
		c.recordRunStats(processed, lastErr)
	}
	return reports, nil
}

// RunPending executes curation on queued jobs (or previews active items if dryRun is true).
func (c *Curator) RunPending(ctx context.Context, limit int, dryRun, includeAll bool) ([]CurateActionReport, error) {
	cfg := c.currentConfig()
	if !cfg.Enabled || cfg.BaseURL == "" || cfg.Model == "" {
		return nil, newMemoryErr("curator is not configured or is disabled (run `fleet memory curator setup`)", 400)
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	if dryRun {
		return c.dryRunEvaluate(ctx, cfg, limit, includeAll)
	}
	c.drainMu.Lock()
	defer c.drainMu.Unlock()
	_, _ = c.store.ResetFailedJobs()
	if includeAll {
		if _, err := c.store.EnqueueAllActive([]string{"preference", "feedback"}); err != nil {
			return nil, err
		}
	}
	return c.drainQueueLocked(ctx, limit)
}

func (c *Curator) dryRunEvaluate(ctx context.Context, cfg CuratorConfig, limit int, includeAll bool) ([]CurateActionReport, error) {
	var cands []Item
	var err error
	if includeAll {
		cands, err = c.store.List(SearchFilter{Kind: "feedback", Limit: limit})
		if err == nil && len(cands) < limit {
			prefs, pErr := c.store.List(SearchFilter{Kind: "preference", Limit: limit - len(cands)})
			if pErr == nil {
				cands = append(cands, prefs...)
			}
		}
	} else {
		// Evaluate active items whose IDs are currently in curation_jobs without claiming them.
		cands, err = c.store.QueuedCandidates(limit)
	}
	if err != nil {
		return nil, err
	}
	reports := make([]CurateActionReport, 0, len(cands))
	for _, cand := range cands {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return reports, ctxErr
		}
		prompt, pErr := c.buildCandidatePrompt(cand)
		if pErr != nil {
			return reports, pErr
		}
		dec, llmErr := c.callDecisionLLM(ctx, cfg, prompt)
		if llmErr != nil {
			reports = append(reports, CurateActionReport{MemoryScope: cand.Scope, MemoryKey: cand.Key, Action: "error", Error: llmErr.Error()})
			continue
		}
		ts, tk, _, _, _, _ := normalizeDecisionFields(cand, dec)
		if dec.Action == "promote_global" {
			ts = "global"
		}
		reports = append(reports, CurateActionReport{
			MemoryScope: cand.Scope,
			MemoryKey:   cand.Key,
			Action:      dec.Action,
			TargetScope: ts,
			TargetKey:   tk,
			Applied:     false,
			Reason:      dec.Reason,
		})
	}
	return reports, nil
}
