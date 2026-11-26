package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/registry"
	"github.com/bdatdo0601/fleet/internal/slacker"
)

func TestSlackerAPIRoutesSecurityAndWebUI(t *testing.T) {
	f, srv, tmp := setupTestServer(t)
	_, office := approveTestDevice(t, f, tmp, "office", "mac", "managed")

	slackMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"user_id":"U999","user":"datdo","team_id":"T1","team":"Home"}`))
		case "/apps.connections.open":
			_, _ = w.Write([]byte(`{"ok":true,"url":"wss://slack.example/ws"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(slackMock.Close)
	f.SlackClient = &slacker.SlackClient{BaseURL: slackMock.URL}

	// Web asset for #slacker panel is served with JS content-type.
	st, hdr, body := doRawReq(t, srv.URL+"/assets/slacker.js", http.MethodGet, "", nil, "")
	if st != http.StatusOK || !strings.HasPrefix(hdr.Get("Content-Type"), "text/javascript") || len(body) == 0 {
		t.Fatalf("GET /assets/slacker.js failed: st=%d ct=%q len=%d", st, hdr.Get("Content-Type"), len(body))
	}

	// Admin saves Slacker config with verify=true and secret redaction.
	var saveRes struct {
		Config slacker.ConfigView `json:"config"`
	}
	if err := client.Call(srv.URL, http.MethodPost, "/v1/slacker/config", map[string]any{
		"enabled":            true,
		"app_token":          "xapp-1-secret-token-1234",
		"user_token":         "xoxp-user-secret-token-5678",
		"authorized_user_id": "U999",
		"channels": map[string]any{
			"C1": map[string]any{"channel_id": "C1", "device": "office", "repo_path": "/tmp/repo"},
		},
		"emojis": map[string]string{"claude": "claude", "agy": "agy"},
		"verify": true,
	}, 5*time.Second, nil, &saveRes); err != nil {
		t.Fatalf("POST /v1/slacker/config failed: %v", err)
	}
	if !saveRes.Config.Enabled || !saveRes.Config.Configured || strings.Contains(saveRes.Config.AppTokenHint, "secret-token-1234") {
		t.Fatalf("unexpected saved config view: %+v", saveRes.Config)
	}

	// Partial update with blank tokens preserves stored secrets.
	if err := client.Call(srv.URL, http.MethodPost, "/v1/slacker/config", map[string]any{
		"app_token":  "",
		"user_token": "",
		"emojis":     map[string]string{"claude": "claude", "agy": "agy", "codex": "codex"},
	}, 5*time.Second, nil, &saveRes); err != nil {
		t.Fatalf("partial POST /v1/slacker/config failed: %v", err)
	}
	rawCfg, err := slacker.LoadConfig(f.SlackerConfigPath)
	if err != nil || rawCfg.Accounts[slacker.DefaultAccountID].AppToken != "xapp-1-secret-token-1234" || len(rawCfg.Emojis) != 3 {
		t.Fatalf("tokens not preserved on partial update: %+v err=%v", rawCfg, err)
	}

	// Enqueue job for office, verify overview summary & slacker logs endpoint.
	_, job, err := f.Slacker.EnqueueJob(slacker.Job{
		EventID: "EvSrv1", ChannelID: "C1", MessageTS: "55.1", Reaction: "claude", UserID: "U999",
		TargetDevice: "office", RepoPath: "/tmp/repo", Harness: "claude", SessionName: "slack-c1-55-1", Prompt: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(f.Root, "slacker.log"), []byte("listener ready\n"), 0o644)
	var logRes struct {
		Lines []string `json:"lines"`
	}
	if err := client.Call(srv.URL, http.MethodGet, "/v1/logs?name=slacker&lines=10", nil, 5*time.Second, nil, &logRes); err != nil {
		t.Fatal(err)
	}
	if len(logRes.Lines) != 1 || logRes.Lines[0] != "listener ready" {
		t.Fatalf("unexpected slacker log tail: %+v", logRes)
	}

	var ov map[string]any
	if err := client.Call(srv.URL, http.MethodGet, "/v1/overview", nil, 5*time.Second, nil, &ov); err != nil {
		t.Fatal(err)
	}
	if ov["slacker"] == nil {
		t.Fatalf("expected slacker summary in admin overview")
	}

	// Managed device (office) blocked from admin slacker routes, but allowed to poll and submit its own receipt.
	f.IdentifyOverride = func(string) *registry.Device { return office }
	for _, ep := range []string{"/v1/slacker/status", "/v1/slacker/config", "/v1/slacker/jobs"} {
		err := client.Call(srv.URL, http.MethodGet, ep, nil, 5*time.Second, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
			t.Fatalf("managed GET %s expected 403, got %v", ep, err)
		}
	}

	var pollRes struct {
		Job    *slacker.Job `json:"job"`
		Device string       `json:"device"`
	}
	if err := client.Call(srv.URL, http.MethodPost, "/v1/slacker/worker/poll", map[string]any{"timeout": 0.2}, 5*time.Second, nil, &pollRes); err != nil {
		t.Fatal(err)
	}
	if pollRes.Job == nil || pollRes.Job.ID != job.ID {
		t.Fatalf("expected office to claim %s, got %+v", job.ID, pollRes)
	}
	if err := client.Call(srv.URL, http.MethodPost, "/v1/slacker/worker/receipt", slacker.Receipt{
		JobID: job.ID, ReceiptID: "rcpt-1", SessionName: job.SessionName, Backend: "tmux", Status: "launched",
	}, 5*time.Second, nil, nil); err != nil {
		t.Fatal(err)
	}

	// Cross-device receipt must be rejected with 403.
	f.IdentifyOverride = nil
	_, miniJob, _ := f.Slacker.EnqueueJob(slacker.Job{
		EventID: "EvMini", ChannelID: "C1", MessageTS: "55.2", Reaction: "claude", UserID: "U999",
		TargetDevice: "mini", RepoPath: "/tmp/repo", Harness: "claude", SessionName: "slack-c1-55-2", Prompt: "mini only",
	})
	f.IdentifyOverride = func(string) *registry.Device { return office }
	err = client.Call(srv.URL, http.MethodPost, "/v1/slacker/worker/receipt", slacker.Receipt{
		JobID: miniJob.ID, ReceiptID: "rcpt-hijack", SessionName: miniJob.SessionName, Backend: "tmux", Status: "launched",
	}, 5*time.Second, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("expected 403 on cross-device receipt, got %v", err)
	}

	// Admin retry/cancel/cleanup endpoints.
	f.IdentifyOverride = nil
	if err := client.Call(srv.URL, http.MethodPost, "/v1/slacker/jobs/"+miniJob.ID+"/cancel", map[string]any{}, 5*time.Second, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(srv.URL, http.MethodPost, "/v1/slacker/jobs/"+miniJob.ID+"/retry", map[string]any{}, 5*time.Second, nil, nil); err != nil {
		t.Fatal(err)
	}
	var cleanRes map[string]int64
	if err := client.Call(srv.URL, http.MethodPost, "/v1/slacker/cleanup", map[string]any{"all_terminal": true}, 5*time.Second, nil, &cleanRes); err != nil {
		t.Fatal(err)
	}
	if cleanRes["pruned_jobs"] != 1 {
		t.Fatalf("expected 1 pruned terminal job, got %+v", cleanRes)
	}
	_ = json.Marshal
}

func TestSlackerMultiAccountAndAgentReadSend(t *testing.T) {
	f, srv, tmp := setupTestServer(t)
	_, office := approveTestDevice(t, f, tmp, "office", "mac", "managed")

	var lastPostAuth, lastPostBody string
	slackMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth.test":
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			switch {
			case strings.Contains(tok, "work"):
				_, _ = w.Write([]byte(`{"ok":true,"user_id":"UWORK","user":"dat-work","team_id":"TWORK","team":"Work"}`))
			default:
				_, _ = w.Write([]byte(`{"ok":true,"user_id":"UPERS","user":"dat-pers","team_id":"TPERS","team":"Personal"}`))
			}
		case "/apps.connections.open":
			_, _ = w.Write([]byte(`{"ok":true,"url":"wss://slack.example/ws"}`))
		case "/users.conversations":
			_, _ = w.Write([]byte(`{"ok":true,"channels":[{"id":"CWORK","name":"eng","is_channel":true,"is_member":true}]}`))
		case "/conversations.history":
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{"ts":"101.1","user":"U1","text":"deploy issue"}]}`))
		case "/conversations.replies":
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{"ts":"101.0","user":"U1","text":"parent"},{"ts":"101.1","user":"U2","text":"reply"}]}`))
		case "/search.messages":
			_, _ = w.Write([]byte(`{"ok":true,"messages":{"matches":[{"ts":"202.2","user":"U3","text":"search hit","channel":{"id":"CWORK","name":"eng"}}]}}`))
		case "/chat.postMessage":
			lastPostAuth = r.Header.Get("Authorization")
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			raw, _ := json.Marshal(b)
			lastPostBody = string(raw)
			_, _ = w.Write([]byte(`{"ok":true,"channel":"` + b["channel"].(string) + `","ts":"999.1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(slackMock.Close)
	f.SlackClient = &slacker.SlackClient{BaseURL: slackMock.URL}

	// Configure two accounts ("personal" and "work") with channel mappings on each.
	var cfgRes struct {
		Config slacker.ConfigView `json:"config"`
	}
	if err := client.Call(srv.URL, http.MethodPost, "/v1/slacker/config", map[string]any{
		"account":            "personal",
		"enabled":            true,
		"app_token":          "xapp-pers-1234",
		"user_token":         "xoxp-pers-5678",
		"authorized_user_id": "UPERS",
		"channels": map[string]any{
			slacker.ChannelKey("personal", "CPERS"): map[string]any{
				"account": "personal", "channel_id": "CPERS", "device": "office", "repo_path": "/tmp/pers",
			},
			slacker.ChannelKey("work", "CWORK"): map[string]any{
				"account": "work", "channel_id": "CWORK", "device": "office", "repo_path": "/tmp/work",
			},
		},
		"verify": true,
	}, 5*time.Second, nil, &cfgRes); err != nil {
		t.Fatalf("save personal account failed: %v", err)
	}
	if err := client.Call(srv.URL, http.MethodPost, "/v1/slacker/config", map[string]any{
		"account":            "work",
		"enabled":            true,
		"app_token":          "xapp-work-1234",
		"user_token":         "xoxp-work-5678",
		"bot_token":          "xoxb-work-bot9",
		"authorized_user_id": "UWORK",
		"verify":             true,
	}, 5*time.Second, nil, &cfgRes); err != nil {
		t.Fatalf("save work account failed: %v", err)
	}
	if len(cfgRes.Config.Accounts) != 2 || !cfgRes.Config.Accounts["work"].HasBotToken {
		t.Fatalf("expected 2 accounts with work bot token, got %+v", cfgRes.Config.Accounts)
	}

	// Managed device ("office") can list channels, read history/thread/search, and send to mapped channels.
	f.IdentifyOverride = func(string) *registry.Device { return office }

	var chRes struct {
		Accounts []slacker.AccountView      `json:"accounts"`
		Channels []slacker.SlackChannelInfo `json:"channels"`
		Mapped   []slacker.ChannelMapping   `json:"mapped_channels"`
	}
	if err := client.Call(srv.URL, http.MethodGet, "/v1/slacker/channels?account=work&include_all=true", nil, 5*time.Second, nil, &chRes); err != nil {
		t.Fatalf("GET /v1/slacker/channels failed: %v", err)
	}
	if len(chRes.Mapped) != 1 || chRes.Mapped[0].ChannelID != "CWORK" || len(chRes.Channels) != 1 {
		t.Fatalf("unexpected channels response: %+v", chRes)
	}

	var readRes slacker.ReadMessagesResult
	if err := client.Call(srv.URL, http.MethodGet, "/v1/slacker/messages?channel=CWORK&thread_ts=101.0", nil, 5*time.Second, nil, &readRes); err != nil {
		t.Fatalf("GET thread failed: %v", err)
	}
	if readRes.Account != "work" || readRes.Mode != "thread" || len(readRes.Messages) != 2 {
		t.Fatalf("unexpected thread read response: %+v", readRes)
	}
	if err := client.Call(srv.URL, http.MethodGet, "/v1/slacker/messages?account=work&query=deploy", nil, 5*time.Second, nil, &readRes); err != nil {
		t.Fatalf("GET search failed: %v", err)
	}
	if readRes.Mode != "search" || len(readRes.Messages) != 1 {
		t.Fatalf("unexpected search read response: %+v", readRes)
	}

	// Unmapped channel blocked by default with 403.
	err := client.Call(srv.URL, http.MethodPost, "/v1/slacker/send", map[string]any{
		"account": "personal", "channel": "CUNMAPPED", "text": "blocked",
	}, 5*time.Second, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("expected 403 on unmapped send, got %v", err)
	}

	// Mapped channel on work uses bot_token; mapped channel on personal uses user_token.
	var postRes slacker.PostedMessage
	if err := client.Call(srv.URL, http.MethodPost, "/v1/slacker/send", map[string]any{
		"channel": "CWORK", "thread_ts": "101.0", "text": "fix landed",
	}, 5*time.Second, nil, &postRes); err != nil {
		t.Fatalf("send work failed: %v", err)
	}
	if postRes.Account != "work" || lastPostAuth != "Bearer xoxb-work-bot9" || !strings.Contains(lastPostBody, "fix landed") {
		t.Fatalf("expected work bot_token send, got %+v auth=%q body=%q", postRes, lastPostAuth, lastPostBody)
	}
	if err := client.Call(srv.URL, http.MethodPost, "/v1/slacker/send", map[string]any{
		"channel": "CPERS", "text": "personal update",
	}, 5*time.Second, nil, &postRes); err != nil {
		t.Fatalf("send personal failed: %v", err)
	}
	if postRes.Account != "personal" || lastPostAuth != "Bearer xoxp-pers-5678" {
		t.Fatalf("expected personal user_token fallback, got %+v auth=%q", postRes, lastPostAuth)
	}

	// Admin enables allow_unmapped_send on personal -> unmapped send now succeeds; deleting work cleans its mappings.
	f.IdentifyOverride = nil
	if err := client.Call(srv.URL, http.MethodPost, "/v1/slacker/config", map[string]any{
		"account": "personal", "allow_unmapped_send": true,
	}, 5*time.Second, nil, &cfgRes); err != nil {
		t.Fatalf("enable allow_unmapped_send failed: %v", err)
	}
	f.IdentifyOverride = func(string) *registry.Device { return office }
	if err := client.Call(srv.URL, http.MethodPost, "/v1/slacker/send", map[string]any{
		"account": "personal", "channel": "CUNMAPPED", "text": "allowed now",
	}, 5*time.Second, nil, &postRes); err != nil {
		t.Fatalf("expected unmapped send to succeed when allowed: %v", err)
	}

	f.IdentifyOverride = nil
	var delRes struct {
		Config slacker.ConfigView `json:"config"`
	}
	if err := client.Call(srv.URL, http.MethodPost, "/v1/slacker/config", map[string]any{
		"delete_account": "work",
	}, 5*time.Second, nil, &delRes); err != nil {
		t.Fatalf("delete work account failed: %v", err)
	}
	if _, stillThere := delRes.Config.Accounts["work"]; stillThere {
		t.Fatalf("expected work account deleted, got %+v", delRes.Config.Accounts)
	}
	if _, mappedStillThere := delRes.Config.Channels[slacker.ChannelKey("work", "CWORK")]; mappedStillThere {
		t.Fatalf("expected work channel mappings cleaned up, got %+v", delRes.Config.Channels)
	}
}
