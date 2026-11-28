// Package assistant implements the lighthouse always-on personal assistant,
// OpenRouter streaming/tool loop, Decisions API routing, Obsidian vault skills
// & Observational Memory, stdio MCP client manager, and 1Password credential proxy.
package assistant

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bdatdo0601/fleet/internal/registry"
)

const (
	PrimaryVaultName = "agent"

	DefaultOpenRouterURL = "https://openrouter.ai/api/v1"
	DefaultChatModel     = "anthropic/claude-haiku-5.5"
	DefaultFastModel     = "thinkingmachines/inkling-2-mini"
	DefaultFrontierModel = "openai/gpt-6.2-sol"
	DefaultDecisionModel = "google/gemini-decision-1"
	DefaultObserverModel = "meta/muse-ember"
	DefaultHeartbeatMins = 30
	DefaultObserveTokens = 12000
	DefaultReflectBytes  = 24000
)

// AssistantError is an error carrying an HTTP status code for API handlers.
type AssistantError struct {
	Msg    string
	Status int
}

func (e *AssistantError) Error() string { return e.Msg }

// HTTPStatus returns the HTTP status code associated with this error.
func (e *AssistantError) HTTPStatus() int { return cmp.Or(e.Status, http.StatusBadRequest) }

func newErr(msg string, status int) *AssistantError {
	return &AssistantError{Msg: msg, Status: cmp.Or(status, http.StatusBadRequest)}
}

// ModelConfig defines per-role OpenRouter model slugs and short slash aliases.
type ModelConfig struct {
	DefaultChat string            `json:"default_chat"`
	Fast        string            `json:"fast"`
	Frontier    string            `json:"frontier"`
	Decision    string            `json:"decision"`
	Observer    string            `json:"observer"`
	Reflector   string            `json:"reflector"`
	Heartbeat   string            `json:"heartbeat"`
	Aliases     map[string]string `json:"aliases,omitempty"`
}

// MCPServerConfig describes one external stdio or Streamable HTTP/SSE MCP server managed by the assistant.
type MCPServerConfig struct {
	Name                 string            `json:"name"`
	Transport            string            `json:"transport,omitempty"`
	Command              string            `json:"command,omitempty"`
	Args                 []string          `json:"args,omitempty"`
	Env                  map[string]string `json:"env,omitempty"`
	EnvRefs              map[string]string `json:"env_refs,omitempty"`
	URL                  string            `json:"url,omitempty"`
	Headers              map[string]string `json:"headers,omitempty"`
	HeaderRefs           map[string]string `json:"header_refs,omitempty"`
	Enabled              bool              `json:"enabled"`
	RequireApprovalTools []string          `json:"require_approval_tools,omitempty"`
}

// NotificationsConfig configures outbound-only notifications (e.g. Discord Webhook into read-only alert channels).
type NotificationsConfig struct {
	Enabled              bool              `json:"enabled"`
	FleetBaseURL         string            `json:"fleet_base_url,omitempty"`
	DiscordWebhookOPRef  string            `json:"discord_webhook_op_ref,omitempty"`
	DiscordChannelOPRefs map[string]string `json:"discord_channel_op_refs,omitempty"`
	NotifyApprovals      *bool             `json:"notify_approvals,omitempty"`
	MinPriority          string            `json:"min_priority,omitempty"`
}

// CredentialPolicy defines a zero-exposure 1Password HTTP credential alias.
type CredentialPolicy struct {
	Alias           string   `json:"alias"`
	OPRef           string   `json:"op_ref"`
	AllowedHosts    []string `json:"allowed_hosts"`
	HeaderName      string   `json:"header_name,omitempty"`
	HeaderPrefix    string   `json:"header_prefix,omitempty"`
	RequireApproval bool     `json:"require_approval"`
}

// VaultMount configures an additional named Obsidian vault mounted alongside the primary agent vault.
type VaultMount struct {
	Name          string   `json:"name"`
	Path          string   `json:"path"`
	ReadOnly      bool     `json:"read_only,omitempty"`
	AutoWriteDirs []string `json:"auto_write_dirs,omitempty"`
}

var vaultNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// IsValidVaultName reports whether name is a valid mounted extra vault identifier.
func IsValidVaultName(name string) bool {
	clean := strings.ToLower(strings.TrimSpace(name))
	if clean == "" || clean == PrimaryVaultName || clean == "all" {
		return false
	}
	return vaultNameRE.MatchString(clean)
}

// Config holds persistent configuration for the lighthouse assistant.
type Config struct {
	Enabled                  bool               `json:"enabled"`
	BaseURL                  string             `json:"base_url"`
	APIKey                   string             `json:"api_key,omitempty"`
	APIKeyOPRef              string             `json:"api_key_op_ref,omitempty"`
	ZDR                      bool               `json:"zdr"`
	DataCollection           string             `json:"data_collection,omitempty"`
	VaultPath                string             `json:"vault_path,omitempty"`
	AutoWriteDirs            []string           `json:"auto_write_dirs,omitempty"`
	ExtraVaults              []VaultMount       `json:"extra_vaults,omitempty"`
	GitAutoCommit            bool               `json:"git_auto_commit"`
	HeartbeatIntervalMinutes int                `json:"heartbeat_interval_minutes"`
	ObserverThresholdTokens  int                `json:"observer_threshold_tokens"`
	ReflectorThresholdBytes  int                `json:"reflector_threshold_bytes"`
	Models                   ModelConfig         `json:"models"`
	MCPServers               []MCPServerConfig   `json:"mcp_servers,omitempty"`
	Credentials              []CredentialPolicy  `json:"credentials,omitempty"`
	Notifications            NotificationsConfig `json:"notifications"`
	UpdatedAt                string              `json:"updated_at,omitempty"`
}

// CredentialView is the secret-free view of a CredentialPolicy returned to API callers.
type CredentialView struct {
	Alias           string   `json:"alias"`
	OPRefHint       string   `json:"op_ref_hint"`
	AllowedHosts    []string `json:"allowed_hosts"`
	HeaderName      string   `json:"header_name"`
	HeaderPrefix    string   `json:"header_prefix"`
	RequireApproval bool     `json:"require_approval"`
}

// ConfigView exposes non-secret assistant configuration to CLI and web clients.
type ConfigView struct {
	Enabled                  bool              `json:"enabled"`
	Configured               bool              `json:"configured"`
	BaseURL                  string            `json:"base_url"`
	APIKeyHint               string            `json:"api_key_hint"`
	APIKeyOPRef              string            `json:"api_key_op_ref,omitempty"`
	ZDR                      bool              `json:"zdr"`
	DataCollection           string            `json:"data_collection"`
	VaultPath                string            `json:"vault_path"`
	EffectiveVaultPath       string            `json:"effective_vault_path"`
	AutoWriteDirs            []string          `json:"auto_write_dirs"`
	ExtraVaults              []VaultMount      `json:"extra_vaults"`
	GitAutoCommit            bool              `json:"git_auto_commit"`
	HeartbeatIntervalMinutes int               `json:"heartbeat_interval_minutes"`
	ObserverThresholdTokens  int               `json:"observer_threshold_tokens"`
	ReflectorThresholdBytes  int               `json:"reflector_threshold_bytes"`
	Models                   ModelConfig         `json:"models"`
	MCPServers               []MCPServerConfig   `json:"mcp_servers"`
	Credentials              []CredentialView    `json:"credentials"`
	Notifications            NotificationsConfig `json:"notifications"`
	UpdatedAt                string              `json:"updated_at,omitempty"`
}

// DefaultAliases returns built-in model shortcuts selectable in chat or via /alias.
func DefaultAliases() map[string]string {
	return map[string]string{
		"auto":         "auto",
		"mini":         DefaultFastModel,
		"inkling-mini": DefaultFastModel,
		"nano":         DefaultFastModel,
		"inkling":      "thinkingmachines/inkling-2-small",
		"ember":        DefaultObserverModel,
		"muse":         "meta/muse-spark-open",
		"luna":         "openai/gpt-6-luna",
		"deepseek":     "deepseek/deepseek-v4.1-flash-1017",
		"terra":        "openai/gpt-6.1-terra",
		"haiku":        DefaultChatModel,
		"sonnet":       "anthropic/claude-sonnet-5.5",
		"sol":          DefaultFrontierModel,
		"opus":         "anthropic/claude-opus-5.5",
		"astra":        "openai/gpt-6.2-astra",
		"gemini":       "google/gemini-4-hydrogen",
		"neon":         "google/gemini-4-neon",
	}
}

var legacyAliasUpgrades = map[string]string{
	"openai/gpt-6-astra":          "openai/gpt-6.2-astra",
	"anthropic/claude-sonnet-4.6": "anthropic/claude-sonnet-5.5",
	"anthropic/claude-opus-4.5":   "anthropic/claude-opus-5.5",
	"anthropic/claude-haiku-4.5":  "anthropic/claude-haiku-5.5",
	"deepseek/deepseek-chat":      "deepseek/deepseek-v4.1-flash-1017",
	"openai/gpt-5-nano":           "thinkingmachines/inkling-2-mini",
}

// DefaultConfig returns a baseline assistant configuration with safe defaults.
func DefaultConfig() Config {
	return Config{
		BaseURL:                  DefaultOpenRouterURL,
		ZDR:                      true,
		DataCollection:           "deny",
		AutoWriteDirs:            []string{"_agent", "_skills", "Daily", "Inbox"},
		GitAutoCommit:            true,
		HeartbeatIntervalMinutes: DefaultHeartbeatMins,
		ObserverThresholdTokens:  DefaultObserveTokens,
		ReflectorThresholdBytes:  DefaultReflectBytes,
		Models: ModelConfig{
			DefaultChat: DefaultChatModel,
			Fast:        DefaultFastModel,
			Frontier:    DefaultFrontierModel,
			Decision:    DefaultDecisionModel,
			Observer:    DefaultObserverModel,
			Reflector:   DefaultObserverModel,
			Heartbeat:   DefaultFastModel,
			Aliases:     DefaultAliases(),
		},
	}
}

// NormalizeConfig fills missing defaults and cleans fields.
func NormalizeConfig(cfg Config) Config {
	def := DefaultConfig()
	cfg.BaseURL = strings.TrimRight(cmp.Or(strings.TrimSpace(cfg.BaseURL), def.BaseURL), "/")
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.APIKeyOPRef = strings.TrimSpace(cfg.APIKeyOPRef)
	if strings.TrimSpace(cfg.DataCollection) == "" {
		cfg.DataCollection = def.DataCollection
	}
	cfg.VaultPath = strings.TrimSpace(cfg.VaultPath)
	if len(cfg.AutoWriteDirs) == 0 {
		cfg.AutoWriteDirs = append([]string(nil), def.AutoWriteDirs...)
	}
	if cfg.HeartbeatIntervalMinutes < 0 {
		cfg.HeartbeatIntervalMinutes = 0
	}
	if cfg.ObserverThresholdTokens <= 0 {
		cfg.ObserverThresholdTokens = def.ObserverThresholdTokens
	}
	if cfg.ReflectorThresholdBytes <= 0 {
		cfg.ReflectorThresholdBytes = def.ReflectorThresholdBytes
	}
	cfg.Models.DefaultChat = cmp.Or(strings.TrimSpace(cfg.Models.DefaultChat), def.Models.DefaultChat)
	cfg.Models.Fast = cmp.Or(strings.TrimSpace(cfg.Models.Fast), def.Models.Fast)
	cfg.Models.Frontier = cmp.Or(strings.TrimSpace(cfg.Models.Frontier), def.Models.Frontier)
	cfg.Models.Decision = cmp.Or(strings.TrimSpace(cfg.Models.Decision), def.Models.Decision)
	cfg.Models.Observer = cmp.Or(strings.TrimSpace(cfg.Models.Observer), def.Models.Observer)
	cfg.Models.Reflector = cmp.Or(strings.TrimSpace(cfg.Models.Reflector), def.Models.Reflector)
	cfg.Models.Heartbeat = cmp.Or(strings.TrimSpace(cfg.Models.Heartbeat), def.Models.Heartbeat)
	aliases := DefaultAliases()
	for k, v := range cfg.Models.Aliases {
		if upgraded, ok := legacyAliasUpgrades[v]; ok {
			aliases[k] = upgraded
		} else {
			aliases[k] = v
		}
	}
	cfg.Models.Aliases = aliases
	cfg.ExtraVaults = normalizeVaultMounts(cfg.ExtraVaults)
	cfg.MCPServers = normalizeMCPServers(cfg.MCPServers)
	cfg.Notifications = normalizeNotificationsConfig(cfg.Notifications)
	return cfg
}

func normalizeVaultMounts(mounts []VaultMount) []VaultMount {
	if mounts == nil {
		return nil
	}
	out := make([]VaultMount, 0, len(mounts))
	for _, m := range mounts {
		m.Name = strings.ToLower(strings.TrimSpace(m.Name))
		m.Path = strings.TrimSpace(m.Path)
		var dirs []string
		for _, d := range m.AutoWriteDirs {
			if clean := strings.Trim(strings.TrimSpace(d), "/"); clean != "" {
				dirs = append(dirs, clean)
			}
		}
		m.AutoWriteDirs = dirs
		out = append(out, m)
	}
	return out
}

func validateExtraVaults(mounts []VaultMount) error {
	seen := make(map[string]struct{}, len(mounts))
	for _, m := range mounts {
		if !IsValidVaultName(m.Name) {
			return newErr(fmt.Sprintf("invalid extra vault name %q (must match [a-z0-9][a-z0-9_-]* and cannot be %q or \"all\")", m.Name, PrimaryVaultName), http.StatusBadRequest)
		}
		if _, dup := seen[m.Name]; dup {
			return newErr(fmt.Sprintf("duplicate extra vault name %q", m.Name), http.StatusBadRequest)
		}
		seen[m.Name] = struct{}{}
		if strings.TrimSpace(m.Path) == "" {
			return newErr(fmt.Sprintf("extra vault %q path is required", m.Name), http.StatusBadRequest)
		}
	}
	return nil
}

func copyVaultMounts(mounts []VaultMount) []VaultMount {
	out := make([]VaultMount, 0, len(mounts))
	for _, m := range mounts {
		cp := m
		if len(m.AutoWriteDirs) > 0 {
			cp.AutoWriteDirs = append([]string(nil), m.AutoWriteDirs...)
		}
		out = append(out, cp)
	}
	return out
}

// LoadConfig reads assistant.json from path, returning defaults when absent.
func LoadConfig(path string) (Config, error) {
	if path == "" {
		return DefaultConfig(), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultConfig(), nil
		}
		return Config{}, err
	}
	cfg := DefaultConfig()
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse assistant config: %w", err)
	}
	return NormalizeConfig(cfg), nil
}

// SaveConfig atomically writes assistant.json with owner-only permissions.
func SaveConfig(path string, cfg Config) error {
	if path == "" {
		return errors.New("assistant config path is required")
	}
	cfg = NormalizeConfig(cfg)
	cfg.UpdatedAt = registry.NowISO()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func maskSecret(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "..." + s[len(s)-4:]
}

func redactMCPServers(servers []MCPServerConfig) []MCPServerConfig {
	out := make([]MCPServerConfig, 0, len(servers))
	for _, srv := range servers {
		copySrv := srv
		if len(srv.Env) > 0 {
			copySrv.Env = make(map[string]string, len(srv.Env))
			for k, v := range srv.Env {
				copySrv.Env[k] = maskSecret(v)
			}
		}
		if len(srv.Headers) > 0 {
			copySrv.Headers = make(map[string]string, len(srv.Headers))
			for k, v := range srv.Headers {
				copySrv.Headers[k] = maskSecret(v)
			}
		}
		out = append(out, copySrv)
	}
	return out
}

// NormalizeMCPServerConfig cleans fields and defaults transport on an MCPServerConfig.
func NormalizeMCPServerConfig(srv MCPServerConfig) MCPServerConfig {
	srv.Name = strings.ToLower(strings.TrimSpace(srv.Name))
	srv.Command = strings.TrimSpace(srv.Command)
	srv.URL = strings.TrimSpace(srv.URL)
	tr := strings.ToLower(strings.TrimSpace(srv.Transport))
	if tr == "sse" || tr == "http" || (tr == "" && srv.URL != "") {
		srv.Transport = "http"
	} else {
		srv.Transport = "stdio"
	}
	return srv
}

func normalizeMCPServers(servers []MCPServerConfig) []MCPServerConfig {
	if servers == nil {
		return nil
	}
	out := make([]MCPServerConfig, 0, len(servers))
	for _, s := range servers {
		out = append(out, NormalizeMCPServerConfig(s))
	}
	return out
}

func normalizeNotificationsConfig(n NotificationsConfig) NotificationsConfig {
	n.FleetBaseURL = strings.TrimRight(strings.TrimSpace(n.FleetBaseURL), "/")
	n.DiscordWebhookOPRef = strings.TrimSpace(n.DiscordWebhookOPRef)
	if n.NotifyApprovals == nil {
		t := true
		n.NotifyApprovals = &t
	}
	switch strings.ToLower(strings.TrimSpace(n.MinPriority)) {
	case "low", "normal", "urgent":
		n.MinPriority = strings.ToLower(strings.TrimSpace(n.MinPriority))
	default:
		n.MinPriority = "normal"
	}
	if len(n.DiscordChannelOPRefs) > 0 {
		clean := make(map[string]string, len(n.DiscordChannelOPRefs))
		for k, v := range n.DiscordChannelOPRefs {
			ck := strings.ToLower(strings.TrimSpace(k))
			cv := strings.TrimSpace(v)
			if ck != "" && cv != "" {
				clean[ck] = cv
			}
		}
		n.DiscordChannelOPRefs = clean
	}
	return n
}

var blockedSelfSetupShells = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "fish": true,
	"csh": true, "tcsh": true, "ksh": true, "osascript": true,
}

func looksLikeSecretKey(k string) bool {
	up := strings.ToUpper(strings.TrimSpace(k))
	for _, marker := range []string{"TOKEN", "SECRET", "KEY", "PASSWORD", "AUTH", "CREDENTIAL", "COOKIE", "BEARER"} {
		if strings.Contains(up, marker) {
			return true
		}
	}
	return false
}

// ValidateMCPServerConfig checks MCPServerConfig fields and enforces self-setup safety rules when selfSetup is true.
func ValidateMCPServerConfig(srv MCPServerConfig, selfSetup bool) error {
	srv = NormalizeMCPServerConfig(srv)
	if !vaultNameRE.MatchString(srv.Name) {
		return newErr(fmt.Sprintf("invalid MCP server name %q (must match [a-z0-9][a-z0-9_-]*)", srv.Name), http.StatusBadRequest)
	}
	if srv.Transport == "http" {
		if srv.URL == "" || (!strings.HasPrefix(srv.URL, "http://") && !strings.HasPrefix(srv.URL, "https://")) {
			return newErr(fmt.Sprintf("MCP server %q requires an http:// or https:// url", srv.Name), http.StatusBadRequest)
		}
	} else {
		if srv.Command == "" {
			return newErr(fmt.Sprintf("MCP server %q requires a command", srv.Name), http.StatusBadRequest)
		}
		if selfSetup && blockedSelfSetupShells[strings.ToLower(filepath.Base(srv.Command))] {
			return newErr(fmt.Sprintf("MCP server %q cannot invoke shell %q directly during self-setup", srv.Name, srv.Command), http.StatusBadRequest)
		}
	}
	for k, ref := range srv.EnvRefs {
		if !strings.HasPrefix(strings.TrimSpace(ref), "op://") {
			return newErr(fmt.Sprintf("MCP %q env_refs[%q] must be an op:// reference", srv.Name, k), http.StatusBadRequest)
		}
	}
	for k, ref := range srv.HeaderRefs {
		if !strings.HasPrefix(strings.TrimSpace(ref), "op://") {
			return newErr(fmt.Sprintf("MCP %q header_refs[%q] must be an op:// reference", srv.Name, k), http.StatusBadRequest)
		}
	}
	if selfSetup {
		for k := range srv.Env {
			if looksLikeSecretKey(k) {
				return newErr(fmt.Sprintf("plain env[%q] rejected during self-setup: store secret in 1Password and use env_refs", k), http.StatusBadRequest)
			}
		}
		for k := range srv.Headers {
			if looksLikeSecretKey(k) {
				return newErr(fmt.Sprintf("plain headers[%q] rejected during self-setup: store secret in 1Password and use header_refs", k), http.StatusBadRequest)
			}
		}
	}
	return nil
}

// View converts Config into a safe non-secret ConfigView.
func (c Config) View(effectiveVault string) ConfigView {
	creds := make([]CredentialView, 0, len(c.Credentials))
	for _, cp := range c.Credentials {
		creds = append(creds, CredentialView{
			Alias:           cp.Alias,
			OPRefHint:       maskSecret(cp.OPRef),
			AllowedHosts:    append([]string(nil), cp.AllowedHosts...),
			HeaderName:      cmp.Or(cp.HeaderName, "Authorization"),
			HeaderPrefix:    cmp.Or(cp.HeaderPrefix, "Bearer "),
			RequireApproval: cp.RequireApproval,
		})
	}
	return ConfigView{
		Enabled:                  c.Enabled,
		Configured:               strings.TrimSpace(c.APIKey) != "" || strings.TrimSpace(c.APIKeyOPRef) != "",
		BaseURL:                  c.BaseURL,
		APIKeyHint:               maskSecret(c.APIKey),
		APIKeyOPRef:              c.APIKeyOPRef,
		ZDR:                      c.ZDR,
		DataCollection:           c.DataCollection,
		VaultPath:                c.VaultPath,
		EffectiveVaultPath:       effectiveVault,
		AutoWriteDirs:            append([]string(nil), c.AutoWriteDirs...),
		ExtraVaults:              copyVaultMounts(c.ExtraVaults),
		GitAutoCommit:            c.GitAutoCommit,
		HeartbeatIntervalMinutes: c.HeartbeatIntervalMinutes,
		ObserverThresholdTokens:  c.ObserverThresholdTokens,
		ReflectorThresholdBytes:  c.ReflectorThresholdBytes,
		Models:                   c.Models,
		MCPServers:               redactMCPServers(c.MCPServers),
		Credentials:              creds,
		Notifications:            normalizeNotificationsConfig(c.Notifications),
		UpdatedAt:                c.UpdatedAt,
	}
}
