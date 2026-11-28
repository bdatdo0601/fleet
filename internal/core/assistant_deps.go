package core

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/assistant"
	"github.com/bdatdo0601/fleet/internal/events"
	"github.com/bdatdo0601/fleet/internal/memory"
	"github.com/bdatdo0601/fleet/internal/slacker"
	"github.com/bdatdo0601/fleet/internal/tlsdns"
)

type assistantDeps struct {
	fleet *Fleet
}

func (d *assistantDeps) ListAgents(device, repo, harness, state string, includeStale bool) (any, error) {
	return d.fleet.Agents.ListSessions(device, repo, harness, state, includeStale)
}

func (d *assistantDeps) InspectAgent(target string) (any, error) {
	return d.fleet.Agents.GetSession(target)
}

func (d *assistantDeps) SendAgentMessage(fromChannel, to, body, threadID string, replyTo *int64) (any, error) {
	known, err := d.fleet.ActiveDeviceNames()
	if err != nil {
		return nil, err
	}
	var tid *string
	if strings.TrimSpace(threadID) != "" {
		clean := strings.TrimSpace(threadID)
		tid = &clean
	}
	return d.fleet.Agents.SendMessageWithReport(agents.AssistantDevice, agents.AssistantSessionID(fromChannel), to, body, tid, replyTo, known)
}

func (d *assistantDeps) AskAgent(ctx context.Context, fromChannel, to, question string, timeoutSec float64) (any, error) {
	known, err := d.fleet.ActiveDeviceNames()
	if err != nil {
		return nil, err
	}
	return d.fleet.Agents.Ask(ctx, agents.AskParams{
		Device:       agents.AssistantDevice,
		FromSession:  agents.AssistantSessionID(fromChannel),
		To:           to,
		Body:         question,
		Timeout:      timeoutSec,
		KnownDevices: known,
	})
}

func (d *assistantDeps) ListPaths(device, query string, limit int) (any, error) {
	list, err := d.fleet.Agents.ListPaths(device, query)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(list) > limit {
		list = list[:limit]
	}
	return list, nil
}

func (d *assistantDeps) SpawnAgent(ctx context.Context, req assistant.SpawnAgentRequest) (assistant.SpawnAgentResult, error) {
	res, err := d.fleet.SpawnAgent(ctx, InternalAdminCaller(), SpawnAgentRequest{
		Harness:  req.Harness,
		Name:     req.Name,
		Device:   req.Device,
		RepoPath: req.RepoPath,
		Prompt:   req.Prompt,
		Model:    req.Model,
		Policy:   req.Policy,
		UseTmux:  req.UseTmux,
	})
	if err != nil {
		return assistant.SpawnAgentResult{}, err
	}
	return assistant.SpawnAgentResult{
		JobID:        res.Job.ID,
		TargetDevice: res.Job.TargetDevice,
		Harness:      res.Job.Harness,
		SessionName:  res.Job.SessionName,
		RepoPath:     res.Job.RepoPath,
		Status:       res.Job.Status,
		WorkerOnline: res.WorkerOnline,
	}, nil
}

func (d *assistantDeps) KillAgent(ctx context.Context, sessionID string) (any, error) {
	return d.fleet.KillAgentContext(ctx, InternalAdminCaller(), sessionID)
}

func toAssistantWatchInfo(w agents.Watch) assistant.AgentWatchInfo {
	ch, ok := agents.ParseAssistantTarget(w.WatcherSession)
	if !ok {
		ch = "general"
	}
	return assistant.AgentWatchInfo{
		ID:            w.ID,
		ChannelID:     ch,
		TargetSession: w.TargetSession,
		TargetDevice:  w.TargetDevice,
		TargetName:    w.TargetName,
		Events:        w.Events,
		Once:          w.Once,
		CreatedAt:     w.CreatedAt,
		UpdatedAt:     w.UpdatedAt,
	}
}

func (d *assistantDeps) WatchAgent(req assistant.AgentWatchRequest) (assistant.AgentWatchInfo, error) {
	w, err := d.fleet.Agents.CreateWatch(agents.CreateWatchParams{
		WatcherDevice:  agents.AssistantDevice,
		WatcherSession: agents.AssistantSessionID(req.ChannelID),
		Target:         req.Target,
		Events:         req.Events,
		Once:           req.Once,
		AllowPending:   true,
	})
	if err != nil {
		return assistant.AgentWatchInfo{}, err
	}
	return toAssistantWatchInfo(w), nil
}

func (d *assistantDeps) ListWatches(channelID string) ([]assistant.AgentWatchInfo, error) {
	ws := ""
	if strings.TrimSpace(channelID) != "" {
		ws = agents.AssistantSessionID(channelID)
	}
	list, err := d.fleet.Agents.ListWatches(agents.AssistantDevice, ws, "")
	if err != nil {
		return nil, err
	}
	out := make([]assistant.AgentWatchInfo, 0, len(list))
	for _, w := range list {
		out = append(out, toAssistantWatchInfo(w))
	}
	return out, nil
}

func (d *assistantDeps) DeleteWatch(channelID, idOrTarget string) (int, error) {
	ws := ""
	if strings.TrimSpace(channelID) != "" {
		ws = agents.AssistantSessionID(channelID)
	}
	clean := strings.TrimSpace(idOrTarget)
	if strings.HasPrefix(clean, "w-") {
		return d.fleet.Agents.DeleteWatch(agents.AssistantDevice, true, clean, ws, "")
	}
	return d.fleet.Agents.DeleteWatch(agents.AssistantDevice, true, "", ws, clean)
}

func (d *assistantDeps) SweepStaleAgents(now time.Time) {
	if d.fleet.Agents != nil {
		d.fleet.Agents.SweepStaleSessions(now)
	}
}

func (d *assistantDeps) SystemHealth() (map[string]any, error) {
	out := map[string]any{}
	if devs, err := d.fleet.List(); err == nil {
		cutoff := time.Now().UTC().Add(14 * 24 * time.Hour)
		var expiring []map[string]any
		for _, dev := range devs {
			if dev.RevokedAt != nil && *dev.RevokedAt != "" {
				continue
			}
			if exp, pErr := time.Parse(time.RFC3339, dev.NotAfter); pErr == nil && !exp.After(cutoff) {
				expiring = append(expiring, map[string]any{"name": dev.Name, "not_after": dev.NotAfter})
			}
		}
		out["devices_total"] = len(devs)
		out["expiring_device_certs"] = expiring
	}
	if neb, err := ReadNebulaHealth(d.fleet.Root); err == nil {
		out["nebula"] = neb
	}
	if tlsCfg, err := d.fleet.TLSConfig(); err == nil && tlsCfg != nil {
		cert := tlsdns.InspectCert(d.fleet.Root+"/tls", tlsCfg.Domain, time.Now().UTC())
		out["tls"] = map[string]any{"domain": tlsCfg.Domain, "cert": cert}
	}
	if d.fleet.Slacker != nil {
		counts, _ := d.fleet.Slacker.JobCounts()
		rts, _ := d.fleet.Slacker.ListRuntimes()
		out["slacker_jobs"] = counts
		out["slacker_runtimes"] = rts
	}
	if d.fleet.Agents != nil {
		if usage, err := d.fleet.Agents.ListUsage(); err == nil {
			out["usage"] = usage
		}
	}
	return out, nil
}

func (d *assistantDeps) LighthouseWorkerStatus() assistant.LighthouseWorkerStatus {
	lh, err := d.fleet.Registry.Lighthouse()
	if err != nil || lh == nil || lh.Name == "" {
		return assistant.LighthouseWorkerStatus{}
	}
	st := assistant.LighthouseWorkerStatus{Device: lh.Name}
	if d.fleet.Slacker != nil {
		online, _ := d.fleet.Slacker.OnlineWorkers(time.Now(), slacker.WorkerOnlineWindow)
		st.WorkerOnline = online[lh.Name]
		if rts, rErr := d.fleet.Slacker.ListRuntimes(); rErr == nil {
			role := "worker:" + lh.Name
			for _, r := range rts {
				if r.Role == role {
					st.LastPollAt = r.LastPollAt
					break
				}
			}
		}
	}
	if cfg, cErr := slacker.LoadConfig(d.fleet.SlackerConfigPath); cErr == nil {
		st.InDefaultDevices = slices.Contains(cfg.DefaultDevices, lh.Name)
	}
	return st
}

func (d *assistantDeps) MemorySearch(query, scope, kind, tier string, limit int) (any, error) {
	return d.fleet.Memory.Search(memory.SearchFilter{
		Query: query,
		Scope: scope,
		Kind:  kind,
		Tier:  tier,
		Limit: limit,
	})
}

func (d *assistantDeps) MemoryGet(scope, key string) (any, error) {
	return d.fleet.Memory.Get(scope, key, false)
}

func (d *assistantDeps) MemorySet(scope, key, kind, tier, summary, body string) (any, error) {
	res, err := d.fleet.Memory.Upsert(memory.UpsertParams{
		Scope:   cmp.Or(scope, "global"),
		Key:     key,
		Kind:    cmp.Or(kind, "reference"),
		Tier:    cmp.Or(tier, "searchable"),
		Summary: summary,
		Body:    body,
		Source:  "fleet",
		Device:  "lighthouse",
		Harness: "assistant",
	})
	if err != nil {
		return nil, err
	}
	if res.Queued && d.fleet.Curator != nil {
		d.fleet.Curator.Notify()
	}
	return res, nil
}

func (d *assistantDeps) MemoryContext(repo, device string) (string, error) {
	res, err := d.fleet.Memory.RenderContext(repo, device)
	if err != nil {
		return "", err
	}
	return res.Rendered, nil
}

func (d *assistantDeps) ListMemoryProposals(status string, limit int) (any, error) {
	return d.fleet.Memory.ListProposals(status, limit)
}

func (d *assistantDeps) DecideMemoryProposal(id int64, approve bool) (any, error) {
	prop, item, err := d.fleet.Memory.ResolveProposal(id, approve, "assistant")
	if err != nil {
		return nil, err
	}
	return map[string]any{"proposal": prop, "memory": item}, nil
}

func (d *assistantDeps) slackClient() *slacker.SlackClient {
	if d.fleet.SlackClient != nil {
		return d.fleet.SlackClient
	}
	return &slacker.SlackClient{}
}

func (d *assistantDeps) SlackChannels(ctx context.Context, account string, includeAll bool, limit int) (any, error) {
	cfg, err := slacker.LoadConfig(d.fleet.SlackerConfigPath)
	if err != nil {
		return nil, err
	}
	acctFilter := strings.ToLower(strings.TrimSpace(account))
	mapped := make([]slacker.ChannelMapping, 0, len(cfg.Channels))
	for _, ch := range cfg.SortedChannels() {
		if acctFilter != "" && cmp.Or(ch.Account, slacker.DefaultAccountID) != acctFilter {
			continue
		}
		mapped = append(mapped, ch)
	}
	res := map[string]any{"mapped_channels": mapped}
	if includeAll {
		acct, rErr := cfg.ResolveAccount(acctFilter, "")
		if rErr != nil {
			return nil, rErr
		}
		if acct.UserToken == "" {
			return nil, newFleetErr(fmt.Sprintf("account %q has no user_token configured", acct.ID), http.StatusBadRequest)
		}
		chs, lErr := d.slackClient().ListUserChannels(ctx, acct.UserToken, limit)
		if lErr != nil {
			return nil, lErr
		}
		res["channels"] = chs
	}
	return res, nil
}

func (d *assistantDeps) SlackRead(ctx context.Context, account, channel, threadTS, query string, limit int) (any, error) {
	cfg, err := slacker.LoadConfig(d.fleet.SlackerConfigPath)
	if err != nil {
		return nil, err
	}
	acct, err := cfg.ResolveAccount(account, channel)
	if err != nil {
		return nil, err
	}
	if acct.UserToken == "" {
		return nil, newFleetErr(fmt.Sprintf("account %q has no user_token configured", acct.ID), http.StatusBadRequest)
	}
	return d.slackClient().ReadMessages(ctx, acct.UserToken, slacker.ReadMessagesOpts{
		ChannelID: channel,
		ThreadTS:  threadTS,
		Query:     query,
		Limit:     limit,
	})
}

func (d *assistantDeps) SlackSend(ctx context.Context, account, channel, text, threadTS string) (any, error) {
	cfg, err := slacker.LoadConfig(d.fleet.SlackerConfigPath)
	if err != nil {
		return nil, err
	}
	acct, err := cfg.ResolveAccount(account, channel)
	if err != nil {
		return nil, err
	}
	if !cfg.CanSendToChannel(acct.ID, channel) {
		return nil, newFleetErr(fmt.Sprintf("channel %q is not mapped on account %q", channel, acct.ID), http.StatusForbidden)
	}
	tok := cmp.Or(acct.BotToken, acct.UserToken)
	if tok == "" {
		return nil, newFleetErr(fmt.Sprintf("account %q has no user_token or bot_token configured", acct.ID), http.StatusBadRequest)
	}
	return d.slackClient().PostMessage(ctx, tok, channel, text, threadTS)
}

func (d *assistantDeps) ListEvents(f events.Filter) ([]events.Event, error) {
	if d.fleet.Events == nil {
		return nil, nil
	}
	return d.fleet.Events.List(f)
}

func (d *assistantDeps) PublishEvent(ev events.Event) (events.Event, bool, error) {
	if d.fleet.Events == nil {
		return events.Event{}, false, nil
	}
	return d.fleet.Events.Publish(ev)
}

func (d *assistantDeps) SubscribeEvents() chan events.Event {
	if d.fleet.Events == nil {
		return make(chan events.Event)
	}
	return d.fleet.Events.Subscribe()
}

func (d *assistantDeps) UnsubscribeEvents(ch chan events.Event) {
	if d.fleet.Events != nil {
		d.fleet.Events.Unsubscribe(ch)
	}
}
