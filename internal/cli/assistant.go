package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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
`
}

func cmdAssistant(args []string, cio *IO) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		cio.printf("%s", assistantUsageText())
		return nil
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "setup":
		return cmdAssistantSetup(rest, cio)
	case "doctor":
		return cmdAssistantDoctor(rest, cio)
	case "op-setup":
		return cmdAssistantOPSetup(rest, cio)
	case "sync-install":
		return cmdAssistantSyncInstall(rest, cio)
	case "sync-uninstall":
		return cmdAssistantSyncUninstall(rest, cio)
	case "status":
		return cmdAssistantStatus(rest, cio)
	case "ask":
		return cmdAssistantAsk(rest, cio)
	case "approvals":
		return cmdAssistantApprovals(rest, cio)
	case "approve":
		return cmdAssistantDecide(rest, true, cio)
	case "deny":
		return cmdAssistantDecide(rest, false, cio)
	case "timers":
		return cmdAssistantTimers(rest, cio)
	case "notify-test":
		return cmdAssistantNotifyTest(rest, cio)
	default:
		return fmt.Errorf("unknown assistant subcommand %q\n\n%s", sub, assistantUsageText())
	}
}

func cmdAssistantSetup(args []string, cio *IO) error {
	fs := flag.NewFlagSet("assistant setup", flag.ContinueOnError)
	checkOnly := fs.Bool("check", false, "run lighthouse doctor check without modifying config")
	opRef := fs.String("op-ref", "", "resolve OpenRouter API key once from local 1Password ref")
	apiKeyOPRef := fs.String("api-key-op-ref", "", "store 1Password op:// ref on lighthouse (resolved in memory via Service Account)")
	apiKey := fs.String("api-key", "", "OpenRouter API key")
	vaultPath := fs.String("vault", "", "Obsidian vault path on lighthouse")
	model := fs.String("model", "", "default chat model")
	decisionModel := fs.String("decision-model", "", "decision model slug")
	observerModel := fs.String("observer-model", "", "observer/reflector model slug")
	zdr := fs.Bool("zdr", true, "enforce OpenRouter Zero Data Retention")
	enable := fs.Bool("enable", true, "enable assistant background scheduler")
	noTest := fs.Bool("no-test", false, "skip live connection test")
	discordRef := fs.String("discord-webhook-op-ref", "", "1Password op:// ref for outbound Discord webhook notifications")
	fleetURL := fs.String("fleet-url", "", "Fleet Console base URL used in Discord deep links")
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
	cfg.Enabled = *enable
	cfg.ZDR = *zdr
	if *apiKeyOPRef != "" {
		cfg.APIKeyOPRef = strings.TrimSpace(*apiKeyOPRef)
		cfg.APIKey = ""
	} else if *opRef != "" {
		secret, rErr := cio.readOPRef(*opRef)
		if rErr != nil {
			return rErr
		}
		cfg.APIKey = secret
		cfg.APIKeyOPRef = ""
	} else if *apiKey != "" {
		cfg.APIKey = strings.TrimSpace(*apiKey)
		cfg.APIKeyOPRef = ""
	}
	if *vaultPath != "" {
		cfg.VaultPath = *vaultPath
	}
	if *model != "" {
		cfg.Models.DefaultChat = *model
	}
	if *decisionModel != "" {
		cfg.Models.Decision = *decisionModel
	}
	if *observerModel != "" {
		cfg.Models.Observer = *observerModel
		cfg.Models.Reflector = *observerModel
	}
	if *discordRef != "" {
		cfg.Notifications.DiscordWebhookOPRef = strings.TrimSpace(*discordRef)
		cfg.Notifications.Enabled = true
	}
	if *fleetURL != "" {
		cfg.Notifications.FleetBaseURL = strings.TrimSpace(*fleetURL)
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
		"notifications":              cfg.Notifications,
		"test":                       !*noTest && (cfg.APIKey != "" || cfg.APIKeyOPRef != ""),
	}
	var out struct {
		Config assistant.ConfigView `json:"config"`
	}
	if err := client.Call(api, http.MethodPost, "/v1/assistant/config", req, 30*time.Second, nil, &out); err != nil {
		return err
	}
	cio.info(fmt.Sprintf("assistant configured (enabled=%v model=%s vault=%s zdr=%v)", out.Config.Enabled, out.Config.Models.DefaultChat, out.Config.EffectiveVaultPath, out.Config.ZDR))
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
