package assistant

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/memory"
)

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
	e.discordMu.RLock()
	discConnected := e.discordConnected
	e.discordMu.RUnlock()
	return map[string]any{
		"config":            e.ConfigView(),
		"summary":           e.store.Summary(),
		"discord_connected": discConnected,
	}
}

func isZeroNotificationsConfig(n NotificationsConfig) bool {
	return n.NotifyApprovals == nil && !n.Enabled && n.DiscordWebhookOPRef == "" &&
		len(n.DiscordChannelOPRefs) == 0 && n.FleetBaseURL == "" && n.MinPriority == ""
}

func isZeroDiscordConfig(d DiscordConfig) bool {
	return d.AllowApprovals == nil && d.MirrorWebTurns == nil && d.AutoSyncChannels == nil &&
		!d.Enabled && !d.AutoApprove && d.BotTokenOPRef == "" && d.AuthorizedUserID == "" &&
		d.GuildID == "" && d.CategoryName == "" && len(d.Channels) == 0 && d.FleetBaseURL == ""
}

func isZeroSandboxConfig(s SandboxConfig) bool {
	return !s.Enabled && s.Image == "" && s.BaseImage == "" && s.Network == "" &&
		s.CPUs == 0 && s.MemoryMB == 0 && s.IdleMinutes == 0 && s.MaxFileMB == 0 &&
		s.MaxActive == 0 && len(s.Profiles) == 0 &&
		len(s.ExtraAptPackages) == 0 && len(s.ExtraPipPackages) == 0
}

func isZeroLocalLLMConfig(l LocalLLMConfig) bool {
	return !l.Enabled && l.BaseURL == "" && l.Model == "" && l.KeepAlive == "" && l.NumCtx == 0
}

func isZeroEmbedConfig(e memory.EmbedConfig) bool {
	return !e.Enabled && e.Provider == "" && e.BaseURL == "" && e.Model == "" && e.APIKey == "" && e.KeepAlive == ""
}

func isZeroTranscribeConfig(t TranscribeConfig) bool {
	return !t.Enabled && t.Command == "" && t.ModelPath == "" && t.Language == "" && t.AutoTranscribe == nil
}

func mergePreservedConfig(cur, next Config, clearAPIKey bool) Config {
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
	if isZeroNotificationsConfig(next.Notifications) {
		next.Notifications = cur.Notifications
	}
	if next.Routing == (RoutingConfig{}) {
		next.Routing = cur.Routing
	}
	if isZeroLocalLLMConfig(next.LocalLLM) {
		next.LocalLLM = cur.LocalLLM
	}
	if isZeroEmbedConfig(next.Embeddings) {
		next.Embeddings = cur.Embeddings
	} else if cur.Embeddings.APIKey != "" && (next.Embeddings.APIKey == "" || next.Embeddings.APIKey == maskSecret(cur.Embeddings.APIKey)) {
		next.Embeddings.APIKey = cur.Embeddings.APIKey
	}
	if isZeroTranscribeConfig(next.Transcribe) {
		next.Transcribe = cur.Transcribe
	}
	if isZeroSandboxConfig(next.Sandbox) {
		next.Sandbox = cur.Sandbox
	}
	if isZeroDiscordConfig(next.Discord) {
		next.Discord = cur.Discord
	}
	return NormalizeConfig(next)
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
		if ok && len(srv.Headers) > 0 && len(ps.Headers) > 0 {
			restoredH := make(map[string]string, len(srv.Headers))
			for k, v := range srv.Headers {
				if oldVal, exists := ps.Headers[k]; exists && v == maskSecret(oldVal) {
					restoredH[k] = oldVal
				} else {
					restoredH[k] = v
				}
			}
			srv.Headers = restoredH
		}
		if ok && srv.OAuth != nil && ps.OAuth != nil && srv.OAuth.ClientSecret != "" && srv.OAuth.ClientSecret == maskSecret(ps.OAuth.ClientSecret) {
			cp := copyMCPOAuthConfig(srv.OAuth)
			cp.ClientSecret = ps.OAuth.ClientSecret
			srv.OAuth = cp
		}
		out = append(out, srv)
	}
	return out
}

// UpdateConfig validates, saves, and applies new assistant configuration.
func (e *Engine) UpdateConfig(next Config, clearAPIKey bool) (ConfigView, error) {
	e.configWriteMu.Lock()
	defer e.configWriteMu.Unlock()
	return e.updateConfigLocked(next, clearAPIKey)
}

func (e *Engine) updateConfigLocked(next Config, clearAPIKey bool) (ConfigView, error) {
	next = mergePreservedConfig(e.Config(), next, clearAPIKey)
	if err := validateSandboxPackages(next.Sandbox); err != nil {
		return ConfigView{}, err
	}
	if err := ValidateLocalLLMConfig(next.LocalLLM); err != nil {
		return ConfigView{}, err
	}
	if next.Embeddings.Enabled {
		if _, err := memory.NormalizeEmbedConfig(next.Embeddings, memory.EmbedConfig{}); err != nil {
			return ConfigView{}, newErr(err.Error(), http.StatusBadRequest)
		}
	}
	if err := ValidateDiscordConfig(next.Discord); err != nil {
		return ConfigView{}, err
	}
	for _, srv := range next.MCPServers {
		if err := ValidateMCPServerConfig(srv, false); err != nil {
			return ConfigView{}, err
		}
	}
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
	e.notifyDiscordReload()
	e.notify()
	view := next.View(vaultRoot)
	e.publishEvent(StreamEvent{
		Type: "config_updated",
		Data: map[string]any{
			"auto_approve":         view.AutoApprove,
			"discord_auto_approve": view.Discord.AutoApprove,
		},
	})
	return view, nil
}

// SetAutoApprove updates global and/or Discord HITL bypass flags without reloading the Discord Gateway socket.
func (e *Engine) SetAutoApprove(global, discord *bool) (ConfigView, error) {
	e.configWriteMu.Lock()
	defer e.configWriteMu.Unlock()

	e.cfgMu.Lock()
	next := e.cfg
	if global != nil {
		next.AutoApprove = *global
	}
	if discord != nil {
		next.Discord.AutoApprove = *discord
	}
	next = NormalizeConfig(next)
	if err := SaveConfig(e.configPath, next); err != nil {
		e.cfgMu.Unlock()
		return ConfigView{}, err
	}
	e.cfg = next
	e.cfgMu.Unlock()

	view := next.View(ResolveVaultRoot(next.VaultPath, e.vaultDir))
	e.publishEvent(StreamEvent{
		Type: "config_updated",
		Data: map[string]any{
			"auto_approve":         view.AutoApprove,
			"discord_auto_approve": view.Discord.AutoApprove,
		},
	})
	return view, nil
}

func (e *Engine) persistDiscordMapping(guildID string, channels map[string]string) error {
	e.configWriteMu.Lock()
	defer e.configWriteMu.Unlock()

	e.cfgMu.Lock()
	defer e.cfgMu.Unlock()
	next := e.cfg
	if guildID != "" {
		next.Discord.GuildID = guildID
	}
	next.Discord.Channels = maps.Clone(channels)
	next = NormalizeConfig(next)
	if err := SaveConfig(e.configPath, next); err != nil {
		return err
	}
	e.cfg = next
	return nil
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
	reader := e.getOPReader()
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

// SetOPReader overrides 1Password resolution (used by tests or custom brokers).
func (e *Engine) SetOPReader(fn OPReaderFunc) {
	e.opMu.Lock()
	e.opReader = fn
	e.opMu.Unlock()
	if e.mcp != nil {
		e.mcp.SetOPReader(fn)
	}
}

func (e *Engine) getOPReader() OPReaderFunc {
	e.opMu.RLock()
	fn := e.opReader
	e.opMu.RUnlock()
	return fn
}

func (e *Engine) findMCPServer(name string) (MCPServerConfig, error) {
	clean := strings.ToLower(strings.TrimSpace(name))
	if clean == "" {
		return MCPServerConfig{}, newErr("MCP server name is required", http.StatusBadRequest)
	}
	for _, srv := range e.Config().MCPServers {
		if strings.EqualFold(srv.Name, clean) {
			return NormalizeMCPServerConfig(srv), nil
		}
	}
	return MCPServerConfig{}, newErr(fmt.Sprintf("MCP server %q not found", clean), http.StatusNotFound)
}

// ListMCPStatuses returns redacted MCP server configs alongside live connection/OAuth status.
func (e *Engine) ListMCPStatuses(ctx context.Context) ([]MCPServerConfig, []MCPServerStatus) {
	cfg := e.Config()
	var statuses []MCPServerStatus
	if e.mcp != nil {
		statuses = e.mcp.ListServerStatuses(ctx, cfg.MCPServers)
	}
	return redactMCPServers(cfg.MCPServers), statuses
}

// StartMCPOAuth initiates an OAuth 2.0/2.1 PKCE authorization flow for a configured HTTP MCP server.
func (e *Engine) StartMCPOAuth(ctx context.Context, serverName, redirectURI string) (MCPOAuthStartResult, error) {
	if e.mcp == nil || e.mcp.OAuth() == nil {
		return MCPOAuthStartResult{}, newErr("MCP manager unavailable", http.StatusServiceUnavailable)
	}
	srv, err := e.findMCPServer(serverName)
	if err != nil {
		return MCPOAuthStartResult{}, err
	}
	return e.mcp.OAuth().StartAuth(ctx, srv, e.getOPReader(), redirectURI)
}

// CompleteMCPOAuth exchanges an authorization code for OAuth tokens and tests the MCP server handshake.
func (e *Engine) CompleteMCPOAuth(ctx context.Context, serverName, state, code string) (MCPOAuthCompleteResult, error) {
	if e.mcp == nil || e.mcp.OAuth() == nil {
		return MCPOAuthCompleteResult{}, newErr("MCP manager unavailable", http.StatusServiceUnavailable)
	}
	srv, err := e.findMCPServer(serverName)
	if err != nil {
		return MCPOAuthCompleteResult{}, err
	}
	tok, err := e.mcp.OAuth().ExchangeCode(ctx, srv, e.getOPReader(), state, code)
	if err != nil {
		return MCPOAuthCompleteResult{}, err
	}
	e.mcp.ResetServer(srv.Name)
	res := MCPOAuthCompleteResult{
		ServerName: srv.Name,
		Authorized: true,
		Scope:      tok.Scope,
		ExpiresAt:  tok.ExpiresAt,
	}
	if srv.Enabled {
		tCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		tools, tErr := e.mcp.TestServer(tCtx, srv)
		if tErr != nil {
			res.TestError = tErr.Error()
		} else {
			res.ToolCount = len(tools)
			res.Tools = make([]string, 0, len(tools))
			for _, t := range tools {
				res.Tools = append(res.Tools, t.RemoteName)
			}
		}
	}
	return res, nil
}

// CompleteMCPOAuthByState completes an OAuth callback using only state and code.
func (e *Engine) CompleteMCPOAuthByState(ctx context.Context, state, code string) (MCPOAuthCompleteResult, error) {
	if e.mcp == nil || e.mcp.OAuth() == nil {
		return MCPOAuthCompleteResult{}, newErr("MCP manager unavailable", http.StatusServiceUnavailable)
	}
	srvName, err := e.mcp.OAuth().LookupPendingServer(state)
	if err != nil {
		return MCPOAuthCompleteResult{}, err
	}
	return e.CompleteMCPOAuth(ctx, srvName, state, code)
}

// RevokeMCPOAuth clears stored OAuth tokens and closes any cached connection for serverName.
func (e *Engine) RevokeMCPOAuth(serverName string) error {
	if e.mcp == nil || e.mcp.OAuth() == nil {
		return nil
	}
	clean := strings.ToLower(strings.TrimSpace(serverName))
	if err := e.mcp.OAuth().Revoke(clean); err != nil {
		return err
	}
	e.mcp.ResetServer(clean)
	return nil
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

// SetSandboxRunner overrides the container runtime backend (used in tests).
func (e *Engine) SetSandboxRunner(r SandboxRunner) {
	if e.sandbox != nil {
		e.sandbox.setRunner(r)
	}
}

// SetSkillRegistry overrides the remote skill registry client (used in tests).
func (e *Engine) SetSkillRegistry(r *SkillRegistry) {
	e.cfgMu.Lock()
	e.skills = r
	e.cfgMu.Unlock()
}

func (e *Engine) skillRegistry() *SkillRegistry {
	e.cfgMu.RLock()
	r := e.skills
	e.cfgMu.RUnlock()
	if r != nil {
		return r
	}
	return NewSkillRegistry()
}

// SearchSkills searches the configured skills registry (skills.sh) for matching playbooks.
func (e *Engine) SearchSkills(ctx context.Context, query string, limit int) ([]SkillSearchResult, error) {
	return e.skillRegistry().Search(ctx, query, limit)
}

// FetchSkill fetches and validates a remote skill bundle without writing it to the vault.
func (e *Engine) FetchSkill(ctx context.Context, source, skill, ref, slug string) (*FetchedSkillBundle, error) {
	return e.skillRegistry().Fetch(ctx, source, skill, ref, slug)
}

// InstallSkill fetches a remote skill bundle and saves it into _skills/<slug>/ in the primary vault.
func (e *Engine) InstallSkill(ctx context.Context, source, skill, ref, slug string, overwrite bool) (Skill, *FetchedSkillBundle, error) {
	bundle, err := e.FetchSkill(ctx, source, skill, ref, slug)
	if err != nil {
		return Skill{}, nil, err
	}
	v, err := e.vault()
	if err != nil {
		return Skill{}, nil, err
	}
	if !overwrite {
		if _, gErr := v.GetSkill(bundle.Slug); gErr == nil {
			return Skill{}, nil, newErr(fmt.Sprintf("skill %q already exists; set overwrite=true to update", bundle.Slug), http.StatusConflict)
		}
	}
	sk, err := v.SaveSkillBundle(bundle.Slug, bundle.Content, bundle.ExtraFiles, &bundle.Provenance)
	if err != nil {
		return Skill{}, nil, err
	}
	v.MaybeGitCommitContext(ctx, e.Config().GitAutoCommit, "assistant: install skill "+sk.Name)
	return sk, bundle, nil
}

// BuildSandboxImages builds or rebuilds the sandbox OCI images and offline network.
func (e *Engine) BuildSandboxImages(ctx context.Context, rebuildBase bool) (SandboxBuildResult, error) {
	if e.sandbox == nil {
		return SandboxBuildResult{}, newErr("sandbox unavailable", http.StatusServiceUnavailable)
	}
	return e.sandbox.BuildImages(ctx, e.Config().Sandbox, rebuildBase)
}

// ResetSandbox stops/removes the channel sandbox container and optionally clears /work/out.
func (e *Engine) ResetSandbox(ctx context.Context, channelID string, clearOutput bool) error {
	if e.sandbox == nil {
		return newErr("sandbox unavailable", http.StatusServiceUnavailable)
	}
	return e.sandbox.Reset(ctx, channelID, clearOutput)
}
