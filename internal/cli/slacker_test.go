package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bdatdo0601/fleet/internal/slacker"
)

func TestCLISlackerManifestSetupAndSafeCleanup(t *testing.T) {
	f, _, tmp := setupCLIServer(t)
	slackMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"user_id":"U123","user":"datdo","team_id":"T1","team":"Home"}`))
		case "/apps.connections.open":
			_, _ = w.Write([]byte(`{"ok":true,"url":"wss://slack.example/ws"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(slackMock.Close)

	var out, errBuf bytes.Buffer
	if rc := Run([]string{"slacker", "manifest"}, &IO{Stdout: &out, Stderr: &errBuf}); rc != 0 || !strings.Contains(out.String(), "socket_mode_enabled: true") || !strings.Contains(out.String(), "reaction_added") {
		t.Fatalf("slacker manifest failed: rc=%d out=%q err=%q", rc, out.String(), errBuf.String())
	}

	out.Reset()
	errBuf.Reset()
	rc := Run([]string{
		"slacker", "setup",
		"--op-item", "Local/Fleet Slacker",
		"--slack-api-url", slackMock.URL,
		"--channel", "C111=mini:" + tmp,
		"--emoji", "claude=claude",
		"--emoji", "agy=agy",
	}, &IO{
		Stdin:  strings.NewReader(""),
		Stdout: &out,
		Stderr: &errBuf,
		ReadOPRef: func(ref string) (string, error) {
			if strings.HasSuffix(ref, "/app_token") {
				return "xapp-1-op-secret-1234", nil
			}
			return "xoxp-op-secret-5678", nil
		},
	})
	if rc != 0 {
		t.Fatalf("slacker setup failed: rc=%d out=%q err=%q", rc, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "enabled=true") || !strings.Contains(out.String(), "U123") {
		t.Fatalf("unexpected slacker setup output:\n%s", out.String())
	}

	// Verify cleanup removes local receipts/locks & terminal jobs while preserving repo directory.
	localState := filepath.Join(tmp, "slacker-local")
	receiptsDir := filepath.Join(localState, "receipts")
	_ = os.MkdirAll(receiptsDir, 0o755)
	_ = os.WriteFile(filepath.Join(receiptsDir, "job-1.json"), []byte("{}"), 0o600)
	repoFile := filepath.Join(tmp, "keep.txt")
	_ = os.WriteFile(repoFile, []byte("keep"), 0o644)

	_, j, _ := f.Slacker.EnqueueJob(slacker.Job{
		EventID: "EvCLI1", ChannelID: "C111", MessageTS: "1.1", TargetDevice: "mini",
		RepoPath: tmp, Harness: "claude", SessionName: "slack-c111-1-1", Prompt: "hi",
	})
	_, _ = f.Slacker.RecordReceipt("mini", slacker.Receipt{
		JobID: j.ID, ReceiptID: "r1", SessionName: j.SessionName, Backend: "tmux", Status: "launched",
	})

	out.Reset()
	errBuf.Reset()
	rc = Run([]string{"slacker", "cleanup", "--local-state-dir", localState, "--prune-jobs", "--yes"}, &IO{
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errBuf,
	})
	if rc != 0 {
		t.Fatalf("slacker cleanup failed: rc=%d out=%q err=%q", rc, out.String(), errBuf.String())
	}
	if _, err := os.Stat(receiptsDir); !os.IsNotExist(err) {
		t.Fatalf("expected receipts dir removed, got err=%v", err)
	}
	if _, err := os.Stat(repoFile); err != nil {
		t.Fatalf("cleanup must preserve repo files: %v", err)
	}
	if jobs, _ := f.Slacker.ListJobs("", "", 10); len(jobs) != 0 {
		t.Fatalf("expected terminal job pruned, got %+v", jobs)
	}
}

func TestCLISlackerMultiAccountReadSend(t *testing.T) {
	f, _, tmp := setupCLIServer(t)
	slackMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth.test":
			if strings.Contains(r.Header.Get("Authorization"), "xoxb-") {
				_, _ = w.Write([]byte(`{"ok":true,"user_id":"BWORK","user":"bot","team_id":"TWORK","team":"Work"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"user_id":"UWORK","user":"datwork","team_id":"TWORK","team":"Work"}`))
		case "/apps.connections.open":
			_, _ = w.Write([]byte(`{"ok":true,"url":"wss://slack.example/ws"}`))
		case "/conversations.history":
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{"ts":"11.1","user":"U1","text":"investigate queue"}]}`))
		case "/chat.postMessage":
			_, _ = w.Write([]byte(`{"ok":true,"channel":"CWORK","ts":"11.2"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(slackMock.Close)
	f.SlackClient = &slacker.SlackClient{BaseURL: slackMock.URL}

	var out, errBuf bytes.Buffer
	ioCfg := &IO{
		Stdin:  strings.NewReader(""),
		Stdout: &out,
		Stderr: &errBuf,
		ReadOPRef: func(ref string) (string, error) {
			switch {
			case strings.HasSuffix(ref, "/app_token"):
				return "xapp-work-111", nil
			case strings.HasSuffix(ref, "/bot_token"):
				return "xoxb-work-333", nil
			default:
				return "xoxp-work-222", nil
			}
		},
	}

	if rc := Run([]string{
		"slacker", "setup",
		"--account", "work",
		"--op-item", "Work/Slack",
		"--bot-token-op-ref", "op://Work/Slack/bot_token",
		"--slack-api-url", slackMock.URL,
		"--channel", "CWORK=mini:" + tmp,
	}, ioCfg); rc != 0 {
		t.Fatalf("multi-account setup failed: rc=%d out=%q err=%q", rc, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "[work]") || !strings.Contains(out.String(), "CWORK") {
		t.Fatalf("unexpected setup output:\n%s", out.String())
	}

	out.Reset()
	errBuf.Reset()
	if rc := Run([]string{"slacker", "accounts"}, &IO{Stdout: &out, Stderr: &errBuf}); rc != 0 || !strings.Contains(out.String(), "work") {
		t.Fatalf("slacker accounts failed: rc=%d out=%q err=%q", rc, out.String(), errBuf.String())
	}

	out.Reset()
	errBuf.Reset()
	if rc := Run([]string{"slacker", "read", "--account", "work", "--channel", "CWORK"}, &IO{Stdout: &out, Stderr: &errBuf}); rc != 0 || !strings.Contains(out.String(), "investigate queue") {
		t.Fatalf("slacker read failed: rc=%d out=%q err=%q", rc, out.String(), errBuf.String())
	}

	out.Reset()
	errBuf.Reset()
	if rc := Run([]string{"slacker", "send", "--account", "work", "--channel", "CWORK", "--text", "all clear"}, &IO{Stdout: &out, Stderr: &errBuf}); rc != 0 || !strings.Contains(out.String(), "ts=11.2") {
		t.Fatalf("slacker send failed: rc=%d out=%q err=%q", rc, out.String(), errBuf.String())
	}

	out.Reset()
	errBuf.Reset()
	if rc := Run([]string{"slacker", "account", "rm", "work"}, &IO{Stdout: &out, Stderr: &errBuf}); rc != 0 || strings.Contains(out.String(), "[work]") {
		t.Fatalf("slacker account rm failed: rc=%d out=%q err=%q", rc, out.String(), errBuf.String())
	}
}
