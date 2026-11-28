package waiter

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

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

func TestWaitForInboxCLIAndFlockContention(t *testing.T) {
	f, srv, tmp := setupWaiterServer(t)
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{Device: "mini", Harness: "antigravity", RawID: "agy-wait", Title: new("Deploy Helper"), CWD: "/d"})
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{Device: "mini", Harness: "claude", RawID: "sender-w", Title: new("Coordinator"), CWD: "/c"})

	lockDir := filepath.Join(tmp, "locks")
	heldLock, err := acquirePIDLock("mini:antigravity:agy-wait", lockDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WaitForInboxCLI(WaitCLIOpts{
		SessionID:   "mini:antigravity:agy-wait",
		APIURL:      srv.URL,
		Harness:     "antigravity",
		MaxWait:     1.0,
		PollTimeout: 0.5,
		LockDir:     lockDir,
	}); err == nil {
		releasePIDLock(heldLock)
		t.Fatal("expected WaitForInboxCLI to fail when flock is already held")
	}
	releasePIDLock(heldLock)

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
	lp := pidLockPath("mini:antigravity:agy-wait", lockDir)
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

func TestRunRelayClaudeCodexAndLivenessSweep(t *testing.T) {
	f, srv, tmp := setupWaiterServer(t)
	cacheDir := filepath.Join(tmp, "cache")
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{Device: "mini", Harness: "antigravity", RawID: "sender-a", CWD: "/s"})

	claudePID := 4401
	deadCodexPID := 4402
	aliveCodexPID := 4403
	codexUUID := "01a1ece5-f00c-7860-a0dc-6c1897e73f99"

	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{Device: "mini", Harness: "claude", RawID: "cl-relay-1", PID: &claudePID, CWD: "/cl", State: "idle"})
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{Device: "mini", Harness: "codex", RawID: codexUUID, PID: &aliveCodexPID, CWD: "/cx", State: "idle"})
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{Device: "mini", Harness: "codex", RawID: "cx-dead-1", PID: &deadCodexPID, CWD: "/dead", State: "working"})

	claudeSock := filepath.Join(tmp, "mock-claude.sock")
	deadSock := filepath.Join(tmp, "mock-dead.sock")
	_ = os.WriteFile(claudeSock, nil, 0o600)
	_ = os.WriteFile(deadSock, nil, 0o600)

	if err := hooks.SaveClaudeSocket(cacheDir, hooks.ClaudeSocketInfo{
		Socket: claudeSock, Token: "t1", SessionID: "mini:claude:cl-relay-1", PID: claudePID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := hooks.SaveClaudeSocket(cacheDir, hooks.ClaudeSocketInfo{
		Socket: deadSock, Token: "t2", SessionID: "mini:claude:cl-dead", PID: 4499,
	}); err != nil {
		t.Fatal(err)
	}

	// Age cl-relay-1 by 90s so the liveness sweep refreshes its heartbeat while preserving State="idle".
	db, err := sql.Open("sqlite", filepath.Join(f.Root, "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	oldTS := time.Now().UTC().Add(-90 * time.Second).Format("2006-01-02T15:04:05+00:00")
	if _, err := db.Exec("UPDATE agent_sessions SET updated_at = ? WHERE session_id = ?", oldTS, "mini:claude:cl-relay-1"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()

	var mu sync.Mutex
	var claudeTexts []string
	var codexCmds [][]string

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var stderr bytes.Buffer
	go func() {
		_, rErr := RunRelay(ctx, MultiRelayOpts{
			Harnesses:        []string{"claude", "codex"},
			APIURL:           srv.URL,
			CodexBin:         "/usr/local/bin/codex",
			CacheDir:         cacheDir,
			PollTimeout:      0.5,
			LockDir:          filepath.Join(tmp, "locks"),
			Stderr:           &stderr,
			LocalDevice:      "mini",
			LivenessInterval: 100 * time.Millisecond,
			SendClaudeFn: func(_, _, text string) error {
				mu.Lock()
				claudeTexts = append(claudeTexts, text)
				mu.Unlock()
				return nil
			},
			RunSendFn: func(cmd []string) (int, string) {
				mu.Lock()
				codexCmds = append(codexCmds, cmd)
				mu.Unlock()
				return 0, ""
			},
			PIDAliveFn: func(pid int) bool {
				return pid != deadCodexPID
			},
		})
		done <- rErr
	}()

	for range 40 {
		s1, _ := f.Agents.GetSession("cl-relay-1")
		s2, _ := f.Agents.GetSession("01a1ece5-f00")
		if s1.Listening && s2.Listening {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	_, _ = f.Agents.SendMessageWithReport("mini", "mini:antigravity:sender-a", "cl-relay-1", "hello claude relay", nil, nil, nil)
	_, _ = f.Agents.SendMessageWithReport("mini", "mini:antigravity:sender-a", "01a1ece5-f00", "hello codex relay", nil, nil, nil)

	for range 60 {
		mu.Lock()
		gotBoth := len(claudeTexts) == 1 && len(codexCmds) == 1
		mu.Unlock()
		deadSess, _ := f.Agents.GetSession("cx-dead-1")
		clSess, _ := f.Agents.GetSession("cl-relay-1")
		if gotBoth && deadSess.State == "exited" && clSess.AgeSeconds < 30 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	mu.Lock()
	ct := append([]string(nil), claudeTexts...)
	cc := append([][]string(nil), codexCmds...)
	mu.Unlock()
	if len(ct) != 1 || !strings.Contains(ct[0], "hello claude relay") {
		t.Fatalf("unexpected claude relay deliveries: %+v", ct)
	}
	if len(cc) != 1 || cc[0][1] != "queue" || cc[0][3] != codexUUID || !strings.Contains(cc[0][5], "hello codex relay") {
		t.Fatalf("unexpected codex relay deliveries: %+v", cc)
	}
	if deadSess, _ := f.Agents.GetSession("cx-dead-1"); deadSess.State != "exited" {
		t.Fatalf("expected dead codex PID to be deregistered, got %+v", deadSess)
	}
	if clSess, _ := f.Agents.GetSession("cl-relay-1"); clSess.State != "idle" || clSess.AgeSeconds >= 30 {
		t.Fatalf("expected alive claude session refreshed as idle, got state=%s age=%d", clSess.State, clSess.AgeSeconds)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunRelay exited with err: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunRelay did not stop on context cancel")
	}
}

func TestLivenessSweepRefreshesTranscriptTail(t *testing.T) {
	f, srv, tmp := setupWaiterServer(t)
	t.Setenv("HOME", tmp)

	uuid := "ca8a2ad4-7afe-431e-a1f5-231ac8134547"
	cwd := "/Users/datdo/workspace/demo"
	projDir := filepath.Join(tmp, ".claude", "projects", "-Users-datdo-workspace-demo")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(projDir, uuid+".jsonl")
	if err := os.WriteFile(transcriptPath, []byte(strings.Join([]string{
		`{"type":"ai-title","aiTitle":"Demo Hygiene Report"}`,
		`{"type":"user","message":{"role":"user","content":"run hygiene check"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"git status"}}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Final hygiene report: everything passed."}]}}`,
	}, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pid := 5501
	if _, err := f.Agents.Heartbeat(agents.HeartbeatParams{
		Device:  "mini",
		Harness: "claude",
		RawID:   uuid,
		PID:     &pid,
		CWD:     cwd,
		State:   "idle",
		Tail: []agents.TailItem{
			{Role: "user", Text: "run hygiene check"},
			{Role: "tool", Text: "Bash(command=git status)"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", filepath.Join(f.Root, "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	oldTS := time.Now().UTC().Add(-90 * time.Second).Format("2006-01-02T15:04:05+00:00")
	if _, err := db.Exec("UPDATE agent_sessions SET updated_at = ? WHERE session_id = ?", oldTS, "mini:claude:ca8a2ad4-7af"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()

	runLivenessSweep(context.Background(), srv.URL, "mini", map[string]bool{"claude": true}, filepath.Join(tmp, "cache"), func(int) bool {
		return true
	})

	detail, err := f.Agents.GetSession("mini:claude:ca8a2ad4-7af")
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Tail) != 3 || detail.Tail[2].Role != "assistant" || detail.Tail[2].Text != "Final hygiene report: everything passed." {
		t.Fatalf("expected liveness sweep to refresh transcript tail, got %+v", detail.Tail)
	}
	if detail.Title != "Demo Hygiene Report" {
		t.Fatalf("expected title refreshed from transcript, got %q", detail.Title)
	}
}

func TestSessionPushLoopStopRestartCancellationAndAckBackoff(t *testing.T) {
	// 1. Stop cancels an in-flight /v1/agents/wait immediately, and Start->Stop->Start leaves listener running.
	waitEntered := make(chan struct{}, 4)
	var ackStatus atomic.Int32
	ackStatus.Store(http.StatusOK)
	var deliverCount atomic.Int32
	var waitCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		switch r.URL.Path {
		case "/v1/agents/wait":
			n := waitCalls.Add(1)
			if n == 1 {
				select {
				case waitEntered <- struct{}{}:
				default:
				}
				<-r.Context().Done()
				return
			}
			_ = json.NewEncoder(w).Encode(agents.WaitResult{
				SessionID: "mini:codex:01a1ece5-f00",
				FullRawID: "01a1ece5-f00c-7860-a0dc-6c1897e73f30",
				Messages: []agents.Message{
					{ID: 42, ThreadID: "t1", FromSession: "mini:claude:peer", Body: "ping"},
				},
			})
		case "/v1/agents/ack":
			code := int(ackStatus.Load())
			if code != http.StatusOK {
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"error":"ack failed"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	t.Cleanup(srv.Close)

	listener := NewCodexQueueListener(CodexListenerOpts{
		APIURL:      srv.URL,
		PPID:        9999,
		CWD:         "/tmp",
		CodexBin:    "/usr/local/bin/codex",
		PollTimeout: 30.0,
		RunQueueFn: func([]string) (int, string) {
			deliverCount.Add(1)
			return 0, ""
		},
	})
	t.Cleanup(func() { listener.Stop() })

	listener.Start()
	select {
	case <-waitEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first /v1/agents/wait call")
	}

	stopDone := make(chan ListenerStatus, 1)
	go func() { stopDone <- listener.Stop() }()
	select {
	case st := <-stopDone:
		if st.Running || st.Enabled {
			t.Fatalf("expected stopped status after Stop, got %+v", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not cancel in-flight wait promptly")
	}

	// Failed ack must report LastError and back off instead of incrementing DeliveredCount or tight-looping.
	ackStatus.Store(http.StatusInternalServerError)
	st := listener.Start()
	if !st.Running || !st.Enabled {
		t.Fatalf("expected running after Stop->Start, got %+v", st)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if listener.Status().LastError != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	stErr := listener.Status()
	if stErr.LastError == nil || stErr.DeliveredCount != 0 {
		t.Fatalf("expected LastError set and DeliveredCount=0 when ack fails, got %+v", stErr)
	}
	if deliverCount.Load() != 1 {
		t.Fatalf("expected backoff after failed ack (1 delivery attempt), got %d", deliverCount.Load())
	}
}
