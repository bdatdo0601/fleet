package waiter

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/pki"
	"github.com/bdatdo0601/fleet/internal/server"
)

func setupWaiterServer(t *testing.T) (*core.Fleet, *httptest.Server, string) {
	t.Helper()
	tmp := t.TempDir()
	pubFile := filepath.Join(tmp, "mini.pub")
	if err := pki.Keygen(filepath.Join(tmp, "mini.key"), pubFile); err != nil {
		t.Skip("nebula-cert not installed")
	}
	miniPub, _ := os.ReadFile(pubFile)
	f, err := core.Initialize(core.InitOpts{
		Root: filepath.Join(tmp, "state"), Host: "h.example.com", Port: 443,
		Passphrase: "correct horse battery staple", LighthousePubkey: string(miniPub),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	srv := httptest.NewServer(server.NewHandler(f, true))
	t.Cleanup(srv.Close)
	return f, srv, tmp
}

func TestWaitForInboxCLIAndPIDLockReplacement(t *testing.T) {
	f, srv, tmp := setupWaiterServer(t)
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{Device: "mini", Harness: "antigravity", RawID: "agy-wait", Title: ptr("Deploy Helper"), CWD: "/d"})
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{Device: "mini", Harness: "claude", RawID: "sender-w", Title: ptr("Coordinator"), CWD: "/c"})

	lockDir := filepath.Join(tmp, "locks")
	_ = os.MkdirAll(lockDir, 0o755)
	dummy := exec.Command("sleep", "30")
	if err := dummy.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if dummy.Process != nil {
			_ = dummy.Process.Kill()
			_, _ = dummy.Process.Wait()
		}
	})
	lp := pidLockPath("mini:antigravity:agy-wait", lockDir)
	_ = os.WriteFile(lp, []byte(fmt.Sprintf("%d\n", dummy.Process.Pid)), 0o600)

	go func() {
		time.Sleep(150 * time.Millisecond)
		_, _ = f.Agents.SendMessage("mini", "mini:claude:sender-w", "deploy-helper", "run migration", nil, nil, nil)
	}()

	var out bytes.Buffer
	rc, err := WaitForInboxCLI(WaitCLIOpts{
		SessionID:   "mini:antigravity:agy-wait",
		APIURL:      srv.URL,
		Harness:     "antigravity",
		MaxWait:     5.0,
		PollTimeout: 2.0,
		Stdout:      &out,
		LockDir:     lockDir,
		WatchParent: false,
	})
	if err != nil || rc != 0 {
		t.Fatalf("WaitForInboxCLI failed: rc=%d err=%v", rc, err)
	}
	// Wait for dummy process reaping
	_ = dummy.Wait()
	if _, err := os.Stat(lp); !os.IsNotExist(err) {
		t.Fatalf("expected lockfile removed, stat err=%v", err)
	}
	text := out.String()
	if !strings.Contains(text, "coordinator (mini:claude:sender-w)") ||
		!strings.Contains(text, "run migration") ||
		!strings.Contains(text, "fleet agents wait --session mini:antigravity:agy-wait") {
		t.Fatalf("unexpected wait output: %q", text)
	}
}

func TestCodexQueueListenerSuccessAndFailurePause(t *testing.T) {
	f, srv, _ := setupWaiterServer(t)
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{Device: "mini", Harness: "claude", RawID: "peer-1", CWD: "/p"})
	pid := 7777
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{Device: "mini", Harness: "codex", RawID: "pid-7777", PID: &pid, CWD: "/codex", State: "idle"})

	var mu sync.Mutex
	var recorded [][]string
	rcMode := 0
	errMode := ""
	fakeQueue := func(cmd []string) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		recorded = append(recorded, cmd)
		return rcMode, errMode
	}

	listener := NewCodexQueueListener(CodexListenerOpts{
		APIURL:      srv.URL,
		PPID:        7777,
		CWD:         "/codex",
		CodexBin:    "/usr/local/bin/codex",
		PollTimeout: 0.5,
		RunQueueFn:  fakeQueue,
	})
	t.Cleanup(func() { listener.Stop() })
	listener.Start()

	fullUUID := "01a1ece5-f00c-7860-a0dc-6c1897e73f30"
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{Device: "mini", Harness: "codex", RawID: fullUUID, PID: &pid, CWD: "/codex", State: "idle"})

	for i := 0; i < 40; i++ {
		if s, err := f.Agents.GetSession("01a1ece5-f00"); err == nil && s.Listening {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if s, _ := f.Agents.GetSession("01a1ece5-f00"); !s.Listening {
		t.Fatal("expected listener to be listening")
	}

	_, _ = f.Agents.SendMessage("mini", "mini:claude:peer-1", "01a1ece5-f00", "check diff", nil, nil, nil)
	for i := 0; i < 50; i++ {
		if listener.Status().DeliveredCount == 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if listener.Status().DeliveredCount != 1 {
		t.Fatalf("expected delivered_count=1, got %+v", listener.Status())
	}
	mu.Lock()
	cmd0 := recorded[0]
	mu.Unlock()
	if len(cmd0) < 6 || cmd0[0] != "/usr/local/bin/codex" || cmd0[1] != "queue" || cmd0[3] != fullUUID {
		t.Fatalf("unexpected codex queue cmd: %v", cmd0)
	}
	if inbox, _ := f.Agents.FetchInbox("mini", "mini:codex:01a1ece5-f00", true, true, 50); len(inbox) != 0 {
		t.Fatalf("expected acked inbox empty, got %+v", inbox)
	}

	// Failure pauses listener without acking
	for i := 0; i < 40; i++ {
		if s, _ := f.Agents.GetSession("01a1ece5-f00"); s.Listening {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	mu.Lock()
	rcMode = 1
	errMode = "daemon unavailable"
	mu.Unlock()

	_, _ = f.Agents.SendMessage("mini", "mini:claude:peer-1", "01a1ece5-f00", "still unread", nil, nil, nil)
	for i := 0; i < 50; i++ {
		st := listener.Status()
		if st.LastError != nil && *st.LastError == "daemon unavailable" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	st := listener.Status()
	if st.Enabled || st.LastError == nil || *st.LastError != "daemon unavailable" {
		t.Fatalf("expected paused on error, got %+v", st)
	}
	inbox, _ := f.Agents.FetchInbox("mini", "mini:codex:01a1ece5-f00", true, true, 50)
	if len(inbox) != 1 || inbox[0].Body != "still unread" {
		t.Fatalf("expected unacked message, got %+v", inbox)
	}
}

func ptr[T any](v T) *T { return &v }
