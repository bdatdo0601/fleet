package core

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/registry"
	"github.com/bdatdo0601/fleet/internal/session"
	"github.com/bdatdo0601/fleet/internal/slacker"
)

// SpawnAgentRequest configures Fleet.SpawnAgent.
type SpawnAgentRequest struct {
	Harness  string   `json:"harness"`
	Name     string   `json:"name"`
	Device   string   `json:"device"`
	Devices  []string `json:"devices"`
	Strategy string   `json:"strategy"`
	Policy   string   `json:"policy"`
	Model    string   `json:"model"`
	RepoPath string   `json:"repo_path"`
	Prompt   string   `json:"prompt"`
	UseTmux  bool     `json:"use_tmux"`
}

// SpawnAgentResult reports the queued Slacker job and target worker availability.
type SpawnAgentResult struct {
	Job          slacker.Job `json:"job"`
	WorkerOnline bool        `json:"worker_online"`
}

// KillAgentResult reports a queued Slacker kill job.
type KillAgentResult struct {
	OK           bool        `json:"ok"`
	SessionID    string      `json:"session_id"`
	Device       string      `json:"device"`
	Job          slacker.Job `json:"job"`
	WorkerOnline bool        `json:"worker_online"`
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (f *Fleet) resolveSpawnDevices(caller *registry.Device, req SpawnAgentRequest, cfg slacker.Config) ([]string, error) {
	var rawDevs []string
	if len(req.Devices) > 0 {
		rawDevs = req.Devices
	} else if strings.TrimSpace(req.Device) != "" {
		rawDevs = []string{req.Device}
	}
	seen := make(map[string]bool, len(rawDevs))
	var devs []string
	for _, d := range rawDevs {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		devs = append(devs, d)
	}
	if len(devs) > 0 {
		activeDevs, err := f.ActiveDeviceNames()
		if err != nil {
			return nil, err
		}
		for _, d := range devs {
			if !activeDevs[d] {
				return nil, newFleetErr(fmt.Sprintf("unknown or revoked device %q", d), http.StatusBadRequest)
			}
			if !IsAdmin(caller) && caller != nil && d != caller.Name {
				return nil, newFleetErr(fmt.Sprintf("%s cannot spawn agents on %s", caller.Name, d), http.StatusForbidden)
			}
		}
		return devs, nil
	}
	if (!IsAdmin(caller) || len(cfg.DefaultDevices) == 0) && caller != nil && caller.Name != "" {
		return []string{caller.Name}, nil
	}
	return nil, nil
}

// SpawnAgent queues a Slacker spawn job on the selected Mac device and records tracked paths.
func (f *Fleet) SpawnAgent(ctx context.Context, caller *registry.Device, req SpawnAgentRequest) (SpawnAgentResult, error) {
	harness, ok := session.NormalizeHarness(req.Harness)
	if !ok {
		return SpawnAgentResult{}, newFleetErr(fmt.Sprintf("unknown harness %q (expected claude, agy, or codex)", req.Harness), http.StatusBadRequest)
	}
	sessName := strings.TrimSpace(req.Name)
	if sessName != "" {
		if err := session.ValidateName(sessName); err != nil {
			return SpawnAgentResult{}, newFleetErr(err.Error(), http.StatusBadRequest)
		}
	} else {
		sessName = "ui-" + harness + "-" + randomHex(4)
	}
	strat, err := slacker.NormalizeStrategy(req.Strategy)
	if err != nil {
		return SpawnAgentResult{}, err
	}
	pol, err := slacker.NormalizePolicy(req.Policy)
	if err != nil {
		return SpawnAgentResult{}, err
	}
	cfg, err := slacker.LoadConfig(f.SlackerConfigPath)
	if err != nil {
		return SpawnAgentResult{}, err
	}
	devs, err := f.resolveSpawnDevices(caller, req, cfg)
	if err != nil {
		return SpawnAgentResult{}, err
	}
	singleDev := ""
	if len(devs) == 1 {
		singleDev = devs[0]
	}
	ch := slacker.ChannelMapping{
		ChannelID: strings.Join(devs, ","),
		Device:    singleDev,
		Devices:   devs,
		Strategy:  strat,
		Policy:    pol,
		RepoPath:  strings.TrimSpace(req.RepoPath),
		UseTmux:   req.UseTmux,
	}
	loadFn := func(context.Context) (map[string]int, error) {
		counts := map[string]int{}
		if f.Agents != nil {
			if sessions, lErr := f.Agents.ListSessions("", "", "", "", false); lErr == nil {
				for _, s := range sessions {
					if s.Device != "" {
						counts[s.Device]++
					}
				}
			}
		}
		return counts, nil
	}

	f.spawnMu.Lock()
	defer f.spawnMu.Unlock()

	now := time.Now()
	targetDevice, resolvedRepo, scope, err := slacker.SelectTarget(ctx, cfg, "ui", ch, f.Slacker, loadFn, now)
	if err != nil {
		return SpawnAgentResult{}, err
	}
	onlineWorkers, _ := f.Slacker.OnlineWorkers(now, slacker.WorkerOnlineWindow)
	workerOnline := onlineWorkers[targetDevice]

	callerName := "admin"
	if caller != nil && caller.Name != "" {
		callerName = caller.Name
	}
	inserted, job, err := f.Slacker.EnqueueJob(slacker.Job{
		Account:      "ui",
		EventID:      "ui-" + randomHex(8),
		ChannelName:  "web-ui",
		Reaction:     "spawn",
		UserID:       callerName,
		TargetDevice: targetDevice,
		RepoPath:     resolvedRepo,
		UseTmux:      req.UseTmux,
		Policy:       cfg.EffectiveChannelPolicy(ch),
		Model:        strings.TrimSpace(req.Model),
		Harness:      harness,
		SessionName:  sessName,
		Prompt:       strings.TrimSpace(req.Prompt),
	})
	if err != nil {
		return SpawnAgentResult{}, err
	}
	if inserted {
		_ = f.Slacker.SetLastDevice(scope, targetDevice)
		if f.Agents != nil && resolvedRepo != "" {
			_ = f.Agents.RecordPath(targetDevice, resolvedRepo, agents.RepoNameFromPath(resolvedRepo), "spawn", true)
		}
	}
	return SpawnAgentResult{Job: job, WorkerOnline: workerOnline}, nil
}

// KillAgent queues a Slacker kill job for target sessionID and deregisters it immediately.
func (f *Fleet) KillAgent(caller *registry.Device, sessionID string) (KillAgentResult, error) {
	if strings.TrimSpace(sessionID) == "" {
		return KillAgentResult{}, newFleetErr("session_id is required", http.StatusBadRequest)
	}
	detail, err := f.Agents.GetSession(sessionID)
	if err != nil {
		return KillAgentResult{}, err
	}
	if !IsAdmin(caller) && caller != nil && detail.Device != caller.Name {
		return KillAgentResult{}, newFleetErr(fmt.Sprintf("%s cannot kill session on %s", caller.Name, detail.Device), http.StatusForbidden)
	}
	sessHarness, ok := session.NormalizeHarness(detail.Harness)
	if !ok {
		sessHarness = detail.Harness
	}
	targetPayload, _ := json.Marshal(session.KillTarget{
		SessionID: detail.SessionID,
		Harness:   sessHarness,
		Name:      detail.Name,
		FullRawID: detail.FullRawID,
		PID:       detail.PID,
	})
	onlineWorkers, _ := f.Slacker.OnlineWorkers(time.Now(), slacker.WorkerOnlineWindow)
	callerName := "admin"
	if caller != nil && caller.Name != "" {
		callerName = caller.Name
	}
	_, job, err := f.Slacker.EnqueueJob(slacker.Job{
		Account:      "ui",
		EventID:      "ui-kill-" + randomHex(8),
		ChannelName:  "web-ui",
		Reaction:     "kill",
		UserID:       callerName,
		TargetDevice: detail.Device,
		Harness:      sessHarness,
		SessionName:  cmp.Or(detail.Name, detail.FullRawID, detail.SessionID),
		Prompt:       string(targetPayload),
	})
	if err != nil {
		return KillAgentResult{}, err
	}
	_ = f.Agents.DeregisterAny(detail.SessionID)
	return KillAgentResult{
		OK:           true,
		SessionID:    detail.SessionID,
		Device:       detail.Device,
		Job:          job,
		WorkerOnline: onlineWorkers[detail.Device],
	}, nil
}
