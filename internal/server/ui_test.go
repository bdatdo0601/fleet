package server

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/codes"
	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/memory"
	"github.com/bdatdo0601/fleet/internal/pki"
	"github.com/bdatdo0601/fleet/internal/registry"
)

func agentsHeartbeat(device, harness, rawID, state string) agents.HeartbeatParams {
	return agents.HeartbeatParams{
		Device:  device,
		Harness: harness,
		RawID:   rawID,
		CWD:     "/tmp/repo",
		Repo:    "fleet",
		State:   state,
	}
}

func doRawReq(t *testing.T, url, method string, body string, headers map[string]string, customHost string) (int, http.Header, []byte) {
	t.Helper()
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if customHost != "" {
		req.Host = customHost
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, b
}

func approveTestDevice(t *testing.T, f *core.Fleet, tmp, name, kind, group string) (string, *registry.Device) {
	t.Helper()
	pubFile := filepath.Join(tmp, name+".pub")
	if err := pki.Keygen(filepath.Join(tmp, name+".key"), pubFile); err != nil {
		t.Fatal(err)
	}
	pub, err := os.ReadFile(pubFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Approve(core.ApproveOpts{
		Name: name, Pubkey: string(pub), Kind: kind, Group: group, Passphrase: testPass,
	}); err != nil {
		t.Fatal(err)
	}
	dev, err := f.Registry.Get(name)
	if err != nil || dev == nil {
		t.Fatalf("missing approved device %s: %v", name, err)
	}
	return string(pub), dev
}

func TestWebUIAssetsSecurityAndStrangers(t *testing.T) {
	f, srv, _ := setupTestServer(t)

	status, hdr, body := doRawReq(t, srv.URL+"/", http.MethodGet, "", nil, "")
	if status != http.StatusOK {
		t.Fatalf("GET / status=%d body=%s", status, string(body))
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("expected text/html, got %q", ct)
	}
	if csp := hdr.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "style-src 'self'") {
		t.Fatalf("unexpected CSP: %q", csp)
	}
	if hdr.Get("X-Content-Type-Options") != "nosniff" || hdr.Get("X-Frame-Options") != "DENY" || hdr.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("missing security headers: %+v", hdr)
	}
	if !strings.Contains(string(body), `<script type="module" src="/assets/app.js"></script>`) {
		t.Fatalf("index.html missing app.js script tag")
	}

	assetCases := []struct {
		path       string
		wantPrefix string
	}{
		{"/assets/app.js", "text/javascript"},
		{"/assets/app.css", "text/css"},
		{"/assets/favicon.svg", "image/svg+xml"},
		{"/favicon.ico", "image/svg+xml"},
	}
	for _, tc := range assetCases {
		st, h, b := doRawReq(t, srv.URL+tc.path, http.MethodGet, "", nil, "")
		if st != http.StatusOK || len(b) == 0 || !strings.HasPrefix(h.Get("Content-Type"), tc.wantPrefix) {
			t.Fatalf("GET %s: status=%d ct=%q len=%d", tc.path, st, h.Get("Content-Type"), len(b))
		}
	}

	for _, bad := range []string{"/assets/%2e%2e%2findex.html", "/assets/missing.js", "/v1/unknown"} {
		st, _, _ := doRawReq(t, srv.URL+bad, http.MethodGet, "", nil, "")
		if st != http.StatusNotFound {
			t.Fatalf("GET %s: expected 404, got %d", bad, st)
		}
	}

	f.IdentifyOverride = func(string) *registry.Device { return nil }
	for _, p := range []string{"/", "/assets/app.js", "/v1/overview"} {
		st, _, _ := doRawReq(t, srv.URL+p, http.MethodGet, "", nil, "")
		if st != http.StatusForbidden {
			t.Fatalf("stranger GET %s: expected 403, got %d", p, st)
		}
	}
}

func TestBrowserOriginAndDNSRebindingProtection(t *testing.T) {
	_, srv, _ := setupTestServer(t)

	// Cross-origin Origin rejected.
	st, _, _ := doRawReq(t, srv.URL+"/v1/overview", http.MethodGet, "", map[string]string{
		"Origin": "https://evil.example",
	}, "")
	if st != http.StatusForbidden {
		t.Fatalf("cross-origin GET expected 403, got %d", st)
	}

	// Same-origin Origin + Sec-Fetch-Site accepted.
	st, _, _ = doRawReq(t, srv.URL+"/v1/overview", http.MethodGet, "", map[string]string{
		"Origin":         srv.URL,
		"Sec-Fetch-Site": "same-origin",
	}, "")
	if st != http.StatusOK {
		t.Fatalf("same-origin GET expected 200, got %d", st)
	}

	// Cross-site POST rejected even without Origin.
	st, _, _ = doRawReq(t, srv.URL+"/v1/agents/messages", http.MethodPost, `{"to":"all","body":"hi"}`, map[string]string{
		"Sec-Fetch-Site": "cross-site",
	}, "")
	if st != http.StatusForbidden {
		t.Fatalf("cross-site POST expected 403, got %d", st)
	}

	// DNS rebinding Host rejected, while mesh IP and configured lighthouse_host allowed.
	st, _, _ = doRawReq(t, srv.URL+"/v1/overview", http.MethodGet, "", nil, "evil.example.com:7443")
	if st != http.StatusForbidden {
		t.Fatalf("disallowed Host expected 403, got %d", st)
	}
	for _, allowed := range []string{"10.213.0.1:7443", "home.example.com:7443", "mini.local:7443"} {
		st, _, _ = doRawReq(t, srv.URL+"/v1/overview", http.MethodGet, "", nil, allowed)
		if st != http.StatusOK {
			t.Fatalf("allowed Host %q expected 200, got %d", allowed, st)
		}
	}
}

func TestOverviewAdminAndManaged(t *testing.T) {
	f, srv, tmp := setupTestServer(t)
	_, office := approveTestDevice(t, f, tmp, "office", "mac", "managed")
	_, rev := approveTestDevice(t, f, tmp, "oldbox", "mac", "managed")
	if err := f.Revoke(rev.Name); err != nil {
		t.Fatal(err)
	}

	if _, err := f.Agents.Heartbeat(agentsHeartbeat("office", "claude", "s1", "waiting_permission")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Memory.Upsert(memory.UpsertParams{
		Scope: "global", Key: "style", Kind: "preference", Body: "concise", EnqueueCurate: new(false),
	}); err != nil {
		t.Fatal(err)
	}

	var adminOv map[string]any
	if err := client.Call(srv.URL, http.MethodGet, "/v1/overview", nil, 5*time.Second, nil, &adminOv); err != nil {
		t.Fatalf("admin overview failed: %v", err)
	}
	caller := adminOv["caller"].(map[string]any)
	if caller["is_admin"] != true {
		t.Fatalf("expected is_admin=true, got %+v", caller)
	}
	if adminOv["devices"] == nil || adminOv["ca"] == nil || adminOv["settings"] == nil || adminOv["ddns"] == nil {
		t.Fatalf("admin overview missing admin sections: %+v", adminOv)
	}
	agentsMap := adminOv["agents"].(map[string]any)
	if agentsMap["waiting"] != float64(1) {
		t.Fatalf("expected 1 waiting agent, got %+v", agentsMap)
	}

	f.IdentifyOverride = func(string) *registry.Device { return office }
	var managedOv map[string]any
	if err := client.Call(srv.URL, http.MethodGet, "/v1/overview", nil, 5*time.Second, nil, &managedOv); err != nil {
		t.Fatalf("managed overview failed: %v", err)
	}
	mCaller := managedOv["caller"].(map[string]any)
	if mCaller["is_admin"] != false || mCaller["name"] != "office" {
		t.Fatalf("unexpected managed caller: %+v", mCaller)
	}
	for _, hidden := range []string{"devices", "ca", "settings", "ddns", "manifest"} {
		if _, exists := managedOv[hidden]; exists {
			t.Fatalf("managed overview leaked admin field %q", hidden)
		}
	}
	if managedOv["agents"] == nil || managedOv["memory"] == nil || managedOv["curator"] == nil {
		t.Fatalf("managed overview missing non-admin sections: %+v", managedOv)
	}
}

func TestAdminJoinDecodeConfigRevocationsDDNSAndLogs(t *testing.T) {
	f, srv, tmp := setupTestServer(t)
	_, office := approveTestDevice(t, f, tmp, "office", "mac", "managed")

	// Prepare Mac join request block.
	lapPubFile := filepath.Join(tmp, "lap.pub")
	if err := pki.Keygen(filepath.Join(tmp, "lap.key"), lapPubFile); err != nil {
		t.Fatal(err)
	}
	lapPub, _ := os.ReadFile(lapPubFile)
	reqBlock, err := codes.Encode(codes.Request, map[string]any{"v": 1, "name": "laptop", "pubkey": string(lapPub)})
	if err != nil {
		t.Fatal(err)
	}

	var decoded core.DecodedJoinBlock
	if err := client.Call(srv.URL, http.MethodPost, "/v1/join/decode", map[string]string{"block": reqBlock}, 5*time.Second, nil, &decoded); err != nil {
		t.Fatalf("join/decode mac failed: %v", err)
	}
	if decoded.Kind != "mac" || decoded.Name != "laptop" || decoded.RequestID == "" || decoded.Existing {
		t.Fatalf("unexpected decoded join block: %+v", decoded)
	}

	var appRes core.ApproveResult
	if err := client.Call(srv.URL, http.MethodPost, "/v1/approve", map[string]any{
		"name": "laptop", "pubkey": decoded.Pubkey, "kind": "mac", "group": "admin", "passphrase": testPass,
	}, 5*time.Second, nil, &appRes); err != nil {
		t.Fatalf("approve failed: %v", err)
	}
	if appRes.ResponseBlock == "" {
		t.Fatalf("expected response_block in approve result")
	}
	var respDecoded map[string]any
	if err := codes.Decode(codes.Response, appRes.ResponseBlock, &respDecoded); err != nil {
		t.Fatalf("decode response_block failed: %v", err)
	}
	if respDecoded["name"] != "laptop" || respDecoded["ip"] != appRes.IP || respDecoded["ca_id"] != appRes.CAID {
		t.Fatalf("unexpected decoded response_block: %+v", respDecoded)
	}

	// Decoding again detects existing device.
	var decodedAgain core.DecodedJoinBlock
	if err := client.Call(srv.URL, http.MethodPost, "/v1/join/decode", map[string]string{"block": reqBlock}, 5*time.Second, nil, &decodedAgain); err != nil {
		t.Fatal(err)
	}
	if !decodedAgain.Existing || decodedAgain.ExistingIP != appRes.IP || decodedAgain.ExistingGroup != "admin" {
		t.Fatalf("expected existing laptop metadata, got %+v", decodedAgain)
	}

	// Mobile public key decode & config check.
	phonePub, _ := approveTestDevice(t, f, tmp, "phone", "mobile", "admin")
	var mobDec core.DecodedJoinBlock
	if err := client.Call(srv.URL, http.MethodPost, "/v1/join/decode", map[string]string{"block": phonePub}, 5*time.Second, nil, &mobDec); err != nil {
		t.Fatal(err)
	}
	if mobDec.Kind != "mobile" || mobDec.RequestID == "" {
		t.Fatalf("unexpected mobile decode: %+v", mobDec)
	}

	var cfgRes map[string]string
	if err := client.Call(srv.URL, http.MethodGet, "/v1/devices/laptop/config", nil, 5*time.Second, nil, &cfgRes); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfgRes["config"], "10.213.0.1") {
		t.Fatalf("unexpected device config: %+v", cfgRes)
	}
	if err := client.Call(srv.URL, http.MethodGet, "/v1/devices/phone/config", nil, 5*time.Second, nil, nil); err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("expected 400 for mobile device config, got %v", err)
	}

	// Revocation list.
	if err := f.Revoke("laptop"); err != nil {
		t.Fatal(err)
	}
	var revRes struct {
		Revocations []registry.RevokedCert `json:"revocations"`
	}
	if err := client.Call(srv.URL, http.MethodGet, "/v1/revocations", nil, 5*time.Second, nil, &revRes); err != nil {
		t.Fatal(err)
	}
	if len(revRes.Revocations) != 1 || revRes.Revocations[0].Name != "laptop" {
		t.Fatalf("unexpected revocations: %+v", revRes.Revocations)
	}

	// Logs tailing.
	if err := os.WriteFile(filepath.Join(f.Root, "serve.log"), []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var logRes struct {
		Name   string   `json:"name"`
		Exists bool     `json:"exists"`
		Lines  []string `json:"lines"`
	}
	if err := client.Call(srv.URL, http.MethodGet, "/v1/logs?name=serve&lines=2", nil, 5*time.Second, nil, &logRes); err != nil {
		t.Fatal(err)
	}
	if !logRes.Exists || len(logRes.Lines) != 2 || logRes.Lines[0] != "line2" || logRes.Lines[1] != "line3" {
		t.Fatalf("unexpected log tail: %+v", logRes)
	}
	if err := client.Call(srv.URL, http.MethodGet, "/v1/logs?name=bogus", nil, 5*time.Second, nil, nil); err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("expected 400 for bogus log name, got %v", err)
	}

	// DDNS & Policy.
	var ddnsRes map[string]any
	if err := client.Call(srv.URL, http.MethodGet, "/v1/ddns", nil, 5*time.Second, nil, &ddnsRes); err != nil {
		t.Fatal(err)
	}
	if ddnsRes["configured"] != false {
		t.Fatalf("expected configured=false, got %+v", ddnsRes)
	}
	var polRes map[string]any
	if err := client.Call(srv.URL, http.MethodGet, "/v1/ddns/policy?zone_id=Z123ABC&name=home.example.com", nil, 5*time.Second, nil, &polRes); err != nil {
		t.Fatal(err)
	}
	if polRes["Version"] != "2012-10-17" {
		t.Fatalf("unexpected IAM policy: %+v", polRes)
	}

	// Managed device blocked from all admin endpoints.
	f.IdentifyOverride = func(string) *registry.Device { return office }
	for _, ep := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/v1/join/decode", map[string]string{"block": reqBlock}},
		{http.MethodGet, "/v1/devices/laptop/config", nil},
		{http.MethodGet, "/v1/revocations", nil},
		{http.MethodGet, "/v1/ddns", nil},
		{http.MethodGet, "/v1/ddns/policy?zone_id=Z123ABC&name=home.example.com", nil},
		{http.MethodGet, "/v1/logs?name=serve", nil},
	} {
		err := client.Call(srv.URL, ep.method, ep.path, ep.body, 5*time.Second, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
			t.Fatalf("managed %s %s expected 403, got %v", ep.method, ep.path, err)
		}
	}
}

func TestAgentMessagesAndAdminControls(t *testing.T) {
	f, srv, tmp := setupTestServer(t)
	_, office := approveTestDevice(t, f, tmp, "office", "mac", "managed")

	if _, err := f.Agents.Heartbeat(agentsHeartbeat("office", "claude", "s1", "idle")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Agents.Heartbeat(agentsHeartbeat("mini", "codex", "s2", "working")); err != nil {
		t.Fatal(err)
	}

	// Start waiter for office:claude:s1.
	f.IdentifyOverride = func(string) *registry.Device { return office }
	var waitOut map[string]any
	var waitErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		waitErr = client.Call(srv.URL, http.MethodPost, "/v1/agents/wait", map[string]any{
			"harness": "claude", "session_id": "office:claude:s1", "timeout": 3.0,
		}, 5*time.Second, nil, &waitOut)
	})
	for range 40 {
		s, _ := f.Agents.GetSession("office:claude:s1")
		if s.Listening {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	// Switch to admin mini and send via web operator endpoint.
	f.IdentifyOverride = nil
	var sendRes map[string]any
	if err := client.Call(srv.URL, http.MethodPost, "/v1/agents/messages", map[string]any{
		"to":   "office:claude:s1",
		"body": "hello from web",
	}, 5*time.Second, nil, &sendRes); err != nil {
		t.Fatalf("POST /v1/agents/messages failed: %v", err)
	}
	if sendRes["delivery"] != "immediate" {
		t.Fatalf("expected immediate delivery, got %+v", sendRes)
	}

	wg.Wait()
	if waitErr != nil {
		t.Fatal(waitErr)
	}
	msgs, ok := waitOut["messages"].([]any)
	if !ok || len(msgs) != 1 || msgs[0].(map[string]any)["from_session"] != "mini:web:ui" {
		t.Fatalf("unexpected wait messages: %+v", waitOut)
	}

	// Verify no phantom session was created for mini:web:ui.
	var listRes struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := client.Call(srv.URL, http.MethodGet, "/v1/agents?include_stale=true", nil, 5*time.Second, nil, &listRes); err != nil {
		t.Fatal(err)
	}
	for _, a := range listRes.Agents {
		if a["session_id"] == "mini:web:ui" {
			t.Fatalf("operator message created phantom session: %+v", a)
		}
	}

	// Message feed contains the sent message.
	var feedRes struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := client.Call(srv.URL, http.MethodGet, "/v1/agents/messages?session=office:claude:s1", nil, 5*time.Second, nil, &feedRes); err != nil {
		t.Fatal(err)
	}
	if len(feedRes.Messages) != 1 || feedRes.Messages[0]["body"] != "hello from web" {
		t.Fatalf("unexpected messages feed: %+v", feedRes.Messages)
	}

	// Admin can rename and deregister another device's session.
	var renameRes map[string]string
	if err := client.Call(srv.URL, http.MethodPost, "/v1/agents/rename", map[string]string{
		"session_id": "office:claude:s1",
		"name":       "office-renamed",
	}, 5*time.Second, nil, &renameRes); err != nil {
		t.Fatalf("admin cross-device rename failed: %v", err)
	}
	if renameRes["name"] != "office-renamed" {
		t.Fatalf("unexpected rename response: %+v", renameRes)
	}
	if err := client.Call(srv.URL, http.MethodPost, "/v1/agents/deregister", map[string]string{
		"session_id": "office:claude:s1",
	}, 5*time.Second, nil, nil); err != nil {
		t.Fatalf("admin cross-device deregister failed: %v", err)
	}
	detail, err := f.Agents.GetSession("office:claude:s1")
	if err != nil || detail.State != "exited" || detail.Name != "office-renamed" {
		t.Fatalf("expected exited + renamed session, got %+v, err=%v", detail, err)
	}

	// Managed device cannot rename or deregister mini's session.
	f.IdentifyOverride = func(string) *registry.Device { return office }
	for _, ep := range []struct {
		path string
		body map[string]string
	}{
		{"/v1/agents/rename", map[string]string{"session_id": "mini:codex:s2", "name": "hijack"}},
		{"/v1/agents/deregister", map[string]string{"session_id": "mini:codex:s2"}},
	} {
		err := client.Call(srv.URL, http.MethodPost, ep.path, ep.body, 5*time.Second, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
			t.Fatalf("managed %s on other device expected 403, got %v", ep.path, err)
		}
	}
}

func TestMemoryScopesAndProposalBatch(t *testing.T) {
	f, srv, _ := setupTestServer(t)

	for _, up := range []memory.UpsertParams{
		{Scope: "repo:fleet", Key: "k1", Kind: "project", Body: "body 1", EnqueueCurate: new(false)},
		{Scope: "global", Key: "g1", Kind: "preference", Body: "body 2", EnqueueCurate: new(false)},
	} {
		if _, err := f.Memory.Upsert(up); err != nil {
			t.Fatal(err)
		}
	}

	var scopesRes struct {
		Scopes []string `json:"scopes"`
	}
	if err := client.Call(srv.URL, http.MethodGet, "/v1/memory/scopes", nil, 5*time.Second, nil, &scopesRes); err != nil {
		t.Fatal(err)
	}
	if len(scopesRes.Scopes) != 2 || scopesRes.Scopes[0] != "global" || scopesRes.Scopes[1] != "repo:fleet" {
		t.Fatalf("unexpected scopes: %+v", scopesRes.Scopes)
	}

	p1, err := f.Memory.CreateProposal(memory.ProposalParams{
		Action: "promote_global", TargetScope: "global", TargetKey: "prop-one",
		Kind: "preference", Tier: "always", Summary: "one", Body: "rule one", Reason: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := f.Memory.CreateProposal(memory.ProposalParams{
		Action: "promote_global", TargetScope: "global", TargetKey: "prop-two",
		Kind: "preference", Tier: "always", Summary: "two", Body: "rule two", Reason: "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	var rejRes map[string]any
	if err := client.Call(srv.URL, http.MethodPost, "/v1/memory/proposals/batch", map[string]any{
		"ids": []int64{p1.ID}, "decision": "reject",
	}, 5*time.Second, nil, &rejRes); err != nil {
		t.Fatal(err)
	}
	if rejRes["count"] != float64(1) {
		t.Fatalf("expected count=1, got %+v", rejRes)
	}

	var appRes map[string]any
	if err := client.Call(srv.URL, http.MethodPost, "/v1/memory/proposals/batch", map[string]any{
		"all_pending": true, "decision": "approve",
	}, 5*time.Second, nil, &appRes); err != nil {
		t.Fatal(err)
	}
	if appRes["count"] != float64(1) {
		t.Fatalf("expected 1 approved pending proposal, got %+v", appRes)
	}
	if gotP2, err := f.Memory.GetProposal(p2.ID); err != nil || gotP2.Status != "approved" {
		t.Fatalf("expected p2 approved, got %+v, err=%v", gotP2, err)
	}
	if _, err := f.Memory.Get("global", "prop-two", false); err != nil {
		t.Fatalf("expected prop-two memory to be created: %v", err)
	}
}
