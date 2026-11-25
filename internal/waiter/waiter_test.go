package waiter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
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
	"github.com/bdatdo0601/fleet/internal/hooks"
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
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{Device: "mini", Harness: "antigravity", RawID: "agy-wait", Title: new("Deploy Helper"), CWD: "/d"})
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{Device: "mini", Harness: "claude", RawID: "sender-w", Title: new("Coordinator"), CWD: "/c"})

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
		_, _ = f.Agents.SendMessageWithReport("mini", "mini:claude:sender-w", "deploy-helper", "run migration", nil, nil, nil)
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

	for range 40 {
		if s, err := f.Agents.GetSession("01a1ece5-f00"); err == nil && s.Listening {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if s, _ := f.Agents.GetSession("01a1ece5-f00"); !s.Listening {
		t.Fatal("expected listener to be listening")
	}

	_, _ = f.Agents.SendMessageWithReport("mini", "mini:claude:peer-1", "01a1ece5-f00", "check diff", nil, nil, nil)
	for range 50 {
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
	for range 40 {
		if s, _ := f.Agents.GetSession("01a1ece5-f00"); s.Listening {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	mu.Lock()
	rcMode = 1
	errMode = "daemon unavailable"
	mu.Unlock()

	_, _ = f.Agents.SendMessageWithReport("mini", "mini:claude:peer-1", "01a1ece5-f00", "still unread", nil, nil, nil)
	for range 50 {
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

func TestClaudeSocketListenerDeliveryAndNoSocketGuard(t *testing.T) {
	f, srv, tmp := setupWaiterServer(t)
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{Device: "mini", Harness: "codex", RawID: "peer-c", CWD: "/p"})
	pid := 8888
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{Device: "mini", Harness: "claude", RawID: "cl-push-1", PID: &pid, CWD: "/claude", State: "idle"})

	cacheDir := filepath.Join(tmp, "cache")
	listener := NewClaudeSocketListener(ClaudeListenerOpts{
		APIURL:      srv.URL,
		PPID:        pid,
		CWD:         "/claude",
		CacheDir:    cacheDir,
		PollTimeout: 0.5,
	})
	t.Cleanup(func() { listener.Stop() })
	listener.Start()

	// Without a socket file, listener must NOT mark the session as listening.
	time.Sleep(150 * time.Millisecond)
	if s, _ := f.Agents.GetSession("cl-push-1"); s.Listening {
		t.Fatalf("expected listening=false when no socket exists, got %+v", s)
	}

	// Use a short /tmp dir to stay within macOS 104-byte sockaddr_un.sun_path limit.
	sockDir, err := os.MkdirTemp("/tmp", "fl-sock-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "c.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	var mu sync.Mutex
	var receivedLines [][]string
	acceptErrCh := make(chan error, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			var lines []string
			sc := bufio.NewScanner(conn)
			for sc.Scan() {
				lines = append(lines, sc.Text())
			}
			_ = conn.Close()
			if err := sc.Err(); err != nil {
				acceptErrCh <- err
				return
			}
			mu.Lock()
			receivedLines = append(receivedLines, lines)
			mu.Unlock()
		}
	}()

	if err := hooks.SaveClaudeSocket(cacheDir, hooks.ClaudeSocketInfo{
		Socket: sockPath, Token: "secret-tok", SessionID: "mini:claude:cl-push-1", PID: pid,
	}); err != nil {
		t.Fatal(err)
	}

	for range 40 {
		if s, _ := f.Agents.GetSession("cl-push-1"); s.Listening {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if s, _ := f.Agents.GetSession("cl-push-1"); !s.Listening {
		t.Fatal("expected claude session to become listening once socket exists")
	}

	_, _ = f.Agents.SendMessageWithReport("mini", "mini:codex:peer-c", "cl-push-1", "rebase ready", nil, nil, nil)
	for range 50 {
		if listener.Status().DeliveredCount == 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	st := listener.Status()
	if st.DeliveredCount != 1 || st.Socket != sockPath {
		t.Fatalf("unexpected claude listener status: %+v", st)
	}
	select {
	case err := <-acceptErrCh:
		t.Fatal(err)
	default:
	}

	mu.Lock()
	if len(receivedLines) != 1 || len(receivedLines[0]) != 2 {
		t.Fatalf("expected 1 conn with 2 JSON lines, got %+v", receivedLines)
	}
	var authFrame, userFrame map[string]any
	_ = json.Unmarshal([]byte(receivedLines[0][0]), &authFrame)
	_ = json.Unmarshal([]byte(receivedLines[0][1]), &userFrame)
	mu.Unlock()

	if authFrame["type"] != "auth" || authFrame["token"] != "secret-tok" {
		t.Fatalf("unexpected auth frame: %+v", authFrame)
	}
	msgObj, _ := userFrame["message"].(map[string]any)
	if userFrame["type"] != "user" || msgObj["role"] != "user" || !strings.Contains(msgObj["content"].(string), "rebase ready") {
		t.Fatalf("unexpected user frame: %+v", userFrame)
	}
	if inbox, _ := f.Agents.FetchInbox("mini", "mini:claude:cl-push-1", true, true, 50); len(inbox) != 0 {
		t.Fatalf("expected acked inbox empty, got %+v", inbox)
	}

	// Removing socket pauses listener on next send failure without acking.
	_ = ln.Close()
	_ = os.Remove(sockPath)
	_, _ = f.Agents.SendMessageWithReport("mini", "mini:codex:peer-c", "cl-push-1", "keep unread", nil, nil, nil)
	for range 50 {
		if !listener.Status().Enabled && listener.Status().LastError != nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if st2 := listener.Status(); st2.Enabled || st2.LastError == nil {
		t.Fatalf("expected listener paused on socket error, got %+v", st2)
	}
	if inbox, _ := f.Agents.FetchInbox("mini", "mini:claude:cl-push-1", true, true, 50); len(inbox) != 1 || inbox[0].Body != "keep unread" {
		t.Fatalf("expected unacked message after socket error, got %+v", inbox)
	}
}

func TestAntigravityRelayDeliveryAndPerSessionBackoff(t *testing.T) {
	f, srv, tmp := setupWaiterServer(t)
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{Device: "mini", Harness: "claude", RawID: "coord", Title: new("Coordinator"), CWD: "/c"})
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{Device: "mini", Harness: "antigravity", RawID: "11111111-1111-1111-1111-111111111111", CWD: "/a1", State: "idle"})
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{Device: "mini", Harness: "antigravity", RawID: "22222222-2222-2222-2222-222222222222", CWD: "/a2", State: "idle"})

	var mu sync.Mutex
	var calls [][]string
	failConv := "11111111-1111-1111-1111-111111111111"
	fakeSend := func(cmd []string) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, cmd)
		if len(cmd) >= 4 && cmd[3] == failConv {
			return 1, "conversation locked"
		}
		return 0, ""
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var stderr bytes.Buffer
	go func() {
		_, err := RunAntigravityRelay(ctx, RelayOpts{
			Harness:      "antigravity",
			APIURL:       srv.URL,
			AgentAPIBin:  "/usr/local/bin/agentapi",
			PollTimeout:  0.5,
			LockDir:      filepath.Join(tmp, "locks"),
			Stderr:       &stderr,
			RunSendFn:    fakeSend,
			ErrorBackoff: 5 * time.Second,
		})
		done <- err
	}()

	for range 40 {
		if s, _ := f.Agents.GetSession("22222222-222"); s.Listening {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if s, _ := f.Agents.GetSession("22222222-222"); !s.Listening {
		t.Fatal("expected relay to mark antigravity sessions as listening")
	}

	// Message to failing session triggers backoff without blocking healthy session.
	_, _ = f.Agents.SendMessageWithReport("mini", "mini:claude:coord", "11111111-111", "msg fail", nil, nil, nil)
	_, _ = f.Agents.SendMessageWithReport("mini", "mini:claude:coord", "22222222-222", "msg ok", nil, nil, nil)

	for range 60 {
		mu.Lock()
		n := len(calls)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	snapshot := append([][]string(nil), calls...)
	mu.Unlock()
	if len(snapshot) != 2 {
		t.Fatalf("expected exactly 2 send-message calls (failing session backed off), got %d: %+v", len(snapshot), snapshot)
	}
	var okCall []string
	for _, c := range snapshot {
		if len(c) >= 5 && c[3] == "22222222-2222-2222-2222-222222222222" {
			okCall = c
		}
	}
	if okCall == nil || okCall[1] != "send-message" || !strings.HasPrefix(okCall[2], "--title=Fleet message from coordinator") || !strings.Contains(okCall[4], "msg ok") {
		t.Fatalf("unexpected healthy agentapi call: %+v", okCall)
	}
	if inbox2, _ := f.Agents.FetchInbox("mini", "mini:antigravity:22222222-222", true, true, 50); len(inbox2) != 0 {
		t.Fatalf("expected healthy inbox acked, got %+v", inbox2)
	}
	if inbox1, _ := f.Agents.FetchInbox("mini", "mini:antigravity:11111111-111", true, true, 50); len(inbox1) != 1 || inbox1[0].Body != "msg fail" {
		t.Fatalf("expected failing inbox unacked, got %+v", inbox1)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("relay exited with err: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not stop on context cancel")
	}
}
