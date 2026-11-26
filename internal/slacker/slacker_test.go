package slacker

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/registry"
	"github.com/bdatdo0601/fleet/internal/session"
)

type mockSlackServer struct {
	srv         *httptest.Server
	mu          sync.Mutex
	wsConns     []net.Conn
	acks        []string
	ackCh       chan string
	historyErr  bool
	messages    map[string]SlackMessage // key channel:ts
	threadReply map[string][]SlackMessage
}

func newMockSlackServer(t *testing.T) *mockSlackServer {
	t.Helper()
	ms := &mockSlackServer{
		ackCh:       make(chan string, 32),
		messages:    map[string]SlackMessage{},
		threadReply: map[string][]SlackMessage{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth.test", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"user_id":"UADMIN","user":"datdo","team_id":"T1","team":"Dat"}`)
	})
	mux.HandleFunc("/apps.connections.open", func(w http.ResponseWriter, _ *http.Request) {
		wsURL := "ws" + strings.TrimPrefix(ms.srv.URL, "http") + "/ws"
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": wsURL})
	})
	mux.HandleFunc("/conversations.history", func(w http.ResponseWriter, r *http.Request) {
		ms.mu.Lock()
		fail := ms.historyErr
		ch := r.URL.Query().Get("channel")
		ts := r.URL.Query().Get("latest")
		m, ok := ms.messages[ch+":"+ts]
		ms.mu.Unlock()
		if fail {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "ratelimited"})
			return
		}
		msgs := []SlackMessage{}
		if ok {
			msgs = append(msgs, m)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": msgs})
	})
	mux.HandleFunc("/conversations.replies", func(w http.ResponseWriter, r *http.Request) {
		ch := r.URL.Query().Get("channel")
		ts := r.URL.Query().Get("ts")
		ms.mu.Lock()
		list := ms.threadReply[ch+":"+ts]
		ms.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": list})
	})
	mux.HandleFunc("/chat.getPermalink", func(w http.ResponseWriter, r *http.Request) {
		ch := r.URL.Query().Get("channel")
		ts := r.URL.Query().Get("message_ts")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "permalink": "https://slack.test/archives/" + ch + "/p" + strings.ReplaceAll(ts, ".", "")})
	})
	mux.HandleFunc("/conversations.info", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": map[string]string{"name": "dev-channel"}})
	})
	mux.HandleFunc("/users.info", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": map[string]any{"name": "alice", "profile": map[string]string{"display_name": "Alice"}}})
	})
	mux.HandleFunc("/ws", ms.handleWS)

	ms.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		ms.mu.Lock()
		for _, c := range ms.wsConns {
			_ = c.Close()
		}
		ms.mu.Unlock()
		ms.srv.Close()
	})
	return ms
}

func (ms *mockSlackServer) handleWS(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Sec-WebSocket-Key")
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", 500)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	accept := wsAcceptHash(key)
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
	_ = writeServerTextFrame(conn, `{"type":"hello"}`)

	ms.mu.Lock()
	ms.wsConns = append(ms.wsConns, conn)
	ms.mu.Unlock()

	go func() {
		br := brw.Reader
		for {
			msg, err := readClientFrame(br)
			if err != nil {
				return
			}
			var ack struct {
				EnvelopeID string `json:"envelope_id"`
			}
			if json.Unmarshal(msg, &ack) == nil && ack.EnvelopeID != "" {
				ms.mu.Lock()
				ms.acks = append(ms.acks, ack.EnvelopeID)
				ms.mu.Unlock()
				ms.ackCh <- ack.EnvelopeID
			}
		}
	}()
}

func (ms *mockSlackServer) sendEnvelope(t *testing.T, env any) {
	t.Helper()
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ms.mu.Lock()
		var c net.Conn
		if len(ms.wsConns) > 0 {
			c = ms.wsConns[len(ms.wsConns)-1]
		}
		ms.mu.Unlock()
		if c != nil {
			if err := writeServerTextFrame(c, string(b)); err == nil {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("timed out waiting for websocket client connection")
}

func writeServerTextFrame(conn net.Conn, text string) error {
	payload := []byte(text)
	var hdr [10]byte
	hdr[0] = 0x81
	n := len(payload)
	hdrLen := 2
	switch {
	case n < 126:
		hdr[1] = byte(n)
	case n <= 65535:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		hdrLen = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		hdrLen = 10
	}
	if _, err := conn.Write(hdr[:hdrLen]); err != nil {
		return err
	}
	_, err := conn.Write(payload)
	return err
}

func readClientFrame(br *bufio.Reader) ([]byte, error) {
	var b2 [2]byte
	if _, err := io.ReadFull(br, b2[:]); err != nil {
		return nil, err
	}
	masked := (b2[1] & 0x80) != 0
	length := uint64(b2[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(br, mask[:]); err != nil {
			return nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(br, payload); err != nil {
		return nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return payload, nil
}

func reactionEnvelope(envID, eventID, userID, reaction, chID, ts string) map[string]any {
	return map[string]any{
		"envelope_id": envID,
		"type":        "events_api",
		"payload": map[string]any{
			"event_id": eventID,
			"event": map[string]any{
				"type":     "reaction_added",
				"user":     userID,
				"reaction": reaction,
				"item": map[string]any{
					"type":    "message",
					"channel": chID,
					"ts":      ts,
				},
			},
		},
	}
}

func TestListenerPersistBeforeAckThreadRepliesAuthAndDedupe(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "slacker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ms := newMockSlackServer(t)
	ms.messages["C1:1700.1"] = SlackMessage{Type: "message", User: "U1", Text: "Top-level task\nwith two lines", TS: "1700.1"}
	ms.threadReply["C1:1700.2"] = []SlackMessage{
		{Type: "message", User: "U1", Text: "Parent context message", TS: "1700.0", ThreadTS: "1700.0"},
		{Type: "message", User: "U2", Text: "Specific reply task", TS: "1700.2", ThreadTS: "1700.0"},
	}

	cfg := Config{
		Enabled:          true,
		AppToken:         "xapp-test-1234567890",
		UserToken:        "xoxp-test-1234567890",
		AuthorizedUserID: "UADMIN",
		Channels: map[string]ChannelMapping{
			"C1": {ChannelID: "C1", ChannelName: "fleet", Device: "laptop", RepoPath: "/tmp/repo"},
		},
		Emojis: DefaultEmojis(),
	}
	cfgPath := filepath.Join(tmp, "slacker.json")
	if err := SaveConfig(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- RunListener(ctx, ListenerOpts{
			StateDir:    tmp,
			ConfigPath:  cfgPath,
			Store:       store,
			Slack:       &SlackClient{BaseURL: ms.srv.URL},
			NotifyURL:   "http://127.0.0.1:1/unused",
			Stderr:      io.Discard,
			BackoffBase: 50 * time.Millisecond,
		})
	}()

	// 1. Unauthorized user is ignored (ACKed as non-actionable, 0 jobs created).
	ms.sendEnvelope(t, reactionEnvelope("env-unauth", "EvUnauth", "UOTHER", "claude", "C1", "1700.1"))
	select {
	case id := <-ms.ackCh:
		if id != "env-unauth" {
			t.Fatalf("unexpected ack %s", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for ack on ignored user")
	}
	if jobs, _ := store.ListJobs("", "", 10); len(jobs) != 0 {
		t.Fatalf("unauthorized user created job: %+v", jobs)
	}

	// 2. Transient Slack API error must NOT ACK!
	ms.mu.Lock()
	ms.historyErr = true
	ms.mu.Unlock()
	ms.sendEnvelope(t, reactionEnvelope("env-fail", "EvFail", "UADMIN", "claude", "C1", "1700.1"))
	select {
	case id := <-ms.ackCh:
		t.Fatalf("must NOT ACK when FetchMessage fails, got ack %s", id)
	case <-time.After(250 * time.Millisecond):
	}
	ms.mu.Lock()
	ms.historyErr = false
	ms.mu.Unlock()

	// 3. Valid top-level reaction -> persisted BEFORE ACK.
	ms.sendEnvelope(t, reactionEnvelope("env-1", "Ev1", "UADMIN", "claude", "C1", "1700.1"))
	select {
	case id := <-ms.ackCh:
		if id != "env-1" {
			t.Fatalf("expected ack env-1, got %s", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for env-1 ack")
	}
	jobs, err := store.ListJobs("", "", 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("expected 1 persisted job, got %+v (err=%v)", jobs, err)
	}
	if jobs[0].SessionName != "slack-c1-1700-1" || !strings.Contains(jobs[0].Prompt, "Top-level task\nwith two lines") {
		t.Fatalf("unexpected job fields: %+v", jobs[0])
	}

	// 4. Duplicate event_id delivery -> still only 1 job.
	ms.sendEnvelope(t, reactionEnvelope("env-1-dup", "Ev1", "UADMIN", "claude", "C1", "1700.1"))
	select {
	case <-ms.ackCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for dup ack")
	}
	if jobs, _ := store.ListJobs("", "", 10); len(jobs) != 1 {
		t.Fatalf("duplicate event created extra job: %+v", jobs)
	}

	// 5. Thread reply reaction -> conversations.replies resolves exact reply + thread parent.
	ms.sendEnvelope(t, reactionEnvelope("env-2", "Ev2", "UADMIN", "agy", "C1", "1700.2"))
	select {
	case id := <-ms.ackCh:
		if id != "env-2" {
			t.Fatalf("expected env-2 ack, got %s", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for env-2 ack")
	}
	jobs, _ = store.ListJobs("", "", 10)
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %+v", jobs)
	}
	var threadJob Job
	for _, j := range jobs {
		if j.EventID == "Ev2" {
			threadJob = j
		}
	}
	if threadJob.Harness != "agy" || threadJob.ThreadTS != "1700.0" ||
		!strings.Contains(threadJob.Prompt, "Parent context message") ||
		!strings.Contains(threadJob.Prompt, "Specific reply task") {
		t.Fatalf("unexpected thread job prompt/metadata: %+v", threadJob)
	}

	// 6. Restart recovery: persist-before-ack & deduplication survive DB reopen.
	cancel()
	<-doneCh
	_ = store.Close()
	store2, err := Open(filepath.Join(tmp, "slacker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store2.Close() })
	inserted, _, err := store2.EnqueueJob(Job{EventID: "Ev1", ChannelID: "C1", MessageTS: "1700.1", TargetDevice: "laptop", RepoPath: "/tmp/repo", Harness: "claude", SessionName: "slack-c1-1700-1", Prompt: "dup"})
	if err != nil || inserted {
		t.Fatalf("expected Ev1 deduplicated after reopen: inserted=%v err=%v", inserted, err)
	}
}

func TestWorkerCrossDeviceRoutingSleepingMacRecoveryAndCleanup(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "slacker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Minimal Nebula-authenticated mock lighthouse API for worker/poll and worker/receipt.
	mu := sync.Mutex{}
	activeDev := "office"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		dev := activeDev
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/slacker/worker/poll":
			var req struct {
				Timeout float64 `json:"timeout"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			j, err := store.WaitForJob(r.Context(), dev, time.Duration(req.Timeout*float64(time.Second)), 90*time.Second)
			if err != nil && !errors.Is(err, context.Canceled) {
				http.Error(w, err.Error(), 500)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"job": j, "device": dev})
		case "/v1/slacker/worker/receipt":
			var rec Receipt
			_ = json.NewDecoder(r.Body).Decode(&rec)
			j, err := store.RecordReceipt(dev, rec)
			if err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"job": j})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	// Enqueue one job for sleeping "laptop" and one for awake "office".
	_, laptopJob, err := store.EnqueueJob(Job{
		EventID: "EvLap", ChannelID: "C1", MessageTS: "100.1", Reaction: "claude", UserID: "U1",
		TargetDevice: "laptop", RepoPath: tmp, Harness: "claude", SessionName: "slack-c1-100-1", Prompt: "Laptop task",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, officeJob, err := store.EnqueueJob(Job{
		EventID: "EvOff", ChannelID: "C2", MessageTS: "200.1", Reaction: "agy", UserID: "U1",
		TargetDevice: "office", RepoPath: tmp, Harness: "agy", SessionName: "slack-c2-200-1", Prompt: "Office task",
	})
	if err != nil {
		t.Fatal(err)
	}

	officeDir := filepath.Join(tmp, "office-state")
	var launched []string
	err = RunWorker(context.Background(), WorkerOpts{
		APIURL:        srv.URL,
		LocalStateDir: officeDir,
		Once:          true,
		SessionExists: func(string, string) (bool, string) { return false, "" },
		LaunchFn: func(job Job, _ bool, _, _ io.Writer) (string, error) {
			launched = append(launched, job.TargetDevice+":"+job.SessionName)
			return "tmux", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(launched, []string{"office:slack-c2-200-1"}) {
		t.Fatalf("expected office to pull only office job, got %v", launched)
	}
	// Laptop is sleeping -> laptopJob must remain queued!
	gotLap, _ := store.GetJob(laptopJob.ID)
	gotOff, _ := store.GetJob(officeJob.ID)
	if gotLap.Status != "queued" || gotOff.Status != "launched" {
		t.Fatalf("unexpected statuses: laptop=%s office=%s", gotLap.Status, gotOff.Status)
	}

	// Simulate interrupted launch on laptop: session exists deterministically before receipt was posted.
	mu.Lock()
	activeDev = "laptop"
	mu.Unlock()
	laptopDir := filepath.Join(tmp, "laptop-state")
	reLaunchCount := 0
	err = RunWorker(context.Background(), WorkerOpts{
		APIURL:        srv.URL,
		LocalStateDir: laptopDir,
		Once:          true,
		SessionExists: func(harness, name string) (bool, string) {
			return harness == "claude" && name == "slack-c1-100-1", "tmux"
		},
		LaunchFn: func(Job, bool, io.Writer, io.Writer) (string, error) {
			reLaunchCount++
			return "tmux", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reLaunchCount != 0 {
		t.Fatalf("expected reconciliation without re-launching existing session, got %d", reLaunchCount)
	}
	gotLap, _ = store.GetJob(laptopJob.ID)
	if gotLap.Status != "launched" || gotLap.Backend != "tmux" {
		t.Fatalf("expected reconciled laptop job launched, got %+v", gotLap)
	}

	// Simulate crash after saving local receipt (Acked=false) -> replay succeeds on worker restart.
	_, replayJob, _ := store.EnqueueJob(Job{
		EventID: "EvReplay", ChannelID: "C3", MessageTS: "300.1", Reaction: "codex", UserID: "U1",
		TargetDevice: "laptop", RepoPath: tmp, Harness: "codex", SessionName: "slack-c3-300-1", Prompt: "Replay task",
	})
	rPath := receiptPath(laptopDir, replayJob.ID)
	if err := saveLocalReceipt(rPath, LocalReceipt{
		JobID: replayJob.ID, ReceiptID: "rcpt-local", SessionName: replayJob.SessionName,
		Harness: "codex", Backend: "rex", Status: "launched", Acked: false, CreatedAt: registry.NowISO(),
	}); err != nil {
		t.Fatal(err)
	}
	replayPendingReceipts(context.Background(), srv.URL, laptopDir, io.Discard)
	gotReplay, _ := store.GetJob(replayJob.ID)
	if gotReplay.Status != "launched" || gotReplay.ReceiptID != "rcpt-local" {
		t.Fatalf("expected replayed receipt, got %+v", gotReplay)
	}

	// Cleanup preserves queued jobs, only prunes terminal jobs.
	_, queuedStay, _ := store.EnqueueJob(Job{
		EventID: "EvQueued", ChannelID: "C4", MessageTS: "400.1", Reaction: "claude", UserID: "U1",
		TargetDevice: "laptop", RepoPath: tmp, Harness: "claude", SessionName: "slack-c4-400-1", Prompt: "Still queued",
	})
	pruned, _, err := store.Cleanup(0, true)
	if err != nil || pruned != 3 {
		t.Fatalf("expected 3 pruned terminal jobs, got %d (err=%v)", pruned, err)
	}
	if still, _ := store.GetJob(queuedStay.ID); still == nil || still.Status != "queued" {
		t.Fatalf("cleanup removed queued job: %+v", still)
	}
}

func TestWorkerRealGoLauncherEndToEndBothHarnesses(t *testing.T) {
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(base, "bin")
	_ = os.MkdirAll(binDir, 0o755)
	repoDir := filepath.Join(base, "myrepo")
	_ = os.MkdirAll(repoDir, 0o755)
	_ = os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("keep me"), 0o644)

	// Build real fleet binary so claude's SessionStart hook (`fleet session claude __hook`) works end-to-end.
	fleetBin := filepath.Join(binDir, "fleet")
	if out, err := exec.Command("go", "build", "-o", fleetBin, "../../cmd/fleet").CombinedOutput(); err != nil {
		t.Fatalf("go build fleet: %v\n%s", err, string(out))
	}

	socket := fmt.Sprintf("fleet-slk-e2e-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		_ = exec.Command(realTmux, "-L", socket, "kill-server").Run()
	})
	var filteredPath []string
	for p := range strings.SplitSeq(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if p == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(p, "rex")); err == nil {
			continue
		}
		filteredPath = append(filteredPath, p)
	}
	t.Setenv("HOME", base)
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+strings.Join(filteredPath, string(os.PathListSeparator)))
	t.Setenv("FLEET_SESSION_SELF_BIN", fleetBin)
	t.Setenv("AGY_HOOKS_FILE", filepath.Join(base, ".gemini", "config", "hooks.json"))

	tmuxWrapper := fmt.Sprintf("#!/bin/bash\nexec %q -L %q -f /dev/null \"$@\"\n", realTmux, socket)
	_ = os.WriteFile(filepath.Join(binDir, "tmux"), []byte(tmuxWrapper), 0o755)

	writeFake := func(name, script string) {
		_ = os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755)
	}
	writeFake("claude", fmt.Sprintf(`#!/usr/bin/env python3
import json, os, signal, subprocess, sys
from pathlib import Path
args = sys.argv[1:]
name = args[args.index("--remote-control") + 1]
Path(%q, name + ".json").write_text(json.dumps({"cwd": os.getcwd(), "args": args}))
subprocess.run(["tmux", "wait-for", "-S", "ready-" + name], check=True)
signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
while True:
    signal.pause()
`, base))
	writeFake("agy", fmt.Sprintf(`#!/usr/bin/env python3
import json, os, signal, subprocess, sys
from pathlib import Path
name = os.environ["FLEET_SESSION_NAME"]
Path(%q, name + ".json").write_text(json.dumps({"cwd": os.getcwd(), "args": sys.argv[1:]}))
subprocess.run(["tmux", "wait-for", "-S", "ready-" + name], check=True)
signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
while True:
    signal.pause()
`, base))

	store, err := Open(filepath.Join(base, "slacker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/slacker/worker/poll":
			j, _ := store.ClaimNextJob("laptop", 90*time.Second)
			_ = json.NewEncoder(w).Encode(map[string]any{"job": j, "device": "laptop"})
		case "/v1/slacker/worker/receipt":
			var rec Receipt
			_ = json.NewDecoder(r.Body).Decode(&rec)
			j, _ := store.RecordReceipt("laptop", rec)
			_ = json.NewEncoder(w).Encode(map[string]any{"job": j})
		}
	}))
	t.Cleanup(srv.Close)

	_, _, _ = store.EnqueueJob(Job{
		EventID: "EvC", ChannelID: "C1", MessageTS: "10.1", Reaction: "claude", UserID: "U1",
		TargetDevice: "laptop", RepoPath: repoDir, Harness: "claude",
		SessionName: DeterministicSessionName("C1", "10.1"), Prompt: "Claude multiline\n'task'",
	})
	_, _, _ = store.EnqueueJob(Job{
		EventID: "EvA", ChannelID: "C1", MessageTS: "10.2", Reaction: "agy", UserID: "U1",
		TargetDevice: "laptop", RepoPath: repoDir, Policy: "auto", Harness: "agy", UseTmux: true,
		SessionName: DeterministicSessionName("C1", "10.2"), Prompt: "Agy multiline\n\"task\"",
	})

	workerState := filepath.Join(base, "slacker-local")
	for range 2 {
		if err := RunWorker(context.Background(), WorkerOpts{APIURL: srv.URL, LocalStateDir: workerState, Once: true}); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []string{"slack-c1-10-1", "slack-c1-10-2"} {
		if out, err := exec.Command(filepath.Join(binDir, "tmux"), "wait-for", "ready-"+s).CombinedOutput(); err != nil {
			t.Fatalf("wait %s: %v (%s)", s, err, string(out))
		}
	}

	var claudeRec, agyRec struct {
		CWD  string   `json:"cwd"`
		Args []string `json:"args"`
	}
	b1, _ := os.ReadFile(filepath.Join(base, "slack-c1-10-1.json"))
	_ = json.Unmarshal(b1, &claudeRec)
	b2, _ := os.ReadFile(filepath.Join(base, "slack-c1-10-2.json"))
	_ = json.Unmarshal(b2, &agyRec)

	if !slices.Contains(claudeRec.Args, "--dangerously-skip-permissions") || slices.Contains(claudeRec.Args, "manual") {
		t.Fatalf("expected claude default bypass mode (--dangerously-skip-permissions): %v", claudeRec.Args)
	}
	if !slices.Contains(agyRec.Args, "--mode") || !slices.Contains(agyRec.Args, "auto") || slices.Contains(agyRec.Args, "--sandbox") || slices.Contains(agyRec.Args, "--dangerously-skip-permissions") {
		t.Fatalf("expected agy auto mode: %v", agyRec.Args)
	}
	if exists, _ := session.SessionExists("claude", "slack-c1-10-1"); !exists {
		t.Fatal("expected claude session alive")
	}
	if exists, _ := session.SessionExists("agy", "slack-c1-10-2"); !exists {
		t.Fatal("expected agy session alive")
	}
	if _, err := os.Stat(filepath.Join(repoDir, "README.md")); err != nil {
		t.Fatalf("expected repo preserved: %v", err)
	}
	var unusedClient *client.APIError
	_ = unusedClient
}

func TestWorkerAgyDefaultAppBackend(t *testing.T) {
	base := t.TempDir()
	repoDir := filepath.Join(base, "myrepo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	appDir := filepath.Join(base, "agy-app")
	daemonDir := filepath.Join(appDir, "daemon")
	if err := os.MkdirAll(daemonDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var gotCreate map[string]any
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/product.v1.ConversationService/CreateConversation":
			_ = json.NewDecoder(r.Body).Decode(&gotCreate)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"conversation": map[string]any{"id": "22222222-2222-4222-8222-000000000001"},
			})
		case "/product.v1.ConversationService/GetConversation":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "22222222-2222-4222-8222-000000000001"})
		}
	}))
	t.Cleanup(hubSrv.Close)

	port := strings.TrimPrefix(hubSrv.URL, "http://127.0.0.1:")
	if idx := strings.LastIndex(hubSrv.URL, ":"); idx >= 0 {
		port = hubSrv.URL[idx+1:]
	}
	_ = os.WriteFile(
		filepath.Join(daemonDir, fmt.Sprintf("hub_%s.json", port)),
		[]byte(fmt.Sprintf(`{"pid":%d,"httpPort":%s,"csrfToken":"slacker-csrf"}`, os.Getpid(), port)),
		0o600,
	)

	t.Setenv("HOME", base)
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	t.Setenv("FLEET_AGY_APP_DATA_DIR", appDir)
	t.Setenv("FLEET_SESSION_NO_OPEN", "1")

	store, err := Open(filepath.Join(base, "slacker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/slacker/worker/poll":
			j, _ := store.ClaimNextJob("laptop", 90*time.Second)
			_ = json.NewEncoder(w).Encode(map[string]any{"job": j, "device": "laptop"})
		case "/v1/slacker/worker/receipt":
			var rec Receipt
			_ = json.NewDecoder(r.Body).Decode(&rec)
			j, _ := store.RecordReceipt("laptop", rec)
			_ = json.NewEncoder(w).Encode(map[string]any{"job": j})
		}
	}))
	t.Cleanup(srv.Close)

	_, _, _ = store.EnqueueJob(Job{
		EventID: "EvApp", ChannelID: "C1", MessageTS: "12.3", Reaction: "agy", UserID: "U1",
		TargetDevice: "laptop", RepoPath: repoDir, Harness: "agy",
		SessionName: DeterministicSessionName("C1", "12.3"), Prompt: "Check mobile sync",
	})

	workerState := filepath.Join(base, "slacker-local")
	if err := RunWorker(context.Background(), WorkerOpts{APIURL: srv.URL, LocalStateDir: workerState, Once: true}); err != nil {
		t.Fatal(err)
	}

	jobs, err := store.ListJobs("", "", 10)
	if err != nil || len(jobs) != 1 || jobs[0].Status != "launched" || jobs[0].Backend != "app" {
		t.Fatalf("expected launched job with backend=app, got %+v (err=%v)", jobs, err)
	}
	if gotCreate == nil || gotCreate["title"] != "slack-c1-12-3" {
		t.Fatalf("unexpected CreateConversation payload: %+v", gotCreate)
	}
}

func TestStoreMigratesLegacyJobsMissingAccountColumn(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "legacy.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE slacker_jobs (
			id TEXT PRIMARY KEY,
			event_id TEXT NOT NULL UNIQUE,
			channel_id TEXT NOT NULL,
			channel_name TEXT NOT NULL DEFAULT '',
			message_ts TEXT NOT NULL,
			thread_ts TEXT NOT NULL DEFAULT '',
			reaction TEXT NOT NULL,
			user_id TEXT NOT NULL,
			target_device TEXT NOT NULL,
			repo_path TEXT NOT NULL,
			use_tmux INTEGER NOT NULL DEFAULT 0,
			harness TEXT NOT NULL,
			session_name TEXT NOT NULL,
			prompt TEXT NOT NULL,
			permalink TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			attempt_count INTEGER NOT NULL DEFAULT 0,
			claimed_by TEXT NOT NULL DEFAULT '',
			claimed_at TEXT NOT NULL DEFAULT '',
			lease_expires_at TEXT NOT NULL DEFAULT '',
			receipt_id TEXT NOT NULL DEFAULT '',
			backend TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		INSERT INTO slacker_jobs (id, event_id, channel_id, message_ts, reaction, user_id, target_device, repo_path, harness, session_name, prompt, status, created_at, updated_at)
		VALUES ('job-legacy', 'EvOld', 'C1', '1.0', 'claude', 'U1', 'laptop', '/tmp', 'claude', 'slack-c1-1-0', 'hi', 'pending', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
	`)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open legacy DB failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	inserted, j, err := store.EnqueueJob(Job{
		Account: "work", EventID: "EvNew", ChannelID: "CW", MessageTS: "2.0", Reaction: "codex",
		UserID: "U2", TargetDevice: "mini", RepoPath: "/tmp", Harness: "codex",
		SessionName: DeterministicSessionNameForAccount("work", "CW", "2.0"), Prompt: "new",
	})
	if err != nil || !inserted || j.Account != "work" {
		t.Fatalf("expected work job inserted after migration: inserted=%v job=%+v err=%v", inserted, j, err)
	}
	jobs, err := store.ListJobs("", "", 10)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("expected 2 jobs after migration, got %+v (err=%v)", jobs, err)
	}
	var foundLegacy bool
	for _, item := range jobs {
		if item.ID == "job-legacy" {
			foundLegacy = true
			if item.Account != DefaultAccountID {
				t.Fatalf("expected legacy job defaulted to %q, got %q", DefaultAccountID, item.Account)
			}
		}
	}
	if !foundLegacy {
		t.Fatal("legacy job missing after migration")
	}
}

func TestMultiAccountListenerAndReadWriteAPI(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "slacker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var (
		mu       sync.Mutex
		wsConns  = map[string]net.Conn{}
		ackCh    = make(chan string, 16)
		postedCh = make(chan string, 8)
		srv      *httptest.Server
	)
	upgradeWS := func(acct string, w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, hErr := hj.Hijack()
		if hErr != nil {
			return
		}
		key := r.Header.Get("Sec-WebSocket-Key")
		resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
			wsAcceptHash(key) + "\r\n\r\n"
		_, _ = conn.Write([]byte(resp))
		mu.Lock()
		wsConns[acct] = conn
		mu.Unlock()
		_ = writeServerTextFrame(conn, `{"type":"hello"}`)

		go func() {
			defer conn.Close()
			br := bufio.NewReader(conn)
			for {
				payload, rErr := readClientFrame(br)
				if rErr != nil {
					return
				}
				var ack struct {
					EnvelopeID string `json:"envelope_id"`
				}
				if json.Unmarshal(payload, &ack) == nil && ack.EnvelopeID != "" {
					ackCh <- acct + ":" + ack.EnvelopeID
				}
			}
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/apps.connections.open", func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		acct := "personal"
		if strings.Contains(tok, "work") {
			acct = "work"
		}
		wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/" + acct
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": wsURL})
	})
	mux.HandleFunc("/ws/personal", func(w http.ResponseWriter, r *http.Request) { upgradeWS("personal", w, r) })
	mux.HandleFunc("/ws/work", func(w http.ResponseWriter, r *http.Request) { upgradeWS("work", w, r) })
	mux.HandleFunc("/conversations.history", func(w http.ResponseWriter, r *http.Request) {
		ch := r.URL.Query().Get("channel")
		ts := r.URL.Query().Get("latest")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":       true,
			"messages": []SlackMessage{{TS: cmpOr(ts, "10.1"), User: "U1", Text: "Message in " + ch}},
		})
	})
	mux.HandleFunc("/conversations.replies", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"messages": []SlackMessage{
				{TS: r.URL.Query().Get("ts"), User: "U1", Text: "Parent thread"},
				{TS: r.URL.Query().Get("ts") + ".1", ThreadTS: r.URL.Query().Get("ts"), User: "U2", Text: "Reply detail"},
			},
		})
	})
	mux.HandleFunc("/search.messages", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"messages": map[string]any{
				"matches": []map[string]any{
					{"ts": "99.1", "user": "U9", "username": "alice", "text": "Matched " + r.URL.Query().Get("query"), "permalink": "https://slack.test/99", "channel": map[string]string{"id": "CW", "name": "work-eng"}},
				},
			},
		})
	})
	mux.HandleFunc("/users.conversations", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"channels": []map[string]any{
				{"id": "CW", "name": "work-eng", "is_channel": true, "is_member": true},
			},
		})
	})
	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		postedCh <- fmt.Sprintf("%s|%v|%v|%v", r.Header.Get("Authorization"), body["channel"], body["thread_ts"], body["text"])
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": body["channel"], "ts": "1234.56"})
	})
	mux.HandleFunc("/chat.getPermalink", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "permalink": "https://slack.test/p"})
	})
	mux.HandleFunc("/conversations.info", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": map[string]string{"name": r.URL.Query().Get("channel")}})
	})
	mux.HandleFunc("/users.info", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": map[string]any{"name": "user"}})
	})

	srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		mu.Lock()
		for _, c := range wsConns {
			_ = c.Close()
		}
		mu.Unlock()
		srv.Close()
	})

	cfgPath := filepath.Join(tmp, "slacker.json")
	cfg := Config{
		Accounts: map[string]AccountConfig{
			"personal": {ID: "personal", Enabled: true, AppToken: "xapp-pers", UserToken: "xoxp-pers", AuthorizedUserID: "UPERS"},
			"work":     {ID: "work", Enabled: true, AppToken: "xapp-work", UserToken: "xoxp-work", BotToken: "xoxb-work", AuthorizedUserID: "UWORK"},
		},
		Channels: map[string]ChannelMapping{
			ChannelKey("personal", "CPERS"): {Account: "personal", ChannelID: "CPERS", Device: "laptop", RepoPath: "/tmp/pers"},
			ChannelKey("work", "CWORK"):     {Account: "work", ChannelID: "CWORK", Device: "office", RepoPath: "/tmp/work"},
		},
		Emojis: DefaultEmojis(),
	}
	if err := SaveConfig(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	sc := &SlackClient{BaseURL: srv.URL}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunListener(ctx, ListenerOpts{StateDir: tmp, ConfigPath: cfgPath, Store: store, Slack: sc, BackoffBase: 50 * time.Millisecond})
	}()
	var stopOnce sync.Once
	stopListener := func() {
		stopOnce.Do(func() {
			cancel()
			<-done
		})
	}
	t.Cleanup(stopListener)

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ready := wsConns["personal"] != nil && wsConns["work"] != nil
		mu.Unlock()
		if ready {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	persConn, workConn := wsConns["personal"], wsConns["work"]
	mu.Unlock()
	if persConn == nil || workConn == nil {
		t.Fatalf("expected both personal and work WebSocket connections, got personal=%v work=%v", persConn != nil, workConn != nil)
	}

	// Wrong account user (UWORK on personal) should be ACKed without enqueueing a job.
	envBad, _ := json.Marshal(reactionEnvelope("e-bad", "EvBadAcct", "UWORK", "claude", "CPERS", "10.0"))
	_ = writeServerTextFrame(persConn, string(envBad))
	envP, _ := json.Marshal(reactionEnvelope("e-p", "EvPers", "UPERS", "claude", "CPERS", "10.1"))
	_ = writeServerTextFrame(persConn, string(envP))
	envW, _ := json.Marshal(reactionEnvelope("e-w", "EvWork", "UWORK", "codex", "CWORK", "20.2"))
	_ = writeServerTextFrame(workConn, string(envW))

	var acks []string
	for range 3 {
		select {
		case a := <-ackCh:
			acks = append(acks, a)
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for acks, got %v", acks)
		}
	}

	jobs, err := store.ListJobs("", "", 10)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("expected 2 jobs across both accounts, got %+v (err=%v)", jobs, err)
	}
	byEvt := map[string]Job{}
	for _, j := range jobs {
		byEvt[j.EventID] = j
	}
	if byEvt["personal:EvPers"].Account != "personal" || byEvt["personal:EvPers"].SessionName != "slack-personal-cpers-10-1" {
		t.Fatalf("unexpected personal job: %+v", byEvt["personal:EvPers"])
	}
	if byEvt["work:EvWork"].Account != "work" || byEvt["work:EvWork"].SessionName != "slack-work-cwork-20-2" || byEvt["work:EvWork"].Harness != "codex" {
		t.Fatalf("unexpected work job: %+v", byEvt["work:EvWork"])
	}

	// Exercise ReadMessages (history, thread, search), ListUserChannels, and PostMessage.
	histRes, err := sc.ReadMessages(context.Background(), "xoxp-work", ReadMessagesOpts{ChannelID: "CWORK", Limit: 5})
	if err != nil || histRes.Mode != "history" || len(histRes.Messages) != 1 {
		t.Fatalf("ReadMessages history failed: %+v err=%v", histRes, err)
	}
	threadRes, err := sc.ReadMessages(context.Background(), "xoxp-work", ReadMessagesOpts{ChannelID: "CWORK", ThreadTS: "55.0"})
	if err != nil || threadRes.Mode != "thread" || len(threadRes.Messages) != 2 {
		t.Fatalf("ReadMessages thread failed: %+v err=%v", threadRes, err)
	}
	searchRes, err := sc.ReadMessages(context.Background(), "xoxp-work", ReadMessagesOpts{Query: "incident"})
	if err != nil || searchRes.Mode != "search" || len(searchRes.Messages) != 1 || searchRes.Messages[0].ChannelID != "CW" {
		t.Fatalf("ReadMessages search failed: %+v err=%v", searchRes, err)
	}
	chs, err := sc.ListUserChannels(context.Background(), "xoxp-work", 10)
	if err != nil || len(chs) != 1 || chs[0].ID != "CW" {
		t.Fatalf("ListUserChannels failed: %+v err=%v", chs, err)
	}
	posted, err := sc.PostMessage(context.Background(), "xoxb-work", "CWORK", "investigation update", "55.0")
	if err != nil || posted.TS != "1234.56" {
		t.Fatalf("PostMessage failed: %+v err=%v", posted, err)
	}
	if got := <-postedCh; !strings.Contains(got, "Bearer xoxb-work|CWORK|55.0|investigation update") {
		t.Fatalf("unexpected post payload: %s", got)
	}
	stopListener()
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func TestWorkspaceDirAndMultiDeviceConfigValidation(t *testing.T) {
	active := map[string]bool{"mini": true, "laptop": true, "office": true}

	// Legacy single-device JSON loads cleanly with normalized Devices slice.
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "slacker.json")
	legacyJSON := `{
  "accounts": {"default": {"id":"default","enabled":true,"app_token":"xapp-1","user_token":"xoxp-1","authorized_user_id":"U1"}},
  "channels": {"C1": {"channel_id":"C1","device":"mini","repo_path":"/tmp/r"}},
  "emojis": {"claude":"claude"}
}`
	if err := os.WriteFile(cfgPath, []byte(legacyJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.EffectiveChannelDevices(loaded.Channels["C1"]); !slices.Equal(got, []string{"mini"}) {
		t.Fatalf("expected legacy channel normalized to [mini], got %v", got)
	}
	if loaded.EffectiveDefaultStrategy() != StrategyLeastAgents {
		t.Fatalf("expected default strategy %q, got %q", StrategyLeastAgents, loaded.EffectiveDefaultStrategy())
	}

	// Relative subpath without workspace_dir must fail validation.
	_, err = ApplyUpdate(Config{}, ConfigUpdate{
		DefaultDevices: &[]string{"mini", "laptop"},
		Channels: map[string]ChannelMapping{
			"C2": {ChannelID: "C2", RepoPath: "fleet"},
		},
	}, active)
	if err == nil {
		t.Fatal("expected error when relative repo_path lacks workspace_dir")
	}

	// Configuring per-machine workspace_dir (stripping trailing slashes) allows relative & blank repo_path.
	defStrat := StrategyRoundRobin
	cfg, err := ApplyUpdate(Config{}, ConfigUpdate{
		Devices: map[string]DeviceConfig{
			"mini":   {WorkspaceDir: "~/workspace/"},
			"laptop": {WorkspaceDir: "/Users/dat/src/"},
			"office": {WorkspaceDir: ""}, // empty clears entry
		},
		DefaultDevices:  &[]string{"mini", "laptop"},
		DefaultStrategy: &defStrat,
		Channels: map[string]ChannelMapping{
			"C2": {ChannelID: "C2", RepoPath: "fleet"},
			"C3": {ChannelID: "C3", Devices: []string{"laptop"}, RepoPath: ""},
			"C4": {ChannelID: "C4", Devices: []string{"mini", "laptop"}, RepoPath: "~/override"},
		},
	}, active)
	if err != nil {
		t.Fatalf("ApplyUpdate failed: %v", err)
	}
	if cfg.Devices["mini"].WorkspaceDir != "~/workspace" || cfg.Devices["laptop"].WorkspaceDir != "/Users/dat/src" {
		t.Fatalf("unexpected normalized devices: %+v", cfg.Devices)
	}
	if _, ok := cfg.Devices["office"]; ok {
		t.Fatalf("expected empty workspace_dir cleared, got %+v", cfg.Devices)
	}
	if got, err := cfg.ResolveRepoPath("mini", "fleet"); err != nil || got != "~/workspace/fleet" {
		t.Fatalf("ResolveRepoPath mini/fleet = %q, %v", got, err)
	}
	if got, err := cfg.ResolveRepoPath("laptop", ""); err != nil || got != "/Users/dat/src" {
		t.Fatalf("ResolveRepoPath laptop/blank = %q, %v", got, err)
	}
	if got, err := cfg.ResolveRepoPath("mini", "~/override"); err != nil || got != "~/override" {
		t.Fatalf("ResolveRepoPath override = %q, %v", got, err)
	}
	if _, err := cfg.ResolveRepoPath("mini", "../escape"); err == nil {
		t.Fatal("expected .. traversal to be rejected")
	}
	if _, err := cfg.ResolveRepoPath("office", "fleet"); err == nil {
		t.Fatal("expected missing workspace_dir for office to fail")
	}
}

func TestRouterLeastAgentsRoundRobinAndOnlineFallback(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "slacker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := Config{
		Devices: map[string]DeviceConfig{
			"mini":   {WorkspaceDir: "~/ws"},
			"laptop": {WorkspaceDir: "/src"},
			"office": {WorkspaceDir: "/work"},
		},
		DefaultDevices:  []string{"mini", "laptop", "office"},
		DefaultStrategy: StrategyRoundRobin,
	}
	chInherit := ChannelMapping{ChannelID: "C1", RepoPath: "fleet"}

	// 1. Round-robin with no online workers falls back to all 3 and cycles across reopen.
	var picked []string
	for range 4 {
		dev, repo, scope, err := SelectTarget(context.Background(), cfg, "default", chInherit, store, nil, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if scope != "default" {
			t.Fatalf("expected default scope, got %q", scope)
		}
		wantRepo, _ := cfg.ResolveRepoPath(dev, "fleet")
		if repo != wantRepo {
			t.Fatalf("repo mismatch for %s: got %q want %q", dev, repo, wantRepo)
		}
		if err := store.SetLastDevice(scope, dev); err != nil {
			t.Fatal(err)
		}
		picked = append(picked, dev)
	}
	if !slices.Equal(picked, []string{"mini", "laptop", "office", "mini"}) {
		t.Fatalf("unexpected round-robin sequence: %v", picked)
	}

	// Reopen DB -> cursor persists (last was mini, so next is laptop).
	_ = store.Close()
	store, err = Open(filepath.Join(tmp, "slacker.db"))
	if err != nil {
		t.Fatal(err)
	}
	devAfterReopen, _, scope, err := SelectTarget(context.Background(), cfg, "default", chInherit, store, nil, time.Now())
	if err != nil || devAfterReopen != "laptop" {
		t.Fatalf("expected laptop after reopen, got %q (err=%v)", devAfterReopen, err)
	}
	_ = store.SetLastDevice(scope, devAfterReopen)

	// 2. Only mini and office online -> skips offline laptop.
	if err := store.UpsertWorkerRuntime("mini", 1, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertWorkerRuntime("office", 2, true, ""); err != nil {
		t.Fatal(err)
	}
	devOnline, _, _, err := SelectTarget(context.Background(), cfg, "default", chInherit, store, nil, time.Now())
	if err != nil || devOnline != "office" {
		t.Fatalf("expected office (skipping offline laptop after laptop), got %q (err=%v)", devOnline, err)
	}

	// 3. Least-agents using agents.db directly + pending slacker_jobs + tie-break cursor.
	hub, err := agents.Open(filepath.Join(tmp, "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	// Give mini 2 live agents and office 1 live agent.
	for i := range 2 {
		if _, err := hub.Heartbeat(agents.HeartbeatParams{Device: "mini", Harness: "claude", RawID: fmt.Sprintf("m%d", i), State: "working"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := hub.Heartbeat(agents.HeartbeatParams{Device: "office", Harness: "claude", RawID: "o1", State: "idle"}); err != nil {
		t.Fatal(err)
	}
	_ = hub.Close()

	loadFn := DefaultAgentLoad(tmp, "")
	chLeast := ChannelMapping{
		ChannelID: "C2",
		Devices:   []string{"mini", "laptop", "office"},
		Strategy:  StrategyLeastAgents,
		RepoPath:  "",
	}
	// Mark laptop online too so all 3 compete: mini=2, laptop=0, office=1 -> picks laptop.
	if err := store.UpsertWorkerRuntime("laptop", 3, true, ""); err != nil {
		t.Fatal(err)
	}
	d1, r1, sc2, err := SelectTarget(context.Background(), cfg, "default", chLeast, store, loadFn, time.Now())
	if err != nil || d1 != "laptop" || r1 != "/src" {
		t.Fatalf("expected laptop /src, got %q %q (err=%v)", d1, r1, err)
	}
	// Enqueue queued job on laptop -> now mini=2, laptop=0+1=1, office=1 -> tie between laptop and office!
	// Cursor is empty (or set to laptop), so tie-break picks office next!
	_, _, err = store.EnqueueJob(Job{
		EventID: "EvLoad1", ChannelID: "C2", MessageTS: "1.1", Reaction: "claude", UserID: "U1",
		TargetDevice: d1, RepoPath: r1, Harness: "claude", SessionName: "s1", Prompt: "p1",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetLastDevice(sc2, d1)

	d2, _, _, err := SelectTarget(context.Background(), cfg, "default", chLeast, store, loadFn, time.Now())
	if err != nil || d2 != "office" {
		t.Fatalf("expected tie-break to pick office over laptop, got %q (err=%v)", d2, err)
	}
}

func TestListenerEndToEndMultiMachineWorkspaceRouting(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "slacker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ms := newMockSlackServer(t)
	ms.messages["C1:1.1"] = SlackMessage{Type: "message", User: "U1", Text: "Task one", TS: "1.1"}
	ms.messages["C1:1.2"] = SlackMessage{Type: "message", User: "U1", Text: "Task two", TS: "1.2"}

	cfg := Config{
		Accounts: map[string]AccountConfig{
			"default": {ID: "default", Enabled: true, AppToken: "xapp-test", UserToken: "xoxp-test", AuthorizedUserID: "UADMIN"},
		},
		Devices: map[string]DeviceConfig{
			"mini":   {WorkspaceDir: "~/workspace"},
			"laptop": {WorkspaceDir: "/Users/dat/src"},
		},
		DefaultDevices:  []string{"mini", "laptop"},
		DefaultStrategy: StrategyLeastAgents,
		Channels: map[string]ChannelMapping{
			"C1": {ChannelID: "C1", ChannelName: "fleet", RepoPath: "fleet"},
		},
		Emojis: DefaultEmojis(),
	}
	cfgPath := filepath.Join(tmp, "slacker.json")
	if err := SaveConfig(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	// Both workers online; mini starts with 1 live agent, laptop has 0 -> 1st goes to laptop, 2nd (with pending job on laptop -> 1 vs 1) tie-breaks to mini.
	_ = store.UpsertWorkerRuntime("mini", 10, true, "")
	_ = store.UpsertWorkerRuntime("laptop", 20, true, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- RunListener(ctx, ListenerOpts{
			StateDir:   tmp,
			ConfigPath: cfgPath,
			Store:      store,
			Slack:      &SlackClient{BaseURL: ms.srv.URL},
			NotifyURL:  "http://127.0.0.1:1/unused",
			AgentLoadFn: func(context.Context) (map[string]int, error) {
				return map[string]int{"mini": 1, "laptop": 0}, nil
			},
			Stderr:      io.Discard,
			BackoffBase: 50 * time.Millisecond,
		})
	}()
	defer func() {
		cancel()
		<-doneCh
	}()

	ms.sendEnvelope(t, reactionEnvelope("e1", "EvM1", "UADMIN", "claude", "C1", "1.1"))
	select {
	case <-ms.ackCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for e1 ack")
	}
	ms.sendEnvelope(t, reactionEnvelope("e2", "EvM2", "UADMIN", "claude", "C1", "1.2"))
	select {
	case <-ms.ackCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for e2 ack")
	}

	jobs, err := store.ListJobs("", "", 10)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %+v (err=%v)", jobs, err)
	}
	byEvt := map[string]Job{}
	for _, j := range jobs {
		byEvt[j.EventID] = j
	}
	j1 := byEvt["EvM1"]
	j2 := byEvt["EvM2"]
	if j1.TargetDevice != "laptop" || j1.RepoPath != "/Users/dat/src/fleet" || !strings.HasPrefix(j1.Prompt, "Task one\n") || j1.Policy != string(session.PolicyBypass) {
		t.Fatalf("unexpected j1: %+v", j1)
	}
	if j2.TargetDevice != "mini" || j2.RepoPath != "~/workspace/fleet" || !strings.HasPrefix(j2.Prompt, "Task two\n") || j2.Policy != string(session.PolicyBypass) {
		t.Fatalf("unexpected j2: %+v", j2)
	}
}

func TestSlackerPolicyConfigAndListenerPropagation(t *testing.T) {
	active := map[string]bool{"mini": true}
	if _, err := ApplyUpdate(Config{}, ConfigUpdate{DefaultPolicy: new("invalid")}, active); err == nil {
		t.Fatal("expected invalid default_policy to fail")
	}
	defPol := "auto"
	cfg, err := ApplyUpdate(Config{}, ConfigUpdate{
		DefaultPolicy: &defPol,
		Channels: map[string]ChannelMapping{
			"C1": {ChannelID: "C1", Device: "mini", RepoPath: "/tmp/r1"},
			"C2": {ChannelID: "C2", Device: "mini", RepoPath: "/tmp/r2", Policy: "safe"},
		},
	}, active)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EffectiveDefaultPolicy() != "auto" || cfg.EffectiveChannelPolicy(cfg.Channels["C1"]) != "auto" || cfg.EffectiveChannelPolicy(cfg.Channels["C2"]) != "safe" {
		t.Fatalf("unexpected policy resolution: default=%q c1=%q c2=%q", cfg.EffectiveDefaultPolicy(), cfg.EffectiveChannelPolicy(cfg.Channels["C1"]), cfg.EffectiveChannelPolicy(cfg.Channels["C2"]))
	}

	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "slacker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ms := newMockSlackServer(t)
	ms.messages["C1:1.1"] = SlackMessage{Type: "message", User: "U1", Text: "Use default auto", TS: "1.1"}
	ms.messages["C2:2.1"] = SlackMessage{Type: "message", User: "U1", Text: "Use safe override", TS: "2.1"}

	cfg.Accounts = map[string]AccountConfig{
		"default": {ID: "default", Enabled: true, AppToken: "xapp-t", UserToken: "xoxp-t", AuthorizedUserID: "UADMIN"},
	}
	cfg.Emojis = DefaultEmojis()
	cfgPath := filepath.Join(tmp, "slacker.json")
	if err := SaveConfig(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- RunListener(ctx, ListenerOpts{
			StateDir: tmp, ConfigPath: cfgPath, Store: store,
			Slack: &SlackClient{BaseURL: ms.srv.URL}, NotifyURL: "http://127.0.0.1:1/unused",
			Stderr: io.Discard, BackoffBase: 50 * time.Millisecond,
		})
	}()
	defer func() {
		cancel()
		<-doneCh
	}()

	ms.sendEnvelope(t, reactionEnvelope("p1", "EvPol1", "UADMIN", "claude", "C1", "1.1"))
	ms.sendEnvelope(t, reactionEnvelope("p2", "EvPol2", "UADMIN", "claude", "C2", "2.1"))
	for range 2 {
		select {
		case <-ms.ackCh:
		case <-time.After(3 * time.Second):
			t.Fatal("timeout waiting for policy test ack")
		}
	}
	jobs, err := store.ListJobs("", "", 10)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %+v (err=%v)", jobs, err)
	}
	byEvt := map[string]Job{}
	for _, j := range jobs {
		byEvt[j.EventID] = j
	}
	if byEvt["EvPol1"].Policy != "auto" || byEvt["EvPol2"].Policy != "safe" {
		t.Fatalf("unexpected job policies: EvPol1=%q EvPol2=%q", byEvt["EvPol1"].Policy, byEvt["EvPol2"].Policy)
	}
}

func TestFormatTaskPrompt(t *testing.T) {
	// Top-level message: leads directly with the ask, omits reaction emoji, Repository:, and default account.
	top := FormatTaskPrompt(
		DefaultAccountID,
		ReactionEvent{Reaction: "claude", ChannelID: "C1", MessageTS: "1700.1"},
		FetchedMessage{
			ChannelName: "eng",
			AuthorName:  "Alice",
			Permalink:   "https://slack.test/archives/C1/p17001",
			Message:     SlackMessage{Text: "  Please check why the build is failing  ", TS: "1700.1"},
		},
		ChannelMapping{ChannelID: "C1", RepoPath: "~/workspace/fleet"},
	)
	if !strings.HasPrefix(top, "Please check why the build is failing\n\n---\n") {
		t.Fatalf("expected top-level prompt to lead directly with trimmed ask, got:\n%s", top)
	}
	for _, unwanted := range []string{":claude:", "Repository:", "account `default`", "thread `"} {
		if strings.Contains(top, unwanted) {
			t.Fatalf("unexpected boilerplate %q in prompt:\n%s", unwanted, top)
		}
	}
	for _, want := range []string{
		"Slack request from Alice in #eng (`C1`, ts `1700.1`).",
		"Link: https://slack.test/archives/C1/p17001",
		"Use `fleet_slack_read`",
		"`fleet_slack_send`",
	} {
		if !strings.Contains(top, want) {
			t.Fatalf("missing %q in prompt:\n%s", want, top)
		}
	}

	// Thread reply on non-default account without channel name: includes parent, thread ts, account, and no #C2 (C2) duplication.
	reply := FormatTaskPrompt(
		"work",
		ReactionEvent{Reaction: "agy", ChannelID: "C2", MessageTS: "1700.2"},
		FetchedMessage{
			AuthorName: "Bob",
			Message:    SlackMessage{Text: "Look at this regression", TS: "1700.2", ThreadTS: "1700.0"},
			Parent:     &SlackMessage{Text: "Deploy failed on staging", TS: "1700.0"},
		},
		ChannelMapping{ChannelID: "C2"},
	)
	if !strings.HasPrefix(reply, "Look at this regression\n\nThread parent:\nDeploy failed on staging\n\n---\n") {
		t.Fatalf("unexpected thread prompt layout:\n%s", reply)
	}
	if !strings.Contains(reply, "Slack request from Bob in channel `C2` (ts `1700.2`, thread `1700.0`, account `work`).") {
		t.Fatalf("unexpected thread footer:\n%s", reply)
	}

	// Empty message text falls back to a clear instruction.
	empty := FormatTaskPrompt(
		DefaultAccountID,
		ReactionEvent{Reaction: "codex", ChannelID: "C3", MessageTS: "1700.3"},
		FetchedMessage{},
		ChannelMapping{ChannelID: "C3"},
	)
	if !strings.HasPrefix(empty, "Inspect this Slack message and handle the request.\n") {
		t.Fatalf("unexpected empty message fallback:\n%s", empty)
	}
}

func TestMessageLevelDeduplicationBlocksButterFingerReacts(t *testing.T) {
	tmp := t.TempDir()
	store, err := Open(filepath.Join(tmp, "slacker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ms := newMockSlackServer(t)
	ms.messages["C1:1700.1"] = SlackMessage{Type: "message", User: "U1", Text: "Fix flaky test", TS: "1700.1"}

	cfg := Config{
		Accounts: map[string]AccountConfig{
			"default": {ID: "default", Enabled: true, AppToken: "xapp-test", UserToken: "xoxp-test", AuthorizedUserID: "UADMIN"},
		},
		Devices: map[string]DeviceConfig{
			"mini":   {WorkspaceDir: "~/ws"},
			"laptop": {WorkspaceDir: "/src"},
		},
		DefaultDevices:  []string{"mini", "laptop"},
		DefaultStrategy: StrategyRoundRobin,
		Channels: map[string]ChannelMapping{
			"C1": {ChannelID: "C1", RepoPath: "fleet"},
		},
		Emojis: DefaultEmojis(),
	}
	cfgPath := filepath.Join(tmp, "slacker.json")
	if err := SaveConfig(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- RunListener(ctx, ListenerOpts{
			StateDir:    tmp,
			ConfigPath:  cfgPath,
			Store:       store,
			Slack:       &SlackClient{BaseURL: ms.srv.URL},
			NotifyURL:   "http://127.0.0.1:1/unused",
			Stderr:      io.Discard,
			BackoffBase: 50 * time.Millisecond,
		})
	}()
	defer func() {
		cancel()
		<-doneCh
	}()

	// 1. Initial :claude: reaction enqueues job on mini and sets cursor to mini.
	ms.sendEnvelope(t, reactionEnvelope("e1", "EvBF1", "UADMIN", "claude", "C1", "1700.1"))
	select {
	case <-ms.ackCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for e1 ack")
	}

	// 2. Butter-finger :agy: and re-react :claude: (new event_ids) on the same message must be ACKed without new jobs or advancing round-robin.
	ms.sendEnvelope(t, reactionEnvelope("e2", "EvBF2", "UADMIN", "agy", "C1", "1700.1"))
	ms.sendEnvelope(t, reactionEnvelope("e3", "EvBF3", "UADMIN", "claude", "C1", "1700.1"))
	for range 2 {
		select {
		case <-ms.ackCh:
		case <-time.After(3 * time.Second):
			t.Fatal("timeout waiting for duplicate reaction ack")
		}
	}

	jobs, err := store.ListJobs("", "", 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("expected only 1 job for butter-finger reactions, got %+v (err=%v)", jobs, err)
	}
	if last, err := store.GetLastDevice("default"); err != nil || last != "mini" {
		t.Fatalf("expected round-robin cursor to stay on mini, got %q (err=%v)", last, err)
	}

	// 3. Direct EnqueueJob deduplicates across queued -> claimed -> launched, while another account stays independent.
	orig := jobs[0]
	if inserted, got, err := store.EnqueueJob(Job{
		EventID: "EvDirectDup1", ChannelID: "C1", MessageTS: "1700.1", Reaction: "codex", UserID: "UADMIN",
		TargetDevice: "laptop", RepoPath: "/src/fleet", Harness: "codex", SessionName: "s-dup", Prompt: "dup",
	}); err != nil || inserted || got.ID != orig.ID {
		t.Fatalf("expected queued message dedupe returning %s, got inserted=%v job=%+v err=%v", orig.ID, inserted, got, err)
	}

	if claimed, err := store.ClaimNextJob("mini", 90*time.Second); err != nil || claimed == nil || claimed.ID != orig.ID {
		t.Fatalf("expected claim of %s, got %+v err=%v", orig.ID, claimed, err)
	}
	if inserted, _, err := store.EnqueueJob(Job{
		EventID: "EvDirectDup2", ChannelID: "C1", MessageTS: "1700.1", Reaction: "agy", UserID: "UADMIN",
		TargetDevice: "laptop", RepoPath: "/src/fleet", Harness: "agy", SessionName: "s-dup2", Prompt: "dup2",
	}); err != nil || inserted {
		t.Fatalf("expected claimed message dedupe, got inserted=%v err=%v", inserted, err)
	}

	if _, err := store.RecordReceipt("mini", Receipt{
		JobID: orig.ID, ReceiptID: "rcpt-bf", SessionName: orig.SessionName, Backend: "tmux", Status: "launched",
	}); err != nil {
		t.Fatal(err)
	}
	if inserted, _, err := store.EnqueueJob(Job{
		EventID: "EvDirectDup3", ChannelID: "C1", MessageTS: "1700.1", Reaction: "agy", UserID: "UADMIN",
		TargetDevice: "laptop", RepoPath: "/src/fleet", Harness: "agy", SessionName: "s-dup3", Prompt: "dup3",
	}); err != nil || inserted {
		t.Fatalf("expected launched message dedupe, got inserted=%v err=%v", inserted, err)
	}

	if inserted, _, err := store.EnqueueJob(Job{
		Account: "work", EventID: "EvWorkSameMsg", ChannelID: "C1", MessageTS: "1700.1", Reaction: "claude", UserID: "UWORK",
		TargetDevice: "laptop", RepoPath: "/src/fleet", Harness: "claude", SessionName: "s-work", Prompt: "work",
	}); err != nil || !inserted {
		t.Fatalf("expected work account message to stay independent, got inserted=%v err=%v", inserted, err)
	}

	// 4. Cancelled/failed job allows a fresh trigger on the same message, and retrying the superseded job is blocked with 409.
	inserted, failJob, err := store.EnqueueJob(Job{
		EventID: "EvFailFirst", ChannelID: "C2", MessageTS: "2000.1", Reaction: "claude", UserID: "UADMIN",
		TargetDevice: "mini", RepoPath: "~/ws/fleet", Harness: "claude", SessionName: "s-fail", Prompt: "first",
	})
	if err != nil || !inserted {
		t.Fatalf("enqueue failJob: inserted=%v err=%v", inserted, err)
	}
	if _, err := store.CancelJob(failJob.ID); err != nil {
		t.Fatal(err)
	}
	inserted, replacement, err := store.EnqueueJob(Job{
		EventID: "EvAfterCancel", ChannelID: "C2", MessageTS: "2000.1", Reaction: "agy", UserID: "UADMIN",
		TargetDevice: "laptop", RepoPath: "/src/fleet", Harness: "agy", SessionName: "s-rep", Prompt: "second",
	})
	if err != nil || !inserted || replacement.ID == failJob.ID {
		t.Fatalf("expected replacement job after cancel, got inserted=%v job=%+v err=%v", inserted, replacement, err)
	}
	if _, err := store.RetryJob(failJob.ID); err == nil || !strings.Contains(err.Error(), "already has an active or launched job") {
		t.Fatalf("expected RetryJob conflict when replacement is active, got %v", err)
	} else if se, ok := errors.AsType[*SlackerError](err); !ok || se.HTTPStatus() != 409 {
		t.Fatalf("expected 409 SlackerError, got %T (%v)", err, err)
	}
}
