package slacker

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/bdatdo0601/fleet/internal/registry"
	"github.com/bdatdo0601/fleet/internal/session"
)

func cloneConfig(c Config) Config {
	out := c
	if c.Accounts != nil {
		out.Accounts = maps.Clone(c.Accounts)
	}
	if c.Devices != nil {
		out.Devices = maps.Clone(c.Devices)
	}
	if c.DefaultDevices != nil {
		out.DefaultDevices = slices.Clone(c.DefaultDevices)
	}
	if c.Channels != nil {
		out.Channels = make(map[string]ChannelMapping, len(c.Channels))
		for k, ch := range c.Channels {
			ch.Devices = slices.Clone(ch.Devices)
			out.Channels[k] = ch
		}
	}
	if c.Emojis != nil {
		out.Emojis = maps.Clone(c.Emojis)
	}
	return out
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

func applyAccountsUpdate(next *Config, upd ConfigUpdate) error {
	if upd.DeleteAccount != "" {
		delID, err := NormalizeAccountID(upd.DeleteAccount)
		if err != nil {
			return err
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
			return err
		}
		cur := next.Accounts[id]
		cur.ID = id
		updated, err := applyAccountUpdate(cur, aUpd)
		if err != nil {
			return err
		}
		next.Accounts[id] = updated
	}
	if hasInlineAccountFields(upd) {
		id, err := NormalizeAccountID(upd.Account)
		if err != nil {
			return err
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
			return err
		}
		next.Accounts[id] = updated
	}
	return nil
}

func applyRoutingUpdates(next *Config, upd ConfigUpdate, activeDevices map[string]bool) error {
	if upd.Devices != nil {
		devs, err := applyDeviceUpdates(upd.Devices, activeDevices)
		if err != nil {
			return err
		}
		next.Devices = devs
	}
	if upd.DefaultDevices != nil {
		defDevs, err := validateDeviceList(*upd.DefaultDevices, activeDevices, "default_devices")
		if err != nil {
			return err
		}
		next.DefaultDevices = defDevs
	}
	if upd.DefaultStrategy != nil {
		strat, err := NormalizeStrategy(*upd.DefaultStrategy)
		if err != nil {
			return err
		}
		next.DefaultStrategy = strat
	}
	if upd.DefaultPolicy != nil {
		pol, err := NormalizePolicy(*upd.DefaultPolicy)
		if err != nil {
			return err
		}
		next.DefaultPolicy = pol
	}
	if upd.Channels != nil {
		ch, err := applyChannelUpdates(upd.Channels, activeDevices)
		if err != nil {
			return err
		}
		next.Channels = ch
	}
	if upd.Devices != nil || upd.DefaultDevices != nil || upd.Channels != nil {
		return validateChannelRoutes(*next)
	}
	return nil
}

func validateChannelRoutes(cfg Config) error {
	for _, ch := range cfg.Channels {
		targets := cfg.EffectiveChannelDevices(ch)
		if len(targets) == 0 {
			return newSlackerErr(fmt.Sprintf("channel %s requires at least one device or default_devices", ch.ChannelID), 400)
		}
		for _, dev := range targets {
			if _, err := cfg.ResolveRepoPath(dev, ch.RepoPath); err != nil {
				return newSlackerErr(fmt.Sprintf("channel %s: %s", ch.ChannelID, err.Error()), 400)
			}
		}
	}
	return nil
}

// ApplyUpdate merges upd onto prev, validating accounts, token prefixes, devices, workspace dirs, routing defaults, repo paths, and harness mappings.
func ApplyUpdate(prev Config, upd ConfigUpdate, activeDevices map[string]bool) (Config, error) {
	next := cloneConfig(prev)
	normalizeLoadedConfig(&next)

	if err := applyAccountsUpdate(&next, upd); err != nil {
		return prev, err
	}
	if err := applyRoutingUpdates(&next, upd, activeDevices); err != nil {
		return prev, err
	}
	if upd.Emojis != nil {
		em, err := applyEmojiUpdates(upd.Emojis)
		if err != nil {
			return prev, err
		}
		next.Emojis = em
	}
	next.UpdatedAt = registry.NowISO()
	return next, nil
}
