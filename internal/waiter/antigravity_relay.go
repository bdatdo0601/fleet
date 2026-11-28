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
	return ackMessages(ctx, api, harness, batch.SessionID, batch.Messages)
}

// RunAntigravityRelay runs a device-level relay that long-polls /v1/agents/wait-harness and pushes messages via `agentapi`.
func RunAntigravityRelay(ctx context.Context, opts RelayOpts) (int, error) {
	harness := cmp.Or(opts.Harness, "antigravity")
	if harness != "antigravity" {
		return 1, fmt.Errorf("unsupported relay harness %q", harness)
	}
	return RunRelay(ctx, MultiRelayOpts{
		Harnesses:        []string{harness},
		APIURL:           opts.APIURL,
		AgentAPIBin:      opts.AgentAPIBin,
		PollTimeout:      opts.PollTimeout,
		LockDir:          opts.LockDir,
		Stderr:           opts.Stderr,
		RunSendFn:        opts.RunSendFn,
		ErrorBackoff:     opts.ErrorBackoff,
		LivenessInterval: -1,
	})
}
