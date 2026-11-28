// Package assistant implements the lighthouse always-on personal assistant,
// OpenRouter streaming/tool loop, Decisions API routing, Obsidian vault skills
// & Observational Memory, stdio MCP client manager, and 1Password credential proxy.
package assistant

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/memory"
	"github.com/bdatdo0601/fleet/internal/registry"
)

const (
	PrimaryVaultName = "agent"

	DefaultOpenRouterURL    = "https://openrouter.ai/api/v1"
	DefaultChatModel        = "anthropic/claude-haiku-5.5"
	DefaultFastModel        = "thinkingmachines/inkling-2-mini"
	DefaultFrontierModel    = "openai/gpt-6.2-sol"
	DefaultDecisionModel    = "typesafe/jev-1.14"
	DefaultObserverModel    = "meta/muse-ember"
	DefaultHeartbeatMins    = 30
	DefaultObserveTokens    = 3000
	DefaultObserverIdleMins = 30
	DefaultReflectBytes     = 24000

	RoutingModeOff     = "off"
	RoutingModeHybrid  = "hybrid"
	RoutingModeGate    = "gate"
	RoutingModeAuto    = "auto"
	DefaultRoutingMode = RoutingModeHybrid

	DefaultFrontierBorderlineProb   = 0.60
	DefaultFrontierGateProb         = 0.80
	DefaultSameTopicProb            = 0.55
	DefaultFrontierGrantMaxTurns    = 5
	DefaultFrontierGrantMaxCostUSD  = 0.25
	DefaultFrontierGrantIdleMinutes = 25
	DefaultDecisionTimeoutMS        = 2000

	DefaultSandboxImage         = "fleet-sandbox:latest"
	DefaultSandboxBaseImage     = "fleet-sandbox:base"
	DefaultSandboxNetwork       = "fleet-sandbox-offline"
	DefaultSandboxSubnet        = "192.168.127.0/24"
	DefaultSandboxCPUs          = 4
	DefaultSandboxMemoryMB      = 4096
	DefaultSandboxIdleMinutes   = 20
	DefaultSandboxMaxFileMB     = 50
	DefaultSandboxMaxActive     = 2
	DefaultSandboxMaxTimeoutSec = 300

	DefaultLocalLLMURL       = "http://127.0.0.1:11434/v1"
	DefaultLocalLLMModel     = "qwen3:8b"
	DefaultLocalLLMKeepAlive = "2m"
	DefaultLocalLLMNumCtx    = 16384

	MCPApprovalPolicyAll        = "all"
	MCPApprovalPolicyWritesOnly = "writes_only"
	MCPApprovalPolicyNone       = "none"

	ExposureAssistantOnly   = "assistant_only"
	ExposurePersonalDevices = "personal_devices"

	DefaultEventsWaitingDwellMins = 5
	DefaultEventsCooldownMins     = 30
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

// RoutingConfig controls automatic frontier escalation, hybrid retry, and topic-scoped HITL grants.
type RoutingConfig struct {
	Mode              string  `json:"mode"`
	BorderlineProb    float64 `json:"borderline_prob,omitempty"`
	GateProb          float64 `json:"gate_prob,omitempty"`
	GrantMaxTurns     int     `json:"grant_max_turns,omitempty"`
	GrantMaxCostUSD   float64 `json:"grant_max_cost_usd,omitempty"`
	GrantIdleMinutes  int     `json:"grant_idle_minutes,omitempty"`
	DailySoftCapUSD   float64 `json:"daily_soft_cap_usd,omitempty"`
	DecisionTimeoutMS int     `json:"decision_timeout_ms,omitempty"`
}

// MCPOAuthConfig configures OAuth 2.0 / 2.1 Authorization Code + PKCE for an HTTP MCP server.
type MCPOAuthConfig struct {
	Enabled           bool              `json:"enabled"`
	Provider          string            `json:"provider,omitempty"`
	AuthURL           string            `json:"auth_url,omitempty"`
	TokenURL          string            `json:"token_url,omitempty"`
	ClientID          string            `json:"client_id,omitempty"`
	ClientIDOPRef     string            `json:"client_id_op_ref,omitempty"`
	ClientSecret      string            `json:"client_secret,omitempty"`
	ClientSecretOPRef string            `json:"client_secret_op_ref,omitempty"`
	RefreshTokenOPRef string            `json:"refresh_token_op_ref,omitempty"`
	Scopes            []string          `json:"scopes,omitempty"`
	RedirectURI       string            `json:"redirect_uri,omitempty"`
	ExtraAuthParams   map[string]string `json:"extra_auth_params,omitempty"`
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
	OAuth                *MCPOAuthConfig   `json:"oauth,omitempty"`
	Enabled              bool              `json:"enabled"`
	Exposure             string            `json:"exposure,omitempty"`
	ApprovalPolicy       string            `json:"approval_policy,omitempty"`
	RequireApprovalTools []string          `json:"require_approval_tools,omitempty"`
}

// EventsConfig controls the unified assistant event router, smart System-1 triage, quiet hours, and escalation cooldowns.
type EventsConfig struct {
	Enabled                   *bool             `json:"enabled,omitempty"`
	QuietHoursStart           string            `json:"quiet_hours_start,omitempty"`
	QuietHoursEnd             string            `json:"quiet_hours_end,omitempty"`
	Timezone                  string            `json:"timezone,omitempty"`
	WaitingDwellMinutes       int               `json:"waiting_dwell_minutes,omitempty"`
	EscalationCooldownMinutes int               `json:"escalation_cooldown_minutes,omitempty"`
	Routes                    map[string]string `json:"routes,omitempty"`
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

// DefaultDiscordCategoryName is the Discord server category used to group auto-synced Fleet channels.
const DefaultDiscordCategoryName = "Fleet Assistant"

// DiscordConfig configures the outbound Discord Gateway bot for two-way 1-to-1 channel sync & approvals.
type DiscordConfig struct {
	Enabled          bool              `json:"enabled"`
	BotTokenOPRef    string            `json:"bot_token_op_ref,omitempty"`
	AuthorizedUserID string            `json:"authorized_user_id,omitempty"`
	GuildID          string            `json:"guild_id,omitempty"`
	CategoryName     string            `json:"category_name,omitempty"`
	AutoSyncChannels *bool             `json:"auto_sync_channels,omitempty"`
	Channels         map[string]string `json:"channels,omitempty"`
	AllowApprovals   *bool             `json:"allow_approvals,omitempty"`
	AutoApprove      bool              `json:"auto_approve,omitempty"`
	MirrorWebTurns   *bool             `json:"mirror_web_turns,omitempty"`
	FleetBaseURL     string            `json:"fleet_base_url,omitempty"`
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
	Exposure      string   `json:"exposure,omitempty"`
	ReadOnly      bool     `json:"read_only,omitempty"`
	AutoWriteDirs []string `json:"auto_write_dirs,omitempty"`
}

// ValidSandboxProfiles lists optional heavier capability layers baked into the sandbox OCI image.
var ValidSandboxProfiles = []string{"browser", "office", "media"}

// LocalLLMConfig configures an optional local OpenAI-compatible/Ollama LLM endpoint for local/<model> routing.
type LocalLLMConfig struct {
	Enabled   bool   `json:"enabled"`
	BaseURL   string `json:"base_url,omitempty"`
	Model     string `json:"model,omitempty"`
	KeepAlive string `json:"keep_alive,omitempty"`
	NumCtx    int    `json:"num_ctx,omitempty"`
}

// TranscribeConfig configures local audio/voice-note transcription via whisper-cli and ffmpeg.
type TranscribeConfig struct {
	Enabled        bool   `json:"enabled"`
	Command        string `json:"command,omitempty"`
	ModelPath      string `json:"model_path,omitempty"`
	Language       string `json:"language,omitempty"`
	AutoTranscribe *bool  `json:"auto_transcribe,omitempty"`
}

// SandboxConfig configures the assistant's disposable Apple container micro-VM sandbox.
type SandboxConfig struct {
	Enabled          bool     `json:"enabled"`
	Image            string   `json:"image,omitempty"`
	BaseImage        string   `json:"base_image,omitempty"`
	Network          string   `json:"network,omitempty"`
	CPUs             int      `json:"cpus,omitempty"`
	MemoryMB         int      `json:"memory_mb,omitempty"`
	IdleMinutes      int      `json:"idle_minutes,omitempty"`
	MaxFileMB        int      `json:"max_file_mb,omitempty"`
	MaxActive        int      `json:"max_active,omitempty"`
	Profiles         []string `json:"profiles,omitempty"`
	ExtraAptPackages []string `json:"extra_apt_packages,omitempty"`
	ExtraPipPackages []string `json:"extra_pip_packages,omitempty"`
}

var (
	vaultNameRE        = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	aptPkgNameRE       = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]{0,63}$`)
	pipPkgSpecRE       = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._\-\[\],<>=!~]{0,95}$`)
	discordSnowflakeRE = regexp.MustCompile(`^[0-9]{15,22}$`)
)

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
	Enabled                  bool                `json:"enabled"`
	BaseURL                  string              `json:"base_url"`
	APIKey                   string              `json:"api_key,omitempty"`
	APIKeyOPRef              string              `json:"api_key_op_ref,omitempty"`
	ZDR                      bool                `json:"zdr"`
	AutoApprove              bool                `json:"auto_approve"`
	DataCollection           string              `json:"data_collection,omitempty"`
	VaultPath                string              `json:"vault_path,omitempty"`
	VaultExposure            string              `json:"vault_exposure,omitempty"`
	AutoWriteDirs            []string            `json:"auto_write_dirs,omitempty"`
	ExtraVaults              []VaultMount        `json:"extra_vaults,omitempty"`
	GitAutoCommit            bool                `json:"git_auto_commit"`
	HeartbeatIntervalMinutes int                 `json:"heartbeat_interval_minutes"`
	ObserverThresholdTokens  int                 `json:"observer_threshold_tokens"`
	ObserverIdleMinutes      int                 `json:"observer_idle_minutes"`
	ReflectorThresholdBytes  int                 `json:"reflector_threshold_bytes"`
	Models                   ModelConfig         `json:"models"`
	LocalLLM                 LocalLLMConfig      `json:"local_llm"`
	Embeddings               memory.EmbedConfig  `json:"embeddings"`
	Transcribe               TranscribeConfig    `json:"transcribe"`
	Routing                  RoutingConfig       `json:"routing"`
	Events                   EventsConfig        `json:"events"`
	MCPServers               []MCPServerConfig   `json:"mcp_servers,omitempty"`
	Credentials              []CredentialPolicy  `json:"credentials,omitempty"`
	Notifications            NotificationsConfig `json:"notifications"`
	Discord                  DiscordConfig       `json:"discord"`
	Sandbox                  SandboxConfig       `json:"sandbox"`
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
	Enabled                  bool                `json:"enabled"`
	Configured               bool                `json:"configured"`
	BaseURL                  string              `json:"base_url"`
	APIKeyHint               string              `json:"api_key_hint"`
	APIKeyOPRef              string              `json:"api_key_op_ref,omitempty"`
	ZDR                      bool                `json:"zdr"`
	AutoApprove              bool                `json:"auto_approve"`
	DataCollection           string              `json:"data_collection"`
	VaultPath                string              `json:"vault_path"`
	VaultExposure            string              `json:"vault_exposure"`
	EffectiveVaultPath       string              `json:"effective_vault_path"`
	AutoWriteDirs            []string            `json:"auto_write_dirs"`
	ExtraVaults              []VaultMount        `json:"extra_vaults"`
	GitAutoCommit            bool                `json:"git_auto_commit"`
	HeartbeatIntervalMinutes int                 `json:"heartbeat_interval_minutes"`
	ObserverThresholdTokens  int                 `json:"observer_threshold_tokens"`
	ObserverIdleMinutes      int                 `json:"observer_idle_minutes"`
	ReflectorThresholdBytes  int                 `json:"reflector_threshold_bytes"`
	Models                   ModelConfig         `json:"models"`
	LocalLLM                 LocalLLMConfig      `json:"local_llm"`
	Embeddings               memory.EmbedConfig  `json:"embeddings"`
	Transcribe               TranscribeConfig    `json:"transcribe"`
	Routing                  RoutingConfig       `json:"routing"`
	Events                   EventsConfig        `json:"events"`
	MCPServers               []MCPServerConfig   `json:"mcp_servers"`
	Credentials              []CredentialView    `json:"credentials"`
	Notifications            NotificationsConfig `json:"notifications"`
	Discord                  DiscordConfig       `json:"discord"`
	Sandbox                  SandboxConfig       `json:"sandbox"`
	UpdatedAt                string              `json:"updated_at,omitempty"`
}

// DefaultAliases returns built-in model shortcuts selectable in chat or via /alias.
func DefaultAliases() map[string]string {
	return map[string]string{
		"auto":         "auto",
		"local":        "local/default",
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

// NormalizeExposure normalizes an assistant capability exposure scope ("assistant_only" or "personal_devices").
func NormalizeExposure(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), ExposurePersonalDevices) {
		return ExposurePersonalDevices
	}
	return ExposureAssistantOnly
}

// ValidateExposure validates an optional exposure setting.
func ValidateExposure(raw, label string) error {
	clean := strings.ToLower(strings.TrimSpace(raw))
	if clean == "" || clean == ExposureAssistantOnly || clean == ExposurePersonalDevices {
		return nil
	}
	return newErr(fmt.Sprintf("%s invalid exposure %q (expected %s or %s)", label, raw, ExposureAssistantOnly, ExposurePersonalDevices), http.StatusBadRequest)
}

// DefaultEventsConfig returns default settings for the unified assistant event router.
func DefaultEventsConfig() EventsConfig {
	on := true
	return EventsConfig{
		Enabled:                   &on,
		QuietHoursStart:           "23:00",
		QuietHoursEnd:             "07:00",
		Timezone:                  "Local",
		WaitingDwellMinutes:       DefaultEventsWaitingDwellMins,
		EscalationCooldownMinutes: DefaultEventsCooldownMins,
	}
}

func normalizeEventsConfig(e EventsConfig) EventsConfig {
	def := DefaultEventsConfig()
	if e.Enabled == nil {
		e.Enabled = def.Enabled
	} else {
		v := *e.Enabled
		e.Enabled = &v
	}
	e.QuietHoursStart = cmp.Or(strings.TrimSpace(e.QuietHoursStart), def.QuietHoursStart)
	e.QuietHoursEnd = cmp.Or(strings.TrimSpace(e.QuietHoursEnd), def.QuietHoursEnd)
	e.Timezone = cmp.Or(strings.TrimSpace(e.Timezone), def.Timezone)
	if e.WaitingDwellMinutes <= 0 {
		e.WaitingDwellMinutes = def.WaitingDwellMinutes
	}
	if e.EscalationCooldownMinutes <= 0 {
		e.EscalationCooldownMinutes = def.EscalationCooldownMinutes
	}
	if len(e.Routes) > 0 {
		clean := make(map[string]string, len(e.Routes))
		for k, v := range e.Routes {
			ck := strings.ToLower(strings.TrimSpace(k))
			cv := strings.ToLower(strings.TrimSpace(v))
			if ck != "" && cv != "" {
				clean[ck] = cv
			}
		}
		e.Routes = clean
	}
	return e
}

func copyEventsConfig(e EventsConfig) EventsConfig {
	cp := normalizeEventsConfig(e)
	if len(e.Routes) > 0 {
		cp.Routes = maps.Clone(cp.Routes)
	}
	return cp
}

func parseHHMM(raw string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	if len(parts) != 2 {
		return 0, false
	}
	hh, err1 := strconv.Atoi(parts[0])
	mm, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, false
	}
	return hh*60 + mm, true
}

// ValidateEventsConfig validates EventsConfig fields.
func ValidateEventsConfig(e EventsConfig) error {
	e = normalizeEventsConfig(e)
	if _, ok := parseHHMM(e.QuietHoursStart); !ok {
		return newErr(fmt.Sprintf("events.quiet_hours_start %q must be HH:MM (00:00-23:59)", e.QuietHoursStart), http.StatusBadRequest)
	}
	if _, ok := parseHHMM(e.QuietHoursEnd); !ok {
		return newErr(fmt.Sprintf("events.quiet_hours_end %q must be HH:MM (00:00-23:59)", e.QuietHoursEnd), http.StatusBadRequest)
	}
	if !strings.EqualFold(e.Timezone, "Local") && !strings.EqualFold(e.Timezone, "UTC") {
		if _, err := time.LoadLocation(e.Timezone); err != nil {
			return newErr(fmt.Sprintf("invalid events.timezone %q: %v", e.Timezone, err), http.StatusBadRequest)
		}
	}
	for evType, route := range e.Routes {
		switch route {
		case "events_only", "ops_notify", "triage", "wake_ops", "escalate_general":
		default:
			return newErr(fmt.Sprintf("events.routes[%q] invalid action %q (expected events_only, ops_notify, triage, wake_ops, or escalate_general)", evType, route), http.StatusBadRequest)
		}
	}
	return nil
}

// DefaultConfig returns a baseline assistant configuration with safe defaults.
func DefaultConfig() Config {
	return Config{
		BaseURL:                  DefaultOpenRouterURL,
		ZDR:                      true,
		DataCollection:           "deny",
		VaultExposure:            ExposureAssistantOnly,
		AutoWriteDirs:            []string{"_agent", "_skills", "Daily", "Inbox"},
		GitAutoCommit:            true,
		HeartbeatIntervalMinutes: DefaultHeartbeatMins,
		ObserverThresholdTokens:  DefaultObserveTokens,
		ObserverIdleMinutes:      DefaultObserverIdleMins,
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
		LocalLLM:   normalizeLocalLLMConfig(LocalLLMConfig{}),
		Embeddings: normalizeAssistantEmbedConfig(memory.EmbedConfig{}),
		Transcribe: normalizeTranscribeConfig(TranscribeConfig{}),
		Routing:    normalizeRoutingConfig(RoutingConfig{Mode: DefaultRoutingMode}),
		Events:     DefaultEventsConfig(),
		Sandbox:    normalizeSandboxConfig(SandboxConfig{}),
	}
}

func normalizeAssistantEmbedConfig(e memory.EmbedConfig) memory.EmbedConfig {
	norm, err := memory.NormalizeEmbedConfig(e, memory.EmbedConfig{})
	if err == nil {
		return norm
	}
	e.Provider = cmp.Or(strings.ToLower(strings.TrimSpace(e.Provider)), memory.DefaultEmbedProvider)
	e.BaseURL = strings.TrimRight(cmp.Or(strings.TrimSpace(e.BaseURL), memory.DefaultEmbedBaseURL), "/")
	e.Model = cmp.Or(strings.TrimSpace(e.Model), memory.DefaultEmbedModel)
	e.KeepAlive = cmp.Or(strings.TrimSpace(e.KeepAlive), memory.DefaultEmbedKeepAlive)
	return e
}

func normalizeRoutingConfig(r RoutingConfig) RoutingConfig {
	switch strings.ToLower(strings.TrimSpace(r.Mode)) {
	case RoutingModeOff, RoutingModeHybrid, RoutingModeGate, RoutingModeAuto:
		r.Mode = strings.ToLower(strings.TrimSpace(r.Mode))
	default:
		r.Mode = DefaultRoutingMode
	}
	if r.BorderlineProb <= 0 || r.BorderlineProb >= 1 {
		r.BorderlineProb = DefaultFrontierBorderlineProb
	}
	if r.GateProb <= 0 || r.GateProb >= 1 {
		r.GateProb = DefaultFrontierGateProb
	}
	if r.BorderlineProb >= r.GateProb {
		r.BorderlineProb = max(0.05, r.GateProb-0.15)
	}
	if r.GrantMaxTurns <= 0 {
		r.GrantMaxTurns = DefaultFrontierGrantMaxTurns
	}
	if r.GrantMaxCostUSD <= 0 {
		r.GrantMaxCostUSD = DefaultFrontierGrantMaxCostUSD
	}
	if r.GrantIdleMinutes <= 0 {
		r.GrantIdleMinutes = DefaultFrontierGrantIdleMinutes
	}
	if r.DailySoftCapUSD < 0 {
		r.DailySoftCapUSD = 0
	}
	if r.DecisionTimeoutMS <= 0 {
		r.DecisionTimeoutMS = DefaultDecisionTimeoutMS
	} else {
		r.DecisionTimeoutMS = min(max(r.DecisionTimeoutMS, 500), 10000)
	}
	return r
}

func concreteDefaultChat(cfg Config) string {
	chat := strings.TrimSpace(cfg.Models.DefaultChat)
	if chat == "" || strings.EqualFold(chat, "auto") {
		return DefaultChatModel
	}
	return chat
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
	cfg.VaultExposure = NormalizeExposure(cfg.VaultExposure)
	if len(cfg.AutoWriteDirs) == 0 {
		cfg.AutoWriteDirs = append([]string(nil), def.AutoWriteDirs...)
	}
	if cfg.HeartbeatIntervalMinutes < 0 {
		cfg.HeartbeatIntervalMinutes = 0
	}
	if cfg.ObserverThresholdTokens <= 0 {
		cfg.ObserverThresholdTokens = def.ObserverThresholdTokens
	}
	if cfg.ObserverIdleMinutes < 0 {
		cfg.ObserverIdleMinutes = 0
	}
	if cfg.ReflectorThresholdBytes <= 0 {
		cfg.ReflectorThresholdBytes = def.ReflectorThresholdBytes
	}
	cfg.Models.DefaultChat = cmp.Or(strings.TrimSpace(cfg.Models.DefaultChat), def.Models.DefaultChat)
	cfg.Models.Fast = cmp.Or(strings.TrimSpace(cfg.Models.Fast), def.Models.Fast)
	cfg.Models.Frontier = cmp.Or(strings.TrimSpace(cfg.Models.Frontier), def.Models.Frontier)
	cfg.Models.Decision = cmp.Or(strings.TrimSpace(cfg.Models.Decision), def.Models.Decision)
	if cfg.ZDR && strings.Contains(strings.ToLower(cfg.BaseURL), "openrouter.ai") && cfg.Models.Decision == "google/gemini-decision-1" {
		cfg.Models.Decision = DefaultDecisionModel
	}
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
	cfg.LocalLLM = normalizeLocalLLMConfig(cfg.LocalLLM)
	cfg.Embeddings = normalizeAssistantEmbedConfig(cfg.Embeddings)
	cfg.Transcribe = normalizeTranscribeConfig(cfg.Transcribe)
	cfg.Routing = normalizeRoutingConfig(cfg.Routing)
	cfg.Events = normalizeEventsConfig(cfg.Events)
	cfg.ExtraVaults = normalizeVaultMounts(cfg.ExtraVaults)
	cfg.MCPServers = normalizeMCPServers(cfg.MCPServers)
	cfg.Notifications = normalizeNotificationsConfig(cfg.Notifications)
	cfg.Discord = normalizeDiscordConfig(cfg.Discord)
	cfg.Sandbox = normalizeSandboxConfig(cfg.Sandbox)
	return cfg
}

func normalizeTranscribeConfig(t TranscribeConfig) TranscribeConfig {
	t.Command = strings.TrimSpace(t.Command)
	t.ModelPath = strings.TrimSpace(t.ModelPath)
	t.Language = cmp.Or(strings.TrimSpace(t.Language), "auto")
	if t.AutoTranscribe == nil {
		on := true
		t.AutoTranscribe = &on
	}
	return t
}

func normalizeLocalLLMConfig(l LocalLLMConfig) LocalLLMConfig {
	l.BaseURL = strings.TrimRight(cmp.Or(strings.TrimSpace(l.BaseURL), DefaultLocalLLMURL), "/")
	l.Model = cmp.Or(strings.TrimSpace(l.Model), DefaultLocalLLMModel)
	l.KeepAlive = cmp.Or(strings.TrimSpace(l.KeepAlive), DefaultLocalLLMKeepAlive)
	if l.NumCtx <= 0 {
		l.NumCtx = DefaultLocalLLMNumCtx
	} else {
		l.NumCtx = min(max(l.NumCtx, 2048), 65536)
	}
	return l
}

// ValidateLocalLLMConfig checks LocalLLMConfig when enabled.
func ValidateLocalLLMConfig(l LocalLLMConfig) error {
	l = normalizeLocalLLMConfig(l)
	if !l.Enabled {
		return nil
	}
	if err := validateHTTPURLField(l.BaseURL, "local_llm.base_url"); err != nil {
		return err
	}
	if strings.TrimSpace(l.Model) == "" {
		return newErr("local_llm.model is required when local_llm is enabled", http.StatusBadRequest)
	}
	return nil
}

func normalizeStringList(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
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
	slices.Sort(out)
	return out
}

func normalizeSandboxConfig(s SandboxConfig) SandboxConfig {
	s.Image = cmp.Or(strings.TrimSpace(s.Image), DefaultSandboxImage)
	s.BaseImage = cmp.Or(strings.TrimSpace(s.BaseImage), DefaultSandboxBaseImage)
	s.Network = cmp.Or(strings.TrimSpace(s.Network), DefaultSandboxNetwork)
	if s.CPUs <= 0 {
		s.CPUs = DefaultSandboxCPUs
	} else {
		s.CPUs = min(max(s.CPUs, 1), 8)
	}
	if s.MemoryMB <= 0 {
		s.MemoryMB = DefaultSandboxMemoryMB
	} else {
		s.MemoryMB = min(max(s.MemoryMB, 256), 8192)
	}
	if s.IdleMinutes <= 0 {
		s.IdleMinutes = DefaultSandboxIdleMinutes
	} else {
		s.IdleMinutes = min(max(s.IdleMinutes, 1), 120)
	}
	if s.MaxFileMB <= 0 {
		s.MaxFileMB = DefaultSandboxMaxFileMB
	} else {
		s.MaxFileMB = min(max(s.MaxFileMB, 1), 100)
	}
	if s.MaxActive <= 0 {
		s.MaxActive = DefaultSandboxMaxActive
	} else {
		s.MaxActive = min(max(s.MaxActive, 1), 3)
	}
	if len(s.Profiles) > 0 {
		lower := make([]string, len(s.Profiles))
		for i, p := range s.Profiles {
			lower[i] = strings.ToLower(strings.TrimSpace(p))
		}
		s.Profiles = normalizeStringList(lower)
	}
	s.ExtraAptPackages = normalizeStringList(s.ExtraAptPackages)
	s.ExtraPipPackages = normalizeStringList(s.ExtraPipPackages)
	return s
}

func copySandboxConfig(s SandboxConfig) SandboxConfig {
	cp := normalizeSandboxConfig(s)
	if len(cp.Profiles) > 0 {
		cp.Profiles = append([]string(nil), cp.Profiles...)
	}
	if len(cp.ExtraAptPackages) > 0 {
		cp.ExtraAptPackages = append([]string(nil), cp.ExtraAptPackages...)
	}
	if len(cp.ExtraPipPackages) > 0 {
		cp.ExtraPipPackages = append([]string(nil), cp.ExtraPipPackages...)
	}
	return cp
}

// ValidateSandboxProfiles validates optional sandbox capability profile names.
func ValidateSandboxProfiles(profiles []string) error {
	for _, p := range profiles {
		clean := strings.ToLower(strings.TrimSpace(p))
		if !slices.Contains(ValidSandboxProfiles, clean) {
			return newErr(fmt.Sprintf("invalid sandbox profile %q (valid: %s)", p, strings.Join(ValidSandboxProfiles, ", ")), http.StatusBadRequest)
		}
	}
	return nil
}

// ValidateAptPackageName validates a Debian apt package name for sandbox image customization.
func ValidateAptPackageName(pkg string) error {
	clean := strings.TrimSpace(pkg)
	if !aptPkgNameRE.MatchString(clean) {
		return newErr(fmt.Sprintf("invalid apt package name %q (must match [a-z0-9][a-z0-9+.-]*)", pkg), http.StatusBadRequest)
	}
	return nil
}

// ValidatePipPackageSpec validates a PyPI package specifier (no URLs, VCS refs, flags, or shell metacharacters).
func ValidatePipPackageSpec(spec string) error {
	clean := strings.TrimSpace(spec)
	if clean == "" || strings.HasPrefix(clean, "-") || strings.Contains(clean, "://") ||
		strings.ContainsAny(clean, "@ \t\r\n;&|$`\\'\"") || !pipPkgSpecRE.MatchString(clean) {
		return newErr(fmt.Sprintf("invalid pip package spec %q", spec), http.StatusBadRequest)
	}
	return nil
}

func validateSandboxPackages(s SandboxConfig) error {
	if err := ValidateSandboxProfiles(s.Profiles); err != nil {
		return err
	}
	for _, pkg := range s.ExtraAptPackages {
		if err := ValidateAptPackageName(pkg); err != nil {
			return err
		}
	}
	for _, spec := range s.ExtraPipPackages {
		if err := ValidatePipPackageSpec(spec); err != nil {
			return err
		}
	}
	return nil
}

func normalizeVaultMounts(mounts []VaultMount) []VaultMount {
	if mounts == nil {
		return nil
	}
	out := make([]VaultMount, 0, len(mounts))
	for _, m := range mounts {
		m.Name = strings.ToLower(strings.TrimSpace(m.Name))
		m.Path = strings.TrimSpace(m.Path)
		m.Exposure = NormalizeExposure(m.Exposure)
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
		if err := ValidateExposure(m.Exposure, fmt.Sprintf("extra vault %q", m.Name)); err != nil {
			return err
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
	tmpFile, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmpFile.Chmod(0o600); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
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

func copyMCPOAuthConfig(o *MCPOAuthConfig) *MCPOAuthConfig {
	if o == nil {
		return nil
	}
	cp := *o
	if len(o.Scopes) > 0 {
		cp.Scopes = append([]string(nil), o.Scopes...)
	}
	if len(o.ExtraAuthParams) > 0 {
		cp.ExtraAuthParams = make(map[string]string, len(o.ExtraAuthParams))
		maps.Copy(cp.ExtraAuthParams, o.ExtraAuthParams)
	}
	return &cp
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
		if srv.OAuth != nil {
			copySrv.OAuth = copyMCPOAuthConfig(srv.OAuth)
			if copySrv.OAuth.ClientSecret != "" {
				copySrv.OAuth.ClientSecret = maskSecret(copySrv.OAuth.ClientSecret)
			}
		}
		out = append(out, copySrv)
	}
	return out
}

func isGoogleAPIHost(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "googleapis.com" || strings.HasSuffix(host, ".googleapis.com")
}

func normalizeMCPOAuthConfig(o *MCPOAuthConfig, serverURL string) *MCPOAuthConfig {
	if o == nil {
		return nil
	}
	cp := &MCPOAuthConfig{
		Enabled:           o.Enabled,
		Provider:          strings.ToLower(strings.TrimSpace(o.Provider)),
		AuthURL:           strings.TrimSpace(o.AuthURL),
		TokenURL:          strings.TrimSpace(o.TokenURL),
		ClientID:          strings.TrimSpace(o.ClientID),
		ClientIDOPRef:     strings.TrimSpace(o.ClientIDOPRef),
		ClientSecret:      strings.TrimSpace(o.ClientSecret),
		ClientSecretOPRef: strings.TrimSpace(o.ClientSecretOPRef),
		RefreshTokenOPRef: strings.TrimSpace(o.RefreshTokenOPRef),
		RedirectURI:       strings.TrimSpace(o.RedirectURI),
	}
	if len(o.Scopes) > 0 {
		seen := make(map[string]struct{}, len(o.Scopes))
		scopes := make([]string, 0, len(o.Scopes))
		for _, raw := range o.Scopes {
			for part := range strings.FieldsSeq(raw) {
				if _, ok := seen[part]; ok {
					continue
				}
				seen[part] = struct{}{}
				scopes = append(scopes, part)
			}
		}
		cp.Scopes = scopes
	}
	if len(o.ExtraAuthParams) > 0 {
		clean := make(map[string]string, len(o.ExtraAuthParams))
		for k, v := range o.ExtraAuthParams {
			ck := strings.TrimSpace(k)
			cv := strings.TrimSpace(v)
			if ck != "" && cv != "" {
				clean[ck] = cv
			}
		}
		if len(clean) > 0 {
			cp.ExtraAuthParams = clean
		}
	}
	if cp.Enabled && cp.Provider == "" && cp.AuthURL == "" && cp.TokenURL == "" && isGoogleAPIHost(serverURL) {
		cp.Provider = "google"
	}
	return cp
}

// NormalizeMCPServerConfig cleans fields and defaults transport on an MCPServerConfig.
func NormalizeMCPServerConfig(srv MCPServerConfig) MCPServerConfig {
	srv.Name = strings.ToLower(strings.TrimSpace(srv.Name))
	srv.Command = strings.TrimSpace(srv.Command)
	srv.URL = strings.TrimSpace(srv.URL)
	srv.Exposure = NormalizeExposure(srv.Exposure)
	srv.ApprovalPolicy = strings.ToLower(strings.TrimSpace(srv.ApprovalPolicy))
	tr := strings.ToLower(strings.TrimSpace(srv.Transport))
	if tr == "sse" || tr == "http" || (tr == "" && srv.URL != "") {
		srv.Transport = "http"
	} else {
		srv.Transport = "stdio"
	}
	srv.OAuth = normalizeMCPOAuthConfig(srv.OAuth, srv.URL)
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

func normalizeDiscordConfig(d DiscordConfig) DiscordConfig {
	d.BotTokenOPRef = strings.TrimSpace(d.BotTokenOPRef)
	d.AuthorizedUserID = strings.TrimSpace(d.AuthorizedUserID)
	d.GuildID = strings.TrimSpace(d.GuildID)
	d.CategoryName = cmp.Or(strings.TrimSpace(d.CategoryName), DefaultDiscordCategoryName)
	d.FleetBaseURL = strings.TrimRight(strings.TrimSpace(d.FleetBaseURL), "/")
	if d.AutoSyncChannels == nil {
		t := true
		d.AutoSyncChannels = &t
	}
	if d.AllowApprovals == nil {
		t := true
		d.AllowApprovals = &t
	}
	if d.MirrorWebTurns == nil {
		t := true
		d.MirrorWebTurns = &t
	}
	if len(d.Channels) > 0 {
		clean := make(map[string]string, len(d.Channels))
		for k, v := range d.Channels {
			ck := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(k, "#")))
			cv := strings.TrimSpace(v)
			if ck != "" && cv != "" {
				clean[ck] = cv
			}
		}
		d.Channels = clean
	}
	return d
}

// ValidateDiscordConfig validates DiscordConfig when enabled.
func ValidateDiscordConfig(d DiscordConfig) error {
	d = normalizeDiscordConfig(d)
	if !d.Enabled {
		return nil
	}
	if !strings.HasPrefix(d.BotTokenOPRef, "op://") {
		return newErr("discord.bot_token_op_ref must be an op:// 1Password reference", http.StatusBadRequest)
	}
	if !discordSnowflakeRE.MatchString(d.AuthorizedUserID) {
		return newErr("discord.authorized_user_id must be a numeric Discord user ID (15-22 digits)", http.StatusBadRequest)
	}
	if d.GuildID != "" && !discordSnowflakeRE.MatchString(d.GuildID) {
		return newErr("discord.guild_id must be a numeric Discord server ID (15-22 digits)", http.StatusBadRequest)
	}
	for ch, id := range d.Channels {
		if !discordSnowflakeRE.MatchString(id) {
			return newErr(fmt.Sprintf("discord.channels[%q] must be a numeric Discord channel ID", ch), http.StatusBadRequest)
		}
	}
	return nil
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
	switch srv.ApprovalPolicy {
	case "", MCPApprovalPolicyAll, MCPApprovalPolicyWritesOnly, MCPApprovalPolicyNone:
	default:
		return newErr(fmt.Sprintf("MCP %q invalid approval_policy %q (expected all, writes_only, or none)", srv.Name, srv.ApprovalPolicy), http.StatusBadRequest)
	}
	if err := ValidateExposure(srv.Exposure, fmt.Sprintf("MCP %q", srv.Name)); err != nil {
		return err
	}
	return validateMCPOAuthConfig(srv, selfSetup)
}

func validateHTTPURLField(raw, label string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return newErr(fmt.Sprintf("%s must be a valid http:// or https:// URL", label), http.StatusBadRequest)
	}
	return nil
}

func validateMCPOAuthConfig(srv MCPServerConfig, selfSetup bool) error {
	o := srv.OAuth
	if o == nil || !o.Enabled {
		return nil
	}
	if srv.Transport != "http" {
		return newErr(fmt.Sprintf("MCP %q oauth requires transport \"http\"", srv.Name), http.StatusBadRequest)
	}
	if o.Provider != "" && o.Provider != "google" {
		return newErr(fmt.Sprintf("MCP %q unsupported oauth.provider %q (supported: \"\", \"google\")", srv.Name, o.Provider), http.StatusBadRequest)
	}
	for field, ref := range map[string]string{
		"oauth.client_id_op_ref":     o.ClientIDOPRef,
		"oauth.client_secret_op_ref": o.ClientSecretOPRef,
		"oauth.refresh_token_op_ref": o.RefreshTokenOPRef,
	} {
		if ref != "" && !strings.HasPrefix(ref, "op://") {
			return newErr(fmt.Sprintf("MCP %q %s must be an op:// reference", srv.Name, field), http.StatusBadRequest)
		}
	}
	if o.ClientID == "" && o.ClientIDOPRef == "" {
		return newErr(fmt.Sprintf("MCP %q oauth requires client_id or client_id_op_ref", srv.Name), http.StatusBadRequest)
	}
	for label, rawURL := range map[string]string{
		fmt.Sprintf("MCP %q oauth.auth_url", srv.Name):     o.AuthURL,
		fmt.Sprintf("MCP %q oauth.token_url", srv.Name):    o.TokenURL,
		fmt.Sprintf("MCP %q oauth.redirect_uri", srv.Name): o.RedirectURI,
	} {
		if err := validateHTTPURLField(rawURL, label); err != nil {
			return err
		}
	}
	if selfSetup && o.ClientSecret != "" {
		return newErr("plain oauth.client_secret rejected during self-setup: store secret in 1Password and use oauth.client_secret_op_ref", http.StatusBadRequest)
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
	embView := normalizeAssistantEmbedConfig(c.Embeddings)
	embView.APIKey = maskSecret(embView.APIKey)
	return ConfigView{
		Enabled:                  c.Enabled,
		Configured:               strings.TrimSpace(c.APIKey) != "" || strings.TrimSpace(c.APIKeyOPRef) != "" || c.LocalLLM.Enabled,
		BaseURL:                  c.BaseURL,
		APIKeyHint:               maskSecret(c.APIKey),
		APIKeyOPRef:              c.APIKeyOPRef,
		ZDR:                      c.ZDR,
		AutoApprove:              c.AutoApprove,
		DataCollection:           c.DataCollection,
		VaultPath:                c.VaultPath,
		VaultExposure:            NormalizeExposure(c.VaultExposure),
		EffectiveVaultPath:       effectiveVault,
		AutoWriteDirs:            append([]string(nil), c.AutoWriteDirs...),
		ExtraVaults:              copyVaultMounts(c.ExtraVaults),
		GitAutoCommit:            c.GitAutoCommit,
		HeartbeatIntervalMinutes: c.HeartbeatIntervalMinutes,
		ObserverThresholdTokens:  c.ObserverThresholdTokens,
		ObserverIdleMinutes:      c.ObserverIdleMinutes,
		ReflectorThresholdBytes:  c.ReflectorThresholdBytes,
		Models:                   c.Models,
		LocalLLM:                 normalizeLocalLLMConfig(c.LocalLLM),
		Embeddings:               embView,
		Transcribe:               normalizeTranscribeConfig(c.Transcribe),
		Routing:                  normalizeRoutingConfig(c.Routing),
		Events:                   copyEventsConfig(c.Events),
		MCPServers:               redactMCPServers(c.MCPServers),
		Credentials:              creds,
		Notifications:            normalizeNotificationsConfig(c.Notifications),
		Discord:                  normalizeDiscordConfig(c.Discord),
		Sandbox:                  copySandboxConfig(c.Sandbox),
		UpdatedAt:                c.UpdatedAt,
	}
}
