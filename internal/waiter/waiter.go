// Package waiter implements real-time inbox polling for CLI watchers and Codex MCP queue delivery.
package waiter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/hooks"
	"github.com/bdatdo0601/fleet/internal/paths"
)

var safeSIDRE = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

// WaitCLIOpts configures WaitForInboxCLI.
type WaitCLIOpts struct {
	SessionID   string
	APIURL      string
	Harness     string
	MaxWait     float64
	PollTimeout float64
	Stdout      io.Writer
	LockDir     string
	WatchParent bool
}

// FindCodexBinary locates the codex executable on PATH or standard install dirs.
func FindCodexBinary() string {
	if p, err := exec.LookPath("codex"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, cand := range []string{
		filepath.Join(home, ".local", "bin", "codex"),
		"/opt/homebrew/bin/codex",
		"/usr/local/bin/codex",
	} {
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return cand
		}
	}
	return ""
}

// FormatWaitOutput formats unread peer messages with an optional re-arm hint.
func FormatWaitOutput(sessionID, sessionName string, messages []agents.Message, rearmCmd string) string {
	hint := ""
	if rearmCmd != "" {
		hint = fmt.Sprintf("Re-arm listener after handling: run `%s` in the background.", rearmCmd)
	}
	return hooks.FormatUnread(sessionID, messages, sessionName, hint)
}

func pidLockPath(sessionID, lockDir string) string {
	if lockDir == "" {
		lockDir = os.TempDir()
	}
	safeSID := safeSIDRE.ReplaceAllString(sessionID, "_")
	return filepath.Join(lockDir, fmt.Sprintf("fleet-wait-%d-%s.pid", os.Getuid(), safeSID))
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func acquirePIDLock(sessionID, lockDir string) (string, error) {
	path := pidLockPath(sessionID, lockDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if data, err := os.ReadFile(path); err == nil {
		if oldPID, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && oldPID > 0 && oldPID != os.Getpid() {
			_ = syscall.Kill(oldPID, syscall.SIGTERM)
			deadline := time.Now().Add(300 * time.Millisecond)
			for time.Now().Before(deadline) && pidAlive(oldPID) {
				time.Sleep(50 * time.Millisecond)
			}
		}
	}
	tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.%d.tmp", filepath.Base(path), os.Getpid()))
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		return "", err
	}
	_ = os.Chmod(tmp, 0o600)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return path, nil
}

func releasePIDLock(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if strings.TrimSpace(string(data)) == strconv.Itoa(os.Getpid()) {
		_ = os.Remove(path)
	}
}

func parentGone(initialPPID int) bool {
	if initialPPID <= 1 {
		return false
	}
	if os.Getppid() != initialPPID {
		return true
	}
	return !pidAlive(initialPPID)
}

func inferHarness(sessionID, override string) string {
	if paths.ValidHarness(override) {
		return override
	}
	parts := strings.SplitN(sessionID, ":", 3)
	if len(parts) >= 2 && paths.ValidHarness(parts[1]) {
		return parts[1]
	}
	return "antigravity"
}

func resolveNodeAPI(apiURL string) (string, error) {
	if apiURL != "" {
		return apiURL, nil
	}
	data, err := os.ReadFile(paths.NodeFile())
	if err != nil {
		return "", &client.ApiError{Msg: "this machine hasn't joined a fleet (fleet join NAME)"}
	}
	var out struct {
		API string `json:"api"`
	}
	if err := unmarshalJSON(data, &out); err != nil || out.API == "" {
		return "", &client.ApiError{Msg: "invalid fleet.json"}
	}
	return out.API, nil
}

// WaitForInboxCLI blocks on /v1/agents/wait until unread messages arrive, then prints and exits 0.
func WaitForInboxCLI(opts WaitCLIOpts) (int, error) {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	api, err := resolveNodeAPI(opts.APIURL)
	if err != nil {
		return 1, err
	}
	pollTimeout := opts.PollTimeout
	if pollTimeout <= 0 {
		pollTimeout = 50.0
	}
	resolvedHarness := inferHarness(opts.SessionID, opts.Harness)
	initialPPID := os.Getppid()

	var deadline time.Time
	hasDeadline := opts.MaxWait > 0
	if hasDeadline {
		deadline = time.Now().Add(time.Duration(opts.MaxWait * float64(time.Second)))
	}

	lockPath, err := acquirePIDLock(opts.SessionID, opts.LockDir)
	if err != nil {
		return 1, err
	}
	defer releasePIDLock(lockPath)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	currentSID := opts.SessionID
	backoff := 2.0
	for {
		if ctx.Err() != nil {
			return 0, nil
		}
		if opts.WatchParent && parentGone(initialPPID) {
			return 0, nil
		}
		reqTimeout := pollTimeout
		if reqTimeout < 0.05 {
			reqTimeout = 0.05
		} else if reqTimeout > 60.0 {
			reqTimeout = 60.0
		}
		if hasDeadline {
			rem := time.Until(deadline).Seconds()
			if rem <= 0 {
				_, _ = io.WriteString(stdout, "[Fleet listener idle timeout; do not auto-restart unless still expecting peer messages]\n")
				return 0, nil
			}
			if rem < reqTimeout {
				reqTimeout = rem
			}
			if reqTimeout < 0.05 {
				reqTimeout = 0.05
			}
		}

		var resp agents.WaitResult
		err := client.Call(
			api,
			"POST",
			"/v1/agents/wait",
			map[string]any{
				"session_id": currentSID,
				"harness":    resolvedHarness,
				"timeout":    reqTimeout,
				"mark_read":  true,
			},
			time.Duration((reqTimeout+15.0)*float64(time.Second)),
			nil,
			&resp,
		)
		if err != nil {
			if ctx.Err() != nil || (opts.WatchParent && parentGone(initialPPID)) {
				return 0, nil
			}
			sleepFor := backoff
			if backoff < 5.0 {
				backoff = 5.0
			} else {
				backoff = 10.0
			}
			if hasDeadline {
				rem := time.Until(deadline).Seconds()
				if rem <= 0 {
					_, _ = io.WriteString(stdout, "[Fleet listener idle timeout; do not auto-restart unless still expecting peer messages]\n")
					return 0, nil
				}
				if rem < sleepFor {
					sleepFor = rem
				}
			}
			t := time.NewTimer(time.Duration(sleepFor * float64(time.Second)))
			select {
			case <-ctx.Done():
				t.Stop()
				return 0, nil
			case <-t.C:
			}
			continue
		}
		backoff = 2.0
		if resp.SessionID != "" {
			currentSID = resp.SessionID
		}
		if resp.Exited {
			return 0, nil
		}
		if len(resp.Messages) > 0 {
			text := FormatWaitOutput(
				currentSID,
				resp.Name,
				resp.Messages,
				fmt.Sprintf("fleet agents wait --session %s", currentSID),
			)
			_, _ = io.WriteString(stdout, text+"\n")
			return 0, nil
		}
	}
}

// RunQueueFn executes `codex queue ...` and returns (returncode, detail).
type RunQueueFn func(cmd []string) (int, string)

// ListenerStatus summarizes CodexQueueListener state for `fleet_listen`.
type ListenerStatus struct {
	Running        bool    `json:"running"`
	Enabled        bool    `json:"enabled"`
	SessionID      string  `json:"session_id"`
	FullRawID      string  `json:"full_raw_id"`
	DeliveredCount int     `json:"delivered_count"`
	LastError      *string `json:"last_error"`
}

// CodexQueueListener runs inside `fleet mcp serve --harness codex` and pushes inbox messages via `codex queue`.
type CodexQueueListener struct {
	apiURL      string
	ppid        int
	cwd         string
	codexBin    string
	pollTimeout float64
	runQueueFn  RunQueueFn

	mu             sync.Mutex
	enabled        bool
	stopping       bool
	running        bool
	stopCh         chan struct{}
	wakeCh         chan struct{}
	doneCh         chan struct{}
	sessionID      string
	fullRawID      string
	deliveredCount int
	lastError      *string
}

// CodexListenerOpts configures NewCodexQueueListener.
type CodexListenerOpts struct {
	APIURL      string
	PPID        int
	CWD         string
	CodexBin    string
	PollTimeout float64
	RunQueueFn  RunQueueFn
}

// NewCodexQueueListener creates a listener for Codex MCP sessions.
func NewCodexQueueListener(opts CodexListenerOpts) *CodexQueueListener {
	pt := opts.PollTimeout
	if pt <= 0 {
		pt = 50.0
	}
	if pt < 0.05 {
		pt = 0.05
	} else if pt > 60.0 {
		pt = 60.0
	}
	bin := opts.CodexBin
	if bin == "" {
		if opts.RunQueueFn != nil {
			bin = "codex"
		} else {
			bin = FindCodexBinary()
		}
	}
	return &CodexQueueListener{
		apiURL:      opts.APIURL,
		ppid:        opts.PPID,
		cwd:         opts.CWD,
		codexBin:    bin,
		pollTimeout: pt,
		runQueueFn:  opts.RunQueueFn,
		wakeCh:      make(chan struct{}, 1),
	}
}

func (l *CodexQueueListener) Start() ListenerStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.codexBin == "" {
		msg := "codex binary not found"
		l.lastError = &msg
		l.enabled = false
		return l.statusLocked()
	}
	l.lastError = nil
	l.enabled = true
	if !l.running {
		l.stopping = false
		l.stopCh = make(chan struct{})
		l.doneCh = make(chan struct{})
		l.running = true
		go l.loop(l.stopCh, l.doneCh)
	}
	select {
	case l.wakeCh <- struct{}{}:
	default:
	}
	return l.statusLocked()
}

func (l *CodexQueueListener) Pause() ListenerStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.enabled = false
	return l.statusLocked()
}

func (l *CodexQueueListener) Stop() ListenerStatus {
	l.mu.Lock()
	l.enabled = false
	l.stopping = true
	stopCh := l.stopCh
	doneCh := l.doneCh
	running := l.running
	if stopCh != nil {
		select {
		case <-stopCh:
		default:
			close(stopCh)
		}
	}
	l.mu.Unlock()

	if running && doneCh != nil {
		select {
		case <-doneCh:
		case <-time.After(1 * time.Second):
		}
	}
	return l.Status()
}

func (l *CodexQueueListener) Status() ListenerStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.statusLocked()
}

func (l *CodexQueueListener) statusLocked() ListenerStatus {
	return ListenerStatus{
		Running:        l.running && !l.stopping,
		Enabled:        l.enabled,
		SessionID:      l.sessionID,
		FullRawID:      l.fullRawID,
		DeliveredCount: l.deliveredCount,
		LastError:      l.lastError,
	}
}

func (l *CodexQueueListener) invokeQueue(cmd []string) (int, string) {
	if l.runQueueFn != nil {
		return l.runQueueFn(cmd)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail == "" {
			detail = err.Error()
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), detail
		}
		return 1, detail
	}
	return 0, strings.TrimSpace(stderr.String())
}

func (l *CodexQueueListener) loop(stopCh, doneCh chan struct{}) {
	defer func() {
		l.mu.Lock()
		l.running = false
		l.mu.Unlock()
		close(doneCh)
	}()

	backoff := 2.0
	for {
		select {
		case <-stopCh:
			return
		default:
		}
		l.mu.Lock()
		enabled := l.enabled
		bin := l.codexBin
		knownSID := l.sessionID
		l.mu.Unlock()

		if !enabled {
			t := time.NewTimer(250 * time.Millisecond)
			select {
			case <-stopCh:
				t.Stop()
				return
			case <-l.wakeCh:
				t.Stop()
			case <-t.C:
			}
			continue
		}
		if bin == "" {
			l.mu.Lock()
			msg := "codex binary not found"
			l.lastError = &msg
			l.enabled = false
			l.mu.Unlock()
			return
		}

		api, err := resolveNodeAPI(l.apiURL)
		if err != nil {
			l.mu.Lock()
			msg := err.Error()
			l.lastError = &msg
			l.mu.Unlock()
			t := time.NewTimer(time.Duration(backoff * float64(time.Second)))
			if backoff < 5.0 {
				backoff = 5.0
			} else {
				backoff = 10.0
			}
			select {
			case <-stopCh:
				t.Stop()
				return
			case <-t.C:
			}
			continue
		}

		payload := map[string]any{
			"harness":         "codex",
			"pid":             l.ppid,
			"cwd":             l.cwd,
			"timeout":         l.pollTimeout,
			"mark_read":       false,
			"require_full_id": true,
		}
		if knownSID != "" {
			payload["session_id"] = knownSID
		}
		var resp agents.WaitResult
		if err := client.Call(api, "POST", "/v1/agents/wait", payload, time.Duration((l.pollTimeout+15.0)*float64(time.Second)), nil, &resp); err != nil {
			l.mu.Lock()
			msg := err.Error()
			l.lastError = &msg
			l.mu.Unlock()
			t := time.NewTimer(time.Duration(backoff * float64(time.Second)))
			if backoff < 5.0 {
				backoff = 5.0
			} else {
				backoff = 10.0
			}
			select {
			case <-stopCh:
				t.Stop()
				return
			case <-t.C:
			}
			continue
		}
		backoff = 2.0

		select {
		case <-stopCh:
			return
		default:
		}
		if resp.Exited {
			l.mu.Lock()
			l.enabled = false
			l.mu.Unlock()
			return
		}

		l.mu.Lock()
		if resp.SessionID != "" {
			l.sessionID = resp.SessionID
		}
		if resp.FullRawID != "" {
			l.fullRawID = resp.FullRawID
		}
		stillEnabled := l.enabled
		l.mu.Unlock()

		if !stillEnabled || len(resp.Messages) == 0 || resp.FullRawID == "" {
			continue
		}

		text := FormatWaitOutput(resp.SessionID, resp.Name, resp.Messages, "")
		cmd := []string{bin, "queue", "--thread", resp.FullRawID, "--message", text, "--resume"}
		rc, errDetail := l.invokeQueue(cmd)
		if rc == 0 {
			ids := make([]int64, len(resp.Messages))
			for i, m := range resp.Messages {
				ids[i] = m.ID
			}
			_ = client.Call(api, "POST", "/v1/agents/ack", map[string]any{
				"harness":     "codex",
				"session_id":  resp.SessionID,
				"message_ids": ids,
			}, 5*time.Second, nil, nil)
			l.mu.Lock()
			l.deliveredCount += len(resp.Messages)
			l.lastError = nil
			l.mu.Unlock()
		} else {
			msg := errDetail
			if msg == "" {
				msg = fmt.Sprintf("codex queue exited with status %d", rc)
			}
			l.mu.Lock()
			l.lastError = &msg
			l.enabled = false
			l.mu.Unlock()
		}
	}
}
