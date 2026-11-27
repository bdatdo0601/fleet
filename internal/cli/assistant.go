package cli

import (
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/assistant"
	"github.com/bdatdo0601/fleet/internal/client"
)

func assistantUsageText() string {
	return `usage: fleet assistant <subcommand> [options]

Subcommands:
  setup      configure OpenRouter, vault path, and models
  status     show assistant status, models, and pending counts
  ask        send a prompt to an assistant channel
  approvals  list pending or recent tool approvals
  approve    authorize a pending tool call by ID
  deny       reject a pending tool call by ID
  timers     list scheduled timers
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
	default:
		return fmt.Errorf("unknown assistant subcommand %q\n\n%s", sub, assistantUsageText())
	}
}

func cmdAssistantSetup(args []string, cio *IO) error {
	fs := flag.NewFlagSet("assistant setup", flag.ContinueOnError)
	opRef := fs.String("op-ref", "", "1Password secret reference for OpenRouter API key")
	apiKey := fs.String("api-key", "", "OpenRouter API key")
	vaultPath := fs.String("vault", "", "Obsidian vault path on lighthouse")
	model := fs.String("model", "", "default chat model")
	decisionModel := fs.String("decision-model", "", "decision model slug")
	observerModel := fs.String("observer-model", "", "observer/reflector model slug")
	zdr := fs.Bool("zdr", true, "enforce OpenRouter Zero Data Retention")
	enable := fs.Bool("enable", true, "enable assistant background scheduler")
	noTest := fs.Bool("no-test", false, "skip live connection test")
	if _, err := parseFlagsInterspersed(fs, args); err != nil {
		return err
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
	if *opRef != "" {
		secret, rErr := cio.readOPRef(*opRef)
		if rErr != nil {
			return rErr
		}
		cfg.APIKey = secret
	} else if *apiKey != "" {
		cfg.APIKey = strings.TrimSpace(*apiKey)
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
	req := map[string]any{
		"enabled":                    cfg.Enabled,
		"base_url":                   cfg.BaseURL,
		"api_key":                    cfg.APIKey,
		"zdr":                        cfg.ZDR,
		"data_collection":            cfg.DataCollection,
		"vault_path":                 cfg.VaultPath,
		"auto_write_dirs":            cfg.AutoWriteDirs,
		"git_auto_commit":            cfg.GitAutoCommit,
		"heartbeat_interval_minutes": cfg.HeartbeatIntervalMinutes,
		"observer_threshold_tokens":  cfg.ObserverThresholdTokens,
		"reflector_threshold_bytes":  cfg.ReflectorThresholdBytes,
		"models":                     cfg.Models,
		"test":                       !*noTest && cfg.APIKey != "",
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
