package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/codes"
	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/pki"
	"github.com/bdatdo0601/fleet/internal/server"
)

const testPass = "correct horse battery staple"

func setupCLIServer(t *testing.T) (*core.Fleet, *httptest.Server, string) {
	t.Helper()
	if _, err := exec.LookPath("nebula-cert"); err != nil {
		t.Skip("nebula-cert not installed")
	}
	tmp := t.TempDir()
	pkiDir := filepath.Join(tmp, "etc")
	_ = os.MkdirAll(pkiDir, 0o755)
	t.Setenv("FLEET_NEBULA_ETC", pkiDir)
	t.Setenv("FLEET_STATE_DIR", filepath.Join(tmp, "state"))

	pubFile := filepath.Join(tmp, "mini.pub")
	if err := pki.Keygen(filepath.Join(tmp, "mini.key"), pubFile); err != nil {
		t.Fatal(err)
	}
	miniPub, _ := os.ReadFile(pubFile)
	f, err := core.Initialize(core.InitOpts{
		Root: filepath.Join(tmp, "state"), Host: "home.example.com", Port: 443,
		LanIP: "192.168.1.50", Passphrase: testPass, LighthousePubkey: string(miniPub),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	srv := httptest.NewServer(server.NewHandler(f, true))
	t.Cleanup(srv.Close)

	nodeBytes, _ := json.Marshal(client.NodeConfig{Name: "mini", API: srv.URL})
	_ = os.WriteFile(filepath.Join(pkiDir, "fleet.json"), nodeBytes, 0o644)
	return f, srv, tmp
}

func TestParseFlagsInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	force := fs.Bool("force", false, "")
	group := fs.String("group", "managed", "")
	pos, err := parseFlagsInterspersed(fs, []string{"laptop", "--force", "--group", "admin"})
	if err != nil || len(pos) != 1 || pos[0] != "laptop" || !*force || *group != "admin" {
		t.Fatalf("unexpected interspersed parse: pos=%v force=%v group=%s err=%v", pos, *force, *group, err)
	}
}

func TestCLIApproveReasksPassphraseAndLSAndAgents(t *testing.T) {
	f, _, tmp := setupCLIServer(t)
	lapPubFile := filepath.Join(tmp, "lap.pub")
	_ = pki.Keygen(filepath.Join(tmp, "lap.key"), lapPubFile)
	lapPub, _ := os.ReadFile(lapPubFile)

	reqBlock, err := codes.Encode(codes.Request, map[string]any{"v": 1, "name": "laptop", "pubkey": string(lapPub)})
	if err != nil {
		t.Fatal(err)
	}

	calls := 0
	var out, errBuf bytes.Buffer
	rc := Run([]string{"approve", "--group", "admin"}, &IO{
		Stdin:  strings.NewReader(reqBlock + "\n"),
		Stdout: &out,
		Stderr: &errBuf,
		ConfirmFn: func(string) error {
			return nil
		},
		AskPassphrase: func(bool) (string, error) {
			calls++
			if calls == 1 {
				return "typo", nil
			}
			return testPass, nil
		},
	})
	if rc != 0 || calls != 2 || !strings.Contains(out.String(), "wrong CA passphrase, try again") {
		t.Fatalf("approve failed: rc=%d calls=%d out=%q err=%q", rc, calls, out.String(), errBuf.String())
	}

	var resp map[string]any
	if err := codes.Decode(codes.Response, out.String(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["name"] != "laptop" || resp["ip"] != "10.213.0.2" {
		t.Fatalf("unexpected decoded approve response: %+v", resp)
	}

	// `fleet ls` includes CLIENT column
	var lsOut, lsErr bytes.Buffer
	rc = Run([]string{"ls"}, &IO{Stdin: strings.NewReader(""), Stdout: &lsOut, Stderr: &lsErr})
	if rc != 0 || !strings.Contains(lsOut.String(), "CLIENT") || !strings.Contains(lsOut.String(), "laptop") {
		t.Fatalf("ls failed: rc=%d out=%q err=%q", rc, lsOut.String(), lsErr.String())
	}

	// `fleet agents` & `fleet agents rename`
	_, _ = f.Agents.Heartbeat(agents.HeartbeatParams{
		Device: "mini", Harness: "antigravity", RawID: "agy-ss1", CWD: tmp,
		State:         "waiting_permission",
		PendingAction: map[string]string{"kind": "permission", "tool": "ask_permission", "detail": "Write /etc/hosts", "event": "PreToolUse"},
	})
	var rnOut bytes.Buffer
	rc = Run([]string{"agents", "rename", "agy-ss1", "ui-lead"}, &IO{Stdin: strings.NewReader(""), Stdout: &rnOut, Stderr: &lsErr})
	if rc != 0 || !strings.Contains(rnOut.String(), "mini:antigravity:agy-ss1 -> ui-lead") {
		t.Fatalf("rename failed: rc=%d out=%q", rc, rnOut.String())
	}
	var agOut bytes.Buffer
	rc = Run([]string{"agents", "--waiting"}, &IO{Stdin: strings.NewReader(""), Stdout: &agOut, Stderr: &lsErr})
	if rc != 0 || !strings.Contains(agOut.String(), "ui-lead") || !strings.Contains(agOut.String(), "waiting_permission") {
		t.Fatalf("agents --waiting failed: rc=%d out=%q", rc, agOut.String())
	}
}

func TestCLIReportsErrorsWithoutStacktrace(t *testing.T) {
	t.Setenv("FLEET_NEBULA_ETC", t.TempDir())
	var out, errBuf bytes.Buffer
	rc := Run([]string{"ls"}, &IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errBuf})
	if rc != 1 || !strings.Contains(errBuf.String(), "hasn't joined a fleet") {
		t.Fatalf("unexpected error output: rc=%d err=%q", rc, errBuf.String())
	}
}

func TestParseSessionActionArgs(t *testing.T) {
	parsed, err := parseSessionActionArgs([]string{"start", "--rex", "--model", "pro", "my-task", "/tmp"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !parsed.useRex || parsed.useTmux || parsed.model != "pro" || len(parsed.positional) != 3 {
		t.Fatalf("unexpected parsed args: %+v", parsed)
	}

	parsedEq, err := parseSessionActionArgs([]string{"start", "--tmux", "--model=flash", "my-task", "/tmp"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsedEq.useRex || !parsedEq.useTmux || parsedEq.model != "flash" {
		t.Fatalf("unexpected parsed = args: %+v", parsedEq)
	}

	if _, err := parseSessionActionArgs([]string{"start", "--rex", "--tmux", "my-task", "/tmp"}); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("expected --rex/--tmux conflict error, got %v", err)
	}
}

func TestCLIUsageAndStatusLine(t *testing.T) {
	_, srv, _ := setupCLIServer(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	wrapScript := filepath.Join(home, "wrap.sh")
	if err := os.WriteFile(wrapScript, []byte("#!/bin/sh\ncat >/dev/null\necho 'ok-status'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := `{"model":{"display_name":"Claude Opus"},"rate_limits":{"five_hour":{"used_percentage":18.0,"resets_at":1796000000}}}`
	var out, errBuf bytes.Buffer
	rc := Run([]string{"statusline", "claude", "--wrap", wrapScript}, &IO{
		Stdin: strings.NewReader(payload), Stdout: &out, Stderr: &errBuf,
	})
	if rc != 0 || strings.TrimSpace(out.String()) != "ok-status" {
		t.Fatalf("statusline claude failed: rc=%d out=%q err=%q", rc, out.String(), errBuf.String())
	}

	out.Reset()
	errBuf.Reset()
	rc = Run([]string{"usage", "--cached", "--json"}, &IO{
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errBuf,
	})
	if rc != 0 || !strings.Contains(out.String(), `"harness": "claude"`) || !strings.Contains(out.String(), `"used_percent": 18`) {
		t.Fatalf("usage --cached --json failed: rc=%d out=%q err=%q", rc, out.String(), errBuf.String())
	}

	// Verify compact usage footer appears in `fleet agents` output too.
	out.Reset()
	if err := client.Call(srv.URL, "POST", "/v1/agents/heartbeat", map[string]any{
		"harness": "claude", "raw_id": "s1", "cwd": home, "state": "working",
	}, 5*time.Second, nil, nil); err != nil {
		t.Fatal(err)
	}
	rc = Run([]string{"agents"}, &IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errBuf})
	if rc != 0 || !strings.Contains(out.String(), "Usage: claude 5h 18%") {
		t.Fatalf("expected compact usage footer in fleet agents, got rc=%d out=%q", rc, out.String())
	}
}

func TestCLIAssistantAutoApproveFlags(t *testing.T) {
	f, _, _ := setupCLIServer(t)

	var out, errBuf bytes.Buffer
	// Enable global auto-approve via CLI.
	rc := Run([]string{"assistant", "setup", "--auto-approve", "--no-test"}, &IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errBuf})
	if rc != 0 || !strings.Contains(out.String(), "auto_approve=true") {
		t.Fatalf("assistant setup --auto-approve failed: rc=%d out=%q err=%q", rc, out.String(), errBuf.String())
	}
	if !f.Assistant.Config().AutoApprove {
		t.Fatalf("expected Config.AutoApprove=true after CLI setup")
	}

	// Verify status reports both flags.
	out.Reset()
	errBuf.Reset()
	rc = Run([]string{"assistant", "status"}, &IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errBuf})
	if rc != 0 || !strings.Contains(out.String(), "auto_approve=true") || !strings.Contains(out.String(), "discord_auto_approve=false") {
		t.Fatalf("assistant status unexpected: rc=%d out=%q err=%q", rc, out.String(), errBuf.String())
	}

	// Enable Discord auto-approve without clobbering global auto-approve, then disable both explicitly.
	out.Reset()
	rc = Run([]string{"assistant", "setup", "--discord-auto-approve", "--no-test"}, &IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errBuf})
	if rc != 0 || !f.Assistant.Config().AutoApprove || !f.Assistant.Config().Discord.AutoApprove {
		t.Fatalf("assistant setup --discord-auto-approve failed: rc=%d cfg=%+v err=%q", rc, f.Assistant.Config(), errBuf.String())
	}

	out.Reset()
	rc = Run([]string{"assistant", "setup", "--no-auto-approve", "--discord-no-auto-approve", "--no-test"}, &IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errBuf})
	if rc != 0 || f.Assistant.Config().AutoApprove || f.Assistant.Config().Discord.AutoApprove {
		t.Fatalf("assistant setup --no-auto-approve/--discord-no-auto-approve failed: rc=%d cfg=%+v err=%q", rc, f.Assistant.Config(), errBuf.String())
	}

	// Conflicting flags must fail cleanly.
	errBuf.Reset()
	if rc := Run([]string{"assistant", "setup", "--auto-approve", "--no-auto-approve", "--no-test"}, &IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errBuf}); rc == 0 {
		t.Fatalf("expected conflicting --auto-approve/--no-auto-approve to fail")
	}
	errBuf.Reset()
	if rc := Run([]string{"assistant", "setup", "--discord-auto-approve", "--discord-no-auto-approve", "--no-test"}, &IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errBuf}); rc == 0 {
		t.Fatalf("expected conflicting --discord-auto-approve/--discord-no-auto-approve to fail")
	}

	// `fleet assistant ask --auto-approve` enqueues inbox event with auto_approve=true.
	out.Reset()
	errBuf.Reset()
	rc = Run([]string{"assistant", "ask", "--auto-approve", "write", "report"}, &IO{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errBuf})
	if rc != 0 || !strings.Contains(out.String(), "queued message") {
		t.Fatalf("assistant ask --auto-approve failed: rc=%d out=%q err=%q", rc, out.String(), errBuf.String())
	}
	ev, err := f.Assistant.Store().ClaimNextInbox()
	if err != nil || ev == nil || !strings.Contains(ev.PayloadJSON, `"auto_approve":true`) {
		t.Fatalf("expected queued inbox payload with auto_approve=true, got %+v (err=%v)", ev, err)
	}
}
