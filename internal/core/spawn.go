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

// InternalAdminCaller returns the trusted in-process admin device identity for internal callers such as the assistant.
func InternalAdminCaller() *registry.Device {
	return &registry.Device{Name: "admin", Grp: "admin"}
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func dedupDevices(req SpawnAgentRequest) []string {
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
	return devs
}

func (f *Fleet) resolveSpawnDevices(caller *registry.Device, req SpawnAgentRequest, cfg slacker.Config) ([]string, error) {
	if caller == nil {
		return nil, newFleetErr("caller authentication required", http.StatusUnauthorized)
	}
	devs := dedupDevices(req)
	if len(devs) > 0 {
		activeDevs, err := f.ActiveDeviceNames()
		if err != nil {
			return nil, err
		}
		for _, d := range devs {
			if !activeDevs[d] {
				return nil, newFleetErr(fmt.Sprintf("unknown or revoked device %q", d), http.StatusBadRequest)
			}
			if !IsAdmin(caller) && d != caller.Name {
				return nil, newFleetErr(fmt.Sprintf("%s cannot spawn agents on %s", caller.Name, d), http.StatusForbidden)
			}
		}
		return devs, nil
	}
	if (!IsAdmin(caller) || len(cfg.DefaultDevices) == 0) && caller.Name != "" {
		return []string{caller.Name}, nil
	}
	return nil, nil
}

func (f *Fleet) buildSpawnChannel(devs []string, strat, pol, repoPath string, useTmux bool, cfg slacker.Config) slacker.ChannelMapping {
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
		RepoPath:  strings.TrimSpace(repoPath),
		UseTmux:   useTmux,
	}
	if len(devs) == 1 {
		if lh, lErr := f.Registry.Lighthouse(); lErr == nil && lh != nil && devs[0] == lh.Name && ch.RepoPath == "" && strings.TrimSpace(cfg.Devices[lh.Name].WorkspaceDir) == "" {
			ch.RepoPath = "~"
		}
	}
	return ch
}

func (f *Fleet) sessionDeviceCounts(context.Context) (map[string]int, error) {
	counts := map[string]int{}
	if f.Agents == nil {
		return counts, nil
	}
	if sessions, err := f.Agents.ListSessions("", "", "", "", false); err == nil {
		for _, s := range sessions {
			if s.Device != "" {
				counts[s.Device]++
			}
		}
	}
	return counts, nil
}

func resolveSpawnSessionName(harness, rawName string) (string, error) {
	sessName := strings.TrimSpace(rawName)
	if sessName == "" {
		return "ui-" + harness + "-" + randomHex(4), nil
	}
	if err := session.ValidateName(sessName); err != nil {
		return "", newFleetErr(err.Error(), http.StatusBadRequest)
	}
	return sessName, nil
}

func (f *Fleet) defaultSpawnHarness(req SpawnAgentRequest) string {
	if strings.TrimSpace(req.Harness) != "" || f.Registry == nil {
		return req.Harness
	}
	lh, lErr := f.Registry.Lighthouse()
	if lErr != nil || lh == nil || lh.Name == "" {
		return req.Harness
	}
	dev := strings.TrimSpace(req.Device)
	if strings.EqualFold(dev, lh.Name) || (len(req.Devices) == 1 && strings.EqualFold(strings.TrimSpace(req.Devices[0]), lh.Name)) {
		return session.HarnessAgy
	}
	return req.Harness
}

// SpawnAgent queues a Slacker spawn job on the selected Mac device and records tracked paths.
func (f *Fleet) SpawnAgent(ctx context.Context, caller *registry.Device, req SpawnAgentRequest) (SpawnAgentResult, error) {
	if caller == nil {
		return SpawnAgentResult{}, newFleetErr("caller authentication required", http.StatusUnauthorized)
	}
	req.Harness = f.defaultSpawnHarness(req)
	harness, ok := session.NormalizeHarness(req.Harness)
	if !ok {
		return SpawnAgentResult{}, newFleetErr(fmt.Sprintf("unknown harness %q (expected claude, agy, or codex)", req.Harness), http.StatusBadRequest)
	}
	sessName, err := resolveSpawnSessionName(harness, req.Name)
	if err != nil {
		return SpawnAgentResult{}, err
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
	ch := f.buildSpawnChannel(devs, strat, pol, req.RepoPath, req.UseTmux, cfg)

	f.spawnMu.Lock()
	defer f.spawnMu.Unlock()

	now := time.Now()
	targetDevice, resolvedRepo, scope, err := slacker.SelectTarget(ctx, cfg, "ui", ch, f.Slacker, f.sessionDeviceCounts, now)
	if err != nil {
		return SpawnAgentResult{}, err
	}
	onlineWorkers, _ := f.Slacker.OnlineWorkers(now, slacker.WorkerOnlineWindow)
	workerOnline := onlineWorkers[targetDevice]

	callerName := cmp.Or(caller.Name, "admin")
	inserted, job, err := f.Slacker.EnqueueJobContext(ctx, slacker.Job{
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
		if f.Agents != nil && resolvedRepo != "" && resolvedRepo != "~" {
			_ = f.Agents.RecordPath(targetDevice, resolvedRepo, agents.RepoNameFromPath(resolvedRepo), "spawn", true)
		}
	}
	return SpawnAgentResult{Job: job, WorkerOnline: workerOnline}, nil
}

// KillAgent queues a Slacker kill job for target sessionID and deregisters it immediately.
func (f *Fleet) KillAgent(caller *registry.Device, sessionID string) (KillAgentResult, error) {
	return f.KillAgentContext(context.Background(), caller, sessionID)
}

// KillAgentContext queues a Slacker kill job for target sessionID using ctx and deregisters it immediately.
func (f *Fleet) KillAgentContext(ctx context.Context, caller *registry.Device, sessionID string) (KillAgentResult, error) {
	if caller == nil {
		return KillAgentResult{}, newFleetErr("caller authentication required", http.StatusUnauthorized)
	}
	if strings.TrimSpace(sessionID) == "" {
		return KillAgentResult{}, newFleetErr("session_id is required", http.StatusBadRequest)
	}
	detail, err := f.Agents.GetSession(sessionID)
	if err != nil {
		return KillAgentResult{}, err
	}
	if !IsAdmin(caller) && detail.Device != caller.Name {
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
	callerName := cmp.Or(caller.Name, "admin")
	_, job, err := f.Slacker.EnqueueJobContext(ctx, slacker.Job{
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
