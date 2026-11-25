package waiter

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/client"
)

// RelayOpts configures RunAntigravityRelay.
type RelayOpts struct {
	Harness      string
	APIURL       string
	AgentAPIBin  string
	PollTimeout  float64
	LockDir      string
	Stderr       io.Writer
	RunSendFn    RunQueueFn
	ErrorBackoff time.Duration
}

// FindAgentAPIBinary locates the Antigravity `agentapi` binary.
func FindAgentAPIBinary() string {
	if envBin := strings.TrimSpace(os.Getenv("ANTIGRAVITY_AGENTAPI_EXE")); envBin != "" {
		if fi, err := os.Stat(envBin); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return envBin
		}
	}
	return findExecutable("agentapi", []string{".gemini/antigravity/bin/agentapi"})
}

func buildRelayTitle(msgs []agents.Message) string {
	if len(msgs) == 0 {
		return "Fleet peer messages"
	}
	firstFrom := msgs[0].FromSession
	for _, m := range msgs[1:] {
		if m.FromSession != firstFrom {
			return "Fleet peer messages"
		}
	}
	sender := strings.ReplaceAll(cmp.Or(msgs[0].FromName, firstFrom), "\n", " ")
	return agents.TruncateRunes("Fleet message from "+sender, 80)
}

func pruneBackedOff(backedOff map[string]time.Time, now time.Time) []string {
	var excluded []string
	for sid, until := range backedOff {
		if !now.Before(until) {
			delete(backedOff, sid)
			continue
		}
		excluded = append(excluded, sid)
	}
	sort.Strings(excluded)
	return excluded
}

func relayDeliverBatch(ctx context.Context, api, harness, bin string, batch agents.SessionMessages, runFn RunQueueFn) error {
	text := FormatWaitOutput(batch.SessionID, batch.Name, batch.Messages, "")
	title := buildRelayTitle(batch.Messages)
	cmd := []string{bin, "send-message", "--title=" + title, batch.FullRawID, text}
	var rc int
	var errDetail string
	if runFn != nil {
		rc, errDetail = runFn(cmd)
	} else {
		rc, errDetail = runCommandCapture(ctx, cmd, 15*time.Second)
	}
	if rc != 0 {
		return errors.New(cmp.Or(errDetail, fmt.Sprintf("agentapi send-message exited with status %d", rc)))
	}
	ackMessages(ctx, api, harness, batch.SessionID, batch.Messages)
	return nil
}

// RunAntigravityRelay runs a device-level relay that long-polls /v1/agents/wait-harness and pushes messages via `agentapi`.
func RunAntigravityRelay(ctx context.Context, opts RelayOpts) (int, error) {
	harness := cmp.Or(opts.Harness, "antigravity")
	if harness != "antigravity" {
		return 1, fmt.Errorf("unsupported relay harness %q", harness)
	}
	api, err := client.ResolveAPI(opts.APIURL)
	if err != nil {
		return 1, err
	}
	bin := opts.AgentAPIBin
	if bin == "" {
		if opts.RunSendFn != nil {
			bin = "agentapi"
		} else {
			bin = FindAgentAPIBinary()
		}
	}
	if bin == "" {
		return 1, fmt.Errorf("agentapi binary not found (set ANTIGRAVITY_AGENTAPI_EXE or install Antigravity)")
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	errorBackoff := opts.ErrorBackoff
	if errorBackoff <= 0 {
		errorBackoff = 60 * time.Second
	}
	pollTimeout := clampPollTimeout(opts.PollTimeout)

	lockPath, err := acquirePIDLock("relay-"+harness, opts.LockDir)
	if err != nil {
		return 1, err
	}
	defer releasePIDLock(lockPath)

	backedOff := map[string]time.Time{}
	backoff := 2.0
	for {
		if ctx.Err() != nil {
			return 0, nil
		}
		payload := map[string]any{
			"harness":         harness,
			"timeout":         pollTimeout,
			"mark_read":       false,
			"require_full_id": true,
		}
		if excluded := pruneBackedOff(backedOff, time.Now()); len(excluded) > 0 {
			payload["exclude_ids"] = excluded
		}
		var resp agents.HarnessWaitResult
		if err := client.CallContext(ctx, api, "POST", "/v1/agents/wait-harness", payload, time.Duration((pollTimeout+15.0)*float64(time.Second)), nil, &resp); err != nil {
			if ctx.Err() != nil {
				return 0, nil
			}
			_, _ = fmt.Fprintf(stderr, "fleet relay wait error: %v\n", err)
			sleepFor := backoff
			backoff = nextBackoff(backoff)
			if !sleepOrCancel(ctx.Done(), sleepFor) {
				return 0, nil
			}
			continue
		}
		backoff = 2.0
		for _, batch := range resp.Batches {
			if ctx.Err() != nil {
				return 0, nil
			}
			if err := relayDeliverBatch(ctx, api, harness, bin, batch, opts.RunSendFn); err != nil {
				_, _ = fmt.Fprintf(stderr, "fleet relay delivery failed for %s: %v\n", batch.SessionID, err)
				backedOff[batch.SessionID] = time.Now().Add(errorBackoff)
				continue
			}
			delete(backedOff, batch.SessionID)
		}
	}
}
