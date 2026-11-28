// Package waiter implements real-time inbox polling for CLI watchers, MCP push listeners, and harness relays.
package waiter

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
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
	return findExecutable("codex", []string{".local/bin/codex", "/opt/homebrew/bin/codex", "/usr/local/bin/codex"})
}

func findExecutable(name string, candidates []string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, cand := range candidates {
		p := cand
		if !filepath.IsAbs(p) {
			if home == "" {
				continue
			}
			p = filepath.Join(home, p)
		}
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p
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

func acquirePIDLock(sessionID, lockDir string) (*os.File, error) {
	path := pidLockPath(sessionID, lockDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another listener/relay already holds lock %s: %w", path, err)
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.Seek(0, 0)
		_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	}
	return f, nil
}

func releasePIDLock(f *os.File) {
	if f == nil {
		return
	}
	path := f.Name()
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
	_ = os.Remove(path)
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

func clampPollTimeout(pt float64) float64 {
	if pt <= 0 {
		return 50.0
	}
	return max(0.05, min(pt, 60.0))
}

func nextBackoff(cur float64) float64 {
	if cur < 5.0 {
		return 5.0
	}
	return 10.0
}

func sleepOrCancel(cancelCh <-chan struct{}, secs float64) bool {
	t := time.NewTimer(time.Duration(secs * float64(time.Second)))
	defer t.Stop()
	select {
	case <-cancelCh:
		return false
	case <-t.C:
		return true
	}
}

func calcPollTimeout(pollTimeout float64, hasDeadline bool, deadline time.Time) (float64, bool) {
	reqTimeout := max(0.05, min(pollTimeout, 60.0))
	if !hasDeadline {
		return reqTimeout, true
	}
	rem := time.Until(deadline).Seconds()
	if rem <= 0 {
		return 0, false
	}
	return max(0.05, min(reqTimeout, rem)), true
}

func runCommandCapture(ctx context.Context, cmd []string, timeout time.Duration) (int, string) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(runCtx, cmd[0], cmd[1:]...)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		detail := cmp.Or(strings.TrimSpace(stderr.String()), strings.TrimSpace(stdout.String()), err.Error())
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return exitErr.ExitCode(), detail
		}
		return 1, detail
	}
	return 0, strings.TrimSpace(stderr.String())
}

func ackMessages(ctx context.Context, api, harness, sessionID string, msgs []agents.Message) error {
	if len(msgs) == 0 || sessionID == "" {
		return nil
	}
	ids := make([]int64, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	return client.CallContext(ctx, api, "POST", "/v1/agents/ack", map[string]any{
		"harness": harness, "session_id": sessionID, "message_ids": ids,
	}, 5*time.Second, nil, nil)
}

// WaitForInboxCLI blocks on /v1/agents/wait until unread messages arrive, then prints and exits 0.
func WaitForInboxCLI(opts WaitCLIOpts) (int, error) {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	api, err := client.ResolveAPI(opts.APIURL)
	if err != nil {
		return 1, err
	}
	pollTimeout := clampPollTimeout(opts.PollTimeout)
	resolvedHarness := inferHarness(opts.SessionID, opts.Harness)
	initialPPID := os.Getppid()
	hasDeadline := opts.MaxWait > 0
	var deadline time.Time
	if hasDeadline {
		deadline = time.Now().Add(time.Duration(opts.MaxWait * float64(time.Second)))
	}

	lockFile, err := acquirePIDLock(opts.SessionID, opts.LockDir)
	if err != nil {
		return 1, err
	}
	defer releasePIDLock(lockFile)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	currentSID := opts.SessionID
	backoff := 2.0
	for {
		if ctx.Err() != nil || (opts.WatchParent && parentGone(initialPPID)) {
			return 0, nil
		}
		reqTimeout, ok := calcPollTimeout(pollTimeout, hasDeadline, deadline)
		if !ok {
			_, _ = io.WriteString(stdout, "[Fleet listener idle timeout; do not auto-restart unless still expecting peer messages]\n")
			return 0, nil
		}
		var resp agents.WaitResult
		err := client.CallContext(ctx, api, "POST", "/v1/agents/wait", map[string]any{
			"session_id": currentSID, "harness": resolvedHarness, "timeout": reqTimeout, "mark_read": true,
		}, time.Duration((reqTimeout+15.0)*float64(time.Second)), nil, &resp)
		if err != nil {
			if ctx.Err() != nil || (opts.WatchParent && parentGone(initialPPID)) {
				return 0, nil
			}
			sleepFor := backoff
			backoff = nextBackoff(backoff)
			if hasDeadline {
				rem := time.Until(deadline).Seconds()
				if rem <= 0 {
					_, _ = io.WriteString(stdout, "[Fleet listener idle timeout; do not auto-restart unless still expecting peer messages]\n")
					return 0, nil
				}
				sleepFor = min(sleepFor, rem)
			}
			if !sleepOrCancel(ctx.Done(), sleepFor) {
				return 0, nil
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
			text := FormatWaitOutput(currentSID, resp.Name, resp.Messages, "fleet agents wait --session "+currentSID)
			_, _ = io.WriteString(stdout, text+"\n")
			return 0, nil
		}
	}
}

// RunQueueFn executes a delivery CLI command and returns (returncode, detail).
type RunQueueFn func(cmd []string) (int, string)

// SessionListener controls a background inbox push listener for an MCP-hosted session.
type SessionListener interface {
	Mode() string
	Start() ListenerStatus
	Pause() ListenerStatus
	Stop() ListenerStatus
	Status() ListenerStatus
}

// ListenerStatus summarizes background push-listener state for `fleet_listen`.
type ListenerStatus struct {
	Running        bool    `json:"running"`
	Enabled        bool    `json:"enabled"`
	SessionID      string  `json:"session_id"`
	FullRawID      string  `json:"full_raw_id"`
	Socket         string  `json:"socket,omitempty"`
	DeliveredCount int     `json:"delivered_count"`
	LastError      *string `json:"last_error"`
}

type pushBackend interface {
	mode() string
	preStart() (string, bool)
	prePoll(knownSID string) bool
	canDeliver(resp agents.WaitResult) bool
	deliver(ctx context.Context, resp agents.WaitResult) (string, bool)
	onStop(sessionID string)
}

type sessionPushLoop struct {
	harness       string
	apiURL        string
	ppid          int
	cwd           string
	pollTimeout   float64
	requireFullID bool
	backend       pushBackend

	mu             sync.Mutex
	enabled        bool
	stopping       bool
	running        bool
	gen            uint64
	cancel         context.CancelFunc
	wakeCh         chan struct{}
	doneCh         chan struct{}
	sessionID      string
	fullRawID      string
	socket         string
	deliveredCount int
	lastError      *string
}

func newSessionPushLoop(harness, apiURL string, ppid int, cwd string, pollTimeout float64, requireFullID bool, backend pushBackend) *sessionPushLoop {
	return &sessionPushLoop{
		harness:       harness,
		apiURL:        apiURL,
		ppid:          ppid,
		cwd:           cwd,
		pollTimeout:   clampPollTimeout(pollTimeout),
		requireFullID: requireFullID,
		backend:       backend,
		wakeCh:        make(chan struct{}, 1),
	}
}

func (l *sessionPushLoop) Mode() string { return l.backend.mode() }

func (l *sessionPushLoop) Start() ListenerStatus {
	l.mu.Lock()
	for l.running && l.stopping && l.doneCh != nil {
		oldDone := l.doneCh
		l.mu.Unlock()
		<-oldDone
		l.mu.Lock()
	}
	defer l.mu.Unlock()
	if msg, ok := l.backend.preStart(); !ok {
		l.lastError = &msg
		l.enabled = false
		return l.statusLocked()
	}
	l.lastError = nil
	l.enabled = true
	if !l.running {
		l.stopping = false
		ctx, cancel := context.WithCancel(context.Background())
		l.cancel = cancel
		l.doneCh = make(chan struct{})
		l.running = true
		l.gen++
		go l.loop(ctx, l.doneCh, l.gen)
	}
	select {
	case l.wakeCh <- struct{}{}:
	default:
	}
	return l.statusLocked()
}

func (l *sessionPushLoop) Pause() ListenerStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.enabled = false
	return l.statusLocked()
}

func (l *sessionPushLoop) Stop() ListenerStatus {
	l.mu.Lock()
	l.enabled = false
	l.stopping = true
	cancel, doneCh, running, sid := l.cancel, l.doneCh, l.running, l.sessionID
	l.cancel = nil
	l.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if running && doneCh != nil {
		<-doneCh
	}
	l.backend.onStop(sid)
	return l.Status()
}

func (l *sessionPushLoop) Status() ListenerStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.statusLocked()
}

func (l *sessionPushLoop) statusLocked() ListenerStatus {
	return ListenerStatus{
		Running:        l.running && !l.stopping,
		Enabled:        l.enabled,
		SessionID:      l.sessionID,
		FullRawID:      l.fullRawID,
		Socket:         l.socket,
		DeliveredCount: l.deliveredCount,
		LastError:      l.lastError,
	}
}

func (l *sessionPushLoop) setError(msg string, disable bool) {
	l.mu.Lock()
	l.lastError = &msg
	if disable {
		l.enabled = false
	}
	l.mu.Unlock()
}

func (l *sessionPushLoop) setSocket(sock string) {
	l.mu.Lock()
	l.socket = sock
	l.mu.Unlock()
}

func (l *sessionPushLoop) waitIdleOrWake(ctx context.Context) bool {
	t := time.NewTimer(250 * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-l.wakeCh:
		return true
	case <-t.C:
		return true
	}
}

func (l *sessionPushLoop) pollOnce(ctx context.Context, knownSID string) (string, agents.WaitResult, error) {
	api, err := client.ResolveAPI(l.apiURL)
	if err != nil {
		return "", agents.WaitResult{}, err
	}
	payload := map[string]any{
		"harness": l.harness, "pid": l.ppid, "cwd": l.cwd, "timeout": l.pollTimeout, "mark_read": false,
	}
	if l.requireFullID {
		payload["require_full_id"] = true
	}
	if knownSID != "" {
		payload["session_id"] = knownSID
	}
	var resp agents.WaitResult
	err = client.CallContext(ctx, api, "POST", "/v1/agents/wait", payload, time.Duration((l.pollTimeout+15.0)*float64(time.Second)), nil, &resp)
	return api, resp, err
}

func (l *sessionPushLoop) loop(ctx context.Context, doneCh chan struct{}, gen uint64) {
	defer func() {
		l.mu.Lock()
		if l.gen == gen {
			l.running = false
			l.stopping = false
			l.cancel = nil
		}
		l.mu.Unlock()
		close(doneCh)
	}()

	backoff := 2.0
	for {
		if ctx.Err() != nil {
			return
		}
		l.mu.Lock()
		enabled, knownSID := l.enabled, l.sessionID
		l.mu.Unlock()

		if !enabled || !l.backend.prePoll(knownSID) {
			if !l.waitIdleOrWake(ctx) {
				return
			}
			continue
		}
		api, resp, err := l.pollOnce(ctx, knownSID)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			l.setError(err.Error(), false)
			sleepFor := backoff
			backoff = nextBackoff(backoff)
			if !sleepOrCancel(ctx.Done(), sleepFor) {
				return
			}
			continue
		}
		backoff = 2.0
		if ctx.Err() != nil {
			return
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
		if stillEnabled && len(resp.Messages) > 0 && l.backend.canDeliver(resp) {
			if errMsg, disable := l.backend.deliver(ctx, resp); errMsg != "" {
				l.setError(errMsg, disable)
			} else if ackErr := ackMessages(ctx, api, l.harness, resp.SessionID, resp.Messages); ackErr != nil {
				l.setError(ackErr.Error(), false)
				sleepFor := backoff
				backoff = nextBackoff(backoff)
				if !sleepOrCancel(ctx.Done(), sleepFor) {
					return
				}
				continue
			} else {
				l.mu.Lock()
				l.deliveredCount += len(resp.Messages)
				l.lastError = nil
				l.mu.Unlock()
			}
		}
	}
}

// CodexQueueListener runs inside `fleet mcp serve --harness codex` and pushes inbox messages via `codex queue`.
type CodexQueueListener struct {
	*sessionPushLoop
	codexBin   string
	runQueueFn RunQueueFn
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
	bin := opts.CodexBin
	if bin == "" {
		if opts.RunQueueFn != nil {
			bin = "codex"
		} else {
			bin = FindCodexBinary()
		}
	}
	l := &CodexQueueListener{codexBin: bin, runQueueFn: opts.RunQueueFn}
	l.sessionPushLoop = newSessionPushLoop("codex", opts.APIURL, opts.PPID, opts.CWD, opts.PollTimeout, true, l)
	return l
}

func (l *CodexQueueListener) mode() string { return "mcp_codex_queue" }

func (l *CodexQueueListener) preStart() (string, bool) {
	if l.codexBin == "" {
		return "codex binary not found", false
	}
	return "", true
}

func (l *CodexQueueListener) prePoll(string) bool { return l.codexBin != "" }

func (l *CodexQueueListener) canDeliver(resp agents.WaitResult) bool { return resp.FullRawID != "" }

func (l *CodexQueueListener) deliver(ctx context.Context, resp agents.WaitResult) (string, bool) {
	text := FormatWaitOutput(resp.SessionID, resp.Name, resp.Messages, "")
	cmd := []string{l.codexBin, "queue", "--thread", resp.FullRawID, "--message", text, "--resume"}
	var rc int
	var errDetail string
	if l.runQueueFn != nil {
		rc, errDetail = l.runQueueFn(cmd)
	} else {
		rc, errDetail = runCommandCapture(ctx, cmd, 15*time.Second)
	}
	if rc != 0 {
		return cmp.Or(errDetail, fmt.Sprintf("codex queue exited with status %d", rc)), true
	}
	return "", false
}

func (l *CodexQueueListener) onStop(string) {}
