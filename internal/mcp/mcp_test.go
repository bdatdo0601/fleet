package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	"github.com/bdatdo0601/fleet/internal/memory"
	"github.com/bdatdo0601/fleet/internal/pki"
	"github.com/bdatdo0601/fleet/internal/server"
	"github.com/bdatdo0601/fleet/internal/slacker"
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

func runMCPSequential(t *testing.T, opts ServeOpts, reqs []string) []string {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	opts.Stdin = inR
	opts.Stdout = outW

	type serveRes struct {
		rc  int
		err error
	}
	doneCh := make(chan serveRes, 1)
	go func() {
		rc, err := ServeStdio(opts)
		_ = outW.Close()
		doneCh <- serveRes{rc: rc, err: err}
	}()

	br := bufio.NewReader(outR)
	var lines []string
	for _, raw := range reqs {
		var meta struct {
			ID any `json:"id"`
		}
		_ = json.Unmarshal([]byte(raw), &meta)
		if _, err := io.WriteString(inW, raw+"\n"); err != nil {
			t.Fatalf("write req %s: %v", raw, err)
		}
		if meta.ID != nil {
			ln, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("read response for %s: %v", raw, err)
			}
			lines = append(lines, strings.TrimSpace(ln))
		}
	}
	_ = inW.Close()
	for {
		ln, err := br.ReadString('\n')
		if strings.TrimSpace(ln) != "" {
			lines = append(lines, strings.TrimSpace(ln))
		}
		if err != nil {
			break
		}
	}
	res := <-doneCh
	if res.err != nil || res.rc != 0 {
		t.Fatalf("ServeStdio failed: rc=%d err=%v", res.rc, res.err)
	}
	return lines
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

	bgCtx, bgCancel := context.WithCancel(context.Background())
	var bgWG sync.WaitGroup
	bgWG.Add(1)
	defer func() {
		bgCancel()
		bgWG.Wait()
	}()
	go func() {
		defer bgWG.Done()
		for range 60 {
			if bgCtx.Err() != nil {
				return
			}
			msgs, _ := f.Agents.FetchInbox("mini", "mini:antigravity:conv-9999", true, true, 50)
			for _, m := range msgs {
				if m.Body == "which port?" {
					_, _ = f.Agents.SendMessageWithReport("mini", "mini:antigravity:conv-9999", m.FromSession, "port 7443", nil, &m.ID, nil)
					return
				}
			}
			select {
			case <-bgCtx.Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
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
	lines := runMCPSequential(t, ServeOpts{
		Harness:           "claude",
		APIURL:            srv.URL,
		HeartbeatInterval: 60.0,
	}, reqs)

	var resps []map[string]any
	for _, ln := range lines {
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
	lines := runMCPSequential(t, ServeOpts{
		Harness:           "codex",
		APIURL:            srv.URL,
		HeartbeatInterval: 60.0,
		AutoListen:        &auto,
		CodexRunQueueFn:   func([]string) (int, string) { return 0, "" },
	}, reqs)
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

func TestMCPMemoryToolsRoundtrip(t *testing.T) {
	_, srv := setupMCPServer(t)

	reqs := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fleet_memory_set","arguments":{"scope":"global","key":"small-prs","kind":"preference","tier":"always","body":"Prefer small focused changes and few PRs."}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fleet_memory_search","arguments":{"query":"focused changes","scope":"global"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"fleet_memory_get","arguments":{"scope":"global","key":"small-prs","include_history":true}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"fleet_memory_delete","arguments":{"scope":"global","key":"small-prs"}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"fleet_memory_search","arguments":{"query":"focused changes","scope":"global"}}}`,
	}

	lines := runMCPSequential(t, ServeOpts{
		Harness:           "antigravity",
		APIURL:            srv.URL,
		HeartbeatInterval: 60.0,
	}, reqs)
	if len(lines) != 7 {
		t.Fatalf("expected 7 responses, got %d (%v)", len(lines), lines)
	}

	parseToolJSON := func(idx int) map[string]any {
		var r map[string]any
		if err := json.Unmarshal([]byte(lines[idx]), &r); err != nil {
			t.Fatalf("line %d json: %v", idx, err)
		}
		res := r["result"].(map[string]any)
		if isErr, _ := res["isError"].(bool); isErr {
			t.Fatalf("line %d returned tool error: %v", idx, res)
		}
		txt := res["content"].([]any)[0].(map[string]any)["text"].(string)
		var payload map[string]any
		if err := json.Unmarshal([]byte(txt), &payload); err != nil {
			t.Fatalf("line %d inner json %q: %v", idx, txt, err)
		}
		return payload
	}

	if !strings.Contains(lines[1], "fleet_memory_set") {
		t.Fatalf("expected fleet_memory_set in tools/list: %s", lines[1])
	}
	if setRes := parseToolJSON(2); setRes["action"] != "created" {
		t.Fatalf("expected created action, got %+v", setRes)
	}
	if search1 := parseToolJSON(3); len(search1["memories"].([]any)) != 1 {
		t.Fatalf("expected 1 search result, got %+v", search1)
	}
	if getRes := parseToolJSON(4); getRes["memory"].(map[string]any)["key"] != "small-prs" || len(getRes["history"].([]any)) != 1 {
		t.Fatalf("unexpected get response: %+v", getRes)
	}
	if delRes := parseToolJSON(5); delRes["ok"] != true {
		t.Fatalf("unexpected delete response: %+v", delRes)
	}
	if search2 := parseToolJSON(6); len(search2["memories"].([]any)) != 0 {
		t.Fatalf("expected 0 results after archive, got %+v", search2)
	}
}

func TestMCPMemoryToolsResolveSessionRepoFromRootCWD(t *testing.T) {
	f, srv := setupMCPServer(t)

	repoDir := filepath.Join(t.TempDir(), "wt-alias")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", repoDir, "init"},
		{"-C", repoDir, "remote", "add", "origin", "git@github.com:example/iron-fillet.git"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}

	if _, err := f.Agents.Heartbeat(agents.HeartbeatParams{
		Device:  "mini",
		Harness: "antigravity",
		RawID:   "conv-mem-1",
		CWD:     repoDir,
		Repo:    "wt-alias",
	}); err != nil {
		t.Fatal(err)
	}

	for _, item := range []memory.UpsertParams{
		{Scope: "global", Key: "g-pref", Kind: "preference", Tier: "always", Body: "global pref", Source: "human"},
		{Scope: "repo:iron-fillet", Key: "r-note", Kind: "project", Tier: "searchable", Body: "iron-fillet note", Source: "fleet"},
		{Scope: "repo:other", Key: "o-note", Kind: "project", Tier: "searchable", Body: "other repo note", Source: "fleet"},
	} {
		if _, err := f.Memory.Upsert(item); err != nil {
			t.Fatal(err)
		}
	}

	reqs := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fleet_memory_search","arguments":{"session_id":"mini:antigravity:conv-mem-1"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fleet_memory_set","arguments":{"key":"new-conv","body":"use moon","session_id":"mini:antigravity:conv-mem-1"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fleet_memory_delete","arguments":{"key":"new-conv","session_id":"mini:antigravity:conv-mem-1"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"fleet_memory_search","arguments":{"session_id":"mini:antigravity:conv-mem-1"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"fleet_memory_search","arguments":{}}}`,
	}

	lines := runMCPSequential(t, ServeOpts{
		Harness:           "antigravity",
		APIURL:            srv.URL,
		CWD:               "/",
		HeartbeatInterval: 60.0,
	}, reqs)
	if len(lines) != 6 {
		t.Fatalf("expected 6 responses, got %d (%v)", len(lines), lines)
	}

	parseToolJSON := func(idx int) map[string]any {
		var r map[string]any
		if err := json.Unmarshal([]byte(lines[idx]), &r); err != nil {
			t.Fatalf("line %d json: %v", idx, err)
		}
		res := r["result"].(map[string]any)
		if isErr, _ := res["isError"].(bool); isErr {
			t.Fatalf("line %d returned tool error: %v", idx, res)
		}
		txt := res["content"].([]any)[0].(map[string]any)["text"].(string)
		var payload map[string]any
		if err := json.Unmarshal([]byte(txt), &payload); err != nil {
			t.Fatalf("line %d inner json %q: %v", idx, txt, err)
		}
		return payload
	}
	memoryKeys := func(payload map[string]any) map[string]bool {
		got := map[string]bool{}
		for _, raw := range payload["memories"].([]any) {
			m := raw.(map[string]any)
			got[m["scope"].(string)+"/"+m["key"].(string)] = true
		}
		return got
	}

	s1 := memoryKeys(parseToolJSON(1))
	if !s1["global/g-pref"] || !s1["repo:iron-fillet/r-note"] || s1["repo:other/o-note"] {
		t.Fatalf("unexpected initial search results: %+v", s1)
	}

	setRes := parseToolJSON(2)
	setItem := setRes["memory"].(map[string]any)
	if setRes["action"] != "created" || setItem["scope"] != "repo:iron-fillet" || setItem["key"] != "new-conv" {
		t.Fatalf("expected repo:iron-fillet/new-conv created, got %+v", setRes)
	}

	delRes := parseToolJSON(3)
	if delRes["ok"] != true {
		t.Fatalf("expected delete ok=true, got %+v", delRes)
	}

	s2 := memoryKeys(parseToolJSON(4))
	if s2["repo:iron-fillet/new-conv"] || !s2["repo:iron-fillet/r-note"] {
		t.Fatalf("expected new-conv deleted after archive, got %+v", s2)
	}

	s3 := memoryKeys(parseToolJSON(5))
	if !s3["global/g-pref"] || s3["repo:iron-fillet/r-note"] || s3["repo:other/o-note"] {
		t.Fatalf("expected global-only fallback without session_id on CWD=/, got %+v", s3)
	}
}

func TestMCPClaudeAutoListenAndAntigravitySidecarStatus(t *testing.T) {
	f, srv := setupMCPServer(t)
	ppid := os.Getppid()
	cwd, _ := os.Getwd()
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{
		Device: "mini", Harness: "claude", RawID: "cl-mcp-1", PID: &ppid, CWD: cwd,
	})
	reqs := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fleet_listen","arguments":{"action":"status"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fleet_listen","arguments":{"action":"stop"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fleet_listen","arguments":{"action":"start"}}}`,
	}
	auto := true
	lines := runMCPSequential(t, ServeOpts{
		Harness:           "claude",
		APIURL:            srv.URL,
		HeartbeatInterval: 60.0,
		AutoListen:        &auto,
		ClaudeSocketPath:  filepath.Join(t.TempDir(), "claude.sock"),
		ClaudeSocketToken: "tok",
		ClaudeCacheDir:    t.TempDir(),
		ClaudeSendFn:      func(string, string, string) error { return nil },
	}, reqs)
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
	if st0["mode"] != "mcp_claude_socket" || st0["enabled"] != true || st1["enabled"] != false || st2["enabled"] != true {
		t.Fatalf("unexpected claude listen states: %v / %v / %v", st0, st1, st2)
	}

	// Antigravity fleet_listen reports sidecar status.
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{
		Device: "mini", Harness: "antigravity", RawID: "agy-sidecar-1", CWD: cwd,
	})
	agyReqs := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fleet_listen","arguments":{"session_id":"mini:antigravity:agy-sidecar-1"}}}`,
	}
	agyLines := runMCPSequential(t, ServeOpts{
		Harness: "antigravity",
		APIURL:  srv.URL,
	}, agyReqs)
	var agyR map[string]any
	_ = json.Unmarshal([]byte(agyLines[1]), &agyR)
	agyTxt := agyR["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	var agyData map[string]any
	_ = json.Unmarshal([]byte(agyTxt), &agyData)
	if agyData["mode"] != "antigravity_sidecar" || agyData["listening"] != false {
		t.Fatalf("unexpected antigravity fleet_listen response: %+v", agyData)
	}
}

func TestMCPSlackTools(t *testing.T) {
	f, srv := setupMCPServer(t)

	var postedText string
	slackMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/conversations.replies":
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{"ts":"100.0","user":"U1","text":"Investigate latency spike"}]}`))
		case "/search.messages":
			_, _ = w.Write([]byte(`{"ok":true,"messages":{"matches":[{"ts":"200.0","user":"U2","text":"latency trace","channel":{"id":"CWORK","name":"eng"}}]}}`))
		case "/chat.postMessage":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			postedText, _ = b["text"].(string)
			_, _ = w.Write([]byte(`{"ok":true,"channel":"CWORK","ts":"100.5"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(slackMock.Close)
	f.SlackClient = &slacker.SlackClient{BaseURL: slackMock.URL}

	if err := slacker.SaveConfig(f.SlackerConfigPath, slacker.Config{
		Accounts: map[string]slacker.AccountConfig{
			"work": {ID: "work", Enabled: true, AppToken: "xapp-w", UserToken: "xoxp-w", BotToken: "xoxb-w", AuthorizedUserID: "UW"},
		},
		Channels: map[string]slacker.ChannelMapping{
			slacker.ChannelKey("work", "CWORK"): {Account: "work", ChannelID: "CWORK", Device: "mini", RepoPath: "/tmp/work"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	reqs := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fleet_slack_channels","arguments":{"account":"work"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fleet_slack_read","arguments":{"channel":"CWORK","thread_ts":"100.0"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fleet_slack_read","arguments":{"account":"work","query":"latency"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"fleet_slack_send","arguments":{"channel":"CWORK","thread_ts":"100.0","text":"Fixed in commit abc"}}}`,
	}
	lines := runMCPSequential(t, ServeOpts{
		Harness:           "claude",
		APIURL:            srv.URL,
		HeartbeatInterval: 60.0,
	}, reqs)
	if len(lines) != 5 {
		t.Fatalf("expected 5 responses, got %d: %v", len(lines), lines)
	}
	parseTool := func(idx int) map[string]any {
		var r map[string]any
		_ = json.Unmarshal([]byte(lines[idx]), &r)
		txt := r["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
		var d map[string]any
		_ = json.Unmarshal([]byte(txt), &d)
		return d
	}
	chData := parseTool(1)
	if len(chData["mapped_channels"].([]any)) != 1 {
		t.Fatalf("unexpected fleet_slack_channels output: %+v", chData)
	}
	threadData := parseTool(2)
	if threadData["mode"] != "thread" || threadData["account"] != "work" {
		t.Fatalf("unexpected thread read output: %+v", threadData)
	}
	searchData := parseTool(3)
	if searchData["mode"] != "search" {
		t.Fatalf("unexpected search read output: %+v", searchData)
	}
	sendData := parseTool(4)
	if sendData["ts"] != "100.5" || postedText != "Fixed in commit abc" {
		t.Fatalf("unexpected send output: %+v posted=%q", sendData, postedText)
	}
}

func TestMCPConcurrentToolsCallEOFCancelAndMalformedParams(t *testing.T) {
	slowStarted := make(chan struct{})
	slowCanceled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agents/ask":
			_, _ = io.Copy(io.Discard, r.Body)
			close(slowStarted)
			select {
			case <-r.Context().Done():
				close(slowCanceled)
			case <-time.After(5 * time.Second):
			}
		case "/v1/agents":
			_, _ = w.Write([]byte(`{"agents":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	doneCh := make(chan error, 1)
	auto := false
	go func() {
		_, err := ServeStdio(ServeOpts{
			Harness:    "antigravity",
			Stdin:      inR,
			Stdout:     outW,
			APIURL:     srv.URL,
			AutoListen: &auto,
		})
		_ = outW.Close()
		doneCh <- err
	}()

	br := bufio.NewReader(outR)

	// 1. Malformed params returns JSON-RPC -32602.
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"not-an-object"}`+"\n")
	ln, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var badResp map[string]any
	_ = json.Unmarshal([]byte(ln), &badResp)
	if badResp["error"].(map[string]any)["code"] != float64(-32602) {
		t.Fatalf("expected -32602 on malformed params, got %s", ln)
	}

	// 2. Oversized line returns -32700 without killing server.
	_, _ = io.WriteString(inW, strings.Repeat("x", maxMCPRequestBytes+64)+"\n")
	ln, err = br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var bigResp map[string]any
	_ = json.Unmarshal([]byte(ln), &bigResp)
	if bigResp["error"].(map[string]any)["code"] != float64(-32700) {
		t.Fatalf("expected -32700 on oversized line, got %s", ln)
	}

	// 3. Slow tools/call does not block concurrent tools/call, and stdin EOF cancels slow call.
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fleet_ask_agent","arguments":{"to":"x","question":"hi","timeout_seconds":5}}}`+"\n")
	<-slowStarted
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fleet_list_agents","arguments":{}}}`+"\n")
	fastLn, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var fastResp map[string]any
	_ = json.Unmarshal([]byte(fastLn), &fastResp)
	if fastResp["id"] != float64(3) {
		t.Fatalf("expected concurrent fast tools/call id=3 first, got %s", fastLn)
	}

	// Closing stdin cancels the in-flight slow call promptly.
	_ = inW.Close()
	select {
	case <-slowCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected slow tools/call HTTP request to be cancelled on stdin EOF")
	}
	// Drain remaining output (cancelled id=2 response) and wait for ServeStdio exit.
	for {
		if _, rErr := br.ReadString('\n'); rErr != nil {
			break
		}
	}
	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("ServeStdio returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeStdio did not exit after stdin EOF")
	}
}

func TestMCPProfilesWatchAndEventsTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/agents/watches":
			_, _ = w.Write([]byte(`{"watch":{"id":"w-test1","target_name":"worker-1","events":["idle"]}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/events":
			_, _ = w.Write([]byte(`{"ok":true,"event":{"id":42,"type":"external.event"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/events":
			_, _ = w.Write([]byte(`{"events":[{"id":42,"type":"external.event","title":"Deployed"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/personal/mcp/tools":
			_, _ = w.Write([]byte(`{"tools":[{"full_name":"mcp__gcal__list_events","server_name":"gcal","remote_name":"list_events","description":"List events","input_schema":{"type":"object","properties":{}}}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/personal/vault/search":
			_, _ = w.Write([]byte(`{"query":"kyoto","hits":[{"vault":"agent","path":"Inbox/trip.md"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/personal/mcp/call":
			_, _ = w.Write([]byte(`{"result":{"items":["ok"]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	auto := false

	// Core profile includes fleet_watch_agent, fleet_events_emit, fleet_events_list and excludes personal_* tools.
	coreResps := runMCPSequential(t, ServeOpts{Harness: "antigravity", Profile: "core", APIURL: srv.URL, AutoListen: &auto}, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fleet_watch_agent","arguments":{"action":"create","target":"worker-1","session_id":"laptop:antigravity:s1"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fleet_events_emit","arguments":{"title":"Deployed"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fleet_events_list","arguments":{"limit":10}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"personal_vault_search","arguments":{"query":"kyoto"}}}`,
	})
	if !strings.Contains(coreResps[0], "fleet_watch_agent") || !strings.Contains(coreResps[0], "fleet_events_emit") || strings.Contains(coreResps[0], "personal_vault_search") {
		t.Fatalf("unexpected core profile tools/list: %s", coreResps[0])
	}
	if !strings.Contains(coreResps[1], "w-test1") || !strings.Contains(coreResps[2], "external.event") || !strings.Contains(coreResps[3], "Deployed") {
		t.Fatalf("unexpected core tool call results: %+v", coreResps[1:4])
	}
	if !strings.Contains(coreResps[4], `"isError":true`) {
		t.Fatalf("expected personal_vault_search rejected in core profile: %s", coreResps[4])
	}

	// Personal profile exposes only personal_vault_* + dynamic /v1/personal/mcp/tools and rejects core tools.
	persResps := runMCPSequential(t, ServeOpts{Harness: "antigravity", Profile: "personal", APIURL: srv.URL, AutoListen: &auto}, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"personal_vault_search","arguments":{"query":"kyoto"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"mcp__gcal__list_events","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fleet_list_agents","arguments":{}}}`,
	})
	if !strings.Contains(persResps[0], "personal_vault_search") || !strings.Contains(persResps[0], "mcp__gcal__list_events") || strings.Contains(persResps[0], "fleet_list_agents") {
		t.Fatalf("unexpected personal profile tools/list: %s", persResps[0])
	}
	if !strings.Contains(persResps[1], "Inbox/trip.md") || !strings.Contains(persResps[2], "items") {
		t.Fatalf("unexpected personal profile call results: %+v", persResps[1:3])
	}
	if !strings.Contains(persResps[3], `"isError":true`) {
		t.Fatalf("expected fleet_list_agents rejected in personal profile: %s", persResps[3])
	}
}
