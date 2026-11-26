package slacker

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
)

// WorkerOnlineWindow is the heartbeat recency threshold used to prefer online Slacker workers.
const WorkerOnlineWindow = 90 * time.Second

// AgentLoadFunc returns the count of live agent sessions per Fleet device.
type AgentLoadFunc func(ctx context.Context) (map[string]int, error)

// DefaultAgentLoad queries the local fleet API /v1/agents if apiBase is reachable,
// falling back to reading agents.db directly under stateDir, or empty counts if unavailable.
func DefaultAgentLoad(stateDir, apiBase string) AgentLoadFunc {
	hc := &http.Client{Timeout: 2 * time.Second}
	return func(ctx context.Context) (map[string]int, error) {
		if apiBase != "" {
			if counts, ok := fetchAgentCountsHTTP(ctx, hc, apiBase); ok {
				return counts, nil
			}
		}
		if stateDir != "" {
			dbPath := filepath.Join(stateDir, "agents.db")
			if _, err := os.Stat(dbPath); err == nil {
				if hub, err := agents.Open(dbPath); err == nil {
					defer func() { _ = hub.Close() }()
					if sessions, err := hub.ListSessions("", "", "", "", false); err == nil {
						counts := map[string]int{}
						for _, s := range sessions {
							if s.Device != "" {
								counts[s.Device]++
							}
						}
						return counts, nil
					}
				}
			}
		}
		return map[string]int{}, nil
	}
}

func fetchAgentCountsHTTP(ctx context.Context, hc *http.Client, apiBase string) (map[string]int, bool) {
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, apiBase+"/v1/agents", nil)
	if err != nil {
		return nil, false
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, false
	}
	var payload struct {
		Agents []struct {
			Device string `json:"device"`
			Live   bool   `json:"live"`
		} `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, false
	}
	counts := map[string]int{}
	for _, a := range payload.Agents {
		if a.Live && a.Device != "" {
			counts[a.Device]++
		}
	}
	return counts, true
}

// DispatchScope returns the persistence key used for round-robin / tie-break cursors.
func DispatchScope(acctID string, ch ChannelMapping) string {
	if len(ch.Devices) > 0 || ch.Device != "" {
		return "channel:" + ChannelKey(cmp.Or(acctID, DefaultAccountID), ch.ChannelID)
	}
	return "default"
}

func pickNextCyclic(allDevices, candidates []string, lastDevice string) string {
	if len(candidates) == 0 {
		return ""
	}
	candSet := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		candSet[c] = true
	}
	idx := -1
	for i, d := range allDevices {
		if d == lastDevice {
			idx = i
			break
		}
	}
	for offset := 1; offset <= len(allDevices); offset++ {
		d := allDevices[(idx+offset)%len(allDevices)]
		if candSet[d] {
			return d
		}
	}
	return candidates[0]
}

// SelectTarget chooses the target device and resolved repo_path for ch using online worker preference,
// least_agents (with round-robin tie-breaking) or round_robin scheduling, and per-device workspace_dir.
func SelectTarget(
	ctx context.Context,
	cfg Config,
	acctID string,
	ch ChannelMapping,
	store *Store,
	loadFn AgentLoadFunc,
	now time.Time,
) (device, repoPath, scope string, err error) {
	allDevices := cfg.EffectiveChannelDevices(ch)
	if len(allDevices) == 0 {
		return "", "", "", newSlackerErr("channel "+ch.ChannelID+" has no target devices or default_devices configured", 400)
	}

	candidates := allDevices
	if store != nil {
		if online, oErr := store.OnlineWorkers(now, WorkerOnlineWindow); oErr == nil && len(online) > 0 {
			var filtered []string
			for _, d := range allDevices {
				if online[d] {
					filtered = append(filtered, d)
				}
			}
			if len(filtered) > 0 {
				candidates = filtered
			}
		}
	}

	scope = DispatchScope(acctID, ch)
	var lastDevice string
	if store != nil {
		lastDevice, _ = store.GetLastDevice(scope)
	}

	var picked string
	strategy := cfg.EffectiveChannelStrategy(ch)
	switch {
	case len(candidates) == 1:
		picked = candidates[0]
	case strategy == StrategyRoundRobin:
		picked = pickNextCyclic(allDevices, candidates, lastDevice)
	default:
		var liveCounts map[string]int
		if loadFn != nil {
			liveCounts, _ = loadFn(ctx)
		}
		var pendingCounts map[string]int
		if store != nil {
			pendingCounts, _ = store.PendingJobCounts()
		}
		minLoad := -1
		var tied []string
		for _, d := range candidates {
			load := liveCounts[d] + pendingCounts[d]
			if minLoad == -1 || load < minLoad {
				minLoad = load
				tied = []string{d}
			} else if load == minLoad {
				tied = append(tied, d)
			}
		}
		picked = pickNextCyclic(allDevices, tied, lastDevice)
	}

	resolvedRepo, err := cfg.ResolveRepoPath(picked, ch.RepoPath)
	if err != nil {
		return "", "", "", err
	}
	return picked, resolvedRepo, scope, nil
}
