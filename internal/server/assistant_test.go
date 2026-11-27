package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	if _, err := f.Assistant.UpdateConfig(cfg, false); err != nil {
		t.Fatal(err)
	}

	// Admin can load skills, create skill, schedule timer, and chat.
	if st, _, _ := doRawReq(t, srv.URL+"/v1/assistant/skills/daily-check", http.MethodPut, `{"content":"---\nname: daily-check\ndescription: Check waiting agents\n---\nRun check.\n"}`, nil, ""); st != http.StatusOK {
		t.Fatalf("PUT skill expected 200, got %d", st)
	}
	if st, _, _ := doRawReq(t, srv.URL+"/v1/assistant/vault/file?path=../bad.md", http.MethodGet, "", nil, ""); st != http.StatusBadRequest {
		t.Fatalf("vault traversal expected 400, got %d", st)
	}
	if st, hdr, _ := doRawReq(t, srv.URL+"/assistant-widget", http.MethodGet, "", nil, ""); st != http.StatusOK || hdr.Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Fatalf("widget frame expected 200 + SAMEORIGIN, got %d %+v", st, hdr)
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

	// Managed device must get 403 on assistant endpoints.
	f.IdentifyOverride = func(string) *registry.Device { return office }
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/assistant/status"},
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
