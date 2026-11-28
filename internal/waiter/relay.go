package waiter

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/hooks"
	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/usage"
)

const defaultLivenessInterval = 45 * time.Second

// MultiRelayOpts configures RunRelay across one or more local agent harnesses.
type MultiRelayOpts struct {
	Harnesses        []string
	APIURL           string
	AgentAPIBin      string
	CodexBin         string
	CacheDir         string
	PollTimeout      float64
	LockDir          string
	Stderr           io.Writer
	RunSendFn        RunQueueFn
	SendClaudeFn     SendClaudeSocketFn
	PIDAliveFn       func(int) bool
	ErrorBackoff     time.Duration
	LivenessInterval time.Duration
	LocalDevice      string
}

func normalizeRelayHarnesses(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return []string{"claude", "codex"}, nil
	}
	seen := map[string]bool{}
	var out []string
	for _, h := range raw {
		norm := strings.ToLower(strings.TrimSpace(h))
		if norm == "agy" {
			norm = "antigravity"
		}
		if norm == "" {
			continue
		}
		if !paths.ValidHarness(norm) {
			return nil, fmt.Errorf("unsupported relay harness %q", h)
		}
		if !seen[norm] {
			seen[norm] = true
			out = append(out, norm)
		}
	}
	if len(out) == 0 {
		return []string{"claude", "codex"}, nil
	}
	slices.Sort(out)
	return out, nil
}

func resolveRelayAgentAPIBin(opts MultiRelayOpts) string {
	if opts.AgentAPIBin != "" {
		return opts.AgentAPIBin
	}
	if opts.RunSendFn != nil {
		return "agentapi"
	}
	return FindAgentAPIBinary()
}

func resolveRelayCodexBin(opts MultiRelayOpts) string {
	if opts.CodexBin != "" {
		return opts.CodexBin
	}
	if opts.RunSendFn != nil {
		return "codex"
	}
	return FindCodexBinary()
}

func deliverClaudeBatch(ctx context.Context, api string, batch agents.SessionMessages, opts MultiRelayOpts) error {
	pidVal := 0
	if batch.PID != nil {
		pidVal = *batch.PID
	}
	info, ok := hooks.LoadClaudeSocket(opts.CacheDir, pidVal, batch.SessionID)
	if !ok {
		return fmt.Errorf("claude messaging socket unavailable for %s", batch.SessionID)
	}
	if opts.SendClaudeFn == nil {
		if _, err := os.Stat(info.Socket); err != nil {
			return fmt.Errorf("claude messaging socket unavailable: %w", err)
		}
	}
	sendFn := opts.SendClaudeFn
	if sendFn == nil {
		sendFn = SendClaudeSocketMessage
	}
	text := FormatWaitOutput(batch.SessionID, batch.Name, batch.Messages, "")
	if err := sendFn(info.Socket, info.Token, text); err != nil {
		return err
	}
	return ackMessages(ctx, api, "claude", batch.SessionID, batch.Messages)
}

func deliverCodexBatch(ctx context.Context, api, codexBin string, batch agents.SessionMessages, runFn RunQueueFn) error {
	bin := codexBin
	if bin == "" {
		bin = FindCodexBinary()
	}
	if bin == "" {
		return errors.New("codex binary not found")
	}
	text := FormatWaitOutput(batch.SessionID, batch.Name, batch.Messages, "")
	cmd := []string{bin, "queue", "--thread", batch.FullRawID, "--message", text, "--resume"}
	var rc int
	var errDetail string
	if runFn != nil {
		rc, errDetail = runFn(cmd)
	} else {
		rc, errDetail = runCommandCapture(ctx, cmd, 15*time.Second)
	}
	if rc != 0 {
		return errors.New(cmp.Or(errDetail, fmt.Sprintf("codex queue exited with status %d", rc)))
	}
	return ackMessages(ctx, api, "codex", batch.SessionID, batch.Messages)
}

func deliverRelayBatch(ctx context.Context, api, harness, agentAPIBin, codexBin string, batch agents.SessionMessages, opts MultiRelayOpts) error {
	switch harness {
	case "antigravity":
		if agentAPIBin == "" {
			return errors.New("agentapi binary not found")
		}
		return relayDeliverBatch(ctx, api, harness, agentAPIBin, batch, opts.RunSendFn)
	case "claude":
		return deliverClaudeBatch(ctx, api, batch, opts)
	case "codex":
		return deliverCodexBatch(ctx, api, codexBin, batch, opts.RunSendFn)
	default:
		return fmt.Errorf("unsupported relay harness %q", harness)
	}
}

func runHarnessRelayLoop(ctx context.Context, api, harness, agentAPIBin, codexBin string, pollTimeout float64, errorBackoff time.Duration, opts MultiRelayOpts) {
	requireFullID := harness == "antigravity" || harness == "codex"
	backedOff := map[string]time.Time{}
	backoff := 2.0
	for {
		if ctx.Err() != nil {
			return
		}
		payload := map[string]any{
			"harness":         harness,
			"timeout":         pollTimeout,
			"mark_read":       false,
			"require_full_id": requireFullID,
		}
		if excluded := pruneBackedOff(backedOff, time.Now()); len(excluded) > 0 {
			payload["exclude_ids"] = excluded
		}
		var resp agents.HarnessWaitResult
		if err := client.CallContext(ctx, api, http.MethodPost, "/v1/agents/wait-harness", payload, time.Duration((pollTimeout+15.0)*float64(time.Second)), nil, &resp); err != nil {
			if ctx.Err() != nil {
				return
			}
			_, _ = fmt.Fprintf(opts.Stderr, "fleet relay (%s) wait error: %v\n", harness, err)
			sleepFor := backoff
			backoff = nextBackoff(backoff)
			if !sleepOrCancel(ctx.Done(), sleepFor) {
				return
			}
			continue
		}
		backoff = 2.0
		for _, batch := range resp.Batches {
			if ctx.Err() != nil {
				return
			}
			if err := deliverRelayBatch(ctx, api, harness, agentAPIBin, codexBin, batch, opts); err != nil {
				_, _ = fmt.Fprintf(opts.Stderr, "fleet relay (%s) delivery failed for %s: %v\n", harness, batch.SessionID, err)
				backedOff[batch.SessionID] = time.Now().Add(errorBackoff)
				continue
			}
			delete(backedOff, batch.SessionID)
		}
	}
}

func sessionShortRawID(sessionID string) string {
	parts := strings.SplitN(sessionID, ":", 3)
	if len(parts) == 3 {
		return parts[2]
	}
	return sessionID
}

func runLivenessSweep(ctx context.Context, api, localDevice string, managed map[string]bool, cacheDir string, aliveFn func(int) bool) {
	dev := strings.TrimSpace(localDevice)
	if dev == "" {
		if node, err := client.LoadNode(); err == nil {
			dev = node.Name
		}
	}
	if dev == "" {
		return
	}
	var resp struct {
		Agents []agents.Session `json:"agents"`
	}
	if err := client.CallContext(ctx, api, http.MethodGet, "/v1/agents?device="+url.QueryEscape(dev), nil, 10*time.Second, nil, &resp); err != nil {
		return
	}
	for _, s := range resp.Agents {
		if ctx.Err() != nil {
			return
		}
		if !managed[s.Harness] || s.PID == nil || *s.PID <= 1 {
			continue
		}
		if !aliveFn(*s.PID) {
			_ = client.CallContext(ctx, api, http.MethodPost, "/v1/agents/deregister", map[string]any{
				"session_id": s.SessionID,
			}, 5*time.Second, nil, nil)
			if s.Harness == "claude" {
				hooks.RemoveClaudeSocket(cacheDir, *s.PID, s.SessionID)
			}
			continue
		}
		if s.AgeSeconds >= 60 {
			rawID := cmp.Or(s.FullRawID, sessionShortRawID(s.SessionID))
			hb := agents.HeartbeatParams{
				Harness: s.Harness,
				RawID:   rawID,
				PID:     s.PID,
				CWD:     s.CWD,
				State:   s.State,
			}
			if tp := hooks.FindSessionTranscript(s.Harness, s.FullRawID, s.CWD); tp != "" {
				if _, tail := hooks.ReadTranscriptTail(s.Harness, tp, s.CWD, 20); len(tail) > 0 {
					hb.Tail = tail
				}
				if title, isExplicit := hooks.ReadSessionTitle(s.Harness, rawID, tp, "", ""); title != "" {
					hb.Title = &title
					if isExplicit {
						hb.Name = &title
						hb.ExplicitName = true
					}
				}
			}
			_ = client.CallContext(ctx, api, http.MethodPost, "/v1/agents/heartbeat", hb, 5*time.Second, nil, nil)
		}
	}
}

func runLivenessLoop(ctx context.Context, api, localDevice string, managed map[string]bool, cacheDir string, interval time.Duration, aliveFn func(int) bool) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runLivenessSweep(ctx, api, localDevice, managed, cacheDir, aliveFn)
		}
	}
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// RunRelay runs device-level long-poll relays for the requested harnesses plus a local PID liveness sweep for claude/codex.
func RunRelay(ctx context.Context, opts MultiRelayOpts) (int, error) {
	harnesses, err := normalizeRelayHarnesses(opts.Harnesses)
	if err != nil {
		return 1, err
	}
	api, err := client.ResolveAPI(opts.APIURL)
	if err != nil {
		return 1, err
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	opts.Stderr = &syncWriter{w: stderr}
	errorBackoff := opts.ErrorBackoff
	if errorBackoff <= 0 {
		errorBackoff = 60 * time.Second
	}
	pollTimeout := clampPollTimeout(opts.PollTimeout)

	var agentAPIBin, codexBin string
	if slices.Contains(harnesses, "antigravity") {
		agentAPIBin = resolveRelayAgentAPIBin(opts)
		if agentAPIBin == "" && len(harnesses) == 1 {
			return 1, errors.New("agentapi binary not found (set ANTIGRAVITY_AGENTAPI_EXE or install Antigravity)")
		}
	}
	if slices.Contains(harnesses, "codex") {
		codexBin = resolveRelayCodexBin(opts)
	}

	lockFile, err := acquirePIDLock("relay-"+strings.Join(harnesses, "-"), opts.LockDir)
	if err != nil {
		return 1, err
	}
	defer releasePIDLock(lockFile)

	if opts.RunSendFn == nil && opts.SendClaudeFn == nil && os.Getenv("FLEET_DISABLE_USAGE_REPORTER") != "1" {
		usage.StartPeriodicReporter(ctx, 10*time.Minute, api)
	}

	var wg sync.WaitGroup
	managedLiveness := map[string]bool{}
	for _, h := range harnesses {
		if h == "claude" || h == "codex" {
			managedLiveness[h] = true
		}
		wg.Go(func() {
			runHarnessRelayLoop(ctx, api, h, agentAPIBin, codexBin, pollTimeout, errorBackoff, opts)
		})
	}

	livenessInterval := opts.LivenessInterval
	if livenessInterval == 0 {
		livenessInterval = defaultLivenessInterval
	}
	if len(managedLiveness) > 0 && livenessInterval > 0 {
		aliveFn := opts.PIDAliveFn
		if aliveFn == nil {
			aliveFn = pidAlive
		}
		wg.Go(func() {
			runLivenessLoop(ctx, api, opts.LocalDevice, managedLiveness, opts.CacheDir, livenessInterval, aliveFn)
		})
	}

	wg.Wait()
	return 0, nil
}
