package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdatdo0601/fleet/internal/assistant"
	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/registry"
)

func TestAssistantRoutesSecurityAndFlow(t *testing.T) {
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat/completions" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Fleet assistant ready.\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}],\"usage\":{\"prompt_tokens\":200,\"completion_tokens\":6,\"cost\":0.0001}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}
	}))
	defer mockLLM.Close()

	f, srv, tmp := setupTestServer(t)
	_, office := approveTestDevice(t, f, tmp, "office", "mac", "managed")

	// Start assistant worker for this test.
	f.Assistant.Start(t.Context())
	defer f.Assistant.Stop()

	cfg := assistant.DefaultConfig()
	cfg.Enabled = true
	cfg.BaseURL = mockLLM.URL
	cfg.APIKey = "sk-or-test"
	personalDir := filepath.Join(tmp, "personal-vault")
	roDir := filepath.Join(tmp, "ref-vault")
	if err := os.MkdirAll(filepath.Join(personalDir, "Notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(personalDir, "Notes", "Travel.md"), []byte("# Japan Trip\nKyoto itinerary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(roDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.ExtraVaults = []assistant.VaultMount{
		{Name: "personal", Path: personalDir, AutoWriteDirs: []string{"Inbox"}},
		{Name: "ref", Path: roDir, ReadOnly: true},
	}
	if _, err := f.Assistant.UpdateConfig(cfg, false); err != nil {
		t.Fatal(err)
	}

	// Admin can load skills, create skill, schedule timer, and chat.
	if st, _, _ := doRawReq(t, srv.URL+"/v1/assistant/skills/daily-check", http.MethodPut, `{"content":"---\nname: daily-check\ndescription: Check waiting agents\nsync_to_fleet: true\n---\nRun check.\n"}`, nil, ""); st != http.StatusOK {
		t.Fatalf("PUT skill expected 200, got %d", st)
	}
	if syncRes, sErr := f.Sync(office, "test", "", ""); sErr != nil || len(syncRes.SyncedSkills) != 1 || syncRes.SyncedSkills[0].Name != "daily-check" {
		t.Fatalf("expected SyncResult.SyncedSkills to include daily-check, got %+v (err=%v)", syncRes.SyncedSkills, sErr)
	}
	if st, _, b := doRawReq(t, srv.URL+"/v1/assistant/vault/search?vault=all&q=Kyoto", http.MethodGet, "", nil, ""); st != http.StatusOK || !strings.Contains(string(b), `"vault":"personal"`) {
		t.Fatalf("GET /v1/assistant/vault/search?vault=all expected personal hit, got %d (%s)", st, string(b))
	}
	if st, _, b := doRawReq(t, srv.URL+"/v1/assistant/vault/file?vault=personal&path=Notes/Travel.md", http.MethodGet, "", nil, ""); st != http.StatusOK || !strings.Contains(string(b), "Kyoto itinerary") {
		t.Fatalf("GET /v1/assistant/vault/file?vault=personal expected 200, got %d (%s)", st, string(b))
	}
	if st, _, b := doRawReq(t, srv.URL+"/v1/assistant/vault/file", http.MethodPut, `{"vault":"personal","path":"Inbox/Trip.md","content":"Packed"}`, nil, ""); st != http.StatusOK || !strings.Contains(string(b), `"vault":"personal"`) {
		t.Fatalf("PUT /v1/assistant/vault/file personal expected 200, got %d (%s)", st, string(b))
	}
	if st, _, _ := doRawReq(t, srv.URL+"/v1/assistant/vault/file", http.MethodPut, `{"vault":"ref","path":"Readme.md","content":"nope"}`, nil, ""); st != http.StatusForbidden {
		t.Fatalf("PUT /v1/assistant/vault/file read-only expected 403, got %d", st)
	}
	if st, _, _ := doRawReq(t, srv.URL+"/v1/assistant/vault/file?path=../bad.md", http.MethodGet, "", nil, ""); st != http.StatusBadRequest {
		t.Fatalf("vault traversal expected 400, got %d", st)
	}
	if st, hdr, wb := doRawReq(t, srv.URL+"/assistant-widget", http.MethodGet, "", nil, ""); st != http.StatusOK || hdr.Get("X-Frame-Options") != "SAMEORIGIN" || !strings.Contains(hdr.Get("Content-Security-Policy"), "default-src 'none'") || !strings.Contains(string(wb), `"theme"`) || !strings.Contains(string(wb), "root.scrollHeight") {
		t.Fatalf("widget frame expected 200 + SAMEORIGIN + strict CSP + theme/root.scrollHeight, got %d %+v (%s)", st, hdr, string(wb))
	}
	if st, _, b := doRawReq(t, srv.URL+"/v1/assistant/doctor", http.MethodGet, "", nil, ""); st != http.StatusOK || !strings.Contains(string(b), `"checks"`) {
		t.Fatalf("GET /v1/assistant/doctor expected 200 with checks, got %d (%s)", st, string(b))
	}
	if st, _, b := doRawReq(t, srv.URL+"/v1/assistant/op-token", http.MethodPost, `{"token":"ops_test_token"}`, nil, ""); st != http.StatusOK || !strings.Contains(string(b), `"check"`) {
		t.Fatalf("POST /v1/assistant/op-token expected 200, got %d (%s)", st, string(b))
	}
	if st, _, b := doRawReq(t, srv.URL+"/v1/assistant/outbox", http.MethodGet, "", nil, ""); st != http.StatusOK || !strings.Contains(string(b), `"items"`) {
		t.Fatalf("GET /v1/assistant/outbox expected 200, got %d (%s)", st, string(b))
	}
	if st, _, b := doRawReq(t, srv.URL+"/v1/assistant/delegations", http.MethodGet, "", nil, ""); st != http.StatusOK || !strings.Contains(string(b), `"delegations"`) {
		t.Fatalf("GET /v1/assistant/delegations expected 200, got %d (%s)", st, string(b))
	}
	if v, vErr := f.Assistant.Vault(); vErr == nil {
		for _, args := range [][]string{
			{"-C", v.Root, "init"},
			{"-C", v.Root, "config", "user.email", "test@fleet.local"},
			{"-C", v.Root, "config", "user.name", "Fleet Test"},
			{"-C", v.Root, "add", "-A"},
			{"-C", v.Root, "commit", "-m", "seed"},
		} {
			_ = exec.Command("git", args...).Run()
		}
		if st, _, b := doRawReq(t, srv.URL+"/v1/assistant/vault/history?vault=agent", http.MethodGet, "", nil, ""); st != http.StatusOK || !strings.Contains(string(b), `"commits"`) {
			t.Fatalf("GET /v1/assistant/vault/history expected 200, got %d (%s)", st, string(b))
		}
	}

	// Post message and wait until worker persists reply.
	if st, _, body := doRawReq(t, srv.URL+"/v1/assistant/messages", http.MethodPost, `{"channel":"general","content":"hi"}`, nil, ""); st != http.StatusAccepted {
		t.Fatalf("POST message expected 202, got %d: %s", st, string(body))
	}
	deadline := time.Now().Add(4 * time.Second)
	var gotReply bool
	for time.Now().Before(deadline) {
		var res struct {
			Messages []assistant.Message `json:"messages"`
		}
		if err := client.Call(srv.URL, http.MethodGet, "/v1/assistant/messages?channel=general", nil, 3*time.Second, nil, &res); err == nil {
			for _, m := range res.Messages {
				if m.Role == "assistant" && strings.Contains(m.Content, "Fleet assistant ready.") {
					gotReply = true
					break
				}
			}
		}
		if gotReply {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !gotReply {
		t.Fatalf("expected persisted assistant reply")
	}

	// Default POST reset clears channel messages while preserving long-term memory.
	if st, _, b := doRawReq(t, srv.URL+"/v1/assistant/channels/general/reset", http.MethodPost, `{}`, nil, ""); st != http.StatusOK || !strings.Contains(string(b), `"cleared_history":true`) || !strings.Contains(string(b), `"long_term_memory_preserved":true`) {
		t.Fatalf("POST /v1/assistant/channels/general/reset failed: %d (%s)", st, string(b))
	}
	var afterReset struct {
		Messages []assistant.Message `json:"messages"`
	}
	if err := client.Call(srv.URL, http.MethodGet, "/v1/assistant/messages?channel=general", nil, 3*time.Second, nil, &afterReset); err != nil {
		t.Fatal(err)
	}
	if len(afterReset.Messages) != 0 {
		t.Fatalf("expected #general history cleared after reset, got %+v", afterReset.Messages)
	}

	// Managed device must get 403 on assistant endpoints.
	f.IdentifyOverride = func(string) *registry.Device { return office }
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/assistant/status"},
		{http.MethodGet, "/v1/assistant/doctor"},
		{http.MethodPost, "/v1/assistant/op-token"},
		{http.MethodGet, "/v1/assistant/skills"},
		{http.MethodPost, "/v1/assistant/messages"},
	} {
		st, _, b := doRawReq(t, srv.URL+tc.path, tc.method, "{}", nil, "")
		if st != http.StatusForbidden {
			t.Fatalf("managed %s %s expected 403, got %d (%s)", tc.method, tc.path, st, string(b))
		}
		var errBody map[string]string
		_ = json.Unmarshal(b, &errBody)
	}
}
