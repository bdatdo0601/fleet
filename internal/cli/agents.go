package cli

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/session"
	"github.com/bdatdo0601/fleet/internal/slacker"
	"github.com/bdatdo0601/fleet/internal/system"
	"github.com/bdatdo0601/fleet/internal/waiter"
)

var (
	sessionStartFn     = session.Start
	sessionKillAgentFn = session.KillAgent
	sessionKillAllFn   = session.KillAllAgents
)

func agentsUsageText() string {
	return `usage: fleet agents [command] [options]

Discovery & Inspection:
  ls       [--device D] [--repo R] [--harness H] [--state S] [--waiting] [--all] [--json]
  inspect  TARGET [--tail N] [--json]
  paths    [add|rm PATH] [--device D] [--repo R] [--q TEXT] [--json]

Peer Messaging:
  send     TO [MESSAGE...] [--message TEXT] [--stdin] [--session SID] [--thread ID] [--reply-to ID] [--json]
  inbox    [--session SID] [--all] [--no-mark-read] [--json]
  ask      TO [QUESTION...] [--question TEXT] [--stdin] [--session SID] [--timeout S] [--wait-if-idle] [--json]
  rename   [TARGET] NAME [--session SID] [--json]
  listen   [--session SID] [--json]

Lifecycle & Relay:
  spawn    --harness claude|agy|codex [--name NAME] [--device DEV] [--repo PATH]
           [--prompt TEXT | --prompt-file PATH | --stdin] [--policy auto|bypass|safe]
           [--model MODEL] [--strategy least_agents|round_robin] [--tmux] [--rex] [--local] [--json]
  kill     TARGET [--session SID] [--force] [--json]
  kill-all --yes [--harness claude|agy|codex|all] [--device DEV] [--json]
  wait     --session SID [--harness H] [--timeout S] [--poll-timeout S]
  relay    [--harness H[,H2...]] [--poll-timeout S] [--agentapi PATH]
`
}

func resolveCLICaller(explicitSID string) (sid, harness, cwd string) {
	cwd, _ = os.Getwd()
	sid = strings.TrimSpace(explicitSID)
	if sid == "" {
		sid = strings.TrimSpace(os.Getenv("FLEET_SESSION_ID"))
	}
	if sid == "" {
		if convID := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID")); convID != "" {
			sid = convID
			harness = "antigravity"
		}
	}
	if harness == "" && sid != "" {
		parts := strings.SplitN(sid, ":", 3)
		if len(parts) >= 2 && paths.ValidHarness(parts[1]) {
			harness = parts[1]
		}
	}
	if harness == "" {
		switch {
		case os.Getenv("CLAUDECODE") != "":
			harness = "claude"
		case os.Getenv("CODEX_THREAD_ID") != "":
			harness = "codex"
		case os.Getenv("ANTIGRAVITY_CONVERSATION_ID") != "":
			harness = "antigravity"
		default:
			harness = "claude"
		}
	}
	return sid, harness, cwd
}

func readTextArg(pos []string, flagVal string, fromStdin bool, cio *IO) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if len(pos) == 1 && pos[0] == "-" {
		raw, err := io.ReadAll(cio.Stdin)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(raw), "\r\n"), nil
	}
	if len(pos) > 0 {
		return strings.Join(pos, " "), nil
	}
	if fromStdin {
		raw, err := io.ReadAll(cio.Stdin)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(raw), "\r\n"), nil
	}
	return "", fmt.Errorf("text is required")
}

func localDeviceName() string {
	n, err := client.LoadNode()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(n.Name)
}

func expandUserHomePath(p string) string {
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil && h != "" {
			return h
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil && h != "" {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func postLocalReceipt(api, jobID, sessionName, backend, status, errMsg string) {
	if api == "" || jobID == "" {
		return
	}
	_ = client.Call(api, http.MethodPost, "/v1/slacker/worker/receipt", slacker.Receipt{
		JobID:       jobID,
		ReceiptID:   fmt.Sprintf("local-%s-%d", jobID, time.Now().UnixNano()),
		SessionName: sessionName,
		Backend:     backend,
		Status:      status,
		Error:       errMsg,
	}, 10*time.Second, nil, nil)
}

func randomAgentHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func formatWaitDuration(secs int) string {
	if secs < 0 {
		secs = 0
	}
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	mins := secs / 60
	if mins < 60 {
		return fmt.Sprintf("%dm", mins)
	}
	hours, rem := mins/60, mins%60
	if rem > 0 {
		return fmt.Sprintf("%dh%dm", hours, rem)
	}
	return fmt.Sprintf("%dh", hours)
}

func formatAgentState(ag agents.Session) string {
	switch {
	case !ag.Live:
		return ag.State + " (stale)"
	case ag.State == "waiting_permission" || ag.State == "waiting_question":
		var tags []string
		if ag.WaitingSeconds != nil {
			tags = append(tags, formatWaitDuration(*ag.WaitingSeconds))
		}
		if ag.Listening {
			tags = append(tags, "listening")
		}
		if len(tags) > 0 {
			return fmt.Sprintf("%s (%s)", ag.State, strings.Join(tags, ", "))
		}
	case ag.Listening:
		return ag.State + " (listening)"
	}
	return ag.State
}

func formatAgentTitle(ag agents.Session) string {
	baseTitle := strings.TrimSpace(strings.ReplaceAll(cmp.Or(ag.Title, ag.Summary), "\n", " "))
	if len(ag.PendingAction) == 0 {
		return agents.TruncateRunes(baseTitle, 48)
	}
	tool := strings.TrimSpace(ag.PendingAction["tool"])
	detail := strings.TrimSpace(strings.ReplaceAll(ag.PendingAction["detail"], "\n", " "))
	snippet := ""
	switch {
	case tool != "" && detail != "":
		snippet = fmt.Sprintf("[%s: %s]", tool, detail)
	case tool != "" || detail != "":
		snippet = fmt.Sprintf("[%s]", cmp.Or(tool, detail))
	}
	if snippet != "" && baseTitle != "" {
		return agents.TruncateRunes(snippet+" "+baseTitle, 60)
	}
	return agents.TruncateRunes(cmp.Or(snippet, baseTitle), 60)
}

func cmdAgentsList(args []string, cio *IO) error {
	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	all := fs.Bool("all", false, "include stale and exited sessions")
	waiting := fs.Bool("waiting", false, "show only sessions waiting on permission or user input")
	state := fs.String("state", "", "filter by session state")
	device := fs.String("device", "", "filter by fleet device name")
	repo := fs.String("repo", "", "filter by repository name")
	harness := fs.String("harness", "", "filter by harness (claude, codex, antigravity)")
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unknown agents subcommand or argument: %s", strings.Join(pos, " "))
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	q := url.Values{}
	if *all {
		q.Set("include_stale", "true")
	}
	if *device != "" {
		q.Set("device", *device)
	}
	if *repo != "" {
		q.Set("repo", *repo)
	}
	if *harness != "" {
		q.Set("harness", *harness)
	}
	stateFilter := *state
	if *waiting {
		stateFilter = "waiting"
	}
	if stateFilter != "" {
		q.Set("state", stateFilter)
	}
	qs := ""
	if enc := q.Encode(); enc != "" {
		qs = "?" + enc
	}
	var resp struct {
		Agents []agents.Session `json:"agents"`
	}
	if err := client.Call(api, http.MethodGet, "/v1/agents"+qs, nil, 30*time.Second, nil, &resp); err != nil {
		return err
	}
	if *jsonOut {
		return writeJSONOut(cio.Stdout, resp)
	}
	if len(resp.Agents) == 0 {
		cio.printf("no active agent sessions\n")
	} else {
		rows := [][]string{{"NAME", "DEVICE", "HARNESS", "REPO", "STATE", "LAST SEEN", "TITLE", "SESSION"}}
		for _, ag := range resp.Agents {
			repoLabel := ag.Repo
			if ag.Repo != "" && ag.Branch != "" {
				repoLabel = ag.Repo + "@" + ag.Branch
			}
			ls := ag.UpdatedAt
			rows = append(rows, []string{
				cmp.Or(ag.Name, "-"), ag.Device, ag.Harness, cmp.Or(repoLabel, "-"),
				formatAgentState(ag), ago(&ls), formatAgentTitle(ag), ag.SessionID,
			})
		}
		printTable(cio.Stdout, rows)
	}
	var uResp struct {
		Usage []agents.HarnessUsage `json:"usage"`
	}
	if uErr := client.Call(api, http.MethodGet, "/v1/usage", nil, 3*time.Second, nil, &uResp); uErr == nil {
		if footer := formatCompactUsageFooter(uResp.Usage); footer != "" {
			cio.printf("\n%s\n", footer)
		}
	}
	return nil
}

func cmdAgentsInspect(args []string, cio *IO) error {
	fs := flag.NewFlagSet("agents inspect", flag.ContinueOnError)
	tailLimit := fs.Int("tail", -1, "max recent tail entries")
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: fleet agents inspect TARGET [--tail N] [--json]")
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var resp struct {
		Agent agents.SessionDetail `json:"agent"`
	}
	if err := client.Call(api, http.MethodGet, "/v1/agents/"+url.PathEscape(pos[0]), nil, 30*time.Second, nil, &resp); err != nil {
		return err
	}
	if *tailLimit >= 0 && len(resp.Agent.Tail) > *tailLimit {
		resp.Agent.Tail = resp.Agent.Tail[len(resp.Agent.Tail)-*tailLimit:]
	}
	if *jsonOut {
		return writeJSONOut(cio.Stdout, resp)
	}
	ag := resp.Agent
	cio.printf("session:   %s\nname:      %s\ndevice:    %s\nharness:   %s\nstate:     %s\ndelivery:  %s (listening=%v)\ncwd:       %s\nrepo:      %s (%s)\ntitle:     %s\n",
		ag.SessionID, cmp.Or(ag.Name, "-"), ag.Device, ag.Harness, formatAgentState(ag.Session),
		cmp.Or(ag.Delivery, "-"), ag.Listening, cmp.Or(ag.CWD, "-"), cmp.Or(ag.Repo, "-"), cmp.Or(ag.Branch, "-"), cmp.Or(ag.Title, ag.Summary, "-"))
	if len(ag.PendingAction) > 0 {
		cio.printf("pending:   %s (%s): %s\n", ag.PendingAction["kind"], ag.PendingAction["tool"], ag.PendingAction["detail"])
	}
	if ag.GitStatus != "" {
		cio.printf("\ngit status:\n%s\n", ag.GitStatus)
	}
	if len(ag.Tail) > 0 {
		cio.printf("\nrecent tail (%d):\n", len(ag.Tail))
		for _, it := range ag.Tail {
			cio.printf("  [%s] %s\n", it.Role, it.Text)
		}
	}
	return nil
}

func cmdAgentsSend(args []string, cio *IO) error {
	fs := flag.NewFlagSet("agents send", flag.ContinueOnError)
	sessionFlag := fs.String("session", "", "caller session_id")
	msgFlag := fs.String("message", "", "message text")
	threadID := fs.String("thread", "", "thread_id to continue")
	replyTo := fs.Int64("reply-to", 0, "message id being replied to")
	fromStdin := fs.Bool("stdin", false, "read message from stdin")
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return fmt.Errorf("usage: fleet agents send TO [MESSAGE...] [--message TEXT] [--stdin] [--session SID] [--thread ID] [--reply-to ID] [--json]")
	}
	to := pos[0]
	text, err := readTextArg(pos[1:], *msgFlag, *fromStdin, cio)
	if err != nil {
		return fmt.Errorf("usage: fleet agents send TO [MESSAGE...] [--message TEXT] [--stdin]: %w", err)
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	sid, harness, cwd := resolveCLICaller(*sessionFlag)
	body := map[string]any{"to": to, "body": text, "harness": harness, "cwd": cwd}
	if sid != "" {
		body["session_id"] = sid
	}
	if *threadID != "" {
		body["thread_id"] = *threadID
	}
	if *replyTo > 0 {
		body["reply_to"] = *replyTo
	}
	var res agents.SendResult
	if err := client.Call(api, http.MethodPost, "/v1/agents/send", body, 30*time.Second, nil, &res); err != nil {
		return err
	}
	if *jsonOut {
		return writeJSONOut(cio.Stdout, res)
	}
	cio.printf("sent id=%d thread=%s delivery=%s\n", res.Message.ID, res.Message.ThreadID, res.Delivery)
	if res.Warning != nil && *res.Warning != "" {
		cio.printf("warning: %s\n", *res.Warning)
	}
	return nil
}

func cmdAgentsInbox(args []string, cio *IO) error {
	fs := flag.NewFlagSet("agents inbox", flag.ContinueOnError)
	sessionFlag := fs.String("session", "", "caller session_id")
	all := fs.Bool("all", false, "include already-read messages")
	noMarkRead := fs.Bool("no-mark-read", false, "do not mark returned messages as read")
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("usage: fleet agents inbox [--session SID] [--all] [--no-mark-read] [--json]")
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	sid, harness, cwd := resolveCLICaller(*sessionFlag)
	body := map[string]any{
		"harness":     harness,
		"cwd":         cwd,
		"unread_only": !*all,
		"mark_read":   !*noMarkRead,
	}
	if sid != "" {
		body["session_id"] = sid
	}
	var res struct {
		SessionID string           `json:"session_id"`
		Messages  []agents.Message `json:"messages"`
	}
	if err := client.Call(api, http.MethodPost, "/v1/agents/inbox", body, 30*time.Second, nil, &res); err != nil {
		return err
	}
	if *jsonOut {
		return writeJSONOut(cio.Stdout, res)
	}
	if len(res.Messages) == 0 {
		cio.printf("inbox empty (%s)\n", res.SessionID)
		return nil
	}
	for _, m := range res.Messages {
		from := cmp.Or(m.FromName, m.FromSession)
		cio.printf("id=%d thread=%s from=%s: %s\n", m.ID, m.ThreadID, from, m.Body)
	}
	return nil
}

func cmdAgentsAsk(args []string, cio *IO) error {
	fs := flag.NewFlagSet("agents ask", flag.ContinueOnError)
	sessionFlag := fs.String("session", "", "caller session_id")
	questionFlag := fs.String("question", "", "question text")
	timeoutSec := fs.Float64("timeout", 30.0, "max seconds to wait for reply (max 120)")
	waitIfIdle := fs.Bool("wait-if-idle", false, "wait even if target is idle with no listener")
	fromStdin := fs.Bool("stdin", false, "read question from stdin")
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return fmt.Errorf("usage: fleet agents ask TO [QUESTION...] [--question TEXT] [--stdin] [--session SID] [--timeout S] [--wait-if-idle] [--json]")
	}
	to := pos[0]
	question, err := readTextArg(pos[1:], *questionFlag, *fromStdin, cio)
	if err != nil {
		return fmt.Errorf("usage: fleet agents ask TO [QUESTION...] [--question TEXT] [--stdin]: %w", err)
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	sid, harness, cwd := resolveCLICaller(*sessionFlag)
	body := map[string]any{
		"to":           to,
		"body":         question,
		"harness":      harness,
		"cwd":          cwd,
		"timeout":      *timeoutSec,
		"wait_if_idle": *waitIfIdle,
	}
	if sid != "" {
		body["session_id"] = sid
	}
	httpTimeout := time.Duration(max(min(*timeoutSec, 120.0)+10.0, 5.0) * float64(time.Second))
	var res agents.AskResult
	if err := client.Call(api, http.MethodPost, "/v1/agents/ask", body, httpTimeout, nil, &res); err != nil {
		return err
	}
	if *jsonOut {
		return writeJSONOut(cio.Stdout, res)
	}
	if res.Reply != nil {
		from := cmp.Or(res.Reply.FromName, res.Reply.FromSession)
		cio.printf("reply from %s (id=%d thread=%s): %s\n", from, res.Reply.ID, res.Reply.ThreadID, res.Reply.Body)
		return nil
	}
	cio.printf("sent id=%d thread=%s delivery=%s timed_out=%v\n", res.Sent.ID, res.Sent.ThreadID, res.Delivery, res.TimedOut)
	if res.Warning != nil && *res.Warning != "" {
		cio.printf("warning: %s\n", *res.Warning)
	}
	return nil
}

func cmdAgentsRename(args []string, cio *IO) error {
	fs := flag.NewFlagSet("agents rename", flag.ContinueOnError)
	sessionFlag := fs.String("session", "", "caller session_id (when renaming self)")
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 && len(pos) != 2 {
		return fmt.Errorf("usage: fleet agents rename [TARGET] NAME [--session SID] [--json]")
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var payload map[string]any
	if len(pos) == 2 {
		var infoResp struct {
			Agent agents.SessionDetail `json:"agent"`
		}
		if err := client.Call(api, http.MethodGet, "/v1/agents/"+url.PathEscape(pos[0]), nil, 30*time.Second, nil, &infoResp); err != nil {
			return err
		}
		payload = map[string]any{"session_id": infoResp.Agent.SessionID, "name": pos[1]}
	} else {
		sid, harness, cwd := resolveCLICaller(*sessionFlag)
		payload = map[string]any{"name": pos[0], "harness": harness, "cwd": cwd}
		if sid != "" {
			payload["session_id"] = sid
		}
	}
	var res struct {
		SessionID string `json:"session_id"`
		Name      string `json:"name"`
	}
	if err := client.Call(api, http.MethodPost, "/v1/agents/rename", payload, 30*time.Second, nil, &res); err != nil {
		return err
	}
	if *jsonOut {
		return writeJSONOut(cio.Stdout, res)
	}
	cio.printf("%s -> %s\n", res.SessionID, res.Name)
	return nil
}

func cmdAgentsListen(args []string, cio *IO) error {
	fs := flag.NewFlagSet("agents listen", flag.ContinueOnError)
	sessionFlag := fs.String("session", "", "caller session_id")
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("usage: fleet agents listen [--session SID] [--json]")
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	sid, harness, cwd := resolveCLICaller(*sessionFlag)
	if sid == "" {
		var inboxResp struct {
			SessionID string `json:"session_id"`
		}
		if err := client.Call(api, http.MethodPost, "/v1/agents/inbox", map[string]any{
			"harness": harness, "cwd": cwd, "unread_only": false, "mark_read": false,
		}, 30*time.Second, nil, &inboxResp); err != nil {
			return err
		}
		sid = inboxResp.SessionID
	}
	var detailResp struct {
		Agent agents.SessionDetail `json:"agent"`
	}
	_ = client.Call(api, http.MethodGet, "/v1/agents/"+url.PathEscape(sid), nil, 15*time.Second, nil, &detailResp)

	relayMode := "agent_relay"
	relayLoaded := system.UserAgentLoaded(paths.AgentRelayLabel)
	if detailResp.Agent.Harness == "antigravity" || harness == "antigravity" {
		relayMode = "antigravity_sidecar"
		relayLoaded = detailResp.Agent.Listening
	}
	out := map[string]any{
		"session_id":   sid,
		"harness":      cmp.Or(detailResp.Agent.Harness, harness),
		"name":         detailResp.Agent.Name,
		"listening":    detailResp.Agent.Listening,
		"delivery":     cmp.Or(detailResp.Agent.Delivery, "offline"),
		"relay_mode":   relayMode,
		"relay_loaded": relayLoaded,
	}
	if *jsonOut {
		return writeJSONOut(cio.Stdout, out)
	}
	cio.printf("session=%s name=%s listening=%v delivery=%s mode=%s relay_loaded=%v\n",
		sid, cmp.Or(detailResp.Agent.Name, "-"), detailResp.Agent.Listening,
		cmp.Or(detailResp.Agent.Delivery, "offline"), relayMode, relayLoaded)
	return nil
}

func runLocalSpawn(harness, name, dir, model, prompt string, pol session.Policy, useRex, useTmux, jsonOut bool, cio *IO) error {
	sessName := strings.TrimSpace(name)
	if sessName == "" {
		sessName = "agent-" + harness + "-" + randomAgentHex(4)
	}
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return err
		}
	}
	dir = expandUserHomePath(dir)
	var outBuf, errBuf strings.Builder
	backend, err := sessionStartFn(session.StartOpts{
		Harness:   harness,
		Name:      sessName,
		Directory: dir,
		UseRex:    useRex,
		UseTmux:   useTmux,
		Model:     model,
		Policy:    pol,
		Prompt:    prompt,
		Stdout:    &outBuf,
		Stderr:    &errBuf,
	})
	if err != nil {
		return err
	}
	if jsonOut {
		return writeJSONOut(cio.Stdout, map[string]any{
			"executed_locally": true,
			"backend":          backend,
			"session_name":     sessName,
			"harness":          harness,
			"directory":        dir,
		})
	}
	if outBuf.Len() > 0 {
		_, _ = io.WriteString(cio.Stdout, outBuf.String())
	} else {
		cio.printf("Started %s session %s (%s) in %s\n", harness, sessName, backend, dir)
	}
	return nil
}

func readSpawnPrompt(promptFlag, promptFile string, fromStdin bool, cio *IO) (string, error) {
	if promptFlag != "" {
		return promptFlag, nil
	}
	if promptFile != "" {
		b, err := os.ReadFile(promptFile)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	if fromStdin {
		b, err := io.ReadAll(cio.Stdin)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	return "", nil
}

func runLocalSpawnFallback(api string, job slacker.Job, fallbackPol session.Policy, localDev string, jsonOut bool, cio *IO) error {
	dir := expandUserHomePath(job.RepoPath)
	jobPol, _ := session.ParsePolicy(job.Policy, fallbackPol)
	var outBuf, errBuf strings.Builder
	backend, lErr := sessionStartFn(session.StartOpts{
		Harness:   job.Harness,
		Name:      job.SessionName,
		Directory: dir,
		UseTmux:   job.UseTmux,
		Model:     job.Model,
		Policy:    jobPol,
		Prompt:    job.Prompt,
		Stdout:    &outBuf,
		Stderr:    &errBuf,
	})
	if lErr != nil {
		postLocalReceipt(api, job.ID, job.SessionName, "", "failed", lErr.Error())
		return lErr
	}
	postLocalReceipt(api, job.ID, job.SessionName, backend, "launched", "")
	if jsonOut {
		return writeJSONOut(cio.Stdout, map[string]any{
			"job":              job,
			"worker_online":    false,
			"executed_locally": true,
			"backend":          backend,
		})
	}
	cio.printf("spawned %s (%s) locally on %s via %s (job=%s)\n", job.SessionName, job.Harness, localDev, backend, job.ID)
	return nil
}

func validateSpawnInputs(harnessRaw, name, policyRaw string, useRex, useTmux bool) (string, session.Policy, error) {
	harness, ok := session.NormalizeHarness(harnessRaw)
	if !ok {
		return "", "", fmt.Errorf("unknown harness %q (expected claude, agy, or codex)", harnessRaw)
	}
	if name != "" {
		if err := session.ValidateName(name); err != nil {
			return "", "", err
		}
	}
	pol, err := session.ParsePolicy(policyRaw, session.PolicyAuto)
	if err != nil {
		return "", "", err
	}
	if useRex && useTmux {
		return "", "", fmt.Errorf("--rex and --tmux cannot be combined")
	}
	return harness, pol, nil
}

func dispatchSpawnRequest(harness string, pol session.Policy, name, deviceFlag, strategy, model, repoPath, prompt string, useTmux, useRex, localOnly, jsonOut bool, cio *IO) error {
	cwd, _ := os.Getwd()
	devs := parseCommaList(deviceFlag)
	localDev := localDeviceName()
	targetsLocal := len(devs) == 0 || (len(devs) == 1 && localDev != "" && devs[0] == localDev)

	if localOnly || useRex {
		return runLocalSpawn(harness, name, cmp.Or(repoPath, cwd), model, prompt, pol, useRex, useTmux, jsonOut, cio)
	}
	api, apiErr := client.ResolveAPI("")
	if apiErr != nil {
		if targetsLocal {
			return runLocalSpawn(harness, name, cmp.Or(repoPath, cwd), model, prompt, pol, false, useTmux, jsonOut, cio)
		}
		return apiErr
	}
	if repoPath == "" && targetsLocal {
		repoPath = cwd
	}
	var spawnResp struct {
		Job          slacker.Job `json:"job"`
		WorkerOnline bool        `json:"worker_online"`
	}
	err := client.Call(api, http.MethodPost, "/v1/agents/spawn", map[string]any{
		"harness":   harness,
		"name":      strings.TrimSpace(name),
		"devices":   devs,
		"strategy":  strings.TrimSpace(strategy),
		"policy":    string(pol),
		"model":     strings.TrimSpace(model),
		"repo_path": repoPath,
		"prompt":    prompt,
		"use_tmux":  useTmux,
	}, 30*time.Second, nil, &spawnResp)
	if err != nil {
		var ae *client.APIError
		if targetsLocal && errors.As(err, &ae) && ae.StatusCode == 0 {
			return runLocalSpawn(harness, name, cmp.Or(repoPath, cwd), model, prompt, pol, false, useTmux, jsonOut, cio)
		}
		return err
	}
	if !spawnResp.WorkerOnline && localDev != "" && spawnResp.Job.TargetDevice == localDev {
		return runLocalSpawnFallback(api, spawnResp.Job, pol, localDev, jsonOut, cio)
	}
	if jsonOut {
		return writeJSONOut(cio.Stdout, spawnResp)
	}
	cio.printf("queued spawn job=%s session=%s harness=%s device=%s worker_online=%v\n",
		spawnResp.Job.ID, spawnResp.Job.SessionName, spawnResp.Job.Harness, spawnResp.Job.TargetDevice, spawnResp.WorkerOnline)
	if !spawnResp.WorkerOnline {
		cio.printf("warning: worker on %s is offline; session will start when `fleet slacker worker` connects\n", spawnResp.Job.TargetDevice)
	}
	return nil
}

func cmdAgentsSpawn(args []string, cio *IO) error {
	fs := flag.NewFlagSet("agents spawn", flag.ContinueOnError)
	harnessFlag := fs.String("harness", "", "target harness (claude, agy, codex)")
	nameFlag := fs.String("name", "", "optional session name")
	deviceFlag := fs.String("device", "", "target fleet device (or comma-separated list)")
	repoFlag := fs.String("repo", "", "target repository path or workspace subpath")
	dirFlag := fs.String("dir", "", "alias for --repo")
	promptFlag := fs.String("prompt", "", "initial task prompt")
	promptFile := fs.String("prompt-file", "", "read initial prompt from file")
	fromStdin := fs.Bool("stdin", false, "read initial prompt from stdin")
	policyFlag := fs.String("policy", "", "permission policy (auto|bypass|safe)")
	modelFlag := fs.String("model", "", "model override (for agy app sessions)")
	strategyFlag := fs.String("strategy", "", "multi-device strategy (least_agents|round_robin)")
	useTmux := fs.Bool("tmux", false, "force tmux backend")
	useRex := fs.Bool("rex", false, "force Rex backend (local only)")
	localOnly := fs.Bool("local", false, "launch directly on this machine without queueing")
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 || strings.TrimSpace(*harnessFlag) == "" {
		return fmt.Errorf("usage: fleet agents spawn --harness claude|agy|codex [--name NAME] [--device DEV] [--repo PATH] [--prompt TEXT] [--policy auto|bypass|safe] [--model MODEL] [--tmux] [--local] [--json]")
	}
	harness, pol, err := validateSpawnInputs(*harnessFlag, *nameFlag, *policyFlag, *useRex, *useTmux)
	if err != nil {
		return err
	}
	prompt, err := readSpawnPrompt(*promptFlag, *promptFile, *fromStdin, cio)
	if err != nil {
		return err
	}
	repoPath := cmp.Or(strings.TrimSpace(*repoFlag), strings.TrimSpace(*dirFlag))
	return dispatchSpawnRequest(harness, pol, *nameFlag, *deviceFlag, *strategyFlag, *modelFlag, repoPath, prompt, *useTmux, *useRex, *localOnly, *jsonOut, cio)
}

func cmdAgentsKill(args []string, cio *IO) error {
	fs := flag.NewFlagSet("agents kill", flag.ContinueOnError)
	sessionFlag := fs.String("session", "", "caller session_id (for self-kill guard)")
	force := fs.Bool("force", false, "allow killing caller's own session")
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: fleet agents kill TARGET [--session SID] [--force] [--json]")
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var detResp struct {
		Agent agents.SessionDetail `json:"agent"`
	}
	if err := client.Call(api, http.MethodGet, "/v1/agents/"+url.PathEscape(pos[0]), nil, 30*time.Second, nil, &detResp); err != nil {
		return err
	}
	detail := detResp.Agent
	callerSID, _, _ := resolveCLICaller(*sessionFlag)
	if callerSID != "" && callerSID == detail.SessionID && !*force {
		return fmt.Errorf("refusing to kill caller's own session %s (pass --force to override)", callerSID)
	}

	var killResp struct {
		OK           bool        `json:"ok"`
		SessionID    string      `json:"session_id"`
		Device       string      `json:"device"`
		Job          slacker.Job `json:"job"`
		WorkerOnline bool        `json:"worker_online"`
	}
	if err := client.Call(api, http.MethodPost, "/v1/agents/kill", map[string]string{
		"session_id": detail.SessionID,
	}, 30*time.Second, nil, &killResp); err != nil {
		return err
	}

	executedLocally := false
	localDev := localDeviceName()
	if !killResp.WorkerOnline && localDev != "" && killResp.Device == localDev {
		normH, ok := session.NormalizeHarness(detail.Harness)
		if !ok {
			normH = detail.Harness
		}
		var outBuf, errBuf strings.Builder
		kErr := sessionKillAgentFn(session.KillTarget{
			SessionID: detail.SessionID,
			Harness:   normH,
			Name:      detail.Name,
			FullRawID: detail.FullRawID,
			PID:       detail.PID,
		}, &outBuf, &errBuf)
		if kErr != nil {
			postLocalReceipt(api, killResp.Job.ID, killResp.Job.SessionName, "kill", "failed", kErr.Error())
			return kErr
		}
		postLocalReceipt(api, killResp.Job.ID, killResp.Job.SessionName, "kill", "launched", "")
		executedLocally = true
	}

	if *jsonOut {
		return writeJSONOut(cio.Stdout, map[string]any{
			"ok":               killResp.OK,
			"session_id":       killResp.SessionID,
			"device":           killResp.Device,
			"job":              killResp.Job,
			"worker_online":    killResp.WorkerOnline,
			"executed_locally": executedLocally,
		})
	}
	cio.printf("killed %s on %s (job=%s executed_locally=%v)\n", killResp.SessionID, killResp.Device, killResp.Job.ID, executedLocally)
	return nil
}

func fetchLocalKillTargets(api, localDev, harnessFilter string) []session.KillTarget {
	q := url.Values{"device": {localDev}}
	if h := strings.ToLower(strings.TrimSpace(harnessFilter)); h != "" && h != "all" {
		if norm, ok := session.NormalizeHarness(h); ok {
			if norm == session.HarnessAgy {
				q.Set("harness", "antigravity")
			} else {
				q.Set("harness", norm)
			}
		}
	}
	var resp struct {
		Agents []agents.Session `json:"agents"`
	}
	if err := client.Call(api, http.MethodGet, "/v1/agents?"+q.Encode(), nil, 15*time.Second, nil, &resp); err != nil {
		return nil
	}
	targets := make([]session.KillTarget, 0, len(resp.Agents))
	for _, s := range resp.Agents {
		h, ok := session.NormalizeHarness(s.Harness)
		if !ok {
			h = s.Harness
		}
		targets = append(targets, session.KillTarget{
			SessionID: s.SessionID, Harness: h, Name: s.Name, FullRawID: s.FullRawID, PID: s.PID,
		})
	}
	return targets
}

func cmdAgentsKillAll(args []string, cio *IO) error {
	fs := flag.NewFlagSet("agents kill-all", flag.ContinueOnError)
	harnessFlag := fs.String("harness", "", "harness filter (claude, agy, codex, or all)")
	deviceFlag := fs.String("device", "", "device filter")
	yes := fs.Bool("yes", false, "confirm killing all matching agent sessions")
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("usage: fleet agents kill-all --yes [--harness claude|agy|codex|all] [--device DEV] [--json]")
	}
	if !*yes {
		return fmt.Errorf("pass --yes to confirm killing all matching agent sessions")
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}

	localDev := localDeviceName()
	var localTargets []session.KillTarget
	if localDev != "" && (*deviceFlag == "" || *deviceFlag == localDev) {
		localTargets = fetchLocalKillTargets(api, localDev, *harnessFlag)
	}

	var kaResp struct {
		OK             bool          `json:"ok"`
		Harness        string        `json:"harness"`
		Deregistered   int           `json:"deregistered"`
		Devices        []string      `json:"devices"`
		OfflineDevices []string      `json:"offline_devices"`
		Jobs           []slacker.Job `json:"jobs"`
	}
	if err := client.Call(api, http.MethodPost, "/v1/agents/kill-all", map[string]string{
		"harness": strings.TrimSpace(*harnessFlag),
		"device":  strings.TrimSpace(*deviceFlag),
	}, 30*time.Second, nil, &kaResp); err != nil {
		return err
	}

	executedLocally := false
	if localDev != "" && slices.Contains(kaResp.OfflineDevices, localDev) {
		var outBuf, errBuf strings.Builder
		kErr := sessionKillAllFn(*harnessFlag, localTargets, &outBuf, &errBuf)
		for _, j := range kaResp.Jobs {
			if j.TargetDevice == localDev {
				if kErr != nil {
					postLocalReceipt(api, j.ID, j.SessionName, "kill-all", "failed", kErr.Error())
				} else {
					postLocalReceipt(api, j.ID, j.SessionName, "kill-all", "launched", "")
				}
			}
		}
		if kErr != nil {
			return kErr
		}
		executedLocally = true
	}

	if *jsonOut {
		return writeJSONOut(cio.Stdout, map[string]any{
			"ok":               kaResp.OK,
			"harness":          kaResp.Harness,
			"deregistered":     kaResp.Deregistered,
			"devices":          kaResp.Devices,
			"offline_devices":  kaResp.OfflineDevices,
			"jobs":             kaResp.Jobs,
			"executed_locally": executedLocally,
		})
	}
	cio.printf("killed %d session(s) across %v (executed_locally=%v)\n", kaResp.Deregistered, kaResp.Devices, executedLocally)
	return nil
}

func cmdAgentsWait(args []string, cio *IO) error {
	fs := flag.NewFlagSet("agents wait", flag.ContinueOnError)
	sessionFlag := fs.String("session", "", "session_id to wait on")
	harness := fs.String("harness", "", "harness override")
	timeout := fs.Float64("timeout", 0.0, "max wait seconds (0 = wait indefinitely)")
	pollTimeout := fs.Float64("poll-timeout", 50.0, "per-request long-poll seconds")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 || *sessionFlag == "" {
		return fmt.Errorf("usage: fleet agents wait --session SESSION_ID [--harness H] [--timeout S]")
	}
	if *harness != "" && !paths.ValidHarness(*harness) {
		return fmt.Errorf("unknown harness %q", *harness)
	}
	rc, err := waiter.WaitForInboxCLI(waiter.WaitCLIOpts{
		SessionID:   *sessionFlag,
		Harness:     *harness,
		MaxWait:     *timeout,
		PollTimeout: *pollTimeout,
		Stdout:      cio.Stdout,
		WatchParent: true,
	})
	if err != nil {
		return err
	}
	if rc != 0 {
		return fmt.Errorf("wait failed")
	}
	return nil
}

func parseRelayHarnessesArg(raw string) ([]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return []string{"antigravity"}, nil
	}
	if strings.EqualFold(trimmed, "all") {
		return slices.Clone(paths.Harnesses), nil
	}
	var out []string
	for part := range strings.SplitSeq(trimmed, ",") {
		p := strings.ToLower(strings.TrimSpace(part))
		if p == "agy" {
			p = "antigravity"
		}
		if p == "" {
			continue
		}
		if !paths.ValidHarness(p) {
			return nil, fmt.Errorf("unknown harness %q", part)
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{"antigravity"}, nil
	}
	return out, nil
}

func cmdAgentsRelay(args []string, cio *IO) error {
	fs := flag.NewFlagSet("agents relay", flag.ContinueOnError)
	harness := fs.String("harness", "antigravity", "comma-separated harnesses (claude,codex,antigravity, or all)")
	pollTimeout := fs.Float64("poll-timeout", 50.0, "per-request long-poll seconds")
	agentAPI := fs.String("agentapi", "", "path to agentapi executable")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("usage: fleet agents relay [--harness antigravity|claude,codex|all] [--poll-timeout S] [--agentapi PATH]")
	}
	harnesses, err := parseRelayHarnessesArg(*harness)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var rc int
	if len(harnesses) == 1 && harnesses[0] == "antigravity" {
		rc, err = waiter.RunAntigravityRelay(ctx, waiter.RelayOpts{
			Harness:     "antigravity",
			AgentAPIBin: *agentAPI,
			PollTimeout: *pollTimeout,
			Stderr:      cio.Stderr,
		})
	} else {
		rc, err = waiter.RunRelay(ctx, waiter.MultiRelayOpts{
			Harnesses:   harnesses,
			AgentAPIBin: *agentAPI,
			PollTimeout: *pollTimeout,
			Stderr:      cio.Stderr,
		})
	}
	if err != nil {
		return err
	}
	if rc != 0 {
		return fmt.Errorf("relay failed")
	}
	return nil
}

func cmdAgents(args []string, cio *IO) error {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "help":
			_, _ = io.WriteString(cio.Stdout, agentsUsageText())
			return nil
		case "ls":
			return cmdAgentsList(args[1:], cio)
		case "inspect":
			return cmdAgentsInspect(args[1:], cio)
		case "paths":
			return cmdAgentsPaths(args[1:], cio)
		case "send":
			return cmdAgentsSend(args[1:], cio)
		case "inbox":
			return cmdAgentsInbox(args[1:], cio)
		case "ask":
			return cmdAgentsAsk(args[1:], cio)
		case "rename":
			return cmdAgentsRename(args[1:], cio)
		case "listen":
			return cmdAgentsListen(args[1:], cio)
		case "spawn":
			return cmdAgentsSpawn(args[1:], cio)
		case "kill":
			return cmdAgentsKill(args[1:], cio)
		case "kill-all":
			return cmdAgentsKillAll(args[1:], cio)
		case "wait":
			return cmdAgentsWait(args[1:], cio)
		case "relay":
			return cmdAgentsRelay(args[1:], cio)
		}
	}
	return cmdAgentsList(args, cio)
}

func cmdAgentsPathsList(args []string, cio *IO) error {
	fs := flag.NewFlagSet("agents paths", flag.ContinueOnError)
	device := fs.String("device", "", "filter by fleet device name")
	query := fs.String("q", "", "filter by path, repo, or device substring")
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("usage: fleet agents paths [--device D] [--q TEXT] [--json]")
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	q := url.Values{}
	if *device != "" {
		q.Set("device", *device)
	}
	if *query != "" {
		q.Set("q", *query)
	}
	qs := ""
	if enc := q.Encode(); enc != "" {
		qs = "?" + enc
	}
	var resp struct {
		Paths      []agents.TrackedPath `json:"paths"`
		Repos      []string             `json:"repos"`
		Workspaces map[string][]string  `json:"workspaces"`
	}
	if err := client.Call(api, http.MethodGet, "/v1/agents/paths"+qs, nil, 15*time.Second, nil, &resp); err != nil {
		return err
	}
	if *jsonOut {
		return writeJSONOut(cio.Stdout, resp)
	}
	if len(resp.Paths) == 0 {
		cio.printf("no tracked directories or repos yet\n")
		return nil
	}
	rows := [][]string{{"DEVICE", "REPO", "SUBPATH", "PATH", "USES", "SOURCE", "LAST SEEN"}}
	for _, p := range resp.Paths {
		ls := p.LastSeen
		rows = append(rows, []string{
			p.Device, cmp.Or(p.Repo, "-"), cmp.Or(p.Subpath, "-"), p.Path,
			fmt.Sprintf("%d", p.UseCount), cmp.Or(p.Source, "-"), ago(&ls),
		})
	}
	printTable(cio.Stdout, rows)
	return nil
}

func cmdAgentsPathsAdd(args []string, cio *IO) error {
	fs := flag.NewFlagSet("agents paths add", flag.ContinueOnError)
	device := fs.String("device", "", "target fleet device (defaults to local device)")
	repo := fs.String("repo", "", "optional repository name")
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: fleet agents paths add PATH [--device D] [--repo R] [--json]")
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	var res map[string]any
	if err := client.Call(api, http.MethodPost, "/v1/agents/paths", map[string]string{
		"device": strings.TrimSpace(*device),
		"path":   pos[0],
		"repo":   strings.TrimSpace(*repo),
	}, 15*time.Second, nil, &res); err != nil {
		return err
	}
	if *jsonOut {
		return writeJSONOut(cio.Stdout, res)
	}
	repoStr, _ := res["repo"].(string)
	cio.printf("tracked %s:%s (repo=%s)\n", res["device"], res["path"], cmp.Or(repoStr, "-"))
	return nil
}

func cmdAgentsPathsRemove(args []string, cio *IO) error {
	fs := flag.NewFlagSet("agents paths rm", flag.ContinueOnError)
	device := fs.String("device", "", "target fleet device (defaults to local device)")
	jsonOut := fs.Bool("json", false, "output JSON")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: fleet agents paths rm PATH [--device D] [--json]")
	}
	api, err := client.ResolveAPI("")
	if err != nil {
		return err
	}
	q := url.Values{"path": {pos[0]}}
	if *device != "" {
		q.Set("device", strings.TrimSpace(*device))
	}
	var res struct {
		OK      bool `json:"ok"`
		Deleted bool `json:"deleted"`
	}
	if err := client.Call(api, http.MethodDelete, "/v1/agents/paths?"+q.Encode(), nil, 15*time.Second, nil, &res); err != nil {
		return err
	}
	if *jsonOut {
		return writeJSONOut(cio.Stdout, res)
	}
	cio.printf("removed=%v\n", res.Deleted)
	return nil
}

func cmdAgentsPaths(args []string, cio *IO) error {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "add":
			return cmdAgentsPathsAdd(args[1:], cio)
		case "rm", "remove", "delete":
			return cmdAgentsPathsRemove(args[1:], cio)
		case "ls":
			return cmdAgentsPathsList(args[1:], cio)
		}
	}
	return cmdAgentsPathsList(args, cio)
}
