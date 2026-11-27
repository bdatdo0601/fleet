package core

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/registry"
)

const (
	// NebulaRestartReqFile is watched by launchd WatchPaths on the lighthouse to trigger a root Nebula restart.
	NebulaRestartReqFile = "nebula-restart.req"
	// NebulaRestartStatusFile records the most recent completed Nebula restart on the lighthouse.
	NebulaRestartStatusFile = "nebula-restart.status"
)

// NebulaRestartRequest describes a pending request to restart the lighthouse Nebula daemon.
type NebulaRestartRequest struct {
	ID          string `json:"id"`
	RequestedAt string `json:"requested_at"`
	Reason      string `json:"reason"`
	By          string `json:"by"`
}

// NebulaRestartStatus records the outcome of the last handled Nebula restart request.
type NebulaRestartStatus struct {
	HandledID   string `json:"handled_id"`
	RestartedAt string `json:"restarted_at"`
	Reason      string `json:"reason"`
	By          string `json:"by"`
	OK          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
}

// NebulaWatchdogState summarizes the runtime state of the lighthouse relay watchdog.
type NebulaWatchdogState struct {
	Enabled        bool   `json:"enabled"`
	LogPath        string `json:"log_path"`
	BurstThreshold int    `json:"burst_threshold"`
	BurstWindowSec int    `json:"burst_window_sec"`
	CooldownSec    int    `json:"cooldown_sec"`
	RecentErrors   int    `json:"recent_errors"`
	LastTriggerAt  string `json:"last_trigger_at,omitempty"`
	LastError      string `json:"last_error,omitempty"`
}

// NebulaHealthStatus combines queued restart state, last restart result, and watchdog state.
type NebulaHealthStatus struct {
	Pending        bool                  `json:"pending"`
	PendingRequest *NebulaRestartRequest `json:"pending_request,omitempty"`
	LastRestart    *NebulaRestartStatus  `json:"last_restart,omitempty"`
	Watchdog       *NebulaWatchdogState  `json:"watchdog,omitempty"`
}

func readJSONFile[T any](path string) (*T, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RequestNebulaRestart writes the restart request file in place so launchd WatchPaths sees NOTE_WRITE.
func RequestNebulaRestart(stateDir, by, reason string) (NebulaRestartRequest, error) {
	if stateDir == "" {
		return NebulaRestartRequest{}, newFleetErr("state directory is required", 400)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return NebulaRestartRequest{}, err
	}
	req := NebulaRestartRequest{
		ID:          strconv.FormatInt(time.Now().UTC().UnixNano(), 10),
		RequestedAt: registry.NowISO(),
		Reason:      strings.TrimSpace(reason),
		By:          strings.TrimSpace(by),
	}
	if req.Reason == "" {
		req.Reason = "manual"
	}
	data, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return NebulaRestartRequest{}, err
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(stateDir, NebulaRestartReqFile), data, 0o644); err != nil {
		return NebulaRestartRequest{}, err
	}
	return req, nil
}

// PendingNebulaRestart returns the unhandled restart request, or nil if none is pending.
func PendingNebulaRestart(stateDir string) (*NebulaRestartRequest, error) {
	if stateDir == "" {
		return nil, nil
	}
	req, err := readJSONFile[NebulaRestartRequest](filepath.Join(stateDir, NebulaRestartReqFile))
	if err != nil || req == nil || req.ID == "" {
		return nil, err
	}
	if st, _ := readJSONFile[NebulaRestartStatus](filepath.Join(stateDir, NebulaRestartStatusFile)); st != nil && st.HandledID == req.ID {
		return nil, nil
	}
	return req, nil
}

// RecordNebulaRestartStatus persists the result of handling a Nebula restart request.
func RecordNebulaRestartStatus(stateDir string, st NebulaRestartStatus, uid, gid *int) error {
	if stateDir == "" {
		return nil
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = paths.WriteFileAtomic(filepath.Join(stateDir, NebulaRestartStatusFile), data, 0o644, uid, gid)
	return err
}

// ReadNebulaHealth reads the current pending restart request and last completed restart status.
func ReadNebulaHealth(stateDir string) (NebulaHealthStatus, error) {
	var out NebulaHealthStatus
	if stateDir == "" {
		return out, nil
	}
	req, err := readJSONFile[NebulaRestartRequest](filepath.Join(stateDir, NebulaRestartReqFile))
	if err != nil {
		return out, err
	}
	st, err := readJSONFile[NebulaRestartStatus](filepath.Join(stateDir, NebulaRestartStatusFile))
	if err != nil {
		return out, err
	}
	out.LastRestart = st
	if req != nil && req.ID != "" && (st == nil || st.HandledID != req.ID) {
		out.Pending = true
		out.PendingRequest = req
	}
	return out, nil
}
