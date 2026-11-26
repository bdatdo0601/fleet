package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/session"
	"github.com/bdatdo0601/fleet/internal/slacker"
	"github.com/bdatdo0601/fleet/internal/system"
)

func slackerUsageText() string {
	return `usage: fleet slacker <subcommand> [options]

Setup & Config:
  manifest  [--format yaml|json]        print Slack App Manifest
  setup     [--account ID] [options]    guided Slack app, OAuth, and 1Password setup
  config                                show redacted Slacker configuration
  accounts                              list configured Slack accounts
  account   rm NAME                     delete a non-default Slack account and its mappings
  map       workspace|default|channel|emoji|ls ...
                                        manage machine workspaces, routing defaults, channel, and emoji mappings

Agent Read & Send:
  read      [--account ID] [--channel ID] [--thread TS] [--query Q] [--limit N] [--json]
                                        read Slack messages, threads, or search results
  send      --channel ID [--account ID] [--thread TS] (--text TEXT | -)
                                        send a Slack message or thread reply

Runtime:
  listen                                run the lighthouse Slack Socket Mode listener(s)
  worker    [--once] [--poll-timeout S] [--use-tmux] [--policy auto|bypass|safe]
                                        pull and run jobs for this Mac as logged-in user

Jobs & Services:
  jobs      [--device D] [--status S] [--limit N]
  retry     JOB_ID
  cancel    JOB_ID
  install   [--role auto|listener|worker|both]
  start     [--role auto|listener|worker|both]
  stop      [--role auto|listener|worker|both]
  status
  logs      [--role auto|listener|worker] [--lines N]
  cleanup   [--uninstall] [--prune-jobs] [--older-than 7d] [--dry-run] [--yes]
`
}

type multiStringFlag []string

func (m *multiStringFlag) String() string { return strings.Join(*m, ",") }
func (m *multiStringFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func slackClientForCLI(baseURL string) *slacker.SlackClient {
	return &slacker.SlackClient{
		BaseURL: cmp.Or(baseURL, os.Getenv("FLEET_SLACK_API_URL")),
	}
}

func cmdSlackerManifest(args []string, cio *IO) error {
	fs := flag.NewFlagSet("slacker manifest", flag.ContinueOnError)
	format := fs.String("format", "yaml", "manifest format (yaml|json)")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("usage: fleet slacker manifest [--format yaml|json]")
	}
	switch strings.ToLower(*format) {
	case "yaml", "yml":
		_, _ = io.WriteString(cio.Stdout, slacker.ManifestYAML())
	case "json":
		_, _ = io.WriteString(cio.Stdout, slacker.ManifestJSON())
	default:
		return fmt.Errorf("unknown format %q (expected yaml or json)", *format)
	}
	return nil
}

func readTokenPairFromOPItem(cio *IO, opItem string) (string, string, string, string, error) {
	var appRef, userRef string
	if strings.HasPrefix(opItem, "op://") {
		base := strings.TrimRight(opItem, "/")
		appRef = base + "/app_token"
		userRef = base + "/user_token"
	} else {
		appRef = "op://" + opItem + "/app_token"
		userRef = "op://" + opItem + "/user_token"
	}
	appTok, err := cio.readOPRef(appRef)
	if err != nil {
		return "", "", "", "", err
	}
	userTok, err := cio.readOPRef(userRef)
	if err != nil {
		return "", "", "", "", err
	}
	return appTok, userTok, appRef, userRef, nil
}

func parseCommaList(raw string) []string {
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func parseChannelSpec(raw string) (slacker.ChannelMapping, error) {
	cid, rest, ok := strings.Cut(raw, "=")
	if !ok {
		return slacker.ChannelMapping{}, fmt.Errorf("invalid --channel %q (expected CHANNEL_ID=DEVICE[,DEVICE2...]:[REPO_PATH])", raw)
	}
	devPart, repo, ok := strings.Cut(rest, ":")
	if !ok || strings.TrimSpace(cid) == "" || strings.TrimSpace(devPart) == "" {
		return slacker.ChannelMapping{}, fmt.Errorf("invalid --channel %q (expected CHANNEL_ID=DEVICE[,DEVICE2...]:[REPO_PATH])", raw)
	}
	devs := parseCommaList(devPart)
	if len(devs) == 0 {
		return slacker.ChannelMapping{}, fmt.Errorf("invalid --channel %q (expected at least one device)", raw)
	}
	singleDev := ""
	if len(devs) == 1 {
		singleDev = devs[0]
	}
	return slacker.ChannelMapping{
		ChannelID: strings.TrimSpace(cid),
		Device:    singleDev,
		Devices:   devs,
		RepoPath:  strings.TrimSpace(repo),
	}, nil
}

func parseEmojiSpec(raw string) (string, string, error) {
	em, hRaw, ok := strings.Cut(raw, "=")
	if !ok {
		return "", "", fmt.Errorf("invalid --emoji %q (expected EMOJI=claude|agy|codex)", raw)
	}
	emoji := slacker.NormalizeEmoji(em)
	h, hOK := session.NormalizeHarness(hRaw)
	if emoji == "" || !hOK {
		return "", "", fmt.Errorf("invalid --emoji %q (expected EMOJI=claude|agy|codex)", raw)
	}
	return emoji, h, nil
}

type resolvedTokens struct {
	appTok  string
	userTok string
	botTok  string
	appRef  string
	userRef string
	botRef  string
}

func resolveSetupTokens(cio *IO, curAcct slacker.AccountView, opItem, appOPRef, userOPRef, botOPRef string) (resolvedTokens, error) {
	var out resolvedTokens
	var err error
	if opItem != "" {
		out.appTok, out.userTok, out.appRef, out.userRef, err = readTokenPairFromOPItem(cio, opItem)
		if err != nil {
			return out, err
		}
	}
	if appOPRef != "" && out.appTok == "" {
		out.appRef = appOPRef
		if out.appTok, err = cio.readOPRef(appOPRef); err != nil {
			return out, err
		}
	}
	if userOPRef != "" && out.userTok == "" {
		out.userRef = userOPRef
		if out.userTok, err = cio.readOPRef(userOPRef); err != nil {
			return out, err
		}
	}
	if botOPRef != "" {
		out.botRef = botOPRef
		if out.botTok, err = cio.readOPRef(botOPRef); err != nil {
			return out, err
		}
	}
	if out.appTok == "" {
		prompt := "Slack App-Level Token (xapp-...): "
		if curAcct.HasAppToken {
			prompt = fmt.Sprintf("Slack App-Level Token (Enter to keep %s): ", curAcct.AppTokenHint)
		}
		if out.appTok, err = cio.askSecret(prompt); err != nil {
			return out, err
		}
	}
	if out.userTok == "" {
		prompt := "Slack User OAuth Token (xoxp-...): "
		if curAcct.HasUserToken {
			prompt = fmt.Sprintf("Slack User OAuth Token (Enter to keep %s): ", curAcct.UserTokenHint)
		}
		if out.userTok, err = cio.askSecret(prompt); err != nil {
			return out, err
		}
	}
	return out, nil
}

func mergeSetupMappings(curCfg slacker.ConfigView, acctID string, chanSpecs, emojiSpecs []string) (map[string]slacker.ChannelMapping, map[string]string, error) {
	channels := curCfg.Channels
	if channels == nil {
		channels = map[string]slacker.ChannelMapping{}
	}
	for _, raw := range chanSpecs {
		m, pErr := parseChannelSpec(raw)
		if pErr != nil {
			return nil, nil, pErr
		}
		m.Account = acctID
		channels[slacker.ChannelKey(acctID, m.ChannelID)] = m
	}

	emojis := curCfg.Emojis
	if len(emojis) == 0 && len(emojiSpecs) == 0 {
		emojis = slacker.DefaultEmojis()
	} else if emojis == nil {
		emojis = map[string]string{}
	}
	for _, raw := range emojiSpecs {
		em, h, pErr := parseEmojiSpec(raw)
		if pErr != nil {
			return nil, nil, pErr
		}
		emojis[em] = h
	}
	return channels, emojis, nil
}

func verifySetupTokens(cio *IO, slackAPI, userID string, toks resolvedTokens, prevUserID string) (authUserID, authUserName, teamID, teamName string, err error) {
	authUserID = cmp.Or(userID, prevUserID)
	sc := slackClientForCLI(slackAPI)
	if toks.userTok != "" {
		authRes, aErr := sc.AuthTest(context.Background(), toks.userTok)
		if aErr != nil {
			return "", "", "", "", fmt.Errorf("slack auth.test failed: %w", aErr)
		}
		authUserID = cmp.Or(userID, authRes.UserID, authUserID)
		authUserName, teamID, teamName = authRes.User, authRes.TeamID, authRes.Team
		cio.info(fmt.Sprintf("verified Slack user %s (%s) in workspace %s", authRes.User, authUserID, cmp.Or(teamName, teamID)))
	}
	if toks.botTok != "" {
		if _, bErr := sc.AuthTest(context.Background(), toks.botTok); bErr != nil {
			return "", "", "", "", fmt.Errorf("slack bot auth.test failed: %w", bErr)
		}
		cio.info("verified Slack Bot OAuth token")
	}
	if toks.appTok != "" {
		if _, oErr := sc.OpenSocketURL(context.Background(), toks.appTok); oErr != nil {
			return "", "", "", "", fmt.Errorf("slack apps.connections.open failed: %w", oErr)
		}
		cio.info("verified Slack Socket Mode app-level token")
	}
	return authUserID, authUserName, teamID, teamName, nil
}

func cmdSlackerSetup(args []string, cio *IO) error {
	fs := flag.NewFlagSet("slacker setup", flag.ContinueOnError)
	accountFlag := fs.String("account", slacker.DefaultAccountID, "Slack account ID (e.g. default, personal, work)")
	appOPRef := fs.String("app-token-op-ref", "", "1Password secret ref for Slack app-level token (xapp-...)")
	userOPRef := fs.String("user-token-op-ref", "", "1Password secret ref for Slack user OAuth token (xoxp-...)")
	botOPRef := fs.String("bot-token-op-ref", "", "1Password secret ref for optional Slack bot OAuth token (xoxb-...)")
	opItem := fs.String("op-item", "", "1Password item (Vault/Item) containing app_token and user_token fields")
	userID := fs.String("user-id", "", "authorized Slack user ID (auto-detected via auth.test if omitted)")
	slackAPI := fs.String("slack-api-url", "", "Slack Web API base URL override")
	noVerify := fs.Bool("no-verify", false, "skip Slack API auth.test / apps.connections.open verification")
	printManifest := fs.Bool("print-manifest", false, "print the Slack App Manifest YAML before setup")
	allowUnmapped := fs.Bool("allow-unmapped-send", false, "allow agents to post to unmapped channels on this account")
	disable := fs.Bool("disable", false, "save account with enabled=false")
	var chanSpecs, emojiSpecs multiStringFlag
	fs.Var(&chanSpecs, "channel", "channel mapping CHANNEL_ID=DEVICE:REPO_PATH (repeatable)")
	fs.Var(&emojiSpecs, "emoji", "emoji mapping EMOJI=claude|agy|codex (repeatable)")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(pos, " "))
	}
	acctID, err := slacker.NormalizeAccountID(*accountFlag)
	if err != nil {
		return err
	}

	if *printManifest {
		cio.printf("Create a Slack app from this manifest at https://api.slack.com/apps (Create New App -> From a manifest):\n\n%s\n", slacker.ManifestYAML())
	} else {
		cio.info("Slack setup: create an app with `fleet slacker manifest`, enable Socket Mode (xapp-...), and install User OAuth (xoxp-...).")
	}

	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var curResp struct {
		Config slacker.ConfigView `json:"config"`
	}
	_ = client.Call(api, http.MethodGet, "/v1/slacker/config", nil, 15*time.Second, nil, &curResp)
	curAcct := curResp.Config.Accounts[acctID]

	toks, err := resolveSetupTokens(cio, curAcct, *opItem, *appOPRef, *userOPRef, *botOPRef)
	if err != nil {
		return err
	}

	authUserID := cmp.Or(*userID, curAcct.AuthorizedUserID)
	var authUserName, teamID, teamName string
	if !*noVerify {
		authUserID, authUserName, teamID, teamName, err = verifySetupTokens(cio, *slackAPI, *userID, toks, curAcct.AuthorizedUserID)
		if err != nil {
			return err
		}
	}

	channels, emojis, err := mergeSetupMappings(curResp.Config, acctID, chanSpecs, emojiSpecs)
	if err != nil {
		return err
	}

	payload := map[string]any{
		"account":            acctID,
		"enabled":            !*disable,
		"authorized_user_id": authUserID,
		"channels":           channels,
		"emojis":             emojis,
		"verify":             false,
		"app_token_op_ref":   toks.appRef,
		"user_token_op_ref":  toks.userRef,
		"bot_token_op_ref":   toks.botRef,
		"authorized_user":    authUserName,
		"team_id":            teamID,
		"team_name":          teamName,
	}
	if *allowUnmapped {
		payload["allow_unmapped_send"] = true
	}
	if toks.appTok != "" {
		payload["app_token"] = toks.appTok
	}
	if toks.userTok != "" {
		payload["user_token"] = toks.userTok
	}
	if toks.botTok != "" {
		payload["bot_token"] = toks.botTok
	}

	var saved struct {
		Config slacker.ConfigView `json:"config"`
	}
	if err := client.Call(api, http.MethodPost, "/v1/slacker/config", payload, 30*time.Second, nil, &saved); err != nil {
		return err
	}
	printSlackerConfig(cio, saved.Config)
	cio.info("next: run `fleet slacker install` on the lighthouse and each target Mac")
	return nil
}

func printSlackerConfig(cio *IO, cfg slacker.ConfigView) {
	cio.printf("accounts:        %d\n", len(cfg.Accounts))
	for _, id := range slices.Sorted(maps.Keys(cfg.Accounts)) {
		a := cfg.Accounts[id]
		cio.printf("  [%s] enabled=%v configured=%v user=%s (%s) app=%s user_tok=%s bot=%s unmapped_send=%v\n",
			id, a.Enabled, a.Configured, cmp.Or(a.AuthorizedUserID, "-"), cmp.Or(a.AuthorizedUser, a.TeamName, "-"),
			cmp.Or(a.AppTokenHint, "-"), cmp.Or(a.UserTokenHint, "-"), cmp.Or(a.BotTokenHint, "-"), a.AllowUnmappedSend)
	}
	defDevs := "-"
	if len(cfg.DefaultDevices) > 0 {
		defDevs = strings.Join(cfg.DefaultDevices, ",")
	}
	cio.printf("default_routing: strategy=%s policy=%s devices=%s\n",
		cmp.Or(cfg.DefaultStrategy, slacker.StrategyLeastAgents),
		cmp.Or(cfg.DefaultPolicy, string(session.PolicyAuto)),
		defDevs)
	cio.printf("workspaces:      %d\n", len(cfg.Devices))
	for _, dev := range slices.Sorted(maps.Keys(cfg.Devices)) {
		cio.printf("  %s -> %s\n", dev, cfg.Devices[dev].WorkspaceDir)
	}
	cio.printf("channels:        %d\n", len(cfg.Channels))
	for _, k := range slices.Sorted(maps.Keys(cfg.Channels)) {
		ch := cfg.Channels[k]
		acct := cmp.Or(ch.Account, slacker.DefaultAccountID)
		devs := ch.Devices
		if len(devs) == 0 && ch.Device != "" {
			devs = []string{ch.Device}
		}
		target := "default"
		if len(devs) > 0 {
			target = strings.Join(devs, ",")
		}
		repo := cmp.Or(ch.RepoPath, "<workspace>")
		extra := ""
		if ch.Strategy != "" {
			extra += " strategy=" + ch.Strategy
		}
		if ch.Policy != "" {
			extra += " policy=" + ch.Policy
		}
		cio.printf("  [%s] %s -> %s:%s (tmux=%v%s)\n", acct, ch.ChannelID, target, repo, ch.UseTmux, extra)
	}
	cio.printf("emojis:          %d\n", len(cfg.Emojis))
	for _, em := range slices.Sorted(maps.Keys(cfg.Emojis)) {
		cio.printf("  :%s: -> %s\n", em, cfg.Emojis[em])
	}
}

func cmdSlackerMap(args []string, cio *IO) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: fleet slacker map <ls|workspace|default|channel|emoji>")
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var cur struct {
		Config slacker.ConfigView `json:"config"`
	}
	if err := client.Call(api, http.MethodGet, "/v1/slacker/config", nil, 15*time.Second, nil, &cur); err != nil {
		return err
	}
	cfg := cur.Config
	if cfg.Devices == nil {
		cfg.Devices = map[string]slacker.DeviceConfig{}
	}
	if cfg.Channels == nil {
		cfg.Channels = map[string]slacker.ChannelMapping{}
	}
	if cfg.Emojis == nil {
		cfg.Emojis = map[string]string{}
	}

	payload := map[string]any{}
	switch args[0] {
	case "ls":
		printSlackerConfig(cio, cfg)
		return nil
	case "workspace":
		switch {
		case len(args) == 3 && args[1] == "rm":
			delete(cfg.Devices, strings.TrimSpace(args[2]))
		case len(args) == 3:
			dev := strings.TrimSpace(args[1])
			dir := strings.TrimSpace(args[2])
			if dev == "" || dir == "" {
				return fmt.Errorf("usage: fleet slacker map workspace DEVICE DIR | rm DEVICE")
			}
			cfg.Devices[dev] = slacker.DeviceConfig{WorkspaceDir: dir}
		default:
			return fmt.Errorf("usage: fleet slacker map workspace DEVICE DIR | rm DEVICE")
		}
		payload["devices"] = cfg.Devices
	case "default":
		fs := flag.NewFlagSet("slacker map default", flag.ContinueOnError)
		devicesFlag := fs.String("devices", "", "comma-separated default devices")
		clearDevices := fs.Bool("clear-devices", false, "clear default devices")
		strategyFlag := fs.String("strategy", "", "default routing strategy (least_agents|round_robin)")
		policyFlag := fs.String("policy", "", "default permission policy (auto|bypass|safe)")
		pos, pErr := parseFlagsInterspersed(fs, args[1:])
		if pErr != nil {
			return pErr
		}
		if len(pos) > 0 || (*devicesFlag == "" && !*clearDevices && *strategyFlag == "" && *policyFlag == "") {
			return fmt.Errorf("usage: fleet slacker map default [--devices D1,D2] [--clear-devices] [--strategy least_agents|round_robin] [--policy auto|bypass|safe]")
		}
		if *clearDevices {
			payload["default_devices"] = []string{}
		} else if *devicesFlag != "" {
			devs := parseCommaList(*devicesFlag)
			if len(devs) == 0 {
				return fmt.Errorf("--devices must include at least one device name (use --clear-devices to clear)")
			}
			payload["default_devices"] = devs
		}
		if *strategyFlag != "" {
			strat, sErr := slacker.NormalizeStrategy(*strategyFlag)
			if sErr != nil {
				return sErr
			}
			payload["default_strategy"] = strat
		}
		if *policyFlag != "" {
			pol, polErr := slacker.NormalizePolicy(*policyFlag)
			if polErr != nil {
				return polErr
			}
			payload["default_policy"] = pol
		}
	case "channel":
		fs := flag.NewFlagSet("slacker map channel", flag.ContinueOnError)
		accountFlag := fs.String("account", slacker.DefaultAccountID, "Slack account ID")
		devicesFlag := fs.String("devices", "", "comma-separated target devices (defaults to global default_devices when omitted)")
		strategyFlag := fs.String("strategy", "", "routing strategy override (least_agents|round_robin)")
		policyFlag := fs.String("policy", "", "permission policy override (auto|bypass|safe)")
		repoFlag := fs.String("repo", "", "repo path or subpath relative to machine workspace_dir")
		chName := fs.String("name", "", "optional Slack channel name label")
		useTmux := fs.Bool("tmux", false, "force tmux for this channel")
		pos, pErr := parseFlagsInterspersed(fs, args[1:])
		if pErr != nil {
			return pErr
		}
		acctID, aErr := slacker.NormalizeAccountID(*accountFlag)
		if aErr != nil {
			return aErr
		}
		if len(pos) == 2 && pos[0] == "rm" {
			delete(cfg.Channels, pos[1])
			delete(cfg.Channels, slacker.ChannelKey(acctID, pos[1]))
		} else {
			if len(pos) < 1 || len(pos) > 3 {
				return fmt.Errorf("usage: fleet slacker map channel [--account ID] CHANNEL_ID [DEVICE[,DEVICE2...]] [REPO_PATH] [--devices D1,D2] [--strategy least_agents|round_robin] [--policy auto|bypass|safe] [--repo PATH] [--name NAME] [--tmux] | rm [--account ID] CHANNEL_ID")
			}
			var devs []string
			if len(pos) >= 2 {
				devs = parseCommaList(pos[1])
			}
			if *devicesFlag != "" {
				devs = parseCommaList(*devicesFlag)
			}
			repo := strings.TrimSpace(*repoFlag)
			if len(pos) == 3 {
				repo = strings.TrimSpace(pos[2])
			}
			strat, sErr := slacker.NormalizeStrategy(*strategyFlag)
			if sErr != nil {
				return sErr
			}
			pol, polErr := slacker.NormalizePolicy(*policyFlag)
			if polErr != nil {
				return polErr
			}
			singleDev := ""
			if len(devs) == 1 {
				singleDev = devs[0]
			}
			key := slacker.ChannelKey(acctID, pos[0])
			cfg.Channels[key] = slacker.ChannelMapping{
				Account:     acctID,
				ChannelID:   pos[0],
				ChannelName: *chName,
				Device:      singleDev,
				Devices:     devs,
				Strategy:    strat,
				Policy:      pol,
				RepoPath:    repo,
				UseTmux:     *useTmux,
			}
		}
		payload["channels"] = cfg.Channels
		payload["emojis"] = cfg.Emojis
	case "emoji":
		switch {
		case len(args) == 3 && args[1] == "rm":
			delete(cfg.Emojis, slacker.NormalizeEmoji(args[2]))
		case len(args) == 3:
			h, ok := session.NormalizeHarness(args[2])
			if !ok {
				return fmt.Errorf("unknown harness %q (expected claude, agy, or codex)", args[2])
			}
			cfg.Emojis[slacker.NormalizeEmoji(args[1])] = h
		default:
			return fmt.Errorf("usage: fleet slacker map emoji EMOJI claude|agy|codex | rm EMOJI")
		}
		payload["channels"] = cfg.Channels
		payload["emojis"] = cfg.Emojis
	default:
		return fmt.Errorf("usage: fleet slacker map <ls|workspace|default|channel|emoji>")
	}

	var updated struct {
		Config slacker.ConfigView `json:"config"`
	}
	if err := client.Call(api, http.MethodPost, "/v1/slacker/config", payload, 15*time.Second, nil, &updated); err != nil {
		return err
	}
	printSlackerConfig(cio, updated.Config)
	return nil
}

func cmdSlackerJobs(args []string, cio *IO) error {
	fs := flag.NewFlagSet("slacker jobs", flag.ContinueOnError)
	device := fs.String("device", "", "filter by target device")
	status := fs.String("status", "", "filter by status")
	limit := fs.Int("limit", 50, "max rows")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("usage: fleet slacker jobs [--device D] [--status S] [--limit N]")
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var resp struct {
		Jobs []slacker.Job `json:"jobs"`
	}
	q := fmt.Sprintf("?limit=%d&device=%s&status=%s", *limit, *device, *status)
	if err := client.Call(api, http.MethodGet, "/v1/slacker/jobs"+q, nil, 15*time.Second, nil, &resp); err != nil {
		return err
	}
	if len(resp.Jobs) == 0 {
		cio.printf("no slacker jobs\n")
		return nil
	}
	rows := [][]string{{"ID", "ACCOUNT", "SESSION", "TARGET", "HARNESS", "REACTION", "STATUS", "BACKEND", "UPDATED"}}
	for _, j := range resp.Jobs {
		up := j.UpdatedAt
		rows = append(rows, []string{
			j.ID, cmp.Or(j.Account, slacker.DefaultAccountID), j.SessionName, j.TargetDevice, j.Harness, ":" + j.Reaction + ":", j.Status, cmp.Or(j.Backend, "-"), ago(&up),
		})
	}
	printTable(cio.Stdout, rows)
	return nil
}

func cmdSlackerJobAction(args []string, action string, cio *IO) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: fleet slacker %s JOB_ID", action)
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var resp struct {
		Job slacker.Job `json:"job"`
	}
	if err := client.Call(api, http.MethodPost, "/v1/slacker/jobs/"+args[0]+"/"+action, map[string]any{}, 15*time.Second, nil, &resp); err != nil {
		return err
	}
	cio.printf("%s -> %s\n", resp.Job.ID, resp.Job.Status)
	return nil
}

func cmdSlackerListen(args []string, cio *IO) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: fleet slacker listen")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return slacker.RunListener(ctx, slacker.ListenerOpts{
		Stderr: cio.Stderr,
	})
}

func cmdSlackerWorker(args []string, cio *IO) error {
	fs := flag.NewFlagSet("slacker worker", flag.ContinueOnError)
	once := fs.Bool("once", false, "poll once and exit")
	pollTimeout := fs.Float64("poll-timeout", 30.0, "long-poll seconds per request")
	useTmux := fs.Bool("use-tmux", false, "force tmux for launched sessions")
	policyFlag := fs.String("policy", "", "fallback permission policy when job policy is unset (auto|bypass|safe)")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("usage: fleet slacker worker [--once] [--poll-timeout S] [--use-tmux] [--policy auto|bypass|safe]")
	}
	pol, err := session.ParsePolicy(*policyFlag, session.PolicyAuto)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return slacker.RunWorker(ctx, slacker.WorkerOpts{
		Once:             *once,
		PollTimeout:      *pollTimeout,
		UseTmux:          *useTmux,
		Policy:           pol,
		RefreshShellPath: true,
		Stdout:           cio.Stdout,
		Stderr:           cio.Stderr,
	})
}

func resolveSlackerServiceRoles(roleFlag string) (doListener, doWorker, isLighthouse bool, err error) {
	if node, nErr := client.LoadNode(); nErr == nil {
		isLighthouse = strings.HasPrefix(node.API, "http://127.0.0.1")
	} else {
		_, sErr := os.Stat(filepath.Join(paths.StateDir(), "fleet.db"))
		isLighthouse = sErr == nil
	}
	switch strings.ToLower(cmp.Or(roleFlag, "auto")) {
	case "auto":
		if isLighthouse {
			return true, false, true, nil
		}
		return false, true, false, nil
	case "listener":
		return true, false, isLighthouse, nil
	case "worker":
		return false, true, isLighthouse, nil
	case "both":
		return true, true, isLighthouse, nil
	default:
		return false, false, isLighthouse, fmt.Errorf("unknown --role %q (expected auto, listener, worker, or both)", roleFlag)
	}
}

func slackerWorkerLogPath() string {
	return filepath.Join(paths.SlackerLocalStateDir(), "worker.log")
}

func cmdSlackerServiceInstall(args []string, cio *IO) error {
	fs := flag.NewFlagSet("slacker install", flag.ContinueOnError)
	role := fs.String("role", "auto", "service role (auto|listener|worker|both)")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("usage: fleet slacker install [--role auto|listener|worker|both]")
	}
	doListener, doWorker, _, err := resolveSlackerServiceRoles(*role)
	if err != nil {
		return err
	}
	instBin, err := ensureInstalledSelf()
	if err != nil {
		return err
	}
	if doListener {
		if err := requireNotRoot("slacker install --role listener"); err != nil {
			return err
		}
		stateDir := paths.StateDir()
		owner := currentUsername()
		if err := system.InstallSlackerDaemon(instBin, owner, stateDir, stateDir); err != nil {
			return err
		}
		cio.info("installed and started " + paths.SlackerLabel)
	}
	if doWorker {
		if err := requireNotRoot("slacker install --role worker"); err != nil {
			return err
		}
		home, _ := os.UserHomeDir()
		if err := system.InstallUserAgent(system.AgentOpts{
			Label:     paths.SlackerWorkerLabel,
			Args:      []string{instBin, "slacker", "worker"},
			LogPath:   slackerWorkerLogPath(),
			WorkDir:   home,
			KeepAlive: true,
		}); err != nil {
			return err
		}
		cio.info("installed and started " + paths.SlackerWorkerLabel + " (logged-in user agent)")
	}
	return nil
}

func cmdSlackerServiceControl(args []string, start bool, cio *IO) error {
	verb := "stop"
	if start {
		verb = "start"
	}
	fs := flag.NewFlagSet("slacker "+verb, flag.ContinueOnError)
	role := fs.String("role", "auto", "service role (auto|listener|worker|both)")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("usage: fleet slacker %s [--role auto|listener|worker|both]", verb)
	}
	doListener, doWorker, _, err := resolveSlackerServiceRoles(*role)
	if err != nil {
		return err
	}
	if doListener {
		if start {
			if err := system.StartDaemon(paths.SlackerLabel); err != nil {
				return err
			}
			cio.info("started " + paths.SlackerLabel)
		} else {
			if err := system.StopDaemon(paths.SlackerLabel); err != nil {
				return err
			}
			cio.info("stopped " + paths.SlackerLabel)
		}
	}
	if doWorker {
		if start {
			if err := system.StartUserAgent(paths.SlackerWorkerLabel); err != nil {
				return err
			}
			cio.info("started " + paths.SlackerWorkerLabel)
		} else {
			if err := system.StopUserAgent(paths.SlackerWorkerLabel); err != nil {
				return err
			}
			cio.info("stopped " + paths.SlackerWorkerLabel)
		}
	}
	return nil
}

func cmdSlackerStatus(args []string, cio *IO) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: fleet slacker status")
	}
	_, _, isLighthouse, _ := resolveSlackerServiceRoles("auto")
	if isLighthouse {
		st := "NOT loaded"
		if system.LaunchdLoaded(paths.SlackerLabel) {
			st = "loaded"
		}
		cio.printf("%-44s %s\n", paths.SlackerLabel+":", st)
	}
	wst := "NOT loaded"
	if system.UserAgentLoaded(paths.SlackerWorkerLabel) {
		wst = "loaded"
	}
	cio.printf("%-44s %s\n", paths.SlackerWorkerLabel+":", wst)

	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var stRes struct {
		Config   slacker.ConfigView      `json:"config"`
		Runtimes []slacker.RuntimeStatus `json:"runtimes"`
		Counts   map[string]int          `json:"counts"`
	}
	if err := client.Call(api, http.MethodGet, "/v1/slacker/status", nil, 15*time.Second, nil, &stRes); err != nil {
		cio.printf("lighthouse slacker API: %v\n", err)
		return nil
	}
	cio.printf("enabled: %v · configured: %v · queued: %d · launched: %d · failed: %d\n",
		stRes.Config.Enabled, stRes.Config.Configured,
		stRes.Counts["queued"], stRes.Counts["launched"], stRes.Counts["failed"])
	for _, rt := range stRes.Runtimes {
		cio.printf("  %-18s connected=%v updated=%s err=%s\n", rt.Role, rt.Connected, ago(&rt.UpdatedAt), cmp.Or(rt.LastError, "-"))
	}
	return nil
}

func tailLocalFile(path string, lines int, w io.Writer) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	all := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	for _, ln := range all {
		_, _ = fmt.Fprintln(w, ln)
	}
	return nil
}

func cmdSlackerLogs(args []string, cio *IO) error {
	fs := flag.NewFlagSet("slacker logs", flag.ContinueOnError)
	role := fs.String("role", "auto", "log source (auto|listener|worker)")
	lines := fs.Int("lines", 100, "number of lines")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("usage: fleet slacker logs [--role auto|listener|worker] [--lines N]")
	}
	doListener, doWorker, _, err := resolveSlackerServiceRoles(*role)
	if err != nil {
		return err
	}
	if doWorker && !doListener {
		p := slackerWorkerLogPath()
		if err := tailLocalFile(p, *lines, cio.Stdout); err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		return nil
	}
	localLog := filepath.Join(paths.StateDir(), "slacker.log")
	if _, err := os.Stat(localLog); err == nil {
		return tailLocalFile(localLog, *lines, cio.Stdout)
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var res struct {
		Lines []string `json:"lines"`
	}
	if err := client.Call(api, http.MethodGet, fmt.Sprintf("/v1/logs?name=slacker&lines=%d", *lines), nil, 15*time.Second, nil, &res); err != nil {
		return err
	}
	for _, ln := range res.Lines {
		_, _ = fmt.Fprintln(cio.Stdout, ln)
	}
	return nil
}

func cmdSlackerCleanup(args []string, cio *IO) error {
	fs := flag.NewFlagSet("slacker cleanup", flag.ContinueOnError)
	uninstall := fs.Bool("uninstall", false, "stop and remove slacker launchd services")
	pruneJobs := fs.Bool("prune-jobs", false, "prune completed/failed/cancelled jobs from lighthouse slacker.db")
	olderThan := fs.String("older-than", "", "only prune terminal jobs older than duration (e.g. 168h)")
	dryRun := fs.Bool("dry-run", false, "preview cleanup without modifying files or services")
	yes := fs.Bool("yes", false, "skip confirmation prompt")
	localDirFlag := fs.String("local-state-dir", "", "override local slacker worker state dir")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("usage: fleet slacker cleanup [--uninstall] [--prune-jobs] [--older-than 168h] [--dry-run] [--yes]")
	}

	localDir := cmp.Or(*localDirFlag, paths.SlackerLocalStateDir())
	receiptsPath := filepath.Join(localDir, "receipts")
	workerLock := filepath.Join(localDir, "worker.lock")
	listenerLock := filepath.Join(paths.StateDir(), "slacker-listener.lock")
	_, _, isLighthouse, _ := resolveSlackerServiceRoles("auto")

	if !*dryRun && !*yes {
		if err := cio.confirm("Clean up slacker state/services (active sessions and repos will be preserved)?"); err != nil {
			return err
		}
	}

	if *dryRun {
		cio.printf("dry-run: would preserve all active tmux/Rex sessions and repositories\n")
		cio.printf("dry-run: would remove %s and %s\n", receiptsPath, workerLock)
		if *uninstall {
			cio.printf("dry-run: would stop and uninstall %s", paths.SlackerWorkerLabel)
			if isLighthouse {
				cio.printf(" and %s", paths.SlackerLabel)
			}
			cio.printf("\n")
		}
		if *pruneJobs || *olderThan != "" {
			cio.printf("dry-run: would prune terminal slacker jobs on lighthouse (older_than=%q)\n", cmp.Or(*olderThan, "all"))
		}
		return nil
	}

	if *uninstall {
		if err := system.UninstallUserAgent(paths.SlackerWorkerLabel); err != nil {
			return err
		}
		cio.info("uninstalled " + paths.SlackerWorkerLabel)
		if isLighthouse {
			if err := system.UninstallDaemon(paths.SlackerLabel); err != nil {
				return err
			}
			cio.info("uninstalled " + paths.SlackerLabel)
		}
	}

	if err := os.RemoveAll(receiptsPath); err != nil {
		return err
	}
	_ = os.Remove(workerLock)
	if isLighthouse {
		_ = os.Remove(listenerLock)
	}
	cio.info("cleaned local slacker receipts and locks (active sessions and repos preserved)")

	if *pruneJobs || *olderThan != "" {
		api, err := client.ResolveAPI("")
		if err != nil {
			return err
		}
		var res struct {
			PrunedJobs    int64 `json:"pruned_jobs"`
			PrunedWorkers int64 `json:"pruned_workers"`
		}
		if err := client.Call(api, http.MethodPost, "/v1/slacker/cleanup", map[string]any{
			"older_than":   *olderThan,
			"all_terminal": *olderThan == "",
		}, 15*time.Second, nil, &res); err != nil {
			return err
		}
		cio.info(fmt.Sprintf("pruned %d terminal job(s) and %d stale worker row(s)", res.PrunedJobs, res.PrunedWorkers))
	}
	return nil
}

func cmdSlackerService(args []string, cio *IO) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: fleet slacker service <install|start|stop|status|logs> [options]")
	}
	switch args[0] {
	case "install":
		return cmdSlackerServiceInstall(args[1:], cio)
	case "start":
		return cmdSlackerServiceControl(args[1:], true, cio)
	case "stop":
		return cmdSlackerServiceControl(args[1:], false, cio)
	case "status":
		return cmdSlackerStatus(args[1:], cio)
	case "logs":
		return cmdSlackerLogs(args[1:], cio)
	default:
		return fmt.Errorf("unknown slacker service subcommand %q", args[0])
	}
}

func cmdSlackerConfig(rest []string, cio *IO) error {
	if len(rest) > 0 {
		return fmt.Errorf("usage: fleet slacker config")
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var resp struct {
		Config slacker.ConfigView `json:"config"`
	}
	if err := client.Call(api, http.MethodGet, "/v1/slacker/config", nil, 15*time.Second, nil, &resp); err != nil {
		return err
	}
	printSlackerConfig(cio, resp.Config)
	return nil
}

func cmdSlackerAccounts(args []string, cio *IO) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: fleet slacker accounts")
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var resp struct {
		Config slacker.ConfigView `json:"config"`
	}
	if err := client.Call(api, http.MethodGet, "/v1/slacker/config", nil, 15*time.Second, nil, &resp); err != nil {
		return err
	}
	rows := [][]string{{"ACCOUNT", "ENABLED", "CONFIGURED", "USER", "TEAM", "BOT", "UNMAPPED SEND"}}
	for _, id := range slices.Sorted(maps.Keys(resp.Config.Accounts)) {
		a := resp.Config.Accounts[id]
		rows = append(rows, []string{
			id,
			strconv.FormatBool(a.Enabled),
			strconv.FormatBool(a.Configured),
			cmp.Or(a.AuthorizedUser, a.AuthorizedUserID, "-"),
			cmp.Or(a.TeamName, a.TeamID, "-"),
			cmp.Or(a.BotTokenHint, "-"),
			strconv.FormatBool(a.AllowUnmappedSend),
		})
	}
	printTable(cio.Stdout, rows)
	return nil
}

func cmdSlackerAccount(args []string, cio *IO) error {
	if len(args) != 2 || args[0] != "rm" {
		return fmt.Errorf("usage: fleet slacker account rm NAME")
	}
	acctID, err := slacker.NormalizeAccountID(args[1])
	if err != nil {
		return err
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var saved struct {
		Config slacker.ConfigView `json:"config"`
	}
	if err := client.Call(api, http.MethodPost, "/v1/slacker/config", map[string]any{
		"delete_account": acctID,
	}, 15*time.Second, nil, &saved); err != nil {
		return err
	}
	printSlackerConfig(cio, saved.Config)
	return nil
}

func cmdSlackerRead(args []string, cio *IO) error {
	fs := flag.NewFlagSet("slacker read", flag.ContinueOnError)
	acctFlag := fs.String("account", "", "Slack account ID (optional; inferred from mapped channel if omitted)")
	channel := fs.String("channel", "", "Slack channel ID (for history or thread)")
	threadTS := fs.String("thread", "", "parent message timestamp (ts) to read replies")
	query := fs.String("query", "", "Slack search query")
	oldest := fs.String("oldest", "", "oldest Slack timestamp bound")
	latest := fs.String("latest", "", "latest Slack timestamp bound")
	limit := fs.Int("limit", 25, "maximum number of messages")
	jsonOut := fs.Bool("json", false, "output raw JSON")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 || (strings.TrimSpace(*channel) == "" && strings.TrimSpace(*query) == "") {
		return fmt.Errorf("usage: fleet slacker read [--account ID] [--channel ID] [--thread TS] [--query Q] [--limit N] [--json]")
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	q := url.Values{}
	if *acctFlag != "" {
		q.Set("account", *acctFlag)
	}
	if *channel != "" {
		q.Set("channel", *channel)
	}
	if *threadTS != "" {
		q.Set("thread_ts", *threadTS)
	}
	if *query != "" {
		q.Set("query", *query)
	}
	if *oldest != "" {
		q.Set("oldest", *oldest)
	}
	if *latest != "" {
		q.Set("latest", *latest)
	}
	if *limit > 0 {
		q.Set("limit", strconv.Itoa(*limit))
	}
	var res slacker.ReadMessagesResult
	if err := client.Call(api, http.MethodGet, "/v1/slacker/messages?"+q.Encode(), nil, 15*time.Second, nil, &res); err != nil {
		return err
	}
	if *jsonOut {
		enc := json.NewEncoder(cio.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	cio.printf("account=%s mode=%s channel=%s count=%d\n", res.Account, res.Mode, cmp.Or(res.ChannelName, res.ChannelID, "-"), len(res.Messages))
	for _, m := range res.Messages {
		who := cmp.Or(m.AuthorName, m.User, "unknown")
		ch := cmp.Or(m.ChannelName, m.ChannelID, res.ChannelID)
		cio.printf("[%s] (%s %s) %s\n", m.TS, who, ch, strings.TrimSpace(m.Text))
	}
	return nil
}

func cmdSlackerSend(args []string, cio *IO) error {
	fs := flag.NewFlagSet("slacker send", flag.ContinueOnError)
	acctFlag := fs.String("account", "", "Slack account ID (optional; inferred from mapped channel if omitted)")
	channel := fs.String("channel", "", "target Slack channel ID")
	threadTS := fs.String("thread", "", "optional parent message timestamp (ts) for thread replies")
	textFlag := fs.String("text", "", "message text (pass '-' positionally to read from stdin)")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	text := *textFlag
	if len(pos) == 1 && pos[0] == "-" && text == "" {
		raw, rErr := io.ReadAll(cio.Stdin)
		if rErr != nil {
			return rErr
		}
		text = strings.TrimRight(string(raw), "\r\n")
	} else if len(pos) > 0 {
		return fmt.Errorf("usage: fleet slacker send --channel ID [--account ID] [--thread TS] (--text TEXT | -)")
	}
	if strings.TrimSpace(*channel) == "" || strings.TrimSpace(text) == "" {
		return fmt.Errorf("usage: fleet slacker send --channel ID [--account ID] [--thread TS] (--text TEXT | -)")
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	body := map[string]any{
		"channel": *channel,
		"text":    text,
	}
	if *acctFlag != "" {
		body["account"] = *acctFlag
	}
	if *threadTS != "" {
		body["thread_ts"] = *threadTS
	}
	var posted slacker.PostedMessage
	if err := client.Call(api, http.MethodPost, "/v1/slacker/send", body, 15*time.Second, nil, &posted); err != nil {
		return err
	}
	cio.printf("sent account=%s channel=%s ts=%s\n", posted.Account, posted.Channel, posted.TS)
	return nil
}

func cmdSlacker(args []string, cio *IO) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		_, _ = io.WriteString(cio.Stdout, slackerUsageText())
		return nil
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "manifest":
		return cmdSlackerManifest(rest, cio)
	case "setup":
		return cmdSlackerSetup(rest, cio)
	case "config":
		return cmdSlackerConfig(rest, cio)
	case "accounts":
		return cmdSlackerAccounts(rest, cio)
	case "account":
		return cmdSlackerAccount(rest, cio)
	case "map":
		return cmdSlackerMap(rest, cio)
	case "read":
		return cmdSlackerRead(rest, cio)
	case "send":
		return cmdSlackerSend(rest, cio)
	case "jobs":
		return cmdSlackerJobs(rest, cio)
	case "retry":
		return cmdSlackerJobAction(rest, "retry", cio)
	case "cancel":
		return cmdSlackerJobAction(rest, "cancel", cio)
	case "listen":
		return cmdSlackerListen(rest, cio)
	case "worker":
		return cmdSlackerWorker(rest, cio)
	case "service":
		return cmdSlackerService(rest, cio)
	case "install", "start", "stop", "status", "logs":
		return cmdSlackerService(append([]string{sub}, rest...), cio)
	case "cleanup":
		return cmdSlackerCleanup(rest, cio)
	default:
		return fmt.Errorf("unknown slacker subcommand %q\n\n%s", sub, slackerUsageText())
	}
}
