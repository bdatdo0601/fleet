package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bdatdo0601/fleet/internal/assistant"
	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/system"
)

func assistantUsageText() string {
	return `usage: fleet assistant <subcommand> [options]

Subcommands:
  setup           configure OpenRouter, vault path, and models (--check runs doctor)
  doctor          verify OpenRouter, Decisions, Vault, obsidian-headless, 1Password, and MCP on lighthouse
  op-setup        send a 1Password Service Account token to the lighthouse (~/.fleet/op-service-account.token)
  sync-install    (lighthouse) install & start obsidian-headless LaunchAgent (--name for extra vaults)
  sync-uninstall  (lighthouse) stop & remove obsidian-headless LaunchAgent (--name for extra vaults)
  status          show assistant status, models, and pending counts
  ask             send a prompt to an assistant channel
  approvals       list pending or recent tool approvals
  approve         authorize a pending tool call by ID
  deny            reject a pending tool call by ID
  timers          list scheduled timers
  notify-test     send a test Discord webhook notification via 1Password ref
  discord-sync    auto-create/adopt Discord channels under Fleet Assistant and save mappings
  sandbox-install (lighthouse) install & start gui/<uid> sandbox LaunchAgent bridge for Apple container
  sandbox-uninstall (lighthouse) stop & remove sandbox LaunchAgent bridge
  sandbox-build   build the sandbox OCI image and offline network on lighthouse
  sandbox-reset   stop/remove a channel's sandbox micro-VM (optionally clearing /work/out)
  mcp-auth        authorize or revoke OAuth 2.0/2.1 for an HTTP MCP server (e.g. Google Workspace)
`
}

func cmdAssistant(args []string, cio *IO) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		cio.printf("%s", assistantUsageText())
		return nil
	}
	handlers := map[string]func([]string, *IO) error{
		"setup":             cmdAssistantSetup,
		"doctor":            cmdAssistantDoctor,
		"op-setup":          cmdAssistantOPSetup,
		"sync-install":      cmdAssistantSyncInstall,
		"sync-uninstall":    cmdAssistantSyncUninstall,
		"status":            cmdAssistantStatus,
		"ask":               cmdAssistantAsk,
		"approvals":         cmdAssistantApprovals,
		"approve":           func(a []string, c *IO) error { return cmdAssistantDecide(a, true, c) },
		"deny":              func(a []string, c *IO) error { return cmdAssistantDecide(a, false, c) },
		"timers":            cmdAssistantTimers,
		"notify-test":       cmdAssistantNotifyTest,
		"discord-sync":      cmdAssistantDiscordSync,
		"sandbox-install":   cmdAssistantSandboxInstall,
		"sandbox-uninstall": cmdAssistantSandboxUninstall,
		"sandbox-agent":     cmdAssistantSandboxAgent,
		"sandbox-build":     cmdAssistantSandboxBuild,
		"sandbox-reset":     cmdAssistantSandboxReset,
		"mcp-auth":          cmdAssistantMCPAuth,
	}
	sub, rest := args[0], args[1:]
	fn, ok := handlers[sub]
	if !ok {
		return fmt.Errorf("unknown assistant subcommand %q\n\n%s", sub, assistantUsageText())
	}
	return fn(rest, cio)
}

type assistantSetupFlags struct {
	opRef             string
	apiKeyOPRef       string
	apiKey            string
	vaultPath         string
	model             string
	routingMode       string
	decisionModel     string
	observerModel     string
	zdr               bool
	enable            bool
	discordRef        string
	fleetURL          string
	discordBotRef     string
	discordUserID     string
	discordGuildID    string
	discordCategory   string
	discordNoAutoSync bool
	discordChPairs    []string
	sandboxEnable     bool
	sandboxDisable    bool
}

func applyAssistantSetupFlags(cfg *assistant.Config, opts assistantSetupFlags, cio *IO) error {
	cfg.Enabled = opts.enable
	cfg.ZDR = opts.zdr
	switch {
	case opts.apiKeyOPRef != "":
		cfg.APIKeyOPRef = strings.TrimSpace(opts.apiKeyOPRef)
		cfg.APIKey = ""
	case opts.opRef != "":
		secret, err := cio.readOPRef(opts.opRef)
		if err != nil {
			return err
		}
		cfg.APIKey = secret
		cfg.APIKeyOPRef = ""
	case opts.apiKey != "":
		cfg.APIKey = strings.TrimSpace(opts.apiKey)
		cfg.APIKeyOPRef = ""
	}
	if opts.vaultPath != "" {
		cfg.VaultPath = opts.vaultPath
	}
	if opts.model != "" {
		cfg.Models.DefaultChat = opts.model
	}
	if opts.routingMode != "" {
		m := strings.ToLower(strings.TrimSpace(opts.routingMode))
		switch m {
		case assistant.RoutingModeHybrid, assistant.RoutingModeGate, assistant.RoutingModeAuto, assistant.RoutingModeOff:
			cfg.Routing.Mode = m
		default:
			return fmt.Errorf("invalid --routing-mode %q (expected hybrid, gate, auto, or off)", opts.routingMode)
		}
	}
	if opts.decisionModel != "" {
		cfg.Models.Decision = opts.decisionModel
	}
	if opts.observerModel != "" {
		cfg.Models.Observer = opts.observerModel
		cfg.Models.Reflector = opts.observerModel
	}
	if opts.discordRef != "" {
		cfg.Notifications.DiscordWebhookOPRef = strings.TrimSpace(opts.discordRef)
		cfg.Notifications.Enabled = true
	}
	if opts.fleetURL != "" {
		cfg.Notifications.FleetBaseURL = strings.TrimSpace(opts.fleetURL)
		cfg.Discord.FleetBaseURL = strings.TrimSpace(opts.fleetURL)
	}
	if opts.discordBotRef != "" {
		cfg.Discord.BotTokenOPRef = strings.TrimSpace(opts.discordBotRef)
		cfg.Discord.Enabled = true
	}
	if opts.discordUserID != "" {
		cfg.Discord.AuthorizedUserID = strings.TrimSpace(opts.discordUserID)
	}
	if opts.discordGuildID != "" {
		cfg.Discord.GuildID = strings.TrimSpace(opts.discordGuildID)
	}
	if opts.discordCategory != "" {
		cfg.Discord.CategoryName = strings.TrimSpace(opts.discordCategory)
	}
	if opts.discordNoAutoSync {
		f := false
		cfg.Discord.AutoSyncChannels = &f
	}
	if err := applyDiscordChannelPairs(cfg, opts.discordChPairs); err != nil {
		return err
	}
	if opts.sandboxEnable {
		cfg.Sandbox.Enabled = true
	} else if opts.sandboxDisable {
		cfg.Sandbox.Enabled = false
	}
	return nil
}

func applyDiscordChannelPairs(cfg *assistant.Config, pairs []string) error {
	if len(pairs) == 0 {
		return nil
	}
	if cfg.Discord.Channels == nil {
		cfg.Discord.Channels = make(map[string]string)
	}
	for _, pair := range pairs {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return fmt.Errorf("invalid --discord-channel %q (expected fleet_ch=discord_id)", pair)
		}
		cfg.Discord.Channels[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(k, "#")))] = strings.TrimSpace(v)
	}
	return nil
}

func cmdAssistantSetup(args []string, cio *IO) error {
	fs := flag.NewFlagSet("assistant setup", flag.ContinueOnError)
	checkOnly := fs.Bool("check", false, "run lighthouse doctor check without modifying config")
	noTest := fs.Bool("no-test", false, "skip live connection test")
	var opts assistantSetupFlags
	fs.StringVar(&opts.opRef, "op-ref", "", "resolve OpenRouter API key once from local 1Password ref")
	fs.StringVar(&opts.apiKeyOPRef, "api-key-op-ref", "", "store 1Password op:// ref on lighthouse (resolved in memory via Service Account)")
	fs.StringVar(&opts.apiKey, "api-key", "", "OpenRouter API key")
	fs.StringVar(&opts.vaultPath, "vault", "", "Obsidian vault path on lighthouse")
	fs.StringVar(&opts.model, "model", "", "default chat model")
	fs.StringVar(&opts.routingMode, "routing-mode", "", "auto frontier routing mode (hybrid|gate|auto|off)")
	fs.StringVar(&opts.decisionModel, "decision-model", "", "decision model slug")
	fs.StringVar(&opts.observerModel, "observer-model", "", "observer/reflector model slug")
	fs.BoolVar(&opts.zdr, "zdr", true, "enforce OpenRouter Zero Data Retention")
	fs.BoolVar(&opts.enable, "enable", true, "enable assistant background scheduler")
	fs.StringVar(&opts.discordRef, "discord-webhook-op-ref", "", "1Password op:// ref for outbound Discord webhook notifications")
	fs.StringVar(&opts.fleetURL, "fleet-url", "", "Fleet Console base URL used in Discord deep links")
	fs.StringVar(&opts.discordBotRef, "discord-bot-token-op-ref", "", "1Password op:// ref for two-way Discord Gateway bot token")
	fs.StringVar(&opts.discordUserID, "discord-user-id", "", "authorized Discord user snowflake ID for two-way bot")
	fs.StringVar(&opts.discordGuildID, "discord-guild-id", "", "optional Discord server ID (auto-detected if bot is in one server)")
	fs.StringVar(&opts.discordCategory, "discord-category", "", "Discord category name for auto-synced channels (default: Fleet Assistant)")
	fs.BoolVar(&opts.discordNoAutoSync, "discord-no-auto-sync", false, "disable automatic Discord channel creation/sync")
	fs.Func("discord-channel", "map fleet channel to Discord channel ID (fleet_ch=discord_id, repeatable)", func(v string) error {
		opts.discordChPairs = append(opts.discordChPairs, v)
		return nil
	})
	fs.BoolVar(&opts.sandboxEnable, "sandbox-enable", false, "enable the sandboxed computer runtime")
	fs.BoolVar(&opts.sandboxDisable, "sandbox-disable", false, "disable the sandboxed computer runtime")
	if _, err := parseFlagsInterspersed(fs, args); err != nil {
		return err
	}
	if *checkOnly {
		return cmdAssistantDoctor(nil, cio)
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var curResp struct {
		Config assistant.Config `json:"config"`
	}
	if err := client.Call(api, http.MethodGet, "/v1/assistant/config", nil, 15*time.Second, nil, &curResp); err != nil {
		return err
	}
	cfg := assistant.NormalizeConfig(curResp.Config)
	if err := applyAssistantSetupFlags(&cfg, opts, cio); err != nil {
		return err
	}
	req := map[string]any{
		"enabled":                    cfg.Enabled,
		"base_url":                   cfg.BaseURL,
		"api_key":                    cfg.APIKey,
		"api_key_op_ref":             cfg.APIKeyOPRef,
		"clear_api_key":              cfg.APIKey == "" && cfg.APIKeyOPRef != "",
		"zdr":                        cfg.ZDR,
		"data_collection":            cfg.DataCollection,
		"vault_path":                 cfg.VaultPath,
		"auto_write_dirs":            cfg.AutoWriteDirs,
		"git_auto_commit":            cfg.GitAutoCommit,
		"heartbeat_interval_minutes": cfg.HeartbeatIntervalMinutes,
		"observer_threshold_tokens":  cfg.ObserverThresholdTokens,
		"reflector_threshold_bytes":  cfg.ReflectorThresholdBytes,
		"models":                     cfg.Models,
		"routing":                    cfg.Routing,
		"notifications":              cfg.Notifications,
		"discord":                    cfg.Discord,
		"sandbox":                    cfg.Sandbox,
		"test":                       !*noTest && (cfg.APIKey != "" || cfg.APIKeyOPRef != ""),
	}
	var out struct {
		Config assistant.ConfigView `json:"config"`
	}
	if err := client.Call(api, http.MethodPost, "/v1/assistant/config", req, 30*time.Second, nil, &out); err != nil {
		return err
	}
	cio.info(fmt.Sprintf("assistant configured (enabled=%v model=%s vault=%s zdr=%v)", out.Config.Enabled, out.Config.Models.DefaultChat, out.Config.EffectiveVaultPath, out.Config.ZDR))
	if out.Config.Discord.Enabled && (out.Config.Discord.AutoSyncChannels == nil || *out.Config.Discord.AutoSyncChannels) {
		var syncOut struct {
			OK   bool                        `json:"ok"`
			Sync assistant.DiscordSyncResult `json:"sync"`
		}
		if sErr := client.Call(api, http.MethodPost, "/v1/assistant/discord/sync", map[string]any{}, 20*time.Second, nil, &syncOut); sErr != nil {
			cio.printf("warning: discord channel auto-sync skipped: %v\n", sErr)
		} else {
			cio.info(fmt.Sprintf("discord channels synced (guild=%s category=%q mapped=%d created=%d adopted=%d)", syncOut.Sync.GuildID, syncOut.Sync.CategoryName, len(syncOut.Sync.Channels), len(syncOut.Sync.CreatedInDiscord), len(syncOut.Sync.AdoptedInDiscord)))
		}
	}
	return nil
}

func cmdAssistantDoctor(args []string, cio *IO) error {
	fs := flag.NewFlagSet("assistant doctor", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "output JSON")
	if _, err := parseFlagsInterspersed(fs, args); err != nil {
		return err
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var rep assistant.DoctorReport
	if err := client.Call(api, http.MethodGet, "/v1/assistant/doctor", nil, 30*time.Second, nil, &rep); err != nil {
		return err
	}
	if *jsonOut {
		return writeJSONOut(cio.Stdout, rep)
	}
	for _, c := range rep.Checks {
		cio.printf("[%-4s] %-14s %s\n", strings.ToUpper(c.Status), c.Name, c.Detail)
		if c.Hint != "" && c.Status != "ok" {
			cio.printf("       hint: %s\n", c.Hint)
		}
	}
	if !rep.OK {
		return errors.New("assistant doctor reported one or more failing checks")
	}
	return nil
}

func cmdAssistantOPSetup(args []string, cio *IO) error {
	fs := flag.NewFlagSet("assistant op-setup", flag.ContinueOnError)
	opRef := fs.String("op-ref", "", "read service account token from a 1Password op:// reference")
	fromStdin := fs.Bool("token-stdin", false, "read service account token from stdin")
	clearTok := fs.Bool("clear", false, "remove stored service account token from lighthouse")
	if _, err := parseFlagsInterspersed(fs, args); err != nil {
		return err
	}
	var token string
	switch {
	case *clearTok:
		token = ""
	case *opRef != "":
		val, err := cio.readOPRef(*opRef)
		if err != nil {
			return err
		}
		token = val
	case *fromStdin:
		b, err := io.ReadAll(cio.Stdin)
		if err != nil {
			return err
		}
		token = strings.TrimSpace(string(b))
	default:
		val, err := cio.askSecret("1Password Service Account token (ops_...): ")
		if err != nil {
			return err
		}
		token = val
	}
	if !*clearTok && token == "" {
		return errors.New("service account token is required")
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var res struct {
		OK    bool                  `json:"ok"`
		Check assistant.DoctorCheck `json:"check"`
	}
	if err := client.Call(api, http.MethodPost, "/v1/assistant/op-token", map[string]any{
		"token": token,
		"clear": *clearTok,
	}, 15*time.Second, nil, &res); err != nil {
		return err
	}
	cio.printf("[%-4s] %s: %s\n", strings.ToUpper(res.Check.Status), res.Check.Name, res.Check.Detail)
	if !*clearTok && !res.OK {
		if res.Check.Hint != "" {
			cio.printf("hint: %s\n", res.Check.Hint)
		}
		return errors.New("lighthouse 1Password verification failed")
	}
	return nil
}

func resolveOBBinary(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("ob binary %q not found: %w", explicit, err)
		}
		return explicit, nil
	}
	if p := assistant.FindOBBinary(); p != "" {
		return p, nil
	}
	return "", errors.New("`ob` (obsidian-headless) not found; install with `npm install -g obsidian-headless`")
}

func cmdAssistantSyncInstall(args []string, cio *IO) error {
	fs := flag.NewFlagSet("assistant sync-install", flag.ContinueOnError)
	mountName := fs.String("name", "", "mounted vault name (omit for primary agent vault; e.g. personal)")
	vaultPath := fs.String("vault", "", "local vault directory (defaults to configured vault path)")
	obBinFlag := fs.String("ob-bin", "", "path to `ob` binary")
	if _, err := parseFlagsInterspersed(fs, args); err != nil {
		return err
	}
	cleanName := strings.ToLower(strings.TrimSpace(*mountName))
	if cleanName != "" && cleanName != assistant.PrimaryVaultName && !assistant.IsValidVaultName(cleanName) {
		return fmt.Errorf("invalid vault name %q (must match [a-z0-9][a-z0-9_-]*)", *mountName)
	}
	obBin, err := resolveOBBinary(*obBinFlag)
	if err != nil {
		return err
	}
	vaultDir := strings.TrimSpace(*vaultPath)
	if vaultDir == "" {
		api, rErr := client.ResolveAPI("")
		if rErr != nil {
			return rErr
		}
		var cfgRes struct {
			Config assistant.ConfigView `json:"config"`
		}
		if err := client.Call(api, http.MethodGet, "/v1/assistant/config", nil, 15*time.Second, nil, &cfgRes); err != nil {
			return err
		}
		if cleanName == "" || cleanName == assistant.PrimaryVaultName {
			vaultDir = cfgRes.Config.EffectiveVaultPath
		} else {
			for _, m := range cfgRes.Config.ExtraVaults {
				if m.Name == cleanName {
					vaultDir = m.Path
					break
				}
			}
			if vaultDir == "" {
				return fmt.Errorf("extra vault %q is not configured yet (pass --vault or add it in extra_vaults)", cleanName)
			}
		}
	}
	vaultDir = assistant.ResolveVaultRoot(vaultDir, filepath.Join(paths.StateDir(), "assistant-vault"))
	if _, err := os.Stat(filepath.Join(vaultDir, ".obsidian")); err != nil {
		return fmt.Errorf("vault %s is not linked to Obsidian Sync yet (missing .obsidian; run `cd %s && ob login && ob sync-setup --vault \"<Name>\"`)", vaultDir, vaultDir)
	}
	label := paths.ObsidianSyncLabelFor(cleanName)
	logName := "obsidian-sync.log"
	if cleanName != "" && cleanName != assistant.PrimaryVaultName {
		logName = fmt.Sprintf("obsidian-sync-%s.log", cleanName)
	}
	logPath := filepath.Join(paths.StateDir(), logName)
	if err := system.InstallUserAgent(system.AgentOpts{
		Label:     label,
		Args:      []string{obBin, "sync", "--continuous"},
		KeepAlive: true,
		LogPath:   logPath,
		WorkDir:   vaultDir,
	}); err != nil {
		return err
	}
	cio.info(fmt.Sprintf("installed & started %s (%s sync --continuous in %s)", label, obBin, vaultDir))
	return nil
}

func cmdAssistantSyncUninstall(args []string, cio *IO) error {
	fs := flag.NewFlagSet("assistant sync-uninstall", flag.ContinueOnError)
	mountName := fs.String("name", "", "mounted vault name (omit for primary agent vault)")
	if _, err := parseFlagsInterspersed(fs, args); err != nil {
		return err
	}
	cleanName := strings.ToLower(strings.TrimSpace(*mountName))
	if cleanName != "" && cleanName != assistant.PrimaryVaultName && !assistant.IsValidVaultName(cleanName) {
		return fmt.Errorf("invalid vault name %q (must match [a-z0-9][a-z0-9_-]*)", *mountName)
	}
	label := paths.ObsidianSyncLabelFor(cleanName)
	if err := system.UninstallUserAgent(label); err != nil {
		return err
	}
	cio.info("uninstalled " + label)
	return nil
}

func cmdAssistantStatus(args []string, cio *IO) error {
	fs := flag.NewFlagSet("assistant status", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "output JSON")
	if _, err := parseFlagsInterspersed(fs, args); err != nil {
		return err
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var res struct {
		Config  assistant.ConfigView    `json:"config"`
		Summary assistant.SummaryCounts `json:"summary"`
	}
	if err := client.Call(api, http.MethodGet, "/v1/assistant/status", nil, 15*time.Second, nil, &res); err != nil {
		return err
	}
	if *jsonOut {
		return writeJSONOut(cio.Stdout, res)
	}
	cio.printf("enabled:           %v (configured=%v, zdr=%v)\n", res.Config.Enabled, res.Config.Configured, res.Config.ZDR)
	cio.printf("vault:             %s\n", res.Config.EffectiveVaultPath)
	for _, m := range res.Config.ExtraVaults {
		mode := "approval-writes"
		if m.ReadOnly {
			mode = "read-only"
		} else if len(m.AutoWriteDirs) > 0 {
			mode = "auto-write:" + strings.Join(m.AutoWriteDirs, ",")
		}
		cio.printf("extra vault %-5s  %s (%s)\n", m.Name+":", m.Path, mode)
	}
	cio.printf("default model:     %s (decision=%s, observer=%s)\n", res.Config.Models.DefaultChat, res.Config.Models.Decision, res.Config.Models.Observer)
	cio.printf("auto routing:      %s (frontier=%s, grant=%d turns / $%.2f)\n", res.Config.Routing.Mode, res.Config.Models.Frontier, res.Config.Routing.GrantMaxTurns, res.Config.Routing.GrantMaxCostUSD)
	cio.printf("sandbox:           enabled=%v image=%s (%d CPU, %d MB, idle=%dm)\n", res.Config.Sandbox.Enabled, res.Config.Sandbox.Image, res.Config.Sandbox.CPUs, res.Config.Sandbox.MemoryMB, res.Config.Sandbox.IdleMinutes)
	cio.printf("channels/timers:   %d channels, %d timers, %d pending approvals\n", res.Summary.Channels, res.Summary.EnabledTimers, res.Summary.PendingApprovals)
	return nil
}

func cmdAssistantAsk(args []string, cio *IO) error {
	fs := flag.NewFlagSet("assistant ask", flag.ContinueOnError)
	ch := fs.String("channel", "general", "target channel")
	model := fs.String("model", "", "optional model override")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	prompt := strings.TrimSpace(strings.Join(pos, " "))
	if prompt == "" {
		return fmt.Errorf("usage: fleet assistant ask [--channel CH] [--model M] PROMPT")
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var out struct {
		Message assistant.Message `json:"message"`
	}
	if err := client.Call(api, http.MethodPost, "/v1/assistant/messages", map[string]string{
		"channel": *ch,
		"content": prompt,
		"model":   *model,
	}, 15*time.Second, nil, &out); err != nil {
		return err
	}
	cio.printf("queued message #%d on #%s\n", out.Message.ID, out.Message.ChannelID)
	return nil
}

func cmdAssistantApprovals(args []string, cio *IO) error {
	fs := flag.NewFlagSet("assistant approvals", flag.ContinueOnError)
	status := fs.String("status", "pending", "filter status")
	if _, err := parseFlagsInterspersed(fs, args); err != nil {
		return err
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var out struct {
		Approvals []assistant.Approval `json:"approvals"`
	}
	if err := client.Call(api, http.MethodGet, "/v1/assistant/approvals?status="+url.QueryEscape(*status), nil, 15*time.Second, nil, &out); err != nil {
		return err
	}
	if len(out.Approvals) == 0 {
		cio.printf("no approvals (%s)\n", *status)
		return nil
	}
	for _, a := range out.Approvals {
		cio.printf("%s  %-8s  %-14s  #%s  %s\n", a.ID, a.Status, a.ToolName, a.ChannelID, a.Summary)
	}
	return nil
}

func cmdAssistantDecide(args []string, approve bool, cio *IO) error {
	fs := flag.NewFlagSet("assistant decide", flag.ContinueOnError)
	note := fs.String("note", "", "optional decision note")
	scope := fs.String("scope", "", "approval scope for frontier escalations (once|topic)")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return fmt.Errorf("approval id is required")
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	dec := "deny"
	if approve {
		dec = "approve"
	}
	var out struct {
		Approval assistant.Approval `json:"approval"`
	}
	if err := client.Call(api, http.MethodPost, "/v1/assistant/approvals/"+url.PathEscape(pos[0])+"/decide", map[string]string{
		"decision": dec,
		"scope":    *scope,
		"note":     *note,
	}, 15*time.Second, nil, &out); err != nil {
		return err
	}
	cio.printf("%s -> %s\n", out.Approval.ID, out.Approval.Status)
	return nil
}

func cmdAssistantTimers(args []string, cio *IO) error {
	fs := flag.NewFlagSet("assistant timers", flag.ContinueOnError)
	if _, err := parseFlagsInterspersed(fs, args); err != nil {
		return err
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var out struct {
		Timers []assistant.Timer `json:"timers"`
	}
	if err := client.Call(api, http.MethodGet, "/v1/assistant/timers", nil, 15*time.Second, nil, &out); err != nil {
		return err
	}
	if len(out.Timers) == 0 {
		cio.printf("no timers scheduled\n")
		return nil
	}
	for _, t := range out.Timers {
		cio.printf("%s  enabled=%v  #%s  next=%s  %s\n", t.ID, t.Enabled, t.ChannelID, t.NextRunAt, t.Name)
	}
	return nil
}

func cmdAssistantNotifyTest(args []string, cio *IO) error {
	fs := flag.NewFlagSet("assistant notify-test", flag.ContinueOnError)
	if _, err := parseFlagsInterspersed(fs, args); err != nil {
		return err
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	if err := client.Call(api, http.MethodPost, "/v1/assistant/notifications/test", map[string]any{}, 15*time.Second, nil, nil); err != nil {
		return err
	}
	cio.info("sent test Discord webhook notification")
	return nil
}

func cmdAssistantDiscordSync(args []string, cio *IO) error {
	fs := flag.NewFlagSet("assistant discord-sync", flag.ContinueOnError)
	if _, err := parseFlagsInterspersed(fs, args); err != nil {
		return err
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var out struct {
		OK   bool                        `json:"ok"`
		Sync assistant.DiscordSyncResult `json:"sync"`
	}
	if err := client.Call(api, http.MethodPost, "/v1/assistant/discord/sync", map[string]any{}, 30*time.Second, nil, &out); err != nil {
		return err
	}
	s := out.Sync
	cio.info(fmt.Sprintf("synced Discord server %s (category=%q mapped=%d created_discord=%v adopted=%v created_fleet=%v)", s.GuildID, s.CategoryName, len(s.Channels), s.CreatedInDiscord, s.AdoptedInDiscord, s.CreatedInFleet))
	return nil
}

func cmdAssistantSandboxBuild(args []string, cio *IO) error {
	fs := flag.NewFlagSet("assistant sandbox-build", flag.ContinueOnError)
	rebuildBase := fs.Bool("rebuild-base", false, "rebuild the base image from scratch")
	if _, err := parseFlagsInterspersed(fs, args); err != nil {
		return err
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var out assistant.SandboxBuildResult
	if err := client.Call(api, http.MethodPost, "/v1/assistant/sandbox/build", map[string]bool{
		"rebuild_base": *rebuildBase,
	}, 10*time.Minute, nil, &out); err != nil {
		return err
	}
	cio.info(fmt.Sprintf("sandbox image ready: %s (base=%s rebuilt=%v net=%s)", out.Image, out.BaseImage, out.BaseRebuilt, out.Network))
	return nil
}

func cmdAssistantSandboxReset(args []string, cio *IO) error {
	fs := flag.NewFlagSet("assistant sandbox-reset", flag.ContinueOnError)
	ch := fs.String("channel", "general", "target channel")
	clearOut := fs.Bool("clear-output", false, "also clear /work/out")
	if _, err := parseFlagsInterspersed(fs, args); err != nil {
		return err
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	if err := client.Call(api, http.MethodPost, "/v1/assistant/sandbox/reset", map[string]any{
		"channel":      *ch,
		"clear_output": *clearOut,
	}, 30*time.Second, nil, nil); err != nil {
		return err
	}
	cio.info(fmt.Sprintf("reset sandbox for #%s (clear_output=%v)", *ch, *clearOut))
	return nil
}

func cmdAssistantSandboxInstall(args []string, cio *IO) error {
	fs := flag.NewFlagSet("assistant sandbox-install", flag.ContinueOnError)
	if _, err := parseFlagsInterspersed(fs, args); err != nil {
		return err
	}
	fleetBin := paths.InstalledBinaryPath()
	if _, err := os.Stat(fleetBin); err != nil {
		if exe, exeErr := os.Executable(); exeErr == nil && exe != "" {
			fleetBin = exe
		}
	}
	stateDir := paths.StateDir()
	sockPath := paths.SandboxSocketPath(stateDir)
	logPath := filepath.Join(stateDir, "sandbox-agent.log")
	if err := system.InstallUserAgent(system.AgentOpts{
		Label:     paths.SandboxAgentLabel,
		Args:      []string{fleetBin, "assistant", "sandbox-agent", "--socket", sockPath},
		KeepAlive: true,
		LogPath:   logPath,
		WorkDir:   stateDir,
	}); err != nil {
		return err
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		err := assistant.PingSandboxAgent(ctx, sockPath)
		cancel()
		if err == nil {
			cio.info(fmt.Sprintf("installed & started %s (socket=%s)", paths.SandboxAgentLabel, sockPath))
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("installed %s, but %s did not become ready (check %s)", paths.SandboxAgentLabel, sockPath, logPath)
}

func cmdAssistantSandboxUninstall(args []string, cio *IO) error {
	fs := flag.NewFlagSet("assistant sandbox-uninstall", flag.ContinueOnError)
	if _, err := parseFlagsInterspersed(fs, args); err != nil {
		return err
	}
	if err := system.UninstallUserAgent(paths.SandboxAgentLabel); err != nil {
		return err
	}
	_ = os.Remove(paths.SandboxSocketPath(paths.StateDir()))
	cio.info("uninstalled " + paths.SandboxAgentLabel)
	return nil
}

func cmdAssistantSandboxAgent(args []string, _ *IO) error {
	fs := flag.NewFlagSet("assistant sandbox-agent", flag.ContinueOnError)
	sock := fs.String("socket", paths.SandboxSocketPath(paths.StateDir()), "Unix socket path")
	if _, err := parseFlagsInterspersed(fs, args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return assistant.RunSandboxAgent(ctx, *sock, nil)
}
