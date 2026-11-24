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

	"github.com/bdatdo0601/fleet/internal/agents"
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

	nodeBytes, _ := json.Marshal(NodeConfig{Name: "mini", API: srv.URL})
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
