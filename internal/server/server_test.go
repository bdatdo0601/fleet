package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/pki"
	"github.com/bdatdo0601/fleet/internal/registry"
)

const testPass = "correct horse battery staple"

func setupTestServer(t *testing.T) (*core.Fleet, *httptest.Server, string) {
	t.Helper()
	if _, err := exec.LookPath("nebula-cert"); err != nil {
		t.Skip("nebula-cert not installed")
	}
	tmp := t.TempDir()
	pkiDir := filepath.Join(tmp, "etc")
	_ = os.MkdirAll(pkiDir, 0o755)
	t.Setenv("FLEET_NEBULA_ETC", pkiDir)

	pubFile := filepath.Join(tmp, "mini.pub")
	if err := pki.Keygen(filepath.Join(tmp, "mini.key"), pubFile); err != nil {
		t.Fatal(err)
	}
	miniPub, _ := os.ReadFile(pubFile)

	f, err := core.Initialize(core.InitOpts{
		Root:             filepath.Join(tmp, "state"),
		Host:             "home.example.com",
		Port:             443,
		LanIP:            "192.168.1.50",
		Passphrase:       testPass,
		LighthousePubkey: string(miniPub),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })

	srv := httptest.NewServer(NewHandler(f, true))
	t.Cleanup(srv.Close)
	return f, srv, tmp
}

func TestAPIFlowAuthBinaryAndClientVersion(t *testing.T) {
	f, srv, tmp := setupTestServer(t)

	// 1. Sync over loopback records client version and returns config
	var syncRes core.SyncResult
	err := client.Call(srv.URL, http.MethodGet, "/v1/sync?os=darwin&arch=arm64", nil, 5*time.Second, nil, &syncRes)
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if syncRes.Name != "mini" || syncRes.IP != "10.213.0.1" || syncRes.BinarySHA256 != "" {
		t.Fatalf("unexpected initial sync: %+v", syncRes)
	}
	miniDev, _ := f.Registry.Get("mini")
	if miniDev.ClientVersion == nil || !strings.HasPrefix(*miniDev.ClientVersion, "go/") {
		t.Fatalf("expected recorded client_version, got %v", miniDev.ClientVersion)
	}

	// 2. Publish a dummy binary manifest and verify /v1/sync + /v1/binary
	binDir := f.BinDir()
	_ = os.MkdirAll(binDir, 0o755)
	payload := []byte("fake-macho-binary-content")
	sum := sha256.Sum256(payload)
	shaHex := hex.EncodeToString(sum[:])
	_ = os.WriteFile(filepath.Join(binDir, "fleet-darwin-arm64"), payload, 0o755)
	manifestBytes, _ := json.Marshal(core.BinaryManifest{
		Version: "0.1.0",
		Commit:  "1234567890abcdef",
		Binaries: map[string]core.BinaryEntry{
			"darwin/arm64": {File: "fleet-darwin-arm64", SHA256: shaHex, Size: int64(len(payload))},
		},
	})
	_ = os.WriteFile(filepath.Join(binDir, "manifest.json"), manifestBytes, 0o644)

	var syncWithBin core.SyncResult
	if err := client.Call(srv.URL, http.MethodGet, "/v1/sync?os=darwin&arch=arm64", nil, 5*time.Second, nil, &syncWithBin); err != nil {
		t.Fatal(err)
	}
	if syncWithBin.BinarySHA256 != shaHex || syncWithBin.BinaryVersion != "go/1234567" {
		t.Fatalf("expected binary metadata in sync, got %+v", syncWithBin)
	}
	dlPath := filepath.Join(tmp, "downloaded-fleet")
	gotSHA, gotSize, err := client.Download(srv.URL, "/v1/binary?os=darwin&arch=arm64", dlPath, 5*time.Second)
	if err != nil || gotSHA != shaHex || gotSize != int64(len(payload)) {
		t.Fatalf("download failed: sha=%s size=%d err=%v", gotSHA, gotSize, err)
	}

	// 3. Approve, duplicate 409, wrong pass 401, list, revoke, 404
	lapPubFile := filepath.Join(tmp, "lap.pub")
	_ = pki.Keygen(filepath.Join(tmp, "lap.key"), lapPubFile)
	lapPub, _ := os.ReadFile(lapPubFile)

	var appRes core.ApproveResult
	err = client.Call(srv.URL, http.MethodPost, "/v1/approve", map[string]any{
		"name": "laptop", "pubkey": string(lapPub), "group": "admin", "passphrase": testPass,
	}, 5*time.Second, nil, &appRes)
	if err != nil || appRes.IP != "10.213.0.2" {
		t.Fatalf("approve failed: %+v, %v", appRes, err)
	}
	if err := client.Call(srv.URL, http.MethodPost, "/v1/approve", map[string]any{
		"name": "laptop", "pubkey": string(lapPub), "passphrase": testPass,
	}, 5*time.Second, nil, nil); err == nil || !strings.Contains(err.Error(), "HTTP 409") {
		t.Fatalf("expected HTTP 409, got %v", err)
	}
	if err := client.Call(srv.URL, http.MethodPost, "/v1/approve", map[string]any{
		"name": "other", "pubkey": string(lapPub), "passphrase": "nope",
	}, 5*time.Second, nil, nil); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("expected HTTP 401, got %v", err)
	}

	// 4. Managed device allowed on sync/binary/agents, blocked on admin routes
	offPubFile := filepath.Join(tmp, "off.pub")
	_ = pki.Keygen(filepath.Join(tmp, "off.key"), offPubFile)
	offPub, _ := os.ReadFile(offPubFile)
	if _, err := f.Approve(core.ApproveOpts{Name: "office", Pubkey: string(offPub), Kind: "mac", Group: "managed", Passphrase: testPass}); err != nil {
		t.Fatal(err)
	}
	office, _ := f.Registry.Get("office")
	f.IdentifyOverride = func(string) *registry.Device { return office }

	if err := client.Call(srv.URL, http.MethodGet, "/v1/sync?os=darwin&arch=arm64", nil, 5*time.Second, nil, &syncRes); err != nil || syncRes.Name != "office" {
		t.Fatalf("managed sync failed: %+v, %v", syncRes, err)
	}
	if _, _, err := client.Download(srv.URL, "/v1/binary?os=darwin&arch=arm64", dlPath, 5*time.Second); err != nil {
		t.Fatalf("managed binary download failed: %v", err)
	}
	if err := client.Call(srv.URL, http.MethodGet, "/v1/devices", nil, 5*time.Second, nil, nil); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("expected 403 for managed on /v1/devices, got %v", err)
	}

	// 5. Stranger rejected
	f.IdentifyOverride = func(string) *registry.Device { return nil }
	if err := client.Call(srv.URL, http.MethodGet, "/v1/sync", nil, 5*time.Second, nil, nil); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("expected 403 for stranger, got %v", err)
	}
}

func TestAgentsHTTPEndpointsAndWait(t *testing.T) {
	f, srv, _ := setupTestServer(t)

	var hb1 map[string]any
	if err := client.Call(srv.URL, http.MethodPost, "/v1/agents/heartbeat", map[string]any{
		"harness": "claude", "raw_id": "s1", "cwd": "/a", "repo": "app", "state": "working",
	}, 5*time.Second, nil, &hb1); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(srv.URL, http.MethodPost, "/v1/agents/heartbeat", map[string]any{
		"harness": "codex", "raw_id": "s2", "cwd": "/b", "repo": "app", "state": "idle",
	}, 5*time.Second, nil, nil); err != nil {
		t.Fatal(err)
	}

	// Start HTTP /v1/agents/wait in background, then send a message
	var waitOut map[string]any
	var waitErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		waitErr = client.Call(srv.URL, http.MethodPost, "/v1/agents/wait", map[string]any{
			"harness": "codex", "session_id": "mini:codex:s2", "timeout": 3.0,
		}, 5*time.Second, nil, &waitOut)
	})

	for range 40 {
		s, _ := f.Agents.GetSession("mini:codex:s2")
		if s.Listening {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	var sendOut map[string]any
	if err := client.Call(srv.URL, http.MethodPost, "/v1/agents/send", map[string]any{
		"harness": "claude", "session_id": "mini:claude:s1", "to": "mini:codex:s2", "body": "hello via http",
	}, 5*time.Second, nil, &sendOut); err != nil {
		t.Fatal(err)
	}
	if sendOut["delivery"] != "immediate" {
		t.Fatalf("expected immediate delivery, got %+v", sendOut)
	}
	wg.Wait()
	if waitErr != nil {
		t.Fatal(waitErr)
	}
	msgs, ok := waitOut["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("expected 1 message from wait, got %+v", waitOut)
	}

	// Verify device/id and percent-encoded device%2Fid both route cleanly
	for _, path := range []string{"/v1/agents/mini/s1", "/v1/agents/mini%2Fs1"} {
		var detail map[string]any
		if err := client.Call(srv.URL, http.MethodGet, path, nil, 5*time.Second, nil, &detail); err != nil {
			t.Fatalf("GET %s failed: %v", path, err)
		}
	}
}

func TestServeUntilCanceledAbortsLongPollImmediately(t *testing.T) {
	f, _, _ := setupTestServer(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(f, ln.Addr().String(), true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveUntilCanceled(ctx, srv, ln)
	}()

	pollStarted := make(chan struct{})
	pollDone := make(chan error, 1)
	go func() {
		close(pollStarted)
		var out map[string]any
		pollDone <- client.Call("http://"+ln.Addr().String(), http.MethodPost, "/v1/agents/wait-harness", map[string]any{
			"harness": "antigravity",
			"timeout": 30.0,
		}, 35*time.Second, nil, &out)
	}()

	<-pollStarted
	// Give the long-poll request a moment to reach WaitForHarnessMessages.
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serveUntilCanceled returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serveUntilCanceled hung on active long-poll request")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("shutdown took %v; expected immediate cancellation of long-poll", elapsed)
	}
	<-pollDone
}
