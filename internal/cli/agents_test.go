package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/pki"
	"github.com/bdatdo0601/fleet/internal/session"
)

func runAgentCLI(args ...string) (int, string, string) {
	var out, errBuf bytes.Buffer
	rc := Run(args, &IO{
		Stdin:  strings.NewReader(""),
		Stdout: &out,
		Stderr: &errBuf,
	})
	return rc, out.String(), errBuf.String()
}

func TestCLIAgentsInspectSendInboxAskRenameListen(t *testing.T) {
	f, _, tmp := setupCLIServer(t)
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{
		Device: "mini", Harness: "claude", RawID: "coord-1", CWD: tmp, Title: new("Coordinator"),
		Tail: []agents.TailItem{{Role: "user", Text: "first"}, {Role: "assistant", Text: "second"}},
	})
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{
		Device: "mini", Harness: "codex", RawID: "worker-1", CWD: tmp, Title: new("Worker"),
	})

	// List --harness --json
	rc, out, errStr := runAgentCLI("agents", "ls", "--harness", "codex", "--json")
	if rc != 0 || !strings.Contains(out, "mini:codex:worker-1") || strings.Contains(out, "coord-1") {
		t.Fatalf("agents ls --json failed: rc=%d out=%q err=%q", rc, out, errStr)
	}

	// Inspect --tail 1 --json
	rc, out, errStr = runAgentCLI("agents", "inspect", "coordinator", "--tail", "1", "--json")
	if rc != 0 {
		t.Fatalf("agents inspect failed: rc=%d err=%q", rc, errStr)
	}
	var insp struct {
		Agent agents.SessionDetail `json:"agent"`
	}
	if err := json.Unmarshal([]byte(out), &insp); err != nil || len(insp.Agent.Tail) != 1 || insp.Agent.Tail[0].Text != "second" {
		t.Fatalf("unexpected inspect output: %s err=%v", out, err)
	}

	// Send + Inbox
	rc, out, errStr = runAgentCLI("agents", "send", "worker", "--session", "mini:claude:coord-1", "--message", "check build", "--json")
	if rc != 0 || !strings.Contains(out, `"body": "check build"`) {
		t.Fatalf("agents send failed: rc=%d out=%q err=%q", rc, out, errStr)
	}
	rc, out, errStr = runAgentCLI("agents", "inbox", "--session", "mini:codex:worker-1", "--json")
	if rc != 0 || !strings.Contains(out, "check build") {
		t.Fatalf("agents inbox failed: rc=%d out=%q err=%q", rc, out, errStr)
	}

	// Ask (peer replies asynchronously)
	go func() {
		for range 40 {
			msgs, _ := f.Agents.FetchInbox("mini", "mini:codex:worker-1", true, true, 10)
			if len(msgs) > 0 {
				_, _ = f.Agents.SendMessageWithReport("mini", "mini:codex:worker-1", "mini:claude:coord-1", "build green", &msgs[0].ThreadID, &msgs[0].ID, nil)
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()
	rc, out, errStr = runAgentCLI("agents", "ask", "worker", "--session", "mini:claude:coord-1", "--wait-if-idle", "--timeout", "5", "--question", "status?", "--json")
	if rc != 0 || !strings.Contains(out, "build green") {
		t.Fatalf("agents ask failed: rc=%d out=%q err=%q", rc, out, errStr)
	}

	// 1-arg self rename & listen
	rc, out, errStr = runAgentCLI("agents", "rename", "lead-agent", "--session", "mini:claude:coord-1", "--json")
	if rc != 0 || !strings.Contains(out, `"name": "lead-agent"`) {
		t.Fatalf("agents self rename failed: rc=%d out=%q err=%q", rc, out, errStr)
	}
	rc, out, errStr = runAgentCLI("agents", "listen", "--session", "mini:claude:coord-1", "--json")
	if rc != 0 || !strings.Contains(out, `"session_id": "mini:claude:coord-1"`) {
		t.Fatalf("agents listen failed: rc=%d out=%q err=%q", rc, out, errStr)
	}
}

func TestCLIAgentsSpawnKillAndLocalFallback(t *testing.T) {
	f, _, tmp := setupCLIServer(t)
	origStart := sessionStartFn
	origKill := sessionKillAgentFn
	origKillAll := sessionKillAllFn
	t.Cleanup(func() {
		sessionStartFn = origStart
		sessionKillAgentFn = origKill
		sessionKillAllFn = origKillAll
	})

	var started []session.StartOpts
	sessionStartFn = func(opts session.StartOpts) (string, error) {
		started = append(started, opts)
		return "tmux", nil
	}
	var killed []session.KillTarget
	sessionKillAgentFn = func(target session.KillTarget, _, _ io.Writer) error {
		killed = append(killed, target)
		return nil
	}
	var killAllCalls []string
	sessionKillAllFn = func(harness string, targets []session.KillTarget, _, _ io.Writer) error {
		killAllCalls = append(killAllCalls, harness+":"+string(rune('0'+len(targets))))
		return nil
	}

	// 1. Spawn on local device (mini) when slacker-worker is offline -> local fallback + receipt posted!
	repoDir := filepath.Join(tmp, "myrepo")
	_ = os.MkdirAll(repoDir, 0o755)
	rc, out, errStr := runAgentCLI("agents", "spawn", "--harness", "claude", "--name", "worker-a", "--repo", repoDir, "--prompt", "fix bug", "--policy", "safe", "--json")
	if rc != 0 {
		t.Fatalf("spawn local fallback failed: rc=%d out=%q err=%q", rc, out, errStr)
	}
	if len(started) != 1 || started[0].Name != "worker-a" || started[0].Policy != session.PolicySafe || started[0].Prompt != "fix bug" {
		t.Fatalf("unexpected local fallback StartOpts: %+v", started)
	}
	var spResp struct {
		Job struct {
			ID string `json:"id"`
		} `json:"job"`
		ExecutedLocally bool `json:"executed_locally"`
	}
	_ = json.Unmarshal([]byte(out), &spResp)
	if !spResp.ExecutedLocally {
		t.Fatalf("expected executed_locally=true, got %s", out)
	}
	if job, err := f.Slacker.GetJob(spResp.Job.ID); err != nil || job == nil || job.Status != "launched" || job.Backend != "tmux" {
		t.Fatalf("expected launched receipt recorded in slacker.db, got %+v err=%v", job, err)
	}

	// 2. Spawn targeting online remote device -> queues without local execution.
	lapPubFile := filepath.Join(tmp, "lap.pub")
	_ = pki.Keygen(filepath.Join(tmp, "lap.key"), lapPubFile)
	lapPub, _ := os.ReadFile(lapPubFile)
	_, _ = f.Approve(core.ApproveOpts{Name: "laptop", Pubkey: string(lapPub), Kind: "mac", Group: "managed", Passphrase: testPass})
	_ = f.Slacker.UpsertWorkerRuntime("laptop", 123, true, "")

	rc, out, errStr = runAgentCLI("agents", "spawn", "--harness", "agy", "--device", "laptop", "--model", "pro", "--repo", "/Users/dat/ws", "--json")
	if rc != 0 || !strings.Contains(out, `"worker_online": true`) || len(started) != 1 {
		t.Fatalf("remote spawn failed: rc=%d out=%q err=%q started=%d", rc, out, errStr, len(started))
	}

	// 3. Kill self guard + kill local fallback.
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{Device: "mini", Harness: "claude", RawID: "self-1", CWD: tmp})
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{Device: "mini", Harness: "codex", RawID: "peer-1", CWD: tmp})

	if rc, _, errStr = runAgentCLI("agents", "kill", "mini:claude:self-1", "--session", "mini:claude:self-1"); rc == 0 || !strings.Contains(errStr, "refusing to kill caller's own session") {
		t.Fatalf("expected self-kill guard error, got rc=%d err=%q", rc, errStr)
	}
	rc, out, errStr = runAgentCLI("agents", "kill", "mini:codex:peer-1", "--session", "mini:claude:self-1", "--json")
	if rc != 0 || !strings.Contains(out, `"executed_locally": true`) || len(killed) != 1 || killed[0].SessionID != "mini:codex:peer-1" {
		t.Fatalf("local kill fallback failed: rc=%d out=%q err=%q killed=%+v", rc, out, errStr, killed)
	}
	if det, err := f.Agents.GetSession("mini:codex:peer-1"); err != nil || det.State != "exited" {
		t.Fatalf("expected peer-1 exited, got %+v err=%v", det, err)
	}

	// 4. Kill-all requires --yes, then executes local fallback when confirmed.
	if rc, _, errStr = runAgentCLI("agents", "kill-all"); rc == 0 || !strings.Contains(errStr, "--yes") {
		t.Fatalf("expected kill-all without --yes to fail, got rc=%d err=%q", rc, errStr)
	}
	rc, out, errStr = runAgentCLI("agents", "kill-all", "--yes", "--device", "mini", "--json")
	if rc != 0 || !strings.Contains(out, `"executed_locally": true`) || len(killAllCalls) != 1 {
		t.Fatalf("kill-all failed: rc=%d out=%q err=%q calls=%v", rc, out, errStr, killAllCalls)
	}
}
