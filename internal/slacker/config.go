package slacker

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/session"
)

func normalizeAccounts(cfg *Config) {
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
}

func normalizeDevices(devices map[string]DeviceConfig) {
	for dev, dc := range devices {
		if norm, err := normalizeWorkspaceDir(dc.WorkspaceDir); err == nil {
			dc.WorkspaceDir = norm
		}
		if dc.WorkspaceDir == "" {
			delete(devices, dev)
		} else {
			devices[dev] = dc
		}
	}
}

func normalizeChannelMappings(channels map[string]ChannelMapping) {
	for k, ch := range channels {
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
		channels[k] = ch
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
	normalizeAccounts(cfg)
	normalizeDevices(cfg.Devices)
	normalizeChannelMappings(cfg.Channels)
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
	next := cloneConfig(cfg)
	normalizeLoadedConfig(&next)
	next.Enabled = false
	next.AppToken, next.UserToken, next.BotToken = "", "", ""
	next.AppTokenOPRef, next.UserTokenOPRef, next.BotTokenOPRef = "", "", ""
	next.AuthorizedUserID, next.AuthorizedUser, next.TeamID, next.TeamName = "", "", "", ""

	b, err := json.MarshalIndent(next, "", "  ")
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

// EffectiveDefaultPolicy returns the configured global default permission policy, defaulting to auto.
func (c Config) EffectiveDefaultPolicy() string {
	return cmp.Or(c.DefaultPolicy, string(session.PolicyAuto))
}

// EffectiveChannelPolicy returns the permission policy for ch, falling back to DefaultPolicy then auto.
func (c Config) EffectiveChannelPolicy(ch ChannelMapping) string {
	return cmp.Or(ch.Policy, c.DefaultPolicy, string(session.PolicyAuto))
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
