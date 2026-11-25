package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/memory"
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

	var out bytes.Buffer
	rc, err := ServeStdio(ServeOpts{
		Harness:           "antigravity",
		Stdin:             strings.NewReader(strings.Join(reqs, "\n") + "\n"),
		Stdout:            &out,
		APIURL:            srv.URL,
		HeartbeatInterval: 60.0,
	})
	if err != nil || rc != 0 {
		t.Fatalf("ServeStdio memory roundtrip failed: rc=%d err=%v", rc, err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 7 {
		t.Fatalf("expected 7 responses, got %d (%s)", len(lines), out.String())
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

	var out bytes.Buffer
	rc, err := ServeStdio(ServeOpts{
		Harness:           "antigravity",
		Stdin:             strings.NewReader(strings.Join(reqs, "\n") + "\n"),
		Stdout:            &out,
		APIURL:            srv.URL,
		CWD:               "/",
		HeartbeatInterval: 60.0,
	})
	if err != nil || rc != 0 {
		t.Fatalf("ServeStdio failed: rc=%d err=%v", rc, err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 6 {
		t.Fatalf("expected 6 responses, got %d (%s)", len(lines), out.String())
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
	var out bytes.Buffer
	rc, err := ServeStdio(ServeOpts{
		Harness:           "claude",
		Stdin:             strings.NewReader(strings.Join(reqs, "\n") + "\n"),
		Stdout:            &out,
		APIURL:            srv.URL,
		HeartbeatInterval: 60.0,
		AutoListen:        &auto,
		ClaudeSocketPath:  filepath.Join(t.TempDir(), "claude.sock"),
		ClaudeSocketToken: "tok",
		ClaudeCacheDir:    t.TempDir(),
		ClaudeSendFn:      func(string, string, string) error { return nil },
	})
	if err != nil || rc != 0 {
		t.Fatalf("claude ServeStdio failed: rc=%d err=%v", rc, err)
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
	var agyOut bytes.Buffer
	rc, err = ServeStdio(ServeOpts{
		Harness: "antigravity",
		Stdin:   strings.NewReader(strings.Join(agyReqs, "\n") + "\n"),
		Stdout:  &agyOut,
		APIURL:  srv.URL,
	})
	if err != nil || rc != 0 {
		t.Fatalf("antigravity ServeStdio failed: rc=%d err=%v", rc, err)
	}
	agyLines := strings.Split(strings.TrimSpace(agyOut.String()), "\n")
	var agyR map[string]any
	_ = json.Unmarshal([]byte(agyLines[1]), &agyR)
	agyTxt := agyR["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	var agyData map[string]any
	_ = json.Unmarshal([]byte(agyTxt), &agyData)
	if agyData["mode"] != "antigravity_sidecar" || agyData["listening"] != false {
		t.Fatalf("unexpected antigravity fleet_listen response: %+v", agyData)
	}
}
