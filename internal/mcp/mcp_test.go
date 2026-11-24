package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/pki"
	"github.com/bdatdo0601/fleet/internal/server"
)

func setupMCPServer(t *testing.T) (*core.Fleet, *httptest.Server) {
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
	return f, srv
}

func TestMCPStdioRoundtripAndDeregisterOnEOF(t *testing.T) {
	f, srv := setupMCPServer(t)

	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{
		Device: "mini", Harness: "antigravity", RawID: "conv-9999",
		CWD: "/srv", Repo: "infra", Summary: new("checking logs"),
		Tail: []agents.TailItem{{Role: "user", Text: "check logs"}},
	})
	ppid := os.Getppid()
	cwd, _ := os.Getwd()
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{
		Device: "mini", Harness: "claude", RawID: fmt.Sprintf("pid-%d", ppid), PID: &ppid, CWD: cwd,
	})
	_, _ = f.Agents.SendMessageWithReport("mini", "mini:antigravity:conv-9999", fmt.Sprintf("mini:claude:pid-%d", ppid), "see line 12", nil, nil, nil)

	go func() {
		for range 60 {
			msgs, _ := f.Agents.FetchInbox("mini", "mini:antigravity:conv-9999", true, true, 50)
			for _, m := range msgs {
				if m.Body == "which port?" {
					_, _ = f.Agents.SendMessageWithReport("mini", "mini:antigravity:conv-9999", m.FromSession, "port 7443", nil, &m.ID, nil)
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	reqs := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fleet_list_agents","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fleet_inspect_agent","arguments":{"agent_id":"conv-9999"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"fleet_check_inbox","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"fleet_send_message","arguments":{"to":"conv-9999","message":"got it"}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"fleet_ask_agent","arguments":{"to":"conv-9999","question":"which port?","timeout_seconds":3}}}`,
	}
	var out bytes.Buffer
	rc, err := ServeStdio(ServeOpts{
		Harness:           "claude",
		Stdin:             strings.NewReader(strings.Join(reqs, "\n") + "\n"),
		Stdout:            &out,
		APIURL:            srv.URL,
		HeartbeatInterval: 60.0,
	})
	if err != nil || rc != 0 {
		t.Fatalf("ServeStdio failed: rc=%d err=%v", rc, err)
	}

	var resps []map[string]any
	for ln := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(ln), &r); err != nil {
			t.Fatalf("bad response %q: %v", ln, err)
		}
		resps = append(resps, r)
	}
	if len(resps) != 7 {
		t.Fatalf("expected 7 responses, got %d", len(resps))
	}

	askText := resps[6]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	var askData map[string]any
	_ = json.Unmarshal([]byte(askText), &askData)
	if askData["timed_out"] != false || askData["reply"].(map[string]any)["body"] != "port 7443" {
		t.Fatalf("unexpected ask response: %s", askText)
	}

	// Verify stdio session deregistered on EOF
	liveAfter, _ := f.Agents.ListSessions("", "", "", "", false)
	if len(liveAfter) != 1 || liveAfter[0].SessionID != "mini:antigravity:conv-9999" {
		t.Fatalf("expected only antigravity live after EOF, got %+v", liveAfter)
	}
}

func TestMCPCodexAutoListenAndSetName(t *testing.T) {
	f, srv := setupMCPServer(t)
	ppid := os.Getppid()
	cwd, _ := os.Getwd()
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{
		Device: "mini", Harness: "codex", RawID: "01a1ece5-f00c-7860-a0dc-6c1897e73f31", PID: &ppid, CWD: cwd,
	})
	reqs := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fleet_listen","arguments":{"action":"status"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fleet_listen","arguments":{"action":"stop"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fleet_listen","arguments":{"action":"start"}}}`,
	}
	auto := true
	var out bytes.Buffer
	rc, err := ServeStdio(ServeOpts{
		Harness:           "codex",
		Stdin:             strings.NewReader(strings.Join(reqs, "\n") + "\n"),
		Stdout:            &out,
		APIURL:            srv.URL,
		HeartbeatInterval: 60.0,
		AutoListen:        &auto,
		CodexRunQueueFn:   func([]string) (int, string) { return 0, "" },
	})
	if err != nil || rc != 0 {
		t.Fatalf("codex ServeStdio failed: rc=%d err=%v", rc, err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 responses, got %d", len(lines))
	}
	parseText := func(idx int) map[string]any {
		var r map[string]any
		_ = json.Unmarshal([]byte(lines[idx]), &r)
		txt := r["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
		var d map[string]any
		_ = json.Unmarshal([]byte(txt), &d)
		return d
	}
	st0, st1, st2 := parseText(1), parseText(2), parseText(3)
	if st0["mode"] != "mcp_codex_queue" || st0["enabled"] != true || st1["enabled"] != false || st2["enabled"] != true {
		t.Fatalf("unexpected codex listen states: %v / %v / %v", st0, st1, st2)
	}
}
