package server

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/registry"
	"github.com/bdatdo0601/fleet/internal/session"
	"github.com/bdatdo0601/fleet/internal/slacker"
)

func recordSlackerConfigPaths(fleet *core.Fleet, cfg slacker.Config) {
	if fleet.Agents == nil {
		return
	}
	for dev, dc := range cfg.Devices {
		if dc.WorkspaceDir != "" {
			_ = fleet.Agents.RecordPath(dev, dc.WorkspaceDir, "", "slacker_config", false)
		}
	}
	for _, ch := range cfg.Channels {
		for _, dev := range cfg.EffectiveChannelDevices(ch) {
			if resolved, err := cfg.ResolveRepoPath(dev, ch.RepoPath); err == nil && resolved != "" {
				_ = fleet.Agents.RecordPath(dev, resolved, agents.RepoNameFromPath(resolved), "slacker_config", false)
			}
		}
	}
}

func recordSlackerJobPath(fleet *core.Fleet, job slacker.Job, bump bool) {
	if fleet.Agents == nil || job.RepoPath == "" || job.Reaction == "kill" || job.Reaction == "kill-all" {
		return
	}
	_ = fleet.Agents.RecordPath(job.TargetDevice, job.RepoPath, agents.RepoNameFromPath(job.RepoPath), "slacker_job", bump)
}

func buildSlackerSummary(fleet *core.Fleet) map[string]any {
	cfg, err := slacker.LoadConfig(fleet.SlackerConfigPath)
	if err != nil {
		return map[string]any{"configured": false, "enabled": false, "accounts": 0}
	}
	view := cfg.View()
	counts, _ := fleet.Slacker.JobCounts()
	runtimes, _ := fleet.Slacker.ListRuntimes()
	listenerConnected := false
	for _, rt := range runtimes {
		if (rt.Role == "listener" || strings.HasPrefix(rt.Role, "listener:")) && rt.Connected {
			listenerConnected = true
			break
		}
	}
	return map[string]any{
		"enabled":            view.Enabled,
		"configured":         view.Configured,
		"accounts":           len(view.Accounts),
		"listener_connected": listenerConnected,
		"channels":           len(view.Channels),
		"emojis":             len(view.Emojis),
		"queued_jobs":        counts["queued"],
		"failed_jobs":        counts["failed"],
	}
}

func slackerClient(fleet *core.Fleet) *slacker.SlackClient {
	if fleet.SlackClient != nil {
		return fleet.SlackClient
	}
	return &slacker.SlackClient{}
}

func verifyAccountTokens(ctx context.Context, sc *slacker.SlackClient, acct *slacker.AccountConfig) error {
	if acct.UserToken != "" {
		authRes, err := sc.AuthTest(ctx, acct.UserToken)
		if err != nil {
			return &core.FleetError{Msg: fmt.Sprintf("Slack[%s] auth.test failed: %s", acct.ID, err.Error()), Status: http.StatusBadGateway}
		}
		if acct.AuthorizedUserID == "" {
			acct.AuthorizedUserID = authRes.UserID
		}
		acct.AuthorizedUser = cmp.Or(acct.AuthorizedUser, authRes.User)
		acct.TeamID = cmp.Or(acct.TeamID, authRes.TeamID)
		acct.TeamName = cmp.Or(acct.TeamName, authRes.Team)
	}
	if acct.BotToken != "" {
		if _, err := sc.AuthTest(ctx, acct.BotToken); err != nil {
			return &core.FleetError{Msg: fmt.Sprintf("Slack[%s] bot auth.test failed: %s", acct.ID, err.Error()), Status: http.StatusBadGateway}
		}
	}
	if acct.AppToken != "" {
		if _, err := sc.OpenSocketURL(ctx, acct.AppToken); err != nil {
			return &core.FleetError{Msg: fmt.Sprintf("Slack[%s] apps.connections.open failed: %s", acct.ID, err.Error()), Status: http.StatusBadGateway}
		}
	}
	return nil
}

func verifySlackerUpdate(ctx context.Context, fleet *core.Fleet, upd slacker.ConfigUpdate, next *slacker.Config) error {
	sc := slackerClient(fleet)
	targets := map[string]bool{}
	for rawID := range upd.Accounts {
		if id, err := slacker.NormalizeAccountID(rawID); err == nil {
			targets[id] = true
		}
	}
	if upd.Account != "" || len(targets) == 0 {
		if id, err := slacker.NormalizeAccountID(upd.Account); err == nil {
			if _, exists := next.Accounts[id]; exists {
				targets[id] = true
			}
		}
	}
	for id := range targets {
		acct, ok := next.Accounts[id]
		if !ok {
			continue
		}
		if err := verifyAccountTokens(ctx, sc, &acct); err != nil {
			return err
		}
		next.Accounts[id] = acct
	}
	return nil
}

func registerSlackerRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	registerSlackerAdminRoutes(mux, fleet, route)
	registerSlackerJobRoutes(mux, fleet, route)
	registerSlackerWorkerRoutes(mux, fleet, route)
	registerSlackerAgentRoutes(mux, fleet, route)
	registerAgentSpawnRoute(mux, fleet, route)
}

func registerSlackerAdminRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("GET /v1/slacker/status", route(true, func(w http.ResponseWriter, _ *http.Request, _ *registry.Device) error {
		cfg, err := slacker.LoadConfig(fleet.SlackerConfigPath)
		if err != nil {
			return err
		}
		runtimes, err := fleet.Slacker.ListRuntimes()
		if err != nil {
			return err
		}
		counts, err := fleet.Slacker.JobCounts()
		if err != nil {
			return err
		}
		pendingCounts, err := fleet.Slacker.PendingJobCounts()
		if err != nil {
			pendingCounts = map[string]int{}
		}
		agentCounts := map[string]int{}
		if fleet.Agents != nil {
			if sessions, lErr := fleet.Agents.ListSessions("", "", "", "", false); lErr == nil {
				for _, s := range sessions {
					if s.Device != "" {
						agentCounts[s.Device]++
					}
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"config":         cfg.View(),
			"runtimes":       runtimes,
			"counts":         counts,
			"agent_counts":   agentCounts,
			"pending_counts": pendingCounts,
		})
		return nil
	}))

	mux.HandleFunc("GET /v1/slacker/config", route(true, func(w http.ResponseWriter, _ *http.Request, _ *registry.Device) error {
		cfg, err := slacker.LoadConfig(fleet.SlackerConfigPath)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"config": cfg.View(), "manifest_yaml": slacker.ManifestYAML()})
		return nil
	}))

	mux.HandleFunc("POST /v1/slacker/config", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var upd slacker.ConfigUpdate
		if err := readJSONBody(r, &upd); err != nil {
			return err
		}
		prev, err := slacker.LoadConfig(fleet.SlackerConfigPath)
		if err != nil {
			return err
		}
		activeDevs, err := fleet.ActiveDeviceNames()
		if err != nil {
			return err
		}
		next, err := slacker.ApplyUpdate(prev, upd, activeDevs)
		if err != nil {
			return err
		}
		if upd.Verify {
			if err := verifySlackerUpdate(r.Context(), fleet, upd, &next); err != nil {
				return err
			}
		}
		if err := slacker.SaveConfig(fleet.SlackerConfigPath, next); err != nil {
			return err
		}
		recordSlackerConfigPaths(fleet, next)
		writeJSON(w, http.StatusOK, map[string]any{"config": next.View()})
		return nil
	}))
}

func registerSlackerJobRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("GET /v1/slacker/jobs", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		q := r.URL.Query()
		jobs, err := fleet.Slacker.ListJobs(q.Get("device"), q.Get("status"), queryInt(r, "limit"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
		return nil
	}))

	mux.HandleFunc("GET /v1/slacker/jobs/{id}", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		job, err := fleet.Slacker.GetJob(r.PathValue("id"))
		if err != nil {
			return err
		}
		if job == nil {
			return &core.FleetError{Msg: "job not found", Status: http.StatusNotFound}
		}
		writeJSON(w, http.StatusOK, map[string]any{"job": job})
		return nil
	}))

	mux.HandleFunc("POST /v1/slacker/jobs/{id}/retry", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		job, err := fleet.Slacker.RetryJob(r.PathValue("id"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"job": job})
		return nil
	}))

	mux.HandleFunc("POST /v1/slacker/jobs/{id}/cancel", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		job, err := fleet.Slacker.CancelJob(r.PathValue("id"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"job": job})
		return nil
	}))

	mux.HandleFunc("POST /v1/slacker/cleanup", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			OlderThan   string `json:"older_than"`
			AllTerminal bool   `json:"all_terminal"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		var olderThan time.Duration
		if req.OlderThan != "" {
			d, err := time.ParseDuration(req.OlderThan)
			if err != nil || d < 0 {
				return &core.FleetError{Msg: "invalid older_than duration", Status: http.StatusBadRequest}
			}
			olderThan = d
		}
		prunedJobs, prunedWorkers, err := fleet.Slacker.Cleanup(olderThan, req.AllTerminal)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"pruned_jobs": prunedJobs, "pruned_workers": prunedWorkers})
		return nil
	}))
}

func registerSlackerWorkerRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("POST /v1/slacker/notify", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			Device string `json:"device"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		if req.Device != "" {
			fleet.Slacker.NotifyDevice(req.Device)
		} else {
			fleet.Slacker.NotifyAll()
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))

	mux.HandleFunc("POST /v1/slacker/worker/poll", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		var req struct {
			Timeout *float64 `json:"timeout"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		timeoutSec := optFloat(req.Timeout, 30.0)
		if timeoutSec <= 0 {
			timeoutSec = 0.2
		}
		if timeoutSec > 55.0 {
			timeoutSec = 55.0
		}
		job, err := fleet.Slacker.WaitForJob(r.Context(), c.Name, time.Duration(timeoutSec*float64(time.Second)), 90*time.Second)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			return err
		}
		if job != nil {
			recordSlackerJobPath(fleet, *job, false)
		}
		writeJSON(w, http.StatusOK, map[string]any{"job": job, "device": c.Name})
		return nil
	}))

	mux.HandleFunc("POST /v1/slacker/worker/receipt", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		var rec slacker.Receipt
		if err := readJSONBody(r, &rec); err != nil {
			return err
		}
		job, err := fleet.Slacker.RecordReceipt(c.Name, rec)
		if err != nil {
			return err
		}
		if rec.Status == "launched" {
			recordSlackerJobPath(fleet, job, true)
		}
		if fleet.Assistant != nil {
			fleet.Assistant.OnSlackerReceipt(rec.JobID, c.Name, job.SessionName, rec.Status, rec.Error)
		}
		writeJSON(w, http.StatusOK, map[string]any{"job": job})
		return nil
	}))
}

func registerSlackerAgentRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("GET /v1/slacker/channels", route(false, handleSlackerChannels(fleet)))
	mux.HandleFunc("GET /v1/slacker/messages", route(false, handleSlackerMessages(fleet)))
	mux.HandleFunc("POST /v1/slacker/send", route(false, handleSlackerSend(fleet)))
}

func handleSlackerChannels(fleet *core.Fleet) func(http.ResponseWriter, *http.Request, *registry.Device) error {
	return func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		cfg, err := slacker.LoadConfig(fleet.SlackerConfigPath)
		if err != nil {
			return err
		}
		q := r.URL.Query()
		acctFilter := strings.ToLower(strings.TrimSpace(q.Get("account")))
		mapped := make([]slacker.ChannelMapping, 0, len(cfg.Channels))
		for _, ch := range cfg.SortedChannels() {
			if acctFilter != "" && cmp.Or(ch.Account, slacker.DefaultAccountID) != acctFilter {
				continue
			}
			mapped = append(mapped, ch)
		}
		view := cfg.View()
		accounts := make([]slacker.AccountView, 0, len(view.Accounts))
		for _, a := range cfg.SortedAccounts() {
			accounts = append(accounts, view.Accounts[a.ID])
		}
		resp := map[string]any{
			"accounts":        accounts,
			"mapped_channels": mapped,
		}
		if queryBool(r, "include_all") {
			acct, rErr := cfg.ResolveAccount(acctFilter, "")
			if rErr != nil {
				return rErr
			}
			if acct.UserToken == "" {
				return &core.FleetError{Msg: fmt.Sprintf("account %q has no user_token configured", acct.ID), Status: http.StatusBadRequest}
			}
			chs, lErr := slackerClient(fleet).ListUserChannels(r.Context(), acct.UserToken, queryInt(r, "limit"))
			if lErr != nil {
				return &core.FleetError{Msg: "Slack users.conversations failed: " + lErr.Error(), Status: http.StatusBadGateway}
			}
			resp["account"] = acct.ID
			resp["channels"] = chs
		}
		writeJSON(w, http.StatusOK, resp)
		return nil
	}
}

func handleSlackerMessages(fleet *core.Fleet) func(http.ResponseWriter, *http.Request, *registry.Device) error {
	return func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		cfg, err := slacker.LoadConfig(fleet.SlackerConfigPath)
		if err != nil {
			return err
		}
		q := r.URL.Query()
		channelID := strings.TrimSpace(q.Get("channel"))
		acct, err := cfg.ResolveAccount(q.Get("account"), channelID)
		if err != nil {
			return err
		}
		if acct.UserToken == "" {
			return &core.FleetError{Msg: fmt.Sprintf("account %q has no user_token configured", acct.ID), Status: http.StatusBadRequest}
		}
		res, err := slackerClient(fleet).ReadMessages(r.Context(), acct.UserToken, slacker.ReadMessagesOpts{
			ChannelID: channelID,
			ThreadTS:  q.Get("thread_ts"),
			Query:     q.Get("query"),
			Oldest:    q.Get("oldest"),
			Latest:    q.Get("latest"),
			Limit:     queryInt(r, "limit"),
		})
		if err != nil {
			if se, ok := errors.AsType[*slacker.SlackerError](err); ok {
				return se
			}
			return &core.FleetError{Msg: "Slack read failed: " + err.Error(), Status: http.StatusBadGateway}
		}
		res.Account = acct.ID
		writeJSON(w, http.StatusOK, res)
		return nil
	}
}

func handleSlackerSend(fleet *core.Fleet) func(http.ResponseWriter, *http.Request, *registry.Device) error {
	return func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			Account  string `json:"account"`
			Channel  string `json:"channel"`
			Text     string `json:"text"`
			ThreadTS string `json:"thread_ts"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		channelID := strings.TrimSpace(req.Channel)
		if channelID == "" || strings.TrimSpace(req.Text) == "" {
			return &core.FleetError{Msg: "channel and text are required", Status: http.StatusBadRequest}
		}
		cfg, err := slacker.LoadConfig(fleet.SlackerConfigPath)
		if err != nil {
			return err
		}
		acct, err := cfg.ResolveAccount(req.Account, channelID)
		if err != nil {
			return err
		}
		if !cfg.CanSendToChannel(acct.ID, channelID) {
			return &core.FleetError{
				Msg:    fmt.Sprintf("channel %q is not mapped on account %q (map it or enable allow_unmapped_send)", channelID, acct.ID),
				Status: http.StatusForbidden,
			}
		}
		tok := cmp.Or(acct.BotToken, acct.UserToken)
		if tok == "" {
			return &core.FleetError{Msg: fmt.Sprintf("account %q has no user_token or bot_token configured", acct.ID), Status: http.StatusBadRequest}
		}
		posted, err := slackerClient(fleet).PostMessage(r.Context(), tok, channelID, req.Text, req.ThreadTS)
		if err != nil {
			return &core.FleetError{Msg: "Slack chat.postMessage failed: " + err.Error(), Status: http.StatusBadGateway}
		}
		posted.Account = acct.ID
		writeJSON(w, http.StatusOK, posted)
		return nil
	}
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func registerAgentSpawnRoute(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("POST /v1/agents/spawn", route(false, func(w http.ResponseWriter, r *http.Request, caller *registry.Device) error {
		var req core.SpawnAgentRequest
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		res, err := fleet.SpawnAgent(r.Context(), caller, req)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}))

	mux.HandleFunc("POST /v1/agents/kill", route(false, func(w http.ResponseWriter, r *http.Request, caller *registry.Device) error {
		var req struct {
			SessionID string `json:"session_id"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		res, err := fleet.KillAgent(caller, req.SessionID)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}))

	mux.HandleFunc("POST /v1/agents/kill-all", route(false, handleAgentKillAll(fleet)))
}

func resolveKillAllParams(fleet *core.Fleet, caller *registry.Device, rawHarness, rawDevice string) (agentHarness, sessHarness, targetDevice string, err error) {
	cleanHarness := strings.ToLower(strings.TrimSpace(rawHarness))
	sessHarness = "all"
	if cleanHarness != "" && cleanHarness != "all" {
		norm, ok := session.NormalizeHarness(cleanHarness)
		if !ok {
			return "", "", "", &core.FleetError{Msg: fmt.Sprintf("unknown harness %q (expected claude, agy, codex, or all)", rawHarness), Status: http.StatusBadRequest}
		}
		sessHarness = norm
		if norm == session.HarnessAgy {
			agentHarness = "antigravity"
		} else {
			agentHarness = norm
		}
	}
	targetDevice = strings.TrimSpace(rawDevice)
	if !core.IsAdmin(caller) {
		if targetDevice != "" && targetDevice != caller.Name {
			return "", "", "", &core.FleetError{Msg: fmt.Sprintf("%s cannot kill agents on %s", caller.Name, targetDevice), Status: http.StatusForbidden}
		}
		targetDevice = caller.Name
	} else if targetDevice != "" {
		activeDevs, aErr := fleet.ActiveDeviceNames()
		if aErr != nil {
			return "", "", "", aErr
		}
		if !activeDevs[targetDevice] {
			return "", "", "", &core.FleetError{Msg: fmt.Sprintf("unknown or revoked device %q", targetDevice), Status: http.StatusBadRequest}
		}
	}
	return agentHarness, sessHarness, targetDevice, nil
}

func handleAgentKillAll(fleet *core.Fleet) func(http.ResponseWriter, *http.Request, *registry.Device) error {
	return func(w http.ResponseWriter, r *http.Request, caller *registry.Device) error {
		var req struct {
			Harness string `json:"harness"`
			Device  string `json:"device"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		agentHarness, sessHarness, targetDevice, err := resolveKillAllParams(fleet, caller, req.Harness, req.Device)
		if err != nil {
			return err
		}

		liveSessions, err := fleet.Agents.ListSessions(targetDevice, "", agentHarness, "", false)
		if err != nil {
			return err
		}
		targetsByDevice := map[string][]session.KillTarget{}
		for _, s := range liveSessions {
			h, ok := session.NormalizeHarness(s.Harness)
			if !ok {
				h = s.Harness
			}
			targetsByDevice[s.Device] = append(targetsByDevice[s.Device], session.KillTarget{
				SessionID: s.SessionID,
				Harness:   h,
				Name:      s.Name,
				FullRawID: s.FullRawID,
				PID:       s.PID,
			})
		}

		onlineWorkers, _ := fleet.Slacker.OnlineWorkers(time.Now(), slacker.WorkerOnlineWindow)
		var targetDevices []string
		if targetDevice != "" {
			targetDevices = []string{targetDevice}
		} else {
			devSet := map[string]bool{}
			for d := range targetsByDevice {
				devSet[d] = true
			}
			for d, online := range onlineWorkers {
				if online {
					devSet[d] = true
				}
			}
			for d := range devSet {
				targetDevices = append(targetDevices, d)
			}
			slices.Sort(targetDevices)
		}

		callerName := "admin"
		if caller != nil && caller.Name != "" {
			callerName = caller.Name
		}
		var jobs []slacker.Job
		var offlineDevices []string
		for _, dev := range targetDevices {
			targetsJSON, _ := json.Marshal(targetsByDevice[dev])
			_, job, jErr := fleet.Slacker.EnqueueJob(slacker.Job{
				Account:      "ui",
				EventID:      "ui-killall-" + dev + "-" + randomHex(6),
				ChannelName:  "web-ui",
				Reaction:     "kill-all",
				UserID:       callerName,
				TargetDevice: dev,
				Harness:      sessHarness,
				SessionName:  "kill-all-" + sessHarness,
				Prompt:       string(targetsJSON),
			})
			if jErr != nil {
				return jErr
			}
			jobs = append(jobs, job)
			if !onlineWorkers[dev] {
				offlineDevices = append(offlineDevices, dev)
			}
		}
		for _, s := range liveSessions {
			_ = fleet.Agents.DeregisterAny(s.SessionID)
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"ok":              true,
			"harness":         cmp.Or(agentHarness, "all"),
			"deregistered":    len(liveSessions),
			"devices":         targetDevices,
			"offline_devices": offlineDevices,
			"jobs":            jobs,
		})
		return nil
	}
}
