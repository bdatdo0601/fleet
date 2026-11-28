package assistant

import (
	"bufio"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdatdo0601/fleet/internal/events"
)

type fakeDeps struct {
	mu           sync.Mutex
	sentSlack    []string
	sentMessages []map[string]any
	spawned      []SpawnAgentRequest
	killed       []string
	watches      []AgentWatchInfo
	swept        int
	events       []events.Event
	eventSubs    map[chan events.Event]struct{}
	lhWorker     LighthouseWorkerStatus
	systemHealth map[string]any
}

func (f *fakeDeps) LighthouseWorkerStatus() LighthouseWorkerStatus {
	return f.lhWorker
}

func (f *fakeDeps) ListAgents(_, _, _, _ string, _ bool) (any, error) {
	return []map[string]any{{"name": "auth-worker", "device": "laptop", "state": "waiting_permission"}}, nil
}

func (f *fakeDeps) InspectAgent(target string) (any, error) {
	return map[string]any{"target": target, "state": "idle"}, nil
}

func (f *fakeDeps) SendAgentMessage(fromChannel, to, body, threadID string, replyTo *int64) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	msg := map[string]any{"from_channel": fromChannel, "to": to, "body": body, "thread_id": threadID, "reply_to": replyTo}
	f.sentMessages = append(f.sentMessages, msg)
	return msg, nil
}

func (f *fakeDeps) AskAgent(_ context.Context, fromChannel, to, question string, _ float64) (any, error) {
	return map[string]any{"from_channel": fromChannel, "to": to, "question": question, "reply": "ok"}, nil
}

func (f *fakeDeps) WatchAgent(req AgentWatchRequest) (AgentWatchInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := AgentWatchInfo{
		ID:            fmt.Sprintf("w-%d", len(f.watches)+1),
		ChannelID:     cmp.Or(req.ChannelID, "fleet-ops"),
		TargetSession: req.Target,
		Events:        req.Events,
		Once:          req.Once,
	}
	f.watches = append(f.watches, w)
	return w, nil
}

func (f *fakeDeps) ListWatches(channelID string) ([]AgentWatchInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if channelID == "" {
		return append([]AgentWatchInfo(nil), f.watches...), nil
	}
	var out []AgentWatchInfo
	for _, w := range f.watches {
		if w.ChannelID == channelID {
			out = append(out, w)
		}
	}
	return out, nil
}

func (f *fakeDeps) DeleteWatch(channelID, idOrTarget string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var kept []AgentWatchInfo
	deleted := 0
	for _, w := range f.watches {
		if (channelID == "" || w.ChannelID == channelID) && (w.ID == idOrTarget || w.TargetSession == idOrTarget) {
			deleted++
			continue
		}
		kept = append(kept, w)
	}
	f.watches = kept
	return deleted, nil
}

func (f *fakeDeps) SweepStaleAgents(_ time.Time) {
	f.mu.Lock()
	f.swept++
	f.mu.Unlock()
}

func (f *fakeDeps) SystemHealth() (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.systemHealth != nil {
		return f.systemHealth, nil
	}
	return map[string]any{"devices_total": 2}, nil
}

func (f *fakeDeps) ListMemoryProposals(_ string, _ int) (any, error) {
	return []map[string]any{}, nil
}

func (f *fakeDeps) DecideMemoryProposal(id int64, approve bool) (any, error) {
	return map[string]any{"id": id, "approved": approve}, nil
}

func (f *fakeDeps) ListEvents(filter events.Filter) ([]events.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []events.Event
	for _, ev := range f.events {
		if filter.TypePrefix != "" && !strings.HasPrefix(ev.Type, filter.TypePrefix) {
			continue
		}
		if filter.Severity != "" && ev.Severity != filter.Severity {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

func (f *fakeDeps) PublishEvent(ev events.Event) (events.Event, bool, error) {
	f.mu.Lock()
	ev.ID = int64(len(f.events) + 1)
	if ev.CreatedAt == "" {
		ev.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	f.events = append(f.events, ev)
	subs := make([]chan events.Event, 0, len(f.eventSubs))
	for ch := range f.eventSubs {
		subs = append(subs, ch)
	}
	f.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
	return ev, true, nil
}

func (f *fakeDeps) SubscribeEvents() chan events.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.eventSubs == nil {
		f.eventSubs = make(map[chan events.Event]struct{})
	}
	ch := make(chan events.Event, 32)
	f.eventSubs[ch] = struct{}{}
	return ch
}

func (f *fakeDeps) UnsubscribeEvents(ch chan events.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.eventSubs, ch)
	close(ch)
}

func (f *fakeDeps) ListPaths(device, query string, _ int) (any, error) {
	return []map[string]any{{"device": cmp.Or(device, "laptop"), "repo": "fleet", "path": "/Users/datdo/workspace/fleet", "query": query}}, nil
}

func (f *fakeDeps) SpawnAgent(_ context.Context, req SpawnAgentRequest) (SpawnAgentResult, error) {
	f.spawned = append(f.spawned, req)
	return SpawnAgentResult{
		JobID:        "job-spawn-1",
		TargetDevice: cmp.Or(req.Device, "laptop"),
		Harness:      req.Harness,
		SessionName:  cmp.Or(req.Name, "task-1"),
		RepoPath:     cmp.Or(req.RepoPath, "/Users/datdo/workspace/fleet"),
		Status:       "queued",
		WorkerOnline: true,
	}, nil
}

func (f *fakeDeps) KillAgent(_ context.Context, sessionID string) (any, error) {
	f.killed = append(f.killed, sessionID)
	return map[string]any{"ok": true, "session_id": sessionID}, nil
}

func (f *fakeDeps) MemorySearch(query, scope, _, _ string, _ int) (any, error) {
	return []map[string]any{{"scope": scope, "key": "pref", "summary": query}}, nil
}

func (f *fakeDeps) MemoryGet(scope, key string) (any, error) {
	return map[string]any{"scope": scope, "key": key, "body": "concise"}, nil
}

func (f *fakeDeps) MemorySet(scope, key, _, _, _, _ string) (any, error) {
	return map[string]any{"scope": scope, "key": key, "action": "created"}, nil
}

func (f *fakeDeps) MemoryContext(_, _ string) (string, error) {
	return "- [global] Always prefer concise responses", nil
}

func (f *fakeDeps) SlackChannels(_ context.Context, _ string, _ bool, _ int) (any, error) {
	return map[string]any{"mapped_channels": []string{"C123"}}, nil
}

func (f *fakeDeps) SlackRead(_ context.Context, _, channel, _, _ string, _ int) (any, error) {
	return map[string]any{"channel": channel, "messages": []string{"hello"}}, nil
}

func (f *fakeDeps) SlackSend(_ context.Context, _, channel, text, _ string) (any, error) {
	f.sentSlack = append(f.sentSlack, channel+":"+text)
	return map[string]any{"ok": true, "channel": channel}, nil
}

func TestHelperMCPProcess(t *testing.T) {
	if os.Getenv("GO_WANT_MCP_HELPER") != "1" {
		t.Log("mcp helper process idle")
		return
	}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			writeHelperJSON(req.ID, map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}})
		case "tools/list":
			writeHelperJSON(req.ID, map[string]any{
				"tools": []map[string]any{{
					"name":        "echo_secret",
					"description": "Echo input with injected env check",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"msg": map[string]any{"type": "string"}}},
				}},
			})
		case "tools/call":
			args, _ := req.Params["arguments"].(map[string]any)
			if mode, _ := args["mode"].(string); mode == "error" {
				b, _ := json.Marshal(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"error":   map[string]any{"code": -32000, "message": "simulated mcp failure"},
				})
				fmt.Println(string(b))
				continue
			} else if mode == "hang" {
				time.Sleep(10 * time.Second)
				continue
			}
			writeHelperJSON(req.ID, map[string]any{
				"msg":     args["msg"],
				"has_env": os.Getenv("TEST_MCP_TOKEN") == "injected-secret-99",
			})
		}
	}
	os.Exit(0)
}

func writeHelperJSON(id, result any) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	fmt.Println(string(b))
}

func TestConfigMaskingAndPersistence(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "assistant.json")
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.APIKey = "sk-or-v1-supersecret1234"
	cfg.Credentials = []CredentialPolicy{{
		Alias:        "linear",
		OPRef:        "op://Fleet-Agent/Linear/token",
		AllowedHosts: []string{"api.linear.app"},
	}}
	if err := SaveConfig(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	view := loaded.View(filepath.Join(tmp, "vault"))
	if !view.Configured || strings.Contains(view.APIKeyHint, "supersecret") {
		t.Fatalf("unexpected key hint: %+v", view)
	}
	if len(view.Credentials) != 1 || strings.Contains(view.Credentials[0].OPRefHint, "Linear") {
		t.Fatalf("credential op_ref leaked in view: %+v", view.Credentials)
	}

	eng, err := Open(filepath.Join(tmp, "assistant.db"), cfgPath, filepath.Join(tmp, "vault"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	initCfg := cfg
	initCfg.MCPServers = []MCPServerConfig{{
		Name:    "gcal",
		Command: "npx",
		Env:     map[string]string{"GCAL_SECRET": "real-token-value-12345"},
		Enabled: true,
	}}
	view1, err := eng.UpdateConfig(initCfg, false)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate saving Settings modal: Credentials omitted (nil) and MCPServers carries masked env values from ConfigView.
	upd := DefaultConfig()
	upd.Enabled = true
	upd.MCPServers = view1.MCPServers
	upd.Models.Aliases = map[string]string{
		"astra":  "openai/gpt-6-astra",
		"custom": "meta/muse-spark-open",
	}
	if _, err := eng.UpdateConfig(upd, false); err != nil {
		t.Fatal(err)
	}
	got := eng.Config()
	if len(got.Credentials) != 1 || got.Credentials[0].Alias != "linear" {
		t.Fatalf("expected omitted credentials preserved, got %+v", got.Credentials)
	}
	if len(got.MCPServers) != 1 || got.MCPServers[0].Env["GCAL_SECRET"] != "real-token-value-12345" {
		t.Fatalf("expected masked MCP env secret preserved, got %+v", got.MCPServers)
	}
	if got.Models.Aliases["astra"] != "openai/gpt-6.2-astra" || got.Models.Aliases["custom"] != "meta/muse-spark-open" {
		t.Fatalf("expected legacy alias upgraded and custom alias kept, got %+v", got.Models.Aliases)
	}
}

func TestVaultSafetySkillsAndSearch(t *testing.T) {
	tmp := t.TempDir()
	v, err := OpenVault(filepath.Join(tmp, "vault"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../escape.md", "/etc/passwd", "Projects/../../bad.md"} {
		if _, _, err := v.ResolveRel(bad); err == nil {
			t.Fatalf("expected path escape error for %q", bad)
		}
	}
	outside := filepath.Join(tmp, "outside.md")
	_ = os.WriteFile(outside, []byte("secret"), 0o600)
	_ = os.Symlink(outside, filepath.Join(v.Root, "Daily", "link.md"))
	if _, _, err := v.ResolveRel("Daily/link.md"); err == nil {
		t.Fatalf("expected file symlink escape rejection")
	}
	outsideDir := filepath.Join(tmp, "extdir")
	_ = os.MkdirAll(outsideDir, 0o755)
	_ = os.Symlink(outsideDir, filepath.Join(v.Root, "ext"))
	if _, _, err := v.ResolveRel("ext/secret.md"); err == nil {
		t.Fatalf("expected symlink dir escape rejection")
	}

	skillBody := "---\nname: morning-briefing\ndescription: Summarize waiting agents and calendar\nmodel: openai/gpt-6.2-astra\ntags: [daily, fleet]\n---\n# Steps\n1. Check agents.\n"
	sk, err := v.SaveSkill("morning-briefing", skillBody)
	if err != nil {
		t.Fatal(err)
	}
	if sk.Model != "openai/gpt-6.2-astra" || len(sk.Tags) != 2 {
		t.Fatalf("unexpected parsed frontmatter: %+v", sk)
	}
	skills, err := v.ListSkills()
	if err != nil || len(skills) != 2 {
		t.Fatalf("expected 2 skills (scaffolded + morning-briefing), got %d (%v)", len(skills), err)
	}
	lhSkill, err := v.GetSkill("lighthouse-self-config")
	if err != nil || !strings.Contains(lhSkill.Content, "fleet_spawn_agent") {
		t.Fatalf("expected scaffolded lighthouse-self-config skill, got %+v (%v)", lhSkill, err)
	}
	// User edits to scaffolded skills must be preserved across EnsureScaffold.
	customSkill := lhSkill.Content + "\n- Custom note\n"
	if _, err := v.SaveSkill("lighthouse-self-config", customSkill); err != nil {
		t.Fatal(err)
	}
	if err := v.EnsureScaffold(); err != nil {
		t.Fatal(err)
	}
	if after, err := v.GetSkill("lighthouse-self-config"); err != nil || !strings.Contains(after.Content, "Custom note") {
		t.Fatalf("expected user edits to lighthouse-self-config preserved, got %+v (%v)", after, err)
	}
	hits, err := v.Search("calendar", 10)
	if err != nil || len(hits) == 0 {
		t.Fatalf("expected vault search hit, got %+v (%v)", hits, err)
	}
	if !IsAutoWritable("Daily/2026-11-27.md", DefaultConfig().AutoWriteDirs) {
		t.Fatalf("Daily/ should be auto-writable")
	}
	if IsAutoWritable("Projects/RoadMap.md", DefaultConfig().AutoWriteDirs) {
		t.Fatalf("Projects/ must require approval")
	}
}

func TestCredentialProxyAllowlistAndScrubbing(t *testing.T) {
	secret := "lin_api_topsecret_xyz789"
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth := r.Header.Get("Authorization")
		_, _ = fmt.Fprintf(w, `{"ok":true,"echo":%q}`, gotAuth)
	}))
	defer targetSrv.Close()

	cfg := DefaultConfig()
	cfg.Credentials = []CredentialPolicy{{
		Alias:        "demo",
		OPRef:        "op://Fleet-Agent/Demo/token",
		AllowedHosts: []string{"127.0.0.1"},
	}}
	reader := func(_ context.Context, _ string) (string, error) { return secret, nil }

	if _, err := ExecuteCredentialHTTP(context.Background(), cfg, reader, targetSrv.Client(), CredentialHTTPRequest{
		Alias: "demo", URL: "https://evil.example.com/steal",
	}); err == nil {
		t.Fatalf("expected disallowed host to fail")
	}

	res, err := ExecuteCredentialHTTP(context.Background(), cfg, reader, targetSrv.Client(), CredentialHTTPRequest{
		Alias: "demo", Method: http.MethodGet, URL: targetSrv.URL + "/check",
	})
	if err != nil {
		t.Fatalf("expected allowlisted call to succeed: %v", err)
	}
	body, _ := res["body"].(string)
	if strings.Contains(body, secret) || !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("expected secret scrubbed from response body, got %q", body)
	}
}

func TestMCPManagerStdioLifecycle(t *testing.T) {
	t.Setenv("GO_WANT_MCP_HELPER", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewMCPManager([]MCPServerConfig{{
		Name:    "helper",
		Command: exe,
		Args:    []string{"-test.run=^TestHelperMCPProcess$"},
		EnvRefs: map[string]string{"TEST_MCP_TOKEN": "op://Fleet-Agent/Helper/token"},
		Enabled: true,
	}}, func(_ context.Context, _ string) (string, error) {
		return "injected-secret-99", nil
	})
	defer mgr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tools := mgr.ListTools(ctx)
	if len(tools) != 1 || tools[0].FullName != "mcp__helper__echo_secret" {
		t.Fatalf("unexpected MCP tools: %+v", tools)
	}
	res, err := mgr.CallTool(ctx, "mcp__helper__echo_secret", map[string]any{"msg": "ping"})
	if err != nil {
		t.Fatalf("MCP CallTool failed: %v", err)
	}
	m, _ := res.(map[string]any)
	if m["msg"] != "ping" || m["has_env"] != true {
		t.Fatalf("unexpected MCP result: %+v", res)
	}
}

func TestEngineStreamingRoutingApprovalsAndObservationalMemory(t *testing.T) {
	var seenModels []string
	var sawCacheControl atomic.Bool
	var callStep atomic.Int32

	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/alpha/decisions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"answers":{"tier":{"type":"choice","choice":"frontier","confidence":0.94}}}`))
		case "/chat/completions":
			var req struct {
				Model    string            `json:"model"`
				Stream   bool              `json:"stream"`
				Messages []json.RawMessage `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			seenModels = append(seenModels, req.Model)
			if len(req.Messages) > 0 && strings.Contains(string(req.Messages[0]), `"cache_control"`) {
				sawCacheControl.Store(true)
			}
			if !req.Stream {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"- 🔴 User tests Observational Memory"}}]}`))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			step := callStep.Add(1)
			if step == 1 {
				// First chat turn triggers protected vault_write -> pauses for HITL approval.
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tc_1\",\"type\":\"function\",\"function\":{\"name\":\"vault_write\",\"arguments\":\"{\\\"path\\\":\\\"Projects/Plan.md\\\",\\\"content\\\":\\\"# Plan\\\\nApproved!\\\"}\"}}]}}]}\n\n"))
				_, _ = w.Write([]byte("data: {\"choices\":[{\"finish_reason\":\"tool_calls\",\"delta\":{}}],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":20,\"cost\":0.0012,\"prompt_tokens_details\":{\"cached_tokens\":800}}}\n\n"))
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				return
			}
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Saved Projects/Plan.md.\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}],\"usage\":{\"prompt_tokens\":1200,\"completion_tokens\":15,\"cost\":0.0021,\"prompt_tokens_details\":{\"cached_tokens\":900}}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}
	}))
	defer mockLLM.Close()

	tmp := t.TempDir()
	deps := &fakeDeps{}
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), deps)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.BaseURL = mockLLM.URL
	cfg.APIKey = "sk-or-test"
	if _, err := eng.UpdateConfig(cfg, false); err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	eng.Start(ctx)

	sub := eng.Subscribe()
	defer eng.Unsubscribe(sub)

	if _, _, err := eng.EnqueueUserMessage("general", "/astra Update Projects/Plan.md", ""); err != nil {
		t.Fatal(err)
	}

	// Wait for deferred approval SSE event.
	var aprID string
	deadline := time.After(5 * time.Second)
	for aprID == "" {
		select {
		case ev := <-sub:
			if ev.Type == "approval" {
				apr, _ := ev.Data.(Approval)
				aprID = apr.ID
			}
		case <-deadline:
			t.Fatalf("timed out waiting for approval SSE event")
		}
	}
	if len(seenModels) == 0 || seenModels[0] != "openai/gpt-6.2-astra" {
		t.Fatalf("expected /astra override model openai/gpt-6.2-astra, got %v", seenModels)
	}
	if !sawCacheControl.Load() {
		t.Fatalf("expected cache_control breakpoint in system prompt")
	}

	// Approve and wait for resumed execution.
	if _, err := eng.DecideApproval(aprID, true, "looks good"); err != nil {
		t.Fatal(err)
	}
	deadline2 := time.After(5 * time.Second)
	var doneContent string
	for doneContent == "" {
		select {
		case ev := <-sub:
			if ev.Type == "message_done" {
				m, _ := ev.Data.(Message)
				if strings.Contains(m.Content, "Saved Projects/Plan.md") {
					doneContent = m.Content
				}
			}
		case <-deadline2:
			t.Fatalf("timed out waiting for resumed assistant reply")
		}
	}
	v, _ := eng.Vault()
	planText, err := v.ReadFile("Projects/Plan.md")
	if err != nil || !strings.Contains(planText, "Approved!") {
		t.Fatalf("expected approved write to Projects/Plan.md, got %q (%v)", planText, err)
	}

	// Verify Observational Memory + Reflector.
	obsRes, err := eng.RunObserver(ctx, "general")
	if err != nil || obsRes["observed"].(int) == 0 {
		t.Fatalf("RunObserver failed: %+v (%v)", obsRes, err)
	}
	obsText, _ := v.ReadFile("_agent/memory/OBSERVATIONS.md")
	if !strings.Contains(obsText, "Observational Memory") {
		t.Fatalf("expected distilled observation in OBSERVATIONS.md, got %q", obsText)
	}
	refl, err := eng.RunReflector(ctx)
	if err != nil || refl == "" {
		t.Fatalf("RunReflector failed: %q (%v)", refl, err)
	}

	// ResetChannel with clearHistory=false bumps epoch while retaining messages;
	// clearHistory=true clears channel messages & cancels pending approvals while preserving Vault memory.
	if _, err := eng.Store().AppendMessage(Message{ChannelID: "general", ContextEpoch: 1, Role: "user", Content: "keep temporarily"}); err != nil {
		t.Fatal(err)
	}
	chKeep, err := eng.ResetChannel("general", false)
	if err != nil || chKeep.ContextEpoch != 2 {
		t.Fatalf("expected epoch=2, got %+v (%v)", chKeep, err)
	}
	if msgs, err := eng.Store().ListMessages("general", 50); err != nil || len(msgs) == 0 {
		t.Fatalf("expected messages kept when clearHistory=false, got %d (%v)", len(msgs), err)
	}
	if _, err := eng.Store().CreateApproval(Approval{ChannelID: "general", ToolName: "vault_write", ArgsJSON: "{}", Summary: "pending test"}); err != nil {
		t.Fatal(err)
	}
	chClear, err := eng.ResetChannel("general", true)
	if err != nil || chClear.ContextEpoch != 3 {
		t.Fatalf("expected epoch=3, got %+v (%v)", chClear, err)
	}
	if msgs, err := eng.Store().ListMessages("general", 50); err != nil || len(msgs) != 0 {
		t.Fatalf("expected channel messages cleared when clearHistory=true, got %d (%v)", len(msgs), err)
	}
	if pend, err := eng.Store().ListApprovals("pending", 10); err != nil || len(pend) != 0 {
		t.Fatalf("expected pending approvals cancelled on clearHistory=true, got %+v (%v)", pend, err)
	}
	if reflAfter, rErr := v.ReadFile("_agent/memory/REFLECTIONS.md"); rErr != nil || !strings.Contains(reflAfter, "Observational Memory") {
		t.Fatalf("expected long-term REFLECTIONS.md preserved across channel reset, got %q (%v)", reflAfter, rErr)
	}
}

func TestSaveAndResolveOPToken(t *testing.T) {
	tmp := t.TempDir()
	tokPath := filepath.Join(tmp, "op-service-account.token")
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "")
	if got := ResolveOPToken(tokPath); got != "" {
		t.Fatalf("expected empty token before save, got %q", got)
	}
	if err := SaveOPToken(tokPath, "ops_test_token_123"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(tokPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 permissions, got %o", fi.Mode().Perm())
	}
	if got := ResolveOPToken(tokPath); got != "ops_test_token_123" {
		t.Fatalf("expected resolved file token, got %q", got)
	}
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "ops_env_override")
	if got := ResolveOPToken(tokPath); got != "ops_env_override" {
		t.Fatalf("expected env override, got %q", got)
	}
	if err := SaveOPToken(tokPath, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tokPath); !os.IsNotExist(err) {
		t.Fatalf("expected token file removed, stat err=%v", err)
	}
}

func TestDoctorAndAPIKeyOPRef(t *testing.T) {
	var seenAuth string
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
		case "/api/alpha/decisions":
			var dReq struct {
				Model     string                    `json:"model"`
				State     map[string]any            `json:"state"`
				Questions map[string]map[string]any `json:"questions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&dReq)
			if dReq.State["input"] == "" || dReq.Questions["tier"]["criteria"] == nil {
				http.Error(w, "invalid decisions payload", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"answers":{"tier":{"type":"choice","choice":"fast","confidence":0.95,"probabilities":{"fast":0.95,"balanced":0.05}}}}`))
		}
	}))
	defer mockLLM.Close()

	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "assistant.json")
	vaultDir := filepath.Join(tmp, "vault")
	eng, err := Open(filepath.Join(tmp, "assistant.db"), cfgPath, vaultDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.BaseURL = mockLLM.URL
	cfg.APIKeyOPRef = "op://Fleet-Agent/OpenRouter/credential"
	view, err := eng.UpdateConfig(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Configured || view.APIKeyHint != "" || view.APIKeyOPRef != cfg.APIKeyOPRef {
		t.Fatalf("unexpected view for api_key_op_ref: %+v", view)
	}
	rawCfg, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(rawCfg), "resolved-or-secret") {
		t.Fatalf("resolved secret must never be written to assistant.json")
	}

	eng.SetOPReader(func(_ context.Context, ref string) (string, error) {
		if ref != cfg.APIKeyOPRef {
			return "", fmt.Errorf("unexpected ref %s", ref)
		}
		return "resolved-or-secret", nil
	})
	_ = os.MkdirAll(filepath.Join(vaultDir, ".obsidian"), 0o755)
	eng.Probes = DoctorProbes{
		FindOB:    func() string { return "/opt/homebrew/bin/ob" },
		OBRunning: func() bool { return true },
		CheckOP: func(context.Context, string) (string, bool, string, error) {
			return "/opt/homebrew/bin/op", true, "fleet-sa", nil
		},
	}

	rep := eng.Doctor(t.Context())
	if !rep.OK {
		t.Fatalf("expected doctor OK=true, got %+v", rep)
	}
	if seenAuth != "Bearer resolved-or-secret" {
		t.Fatalf("expected resolved OP key in Authorization header, got %q", seenAuth)
	}
	statuses := map[string]string{}
	for _, c := range rep.Checks {
		statuses[c.Name] = c.Status
	}
	for _, k := range []string{"config", "openrouter", "decisions", "vault", "obsidian_sync", "onepassword", "mcp_servers"} {
		if statuses[k] != "ok" {
			t.Fatalf("expected check %s=ok, got %q (%+v)", k, statuses[k], rep.Checks)
		}
	}

	// Also verify Decisions probabilities parsing supports object, nested-object, and array shapes.
	if p := parseDecisionProbabilities(json.RawMessage(`{"fast":0.9,"balanced":0.1}`)); p["fast"] != 0.9 {
		t.Fatalf("expected flat object probabilities parsed, got %+v", p)
	}
	if p := parseDecisionProbabilities(json.RawMessage(`{"fast":{"probability":0.85}}`)); p["fast"] != 0.85 {
		t.Fatalf("expected nested object probabilities parsed, got %+v", p)
	}
	if p := parseDecisionProbabilities(json.RawMessage(`[{"key":"frontier","probability":0.7}]`)); p["frontier"] != 0.7 {
		t.Fatalf("expected array probabilities parsed, got %+v", p)
	}

	// Missing op auth when api_key_op_ref is set must fail doctor on onepassword & openrouter.
	eng.SetOPReader(func(context.Context, string) (string, error) { return "", fmt.Errorf("no token") })
	if err := eng.SaveOPServiceAccountToken(""); err != nil {
		t.Fatal(err)
	}
	eng.Probes.CheckOP = func(context.Context, string) (string, bool, string, error) {
		return "/opt/homebrew/bin/op", false, "", fmt.Errorf("no token")
	}
	repFail := eng.Doctor(t.Context())
	if repFail.OK {
		t.Fatalf("expected doctor OK=false when op fails for api_key_op_ref")
	}
}

func TestOPMetadataRedactionAndPromptAwareness(t *testing.T) {
	rawItem := []byte(`{
		"id": "item123",
		"title": "Linear API",
		"category": "API_CREDENTIAL",
		"vault": {"id": "v1", "name": "FleetAgent"},
		"urls": [{"href": "https://api.linear.app/graphql"}],
		"tags": ["ops"],
		"fields": [
			{"id": "credential", "label": "credential", "type": "CONCEALED", "value": "lin_api_SECRET_VALUE_DO_NOT_LEAK", "reference": "op://FleetAgent/Linear API/credential"}
		]
	}`)
	meta, err := parseOPItemDetailMeta(rawItem)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(meta)
	if strings.Contains(string(encoded), "SECRET_VALUE_DO_NOT_LEAK") {
		t.Fatalf("secret leaked in OPItemDetailMeta: %s", encoded)
	}
	if len(meta.Fields) != 1 || meta.Fields[0].Reference != "op://FleetAgent/Linear API/credential" {
		t.Fatalf("expected op:// reference preserved, got %+v", meta.Fields)
	}

	tmp := t.TempDir()
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	eng.Probes.ListVaults = func(context.Context, string) ([]OPVaultMeta, error) {
		return []OPVaultMeta{{ID: "v1", Name: "FleetAgent"}}, nil
	}
	msgs := eng.buildPromptMessages(t.Context(), Channel{ID: "general"}, nil, DefaultAliases())
	rawPrompt, _ := json.Marshal(msgs[0])
	if !strings.Contains(string(rawPrompt), "FleetAgent") || !strings.Contains(string(rawPrompt), "onepassword_list_items") {
		t.Fatalf("expected 1Password Agent Vault section in system prompt, got %s", rawPrompt)
	}
}

func TestMultiVaultToolsPolicyAndDoctor(t *testing.T) {
	tmp := t.TempDir()
	agentDir := filepath.Join(tmp, "agent-vault")
	personalDir := filepath.Join(tmp, "personal-vault")
	roDir := filepath.Join(tmp, "ref-vault")

	if err := os.MkdirAll(filepath.Join(personalDir, "Notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(personalDir, "Notes", "Idea.md"), []byte("# Espresso\nDial in grinder\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(roDir, "Handbook"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roDir, "Handbook", "Policy.md"), []byte("# Espresso Policy\nClean portafilter\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), agentDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	for _, bad := range []string{"agent", "all", "Bad Name!"} {
		badCfg := DefaultConfig()
		badCfg.ExtraVaults = []VaultMount{{Name: bad, Path: personalDir}}
		if _, uErr := eng.UpdateConfig(badCfg, false); uErr == nil {
			t.Fatalf("expected invalid vault name error for %q", bad)
		}
	}

	cfg := DefaultConfig()
	cfg.ExtraVaults = []VaultMount{
		{Name: "personal", Path: personalDir, AutoWriteDirs: []string{"Inbox"}},
		{Name: "ref", Path: roDir, ReadOnly: true},
	}
	view, err := eng.UpdateConfig(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.ExtraVaults) != 2 {
		t.Fatalf("expected 2 extra vaults in view, got %+v", view.ExtraVaults)
	}

	// Extra vaults must never be scaffolded with _agent or _skills.
	for _, forbidden := range []string{"_agent", "_skills", "Daily"} {
		if _, sErr := os.Stat(filepath.Join(personalDir, forbidden)); !os.IsNotExist(sErr) {
			t.Fatalf("extra vault must not contain %s", forbidden)
		}
	}

	// UpdateConfig with nil ExtraVaults must preserve existing mounts.
	upd := DefaultConfig()
	upd.Enabled = true
	if _, err := eng.UpdateConfig(upd, false); err != nil {
		t.Fatal(err)
	}
	if len(eng.Config().ExtraVaults) != 2 {
		t.Fatalf("expected omitted ExtraVaults preserved, got %+v", eng.Config().ExtraVaults)
	}

	// Cross-vault search and single-vault search tag hits by vault name.
	allHits, err := eng.SearchVaults("all", "Espresso", 10)
	if err != nil || len(allHits) != 2 {
		t.Fatalf("expected 2 hits across extra vaults, got %+v (%v)", allHits, err)
	}
	personalHits, err := eng.SearchVaults("personal", "Espresso", 10)
	if err != nil || len(personalHits) != 1 || personalHits[0].Vault != "personal" {
		t.Fatalf("expected 1 personal hit, got %+v (%v)", personalHits, err)
	}

	tctx := &toolExecContext{ChannelID: "general", Source: "chat", Config: eng.Config()}

	// vault_read from extra vault + traversal check.
	readOut, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "r1",
		Type:     "function",
		Function: ToolCallFunction{Name: "vault_read", Arguments: `{"vault":"personal","path":"Notes/Idea.md"}`},
	}, tctx)
	if err != nil || !strings.Contains(readOut, "Dial in grinder") {
		t.Fatalf("expected personal vault read, got %q (%v)", readOut, err)
	}
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "r2",
		Type:     "function",
		Function: ToolCallFunction{Name: "vault_read", Arguments: `{"vault":"personal","path":"../agent-vault/_agent/SOUL.md"}`},
	}, tctx); err == nil {
		t.Fatalf("expected traversal out of personal vault to fail")
	}

	// Read-only vault rejects writes.
	_, roErr := eng.executeTool(t.Context(), ToolCall{
		ID:       "w_ro",
		Type:     "function",
		Function: ToolCallFunction{Name: "vault_write", Arguments: `{"vault":"ref","path":"Handbook/New.md","content":"nope"}`},
	}, tctx)
	if roErr == nil {
		t.Fatalf("expected read-only vault write error")
	}

	// Writable extra vault auto-writes inside Inbox/.
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "w_inbox",
		Type:     "function",
		Function: ToolCallFunction{Name: "vault_write", Arguments: `{"vault":"personal","path":"Inbox/Capture.md","content":"hello"}`},
	}, tctx); err != nil {
		t.Fatalf("expected Inbox write in personal vault without approval, got %v", err)
	}

	// Write outside auto_write_dirs triggers HITL approval and resumes into personal vault.
	protArgs := `{"vault":"personal","path":"Notes/Project.md","content":"# Project"}`
	_, protErr := eng.executeTool(t.Context(), ToolCall{
		ID:       "w_prot",
		Type:     "function",
		Function: ToolCallFunction{Name: "vault_write", Arguments: protArgs},
	}, tctx)
	aprErr, ok := errors.AsType[*approvalRequiredError](protErr)
	if !ok || !strings.Contains(aprErr.Summary, "personal:Notes/Project.md") {
		t.Fatalf("expected approvalRequiredError for personal:Notes/Project.md, got %v", protErr)
	}
	tctxApproved := &toolExecContext{ChannelID: "general", Source: "approval", Config: eng.Config(), BypassHITL: true}
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "w_prot_ok",
		Type:     "function",
		Function: ToolCallFunction{Name: "vault_write", Arguments: aprErr.ArgsJSON},
	}, tctxApproved); err != nil {
		t.Fatalf("expected approved write to personal vault to succeed, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(personalDir, "Notes", "Project.md")); err != nil {
		t.Fatalf("expected Notes/Project.md in personal vault: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agentDir, "Notes", "Project.md")); !os.IsNotExist(err) {
		t.Fatalf("write leaked into primary agent vault")
	}

	// System prompt announces mounted vaults and policies.
	msgs := eng.buildPromptMessages(t.Context(), Channel{ID: "general"}, nil, DefaultAliases())
	rawPrompt, _ := json.Marshal(msgs[0])
	if !strings.Contains(string(rawPrompt), "Mounted Obsidian Vaults") || !strings.Contains(string(rawPrompt), "personal") {
		t.Fatalf("expected Mounted Obsidian Vaults section in prompt, got %s", rawPrompt)
	}

	// Doctor checks primary + extra vaults and named sync status.
	_ = os.MkdirAll(filepath.Join(agentDir, ".obsidian"), 0o755)
	_ = os.MkdirAll(filepath.Join(personalDir, ".obsidian"), 0o755)
	_ = os.MkdirAll(filepath.Join(roDir, ".obsidian"), 0o755)
	eng.Probes = DoctorProbes{
		FindOB: func() string { return "/opt/homebrew/bin/ob" },
		OBRunningFor: func(name, _ string) bool {
			return name == "" || name == "personal"
		},
	}
	rep := eng.Doctor(t.Context())
	byName := map[string]DoctorCheck{}
	for _, c := range rep.Checks {
		byName[c.Name] = c
	}
	if byName["vault:personal"].Status != "ok" || byName["obsidian_sync:personal"].Status != "ok" {
		t.Fatalf("expected personal vault and sync ok, got %+v", byName)
	}
	if byName["vault:ref"].Status != "ok" || byName["obsidian_sync:ref"].Status != "warn" {
		t.Fatalf("expected ref vault ok and sync warn, got %+v", byName)
	}
}

func TestGenerativeUIPromptAndValidation(t *testing.T) {
	tmp := t.TempDir()
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	msgs := eng.buildPromptMessages(t.Context(), Channel{ID: "general"}, nil, DefaultAliases())
	rawPrompt, _ := json.Marshal(msgs[0])
	prompt := string(rawPrompt)
	for _, want := range []string{"render_card", "render_widget", "data-fleet-action", "--accent"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("expected %q in system prompt, got %s", want, prompt)
		}
	}

	var widgets []WidgetPayload
	tctx := &toolExecContext{ChannelID: "general", Source: "chat", Config: eng.Config(), Widgets: &widgets}

	// render_card(kind="markdown") normalizes string data to {"markdown": ...}.
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "ui1",
		Type:     "function",
		Function: ToolCallFunction{Name: "render_card", Arguments: `{"kind":"markdown","title":"Summary","data":"**Done**"}`},
	}, tctx); err != nil {
		t.Fatalf("render_card markdown failed: %v", err)
	}
	if len(widgets) != 1 || widgets[0].Kind != "markdown" {
		t.Fatalf("expected 1 markdown widget, got %+v", widgets)
	}
	dataMap, ok := widgets[0].Data.(map[string]any)
	if !ok || dataMap["markdown"] != "**Done**" {
		t.Fatalf("expected normalized markdown map, got %+v", widgets[0].Data)
	}

	// Unsupported render_card kind fails validation.
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "ui2",
		Type:     "function",
		Function: ToolCallFunction{Name: "render_card", Arguments: `{"kind":"unknown_card","data":{}}`},
	}, tctx); err == nil {
		t.Fatalf("expected unsupported render_card kind to fail")
	}

	// Empty render_widget html fails validation.
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "ui3",
		Type:     "function",
		Function: ToolCallFunction{Name: "render_widget", Arguments: `{"title":"Empty","html":"   "}`},
	}, tctx); err == nil {
		t.Fatalf("expected empty widget html to fail")
	}

	// External CDN script, stylesheet link, and CSS @import fail validation.
	for i, badHTML := range []string{
		`<canvas id="c"></canvas><script src="https://cdn.jsdelivr.net/npm/chart.js"></script>`,
		`<link rel="stylesheet" href="https://example.com/a.css"><div>Hi</div>`,
		`<style>@import url("https://example.com/a.css");</style><div>Hi</div>`,
	} {
		argsJSON, _ := json.Marshal(map[string]string{"title": "Bad", "html": badHTML})
		if _, err := eng.executeTool(t.Context(), ToolCall{
			ID:       fmt.Sprintf("ui_bad_%d", i),
			Type:     "function",
			Function: ToolCallFunction{Name: "render_widget", Arguments: string(argsJSON)},
		}, tctx); err == nil {
			t.Fatalf("expected external resource widget %d to fail: %s", i, badHTML)
		}
	}

	// Markdown code fence around HTML is stripped; inline SVG xmlns, canvas, and JSON data scripts succeed.
	fencedHTML := "```html\n<svg xmlns=\"http://www.w3.org/2000/svg\" viewBox=\"0 0 200 100\"><rect width=\"50\" height=\"80\"/></svg><canvas id=\"c\"></canvas><script type=\"application/json\">{\"v\":[1,2]}</script><script>const c=document.getElementById('c');</script>\n```"
	goodArgs, _ := json.Marshal(map[string]string{"title": "Chart", "html": fencedHTML})
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "ui4",
		Type:     "function",
		Function: ToolCallFunction{Name: "render_widget", Arguments: string(goodArgs)},
	}, tctx); err != nil {
		t.Fatalf("expected fenced inline chart widget to succeed: %v", err)
	}
	if len(widgets) != 2 || widgets[1].Kind != "html" || strings.HasPrefix(widgets[1].HTML, "```") || !strings.HasPrefix(widgets[1].HTML, "<svg ") {
		t.Fatalf("expected stripped inline widget html, got %+v", widgets)
	}
}

func TestHTTPMCPTransportAndSelfSetupTools(t *testing.T) {
	secret := "http-mcp-secret-999"
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			http.Error(w, "unauthorized got="+r.Header.Get("Authorization"), http.StatusUnauthorized)
			return
		}
		var req struct {
			ID     int64          `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-http-1")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2024-11-05"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			if r.Header.Get("Mcp-Session-Id") != "sess-http-1" {
				http.Error(w, "missing Mcp-Session-Id", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"tools\":[{\"name\":\"list_events\",\"description\":\"List calendar events\"}]}}\n\n", req.ID)
		case "tools/call":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"events": []string{"Standup"}}})
		}
	}))
	defer mcpSrv.Close()

	tmp := t.TempDir()
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	eng.SetOPReader(func(_ context.Context, ref string) (string, error) {
		if ref == "op://Fleet-Agent/GCal/token" {
			return "Bearer " + secret, nil
		}
		return "", fmt.Errorf("unexpected ref %q", ref)
	})

	tctx := &toolExecContext{ChannelID: "general", Source: "chat", Config: eng.Config()}
	// Rejects shell interpreter during self-setup.
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "m1",
		Type:     "function",
		Function: ToolCallFunction{Name: "mcp_configure", Arguments: `{"name":"bad","transport":"stdio","command":"/bin/sh","args":["-c","id"]}`},
	}, tctx); err == nil {
		t.Fatalf("expected shell command to be rejected by mcp_configure")
	}
	// Rejects plaintext secret in headers during self-setup.
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "m2",
		Type:     "function",
		Function: ToolCallFunction{Name: "mcp_configure", Arguments: fmt.Sprintf(`{"name":"gcal","transport":"http","url":%q,"headers":{"Authorization":"Bearer raw"}}`, mcpSrv.URL)},
	}, tctx); err == nil {
		t.Fatalf("expected plaintext Authorization header to be rejected by mcp_configure")
	}

	// Valid mcp_configure pauses for HITL approval.
	cfgArgs := fmt.Sprintf(`{"name":"gcal","transport":"http","url":%q,"header_refs":{"Authorization":"op://Fleet-Agent/GCal/token"}}`, mcpSrv.URL)
	_, err = eng.executeTool(t.Context(), ToolCall{
		ID:       "m3",
		Type:     "function",
		Function: ToolCallFunction{Name: "mcp_configure", Arguments: cfgArgs},
	}, tctx)
	aprErr, ok := errors.AsType[*approvalRequiredError](err)
	if !ok || aprErr.RiskLevel != "mcp_config" {
		t.Fatalf("expected approvalRequiredError for mcp_configure, got %v", err)
	}

	// Approving saves config, defaults require_approval_tools=["*"] and tests HTTP+SSE handshake.
	tctxApproved := &toolExecContext{ChannelID: "general", Source: "approval", Config: eng.Config(), BypassHITL: true}
	outStr, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "m3-ok",
		Type:     "function",
		Function: ToolCallFunction{Name: "mcp_configure", Arguments: aprErr.ArgsJSON},
	}, tctxApproved)
	if err != nil {
		t.Fatalf("approved mcp_configure failed: %v", err)
	}
	if !strings.Contains(outStr, "list_events") {
		t.Fatalf("expected discovered tool list_events in output, got %s", outStr)
	}
	if len(eng.Config().MCPServers) != 1 || len(eng.Config().MCPServers[0].RequireApprovalTools) != 1 || eng.Config().MCPServers[0].RequireApprovalTools[0] != "*" {
		t.Fatalf("expected default require_approval_tools=[\"*\"], got %+v", eng.Config().MCPServers)
	}

	// mcp_list and mcp_test inspect live server; calling mcp__gcal__list_events requires approval due to ["*"].
	listOut, err := eng.executeTool(t.Context(), ToolCall{ID: "ml", Type: "function", Function: ToolCallFunction{Name: "mcp_list", Arguments: `{}`}}, tctx)
	if err != nil || !strings.Contains(listOut, "list_events") {
		t.Fatalf("mcp_list failed: %v (%s)", err, listOut)
	}
	testOut, err := eng.executeTool(t.Context(), ToolCall{ID: "mt", Type: "function", Function: ToolCallFunction{Name: "mcp_test", Arguments: `{"name":"gcal"}`}}, tctx)
	if err != nil || !strings.Contains(testOut, `"tool_count":1`) {
		t.Fatalf("mcp_test failed: %v (%s)", err, testOut)
	}
	_, callErr := eng.executeTool(t.Context(), ToolCall{ID: "mc", Type: "function", Function: ToolCallFunction{Name: "mcp__gcal__list_events", Arguments: `{}`}}, tctx)
	if _, isApr := errors.AsType[*approvalRequiredError](callErr); !isApr {
		t.Fatalf("expected approvalRequiredError for newly configured MCP tool, got %v", callErr)
	}
	resOut, err := eng.executeTool(t.Context(), ToolCall{ID: "mc-ok", Type: "function", Function: ToolCallFunction{Name: "mcp__gcal__list_events", Arguments: `{}`}}, tctxApproved)
	if err != nil || !strings.Contains(resOut, "Standup") {
		t.Fatalf("approved mcp__gcal__list_events failed: %v (%s)", err, resOut)
	}

	// Verify HTTP errors scrub resolved secret values.
	eng.SetOPReader(func(context.Context, string) (string, error) { return "Bearer wrong-" + secret, nil })
	if _, err := eng.mcp.TestServer(t.Context(), MCPServerConfig{
		Name: "gcal", Transport: "http", URL: mcpSrv.URL,
		HeaderRefs: map[string]string{"Authorization": "op://Fleet-Agent/GCal/token"},
	}); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("expected secret scrubbed from HTTP MCP error, got %v", err)
	}

	// mcp_remove cleans up server after approval.
	if _, err := eng.executeTool(t.Context(), ToolCall{ID: "mr", Type: "function", Function: ToolCallFunction{Name: "mcp_remove", Arguments: `{"name":"gcal"}`}}, tctxApproved); err != nil {
		t.Fatalf("mcp_remove failed: %v", err)
	}
	if len(eng.Config().MCPServers) != 0 {
		t.Fatalf("expected mcp_servers empty after remove, got %+v", eng.Config().MCPServers)
	}
}

func TestCronTimersDelegationLifecycleAndVaultGit(t *testing.T) {
	tmp := t.TempDir()
	deps := &fakeDeps{}
	vaultDir := filepath.Join(tmp, "vault")
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), vaultDir, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	// Cron evaluation for 8:00 AM weekdays: Friday 2026-11-27 09:00 -> Monday 2026-11-30 08:00.
	loc := time.UTC
	fri := time.Date(2026, 11, 27, 9, 0, 0, 0, loc)
	next, err := NextCronRun("0 8 * * 1-5", fri)
	if err != nil {
		t.Fatalf("NextCronRun failed: %v", err)
	}
	if next.Weekday() != time.Monday || next.Day() != 30 || next.Hour() != 8 || next.Minute() != 0 {
		t.Fatalf("unexpected next weekday run: %v", next)
	}
	if _, err := eng.store.UpsertTimer(Timer{
		ChannelID: "briefing", Name: "weekday-brief", Prompt: "Summarize", ScheduleKind: "cron", CronExpr: "0 8 * * 1-5", Enabled: true,
	}); err != nil {
		t.Fatalf("UpsertTimer cron failed: %v", err)
	}

	// Phase 3: fleet_list_paths, fleet_spawn_agent (approval -> watch), agent.watch event wakeup, fleet_kill_agent.
	tctx := &toolExecContext{ChannelID: "fleet-ops", Source: "chat", Config: eng.Config()}
	pathsOut, err := eng.executeTool(t.Context(), ToolCall{ID: "p1", Type: "function", Function: ToolCallFunction{Name: "fleet_list_paths", Arguments: `{"device":"laptop"}`}}, tctx)
	if err != nil || !strings.Contains(pathsOut, "/Users/datdo/workspace/fleet") {
		t.Fatalf("fleet_list_paths failed: %v (%s)", err, pathsOut)
	}
	spawnArgs := `{"harness":"claude","device":"laptop","name":"fix-auth","repo_path":"~/workspace/fleet","prompt":"Fix auth bug"}`
	_, spawnErr := eng.executeTool(t.Context(), ToolCall{ID: "sp1", Type: "function", Function: ToolCallFunction{Name: "fleet_spawn_agent", Arguments: spawnArgs}}, tctx)
	if _, isApr := errors.AsType[*approvalRequiredError](spawnErr); !isApr {
		t.Fatalf("expected approvalRequiredError for fleet_spawn_agent, got %v", spawnErr)
	}
	tctxApproved := &toolExecContext{ChannelID: "fleet-ops", Source: "approval", Config: eng.Config(), BypassHITL: true}
	spawnOut, err := eng.executeTool(t.Context(), ToolCall{ID: "sp1-ok", Type: "function", Function: ToolCallFunction{Name: "fleet_spawn_agent", Arguments: spawnArgs}}, tctxApproved)
	if err != nil || !strings.Contains(spawnOut, "job-spawn-1") || len(deps.watches) != 1 {
		t.Fatalf("approved fleet_spawn_agent failed: %v (%s watches=%+v)", err, spawnOut, deps.watches)
	}

	eng.HandleBusEvent(t.Context(), events.Event{
		Type:        "agent.watch",
		Severity:    "warn",
		Source:      "agents",
		Actor:       "laptop:claude:abc1",
		Subject:     "laptop:claude:abc1",
		ChannelHint: "fleet-ops",
		Title:       "Watched agent fix-auth: waiting_permission",
		Summary:     "Watched claude session fix-auth transitioned to waiting_permission.",
		Payload: map[string]any{
			"watch_id":       deps.watches[0].ID,
			"session_id":     "laptop:claude:abc1",
			"device":         "laptop",
			"harness":        "claude",
			"name":           "fix-auth",
			"new_state":      "waiting_permission",
			"last_assistant": "Ready to run tests",
		},
	})

	var foundWatchEvent bool
	for range 10 {
		ev, err := eng.store.ClaimNextInbox()
		if err == nil && ev != nil && ev.Source == "agent_watch" {
			foundWatchEvent = strings.Contains(ev.PayloadJSON, "Ready to run tests")
			break
		}
	}
	if !foundWatchEvent {
		t.Fatalf("expected agent_watch inbox event after HandleBusEvent(agent.watch)")
	}

	// Vault git history, diff, and rollback.
	for _, args := range [][]string{
		{"-C", vaultDir, "init"},
		{"-C", vaultDir, "config", "user.email", "test@fleet.local"},
		{"-C", vaultDir, "config", "user.name", "Fleet Test"},
		{"-C", vaultDir, "add", "-A"},
		{"-C", vaultDir, "commit", "-m", "initial scaffold"},
	} {
		if out, gErr := exec.Command("git", args...).CombinedOutput(); gErr != nil {
			t.Fatalf("git %v failed: %v (%s)", args, gErr, string(out))
		}
	}
	v, _ := eng.vault()
	if _, err := v.WriteFile("Inbox/note.md", "v1\n"); err != nil {
		t.Fatal(err)
	}
	v.MaybeGitCommit(true, "add note v1")
	if _, err := v.WriteFile("Inbox/note.md", "v2\n"); err != nil {
		t.Fatal(err)
	}
	v.MaybeGitCommit(true, "update note v2")

	hist, err := v.GitHistory(10)
	if err != nil || len(hist) < 3 {
		t.Fatalf("expected >= 3 git commits, got %+v (%v)", hist, err)
	}
	diff, err := v.GitDiff(hist[0].SHA)
	if err != nil || !strings.Contains(diff, "v2") {
		t.Fatalf("expected v2 in GitDiff, got %q (%v)", diff, err)
	}
	// Roll back to the "add note v1" commit (index 1).
	if _, err := v.GitRollback(hist[1].SHA); err != nil {
		t.Fatalf("GitRollback failed: %v", err)
	}
	gotNote, err := v.ReadFile("Inbox/note.md")
	if err != nil || strings.TrimSpace(gotNote) != "v1" {
		t.Fatalf("expected Inbox/note.md rolled back to v1, got %q (%v)", gotNote, err)
	}
}

func TestDiscordWebhookNotifierAndDoctor(t *testing.T) {
	var received []map[string]any
	discordSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		received = append(received, body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer discordSrv.Close()

	tmp := t.TempDir()
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	cfg := DefaultConfig()
	cfg.Notifications = NotificationsConfig{
		Enabled:             true,
		FleetBaseURL:        "https://fleet.example.com",
		DiscordWebhookOPRef: "op://Fleet-Agent/Discord/webhook",
		DiscordChannelOPRefs: map[string]string{
			"approvals": "op://Fleet-Agent/Discord-Approvals/webhook",
		},
		MinPriority: "normal",
	}
	if _, err := eng.UpdateConfig(cfg, false); err != nil {
		t.Fatal(err)
	}
	reader := func(_ context.Context, ref string) (string, error) {
		switch ref {
		case "op://Fleet-Agent/Discord/webhook", "op://Fleet-Agent/Discord-Approvals/webhook":
			return discordSrv.URL + "/api/webhooks/123/secret-tok", nil
		default:
			return "", fmt.Errorf("unknown ref %q", ref)
		}
	}
	eng.SetOPReader(reader)
	notifier := NewDiscordWebhookNotifier(cfg.Notifications, reader, nil)
	notifier.allowInsecureTestURL = true
	eng.notifyMu.Lock()
	eng.notifier = notifier
	eng.notifyMu.Unlock()

	if err := eng.TestNotification(t.Context()); err != nil {
		t.Fatalf("TestNotification failed: %v", err)
	}
	if len(received) != 1 {
		t.Fatalf("expected 1 Discord embed delivered, got %d", len(received))
	}
	rawEmbed, _ := json.Marshal(received[0])
	if !strings.Contains(string(rawEmbed), "https://fleet.example.com/#chat/general") || strings.Contains(string(rawEmbed), "secret-tok") {
		t.Fatalf("unexpected Discord embed payload: %s", rawEmbed)
	}

	// Verify URL validation rejects non-Discord webhook hosts and scrubs webhook URL from errors.
	if err := validateDiscordWebhookURL("https://evil.example.com/hook", false); err == nil {
		t.Fatalf("expected non-Discord host to be rejected")
	}
	dc := eng.checkNotificationsDoctor(t.Context(), Config{Notifications: NotificationsConfig{
		Enabled:             true,
		DiscordWebhookOPRef: "op://Fleet-Agent/Discord/webhook",
	}})
	// Mock reader returns http://127.0.0.1..., so production validator flags non-https discord.com without leaking secret-tok.
	if dc.Status != "fail" || strings.Contains(dc.Detail, "secret-tok") {
		t.Fatalf("expected doctor fail with scrubbed webhook token, got %+v", dc)
	}
	dcOK := eng.checkNotificationsDoctor(t.Context(), Config{Notifications: NotificationsConfig{
		Enabled:             true,
		DiscordWebhookOPRef: "op://Fleet-Agent/Discord/valid",
	}})
	_ = dcOK
	eng.SetOPReader(func(context.Context, string) (string, error) { return "https://discord.com/api/webhooks/999/tok", nil })
	dcPass := eng.checkNotificationsDoctor(t.Context(), Config{Notifications: NotificationsConfig{
		Enabled:             true,
		DiscordWebhookOPRef: "op://Fleet-Agent/Discord/valid",
	}})
	if dcPass.Status != "ok" {
		t.Fatalf("expected notifications doctor ok for valid https://discord.com/api/webhooks/ URL, got %+v", dcPass)
	}

	dcSkip := eng.checkNotificationsDoctor(t.Context(), Config{})
	if dcSkip.Status != "skip" {
		t.Fatalf("expected notifications doctor skip when webhook and discord bot are disabled, got %+v", dcSkip)
	}

	dcBot := eng.checkNotificationsDoctor(t.Context(), Config{
		Discord: DiscordConfig{
			Enabled:          true,
			BotTokenOPRef:    "op://Fleet-Agent/Discord/bot",
			AuthorizedUserID: "143190607408398336",
		},
	})
	if dcBot.Status != "ok" || !strings.Contains(dcBot.Detail, "two-way Discord bot") {
		t.Fatalf("expected notifications doctor ok via two-way Discord bot, got %+v", dcBot)
	}
}

type mockTestDecision struct {
	NeedsFrontier float64
	SameTopic     float64
	ToolCall      bool
	CostUSD       float64
}

type testNotifier func(context.Context, Notification) error

func (f testNotifier) Send(ctx context.Context, n Notification) error { return f(ctx, n) }

func TestHybridAutoRoutingTopicGrantsAndModes(t *testing.T) {
	def := DefaultConfig()
	if def.Routing.Mode != RoutingModeHybrid {
		t.Fatalf("expected default routing mode %q, got %q", RoutingModeHybrid, def.Routing.Mode)
	}
	orBody := buildDecisionsRequestBody(Config{
		BaseURL:        DefaultOpenRouterURL,
		ZDR:            true,
		DataCollection: "deny",
		Models:         ModelConfig{Decision: DefaultDecisionModel},
	}, map[string]any{"input": "hi"}, nil)
	prov, _ := orBody["provider"].(map[string]any)
	if prov["zdr"] != true || prov["data_collection"] != "deny" {
		t.Fatalf("expected ZDR provider policy on OpenRouter decisions request, got %+v", orBody)
	}

	var decisionCount atomic.Int32
	var nextDecision atomic.Value
	nextDecision.Store(mockTestDecision{NeedsFrontier: 0.15, SameTopic: 0.90, CostUSD: 0.01})
	var recordedChatReqs []struct {
		Model    string
		Messages []ChatMessage
	}

	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/alpha/decisions":
			decisionCount.Add(1)
			d := nextDecision.Load().(mockTestDecision)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w,
				`{"answers":{"needs_frontier":{"type":"noul","noul":%v},"same_topic":{"type":"noul","noul":%v}}}`,
				d.NeedsFrontier, d.SameTopic,
			)
		case "/chat/completions":
			var req struct {
				Model    string        `json:"model"`
				Messages []ChatMessage `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			recordedChatReqs = append(recordedChatReqs, struct {
				Model    string
				Messages []ChatMessage
			}{Model: req.Model, Messages: req.Messages})
			d := nextDecision.Load().(mockTestDecision)
			w.Header().Set("Content-Type", "text/event-stream")
			if d.ToolCall {
				// Trigger protected vault write on first call of tool test, then stop.
				nextDecision.Store(mockTestDecision{NeedsFrontier: d.NeedsFrontier, SameTopic: d.SameTopic, ToolCall: false, CostUSD: d.CostUSD})
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tc_f1\",\"type\":\"function\",\"function\":{\"name\":\"vault_write\",\"arguments\":\"{\\\"path\\\":\\\"Projects/Arch.md\\\",\\\"content\\\":\\\"# Arch\\\"}\"}}]}}]}\n\n"))
				_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"finish_reason\":\"tool_calls\",\"delta\":{}}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":10,\"cost\":%v}}\n\n", d.CostUSD)
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				return
			}
			lastUser := ""
			for _, m := range slices.Backward(req.Messages) {
				if m.Role == "user" {
					if s, ok := m.Content.(string); ok {
						lastUser = s
						break
					}
				}
			}
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"reply-from-%s:%s\"}}]}\n\n", req.Model, lastUser)
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":10,\"cost\":%v}}\n\n", d.CostUSD)
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}
	}))
	defer mockLLM.Close()

	tmp := t.TempDir()
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	var discordSent atomic.Int32
	eng.notifyMu.Lock()
	eng.notifier = testNotifier(func(context.Context, Notification) error {
		discordSent.Add(1)
		return nil
	})
	eng.notifyMu.Unlock()

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.BaseURL = mockLLM.URL
	cfg.APIKey = "sk-or-test"
	cfg.Notifications.Enabled = true
	cfg.Routing.GrantMaxTurns = 3
	cfg.Routing.GrantMaxCostUSD = 0.50
	if _, err := eng.UpdateConfig(cfg, false); err != nil {
		t.Fatal(err)
	}

	eng.Start(t.Context())
	sub := eng.Subscribe()
	defer eng.Unsubscribe(sub)

	waitDone := func() Message {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case ev := <-sub:
				if ev.Type == "message_done" {
					m, _ := ev.Data.(Message)
					return m
				}
			case <-deadline:
				t.Fatalf("timed out waiting for message_done")
			}
		}
	}
	waitApproval := func() Approval {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case ev := <-sub:
				if ev.Type == "approval" {
					a, _ := ev.Data.(Approval)
					return a
				}
			case <-deadline:
				t.Fatalf("timed out waiting for approval")
			}
		}
	}

	// 1. Low complexity (0.15) -> Haiku, no retry widget.
	nextDecision.Store(mockTestDecision{NeedsFrontier: 0.15, CostUSD: 0.001})
	if _, _, err := eng.EnqueueUserMessage("general", "hello there", ""); err != nil {
		t.Fatal(err)
	}
	m1 := waitDone()
	if m1.Model != DefaultChatModel || len(m1.Widgets) != 0 {
		t.Fatalf("expected Haiku without widgets for low complexity, got %+v", m1)
	}

	// 2. Borderline complexity (0.72) -> Haiku + frontier_retry widget; RetryWithFrontier runs Frontier cleanly without duplicating user prompt or leaking Haiku reply.
	nextDecision.Store(mockTestDecision{NeedsFrontier: 0.72, CostUSD: 0.002})
	if _, _, err := eng.EnqueueUserMessage("general", "compare caching approaches", ""); err != nil {
		t.Fatal(err)
	}
	m2 := waitDone()
	if m2.Model != DefaultChatModel || len(m2.Widgets) != 1 || m2.Widgets[0].Kind != "frontier_retry" {
		t.Fatalf("expected Haiku + frontier_retry widget, got %+v", m2)
	}
	if _, err := eng.RetryWithFrontier("general", m2.ID, "compare caching approaches", false); err != nil {
		t.Fatal(err)
	}
	m2Retry := waitDone()
	if m2Retry.Model != DefaultFrontierModel {
		t.Fatalf("expected Frontier retry model %s, got %s", DefaultFrontierModel, m2Retry.Model)
	}
	lastReq := recordedChatReqs[len(recordedChatReqs)-1]
	if lastReq.Messages[len(lastReq.Messages)-1].Role != "user" {
		t.Fatalf("expected final message in Frontier retry to be user, got %+v", lastReq.Messages)
	}
	var compareCount int
	for _, cm := range lastReq.Messages {
		if s, ok := cm.Content.(string); ok {
			if strings.Contains(s, "compare caching approaches") {
				compareCount++
			}
			if cm.Role == "assistant" && cm.Content == m2.Content {
				t.Fatalf("borderline Haiku reply leaked into Frontier retry context: %+v", lastReq.Messages)
			}
		}
	}
	if compareCount != 1 {
		t.Fatalf("expected user prompt once in Frontier retry, got %d (%+v)", compareCount, lastReq.Messages)
	}

	// 3. High complexity (0.92) -> creates frontier_escalation HITL card (no Discord alert), approve with scope="topic".
	nextDecision.Store(mockTestDecision{NeedsFrontier: 0.92, SameTopic: 0.90, CostUSD: 0.02})
	if _, _, err := eng.EnqueueUserMessage("general", "design multi-region consensus architecture", ""); err != nil {
		t.Fatal(err)
	}
	apr := waitApproval()
	if apr.ToolName != FrontierEscalationTool || discordSent.Load() != 0 {
		t.Fatalf("expected frontier_escalation approval with 0 Discord alerts, got %+v discord=%d", apr, discordSent.Load())
	}
	if _, err := eng.DecideApprovalWithScope(apr.ID, true, "topic", ""); err != nil {
		t.Fatal(err)
	}
	m3 := waitDone()
	if m3.Model != DefaultFrontierModel {
		t.Fatalf("expected Frontier model after topic approval, got %s", m3.Model)
	}
	grant := eng.ActiveFrontierGrant("general")
	if grant == nil || grant.TurnsUsed != 1 || grant.MaxTurns != 3 {
		t.Fatalf("expected active grant with turns_used=1/3, got %+v", grant)
	}

	// 4. Same-topic complex follow-up auto-approves without HITL (turns_used -> 2).
	nextDecision.Store(mockTestDecision{NeedsFrontier: 0.91, SameTopic: 0.90, CostUSD: 0.02})
	if _, _, err := eng.EnqueueUserMessage("general", "now detail failover fencing tokens", ""); err != nil {
		t.Fatal(err)
	}
	m4 := waitDone()
	if m4.Model != DefaultFrontierModel {
		t.Fatalf("expected auto-approved Frontier on same topic, got %s", m4.Model)
	}
	if g2 := eng.ActiveFrontierGrant("general"); g2 == nil || g2.TurnsUsed != 2 {
		t.Fatalf("expected turns_used=2, got %+v", g2)
	}

	// 5. Same-topic simple follow-up drops to Haiku while preserving grant.
	nextDecision.Store(mockTestDecision{NeedsFrontier: 0.20, SameTopic: 0.85, CostUSD: 0.001})
	if _, _, err := eng.EnqueueUserMessage("general", "save a quick note in Daily", ""); err != nil {
		t.Fatal(err)
	}
	m5 := waitDone()
	if m5.Model != DefaultChatModel || eng.ActiveFrontierGrant("general") == nil {
		t.Fatalf("expected Haiku drop-down while preserving grant, got model=%s grant=%+v", m5.Model, eng.ActiveFrontierGrant("general"))
	}

	// 6. Topic shift revokes active grant.
	nextDecision.Store(mockTestDecision{NeedsFrontier: 0.20, SameTopic: 0.10, CostUSD: 0.001})
	if _, _, err := eng.EnqueueUserMessage("general", "what is the weather in Tokyo?", ""); err != nil {
		t.Fatal(err)
	}
	_ = waitDone()
	if eng.ActiveFrontierGrant("general") != nil {
		t.Fatalf("expected topic shift to revoke active grant")
	}

	// 7. Deny escalation -> runs Haiku.
	nextDecision.Store(mockTestDecision{NeedsFrontier: 0.94, CostUSD: 0.001})
	if _, _, err := eng.EnqueueUserMessage("general", "deep formal proof of raft safety", ""); err != nil {
		t.Fatal(err)
	}
	aprDeny := waitApproval()
	if _, err := eng.DecideApprovalWithScope(aprDeny.ID, false, "", "too expensive"); err != nil {
		t.Fatal(err)
	}
	mDeny := waitDone()
	if mDeny.Model != DefaultChatModel {
		t.Fatalf("expected fallback Haiku on denied escalation, got %s", mDeny.Model)
	}

	// 8. Tool approval inside a Frontier turn preserves Frontier model on resume.
	nextDecision.Store(mockTestDecision{NeedsFrontier: 0.94, ToolCall: true, CostUSD: 0.01})
	if _, _, err := eng.EnqueueUserMessage("general", "write architecture doc", ""); err != nil {
		t.Fatal(err)
	}
	aprEsc := waitApproval()
	if _, err := eng.DecideApprovalWithScope(aprEsc.ID, true, "topic", ""); err != nil {
		t.Fatal(err)
	}
	aprTool := waitApproval()
	if aprTool.ToolName != "vault_write" {
		t.Fatalf("expected vault_write tool approval, got %+v", aprTool)
	}
	if _, err := eng.DecideApproval(aprTool.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	mToolDone := waitDone()
	if mToolDone.Model != DefaultFrontierModel {
		t.Fatalf("expected Frontier model preserved across tool approval resume, got %s", mToolDone.Model)
	}

	// ResetChannel clears active grant.
	if eng.ActiveFrontierGrant("general") == nil {
		t.Fatalf("expected active grant before ResetChannel")
	}
	if _, err := eng.ResetChannel("general", false); err != nil {
		t.Fatal(err)
	}
	if eng.ActiveFrontierGrant("general") != nil {
		t.Fatalf("expected ResetChannel to clear active grant")
	}

	// 9. Mode comparisons: gate (no borderline widget), auto (no HITL), off (no Decisions call), and daily soft cap.
	cfg.Routing.Mode = RoutingModeGate
	if _, err := eng.UpdateConfig(cfg, false); err != nil {
		t.Fatal(err)
	}
	nextDecision.Store(mockTestDecision{NeedsFrontier: 0.72, CostUSD: 0.001})
	if _, _, err := eng.EnqueueUserMessage("general", "borderline in gate mode", ""); err != nil {
		t.Fatal(err)
	}
	mGate := waitDone()
	if mGate.Model != DefaultChatModel || len(mGate.Widgets) != 0 {
		t.Fatalf("expected no borderline widget in gate mode, got %+v", mGate)
	}

	cfg.Routing.Mode = RoutingModeAuto
	if _, err := eng.UpdateConfig(cfg, false); err != nil {
		t.Fatal(err)
	}
	nextDecision.Store(mockTestDecision{NeedsFrontier: 0.95, CostUSD: 0.01})
	if _, _, err := eng.EnqueueUserMessage("general", "complex in auto mode", ""); err != nil {
		t.Fatal(err)
	}
	mAuto := waitDone()
	if mAuto.Model != DefaultFrontierModel {
		t.Fatalf("expected unattended Frontier in auto mode, got %s", mAuto.Model)
	}

	cfg.Routing.Mode = RoutingModeOff
	if _, err := eng.UpdateConfig(cfg, false); err != nil {
		t.Fatal(err)
	}
	beforeOff := decisionCount.Load()
	if _, _, err := eng.EnqueueUserMessage("general", "routine in off mode", ""); err != nil {
		t.Fatal(err)
	}
	mOff := waitDone()
	if mOff.Model != DefaultChatModel || decisionCount.Load() != beforeOff {
		t.Fatalf("expected off mode to skip Decisions API and use %s, got model=%s calls=%d->%d", DefaultChatModel, mOff.Model, beforeOff, decisionCount.Load())
	}

	// 10. Grant expiry by max_turns and daily_soft_cap_usd + background non-HITL safety.
	cfg.Routing.Mode = RoutingModeHybrid
	cfg.Routing.GrantMaxTurns = 1
	cfg.Routing.DailySoftCapUSD = 0.01
	if _, err := eng.UpdateConfig(cfg, false); err != nil {
		t.Fatal(err)
	}
	// Seed an exhausted grant (turns_used=1/1) -> must gate with HITL even though grant exists.
	_, _ = eng.store.UpsertFrontierGrant(FrontierGrant{
		ChannelID: "general", ContextEpoch: 2, TopicSummary: "arch", TurnsUsed: 1, MaxTurns: 1, MaxCostUSD: 0.50,
		ExpiresAt: time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339),
	})
	nextDecision.Store(mockTestDecision{NeedsFrontier: 0.95, SameTopic: 0.95})
	if _, _, err := eng.EnqueueUserMessage("general", "more arch work", ""); err != nil {
		t.Fatal(err)
	}
	aprCap := waitApproval()
	if aprCap.ToolName != FrontierEscalationTool {
		t.Fatalf("expected HITL escalation once max_turns exhausted, got %+v", aprCap)
	}
	_, _ = eng.DecideApproval(aprCap.ID, false, "")
	_ = waitDone()

	// Seed fresh grant under daily_soft_cap_usd (already > $0.01 today) -> forces HITL.
	_, _ = eng.store.UpsertFrontierGrant(FrontierGrant{
		ChannelID: "general", ContextEpoch: 2, TopicSummary: "arch", TurnsUsed: 0, MaxTurns: 5, MaxCostUSD: 0.50,
		ExpiresAt: time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339),
	})
	if _, _, err := eng.EnqueueUserMessage("general", "another arch turn over daily soft cap", ""); err != nil {
		t.Fatal(err)
	}
	aprDaily := waitApproval()
	if aprDaily.ToolName != FrontierEscalationTool {
		t.Fatalf("expected HITL escalation when daily_soft_cap_usd reached, got %+v", aprDaily)
	}
	_, _ = eng.DecideApproval(aprDaily.ID, false, "")
	_ = waitDone()

	// Background heartbeat never triggers HITL even when Decisions returns frontier.
	nextDecision.Store(mockTestDecision{NeedsFrontier: 0.99})
	if _, _, err := eng.store.EnqueueInbox("briefing", "heartbeat", "hb:test-no-hitl", map[string]any{"prompt": "check fleet"}, "auto"); err != nil {
		t.Fatal(err)
	}
	eng.notify()
	mHB := waitDone()
	if mHB.Model != DefaultFrontierModel && mHB.Model != DefaultChatModel {
		t.Fatalf("unexpected heartbeat model %s", mHB.Model)
	}
}

func TestDecisionModelZDRMigrationAndDoctorHint(t *testing.T) {
	if DefaultDecisionModel != "typesafe/jev-1.14" {
		t.Fatalf("expected DefaultDecisionModel=typesafe/jev-1.14, got %q", DefaultDecisionModel)
	}

	upgraded := NormalizeConfig(Config{
		BaseURL: DefaultOpenRouterURL,
		ZDR:     true,
		Models:  ModelConfig{Decision: "google/gemini-decision-1"},
	})
	if upgraded.Models.Decision != DefaultDecisionModel {
		t.Fatalf("expected ZDR OpenRouter config to auto-upgrade decision model to %q, got %q", DefaultDecisionModel, upgraded.Models.Decision)
	}

	preserved := NormalizeConfig(Config{
		BaseURL: DefaultOpenRouterURL,
		ZDR:     false,
		Models:  ModelConfig{Decision: "google/gemini-decision-1"},
	})
	if preserved.Models.Decision != "google/gemini-decision-1" {
		t.Fatalf("expected non-ZDR config to preserve explicit decision model, got %q", preserved.Models.Decision)
	}

	mockDecisions := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"No endpoints found matching your data policy (Zero data retention).","code":404}}`))
	}))
	defer mockDecisions.Close()

	tmp := t.TempDir()
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	check := eng.checkDecisionsDoctor(t.Context(), Config{
		BaseURL: mockDecisions.URL + "/api/v1",
		APIKey:  "sk-or-test",
		ZDR:     true,
		Models:  ModelConfig{DefaultChat: DefaultChatModel, Decision: "google/gemini-decision-1"},
	}, nil)
	if check.Status != "warn" || !strings.Contains(check.Hint, DefaultDecisionModel) {
		t.Fatalf("expected decisions doctor warn with ZDR hint mentioning %s, got %+v", DefaultDecisionModel, check)
	}
}

func TestAutoApproveBypassAndSlashDirectives(t *testing.T) {
	var step atomic.Int32
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/alpha/decisions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"answers":{"needs_frontier":{"type":"noul","noul":0.96}}}`))
		case "/chat/completions":
			var req struct {
				Model    string        `json:"model"`
				Messages []ChatMessage `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.Header().Set("Content-Type", "text/event-stream")
			switch step.Add(1) {
			case 1:
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tc_1\",\"type\":\"function\",\"function\":{\"name\":\"vault_write\",\"arguments\":\"{\\\"path\\\":\\\"Projects/Bypass.md\\\",\\\"content\\\":\\\"# Bypassed\\\"}\"}}]}}]}\n\n"))
				_, _ = w.Write([]byte("data: {\"choices\":[{\"finish_reason\":\"tool_calls\",\"delta\":{}}]}\n\n"))
			case 2:
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tc_2\",\"type\":\"function\",\"function\":{\"name\":\"fleet_spawn_agent\",\"arguments\":\"{\\\"harness\\\":\\\"claude\\\",\\\"device\\\":\\\"laptop\\\",\\\"prompt\\\":\\\"check\\\"}\"}}]}}]}\n\n"))
				_, _ = w.Write([]byte("data: {\"choices\":[{\"finish_reason\":\"tool_calls\",\"delta\":{}}]}\n\n"))
			case 3:
				_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done-global:%s\"}}]}\n\n", req.Model)
				_, _ = w.Write([]byte("data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n"))
			case 4:
				// One-shot /yolo /sol -> should write without approval and use frontier model.
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tc_3\",\"type\":\"function\",\"function\":{\"name\":\"vault_write\",\"arguments\":\"{\\\"path\\\":\\\"Projects/OneShot.md\\\",\\\"content\\\":\\\"# OneShot\\\"}\"}}]}}]}\n\n"))
				_, _ = w.Write([]byte("data: {\"choices\":[{\"finish_reason\":\"tool_calls\",\"delta\":{}}]}\n\n"))
			default:
				lastUser := ""
				for _, m := range slices.Backward(req.Messages) {
					if m.Role == "user" {
						if s, ok := m.Content.(string); ok {
							lastUser = s
							break
						}
					}
				}
				_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"reply:%s:%s\"}}]}\n\n", req.Model, lastUser)
				_, _ = w.Write([]byte("data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n"))
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}
	}))
	defer mockLLM.Close()

	// Verify directive parser edge cases.
	if m, aa, cl := parseTurnDirectives("/auto-approve /sol write this", DefaultAliases()); m != DefaultFrontierModel || !aa || cl != "write this" {
		t.Fatalf("unexpected parseTurnDirectives(/auto-approve /sol): model=%q aa=%v clean=%q", m, aa, cl)
	}
	if m, aa, cl := parseTurnDirectives("/sol /yolo write this", DefaultAliases()); m != DefaultFrontierModel || !aa || cl != "write this" {
		t.Fatalf("unexpected parseTurnDirectives(/sol /yolo): model=%q aa=%v clean=%q", m, aa, cl)
	}
	if m, aa, cl := parseTurnDirectives("/auto-approve", DefaultAliases()); m != "" || aa || cl != "/auto-approve" {
		t.Fatalf("expected bare /auto-approve not to consume empty prompt, got model=%q aa=%v clean=%q", m, aa, cl)
	}

	tmp := t.TempDir()
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), &fakeDeps{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	roDir := filepath.Join(tmp, "readonly-vault")
	if err := os.MkdirAll(roDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.BaseURL = mockLLM.URL
	cfg.APIKey = "sk-or-test"
	cfg.AutoApprove = true
	cfg.ExtraVaults = []VaultMount{{Name: "ref", Path: roDir, ReadOnly: true}}
	if _, err := eng.UpdateConfig(cfg, false); err != nil {
		t.Fatal(err)
	}

	// Read-only vault invariant must remain enforced even with BypassHITL=true.
	if _, roErr := eng.execVaultWriteTool(t.Context(), "vault_write", map[string]any{"vault": "ref", "path": "Note.md", "content": "x"}, "{}", &toolExecContext{ChannelID: "general", Source: "chat", Config: cfg, BypassHITL: true}); roErr == nil {
		t.Fatalf("expected read-only vault write to stay forbidden even under BypassHITL")
	}

	eng.Start(t.Context())
	sub := eng.Subscribe()
	defer eng.Unsubscribe(sub)

	waitDone := func() Message {
		t.Helper()
		dl := time.After(5 * time.Second)
		for {
			select {
			case ev := <-sub:
				if ev.Type == "message_done" {
					m, _ := ev.Data.(Message)
					return m
				}
			case <-dl:
				t.Fatalf("timed out waiting for message_done")
			}
		}
	}
	waitApproval := func() Approval {
		t.Helper()
		dl := time.After(5 * time.Second)
		for {
			select {
			case ev := <-sub:
				if ev.Type == "approval" {
					a, _ := ev.Data.(Approval)
					return a
				}
			case <-dl:
				t.Fatalf("timed out waiting for approval")
			}
		}
	}

	// 1. Global AutoApprove=true bypasses frontier escalation, protected vault_write, and fleet_spawn_agent.
	if _, _, err := eng.EnqueueUserMessage("general", "write Projects/Bypass.md and spawn agent", ""); err != nil {
		t.Fatal(err)
	}
	m1 := waitDone()
	if m1.Model != DefaultFrontierModel || !strings.Contains(m1.Content, "done-global:"+DefaultFrontierModel) {
		t.Fatalf("expected direct frontier completion without HITL, got %+v", m1)
	}
	pending, err := eng.Store().ListApprovals("pending", 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("expected 0 pending approvals with AutoApprove=true, got %+v (err=%v)", pending, err)
	}
	v, _ := eng.Vault()
	if got, rErr := v.ReadFile("Projects/Bypass.md"); rErr != nil || got != "# Bypassed" {
		t.Fatalf("expected Projects/Bypass.md written directly, got %q (%v)", got, rErr)
	}

	// 2. SetAutoApprove(false) restores HITL while preserving ExtraVaults/APIKey.
	fVal := false
	view, err := eng.SetAutoApprove(&fVal, nil)
	if err != nil {
		t.Fatal(err)
	}
	if view.AutoApprove || len(view.ExtraVaults) != 1 || !view.Configured {
		t.Fatalf("unexpected view after SetAutoApprove(false): %+v", view)
	}

	// 3. One-shot /yolo /sol bypasses HITL for that turn only without persisting AutoApprove.
	if _, _, err := eng.EnqueueUserMessage("general", "/yolo /sol write Projects/OneShot.md", ""); err != nil {
		t.Fatal(err)
	}
	m2 := waitDone()
	if m2.Model != DefaultFrontierModel || !strings.Contains(m2.Content, "write Projects/OneShot.md") {
		t.Fatalf("expected /yolo /sol turn completion with stripped directive, got %+v", m2)
	}
	if got, rErr := v.ReadFile("Projects/OneShot.md"); rErr != nil || got != "# OneShot" {
		t.Fatalf("expected Projects/OneShot.md written, got %q (%v)", got, rErr)
	}
	if eng.Config().AutoApprove {
		t.Fatalf("one-shot /yolo must not mutate persistent Config.AutoApprove")
	}

	// 4. Subsequent normal turn still triggers HITL frontier escalation.
	if _, _, err := eng.EnqueueUserMessage("general", "complex prompt requiring approval", ""); err != nil {
		t.Fatal(err)
	}
	apr := waitApproval()
	if apr.ToolName != FrontierEscalationTool {
		t.Fatalf("expected frontier_escalation approval after one-shot turn, got %+v", apr)
	}
}

func TestLighthouseSpawnGuardrailsPromptAndDoctor(t *testing.T) {
	tmp := t.TempDir()
	deps := &fakeDeps{lhWorker: LighthouseWorkerStatus{Device: "mini", WorkerOnline: true}}
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), deps)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	cfg := DefaultConfig()
	cfg.AutoApprove = true
	lhArgsDefault := `{"device":"mini","prompt":"install uv"}`
	lhArgsExplicit := `{"harness":"claude","device":"mini","prompt":"install uv"}`

	// 1. AutoApprove / BypassHITL must NOT bypass HITL for lighthouse spawns, and omitting harness defaults to agy.
	chatCtx := &toolExecContext{ChannelID: "general", Source: "chat", Config: cfg, BypassHITL: true}
	_, chatErr := eng.executeTool(t.Context(), ToolCall{ID: "lh-1", Type: "function", Function: ToolCallFunction{Name: "fleet_spawn_agent", Arguments: lhArgsDefault}}, chatCtx)
	aprErr, isApr := errors.AsType[*approvalRequiredError](chatErr)
	if !isApr || aprErr.RiskLevel != "lighthouse_spawn" || !strings.Contains(aprErr.Summary, "Spawn agy worker on lighthouse mini") {
		t.Fatalf("expected mandatory lighthouse_spawn approvalRequiredError with agy default, got %v", chatErr)
	}

	// Non-lighthouse spawns still require an explicit harness.
	_, missingHarnessErr := eng.executeTool(t.Context(), ToolCall{ID: "lh-nonlh", Type: "function", Function: ToolCallFunction{Name: "fleet_spawn_agent", Arguments: `{"device":"laptop","prompt":"fix"}`}}, chatCtx)
	if ae, ok := errors.AsType[*AssistantError](missingHarnessErr); !ok || ae.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("expected 400 when omitting harness on non-lighthouse device, got %v", missingHarnessErr)
	}

	// 2. Non-interactive timer/heartbeat runs are forbidden from spawning on the lighthouse.
	timerCtx := &toolExecContext{ChannelID: "briefing", Source: "timer", Config: cfg, BypassHITL: true}
	_, timerErr := eng.executeTool(t.Context(), ToolCall{ID: "lh-2", Type: "function", Function: ToolCallFunction{Name: "fleet_spawn_agent", Arguments: lhArgsDefault}}, timerCtx)
	if ae, ok := errors.AsType[*AssistantError](timerErr); !ok || ae.HTTPStatus() != http.StatusForbidden {
		t.Fatalf("expected 403 for timer lighthouse spawn, got %v", timerErr)
	}

	// 3. Explicit approval executes: omitted harness defaults to agy, explicit harness overrides it, empty repo_path defaults to "~".
	aprCtx := &toolExecContext{ChannelID: "general", Source: "approval", Config: cfg, BypassHITL: true}
	out, err := eng.executeTool(t.Context(), ToolCall{ID: "lh-3", Type: "function", Function: ToolCallFunction{Name: "fleet_spawn_agent", Arguments: lhArgsDefault}}, aprCtx)
	if err != nil {
		t.Fatalf("approved lighthouse spawn failed: %v", err)
	}
	if len(deps.spawned) != 1 || deps.spawned[0].Harness != "agy" || deps.spawned[0].RepoPath != "~" {
		t.Fatalf("expected harness=agy repo_path=~, got %+v", deps.spawned)
	}
	if _, err := eng.executeTool(t.Context(), ToolCall{ID: "lh-4", Type: "function", Function: ToolCallFunction{Name: "fleet_spawn_agent", Arguments: lhArgsExplicit}}, aprCtx); err != nil {
		t.Fatalf("approved explicit lighthouse spawn failed: %v", err)
	}
	if len(deps.spawned) != 2 || deps.spawned[1].Harness != "claude" {
		t.Fatalf("expected explicit harness=claude override, got %+v", deps.spawned)
	}
	if strings.Contains(out, `"hint"`) {
		t.Fatalf("did not expect offline hint when WorkerOnline=true: %s", out)
	}

	// 4. Prompt includes both lighthouse-self-config skill and Lighthouse Host Worker online/offline guidance.
	ch, _ := eng.Store().GetChannel("general")
	msgsOnline := eng.buildPromptMessages(t.Context(), *ch, nil, DefaultAliases())
	sysOnline := fmt.Sprint(msgsOnline[0].Content)
	if !strings.Contains(sysOnline, "lighthouse-self-config") || !strings.Contains(sysOnline, "Lighthouse Host Worker (mini, online)") {
		t.Fatalf("expected lighthouse skill and online worker section in prompt, got: %s", sysOnline)
	}

	deps.lhWorker.WorkerOnline = false
	msgsOffline := eng.buildPromptMessages(t.Context(), *ch, nil, DefaultAliases())
	sysOffline := fmt.Sprint(msgsOffline[0].Content)
	if !strings.Contains(sysOffline, "Lighthouse Host Worker (mini, offline)") || !strings.Contains(sysOffline, "worker-install") {
		t.Fatalf("expected offline worker guidance in prompt, got: %s", sysOffline)
	}

	// 5. Doctor lighthouse_worker check across all 4 states.
	deps.lhWorker = LighthouseWorkerStatus{Device: "mini", WorkerOnline: true, LastPollAt: "2026-11-28T08:00:00Z"}
	if dc := eng.checkLighthouseWorkerDoctor(t.Context()); dc.Status != "ok" {
		t.Fatalf("expected ok, got %+v", dc)
	}
	deps.lhWorker.InDefaultDevices = true
	if dc := eng.checkLighthouseWorkerDoctor(t.Context()); dc.Status != "warn" || !strings.Contains(dc.Detail, "default_devices") {
		t.Fatalf("expected warn for default_devices, got %+v", dc)
	}
	deps.lhWorker = LighthouseWorkerStatus{Device: "mini", WorkerOnline: false, LastPollAt: "2026-11-28T08:00:00Z"}
	if dc := eng.checkLighthouseWorkerDoctor(t.Context()); dc.Status != "warn" || !strings.Contains(dc.Detail, "offline") {
		t.Fatalf("expected warn for offline worker with last_poll_at, got %+v", dc)
	}
	deps.lhWorker = LighthouseWorkerStatus{Device: "mini"}
	if dc := eng.checkLighthouseWorkerDoctor(t.Context()); dc.Status != "skip" {
		t.Fatalf("expected skip when never enabled, got %+v", dc)
	}
}

func TestMCPHostPathAndResolveCommand(t *testing.T) {
	home := t.TempDir()
	localBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		t.Fatal(err)
	}
	uvxPath := filepath.Join(localBin, "uvx")
	if err := os.WriteFile(uvxPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pathEnv := mcpHostPathEnv("", home)
	if !strings.Contains(pathEnv, localBin) {
		t.Fatalf("expected %s in mcpHostPathEnv, got %q", localBin, pathEnv)
	}
	if got := resolveMCPCommand("uvx", pathEnv); got != uvxPath {
		t.Fatalf("expected %q, got %q", uvxPath, got)
	}
	if got := resolveMCPCommand("/custom/bin/uvx", pathEnv); got != "/custom/bin/uvx" {
		t.Fatalf("expected explicit path preserved, got %q", got)
	}
	env := setEnvVar([]string{"FOO=1", "PATH=/bin"}, "PATH", pathEnv)
	if !slices.Contains(env, "PATH="+pathEnv) || !slices.Contains(env, "FOO=1") {
		t.Fatalf("unexpected setEnvVar result: %+v", env)
	}
}

func TestNestedNonExistentSymlinkEscape(t *testing.T) {
	tmp := t.TempDir()
	outside := filepath.Join(tmp, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	// Vault nested non-existent subpath through symlink + dangling symlink.
	v, err := OpenVault(filepath.Join(tmp, "vault"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(v.Root, "ext")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "does-not-exist"), filepath.Join(v.Root, "dangling")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := v.ResolveRel("ext/nested/nonexistent/secret.md"); err == nil {
		t.Fatalf("expected nested non-existent symlink escape in Vault to be rejected")
	}
	if _, _, err := v.ResolveRel("dangling/nested/file.md"); err == nil {
		t.Fatalf("expected dangling symlink escape in Vault to be rejected")
	}
	if _, _, err := v.ResolveRel("Notes/newdir/sub/ok.md"); err != nil {
		t.Fatalf("expected legitimate nested non-existent path in Vault to succeed, got %v", err)
	}

	// Sandbox nested non-existent subpath through symlink.
	sb := NewSandboxManager(filepath.Join(tmp, "sandbox"), newMockSandboxRunner())
	workDir, err := sb.WorkDir("general")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workDir, "ext")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sb.ResolveWorkPath("general", "ext/deep/nonexistent/out.txt"); err == nil {
		t.Fatalf("expected nested non-existent symlink escape in Sandbox to be rejected")
	}
	if _, _, err := sb.ResolveWorkPath("general", "out/nested/report.txt"); err != nil {
		t.Fatalf("expected legitimate nested non-existent path in Sandbox to succeed, got %v", err)
	}
}

func TestExecuteCredentialHTTPBlocksRedirects(t *testing.T) {
	secret := "cred-secret-redirect-999"
	reader := func(_ context.Context, _ string) (string, error) { return secret, nil }

	var evilSawSecret atomic.Bool
	evilSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), secret) {
			evilSawSecret.Store(true)
		}
		_, _ = w.Write([]byte("evil"))
	}))
	defer evilSrv.Close()

	redirSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/to-evil":
			http.Redirect(w, r, evilSrv.URL+"/leak", http.StatusFound)
		case "/to-localhost":
			u, _ := url.Parse(evilSrv.URL)
			http.Redirect(w, r, "http://localhost:"+u.Port()+"/leak", http.StatusFound)
		case "/to-file":
			http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer redirSrv.Close()

	cfg := DefaultConfig()
	cfg.Credentials = []CredentialPolicy{{
		Alias:        "demo",
		OPRef:        "op://Vault/Item/tok",
		AllowedHosts: []string{"127.0.0.1", "localhost"},
	}}

	// Redirect to cross-domain host (localhost vs 127.0.0.1) must be blocked.
	if _, err := ExecuteCredentialHTTP(t.Context(), cfg, reader, redirSrv.Client(), CredentialHTTPRequest{
		Alias: "demo",
		URL:   redirSrv.URL + "/to-localhost",
	}); err == nil {
		t.Fatalf("expected cross-host redirect to be blocked")
	}
	// Redirect to non-HTTP(S) scheme must be blocked.
	if _, err := ExecuteCredentialHTTP(t.Context(), cfg, reader, redirSrv.Client(), CredentialHTTPRequest{
		Alias: "demo",
		URL:   redirSrv.URL + "/to-file",
	}); err == nil {
		t.Fatalf("expected non-http/https redirect to be blocked")
	}
	// Redirect to unallowlisted host must be blocked.
	cfgStrict := DefaultConfig()
	cfgStrict.Credentials = []CredentialPolicy{{
		Alias:        "strict",
		OPRef:        "op://Vault/Item/tok",
		AllowedHosts: []string{"127.0.0.1"},
	}}
	if _, err := ExecuteCredentialHTTP(t.Context(), cfgStrict, reader, redirSrv.Client(), CredentialHTTPRequest{
		OPRef:        "op://Vault/Item/tok",
		AllowedHosts: []string{"127.0.0.1"},
		URL:          redirSrv.URL + "/to-localhost",
	}); err == nil {
		t.Fatalf("expected redirect to non-allowlisted host to be blocked")
	}
	if evilSawSecret.Load() {
		t.Fatalf("credential secret leaked across redirect")
	}
}

func TestOPCLIValidationAndStdinTemplate(t *testing.T) {
	if _, err := NewOPReader("")(t.Context(), "--help"); err == nil {
		t.Fatalf("expected NewOPReader to reject '-' prefixed ref")
	}
	if _, err := GetOPItemMeta(t.Context(), "", "--vault=all", ""); err == nil {
		t.Fatalf("expected GetOPItemMeta to reject '-' prefixed item")
	}
	if _, err := StoreOPItem(t.Context(), "", "Vault", "Title", "", "", map[string]string{"--flag": "val"}); err == nil {
		t.Fatalf("expected StoreOPItem to reject '-' prefixed field key")
	}

	// Verify StoreOPItem sends fields in stdin JSON rather than CLI arguments.
	binDir := t.TempDir()
	argsLog := filepath.Join(binDir, "args.txt")
	stdinLog := filepath.Join(binDir, "stdin.json")
	fakeOP := filepath.Join(binDir, "op")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\ncat > %q\nprintf '{\"id\":\"it1\",\"title\":\"MyAPI\",\"category\":\"API_CREDENTIAL\"}'\n", argsLog, stdinLog)
	if err := os.WriteFile(fakeOP, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	meta, err := StoreOPItem(t.Context(), "", "AgentVault", "MyAPI", "API Credential", "https://api.example.com", map[string]string{
		"credential": "super-secret-value-xyz",
	})
	if err != nil {
		t.Fatalf("StoreOPItem failed: %v", err)
	}
	if meta.Title != "MyAPI" {
		t.Fatalf("unexpected StoreOPItem meta: %+v", meta)
	}
	rawArgs, _ := os.ReadFile(argsLog)
	if strings.Contains(string(rawArgs), "super-secret-value-xyz") {
		t.Fatalf("secret leaked in CLI arguments: %s", string(rawArgs))
	}
	rawStdin, _ := os.ReadFile(stdinLog)
	if !strings.Contains(string(rawStdin), "super-secret-value-xyz") {
		t.Fatalf("expected secret passed via stdin JSON template, got: %s", string(rawStdin))
	}
}

func TestMCPStdioErrorAndContextCancel(t *testing.T) {
	t.Setenv("GO_WANT_MCP_HELPER", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewMCPManager([]MCPServerConfig{{
		Name:    "helper",
		Command: exe,
		Args:    []string{"-test.run=^TestHelperMCPProcess$"},
		EnvRefs: map[string]string{"TEST_MCP_TOKEN": "op://Fleet-Agent/Helper/token"},
		Enabled: true,
	}}, func(_ context.Context, _ string) (string, error) {
		return "injected-secret-99", nil
	})
	defer mgr.Close()

	// 1. JSON-RPC error response must return immediately instead of hanging.
	ctxErr, cancelErr := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelErr()
	if _, err := mgr.CallTool(ctxErr, "mcp__helper__echo_secret", map[string]any{"mode": "error"}); err == nil || !strings.Contains(err.Error(), "simulated mcp failure") {
		t.Fatalf("expected JSON-RPC error returned immediately, got %v", err)
	}

	// 2. Context cancellation during blocked stdio read must return promptly and tear down subprocess.
	ctxHang, cancelHang := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancelHang()
	start := time.Now()
	if _, err := mgr.CallTool(ctxHang, "mcp__helper__echo_secret", map[string]any{"mode": "hang"}); err == nil {
		t.Fatalf("expected canceled stdio call to return error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("stdio cancel took too long: %v", time.Since(start))
	}

	// 3. Subsequent call cleanly restarts subprocess because c.started was reset.
	ctxOK, cancelOK := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelOK()
	res, err := mgr.CallTool(ctxOK, "mcp__helper__echo_secret", map[string]any{"msg": "recovered"})
	if err != nil {
		t.Fatalf("expected stdio MCP to recover after cancel, got %v", err)
	}
	if m, _ := res.(map[string]any); m["msg"] != "recovered" {
		t.Fatalf("unexpected recovered result: %+v", res)
	}
}

func TestStoreRecoveryPermissionsAndPostClose(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "assistant.db")
	st, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 permissions on assistant.db, got %o", fi.Mode().Perm())
	}

	ev, inserted, err := st.EnqueueInbox("general", "chat", "rec-1", map[string]any{"content": "hello"}, "")
	if err != nil || !inserted {
		t.Fatalf("EnqueueInbox failed: inserted=%v err=%v", inserted, err)
	}
	claimed, err := st.ClaimNextInbox()
	if err != nil || claimed == nil || claimed.ID != ev.ID || claimed.Status != "running" {
		t.Fatalf("ClaimNextInbox failed: %+v err=%v", claimed, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Post-Close calls must return error safely without nil-pointer panic.
	if _, err := st.ListChannels(); err == nil {
		t.Fatalf("expected error on ListChannels after Close")
	}
	_ = st.Summary()
	if err := st.Close(); err != nil {
		t.Fatalf("expected idempotent Close, got %v", err)
	}

	// Reopening must recover stale 'running' inbox rows back to 'queued'.
	st2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()
	recovered, err := st2.ClaimNextInbox()
	if err != nil || recovered == nil || recovered.ID != ev.ID {
		t.Fatalf("expected stale running row recovered and claimable, got %+v err=%v", recovered, err)
	}
}

func TestOpenRouterMidStreamError(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n" +
		"data: {\"error\":{\"code\":502,\"message\":\"upstream provider reset\"}}\n\n"
	_, err := readChatSSE(strings.NewReader(sse), nil)
	if err == nil || !strings.Contains(err.Error(), "upstream provider reset") {
		t.Fatalf("expected mid-stream SSE error to fail, got %v", err)
	}
}

func TestSchemaV2ResetEventRouterAndPersonalProxy(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "assistant.db")

	// Seed a simulated v1 database with legacy assistant_delegations + old messages + preserved OAuth token.
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rawDB.Exec(`
		CREATE TABLE assistant_delegations(id TEXT PRIMARY KEY);
		INSERT INTO assistant_delegations(id) VALUES ('del-legacy');
		CREATE TABLE assistant_messages(id INTEGER PRIMARY KEY AUTOINCREMENT, channel_id TEXT, role TEXT, content TEXT, created_at TEXT);
		INSERT INTO assistant_messages(channel_id, role, content, created_at) VALUES ('general', 'user', 'old-v1', '2026-01-01T00:00:00Z');
		CREATE TABLE assistant_mcp_oauth_tokens(
			server_name TEXT PRIMARY KEY, access_token TEXT NOT NULL DEFAULT '', refresh_token TEXT NOT NULL DEFAULT '',
			token_type TEXT NOT NULL DEFAULT 'Bearer', scope TEXT NOT NULL DEFAULT '', expires_at TEXT NOT NULL DEFAULT '',
			token_url TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL
		);
		INSERT INTO assistant_mcp_oauth_tokens(server_name, access_token, updated_at) VALUES ('gcal', 'tok-keep', '2026-01-01T00:00:00Z');
	`)
	_ = rawDB.Close()
	if err != nil {
		t.Fatal(err)
	}

	var triageAction atomic.Value
	triageAction.Store("ops_turn")
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/alpha/decisions" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"answers":{"action":{"type":"choice","choice":%q}}}`, triageAction.Load().(string))
			return
		}
		http.NotFound(w, r)
	}))
	defer mockLLM.Close()

	deps := &fakeDeps{}
	eng, err := Open(dbPath, filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), deps)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	// Confirm OAuth token preserved and v1 legacy table dropped + 4 channels seeded.
	tok, ok, err := eng.store.GetMCPOAuthToken("gcal")
	if err != nil || !ok || tok.AccessToken != "tok-keep" {
		t.Fatalf("expected OAuth token preserved across v2 reset, got ok=%v tok=%+v err=%v", ok, tok, err)
	}
	channels, err := eng.store.ListChannels()
	if err != nil || len(channels) != 4 || channels[3].ID != "events" {
		t.Fatalf("expected 4 default channels including #events, got %+v (%v)", channels, err)
	}
	oldMsgs, _ := eng.store.ListMessages("general", 10)
	if len(oldMsgs) != 0 {
		t.Fatalf("expected legacy v1 messages cleared by v2 reset, got %d", len(oldMsgs))
	}

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.BaseURL = mockLLM.URL
	cfg.APIKey = "sk-test"
	cfg.VaultExposure = ExposureAssistantOnly
	cfg.Events.QuietHoursStart = "00:00"
	cfg.Events.QuietHoursEnd = "00:00"
	cfg.MCPServers = []MCPServerConfig{
		{
			Name:           "secret_srv",
			Transport:      "stdio",
			Command:        os.Args[0],
			Args:           []string{"-test.run=TestHelperMCPProcess"},
			Env:            map[string]string{"GO_WANT_MCP_HELPER": "1"},
			Enabled:        true,
			Exposure:       ExposureAssistantOnly,
			ApprovalPolicy: MCPApprovalPolicyNone,
		},
		{
			Name:           "personal_srv",
			Transport:      "stdio",
			Command:        os.Args[0],
			Args:           []string{"-test.run=TestHelperMCPProcess"},
			Env:            map[string]string{"GO_WANT_MCP_HELPER": "1"},
			Enabled:        true,
			Exposure:       ExposurePersonalDevices,
			ApprovalPolicy: MCPApprovalPolicyAll,
		},
	}
	if _, err := eng.UpdateConfig(cfg, false); err != nil {
		t.Fatal(err)
	}

	// 1. memory.proposal -> logged to #events with Observed=true (0-token), nothing in #general or #fleet-ops.
	eng.HandleBusEvent(t.Context(), events.Event{
		Type: "memory.proposal", Severity: "info", Source: "memory", Subject: "global/k", Title: "Proposal k", Summary: "body",
	})
	evMsgs, _ := eng.store.ListMessages("events", 10)
	if len(evMsgs) != 1 || !evMsgs[0].Observed {
		t.Fatalf("expected 1 observed message in #events, got %+v", evMsgs)
	}
	if unobs, _ := eng.store.ListUnobservedMessages("events", 10); len(unobs) != 0 {
		t.Fatalf("expected 0 unobserved messages in #events, got %d", len(unobs))
	}

	// 2. slacker.job failure triggers Stage-1 triage ("ops_turn") -> wakes #fleet-ops.
	eng.HandleBusEvent(t.Context(), events.Event{
		Type: "slacker.job", Severity: "error", Source: "slacker", Subject: "job-9", Title: "Slacker failed", Summary: "boom",
		Payload: map[string]any{"status": "failed"},
	})
	inbox, _ := eng.store.ListInbox("queued", 10)
	if len(inbox) == 0 || inbox[0].ChannelID != "fleet-ops" {
		t.Fatalf("expected queued inbox wakeup on fleet-ops from Stage-1 triage, got %+v", inbox)
	}

	// 3. Critical outage escalates to #general, and non-critical repeat respects cooldown.
	eng.HandleBusEvent(t.Context(), events.Event{
		Type: "infra.tls_error", Severity: "error", Source: "tls", Subject: "renew", Title: "TLS renewal failed", Summary: "dns error",
	})
	genMsgs, _ := eng.store.ListMessages("general", 10)
	if len(genMsgs) != 1 || !strings.Contains(genMsgs[0].Content, "TLS renewal failed") {
		t.Fatalf("expected escalated alert in #general, got %+v", genMsgs)
	}
	// Repeat within cooldown -> suppressed from #general, logged to #fleet-ops instead.
	eng.HandleBusEvent(t.Context(), events.Event{
		Type: "infra.tls_error", Severity: "error", Source: "tls", Subject: "renew", Title: "TLS renewal failed", Summary: "dns error 2",
	})
	genMsgs2, _ := eng.store.ListMessages("general", 10)
	if len(genMsgs2) != 1 {
		t.Fatalf("expected cooldown to suppress second #general escalation, got %d", len(genMsgs2))
	}

	// 4. Personal proxy exposure checks: personal_srv tools listed, secret_srv excluded, vault assistant_only rejected.
	pTools := eng.ListPersonalMCPTools(t.Context())
	for _, pt := range pTools {
		if pt.ServerName == "secret_srv" {
			t.Fatalf("assistant_only server leaked into ListPersonalMCPTools: %+v", pt)
		}
	}
	if _, err := eng.CallPersonalMCPTool(t.Context(), "laptop", "mcp__secret_srv__echo_secret", nil); err == nil {
		t.Fatalf("expected CallPersonalMCPTool to reject assistant_only server")
	}
	if len(pTools) > 0 {
		aprRes, err := eng.CallPersonalMCPTool(t.Context(), "laptop", pTools[0].FullName, map[string]any{"msg": "hi"})
		if err != nil {
			t.Fatalf("expected personal_srv approval queue, got %v", err)
		}
		if m, _ := aprRes.(map[string]any); m["status"] != "approval_required" || m["channel_id"] != "fleet-ops" {
			t.Fatalf("unexpected personal approval result: %+v", aprRes)
		}
	}
	if _, err := eng.PersonalVaultSearch(t.Context(), "test", "", 5); err == nil {
		t.Fatalf("expected PersonalVaultSearch to reject when vault_exposure=assistant_only")
	}
}
