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

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/memory"
	"github.com/bdatdo0601/fleet/internal/pki"
	"github.com/bdatdo0601/fleet/internal/registry"
)

func TestMemoryHTTPEndpointsAndPermissions(t *testing.T) {
	f, srv, tmp := setupTestServer(t)

	// Approve a managed device so we can verify non-admin vs admin permissions.
	offPubFile := filepath.Join(tmp, "off.pub")
	_ = pki.Keygen(filepath.Join(tmp, "off.key"), offPubFile)
	offPub, _ := os.ReadFile(offPubFile)
	if _, err := f.Approve(core.ApproveOpts{Name: "office", Pubkey: string(offPub), Kind: "mac", Group: "managed", Passphrase: testPass}); err != nil {
		t.Fatal(err)
	}
	office, _ := f.Registry.Get("office")

	// Mock OpenRouter endpoint for curator setup + run.
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": `{"action":"keep","target_key":"","scope":"global","kind":"preference","tier":"always","summary":"","body":"","reason":"ok"}`}}},
		})
	}))
	defer llmSrv.Close()

	// Admin configures curator -> key is masked in response.
	var st memory.CuratorStatus
	if err := client.Call(srv.URL, http.MethodPost, "/v1/memory/curator", map[string]any{
		"enabled":  true,
		"provider": "openrouter",
		"base_url": llmSrv.URL,
		"model":    memory.DefaultOpenRouterModel,
		"api_key":  "sk-or-v1-adminsecret9999",
		"test":     true,
	}, 5*time.Second, nil, &st); err != nil {
		t.Fatalf("admin curator configure failed: %v", err)
	}
	if st.APIKeyHint != "sk-or-v1...9999" || !st.Enabled {
		t.Fatalf("unexpected curator status: %+v", st)
	}

	// Managed device can read/write memories and trigger heartbeat with include_memory.
	f.IdentifyOverride = func(string) *registry.Device { return office }

	var upRes memory.UpsertResult
	if err := client.Call(srv.URL, http.MethodPost, "/v1/memory", map[string]any{
		"scope": "global",
		"key":   "prefer-few-prs",
		"kind":  "preference",
		"body":  "Keep PR count minimal and changes focused.",
	}, 5*time.Second, nil, &upRes); err != nil {
		t.Fatalf("managed memory write failed: %v", err)
	}

	var hbRes agents.HeartbeatResult
	if err := client.Call(srv.URL, http.MethodPost, "/v1/agents/heartbeat", map[string]any{
		"harness":        "codex",
		"raw_id":         "s-mem-1",
		"cwd":            "/tmp/iron-fillet",
		"repo":           "iron-fillet",
		"include_memory": true,
		"memory_repo":    "iron-fillet",
	}, 5*time.Second, nil, &hbRes); err != nil {
		t.Fatalf("heartbeat failed: %v", err)
	}
	if !strings.Contains(hbRes.MemoryContext, "prefer-few-prs") {
		t.Fatalf("expected memory context in heartbeat, got %q", hbRes.MemoryContext)
	}

	// Managed device is forbidden from configuring or triggering curator.
	if err := client.Call(srv.URL, http.MethodPost, "/v1/memory/curator", map[string]any{"disable": true}, 5*time.Second, nil, nil); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("expected 403 for managed POST /v1/memory/curator, got %v", err)
	}
	if err := client.Call(srv.URL, http.MethodPost, "/v1/memory/curator/run", map[string]any{"limit": 5}, 5*time.Second, nil, nil); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("expected 403 for managed POST /v1/memory/curator/run, got %v", err)
	}

	// Managed device POST /v1/memory forces req.Device = caller.Name even if spoofing another device.
	var spoofRes memory.UpsertResult
	if err := client.Call(srv.URL, http.MethodPost, "/v1/memory", map[string]any{
		"scope":  "global",
		"key":    "spoof-check",
		"kind":   "preference",
		"body":   "Check device attribution.",
		"device": "mini",
	}, 5*time.Second, nil, &spoofRes); err != nil {
		t.Fatalf("managed POST /v1/memory failed: %v", err)
	}
	if spoofRes.Item.Device != "office" {
		t.Fatalf("expected Item.Device forced to office, got %q", spoofRes.Item.Device)
	}

	// Managed device is forbidden from resolving or batch-resolving proposals (route(true)).
	prop1, err := f.Memory.CreateProposal(memory.ProposalParams{
		Action:         "promote",
		SourceMemoryID: upRes.Item.ID,
		TargetScope:    "global",
		TargetKey:      "prop-1",
		Kind:           "preference",
		Tier:           "always",
		Summary:        "s1",
		Body:           "b1",
		Reason:         "r1",
		Model:          "m1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Call(srv.URL, http.MethodPost, "/v1/memory/proposals/resolve", map[string]any{
		"id": prop1.ID, "decision": "approve",
	}, 5*time.Second, nil, nil); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("expected 403 for managed POST /v1/memory/proposals/resolve, got %v", err)
	}
	if err := client.Call(srv.URL, http.MethodPost, "/v1/memory/proposals/batch", map[string]any{
		"ids": []int64{prop1.ID}, "decision": "reject",
	}, 5*time.Second, nil, nil); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("expected 403 for managed POST /v1/memory/proposals/batch, got %v", err)
	}

	// Admin CAN resolve proposals.
	f.IdentifyOverride = nil
	var resOut map[string]any
	if err := client.Call(srv.URL, http.MethodPost, "/v1/memory/proposals/resolve", map[string]any{
		"id": prop1.ID, "decision": "reject",
	}, 5*time.Second, nil, &resOut); err != nil {
		t.Fatalf("admin resolve failed: %v", err)
	}
}
