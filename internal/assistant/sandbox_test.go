package assistant

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type mockSandboxRunner struct {
	mu           sync.Mutex
	binPath      string
	statusErr    error
	networkReady bool
	imageExists  map[string]bool
	started      []SandboxStartOpts
	execCalls    []SandboxExecOpts
	stopped      []string
	built        []string
	execFn       func(SandboxExecOpts) (SandboxRawResult, error)
}

func newMockSandboxRunner() *mockSandboxRunner {
	return &mockSandboxRunner{
		binPath:      "/opt/homebrew/bin/container",
		networkReady: true,
		imageExists: map[string]bool{
			DefaultSandboxBaseImage: true,
			DefaultSandboxImage:     true,
		},
	}
}

func (m *mockSandboxRunner) FindBinary() string { return m.binPath }

func (m *mockSandboxRunner) SystemStatus(_ context.Context) error { return m.statusErr }

func (m *mockSandboxRunner) NetworkExists(_ context.Context, _ string) bool { return m.networkReady }

func (m *mockSandboxRunner) EnsureNetwork(_ context.Context, _ string, _ string) (string, error) {
	m.networkReady = true
	return "127.0.0.1", nil
}

func (m *mockSandboxRunner) ImageExists(_ context.Context, image string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.imageExists[image]
}

func (m *mockSandboxRunner) BuildImage(_ context.Context, tag, containerfilePath, _ string, _, _ int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, _ := os.ReadFile(containerfilePath)
	m.built = append(m.built, tag+"::"+string(b))
	if m.imageExists == nil {
		m.imageExists = make(map[string]bool)
	}
	m.imageExists[tag] = true
	return "built " + tag, nil
}

func (m *mockSandboxRunner) StartContainer(_ context.Context, opts SandboxStartOpts) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = append(m.started, opts)
	return nil
}

func (m *mockSandboxRunner) ExecContainer(_ context.Context, opts SandboxExecOpts) (SandboxRawResult, error) {
	m.mu.Lock()
	m.execCalls = append(m.execCalls, opts)
	fn := m.execFn
	m.mu.Unlock()
	if fn != nil {
		return fn(opts)
	}
	return SandboxRawResult{ExitCode: 0, Stdout: "ok\n"}, nil
}

func (m *mockSandboxRunner) StopRemoveContainer(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = append(m.stopped, name)
	return nil
}

func TestSandboxPackageValidationAndContainerfile(t *testing.T) {
	for _, okApt := range []string{"poppler-utils", "tesseract-ocr", "g++", "python3.13"} {
		if err := ValidateAptPackageName(okApt); err != nil {
			t.Fatalf("expected valid apt package %q, got %v", okApt, err)
		}
	}
	for _, badApt := range []string{"", "-y", "BadPkg", "foo;rm", "pkg/bar", "pkg name"} {
		if err := ValidateAptPackageName(badApt); err == nil {
			t.Fatalf("expected invalid apt package %q to fail", badApt)
		}
	}

	for _, okPip := range []string{"weasyprint", "pandas>=2.2.0", "pymupdf[all]", "reportlab==4.1.0"} {
		if err := ValidatePipPackageSpec(okPip); err != nil {
			t.Fatalf("expected valid pip spec %q, got %v", okPip, err)
		}
	}
	for _, badPip := range []string{"", "--index-url=http://x", "-r", "https://evil.com/a.whl", "pkg@https://x", "pkg;whoami", "pkg name"} {
		if err := ValidatePipPackageSpec(badPip); err == nil {
			t.Fatalf("expected invalid pip spec %q to fail", badPip)
		}
	}

	cf, err := RenderCustomContainerfile("fleet-sandbox:base", []string{"graphviz"}, []string{"seaborn>=0.13"})
	if err != nil {
		t.Fatalf("RenderCustomContainerfile failed: %v", err)
	}
	if !strings.Contains(cf, "FROM fleet-sandbox:base") ||
		!strings.Contains(cf, "apt-get install -y --no-install-recommends graphviz") ||
		!strings.Contains(cf, `pip install --no-cache-dir "seaborn>=0.13"`) ||
		!strings.HasSuffix(cf, "USER sandbox\nWORKDIR /work\n") {
		t.Fatalf("unexpected containerfile output:\n%s", cf)
	}
}

func TestEgressProxySSRFAndAuth(t *testing.T) {
	for _, blocked := range []string{
		"127.0.0.1", "10.0.0.1", "172.16.5.4", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "10.213.0.1", "::1", "fe80::1",
	} {
		addr := netip.MustParseAddr(blocked)
		if !IsBlockedEgressAddr(addr) {
			t.Fatalf("expected %s to be blocked", blocked)
		}
	}
	if IsBlockedEgressAddr(netip.MustParseAddr("93.184.216.34")) {
		t.Fatalf("expected public IP 93.184.216.34 to be allowed")
	}

	if !HostMatchesAllowlist("api.github.com", []string{"*.github.com"}) {
		t.Fatalf("expected wildcard match for api.github.com")
	}
	if HostMatchesAllowlist("evil.com", []string{"*.github.com"}) {
		t.Fatalf("expected evil.com to be rejected by allowlist")
	}

	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "nebula.internal":
			return []netip.Addr{netip.MustParseAddr("10.213.0.1")}, nil
		case "metadata.local":
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		default:
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
	}
	px, err := StartEgressProxy("127.0.0.1:0", []string{"api.example.com", "nebula.internal", "metadata.local"}, lookup)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = px.Close() }()

	// 1. Unauthenticated proxy request -> 407.
	proxyParsed, _ := url.Parse(px.ProxyURL())
	noAuthProxy, _ := url.Parse("http://" + proxyParsed.Host)
	noAuthClient := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(noAuthProxy)}}
	if resp, rErr := noAuthClient.Get("http://api.example.com/"); rErr == nil {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusProxyAuthRequired {
			t.Fatalf("expected 407 without Proxy-Authorization, got %d", resp.StatusCode)
		}
	}

	// 2. Authenticated client: blocked port, non-allowlisted host, Nebula IP, metadata IP all blocked (403).
	authClient := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyParsed)}}
	for _, target := range []string{
		"http://api.example.com:8443/test",
		"http://not-allowed.com/test",
		"http://nebula.internal/v1/assistant/status",
		"http://metadata.local/latest/meta-data",
	} {
		resp, rErr := authClient.Get(target)
		if rErr != nil {
			t.Fatalf("GET %s transport error: %v", target, rErr)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 for %s, got %d", target, resp.StatusCode)
		}
	}

	// Verify CONNECT rejects blocked port/IP too.
	req, _ := http.NewRequest(http.MethodConnect, "http://nebula.internal:443", nil)
	tok := base64.StdEncoding.EncodeToString([]byte("fleet:" + px.token))
	req.Header.Set("Proxy-Authorization", "Basic "+tok)
	rec := httptest.NewRecorder()
	px.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected CONNECT to nebula.internal to fail with 403, got %d", rec.Code)
	}
}

func TestSandboxManagerAndAssistantTools(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "assistant.json")
	vaultDir := filepath.Join(tmp, "vault")
	eng, err := Open(filepath.Join(tmp, "assistant.db"), cfgPath, vaultDir, &fakeDeps{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	runner := newMockSandboxRunner()
	eng.SetSandboxRunner(runner)

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.APIKey = "sk-test"
	cfg.Sandbox.Enabled = true
	cfg.Sandbox.IdleMinutes = 1
	extraDir := filepath.Join(tmp, "extra-vault")
	if err := os.MkdirAll(filepath.Join(extraDir, "Notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extraDir, "Notes", "data.csv"), []byte("a,b\n1,2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.ExtraVaults = []VaultMount{{Name: "personal", Path: extraDir, AutoWriteDirs: []string{"Inbox"}}}
	if _, err := eng.UpdateConfig(cfg, false); err != nil {
		t.Fatal(err)
	}

	// Doctor check passes with ready mock runner.
	rep := eng.Doctor(t.Context())
	var foundSB bool
	for _, c := range rep.Checks {
		if c.Name == "sandbox" {
			foundSB = true
			if c.Status != "ok" {
				t.Fatalf("expected sandbox doctor ok, got %+v", c)
			}
		}
	}
	if !foundSB {
		t.Fatalf("expected sandbox doctor check")
	}

	// Upload a file and enqueue a user message with attachment.
	uploaded, err := eng.SaveUploadedFile("general", "../../evil/sales report.csv", "text/csv", strings.NewReader("item,qty\npen,5\n"))
	if err != nil {
		t.Fatal(err)
	}
	if uploaded.Name != "sales_report.csv" {
		t.Fatalf("expected sanitized filename sales_report.csv, got %q", uploaded.Name)
	}
	msg, _, err := eng.EnqueueUserMessageWithFiles("general", "", "", []string{uploaded.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Widgets) != 1 || msg.Widgets[0].Kind != "files" {
		t.Fatalf("expected files widget on user message, got %+v", msg.Widgets)
	}
	ch, _ := eng.store.GetChannel("general")
	tail, _ := eng.store.ListContextMessages("general", ch.ContextEpoch, 10)
	promptMsgs := eng.buildPromptMessages(t.Context(), *ch, tail, cfg.Models.Aliases)
	if !strings.Contains(promptMsgs[len(promptMsgs)-1].Content.(string), "/work/in/: sales_report.csv") {
		t.Fatalf("expected prompt attachment hint, got %#v", promptMsgs[len(promptMsgs)-1].Content)
	}

	// Execute sandbox_exec offline in chat -> succeeds, starts container lazily, detects new /work/out/ file.
	workGeneral, _ := eng.sandbox.WorkDir("general")
	runner.execFn = func(_ SandboxExecOpts) (SandboxRawResult, error) {
		_ = os.WriteFile(filepath.Join(workGeneral, "out", "report.pdf"), []byte("%PDF-1.4\nmock"), 0o644)
		return SandboxRawResult{ExitCode: 0, Stdout: "generated report.pdf\n"}, nil
	}
	tctx := &toolExecContext{ChannelID: "general", Source: "chat", Config: eng.Config(), Widgets: &[]WidgetPayload{}}
	outRaw, err := eng.executeTool(t.Context(), ToolCall{
		ID: "tc1", Type: "function",
		Function: ToolCallFunction{Name: "sandbox_exec", Arguments: `{"script":"print('hi')","lang":"python"}`},
	}, tctx)
	if err != nil {
		t.Fatalf("offline sandbox_exec failed: %v", err)
	}
	if !strings.Contains(outRaw, "out/report.pdf") {
		t.Fatalf("expected new_files out/report.pdf in result: %s", outRaw)
	}

	// Publish generated file -> attaches files widget.
	pubRaw, err := eng.executeTool(t.Context(), ToolCall{
		ID: "tc2", Type: "function",
		Function: ToolCallFunction{Name: "sandbox_publish_file", Arguments: `{"path":"/work/out/report.pdf","title":"Sales PDF"}`},
	}, tctx)
	if err != nil {
		t.Fatalf("sandbox_publish_file failed: %v", err)
	}
	if !strings.Contains(pubRaw, `"published":true`) || len(*tctx.Widgets) != 1 || (*tctx.Widgets)[0].Kind != "files" {
		t.Fatalf("expected published files widget, got raw=%s widgets=%+v", pubRaw, *tctx.Widgets)
	}

	// Path traversal must be rejected.
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID: "tc3", Type: "function",
		Function: ToolCallFunction{Name: "sandbox_read_file", Arguments: `{"path":"../../etc/passwd"}`},
	}, tctx); err == nil {
		t.Fatalf("expected path traversal to fail")
	}

	// Vault bridge: import personal:Notes/data.csv -> /work/in/data.csv; export to protected path requires HITL.
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID: "tc4", Type: "function",
		Function: ToolCallFunction{Name: "sandbox_vault_bridge", Arguments: `{"action":"import","vault":"personal","vault_path":"Notes/data.csv"}`},
	}, tctx); err != nil {
		t.Fatalf("vault import failed: %v", err)
	}
	if _, err := eng.sandbox.WriteTextFile("general", "out/summary.md", "# Summary\nDone\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID: "tc5", Type: "function",
		Function: ToolCallFunction{Name: "sandbox_vault_bridge", Arguments: `{"action":"export","vault":"personal","vault_path":"Projects/Summary.md","sandbox_path":"out/summary.md"}`},
	}, tctx); err == nil || !strings.Contains(err.Error(), "approval required") {
		t.Fatalf("expected HITL approval for protected vault export, got %v", err)
	}

	// Internet exec must require HITL approval; when approved, injects proxy env vars.
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID: "tc6", Type: "function",
		Function: ToolCallFunction{Name: "sandbox_exec", Arguments: `{"command":"curl https://example.com","internet":true,"allowed_hosts":["example.com"],"reason":"fetch ref"}`},
	}, tctx); err == nil || !strings.Contains(err.Error(), "approval required") {
		t.Fatalf("expected HITL approval for internet=true, got %v", err)
	}

	var seenProxyEnv bool
	runner.execFn = func(opts SandboxExecOpts) (SandboxRawResult, error) {
		for _, kv := range opts.Env {
			if strings.HasPrefix(kv, "HTTPS_PROXY=http://fleet:") {
				seenProxyEnv = true
			}
		}
		return SandboxRawResult{ExitCode: 0, Stdout: "proxied"}, nil
	}
	approvedCtx := &toolExecContext{ChannelID: "general", Source: "approval", Config: eng.Config(), BypassHITL: true, Widgets: &[]WidgetPayload{}}
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID: "tc7", Type: "function",
		Function: ToolCallFunction{Name: "sandbox_exec", Arguments: `{"command":"curl https://example.com","internet":true,"allowed_hosts":["example.com"]}`},
	}, approvedCtx); err != nil || !seenProxyEnv {
		t.Fatalf("expected approved internet exec to inject HTTPS_PROXY, err=%v seenProxy=%v", err, seenProxyEnv)
	}

	// Background heartbeat offline sandbox_exec also requires HITL approval.
	hbCtx := &toolExecContext{ChannelID: "briefing", Source: "heartbeat", Config: eng.Config(), Widgets: &[]WidgetPayload{}}
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID: "tc8", Type: "function",
		Function: ToolCallFunction{Name: "sandbox_exec", Arguments: `{"command":"echo hi"}`},
	}, hbCtx); err == nil || !strings.Contains(err.Error(), "approval required") {
		t.Fatalf("expected heartbeat sandbox_exec to require HITL approval, got %v", err)
	}

	// Switching channel evicts the previous active container (max 1 running).
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID: "tc9", Type: "function",
		Function: ToolCallFunction{Name: "sandbox_exec", Arguments: `{"command":"echo briefing"}`},
	}, &toolExecContext{ChannelID: "briefing", Source: "chat", Config: eng.Config(), Widgets: &[]WidgetPayload{}}); err != nil {
		t.Fatalf("briefing sandbox_exec failed: %v", err)
	}
	if len(runner.stopped) == 0 || runner.stopped[0] != "fleet-sb-general" {
		t.Fatalf("expected fleet-sb-general to be stopped on channel switch, stopped=%+v", runner.stopped)
	}

	// Idle reap stops active container.
	eng.sandbox.mu.Lock()
	eng.sandbox.active.lastUsed = time.Now().Add(-5 * time.Minute)
	eng.sandbox.mu.Unlock()
	eng.sandbox.ReapIdle(t.Context(), 1)
	if eng.sandbox.active != nil {
		t.Fatalf("expected active sandbox to be reaped when idle")
	}

	// sandbox_install_packages: validates upfront, gates with HITL, persists & rebuilds on approval.
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID: "tc10", Type: "function",
		Function: ToolCallFunction{Name: "sandbox_install_packages", Arguments: `{"pip_packages":["https://evil.com/x.whl"]}`},
	}, tctx); err == nil || strings.Contains(err.Error(), "approval required") {
		t.Fatalf("expected invalid pip spec to fail upfront without approval, got %v", err)
	}
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID: "tc11", Type: "function",
		Function: ToolCallFunction{Name: "sandbox_install_packages", Arguments: `{"apt_packages":["graphviz"],"pip_packages":["seaborn>=0.13"],"reason":"render flowcharts"}`},
	}, tctx); err == nil || !strings.Contains(err.Error(), "approval required") {
		t.Fatalf("expected HITL approval for sandbox_install_packages, got %v", err)
	}
	instRaw, err := eng.executeTool(t.Context(), ToolCall{
		ID: "tc12", Type: "function",
		Function: ToolCallFunction{Name: "sandbox_install_packages", Arguments: `{"apt_packages":["graphviz"],"pip_packages":["seaborn>=0.13"]}`},
	}, approvedCtx)
	if err != nil {
		t.Fatalf("approved sandbox_install_packages failed: %v", err)
	}
	var instRes map[string]any
	_ = json.Unmarshal([]byte(instRaw), &instRes)
	if instRes["status"] != "installed" || len(eng.Config().Sandbox.ExtraAptPackages) != 1 || len(eng.Config().Sandbox.ExtraPipPackages) != 1 {
		t.Fatalf("expected persisted custom packages & rebuild, got %s cfg=%+v", instRaw, eng.Config().Sandbox)
	}
}
