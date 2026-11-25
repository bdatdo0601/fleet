package hooks

import (
	"cmp"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/bdatdo0601/fleet/internal/paths"
)

var socketKeySafeRE = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

// ClaudeSocketInfo stores the local Unix socket endpoint and auth token for a live Claude session.
type ClaudeSocketInfo struct {
	Socket    string `json:"socket"`
	Token     string `json:"token,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	PID       int    `json:"pid,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// ClaudeSocketsDir returns the private local directory holding per-session Claude socket handoff files.
func ClaudeSocketsDir(cacheDir string) string {
	return filepath.Join(cmp.Or(cacheDir, paths.CacheDir()), "claude-sockets")
}

// ClaudeProcessPID returns the owning Claude process PID from CLAUDE_PID, falling back to os.Getppid().
func ClaudeProcessPID() int {
	if raw := strings.TrimSpace(os.Getenv("CLAUDE_PID")); raw != "" {
		if pid, err := strconv.Atoi(raw); err == nil && pid > 0 {
			return pid
		}
	}
	return os.Getppid()
}

func claudePIDSocketPath(dir string, pid int) string {
	return filepath.Join(dir, "pid-"+strconv.Itoa(pid)+".json")
}

func claudeSIDSocketPath(dir, sessionID string) string {
	return filepath.Join(dir, "sid-"+socketKeySafeRE.ReplaceAllString(sessionID, "_")+".json")
}

func readClaudeSocketFile(path string) (ClaudeSocketInfo, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ClaudeSocketInfo{}, false
	}
	var info ClaudeSocketInfo
	if json.Unmarshal(raw, &info) != nil || strings.TrimSpace(info.Socket) == "" {
		return ClaudeSocketInfo{}, false
	}
	return info, true
}

func pruneStaleClaudeSockets(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, entry.Name())
		info, ok := readClaudeSocketFile(p)
		if !ok {
			_ = os.Remove(p)
			continue
		}
		if _, err := os.Stat(info.Socket); os.IsNotExist(err) {
			_ = os.Remove(p)
		}
	}
}

// SaveClaudeSocket persists a Claude session's local UDS socket info with 0600 permissions.
func SaveClaudeSocket(cacheDir string, info ClaudeSocketInfo) error {
	info.Socket = strings.TrimSpace(info.Socket)
	if info.Socket == "" {
		return nil
	}
	dir := ClaudeSocketsDir(cacheDir)
	if err := paths.EnsureDir(dir, 0o700, nil, nil); err != nil {
		return err
	}
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if info.PID > 0 {
		if _, err := paths.WriteFileAtomic(claudePIDSocketPath(dir, info.PID), data, 0o600, nil, nil); err != nil {
			return err
		}
	}
	if info.SessionID != "" {
		if _, err := paths.WriteFileAtomic(claudeSIDSocketPath(dir, info.SessionID), data, 0o600, nil, nil); err != nil {
			return err
		}
	}
	pruneStaleClaudeSockets(dir)
	return nil
}

// LoadClaudeSocket loads saved Claude UDS socket info by PID or session ID.
func LoadClaudeSocket(cacheDir string, pid int, sessionID string) (ClaudeSocketInfo, bool) {
	dir := ClaudeSocketsDir(cacheDir)
	if pid > 0 {
		if info, ok := readClaudeSocketFile(claudePIDSocketPath(dir, pid)); ok {
			return info, true
		}
	}
	if sessionID != "" {
		if info, ok := readClaudeSocketFile(claudeSIDSocketPath(dir, sessionID)); ok {
			return info, true
		}
	}
	return ClaudeSocketInfo{}, false
}

// RemoveClaudeSocket deletes saved Claude UDS socket handoff files for pid and/or sessionID.
func RemoveClaudeSocket(cacheDir string, pid int, sessionID string) {
	dir := ClaudeSocketsDir(cacheDir)
	if pid > 0 {
		_ = os.Remove(claudePIDSocketPath(dir, pid))
	}
	if sessionID != "" {
		_ = os.Remove(claudeSIDSocketPath(dir, sessionID))
	}
}
