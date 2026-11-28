package assistant

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
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
	buildCPUs    []int
	buildMemMB   []int
	buildNoCache []bool
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

func (m *mockSandboxRunner) BuildImage(_ context.Context, tag, containerfilePath, _ string, cpus, memoryMB int, noCache bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, _ := os.ReadFile(containerfilePath)
	m.built = append(m.built, tag+"::"+string(b))
	m.buildCPUs = append(m.buildCPUs, cpus)
	m.buildMemMB = append(m.buildMemMB, memoryMB)
	m.buildNoCache = append(m.buildNoCache, noCache)
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

	cf, err := RenderCustomContainerfile("fleet-sandbox:base", nil, []string{"graphviz"}, []string{"seaborn>=0.13"})
	if err != nil {
		t.Fatalf("RenderCustomContainerfile failed: %v", err)
	}
	if !strings.Contains(cf, "FROM ghcr.io/astral-sh/uv:python3.13-bookworm-slim") ||
		strings.Contains(cf, "FROM fleet-sandbox:base") ||
		!strings.Contains(cf, "apt-get install -y -o Acquire::ForceIPv4=true -o Acquire::Retries=3 --no-install-recommends graphviz") ||
		!strings.Contains(cf, `uv pip install --system --no-cache "seaborn>=0.13"`) ||
		!strings.HasSuffix(cf, "ENV HOME=/tmp MPLCONFIGDIR=/tmp/matplotlib\n") {
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

	// Switching channel with MaxActive=1 evicts the previous active container.
	cfgOne := eng.Config()
	cfgOne.Sandbox.MaxActive = 1
	if _, err := eng.UpdateConfig(cfgOne, false); err != nil {
		t.Fatal(err)
	}
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
	eng.sandbox.active["briefing"].lastUsed = time.Now().Add(-5 * time.Minute)
	eng.sandbox.mu.Unlock()
	eng.sandbox.ReapIdle(t.Context(), 1)
	eng.sandbox.mu.Lock()
	remaining := len(eng.sandbox.active)
	eng.sandbox.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected active sandbox to be reaped when idle, remaining=%d", remaining)
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
	if len(runner.built) != 1 || runner.buildCPUs[0] < 4 || runner.buildMemMB[0] < 4096 {
		t.Fatalf("expected single 4-CPU/4GB build, got built=%d cpus=%v mem=%v", len(runner.built), runner.buildCPUs, runner.buildMemMB)
	}
}

func TestSocketSandboxRunnerBridge(t *testing.T) {
	tmp := t.TempDir()
	sockDir := tmp
	if len(filepath.Join(sockDir, "sandbox.sock")) > 96 {
		var mkErr error
		sockDir, mkErr = os.MkdirTemp("", "fsb-")
		if mkErr != nil {
			t.Fatal(mkErr)
		}
		t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	}
	sockPath := filepath.Join(sockDir, "sandbox.sock")

	directFail := newMockSandboxRunner()
	directFail.binPath = ""
	directFail.statusErr = newErr("container system is not running: apiserver is not running and not registered with launchd", http.StatusServiceUnavailable)
	directFail.networkReady = false
	directFail.imageExists = map[string]bool{}

	// 1. Without socket, SystemStatus falls back to direct and appends sandbox-install guidance.
	sockRunner := NewSocketSandboxRunner(sockPath, directFail)
	if err := sockRunner.SystemStatus(t.Context()); err == nil || !strings.Contains(err.Error(), "sandbox-install") {
		t.Fatalf("expected sandbox-install hint when direct fails with launchd mismatch, got %v", err)
	}

	// 2. Start RunSandboxAgent with a healthy gui/<uid> runner and verify all 8 methods route over the 0600 socket.
	guiRunner := newMockSandboxRunner()
	guiRunner.execFn = func(opts SandboxExecOpts) (SandboxRawResult, error) {
		return SandboxRawResult{ExitCode: 0, Stdout: "via-socket:" + strings.Join(opts.Command, " ")}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunSandboxAgent(ctx, sockPath, guiRunner)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case aErr := <-errCh:
			t.Fatalf("RunSandboxAgent exited early (sock=%s): %v", sockPath, aErr)
		default:
		}
		if err := PingSandboxAgent(t.Context(), sockPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sandbox agent socket %s did not become ready", sockPath)
		}
		time.Sleep(20 * time.Millisecond)
	}
	info, err := os.Stat(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected 0600 socket permissions, got %o", perm)
	}

	if err := sockRunner.SystemStatus(t.Context()); err != nil {
		t.Fatalf("expected SystemStatus via socket to succeed even though direct fails: %v", err)
	}
	if !sockRunner.NetworkExists(t.Context(), DefaultSandboxNetwork) {
		t.Fatalf("expected NetworkExists true via socket")
	}
	if gw, err := sockRunner.EnsureNetwork(t.Context(), DefaultSandboxNetwork, DefaultSandboxSubnet); err != nil || gw == "" {
		t.Fatalf("expected EnsureNetwork via socket to succeed, gw=%q err=%v", gw, err)
	}
	if !sockRunner.ImageExists(t.Context(), DefaultSandboxImage) {
		t.Fatalf("expected ImageExists true via socket")
	}
	cfPath := filepath.Join(tmp, "Containerfile")
	_ = os.WriteFile(cfPath, []byte("FROM scratch\n"), 0o644)
	if out, err := sockRunner.BuildImage(t.Context(), "test:latest", cfPath, tmp, 4, 4096, false); err != nil || !strings.Contains(out, "built test:latest") {
		t.Fatalf("BuildImage via socket failed: out=%q err=%v", out, err)
	}
	if err := sockRunner.StartContainer(t.Context(), SandboxStartOpts{Name: "sb-test", Image: DefaultSandboxImage}); err != nil {
		t.Fatalf("StartContainer via socket failed: %v", err)
	}
	execRes, err := sockRunner.ExecContainer(t.Context(), SandboxExecOpts{Name: "sb-test", Command: []string{"echo", "hi"}})
	if err != nil || execRes.Stdout != "via-socket:echo hi" {
		t.Fatalf("ExecContainer via socket unexpected: res=%+v err=%v", execRes, err)
	}
	if err := sockRunner.StopRemoveContainer(t.Context(), "sb-test"); err != nil {
		t.Fatalf("StopRemoveContainer via socket failed: %v", err)
	}

	cancel()
	if aErr := <-errCh; aErr != nil {
		t.Fatalf("RunSandboxAgent returned unexpected error on shutdown: %v", aErr)
	}
}

func TestAppleContainerRunnerBuildImageDNSRecoveryAndTailError(t *testing.T) {
	if got := truncateTailText("short text", 20); got != "short text" {
		t.Fatalf("truncateTailText short = %q", got)
	}
	if got := truncateTailText("alpha-beta-gamma-tail", 10); got != "...gamma-tail" {
		t.Fatalf("truncateTailText long = %q", got)
	}

	tmp := t.TempDir()
	logFile := filepath.Join(tmp, "calls.log")
	stateFile := filepath.Join(tmp, "state")
	fakeBin := filepath.Join(tmp, "container")

	script := `#!/bin/sh
echo "$*" >> "` + logFile + `"
cmd="$1"
sub="$2"
if [ "$cmd" = "builder" ] && [ "$sub" = "status" ]; then
  if [ -f "` + stateFile + `.running" ]; then
    echo "builder is running"
    exit 0
  fi
  echo "builder is not running"
  exit 0
fi
if [ "$cmd" = "builder" ] && [ "$sub" = "start" ]; then
  touch "` + stateFile + `.running"
  exit 0
fi
if [ "$cmd" = "builder" ] && [ "$sub" = "delete" ]; then
  rm -f "` + stateFile + `.running"
  exit 0
fi
if [ "$cmd" = "build" ]; then
  if [ -f "` + stateFile + `.fail_tail" ]; then
    python3 -c 'print("PREFIX " + "x"*2000 + " FATAL_END_MARKER")'
    exit 1
  fi
  if [ ! -f "` + stateFile + `.dns_retried" ] && [ -f "` + stateFile + `.fail_dns_once" ]; then
    touch "` + stateFile + `.dns_retried"
    echo "Temporary failure resolving 'deb.debian.org'" >&2
    exit 1
  fi
  echo "build ok"
  exit 0
fi
exit 0
`
	if err := os.WriteFile(fakeBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &AppleContainerRunner{bin: fakeBin}
	cfPath := filepath.Join(tmp, "Containerfile")
	if err := os.WriteFile(cfPath, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Case 1: builder not running initially -> started with explicit --dns before build, and build itself passes --dns.
	if out, err := runner.BuildImage(t.Context(), "test:c1", cfPath, tmp, 4, 4096, false); err != nil || !strings.Contains(out, "build ok") {
		t.Fatalf("Case 1 BuildImage failed: out=%q err=%v", out, err)
	}
	calls1Bytes, _ := os.ReadFile(logFile)
	calls1 := string(calls1Bytes)
	startIdx := strings.Index(calls1, "builder start --cpus 4 --memory 4096M --dns 1.1.1.1 --dns 8.8.8.8")
	buildIdx := strings.Index(calls1, "build -t test:c1")
	if startIdx == -1 || buildIdx == -1 || startIdx > buildIdx {
		t.Fatalf("expected builder start with DNS before build, got log:\n%s", calls1)
	}
	if !strings.Contains(calls1, "-c 4 -m 4096M --dns 1.1.1.1 --dns 8.8.8.8") {
		t.Fatalf("expected container build command to include --dns flags, got log:\n%s", calls1)
	}

	// Case 2: already-running builder hits DNS failure -> deleted, restarted with DNS, retried cleanly.
	_ = os.WriteFile(logFile, nil, 0o644)
	_ = os.WriteFile(stateFile+".fail_dns_once", []byte("1"), 0o644)
	if out, err := runner.BuildImage(t.Context(), "test:c2", cfPath, tmp, 4, 4096, false); err != nil || !strings.Contains(out, "build ok") {
		t.Fatalf("Case 2 BuildImage DNS recovery failed: out=%q err=%v", out, err)
	}
	calls2Bytes, _ := os.ReadFile(logFile)
	calls2 := string(calls2Bytes)
	delIdx := strings.Index(calls2, "builder delete --force")
	restartIdx := strings.Index(calls2, "builder start --cpus 4 --memory 4096M --dns 1.1.1.1 --dns 8.8.8.8")
	if delIdx == -1 || restartIdx == -1 || delIdx > restartIdx {
		t.Fatalf("expected builder delete then DNS start, got log:\n%s", calls2)
	}

	// Case 3: persistent non-DNS failure keeps the tail of build output.
	_ = os.WriteFile(stateFile+".fail_tail", []byte("1"), 0o644)
	_, err := runner.BuildImage(t.Context(), "test:c3", cfPath, tmp, 4, 4096, false)
	if err == nil {
		t.Fatal("Case 3 expected BuildImage error")
	}
	if !strings.Contains(err.Error(), "FATAL_END_MARKER") || strings.Contains(err.Error(), "PREFIX ") {
		t.Fatalf("expected error to preserve tail and drop prefix, got:\n%s", err.Error())
	}
}

func TestEgressProxyCloseTerminatesActiveTunnels(t *testing.T) {
	upstreamLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upstreamLn.Close() }()

	upstreamDone := make(chan struct{})
	go func() {
		defer close(upstreamDone)
		conn, aErr := upstreamLn.Accept()
		if aErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = conn.Write([]byte("hello"))
		_, _ = io.Copy(io.Discard, conn)
	}()

	px, err := StartEgressProxy("127.0.0.1:0", []string{"api.example.com"}, func(_ context.Context, _ string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	px.dialTCP = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, upstreamLn.Addr().String())
	}

	proxyParsed, _ := url.Parse(px.ProxyURL())
	clientConn, err := net.DialTimeout("tcp", proxyParsed.Host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = clientConn.Close() }()

	tok := base64.StdEncoding.EncodeToString([]byte("fleet:" + px.token))
	reqStr := "CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\nProxy-Authorization: Basic " + tok + "\r\n\r\n"
	if _, err := io.WriteString(clientConn, reqStr); err != nil {
		t.Fatal(err)
	}
	_ = clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(clientConn)
	statusLine, err := br.ReadString('\n')
	if err != nil || !strings.Contains(statusLine, "200 Connection Established") {
		t.Fatalf("expected 200 Connection Established, got %q (err=%v)", statusLine, err)
	}
	for {
		hdrLine, hErr := br.ReadString('\n')
		if hErr != nil {
			t.Fatalf("reading CONNECT headers: %v", hErr)
		}
		if hdrLine == "\r\n" || hdrLine == "\n" {
			break
		}
	}

	// Wait for initial greeting bytes from upstream before closing proxy.
	helloBuf := make([]byte, 5)
	if _, err := io.ReadFull(br, helloBuf); err != nil || string(helloBuf) != "hello" {
		t.Fatalf("expected upstream hello over tunnel, got %q (err=%v)", string(helloBuf), err)
	}

	if err := px.Close(); err != nil {
		t.Fatalf("px.Close() failed: %v", err)
	}

	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	if _, err := br.Read(buf); err == nil {
		t.Fatalf("expected client tunnel connection to close on px.Close()")
	}
	select {
	case <-upstreamDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected upstream tunnel connection to close on px.Close()")
	}
}

func TestSandboxWarmLRUPoolAndProfiles(t *testing.T) {
	if err := ValidateSandboxProfiles([]string{"browser", "office", "media"}); err != nil {
		t.Fatalf("ValidateSandboxProfiles failed: %v", err)
	}
	if err := ValidateSandboxProfiles([]string{"invalid-profile"}); err == nil {
		t.Fatalf("expected invalid sandbox profile to be rejected")
	}

	cf, err := RenderCustomContainerfile("", []string{"browser", "office", "media"}, nil, nil)
	if err != nil {
		t.Fatalf("RenderCustomContainerfile with profiles failed: %v", err)
	}
	for _, want := range []string{"chromium", "playwright", "libreoffice-writer", "ffmpeg"} {
		if !strings.Contains(cf, want) {
			t.Fatalf("expected profile package %q in Containerfile:\n%s", want, cf)
		}
	}

	runner := newMockSandboxRunner()
	mgr := NewSandboxManager(t.TempDir(), runner)
	cfg := DefaultConfig().Sandbox
	cfg.Enabled = true
	if cfg.CPUs != 4 || cfg.MemoryMB != 4096 || cfg.IdleMinutes != 20 || cfg.MaxFileMB != 50 || cfg.MaxActive != 2 {
		t.Fatalf("unexpected 16GB default sandbox config: %+v", cfg)
	}

	// Start two channels concurrently -> both remain warm in pool.
	if _, err := mgr.Exec(t.Context(), cfg, "ch1", SandboxExecRequest{Command: "echo 1"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(15 * time.Millisecond)
	if _, err := mgr.Exec(t.Context(), cfg, "ch2", SandboxExecRequest{Command: "echo 2"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.stopped) != 0 {
		t.Fatalf("expected 0 evictions for MaxActive=2 with 2 channels, got %v", runner.stopped)
	}

	// Third channel evicts LRU ("ch1"), keeping "ch2" and "ch3".
	if _, err := mgr.Exec(t.Context(), cfg, "ch3", SandboxExecRequest{Command: "echo 3"}); err != nil {
		t.Fatal(err)
	}
	if len(runner.stopped) != 1 || runner.stopped[0] != "fleet-sb-ch1" {
		t.Fatalf("expected LRU fleet-sb-ch1 to be evicted, got stopped=%v", runner.stopped)
	}

	// Reset removes only target channel.
	if err := mgr.Reset(t.Context(), "ch2", false); err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	_, hasCH2 := mgr.active["ch2"]
	_, hasCH3 := mgr.active["ch3"]
	mgr.mu.Unlock()
	if hasCH2 || !hasCH3 {
		t.Fatalf("expected only ch2 removed on Reset, hasCH2=%v hasCH3=%v", hasCH2, hasCH3)
	}

	// Selective idle reap only removes expired entries.
	if _, err := mgr.Exec(t.Context(), cfg, "ch4", SandboxExecRequest{Command: "echo 4"}); err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	mgr.active["ch3"].lastUsed = time.Now().Add(-30 * time.Minute)
	mgr.active["ch4"].lastUsed = time.Now()
	mgr.mu.Unlock()
	mgr.ReapIdle(t.Context(), 20)
	mgr.mu.Lock()
	_, hasCH3After := mgr.active["ch3"]
	_, hasCH4After := mgr.active["ch4"]
	mgr.mu.Unlock()
	if hasCH3After || !hasCH4After {
		t.Fatalf("expected selective reap to drop ch3 and keep ch4, hasCH3=%v hasCH4=%v", hasCH3After, hasCH4After)
	}

	// BuildImages flushes active containers and allocates 6144 MB floor for browser/office profiles.
	cfg.Profiles = []string{"browser"}
	res, err := mgr.BuildImages(t.Context(), cfg, false)
	if err != nil {
		t.Fatalf("BuildImages failed: %v", err)
	}
	if len(res.Profiles) != 1 || res.Profiles[0] != "browser" {
		t.Fatalf("expected browser profile in build result: %+v", res)
	}
	if len(runner.buildMemMB) == 0 || runner.buildMemMB[len(runner.buildMemMB)-1] < 6144 {
		t.Fatalf("expected >=6144 MB build memory for browser profile, got %v", runner.buildMemMB)
	}
	mgr.mu.Lock()
	activeAfterBuild := len(mgr.active)
	mgr.mu.Unlock()
	if activeAfterBuild != 0 {
		t.Fatalf("expected active pool flushed after BuildImages, got %d", activeAfterBuild)
	}

	// Close stops all remaining warm containers.
	if _, err := mgr.Exec(t.Context(), cfg, "ch5", SandboxExecRequest{Command: "echo 5"}); err != nil {
		t.Fatal(err)
	}
	mgr.Close(t.Context())
	mgr.mu.Lock()
	activeAfterClose := len(mgr.active)
	mgr.mu.Unlock()
	if activeAfterClose != 0 {
		t.Fatalf("expected 0 active after Close, got %d", activeAfterClose)
	}
}
