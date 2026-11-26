// Package slacker implements the lighthouse-hosted Slack Socket Mode dispatcher and pull-based Mac workers.
package slacker

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/registry"
	"github.com/bdatdo0601/fleet/internal/session"
)

// DefaultAccountID is the implicit Slack account identifier used when none is specified.
const DefaultAccountID = "default"

// Routing strategy constants for multi-device Slacker channels.
const (
	StrategyLeastAgents = "least_agents"
	StrategyRoundRobin  = "round_robin"
)

// SlackerError represents a caller-level error carrying an HTTP status code.
type SlackerError struct {
	Msg    string
	Status int
}

func (e *SlackerError) Error() string { return e.Msg }

// HTTPStatus returns the HTTP status code associated with this error.
func (e *SlackerError) HTTPStatus() int { return e.Status }

func newSlackerErr(msg string, status int) *SlackerError {
	if status == 0 {
		status = 400
	}
	return &SlackerError{Msg: msg, Status: status}
}

// AccountConfig stores credentials and policy for one Slack workspace/user account.
type AccountConfig struct {
	ID                string `json:"id"`
	Enabled           bool   `json:"enabled"`
	AppToken          string `json:"app_token,omitempty"`
	UserToken         string `json:"user_token,omitempty"`
	BotToken          string `json:"bot_token,omitempty"`
	AppTokenOPRef     string `json:"app_token_op_ref,omitempty"`
	UserTokenOPRef    string `json:"user_token_op_ref,omitempty"`
	BotTokenOPRef     string `json:"bot_token_op_ref,omitempty"`
	AuthorizedUserID  string `json:"authorized_user_id"`
	AuthorizedUser    string `json:"authorized_user,omitempty"`
	TeamID            string `json:"team_id,omitempty"`
	TeamName          string `json:"team_name,omitempty"`
	AllowUnmappedSend bool   `json:"allow_unmapped_send,omitempty"`
}

// AccountView is the redacted view of AccountConfig returned over HTTP APIs.
type AccountView struct {
	ID                string `json:"id"`
	Enabled           bool   `json:"enabled"`
	Configured        bool   `json:"configured"`
	HasAppToken       bool   `json:"has_app_token"`
	AppTokenHint      string `json:"app_token_hint,omitempty"`
	HasUserToken      bool   `json:"has_user_token"`
	UserTokenHint     string `json:"user_token_hint,omitempty"`
	HasBotToken       bool   `json:"has_bot_token"`
	BotTokenHint      string `json:"bot_token_hint,omitempty"`
	AppTokenOPRef     string `json:"app_token_op_ref,omitempty"`
	UserTokenOPRef    string `json:"user_token_op_ref,omitempty"`
	BotTokenOPRef     string `json:"bot_token_op_ref,omitempty"`
	AuthorizedUserID  string `json:"authorized_user_id"`
	AuthorizedUser    string `json:"authorized_user,omitempty"`
	TeamID            string `json:"team_id,omitempty"`
	TeamName          string `json:"team_name,omitempty"`
	AllowUnmappedSend bool   `json:"allow_unmapped_send"`
}

// AccountUpdate describes partial updates for a single Slack account.
type AccountUpdate struct {
	Enabled           *bool   `json:"enabled"`
	AppToken          *string `json:"app_token"`
	UserToken         *string `json:"user_token"`
	BotToken          *string `json:"bot_token"`
	AppTokenOPRef     *string `json:"app_token_op_ref"`
	UserTokenOPRef    *string `json:"user_token_op_ref"`
	BotTokenOPRef     *string `json:"bot_token_op_ref"`
	AuthorizedUserID  *string `json:"authorized_user_id"`
	AuthorizedUser    *string `json:"authorized_user"`
	TeamID            *string `json:"team_id"`
	TeamName          *string `json:"team_name"`
	AllowUnmappedSend *bool   `json:"allow_unmapped_send"`
}

// DeviceConfig stores per-machine Slacker defaults such as its workspace root directory.
type DeviceConfig struct {
	WorkspaceDir string `json:"workspace_dir,omitempty"`
}

// ChannelMapping maps a Slack (account, channel_id) pair to Fleet device(s), strategy, policy, and local repo path.
type ChannelMapping struct {
	Account     string   `json:"account,omitempty"`
	ChannelID   string   `json:"channel_id"`
	ChannelName string   `json:"channel_name,omitempty"`
	Device      string   `json:"device,omitempty"`
	Devices     []string `json:"devices,omitempty"`
	Strategy    string   `json:"strategy,omitempty"`
	Policy      string   `json:"policy,omitempty"`
	RepoPath    string   `json:"repo_path,omitempty"`
	UseTmux     bool     `json:"use_tmux,omitempty"`
}

// Config persists lighthouse Slacker settings in ~/.fleet/slacker.json.
// Top-level token fields are kept only for backward-compatible unmarshaling of single-account configs.
type Config struct {
	Accounts         map[string]AccountConfig  `json:"accounts,omitempty"`
	Devices          map[string]DeviceConfig   `json:"devices,omitempty"`
	DefaultDevices   []string                  `json:"default_devices,omitempty"`
	DefaultStrategy  string                    `json:"default_strategy,omitempty"`
	DefaultPolicy    string                    `json:"default_policy,omitempty"`
	Channels         map[string]ChannelMapping `json:"channels"`
	Emojis           map[string]string         `json:"emojis"`
	UpdatedAt        string                    `json:"updated_at,omitempty"`
	Enabled          bool                      `json:"enabled,omitempty"`
	AppToken         string                    `json:"app_token,omitempty"`
	UserToken        string                    `json:"user_token,omitempty"`
	BotToken         string                    `json:"bot_token,omitempty"`
	AppTokenOPRef    string                    `json:"app_token_op_ref,omitempty"`
	UserTokenOPRef   string                    `json:"user_token_op_ref,omitempty"`
	BotTokenOPRef    string                    `json:"bot_token_op_ref,omitempty"`
	AuthorizedUserID string                    `json:"authorized_user_id,omitempty"`
	AuthorizedUser   string                    `json:"authorized_user,omitempty"`
	TeamID           string                    `json:"team_id,omitempty"`
	TeamName         string                    `json:"team_name,omitempty"`
}

// ConfigView is the redacted view of Config returned over HTTP APIs.
type ConfigView struct {
	Accounts          map[string]AccountView    `json:"accounts"`
	Devices           map[string]DeviceConfig   `json:"devices"`
	DefaultDevices    []string                  `json:"default_devices"`
	DefaultStrategy   string                    `json:"default_strategy"`
	DefaultPolicy     string                    `json:"default_policy"`
	Enabled           bool                      `json:"enabled"`
	Configured        bool                      `json:"configured"`
	HasAppToken       bool                      `json:"has_app_token"`
	AppTokenHint      string                    `json:"app_token_hint,omitempty"`
	HasUserToken      bool                      `json:"has_user_token"`
	UserTokenHint     string                    `json:"user_token_hint,omitempty"`
	HasBotToken       bool                      `json:"has_bot_token"`
	BotTokenHint      string                    `json:"bot_token_hint,omitempty"`
	AppTokenOPRef     string                    `json:"app_token_op_ref,omitempty"`
	UserTokenOPRef    string                    `json:"user_token_op_ref,omitempty"`
	BotTokenOPRef     string                    `json:"bot_token_op_ref,omitempty"`
	AuthorizedUserID  string                    `json:"authorized_user_id"`
	AuthorizedUser    string                    `json:"authorized_user,omitempty"`
	TeamID            string                    `json:"team_id,omitempty"`
	TeamName          string                    `json:"team_name,omitempty"`
	AllowUnmappedSend bool                      `json:"allow_unmapped_send"`
	Channels          map[string]ChannelMapping `json:"channels"`
	Emojis            map[string]string         `json:"emojis"`
	UpdatedAt         string                    `json:"updated_at,omitempty"`
}

// ConfigUpdate is accepted by POST /v1/slacker/config; blank tokens preserve existing stored tokens.
type ConfigUpdate struct {
	Account           string                    `json:"account"`
	DeleteAccount     string                    `json:"delete_account"`
	Accounts          map[string]AccountUpdate  `json:"accounts"`
	Devices           map[string]DeviceConfig   `json:"devices"`
	DefaultDevices    *[]string                 `json:"default_devices"`
	DefaultStrategy   *string                   `json:"default_strategy"`
	DefaultPolicy     *string                   `json:"default_policy"`
	Enabled           *bool                     `json:"enabled"`
	AppToken          *string                   `json:"app_token"`
	UserToken         *string                   `json:"user_token"`
	BotToken          *string                   `json:"bot_token"`
	AppTokenOPRef     *string                   `json:"app_token_op_ref"`
	UserTokenOPRef    *string                   `json:"user_token_op_ref"`
	BotTokenOPRef     *string                   `json:"bot_token_op_ref"`
	AuthorizedUserID  *string                   `json:"authorized_user_id"`
	AuthorizedUser    *string                   `json:"authorized_user"`
	TeamID            *string                   `json:"team_id"`
	TeamName          *string                   `json:"team_name"`
	AllowUnmappedSend *bool                     `json:"allow_unmapped_send"`
	Channels          map[string]ChannelMapping `json:"channels"`
	Emojis            map[string]string         `json:"emojis"`
	Verify            bool                      `json:"verify"`
}

// Job represents a persisted Slack-reaction launch job in slacker.db.
type Job struct {
	ID             string `json:"id"`
	Account        string `json:"account"`
	EventID        string `json:"event_id"`
	ChannelID      string `json:"channel_id"`
	ChannelName    string `json:"channel_name,omitempty"`
	MessageTS      string `json:"message_ts"`
	ThreadTS       string `json:"thread_ts,omitempty"`
	Reaction       string `json:"reaction"`
	UserID         string `json:"user_id"`
	TargetDevice   string `json:"target_device"`
	RepoPath       string `json:"repo_path"`
	UseTmux        bool   `json:"use_tmux"`
	Policy         string `json:"policy,omitempty"`
	Harness        string `json:"harness"`
	SessionName    string `json:"session_name"`
	Prompt         string `json:"prompt"`
	Permalink      string `json:"permalink,omitempty"`
	Status         string `json:"status"`
	AttemptCount   int    `json:"attempt_count"`
	ClaimedBy      string `json:"claimed_by,omitempty"`
	ClaimedAt      string `json:"claimed_at,omitempty"`
	LeaseExpiresAt string `json:"lease_expires_at,omitempty"`
	ReceiptID      string `json:"receipt_id,omitempty"`
	Backend        string `json:"backend,omitempty"`
	Error          string `json:"error,omitempty"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

// Receipt is reported by a target Mac worker back to the lighthouse after launching or reconciling a job.
type Receipt struct {
	JobID       string `json:"job_id"`
	ReceiptID   string `json:"receipt_id"`
	SessionName string `json:"session_name"`
	Backend     string `json:"backend"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
}

// RuntimeStatus tracks listener and per-device worker liveness in slacker.db.
type RuntimeStatus struct {
	Role        string `json:"role"`
	PID         int    `json:"pid"`
	Connected   bool   `json:"connected"`
	LastPollAt  string `json:"last_poll_at,omitempty"`
	LastEventAt string `json:"last_event_at,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	UpdatedAt   string `json:"updated_at"`
}

var (
	unsafeNameCharsRE = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)
	accountIDRE       = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
)

// MaskSecret redacts a token/secret for display without leaking secret material.
func MaskSecret(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) >= 16 {
		return s[:8] + "..." + s[len(s)-4:]
	}
	return "***"
}

// NormalizeEmoji strips surrounding colons and lowercases the emoji name.
func NormalizeEmoji(raw string) string {
	s := strings.TrimSpace(strings.ToLower(raw))
	return strings.Trim(s, ":")
}

// NormalizeStrategy validates and normalizes a multi-device routing strategy.
func NormalizeStrategy(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return "", nil
	case "least_agents", "least-agents", "least":
		return StrategyLeastAgents, nil
	case "round_robin", "round-robin", "rr":
		return StrategyRoundRobin, nil
	default:
		return "", newSlackerErr(fmt.Sprintf("invalid routing strategy %q (expected least_agents or round_robin)", raw), 400)
	}
}

// NormalizePolicy validates and normalizes a Slacker session permission policy.
func NormalizePolicy(raw string) (string, error) {
	p := strings.ToLower(strings.TrimSpace(raw))
	if p == "" {
		return "", nil
	}
	pol, err := session.ParsePolicy(p, "")
	if err != nil {
		return "", newSlackerErr(err.Error(), 400)
	}
	return string(pol), nil
}

func normalizeDeviceList(raw []string) []string {
	if len(raw) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		d := strings.TrimSpace(item)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeWorkspaceDir(raw string) (string, error) {
	ws := strings.TrimSpace(raw)
	if ws == "" {
		return "", nil
	}
	if !strings.HasPrefix(ws, "/") && ws != "~" && !strings.HasPrefix(ws, "~/") {
		return "", newSlackerErr(fmt.Sprintf("workspace_dir %q must be an absolute or ~/ path", ws), 400)
	}
	if len(ws) > 1 {
		trimmed := strings.TrimRight(ws, "/")
		if trimmed == "" {
			ws = "/"
		} else {
			ws = trimmed
		}
	}
	return ws, nil
}

// NormalizeAccountID validates and normalizes a Slack account identifier, defaulting empty to "default".
func NormalizeAccountID(raw string) (string, error) {
	id := strings.ToLower(strings.TrimSpace(raw))
	if id == "" {
		return DefaultAccountID, nil
	}
	if !accountIDRE.MatchString(id) {
		return "", newSlackerErr("account id must be 1-32 lowercase letters, digits, dashes, or underscores", 400)
	}
	return id, nil
}

// ChannelKey builds the canonical map key for a (account, channelID) mapping.
func ChannelKey(account, channelID string) string {
	acct := strings.ToLower(strings.TrimSpace(account))
	cid := strings.TrimSpace(channelID)
	if acct == "" || acct == DefaultAccountID {
		return cid
	}
	return acct + ":" + cid
}

func sanitizeNameSegment(raw, fallback string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = unsafeNameCharsRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-_")
	if s == "" {
		return fallback
	}
	return s
}

// DeterministicSessionName derives a stable, valid session name from channelID and messageTS.
func DeterministicSessionName(channelID, messageTS string) string {
	ch := sanitizeNameSegment(channelID, "chan")
	ts := sanitizeNameSegment(strings.ReplaceAll(messageTS, ".", "-"), "0")
	return "slack-" + ch + "-" + ts
}

// DeterministicSessionNameForAccount derives a stable, valid session name scoped by account when non-default.
func DeterministicSessionNameForAccount(account, channelID, messageTS string) string {
	acct := strings.ToLower(strings.TrimSpace(account))
	if acct == "" || acct == DefaultAccountID {
		return DeterministicSessionName(channelID, messageTS)
	}
	ch := sanitizeNameSegment(channelID, "chan")
	ts := sanitizeNameSegment(strings.ReplaceAll(messageTS, ".", "-"), "0")
	return "slack-" + sanitizeNameSegment(acct, "acct") + "-" + ch + "-" + ts
}

// DefaultEmojis returns starter emoji-to-harness mappings.
func DefaultEmojis() map[string]string {
	return map[string]string{
		"claude":      session.HarnessClaude,
		"agy":         session.HarnessAgy,
		"antigravity": session.HarnessAgy,
		"codex":       session.HarnessCodex,
	}
}

func (a AccountConfig) configured() bool {
	return a.AppToken != "" && a.UserToken != "" && a.AuthorizedUserID != ""
}

// View returns the redacted AccountView for a.
func (a AccountConfig) View() AccountView {
	return AccountView{
		ID:                a.ID,
		Enabled:           a.Enabled,
		Configured:        a.configured(),
		HasAppToken:       a.AppToken != "",
		AppTokenHint:      MaskSecret(a.AppToken),
		HasUserToken:      a.UserToken != "",
		UserTokenHint:     MaskSecret(a.UserToken),
		HasBotToken:       a.BotToken != "",
		BotTokenHint:      MaskSecret(a.BotToken),
		AppTokenOPRef:     a.AppTokenOPRef,
		UserTokenOPRef:    a.UserTokenOPRef,
		BotTokenOPRef:     a.BotTokenOPRef,
		AuthorizedUserID:  a.AuthorizedUserID,
		AuthorizedUser:    a.AuthorizedUser,
		TeamID:            a.TeamID,
		TeamName:          a.TeamName,
		AllowUnmappedSend: a.AllowUnmappedSend,
	}
}

func normalizeLoadedConfig(cfg *Config) {
	if cfg.Accounts == nil {
		cfg.Accounts = map[string]AccountConfig{}
	}
	if cfg.Devices == nil {
		cfg.Devices = map[string]DeviceConfig{}
	}
	if cfg.Channels == nil {
		cfg.Channels = map[string]ChannelMapping{}
	}
	if cfg.Emojis == nil {
		cfg.Emojis = map[string]string{}
	}
	cfg.DefaultDevices = normalizeDeviceList(cfg.DefaultDevices)
	if strat, err := NormalizeStrategy(cfg.DefaultStrategy); err == nil {
		cfg.DefaultStrategy = strat
	}
	if pol, err := NormalizePolicy(cfg.DefaultPolicy); err == nil {
		cfg.DefaultPolicy = pol
	}
	// Migrate legacy top-level single-account config into Accounts["default"].
	if len(cfg.Accounts) == 0 && (cfg.Enabled || cfg.AppToken != "" || cfg.UserToken != "" || cfg.AuthorizedUserID != "") {
		cfg.Accounts[DefaultAccountID] = AccountConfig{
			ID:               DefaultAccountID,
			Enabled:          cfg.Enabled,
			AppToken:         cfg.AppToken,
			UserToken:        cfg.UserToken,
			BotToken:         cfg.BotToken,
			AppTokenOPRef:    cfg.AppTokenOPRef,
			UserTokenOPRef:   cfg.UserTokenOPRef,
			BotTokenOPRef:    cfg.BotTokenOPRef,
			AuthorizedUserID: cfg.AuthorizedUserID,
			AuthorizedUser:   cfg.AuthorizedUser,
			TeamID:           cfg.TeamID,
			TeamName:         cfg.TeamName,
		}
	}
	for id, acct := range cfg.Accounts {
		acct.ID = id
		cfg.Accounts[id] = acct
	}
	for dev, dc := range cfg.Devices {
		if norm, err := normalizeWorkspaceDir(dc.WorkspaceDir); err == nil {
			dc.WorkspaceDir = norm
		}
		if dc.WorkspaceDir == "" {
			delete(cfg.Devices, dev)
		} else {
			cfg.Devices[dev] = dc
		}
	}
	for k, ch := range cfg.Channels {
		if ch.Account == "" {
			ch.Account = DefaultAccountID
		}
		devs := normalizeDeviceList(ch.Devices)
		if len(devs) == 0 && strings.TrimSpace(ch.Device) != "" {
			devs = []string{strings.TrimSpace(ch.Device)}
		}
		ch.Devices = devs
		if len(devs) == 1 {
			ch.Device = devs[0]
		} else {
			ch.Device = ""
		}
		if strat, err := NormalizeStrategy(ch.Strategy); err == nil {
			ch.Strategy = strat
		}
		if pol, err := NormalizePolicy(ch.Policy); err == nil {
			ch.Policy = pol
		}
		cfg.Channels[k] = ch
	}
}

// LoadConfig reads ~/.fleet/slacker.json, returning an empty initialized Config if absent.
func LoadConfig(path string) (Config, error) {
	cfg := Config{
		Accounts: map[string]AccountConfig{},
		Devices:  map[string]DeviceConfig{},
		Channels: map[string]ChannelMapping{},
		Emojis:   map[string]string{},
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("invalid slacker config %s: %w", path, err)
	}
	normalizeLoadedConfig(&cfg)
	return cfg, nil
}

// SaveConfig atomically writes Config with 0600 permissions.
func SaveConfig(path string, cfg Config) error {
	normalizeLoadedConfig(&cfg)
	cfg.Enabled = false
	cfg.AppToken, cfg.UserToken, cfg.BotToken = "", "", ""
	cfg.AppTokenOPRef, cfg.UserTokenOPRef, cfg.BotTokenOPRef = "", "", ""
	cfg.AuthorizedUserID, cfg.AuthorizedUser, cfg.TeamID, cfg.TeamName = "", "", "", ""

	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	_, err = paths.WriteFileAtomic(path, append(b, '\n'), 0o600, nil, nil)
	return err
}

// SortedAccounts returns all configured accounts sorted by ID.
func (c Config) SortedAccounts() []AccountConfig {
	keys := make([]string, 0, len(c.Accounts))
	for k := range c.Accounts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]AccountConfig, 0, len(keys))
	for _, k := range keys {
		out = append(out, c.Accounts[k])
	}
	return out
}

// EnabledAccounts returns accounts that are enabled and have complete credentials.
func (c Config) EnabledAccounts() []AccountConfig {
	var out []AccountConfig
	for _, a := range c.SortedAccounts() {
		if a.Enabled && a.configured() {
			out = append(out, a)
		}
	}
	return out
}

func (c Config) primaryAccount() (AccountConfig, bool) {
	if a, ok := c.Accounts[DefaultAccountID]; ok {
		return a, true
	}
	sorted := c.SortedAccounts()
	if len(sorted) > 0 {
		return sorted[0], true
	}
	return AccountConfig{}, false
}

// EffectiveDefaultStrategy returns the configured global default routing strategy, defaulting to least_agents.
func (c Config) EffectiveDefaultStrategy() string {
	return cmp.Or(c.DefaultStrategy, StrategyLeastAgents)
}

// EffectiveChannelDevices returns the ordered candidate device list for ch, falling back to DefaultDevices.
func (c Config) EffectiveChannelDevices(ch ChannelMapping) []string {
	if devs := normalizeDeviceList(ch.Devices); len(devs) > 0 {
		return devs
	}
	if d := strings.TrimSpace(ch.Device); d != "" {
		return []string{d}
	}
	return normalizeDeviceList(c.DefaultDevices)
}

// EffectiveChannelStrategy returns the routing strategy for ch, falling back to DefaultStrategy then least_agents.
func (c Config) EffectiveChannelStrategy(ch ChannelMapping) string {
	return cmp.Or(ch.Strategy, c.DefaultStrategy, StrategyLeastAgents)
}

// EffectiveDefaultPolicy returns the configured global default permission policy, defaulting to bypass.
func (c Config) EffectiveDefaultPolicy() string {
	return cmp.Or(c.DefaultPolicy, string(session.PolicyBypass))
}

// EffectiveChannelPolicy returns the permission policy for ch, falling back to DefaultPolicy then bypass.
func (c Config) EffectiveChannelPolicy(ch ChannelMapping) string {
	return cmp.Or(ch.Policy, c.DefaultPolicy, string(session.PolicyBypass))
}

// ResolveRepoPath resolves repoPath on device, joining relative subpaths (or blank) onto the device's workspace_dir.
func (c Config) ResolveRepoPath(device, repoPath string) (string, error) {
	repo := strings.TrimSpace(repoPath)
	if strings.HasPrefix(repo, "/") || repo == "~" || strings.HasPrefix(repo, "~/") {
		return repo, nil
	}
	if strings.HasPrefix(repo, "~") {
		return "", newSlackerErr(fmt.Sprintf("repo_path %q must use ~/ or an absolute/relative path", repo), 400)
	}
	ws := strings.TrimSpace(c.Devices[device].WorkspaceDir)
	if ws == "" {
		return "", newSlackerErr(fmt.Sprintf("device %q has no workspace_dir configured for repo_path %q", device, repo), 400)
	}
	if repo == "" || repo == "." {
		return ws, nil
	}
	clean := filepath.Clean(repo)
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", newSlackerErr(fmt.Sprintf("relative repo_path %q cannot escape workspace_dir", repo), 400)
	}
	if clean == "." {
		return ws, nil
	}
	return filepath.Join(ws, clean), nil
}

// View returns a redacted ConfigView safe for HTTP/CLI display.
func (c Config) View() ConfigView {
	channels := make(map[string]ChannelMapping, len(c.Channels))
	for k, ch := range c.Channels {
		ch.Devices = slices.Clone(ch.Devices)
		channels[k] = ch
	}
	devices := make(map[string]DeviceConfig, len(c.Devices))
	maps.Copy(devices, c.Devices)
	defDevices := []string{}
	if len(c.DefaultDevices) > 0 {
		defDevices = slices.Clone(c.DefaultDevices)
	}
	emojis := make(map[string]string, len(c.Emojis))
	maps.Copy(emojis, c.Emojis)
	accts := make(map[string]AccountView, len(c.Accounts))
	anyEnabled, anyConfigured := false, false
	for id, a := range c.Accounts {
		v := a.View()
		accts[id] = v
		if a.Enabled {
			anyEnabled = true
		}
		if v.Configured {
			anyConfigured = true
		}
	}
	view := ConfigView{
		Accounts:        accts,
		Devices:         devices,
		DefaultDevices:  defDevices,
		DefaultStrategy: c.EffectiveDefaultStrategy(),
		DefaultPolicy:   c.EffectiveDefaultPolicy(),
		Enabled:         anyEnabled,
		Configured:      anyConfigured,
		Channels:        channels,
		Emojis:          emojis,
		UpdatedAt:       c.UpdatedAt,
	}
	if p, ok := c.primaryAccount(); ok {
		pv := p.View()
		view.HasAppToken = pv.HasAppToken
		view.AppTokenHint = pv.AppTokenHint
		view.HasUserToken = pv.HasUserToken
		view.UserTokenHint = pv.UserTokenHint
		view.HasBotToken = pv.HasBotToken
		view.BotTokenHint = pv.BotTokenHint
		view.AppTokenOPRef = pv.AppTokenOPRef
		view.UserTokenOPRef = pv.UserTokenOPRef
		view.BotTokenOPRef = pv.BotTokenOPRef
		view.AuthorizedUserID = pv.AuthorizedUserID
		view.AuthorizedUser = pv.AuthorizedUser
		view.TeamID = pv.TeamID
		view.TeamName = pv.TeamName
		view.AllowUnmappedSend = pv.AllowUnmappedSend
	}
	return view
}

// FindChannel looks up a channel mapping for (account, channelID).
func (c Config) FindChannel(account, channelID string) (ChannelMapping, bool) {
	acct := strings.ToLower(strings.TrimSpace(cmp.Or(account, DefaultAccountID)))
	if ch, ok := c.Channels[ChannelKey(acct, channelID)]; ok {
		return ch, true
	}
	if ch, ok := c.Channels[channelID]; ok && cmp.Or(ch.Account, DefaultAccountID) == acct {
		return ch, true
	}
	for _, ch := range c.Channels {
		if ch.ChannelID == channelID && cmp.Or(ch.Account, DefaultAccountID) == acct {
			return ch, true
		}
	}
	return ChannelMapping{}, false
}

// ResolveAccount resolves the target Slack account from an explicit account name, a mapped channel, or defaults.
func (c Config) ResolveAccount(account, channelID string) (AccountConfig, error) {
	if strings.TrimSpace(account) != "" {
		id, err := NormalizeAccountID(account)
		if err != nil {
			return AccountConfig{}, err
		}
		acct, ok := c.Accounts[id]
		if !ok {
			return AccountConfig{}, newSlackerErr(fmt.Sprintf("unknown Slack account %q", id), 404)
		}
		return acct, nil
	}
	if channelID != "" {
		var matched []AccountConfig
		for _, ch := range c.Channels {
			if ch.ChannelID == channelID {
				acctID := cmp.Or(ch.Account, DefaultAccountID)
				if a, ok := c.Accounts[acctID]; ok {
					matched = append(matched, a)
				}
			}
		}
		if len(matched) == 1 {
			return matched[0], nil
		}
	}
	if len(c.Accounts) == 1 {
		for _, a := range c.Accounts {
			return a, nil
		}
	}
	if a, ok := c.Accounts[DefaultAccountID]; ok {
		return a, nil
	}
	if len(c.Accounts) == 0 {
		return AccountConfig{}, newSlackerErr("no Slack accounts configured", 400)
	}
	return AccountConfig{}, newSlackerErr("multiple Slack accounts configured; specify account", 400)
}

// CanSendToChannel reports whether outbound messages on accountID are permitted for channelID.
func (c Config) CanSendToChannel(accountID, channelID string) bool {
	acct, ok := c.Accounts[accountID]
	if !ok {
		return false
	}
	if acct.AllowUnmappedSend {
		return true
	}
	_, mapped := c.FindChannel(accountID, channelID)
	return mapped
}

func applyAccountTokens(next *AccountConfig, upd AccountUpdate) error {
	if upd.AppToken != nil && strings.TrimSpace(*upd.AppToken) != "" {
		tok := strings.TrimSpace(*upd.AppToken)
		if !strings.HasPrefix(tok, "xapp-") {
			return newSlackerErr("app_token must start with xapp-", 400)
		}
		next.AppToken = tok
	}
	if upd.UserToken != nil && strings.TrimSpace(*upd.UserToken) != "" {
		tok := strings.TrimSpace(*upd.UserToken)
		if !strings.HasPrefix(tok, "xoxp-") && !strings.HasPrefix(tok, "xoxb-") {
			return newSlackerErr("user_token must start with xoxp- (or xoxb-)", 400)
		}
		next.UserToken = tok
	}
	if upd.BotToken != nil && strings.TrimSpace(*upd.BotToken) != "" {
		tok := strings.TrimSpace(*upd.BotToken)
		if !strings.HasPrefix(tok, "xoxb-") {
			return newSlackerErr("bot_token must start with xoxb-", 400)
		}
		next.BotToken = tok
	}
	return nil
}

func applyAccountUpdate(prev AccountConfig, upd AccountUpdate) (AccountConfig, error) {
	next := prev
	if upd.Enabled != nil {
		next.Enabled = *upd.Enabled
	}
	if upd.AllowUnmappedSend != nil {
		next.AllowUnmappedSend = *upd.AllowUnmappedSend
	}
	if err := applyAccountTokens(&next, upd); err != nil {
		return next, err
	}
	if upd.AppTokenOPRef != nil {
		next.AppTokenOPRef = strings.TrimSpace(*upd.AppTokenOPRef)
	}
	if upd.UserTokenOPRef != nil {
		next.UserTokenOPRef = strings.TrimSpace(*upd.UserTokenOPRef)
	}
	if upd.BotTokenOPRef != nil {
		next.BotTokenOPRef = strings.TrimSpace(*upd.BotTokenOPRef)
	}
	if upd.AuthorizedUserID != nil {
		next.AuthorizedUserID = strings.TrimSpace(*upd.AuthorizedUserID)
	}
	if upd.AuthorizedUser != nil {
		next.AuthorizedUser = strings.TrimSpace(*upd.AuthorizedUser)
	}
	if upd.TeamID != nil {
		next.TeamID = strings.TrimSpace(*upd.TeamID)
	}
	if upd.TeamName != nil {
		next.TeamName = strings.TrimSpace(*upd.TeamName)
	}
	if next.Enabled {
		if next.AppToken == "" || next.UserToken == "" {
			return next, newSlackerErr(fmt.Sprintf("account %q requires app_token and user_token before enabling", next.ID), 400)
		}
		if next.AuthorizedUserID == "" {
			return next, newSlackerErr(fmt.Sprintf("account %q requires authorized_user_id before enabling", next.ID), 400)
		}
	}
	return next, nil
}

func hasInlineAccountFields(upd ConfigUpdate) bool {
	return upd.Enabled != nil || upd.AppToken != nil || upd.UserToken != nil || upd.BotToken != nil ||
		upd.AppTokenOPRef != nil || upd.UserTokenOPRef != nil || upd.BotTokenOPRef != nil ||
		upd.AuthorizedUserID != nil || upd.AuthorizedUser != nil || upd.TeamID != nil || upd.TeamName != nil ||
		upd.AllowUnmappedSend != nil
}

func applyDeviceUpdates(rawDevices map[string]DeviceConfig, activeDevices map[string]bool) (map[string]DeviceConfig, error) {
	normalized := make(map[string]DeviceConfig, len(rawDevices))
	for rawDev, dc := range rawDevices {
		dev := strings.TrimSpace(rawDev)
		if dev == "" {
			return nil, newSlackerErr("device name cannot be empty in devices map", 400)
		}
		if len(activeDevices) > 0 && !activeDevices[dev] {
			return nil, newSlackerErr(fmt.Sprintf("unknown or revoked device %q in devices", dev), 400)
		}
		ws, err := normalizeWorkspaceDir(dc.WorkspaceDir)
		if err != nil {
			return nil, err
		}
		if ws == "" {
			continue
		}
		normalized[dev] = DeviceConfig{WorkspaceDir: ws}
	}
	return normalized, nil
}

func validateDeviceList(raw []string, activeDevices map[string]bool, ctxLabel string) ([]string, error) {
	devs := normalizeDeviceList(raw)
	if len(activeDevices) > 0 {
		for _, dev := range devs {
			if !activeDevices[dev] {
				return nil, newSlackerErr(fmt.Sprintf("unknown or revoked device %q in %s", dev, ctxLabel), 400)
			}
		}
	}
	if devs == nil {
		return []string{}, nil
	}
	return devs, nil
}

func applyChannelUpdates(rawChannels map[string]ChannelMapping, activeDevices map[string]bool) (map[string]ChannelMapping, error) {
	normalized := make(map[string]ChannelMapping, len(rawChannels))
	for rawKey, ch := range rawChannels {
		acctID, err := NormalizeAccountID(ch.Account)
		if err != nil {
			return nil, err
		}
		cid := strings.TrimSpace(ch.ChannelID)
		if cid == "" {
			if _, after, ok := strings.Cut(rawKey, ":"); ok {
				cid = strings.TrimSpace(after)
			} else {
				cid = strings.TrimSpace(rawKey)
			}
		}
		if cid == "" {
			return nil, newSlackerErr("each channel mapping requires channel_id", 400)
		}
		devs := normalizeDeviceList(ch.Devices)
		if len(devs) == 0 && strings.TrimSpace(ch.Device) != "" {
			devs = []string{strings.TrimSpace(ch.Device)}
		}
		if len(activeDevices) > 0 {
			for _, dev := range devs {
				if !activeDevices[dev] {
					return nil, newSlackerErr(fmt.Sprintf("unknown or revoked device %q in channel %s", dev, cid), 400)
				}
			}
		}
		strat, err := NormalizeStrategy(ch.Strategy)
		if err != nil {
			return nil, err
		}
		pol, err := NormalizePolicy(ch.Policy)
		if err != nil {
			return nil, err
		}
		repo := strings.TrimSpace(ch.RepoPath)
		ch.Account = acctID
		ch.ChannelID = cid
		ch.ChannelName = strings.TrimSpace(ch.ChannelName)
		ch.Devices = devs
		if len(devs) == 1 {
			ch.Device = devs[0]
		} else {
			ch.Device = ""
		}
		ch.Strategy = strat
		ch.Policy = pol
		ch.RepoPath = repo
		normalized[ChannelKey(acctID, cid)] = ch
	}
	return normalized, nil
}

func applyEmojiUpdates(rawEmojis map[string]string) (map[string]string, error) {
	normalized := make(map[string]string, len(rawEmojis))
	for rawEmoji, rawHarness := range rawEmojis {
		em := NormalizeEmoji(rawEmoji)
		h, ok := session.NormalizeHarness(rawHarness)
		if em == "" || !ok {
			return nil, newSlackerErr(fmt.Sprintf("invalid emoji mapping %q -> %q (expected claude, agy, or codex)", rawEmoji, rawHarness), 400)
		}
		normalized[em] = h
	}
	return normalized, nil
}

// ApplyUpdate merges upd onto prev, validating accounts, token prefixes, devices, workspace dirs, routing defaults, repo paths, and harness mappings.
func ApplyUpdate(prev Config, upd ConfigUpdate, activeDevices map[string]bool) (Config, error) {
	next := prev
	normalizeLoadedConfig(&next)

	if upd.DeleteAccount != "" {
		delID, err := NormalizeAccountID(upd.DeleteAccount)
		if err != nil {
			return next, err
		}
		delete(next.Accounts, delID)
		for k, ch := range next.Channels {
			if cmp.Or(ch.Account, DefaultAccountID) == delID {
				delete(next.Channels, k)
			}
		}
	}

	for rawID, aUpd := range upd.Accounts {
		id, err := NormalizeAccountID(rawID)
		if err != nil {
			return next, err
		}
		cur := next.Accounts[id]
		cur.ID = id
		updated, err := applyAccountUpdate(cur, aUpd)
		if err != nil {
			return next, err
		}
		next.Accounts[id] = updated
	}

	if hasInlineAccountFields(upd) {
		id, err := NormalizeAccountID(upd.Account)
		if err != nil {
			return next, err
		}
		cur := next.Accounts[id]
		cur.ID = id
		updated, err := applyAccountUpdate(cur, AccountUpdate{
			Enabled:           upd.Enabled,
			AppToken:          upd.AppToken,
			UserToken:         upd.UserToken,
			BotToken:          upd.BotToken,
			AppTokenOPRef:     upd.AppTokenOPRef,
			UserTokenOPRef:    upd.UserTokenOPRef,
			BotTokenOPRef:     upd.BotTokenOPRef,
			AuthorizedUserID:  upd.AuthorizedUserID,
			AuthorizedUser:    upd.AuthorizedUser,
			TeamID:            upd.TeamID,
			TeamName:          upd.TeamName,
			AllowUnmappedSend: upd.AllowUnmappedSend,
		})
		if err != nil {
			return next, err
		}
		next.Accounts[id] = updated
	}

	if upd.Devices != nil {
		devs, err := applyDeviceUpdates(upd.Devices, activeDevices)
		if err != nil {
			return next, err
		}
		next.Devices = devs
	}
	if upd.DefaultDevices != nil {
		defDevs, err := validateDeviceList(*upd.DefaultDevices, activeDevices, "default_devices")
		if err != nil {
			return next, err
		}
		next.DefaultDevices = defDevs
	}
	if upd.DefaultStrategy != nil {
		strat, err := NormalizeStrategy(*upd.DefaultStrategy)
		if err != nil {
			return next, err
		}
		next.DefaultStrategy = strat
	}
	if upd.DefaultPolicy != nil {
		pol, err := NormalizePolicy(*upd.DefaultPolicy)
		if err != nil {
			return next, err
		}
		next.DefaultPolicy = pol
	}
	if upd.Channels != nil {
		ch, err := applyChannelUpdates(upd.Channels, activeDevices)
		if err != nil {
			return next, err
		}
		next.Channels = ch
	}
	if upd.Devices != nil || upd.DefaultDevices != nil || upd.Channels != nil {
		for _, ch := range next.Channels {
			targets := next.EffectiveChannelDevices(ch)
			if len(targets) == 0 {
				return next, newSlackerErr(fmt.Sprintf("channel %s requires at least one device or default_devices", ch.ChannelID), 400)
			}
			for _, dev := range targets {
				if _, err := next.ResolveRepoPath(dev, ch.RepoPath); err != nil {
					return next, newSlackerErr(fmt.Sprintf("channel %s: %s", ch.ChannelID, err.Error()), 400)
				}
			}
		}
	}
	if upd.Emojis != nil {
		em, err := applyEmojiUpdates(upd.Emojis)
		if err != nil {
			return next, err
		}
		next.Emojis = em
	}
	next.UpdatedAt = registry.NowISO()
	return next, nil
}

// SortedChannels returns channel mappings sorted by ChannelKey.
func (c Config) SortedChannels() []ChannelMapping {
	keys := make([]string, 0, len(c.Channels))
	for k := range c.Channels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]ChannelMapping, 0, len(keys))
	for _, k := range keys {
		out = append(out, c.Channels[k])
	}
	return out
}

var manifestUserScopes = []string{
	"reactions:read",
	"channels:history",
	"groups:history",
	"im:history",
	"mpim:history",
	"channels:read",
	"groups:read",
	"users:read",
	"chat:write",
	"search:read",
}

// ManifestYAML returns the Slack App Manifest YAML for Socket Mode, user reaction dispatch, and agent read/send.
func ManifestYAML() string {
	var b strings.Builder
	b.WriteString(`display_information:
  name: Fleet Slacker
  description: Dispatch Fleet agent sessions from Slack reactions and let agents read/send Slack context
settings:
  event_subscriptions:
    user_events:
      - reaction_added
  interactivity:
    is_enabled: false
  org_deploy_enabled: false
  socket_mode_enabled: true
  token_rotation_enabled: false
oauth_config:
  scopes:
    user:
`)
	for _, sc := range manifestUserScopes {
		fmt.Fprintf(&b, "      - %s\n", sc)
	}
	return b.String()
}

// ManifestJSON returns the Slack App Manifest JSON for Socket Mode, user reaction dispatch, and agent read/send.
func ManifestJSON() string {
	obj := map[string]any{
		"display_information": map[string]any{
			"name":        "Fleet Slacker",
			"description": "Dispatch Fleet agent sessions from Slack reactions and let agents read/send Slack context",
		},
		"settings": map[string]any{
			"event_subscriptions": map[string]any{
				"user_events": []string{"reaction_added"},
			},
			"interactivity":          map[string]any{"is_enabled": false},
			"org_deploy_enabled":     false,
			"socket_mode_enabled":    true,
			"token_rotation_enabled": false,
		},
		"oauth_config": map[string]any{
			"scopes": map[string]any{
				"user": manifestUserScopes,
			},
		},
	}
	b, _ := json.MarshalIndent(obj, "", "  ")
	return string(b) + "\n"
}
