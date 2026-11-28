package assistant

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/paths"
)

// DoctorCheck describes one verification step in DoctorReport.
type DoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok, warn, fail, skip
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

// DoctorReport summarizes lighthouse readiness across OpenRouter, Vault, Obsidian Sync, 1Password, and MCP.
type DoctorReport struct {
	OK        bool          `json:"ok"`
	CheckedAt string        `json:"checked_at"`
	Checks    []DoctorCheck `json:"checks"`
}

// DoctorProbes allows tests to override local OS/binary checks.
type DoctorProbes struct {
	FindOB       func() string
	OBRunning    func() bool
	OBRunningFor func(name, vaultPath string) bool
	CheckOP      func(context.Context, string) (string, bool, string, error)
	ListVaults   func(context.Context, string) ([]OPVaultMeta, error)
	SandboxProbe func(context.Context, SandboxConfig) DoctorCheck
}

// FindOBBinary locates `ob` (obsidian-headless) across PATH, Homebrew, npm/nvm/fnm/volta/mise/pnpm, and login shell PATH.
func FindOBBinary() string {
	if p, err := exec.LookPath("ob"); err == nil {
		return p
	}
	cands := []string{"/opt/homebrew/bin/ob", "/usr/local/bin/ob"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cands = append(cands,
			filepath.Join(home, ".volta", "bin", "ob"),
			filepath.Join(home, ".local", "share", "mise", "shims", "ob"),
			filepath.Join(home, ".asdf", "shims", "ob"),
			filepath.Join(home, "Library", "pnpm", "ob"),
			filepath.Join(home, ".npm-global", "bin", "ob"),
			filepath.Join(home, ".local", "bin", "ob"),
		)
		for _, glob := range []string{
			filepath.Join(home, ".nvm", "versions", "node", "*", "bin", "ob"),
			filepath.Join(home, ".local", "share", "fnm", "node-versions", "*", "installation", "bin", "ob"),
		} {
			if matches, gErr := filepath.Glob(glob); gErr == nil && len(matches) > 0 {
				sort.Strings(matches)
				cands = append(cands, matches[len(matches)-1])
			}
		}
		if shellPath, pErr := paths.LoginShellPath(context.Background(), "", home, ""); pErr == nil && shellPath != "" {
			for dir := range strings.SplitSeq(shellPath, string(os.PathListSeparator)) {
				if dir != "" {
					cands = append(cands, filepath.Join(dir, "ob"))
				}
			}
		}
	}
	for _, cand := range cands {
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand
		}
	}
	return ""
}

func defaultOBRunningFor(name, vaultPath string) bool {
	label := paths.ObsidianSyncLabelFor(name)
	if exec.Command("launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), label)).Run() == nil {
		return true
	}
	if name == "" || name == PrimaryVaultName {
		if exec.Command("pgrep", "-f", "ob sync").Run() == nil {
			return true
		}
		return exec.Command("pgrep", "-f", "obsidian-headless").Run() == nil
	}
	return vaultPath != "" && exec.Command("pgrep", "-f", vaultPath).Run() == nil
}

// Doctor verifies all assistant subsystems on the lighthouse and returns actionable hints.
func (e *Engine) Doctor(ctx context.Context) DoctorReport {
	cfg := e.Config()
	view := e.ConfigView()

	checks := []DoctorCheck{
		checkConfigDoctor(cfg, view),
	}
	opCheck := e.checkOPDoctor(ctx, cfg)
	rtCfg, rtErr := e.resolveRuntimeConfig(ctx)
	checks = append(checks, e.checkOpenRouterDoctor(ctx, cfg, rtCfg, rtErr))
	checks = append(checks, e.checkDecisionsDoctor(ctx, rtCfg, rtErr))
	checks = append(checks, e.checkVaultDoctor(view.EffectiveVaultPath))
	checks = append(checks, e.checkObsidianSyncForDoctor("", view.EffectiveVaultPath))
	for _, m := range cfg.ExtraVaults {
		resolved := ResolveVaultRoot(m.Path, "")
		checks = append(checks, checkExtraVaultDoctor(m, resolved))
		checks = append(checks, e.checkObsidianSyncForDoctor(m.Name, resolved))
	}
	checks = append(checks, opCheck)
	checks = append(checks, e.checkMCPDoctor(ctx, cfg))
	checks = append(checks, e.checkNotificationsDoctor(ctx, cfg))
	checks = append(checks, e.checkDiscordBotDoctor(ctx, cfg))
	checks = append(checks, e.checkSandboxDoctor(ctx, cfg))

	ok := true
	for _, c := range checks {
		if c.Status == "fail" {
			ok = false
			break
		}
	}
	return DoctorReport{
		OK:        ok,
		CheckedAt: time.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
		Checks:    checks,
	}
}

func checkConfigDoctor(cfg Config, view ConfigView) DoctorCheck {
	if !view.Configured {
		return DoctorCheck{
			Name:   "config",
			Status: "fail",
			Detail: "No OpenRouter API key or api_key_op_ref configured",
			Hint:   "Run `fleet assistant setup --op-ref \"op://Vault/OpenRouter/credential\"` or `--api-key-op-ref ...`",
		}
	}
	authMode := "stored api_key"
	if cfg.APIKey == "" && cfg.APIKeyOPRef != "" {
		authMode = "1Password ref (" + cfg.APIKeyOPRef + ")"
	}
	return DoctorCheck{
		Name:   "config",
		Status: "ok",
		Detail: fmt.Sprintf("enabled=%v, zdr=%v, auth=%s, model=%s", cfg.Enabled, cfg.ZDR, authMode, cfg.Models.DefaultChat),
	}
}

func (e *Engine) checkOpenRouterDoctor(ctx context.Context, rawCfg, rtCfg Config, rtErr error) DoctorCheck {
	if rawCfg.APIKey == "" && rawCfg.APIKeyOPRef == "" {
		return DoctorCheck{Name: "openrouter", Status: "skip", Detail: "Skipped (not configured)"}
	}
	if rtErr != nil {
		return DoctorCheck{
			Name:   "openrouter",
			Status: "fail",
			Detail: "Failed to resolve api_key_op_ref: " + rtErr.Error(),
			Hint:   "Check 1Password Service Account token (`fleet assistant op-setup`)",
		}
	}
	if _, err := e.llm.CompleteText(ctx, rtCfg, rtCfg.Models.Fast, "Reply with OK.", "ping", 16); err != nil {
		return DoctorCheck{
			Name:   "openrouter",
			Status: "fail",
			Detail: err.Error(),
			Hint:   "Verify OpenRouter key, balance, and ZDR availability for " + rtCfg.Models.Fast,
		}
	}
	return DoctorCheck{
		Name:   "openrouter",
		Status: "ok",
		Detail: fmt.Sprintf("Connected (%s, fast=%s, zdr=%v)", rtCfg.BaseURL, rtCfg.Models.Fast, rtCfg.ZDR),
	}
}

func (e *Engine) checkDecisionsDoctor(ctx context.Context, rtCfg Config, rtErr error) DoctorCheck {
	if rtErr != nil || strings.TrimSpace(rtCfg.APIKey) == "" {
		return DoctorCheck{Name: "decisions", Status: "skip", Detail: "Skipped (OpenRouter not ready)"}
	}
	if strings.TrimSpace(rtCfg.Models.Decision) == "" {
		return DoctorCheck{Name: "decisions", Status: "skip", Detail: "No decision model configured"}
	}
	ans, err := e.llm.Decide(ctx, rtCfg, "ping", []DecisionQuestion{{
		ID:           "tier",
		Type:         "choice",
		Instructions: "Choose the model tier needed for this request.",
		Criteria: map[string]string{
			"fast":     "Simple ping, greeting, or quick lookup",
			"balanced": "Standard assistant turn or tool call",
		},
	}})
	if err != nil {
		return DoctorCheck{
			Name:   "decisions",
			Status: "warn",
			Detail: fmt.Sprintf("%s failed (%v); chat falls back to %s", rtCfg.Models.Decision, err, rtCfg.Models.DefaultChat),
		}
	}
	return DoctorCheck{
		Name:   "decisions",
		Status: "ok",
		Detail: fmt.Sprintf("%s ready (sample=%s)", rtCfg.Models.Decision, cmp.Or(ans["tier"].Value, "ok")),
	}
}

func (e *Engine) checkVaultDoctor(vaultPath string) DoctorCheck {
	v, err := OpenVault(vaultPath)
	if err != nil {
		return DoctorCheck{Name: "vault", Status: "fail", Detail: err.Error(), Hint: "Check vault_path permissions on lighthouse"}
	}
	skills, _ := v.ListSkills()
	_, gitErr := os.Stat(filepath.Join(v.Root, ".git"))
	gitState := "no git"
	if gitErr == nil {
		gitState = "git-backed"
	}
	return DoctorCheck{
		Name:   "vault",
		Status: "ok",
		Detail: fmt.Sprintf("%s (%d skill(s), %s)", v.Root, len(skills), gitState),
	}
}

func checkExtraVaultDoctor(m VaultMount, resolved string) DoctorCheck {
	name := "vault:" + m.Name
	v, err := OpenVaultNoScaffold(resolved)
	if err != nil {
		return DoctorCheck{
			Name:   name,
			Status: "fail",
			Detail: err.Error(),
			Hint:   fmt.Sprintf("Create/sync %s (e.g. `ob sync-setup` inside it) or update extra_vaults", resolved),
		}
	}
	policy := "approval-gated writes"
	if m.ReadOnly {
		policy = "read-only"
	} else if len(m.AutoWriteDirs) > 0 {
		policy = "auto-write: " + strings.Join(m.AutoWriteDirs, ", ")
	}
	gitState := "no git"
	if _, gitErr := os.Stat(filepath.Join(v.Root, ".git")); gitErr == nil {
		gitState = "git-backed"
	}
	return DoctorCheck{
		Name:   name,
		Status: "ok",
		Detail: fmt.Sprintf("%s (%s, %s)", v.Root, policy, gitState),
	}
}

func (e *Engine) checkObsidianSyncForDoctor(name, vaultPath string) DoctorCheck {
	cleanName := strings.ToLower(strings.TrimSpace(name))
	checkName := "obsidian_sync"
	installCmd := "fleet assistant sync-install"
	if cleanName != "" && cleanName != PrimaryVaultName {
		checkName = "obsidian_sync:" + cleanName
		installCmd = fmt.Sprintf("fleet assistant sync-install --name %s --vault %s", cleanName, vaultPath)
	}
	findOB := e.Probes.FindOB
	if findOB == nil {
		findOB = FindOBBinary
	}
	running := false
	switch {
	case e.Probes.OBRunningFor != nil:
		running = e.Probes.OBRunningFor(cleanName, vaultPath)
	case (cleanName == "" || cleanName == PrimaryVaultName) && e.Probes.OBRunning != nil:
		running = e.Probes.OBRunning()
	default:
		running = defaultOBRunningFor(cleanName, vaultPath)
	}
	obBin := findOB()
	if obBin == "" {
		return DoctorCheck{
			Name:   checkName,
			Status: "warn",
			Detail: "obsidian-headless (`ob`) CLI not found on lighthouse (local vault still works)",
			Hint:   "To sync with Obsidian Sync: `npm install -g obsidian-headless && ob login && ob sync-setup --vault \"<Name>\" && " + installCmd + "`",
		}
	}
	_, obsErr := os.Stat(filepath.Join(vaultPath, ".obsidian"))
	if obsErr != nil {
		return DoctorCheck{
			Name:   checkName,
			Status: "warn",
			Detail: fmt.Sprintf("ob installed (%s), but %s is not linked to an Obsidian vault yet", obBin, vaultPath),
			Hint:   fmt.Sprintf("Run `cd %s && ob login && ob sync-setup --vault \"<Name>\" && %s`", vaultPath, installCmd),
		}
	}
	if !running {
		return DoctorCheck{
			Name:   checkName,
			Status: "warn",
			Detail: fmt.Sprintf("vault linked at %s, but `ob sync --continuous` is not running", vaultPath),
			Hint:   fmt.Sprintf("Run `%s` on the lighthouse to run continuous Obsidian Sync", installCmd),
		}
	}
	return DoctorCheck{
		Name:   checkName,
		Status: "ok",
		Detail: fmt.Sprintf("ob sync running (%s -> %s)", obBin, vaultPath),
	}
}

func opRequiredByConfig(cfg Config) bool {
	if strings.TrimSpace(cfg.APIKeyOPRef) != "" || len(cfg.Credentials) > 0 {
		return true
	}
	if cfg.Discord.Enabled && strings.TrimSpace(cfg.Discord.BotTokenOPRef) != "" {
		return true
	}
	if cfg.Notifications.Enabled && (strings.TrimSpace(cfg.Notifications.DiscordWebhookOPRef) != "" || len(cfg.Notifications.DiscordChannelOPRefs) > 0) {
		return true
	}
	for _, s := range cfg.MCPServers {
		if s.Enabled && (len(s.EnvRefs) > 0 || len(s.HeaderRefs) > 0) {
			return true
		}
	}
	return false
}

func (e *Engine) checkOPDoctor(ctx context.Context, cfg Config) DoctorCheck {
	needed := opRequiredByConfig(cfg)
	checkFn := e.Probes.CheckOP
	if checkFn == nil {
		checkFn = CheckOPHealth
	}
	binPath, hasToken, identity, err := checkFn(ctx, e.opTokenPath)
	if err == nil {
		return DoctorCheck{
			Name:   "onepassword",
			Status: "ok",
			Detail: fmt.Sprintf("op authenticated (%s, %s)", cmp.Or(binPath, "op"), identity),
		}
	}
	status := "warn"
	if needed {
		status = "fail"
	}
	hint := "Install `brew install 1password-cli` on lighthouse and run `fleet assistant op-setup` from an admin Mac"
	if binPath != "" && !hasToken {
		hint = "Run `fleet assistant op-setup` to install a scoped OP_SERVICE_ACCOUNT_TOKEN on the lighthouse"
	}
	return DoctorCheck{
		Name:   "onepassword",
		Status: status,
		Detail: err.Error(),
		Hint:   hint,
	}
}

func (e *Engine) checkMCPDoctor(ctx context.Context, cfg Config) DoctorCheck {
	enabled := 0
	for _, s := range cfg.MCPServers {
		if s.Enabled {
			enabled++
		}
	}
	if enabled == 0 {
		return DoctorCheck{Name: "mcp_servers", Status: "ok", Detail: "In-process Fleet/Vault/Skill tools active (0 external stdio servers)"}
	}
	tools := e.mcp.ListTools(ctx)
	if len(tools) == 0 {
		return DoctorCheck{
			Name:   "mcp_servers",
			Status: "warn",
			Detail: fmt.Sprintf("%d external MCP server(s) enabled, 0 tools discovered", enabled),
			Hint:   "Check command path and env_refs for configured MCP servers",
		}
	}
	return DoctorCheck{
		Name:   "mcp_servers",
		Status: "ok",
		Detail: fmt.Sprintf("%d external MCP server(s) enabled, %d tool(s) ready", enabled, len(tools)),
	}
}

func (e *Engine) checkNotificationsDoctor(ctx context.Context, cfg Config) DoctorCheck {
	n := normalizeNotificationsConfig(cfg.Notifications)
	if !n.Enabled {
		return DoctorCheck{Name: "notifications", Status: "skip", Detail: "Outbound Discord webhook notifications disabled"}
	}
	if n.DiscordWebhookOPRef == "" && len(n.DiscordChannelOPRefs) == 0 {
		return DoctorCheck{
			Name:   "notifications",
			Status: "fail",
			Detail: "notifications.enabled=true but no discord_webhook_op_ref configured",
			Hint:   "Set discord_webhook_op_ref (e.g. op://Fleet-Agent/Discord/webhook) in Settings or `fleet assistant setup`",
		}
	}
	reader := e.opReader
	if reader == nil {
		reader = defaultOPReader
	}
	refs := make([]string, 0, 1+len(n.DiscordChannelOPRefs))
	if n.DiscordWebhookOPRef != "" {
		refs = append(refs, n.DiscordWebhookOPRef)
	}
	for _, r := range n.DiscordChannelOPRefs {
		refs = append(refs, r)
	}
	for _, ref := range refs {
		val, err := reader(ctx, ref)
		if err != nil {
			return DoctorCheck{
				Name:   "notifications",
				Status: "fail",
				Detail: fmt.Sprintf("Failed to resolve %s: %v", ref, err),
				Hint:   "Verify 1Password Service Account access to the Discord webhook item",
			}
		}
		if err := validateDiscordWebhookURL(val, false); err != nil {
			return DoctorCheck{
				Name:   "notifications",
				Status: "fail",
				Detail: fmt.Sprintf("%s: %s", ref, scrubSecret(err.Error(), val)),
			}
		}
	}
	return DoctorCheck{
		Name:   "notifications",
		Status: "ok",
		Detail: fmt.Sprintf("Discord webhook ready (%d ref(s), min_priority=%s)", len(refs), n.MinPriority),
	}
}

func (e *Engine) checkDiscordBotDoctor(ctx context.Context, cfg Config) DoctorCheck {
	d := normalizeDiscordConfig(cfg.Discord)
	if !d.Enabled {
		return DoctorCheck{Name: "discord_bot", Status: "skip", Detail: "Two-way Discord Gateway bot disabled"}
	}
	if err := ValidateDiscordConfig(d); err != nil {
		return DoctorCheck{Name: "discord_bot", Status: "fail", Detail: err.Error()}
	}
	tok, err := e.resolveDiscordBotToken(ctx, d)
	if err != nil {
		return DoctorCheck{
			Name:   "discord_bot",
			Status: "fail",
			Detail: "Failed to resolve bot_token_op_ref: " + err.Error(),
			Hint:   "Verify 1Password Service Account access to the Discord bot token item",
		}
	}
	api := e.discordClient()
	user, err := api.GetMe(ctx, tok)
	if err != nil {
		return DoctorCheck{
			Name:   "discord_bot",
			Status: "fail",
			Detail: err.Error(),
			Hint:   "Verify the Discord bot token and Gateway Message Content Intent",
		}
	}
	autoSync := d.AutoSyncChannels == nil || *d.AutoSyncChannels
	guildID := d.GuildID
	if autoSync && guildID == "" {
		guilds, gErr := api.GetCurrentUserGuilds(ctx, tok)
		if gErr == nil {
			switch len(guilds) {
			case 0:
				return DoctorCheck{
					Name:   "discord_bot",
					Status: "warn",
					Detail: fmt.Sprintf("@%s verified, but bot is in 0 Discord servers yet", cmp.Or(user.Username, user.ID)),
					Hint:   "Invite the bot to your Discord server with Manage Channels permission, then run `fleet assistant discord-sync`",
				}
			case 1:
				guildID = guilds[0].ID
			default:
				return DoctorCheck{
					Name:   "discord_bot",
					Status: "warn",
					Detail: fmt.Sprintf("@%s is in %d servers; set discord.guild_id for auto-sync", cmp.Or(user.Username, user.ID), len(guilds)),
					Hint:   "Pass `--discord-guild-id <ID>` to `fleet assistant setup` or set Server ID in Settings",
				}
			}
		}
	}
	e.discordMu.RLock()
	connected := e.discordConnected
	e.discordMu.RUnlock()
	return DoctorCheck{
		Name:   "discord_bot",
		Status: "ok",
		Detail: fmt.Sprintf("@%s (user=%s, guild=%s, %d channel(s), auto_sync=%v, gateway_connected=%v)", cmp.Or(user.Username, user.ID), d.AuthorizedUserID, cmp.Or(guildID, "auto"), len(d.Channels), autoSync, connected),
	}
}

func (e *Engine) checkSandboxDoctor(ctx context.Context, cfg Config) DoctorCheck {
	sb := normalizeSandboxConfig(cfg.Sandbox)
	if !sb.Enabled {
		return DoctorCheck{Name: "sandbox", Status: "skip", Detail: "Sandboxed computer disabled"}
	}
	if e.Probes.SandboxProbe != nil {
		return e.Probes.SandboxProbe(ctx, sb)
	}
	var runner SandboxRunner
	if e.sandbox != nil && e.sandbox.runner != nil {
		runner = e.sandbox.runner
	} else {
		runner = NewAppleContainerRunner()
	}
	if runner.FindBinary() == "" {
		return DoctorCheck{
			Name:   "sandbox",
			Status: "fail",
			Detail: "Apple `container` CLI not found",
			Hint:   "Install with `brew install container` on macOS 26+",
		}
	}
	if err := runner.SystemStatus(ctx); err != nil {
		return DoctorCheck{
			Name:   "sandbox",
			Status: "fail",
			Detail: err.Error(),
			Hint:   "Run `container system kernel set --recommended && brew services start container` (or `container system start`) on the lighthouse",
		}
	}
	if !runner.NetworkExists(ctx, sb.Network) {
		return DoctorCheck{
			Name:   "sandbox",
			Status: "warn",
			Detail: fmt.Sprintf("Internal network %q not created yet", sb.Network),
			Hint:   "Run `fleet assistant sandbox-build` to create the offline network and build the sandbox image",
		}
	}
	if !runner.ImageExists(ctx, sb.Image) {
		return DoctorCheck{
			Name:   "sandbox",
			Status: "fail",
			Detail: fmt.Sprintf("Sandbox image %q not built yet", sb.Image),
			Hint:   "Run `fleet assistant sandbox-build` (or click Build Sandbox Image in Settings)",
		}
	}
	return DoctorCheck{
		Name:   "sandbox",
		Status: "ok",
		Detail: fmt.Sprintf("Apple container ready (%s, net=%s, %d CPU, %d MB)", sb.Image, sb.Network, sb.CPUs, sb.MemoryMB),
	}
}
