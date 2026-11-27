package assistant

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeDeps struct {
	sentSlack []string
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

	skillBody := "---\nname: morning-briefing\ndescription: Summarize waiting agents and calendar\nmodel: openai/gpt-6-astra\ntags: [daily, fleet]\n---\n# Steps\n1. Check agents.\n"
	sk, err := v.SaveSkill("morning-briefing", skillBody)
	if err != nil {
		t.Fatal(err)
	}
	if sk.Model != "openai/gpt-6-astra" || len(sk.Tags) != 2 {
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
		case "/decisions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"answers":[{"id":"tier","value":"frontier","confidence":0.94}]}`))
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
	if len(seenModels) == 0 || seenModels[0] != "openai/gpt-6-astra" {
		t.Fatalf("expected /astra override model openai/gpt-6-astra, got %v", seenModels)
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
		case "/decisions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"answers":[{"id":"tier","value":"fast","confidence":0.95}]}`))
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
