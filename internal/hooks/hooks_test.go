package hooks

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/memory"
	"github.com/bdatdo0601/fleet/internal/pki"
	"github.com/bdatdo0601/fleet/internal/server"
)

func TestTranscriptTailParsers(t *testing.T) {
	tmp := t.TempDir()

	claudeLog := filepath.Join(tmp, "claude.jsonl")
	claudeLines := []string{
		`{"type":"user","isMeta":true,"message":{"role":"user","content":"meta"}}`,
		`{"type":"user","message":{"role":"user","content":"refactor auth"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"secret"},{"type":"tool_use","name":"Edit","input":{"file":"auth.py"}},{"type":"text","text":"Updated auth.py"}]}}`,
	}
	_ = os.WriteFile(claudeLog, []byte(strings.Join(claudeLines, "\n")), 0o644)
	sum, tail := ReadTranscriptTail("claude", claudeLog, "", 20)
	if sum != "refactor auth" || len(tail) != 3 || tail[1].Text != "Edit(file=auth.py)" {
		t.Fatalf("unexpected claude tail: sum=%q tail=%+v", sum, tail)
	}

	codexLog := filepath.Join(tmp, "codex.jsonl")
	codexLines := []string{
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>skip</environment_context>"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"run tests"}]}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"pytest"}}`,
	}
	_ = os.WriteFile(codexLog, []byte(strings.Join(codexLines, "\n")), 0o644)
	cSum, cTail := ReadTranscriptTail("codex", codexLog, "", 20)
	if cSum != "run tests" || len(cTail) != 2 {
		t.Fatalf("unexpected codex tail: sum=%q tail=%+v", cSum, cTail)
	}

	agyLog := filepath.Join(tmp, "agy.jsonl")
	agyLines := []string{
		`{"type":"USER_INPUT","content":"<USER_REQUEST>\ncheck build\n</USER_REQUEST>"}`,
		`{"type":"PLANNER_RESPONSE","content":"Running build","tool_calls":[{"name":"run"}]}`,
	}
	_ = os.WriteFile(agyLog, []byte(strings.Join(agyLines, "\n")), 0o644)
	aSum, aTail := ReadTranscriptTail("antigravity", agyLog, "", 20)
	if aSum != "check build" || len(aTail) != 3 || aTail[2].Role != "tool" {
		t.Fatalf("unexpected agy tail: sum=%q tail=%+v", aSum, aTail)
	}
}

func TestTitleExtractionAllThreeHarnesses(t *testing.T) {
	tmp := t.TempDir()

	// 1. Claude >256KB transcript
	claudeLog := filepath.Join(tmp, "claude_big.jsonl")
	filler := `{"type":"assistant","message":{"role":"assistant","content":"` + strings.Repeat("x", 2048) + `"}}`
	lines := []string{
		`{"type":"ai-title","aiTitle":"Auto Generated Title"}`,
		`{"type":"agent-name","agentName":"Agent Alias"}`,
		`{"type":"custom-title","customTitle":"Justin"}`,
	}
	for range 140 {
		lines = append(lines, filler)
	}
	_ = os.WriteFile(claudeLog, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	title, exp := ReadSessionTitle("claude", "c1", claudeLog, "", "")
	if title != "Justin" || !exp {
		t.Fatalf("unexpected claude title: (%q, %v)", title, exp)
	}

	// 2. Codex session_index.jsonl
	codexHome := filepath.Join(tmp, ".codex")
	_ = os.MkdirAll(codexHome, 0o755)
	uid := "01a1ece5-f00c-7860-a0dc-6c1897e73f30"
	_ = os.WriteFile(filepath.Join(codexHome, "session_index.jsonl"), []byte(strings.Join([]string{
		`{"id":"` + uid + `","thread_name":"Initial Thread"}`,
		`{"id":"other-uuid","thread_name":"Other"}`,
		`{"id":"` + uid + `","thread_name":"Renamed Codex Worker"}`,
	}, "\n")+"\n"), 0o644)
	title, exp = ReadSessionTitle("codex", uid, "", codexHome, "")
	if title != "Renamed Codex Worker" || exp {
		t.Fatalf("unexpected codex title: (%q, %v)", title, exp)
	}

	// 3. Antigravity conversation_summaries.db
	gemHome := filepath.Join(tmp, ".gemini")
	agyDir := filepath.Join(gemHome, "antigravity")
	tDir := filepath.Join(agyDir, "brain", "conv-abc", ".system_generated", "logs")
	_ = os.MkdirAll(tDir, 0o755)
	tFile := filepath.Join(tDir, "transcript.jsonl")
	_ = os.WriteFile(tFile, []byte("{}\n"), 0o644)
	db, err := sql.Open("sqlite", filepath.Join(agyDir, "conversation_summaries.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = db.Exec(`CREATE TABLE conversation_summaries (conversation_id TEXT PRIMARY KEY, title TEXT NOT NULL);
		INSERT INTO conversation_summaries(conversation_id, title) VALUES ('conv-abc', 'Implement Fleet Realtime Push');`)
	_ = db.Close()

	title, exp = ReadSessionTitle("antigravity", "conv-abc", tFile, "", "")
	if title != "Implement Fleet Realtime Push" || exp {
		t.Fatalf("unexpected agy title from transcript path: (%q, %v)", title, exp)
	}
	title, exp = ReadSessionTitle("antigravity", "conv-abc", "", "", gemHome)
	if title != "Implement Fleet Realtime Push" || exp {
		t.Fatalf("unexpected agy title from gemini home: (%q, %v)", title, exp)
	}
}

func TestHookLifecycleAllHarnesses(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_CACHE_DIR", filepath.Join(tmp, "cache"))
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

	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{Device: "mini", Harness: "codex", RawID: "sender-1", CWD: "/tmp"})

	fire := func(harness string, payload map[string]any) map[string]any {
		b, _ := json.Marshal(payload)
		var out bytes.Buffer
		rc, err := RunHook(harness, bytes.NewReader(b), &out, srv.URL)
		if err != nil || rc != 0 {
			t.Fatalf("RunHook %s failed: rc=%d err=%v", harness, rc, err)
		}
		var resp map[string]any
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatalf("bad JSON %q: %v", out.String(), err)
		}
		return resp
	}

	// Antigravity SessionStart -> PreInvocation -> Stop continue -> Stop allow
	startResp := fire("antigravity", map[string]any{"hookEventName": "SessionStart", "conversationId": "40f94b03-1363", "cwd": tmp})
	bStart, _ := json.Marshal(startResp)
	if !strings.Contains(string(bStart), "mini:antigravity:40f94b03-136") {
		t.Fatalf("expected session id in SessionStart: %s", bStart)
	}

	_, _ = f.Agents.SendMessageWithReport("mini", "mini:codex:sender-1", "mini:antigravity:40f94b03-136", "please check schema", nil, nil, nil)
	if len(fire("antigravity", map[string]any{"hookEventName": "PostToolUse", "conversationId": "40f94b03-1363", "cwd": tmp})) != 0 {
		t.Fatal("expected {} from antigravity PostToolUse")
	}
	preResp := fire("antigravity", map[string]any{"hookEventName": "PreInvocation", "conversationId": "40f94b03-1363", "cwd": tmp})
	bPre, _ := json.Marshal(preResp)
	if !strings.Contains(string(bPre), "please check schema") {
		t.Fatalf("expected unread in PreInvocation: %s", bPre)
	}

	_, _ = f.Agents.SendMessageWithReport("mini", "mini:codex:sender-1", "mini:antigravity:40f94b03-136", "one more question", nil, nil, nil)
	stop1 := fire("antigravity", map[string]any{
		"hookEventName": "Stop", "conversationId": "40f94b03-1363", "cwd": tmp,
		"continuationActive": false, "terminationReason": "NO_TOOL_CALL",
	})
	if stop1["decision"] != "continue" {
		t.Fatalf("expected continue, got %v", stop1)
	}
	stop2 := fire("antigravity", map[string]any{
		"hookEventName": "Stop", "conversationId": "40f94b03-1363", "cwd": tmp,
		"continuationActive": true, "terminationReason": "NO_TOOL_CALL",
	})
	if stop2["decision"] != "allow" {
		t.Fatalf("expected allow, got %v", stop2)
	}

	// Claude PermissionRequest -> waiting_permission
	fire("claude", map[string]any{"hook_event_name": "SessionStart", "session_id": "cl-h1", "cwd": tmp})
	fire("claude", map[string]any{
		"hook_event_name": "PermissionRequest", "session_id": "cl-h1", "cwd": tmp,
		"tool_name": "Bash", "tool_input": map[string]any{"command": "npm test"},
	})
	s, _ := f.Agents.GetSession("cl-h1")
	if s.State != "waiting_permission" || s.PendingAction["detail"] != "npm test" {
		t.Fatalf("unexpected claude state: %+v", s)
	}

	// Offline fail-open
	var offlineOut bytes.Buffer
	rc, err := RunHook("claude", strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"c1"}`), &offlineOut, "http://127.0.0.1:1")
	if err != nil || rc != 0 || strings.TrimSpace(offlineOut.String()) != "{}" {
		t.Fatalf("expected fail-open {}, got rc=%d err=%v out=%q", rc, err, offlineOut.String())
	}
}

func TestSessionStartMemoryInjectionAndOfflineCache(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_CACHE_DIR", filepath.Join(tmp, "cache"))
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

	if _, err := f.Memory.Upsert(memory.UpsertParams{
		Scope:  "global",
		Key:    "prefer-few-prs",
		Kind:   "preference",
		Tier:   "always",
		Body:   "Keep PR count minimal and avoid sweeping changes.",
		Source: "human",
	}); err != nil {
		t.Fatal(err)
	}

	repoDir := filepath.Join(tmp, "demo-repo")
	_ = os.MkdirAll(repoDir, 0o755)
	if _, err := f.Memory.Upsert(memory.UpsertParams{
		Scope:  "repo:demo-repo",
		Key:    "build-check",
		Kind:   "project",
		Tier:   "searchable",
		Body:   "Run make check before pushing.",
		Source: "fleet",
	}); err != nil {
		t.Fatal(err)
	}

	// Online SessionStart for claude, codex, and antigravity receives memory and saves cache.
	for _, tc := range []struct {
		harness string
		payload string
	}{
		{"claude", `{"hook_event_name":"SessionStart","session_id":"c-mem-1","cwd":` + mustHookJSON(repoDir) + `}`},
		{"codex", `{"hook_event_name":"SessionStart","session_id":"x-mem-1","cwd":` + mustHookJSON(repoDir) + `}`},
		{"antigravity", `{"hookEventName":"SessionStart","conversationId":"a-mem-1","cwd":` + mustHookJSON(repoDir) + `}`},
	} {
		var out bytes.Buffer
		rc, runErr := RunHook(tc.harness, strings.NewReader(tc.payload), &out, srv.URL)
		if runErr != nil || rc != 0 {
			t.Fatalf("%s SessionStart failed: rc=%d err=%v", tc.harness, rc, runErr)
		}
		if !strings.Contains(out.String(), "[Fleet Memory]") || !strings.Contains(out.String(), "prefer-few-prs") {
			t.Fatalf("%s expected memory in SessionStart output, got %s", tc.harness, out.String())
		}
		if !strings.Contains(out.String(), "fleet_memory_*") {
			t.Fatalf("%s expected fleet_memory_* session_id hint in SessionStart output, got %s", tc.harness, out.String())
		}
	}

	// Offline SessionStart falls back to cached memory context.
	var cachedOut bytes.Buffer
	rc, err := RunHook("claude", strings.NewReader(`{"hook_event_name":"SessionStart","session_id":"c-off","cwd":`+mustHookJSON(repoDir)+`}`), &cachedOut, "http://127.0.0.1:1")
	if err != nil || rc != 0 {
		t.Fatalf("offline SessionStart failed: rc=%d err=%v", rc, err)
	}
	if !strings.Contains(cachedOut.String(), "[Fleet Memory]") || !strings.Contains(cachedOut.String(), "prefer-few-prs") {
		t.Fatalf("expected offline cached memory on SessionStart, got %s", cachedOut.String())
	}
}

func mustHookJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
