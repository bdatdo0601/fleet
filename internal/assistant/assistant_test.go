package assistant

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
