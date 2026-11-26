package server

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/registry"
	"github.com/bdatdo0601/fleet/internal/slacker"
)

func TestAgentPathsTrackingAPIAndPermissions(t *testing.T) {
	f, srv, tmp := setupTestServer(t)
	_, office := approveTestDevice(t, f, tmp, "office", "mac", "managed")
	_ = f.Slacker.UpsertWorkerRuntime("office", 101, true, "")

	// 1. Heartbeat records cwd & repo.
	if _, err := f.Agents.Heartbeat(agents.HeartbeatParams{
		Device:  "mini",
		Harness: "claude",
		RawID:   "hb1",
		CWD:     "/Users/datmini/workspace/fleet",
		Repo:    "fleet",
		State:   "working",
	}); err != nil {
		t.Fatal(err)
	}

	// 2. Slacker config records workspace_dir and channel-mapped repo paths.
	if err := client.Call(srv.URL, http.MethodPost, "/v1/slacker/config", map[string]any{
		"devices": map[string]any{
			"mini":   map[string]any{"workspace_dir": "~/workspace"},
			"office": map[string]any{"workspace_dir": "/Users/dat/src"},
		},
		"default_devices": []string{"mini", "office"},
		"channels": map[string]any{
			"C1": map[string]any{"channel_id": "C1", "device": "office", "repo_path": "alpha"},
		},
	}, 5*time.Second, nil, nil); err != nil {
		t.Fatalf("POST /v1/slacker/config failed: %v", err)
	}

	// 3. Agent spawn records resolved target path.
	var spawnRes struct {
		Job slacker.Job `json:"job"`
	}
	if err := client.Call(srv.URL, http.MethodPost, "/v1/agents/spawn", map[string]any{
		"harness":   "claude",
		"device":    "office",
		"repo_path": "beta",
	}, 5*time.Second, nil, &spawnRes); err != nil {
		t.Fatalf("POST /v1/agents/spawn failed: %v", err)
	}
	// Cancel spawn job so worker poll claims the Slack-reaction job below.
	if err := client.Call(srv.URL, http.MethodPost, "/v1/slacker/jobs/"+spawnRes.Job.ID+"/cancel", map[string]any{}, 5*time.Second, nil, nil); err != nil {
		t.Fatal(err)
	}

	// 4. Slacker worker poll/receipt records Slack-reaction-originated jobs too.
	_, job, err := f.Slacker.EnqueueJob(slacker.Job{
		EventID: "EvTrack1", ChannelID: "C1", MessageTS: "10.1", Reaction: "claude", UserID: "U1",
		TargetDevice: "office", RepoPath: "/Users/dat/src/gamma", Harness: "claude", SessionName: "slack-c1-10-1", Prompt: "hi",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.IdentifyOverride = func(string) *registry.Device { return office }
	if err := client.Call(srv.URL, http.MethodPost, "/v1/slacker/worker/poll", map[string]any{"timeout": 0.2}, 5*time.Second, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(srv.URL, http.MethodPost, "/v1/slacker/worker/receipt", slacker.Receipt{
		JobID: job.ID, ReceiptID: "rcpt-track", SessionName: job.SessionName, Backend: "tmux", Status: "launched",
	}, 5*time.Second, nil, nil); err != nil {
		t.Fatal(err)
	}

	// Verify GET /v1/agents/paths enriches subpath, tilde_path, repos, and workspaces.
	var cat struct {
		Paths      []agents.TrackedPath `json:"paths"`
		Repos      []string             `json:"repos"`
		Workspaces map[string][]string  `json:"workspaces"`
	}
	if err := client.Call(srv.URL, http.MethodGet, "/v1/agents/paths", nil, 5*time.Second, nil, &cat); err != nil {
		t.Fatalf("GET /v1/agents/paths failed: %v", err)
	}

	byKey := map[string]agents.TrackedPath{}
	for _, p := range cat.Paths {
		byKey[p.Device+"|"+p.Path] = p
	}
	miniFleet, ok := byKey["mini|/Users/datmini/workspace/fleet"]
	if !ok || miniFleet.Subpath != "fleet" || miniFleet.TildePath != "~/workspace/fleet" || miniFleet.Repo != "fleet" {
		t.Fatalf("unexpected mini fleet tracked path: %+v (all=%+v)", miniFleet, cat.Paths)
	}
	officeBeta, ok := byKey["office|/Users/dat/src/beta"]
	if !ok || officeBeta.Subpath != "beta" || officeBeta.Source != "spawn" {
		t.Fatalf("unexpected office beta tracked path: %+v", officeBeta)
	}
	if _, ok := byKey["office|/Users/dat/src/alpha"]; !ok {
		t.Fatalf("expected channel mapping office|/Users/dat/src/alpha in %+v", cat.Paths)
	}
	if _, ok := byKey["office|/Users/dat/src/gamma"]; !ok {
		t.Fatalf("expected slacker job office|/Users/dat/src/gamma in %+v", cat.Paths)
	}
	for _, wantRepo := range []string{"alpha", "beta", "fleet", "gamma"} {
		if !slices.Contains(cat.Repos, wantRepo) {
			t.Fatalf("expected repo %q in %v", wantRepo, cat.Repos)
		}
	}
	if !slices.Contains(cat.Workspaces["mini"], "~/workspace") || !slices.Contains(cat.Workspaces["office"], "/Users/dat/src") {
		t.Fatalf("unexpected workspace suggestions: %+v", cat.Workspaces)
	}

	// Managed device cannot add or delete paths on another device (403), but CAN on itself (200).
	if err := client.Call(srv.URL, http.MethodPost, "/v1/agents/paths", map[string]string{
		"device": "mini", "path": "/tmp/hijack",
	}, 5*time.Second, nil, nil); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("expected 403 for cross-device POST /v1/agents/paths, got %v", err)
	}
	if err := client.Call(srv.URL, http.MethodDelete, "/v1/agents/paths?device=mini&path=/Users/datmini/workspace/fleet", nil, 5*time.Second, nil, nil); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("expected 403 for cross-device DELETE /v1/agents/paths, got %v", err)
	}

	if err := client.Call(srv.URL, http.MethodPost, "/v1/agents/paths", map[string]string{
		"path": "delta",
	}, 5*time.Second, nil, nil); err != nil {
		t.Fatalf("managed self POST /v1/agents/paths failed: %v", err)
	}
	if err := client.Call(srv.URL, http.MethodDelete, "/v1/agents/paths?path=/Users/dat/src/delta", nil, 5*time.Second, nil, nil); err != nil {
		t.Fatalf("managed self DELETE /v1/agents/paths failed: %v", err)
	}

	// Re-opening core.Fleet does NOT resurrect deleted paths once backfilled.
	stateRoot := f.Root
	_ = f.Close()
	f2, err := core.Open(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f2.Close() }()
	// Avoid double close in cleanup by leaving f closed; f.Close is idempotent.
	afterReopen, err := f2.Agents.ListPaths("office", "delta")
	if err != nil || len(afterReopen) != 0 {
		t.Fatalf("expected deleted path to stay deleted on reopen, got %+v err=%v", afterReopen, err)
	}
}
