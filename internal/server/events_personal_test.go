package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/assistant"
	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/registry"
)

func TestEventsAndAgentWatchRoutes(t *testing.T) {
	f, srv, tmp := setupTestServer(t)
	_, laptop := approveTestDevice(t, f, tmp, "laptop", "mac", "managed")

	// Emit external event as managed device -> stored in EventBus and queryable via GET /v1/events.
	f.IdentifyOverride = func(string) *registry.Device { return laptop }
	var emitRes struct {
		OK    bool `json:"ok"`
		Event struct {
			ID     int64  `json:"id"`
			Type   string `json:"type"`
			Source string `json:"source"`
			Actor  string `json:"actor"`
		} `json:"event"`
	}
	if err := client.Call(srv.URL, http.MethodPost, "/v1/events", map[string]any{
		"type":     "external.event",
		"severity": "warn",
		"title":    "CI build slow",
		"summary":  "Took 12m",
	}, 3*time.Second, nil, &emitRes); err != nil || !emitRes.OK || emitRes.Event.Actor != "laptop" {
		t.Fatalf("POST /v1/events failed: err=%v res=%+v", err, emitRes)
	}

	// Register a pending watch, list it, and delete it.
	if err := client.Call(srv.URL, http.MethodPost, "/v1/agents/heartbeat", map[string]any{
		"harness": "claude",
		"raw_id":  "s-watcher",
		"state":   "working",
		"cwd":     "/tmp/w",
	}, 3*time.Second, nil, nil); err != nil {
		t.Fatal(err)
	}
	var createRes struct {
		Watch agents.Watch `json:"watch"`
	}
	if err := client.Call(srv.URL, http.MethodPost, "/v1/agents/watches", map[string]any{
		"session_id":    "laptop:claude:s-watcher",
		"target":        "laptop/worker-2",
		"allow_pending": true,
	}, 3*time.Second, nil, &createRes); err != nil || createRes.Watch.ID == "" {
		t.Fatalf("POST /v1/agents/watches failed: err=%v res=%+v", err, createRes)
	}
	if st, _, b := doRawReq(t, srv.URL+"/v1/agents/watches", http.MethodGet, "", nil, ""); st != http.StatusOK || !strings.Contains(string(b), createRes.Watch.ID) {
		t.Fatalf("GET /v1/agents/watches expected watch %s, got %d (%s)", createRes.Watch.ID, st, string(b))
	}
	if st, _, _ := doRawReq(t, srv.URL+"/v1/agents/watches/"+createRes.Watch.ID, http.MethodDelete, "", nil, ""); st != http.StatusOK {
		t.Fatalf("DELETE /v1/agents/watches/{id} expected 200, got %d", st)
	}
	f.IdentifyOverride = nil

	if st, _, b := doRawReq(t, srv.URL+"/v1/events?type=external", http.MethodGet, "", nil, ""); st != http.StatusOK || !strings.Contains(string(b), "CI build slow") {
		t.Fatalf("GET /v1/events expected event, got %d (%s)", st, string(b))
	}
	if st, _, b := doRawReq(t, srv.URL+"/v1/assistant/inbox", http.MethodGet, "", nil, ""); st != http.StatusOK || !strings.Contains(string(b), `"items"`) {
		t.Fatalf("GET /v1/assistant/inbox expected 200, got %d (%s)", st, string(b))
	}
}

func TestPersonalProfileGatesAndEndpoints(t *testing.T) {
	f, srv, tmp := setupTestServer(t)
	_, workDev := approveTestDevice(t, f, tmp, "workbox", "mac", "managed")
	_, personalDev := approveTestDevice(t, f, tmp, "homebox", "mac", "managed")
	_ = workDev

	// Set workbox profile = work, homebox profile = personal via POST /v1/set-profile.
	if st, _, b := doRawReq(t, srv.URL+"/v1/set-profile", http.MethodPost, `{"name":"workbox","profile":"work"}`, nil, ""); st != http.StatusOK || !strings.Contains(string(b), `"work"`) {
		t.Fatalf("POST /v1/set-profile work failed: %d (%s)", st, string(b))
	}
	workDev, _ = f.Registry.Get("workbox")
	if workDev.Profile != "work" {
		t.Fatalf("expected workbox profile=work, got %q", workDev.Profile)
	}

	cfg := assistant.DefaultConfig()
	cfg.Enabled = true
	cfg.VaultExposure = assistant.ExposurePersonalDevices
	if _, err := f.Assistant.UpdateConfig(cfg, false); err != nil {
		t.Fatal(err)
	}
	v, err := f.Assistant.Vault()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v.Root, "Inbox", "todo.md"), []byte("# Home Todo\nBuy coffee"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Non-admin work device MUST receive 403 on all /v1/personal/* routes.
	f.IdentifyOverride = func(string) *registry.Device { return workDev }
	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/v1/personal/mcp/tools", ""},
		{http.MethodPost, "/v1/personal/mcp/call", `{"tool":"mcp__x__y"}`},
		{http.MethodGet, "/v1/personal/vault/search?q=coffee", ""},
		{http.MethodGet, "/v1/personal/vault/read?path=Inbox/todo.md", ""},
		{http.MethodPost, "/v1/personal/vault/capture", `{"path":"Inbox/new.md","content":"hi"}`},
	} {
		st, _, b := doRawReq(t, srv.URL+tc.path, tc.method, tc.body, nil, "")
		if st != http.StatusForbidden {
			t.Fatalf("%s %s as work device expected 403, got %d (%s)", tc.method, tc.path, st, string(b))
		}
	}

	// Personal device succeeds for personal-exposed vault search/read/capture and rejects writes outside auto_write_dirs.
	f.IdentifyOverride = func(string) *registry.Device { return personalDev }
	if st, _, b := doRawReq(t, srv.URL+"/v1/personal/mcp/tools", http.MethodGet, "", nil, ""); st != http.StatusOK || !strings.Contains(string(b), `"tools"`) {
		t.Fatalf("GET /v1/personal/mcp/tools as personal failed: %d (%s)", st, string(b))
	}
	if st, _, b := doRawReq(t, srv.URL+"/v1/personal/vault/search?q=coffee", http.MethodGet, "", nil, ""); st != http.StatusOK || !strings.Contains(string(b), "Inbox/todo.md") {
		t.Fatalf("GET /v1/personal/vault/search as personal failed: %d (%s)", st, string(b))
	}
	if st, _, b := doRawReq(t, srv.URL+"/v1/personal/vault/read?path=Inbox/todo.md", http.MethodGet, "", nil, ""); st != http.StatusOK || !strings.Contains(string(b), "Buy coffee") {
		t.Fatalf("GET /v1/personal/vault/read as personal failed: %d (%s)", st, string(b))
	}
	if st, _, b := doRawReq(t, srv.URL+"/v1/personal/vault/capture", http.MethodPost, `{"path":"Inbox/cap.md","content":"captured item"}`, nil, ""); st != http.StatusOK || !strings.Contains(string(b), `"saved"`) {
		t.Fatalf("POST /v1/personal/vault/capture failed: %d (%s)", st, string(b))
	}
	if st, _, _ := doRawReq(t, srv.URL+"/v1/personal/vault/capture", http.MethodPost, `{"path":"Outside/bad.md","content":"no"}`, nil, ""); st != http.StatusForbidden {
		t.Fatalf("POST /v1/personal/vault/capture outside auto_write_dirs expected 403, got %d", st)
	}
	f.IdentifyOverride = nil
}
