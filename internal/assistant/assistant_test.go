package assistant

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeDeps struct {
	sentSlack []string
	spawned   []SpawnAgentRequest
	killed    []string
}

func (f *fakeDeps) ListAgents(_, _, _, _ string, _ bool) (any, error) {
	return []map[string]any{{"name": "auth-worker", "device": "laptop", "state": "waiting_permission"}}, nil
}

func (f *fakeDeps) InspectAgent(target string) (any, error) {
	return map[string]any{"target": target, "state": "idle"}, nil
}

func (f *fakeDeps) SendAgentMessage(fromDevice, to, body string) (any, error) {
	return map[string]any{"from": fromDevice, "to": to, "body": body}, nil
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
	if err != nil || len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d (%v)", len(skills), err)
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

	// Phase 3: fleet_list_paths, fleet_spawn_agent (approval -> delegation), OnAgentHeartbeat wakeup, fleet_kill_agent.
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
	if err != nil || !strings.Contains(spawnOut, "job-spawn-1") {
		t.Fatalf("approved fleet_spawn_agent failed: %v (%s)", err, spawnOut)
	}

	// Simulate Slacker launch receipt and agent heartbeat transition working -> waiting_permission.
	eng.OnSlackerReceipt("job-spawn-1", "laptop", "fix-auth", "launched", "")
	eng.OnAgentHeartbeat("laptop", "claude", "laptop:claude:abc1", "fix-auth", "working", "Editing auth.go", "", nil)
	eng.OnAgentHeartbeat("laptop", "claude", "laptop:claude:abc1", "fix-auth", "waiting_permission", "Need bash approval", "M auth.go", map[string]string{"tool": "Bash"})

	var foundDelegationEvent bool
	for range 10 {
		ev, err := eng.store.ClaimNextInbox()
		if err == nil && ev != nil && ev.Source == "delegation" {
			foundDelegationEvent = true
			break
		}
	}
	if !foundDelegationEvent {
		t.Fatalf("expected delegation inbox event after waiting_permission heartbeat")
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
	eng.delMu.Lock()
	eng.notifier = notifier
	eng.delMu.Unlock()

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
}
