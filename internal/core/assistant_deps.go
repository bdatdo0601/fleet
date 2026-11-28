package core

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/bdatdo0601/fleet/internal/assistant"
	"github.com/bdatdo0601/fleet/internal/memory"
	"github.com/bdatdo0601/fleet/internal/slacker"
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

func (d *assistantDeps) SendAgentMessage(fromDevice, to, body string) (any, error) {
	known, err := d.fleet.ActiveDeviceNames()
	if err != nil {
		return nil, err
	}
	return d.fleet.Agents.SendMessageWithReport(fromDevice, "", to, body, nil, nil, known)
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
	res, err := d.fleet.SpawnAgent(ctx, nil, SpawnAgentRequest{
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

func (d *assistantDeps) KillAgent(_ context.Context, sessionID string) (any, error) {
	return d.fleet.KillAgent(nil, sessionID)
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
