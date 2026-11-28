package assistant

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMCPOAuthConfigNormalizationValidationAndMasking(t *testing.T) {
	srv := NormalizeMCPServerConfig(MCPServerConfig{
		Name: "gworkspace",
		URL:  "https://workspacemcp.googleapis.com/mcp/v1self",
		OAuth: &MCPOAuthConfig{
			Enabled:           true,
			ClientID:          "agent-client-123.apps.googleusercontent.com",
			ClientSecretOPRef: "op://Fleet-Agent/Google-MCP/client_secret",
			Scopes:            []string{"https://www.googleapis.com/auth/gmail.modify https://www.googleapis.com/auth/calendar", "https://www.googleapis.com/auth/calendar"},
		},
		Enabled: true,
	})
	if srv.Transport != "http" {
		t.Fatalf("expected http transport, got %q", srv.Transport)
	}
	if srv.OAuth == nil || srv.OAuth.Provider != "google" {
		t.Fatalf("expected auto-detected google provider, got %+v", srv.OAuth)
	}
	if len(srv.OAuth.Scopes) != 2 {
		t.Fatalf("expected 2 deduplicated scopes, got %+v", srv.OAuth.Scopes)
	}
	if err := ValidateMCPServerConfig(srv, true); err != nil {
		t.Fatalf("expected valid Google OAuth config, got %v", err)
	}

	// Self-setup rejects plaintext client_secret.
	badSelf := srv
	badSelf.OAuth = copyMCPOAuthConfig(srv.OAuth)
	badSelf.OAuth.ClientSecret = "GOCSPX-plaintext-secret"
	if err := ValidateMCPServerConfig(badSelf, true); err == nil {
		t.Fatalf("expected plaintext client_secret rejected in self-setup")
	}
	// Non-self-setup allows plaintext client_secret and masks it in ConfigView + preserves on UpdateConfig.
	if err := ValidateMCPServerConfig(badSelf, false); err != nil {
		t.Fatalf("expected plaintext client_secret allowed in manual config, got %v", err)
	}

	tmp := t.TempDir()
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	cfg := DefaultConfig()
	cfg.MCPServers = []MCPServerConfig{badSelf}
	view, err := eng.UpdateConfig(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if view.MCPServers[0].OAuth.ClientSecret == "GOCSPX-plaintext-secret" || !strings.Contains(view.MCPServers[0].OAuth.ClientSecret, "...") {
		t.Fatalf("expected masked client_secret in view, got %q", view.MCPServers[0].OAuth.ClientSecret)
	}
	// Saving ConfigView back preserves the real client_secret.
	cfg2 := DefaultConfig()
	cfg2.MCPServers = view.MCPServers
	if _, err := eng.UpdateConfig(cfg2, false); err != nil {
		t.Fatal(err)
	}
	if got := eng.Config().MCPServers[0].OAuth.ClientSecret; got != "GOCSPX-plaintext-secret" {
		t.Fatalf("expected masked client_secret preserved, got %q", got)
	}
}

func TestMCPOAuthFullPKCEFlowRefreshPaginationAnd401Retry(t *testing.T) {
	const (
		clientID     = "agent-client.apps.googleusercontent.com"
		clientSecret = "super-secret-client-007"
		authCode     = "auth-code-xyz-123"
		refreshTok   = "refresh-token-initial-999"
		accessTok1   = "access-token-one-111"
		accessTok2   = "access-token-two-222"
	)

	var tokenCalls atomic.Int32
	var forceExpiredOnNextMCP atomic.Bool
	var lastChallenge string

	oauthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
			if r.Form.Get("client_id") != clientID || r.Form.Get("client_secret") != clientSecret {
				http.Error(w, "invalid client credentials", http.StatusUnauthorized)
				return
			}
			call := tokenCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			switch r.Form.Get("grant_type") {
			case "authorization_code":
				if r.Form.Get("code") != authCode {
					http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
					return
				}
				sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
				gotChallenge := base64.RawURLEncoding.EncodeToString(sum[:])
				if gotChallenge != lastChallenge {
					http.Error(w, `{"error":"invalid_grant","error_description":"pkce mismatch"}`, http.StatusBadRequest)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token":  accessTok1,
					"refresh_token": refreshTok,
					"token_type":    "Bearer",
					"expires_in":    3600,
					"scope":         "https://www.googleapis.com/auth/gmail.modify",
				})
			case "refresh_token":
				if r.Form.Get("refresh_token") != refreshTok {
					http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
					return
				}
				// Omit refresh_token on refresh (just like Google) to verify preservation.
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token": accessTok2,
					"token_type":   "Bearer",
					"expires_in":   3600,
				})
			default:
				http.Error(w, fmt.Sprintf(`{"error":"unsupported_grant_%d"}`, call), http.StatusBadRequest)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer oauthSrv.Close()

	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if forceExpiredOnNextMCP.Load() && auth == "Bearer "+accessTok1 {
			http.Error(w, "expired token", http.StatusUnauthorized)
			return
		}
		if auth != "Bearer "+accessTok1 && auth != "Bearer "+accessTok2 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req struct {
			ID     int64          `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			if pv, _ := req.Params["protocolVersion"].(string); pv != "2026-07-28" {
				http.Error(w, "unexpected protocolVersion "+pv, http.StatusBadRequest)
				return
			}
			w.Header().Set("Mcp-Session-Id", "sess-oauth-1")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2026-07-28"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			cursor, _ := req.Params["cursor"].(string)
			if cursor == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]any{
						"tools":      []map[string]any{{"name": "list_messages", "description": "List Gmail messages"}},
						"nextCursor": "page2",
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"tools": []map[string]any{{"name": "get_message", "description": "Get Gmail message"}},
				},
			})
		case "tools/call":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]any{"ok": true, "auth_used": auth},
			})
		}
	}))
	defer mcpSrv.Close()

	tmp := t.TempDir()
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	eng.SetOPReader(func(_ context.Context, ref string) (string, error) {
		switch ref {
		case "op://Fleet-Agent/Google-MCP/client_secret":
			return clientSecret, nil
		default:
			return "", fmt.Errorf("unexpected ref %q", ref)
		}
	})

	// Configure via mcp_configure self-setup and verify oauth_required + mcp_oauth widget attachment.
	cfgArgs := fmt.Sprintf(`{
		"name": "gworkspace",
		"transport": "http",
		"url": %q,
		"oauth": {
			"enabled": true,
			"provider": "google",
			"auth_url": %q,
			"token_url": %q,
			"client_id": %q,
			"client_secret_op_ref": "op://Fleet-Agent/Google-MCP/client_secret",
			"scopes": ["https://www.googleapis.com/auth/gmail.modify"]
		}
	}`, mcpSrv.URL, oauthSrv.URL+"/auth", oauthSrv.URL+"/token", clientID)

	tctxChat := &toolExecContext{ChannelID: "general", Source: "chat", Config: eng.Config()}
	_, err = eng.executeTool(t.Context(), ToolCall{ID: "c1", Type: "function", Function: ToolCallFunction{Name: "mcp_configure", Arguments: cfgArgs}}, tctxChat)
	aprErr, ok := errors.AsType[*approvalRequiredError](err)
	if !ok {
		t.Fatalf("expected approvalRequiredError, got %v", err)
	}

	var widgets []WidgetPayload
	tctxApproved := &toolExecContext{ChannelID: "general", Source: "approval", Config: eng.Config(), BypassHITL: true, Widgets: &widgets}
	outStr, err := eng.executeTool(t.Context(), ToolCall{ID: "c2", Type: "function", Function: ToolCallFunction{Name: "mcp_configure", Arguments: aprErr.ArgsJSON}}, tctxApproved)
	if err != nil {
		t.Fatalf("approved mcp_configure failed: %v", err)
	}
	if !strings.Contains(outStr, `"oauth_required":true`) {
		t.Fatalf("expected oauth_required=true, got %s", outStr)
	}
	if len(widgets) != 1 || widgets[0].Kind != "mcp_oauth" {
		t.Fatalf("expected attached mcp_oauth widget, got %+v", widgets)
	}

	// Doctor warns with actionable mcp-auth hint while unauthorized.
	doc := eng.Doctor(t.Context())
	var mcpCheck DoctorCheck
	for _, c := range doc.Checks {
		if c.Name == "mcp_servers" {
			mcpCheck = c
			break
		}
	}
	if mcpCheck.Status != "warn" || !strings.Contains(mcpCheck.Hint, "fleet assistant mcp-auth gworkspace") {
		t.Fatalf("expected doctor warn with mcp-auth hint, got %+v", mcpCheck)
	}

	// Start OAuth and verify PKCE + Google offline params.
	startRes, err := eng.StartMCPOAuth(t.Context(), "gworkspace", "http://127.0.0.1:9999/oauth2callback")
	if err != nil {
		t.Fatalf("StartMCPOAuth failed: %v", err)
	}
	authURL, err := url.Parse(startRes.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	q := authURL.Query()
	lastChallenge = q.Get("code_challenge")
	if q.Get("code_challenge_method") != "S256" || lastChallenge == "" || q.Get("access_type") != "offline" || q.Get("prompt") != "consent" {
		t.Fatalf("unexpected Google PKCE authorize query: %v", q)
	}

	// Complete OAuth and verify cursor-paginated tools discovery (2 tools across 2 pages).
	compRes, err := eng.CompleteMCPOAuth(t.Context(), "gworkspace", startRes.State, authCode)
	if err != nil {
		t.Fatalf("CompleteMCPOAuth failed: %v", err)
	}
	if !compRes.Authorized || compRes.ToolCount != 2 || compRes.TestError != "" {
		t.Fatalf("unexpected CompleteMCPOAuth result: %+v", compRes)
	}

	// Call discovered tool with accessTok1.
	res1, err := eng.mcp.CallTool(t.Context(), "mcp__gworkspace__list_messages", map[string]any{})
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if m, _ := res1.(map[string]any); m["auth_used"] != "Bearer "+accessTok1 {
		t.Fatalf("expected accessTok1, got %+v", res1)
	}

	// Simulate server 401 -> automatic refresh + retry once -> succeeds with accessTok2.
	forceExpiredOnNextMCP.Store(true)
	res2, err := eng.mcp.CallTool(t.Context(), "mcp__gworkspace__get_message", map[string]any{})
	if err != nil {
		t.Fatalf("CallTool 401 retry failed: %v", err)
	}
	if m, _ := res2.(map[string]any); m["auth_used"] != "Bearer "+accessTok2 {
		t.Fatalf("expected accessTok2 after 401 refresh, got %+v", res2)
	}

	// Stored refresh_token must still be preserved even though refresh response omitted it.
	stored, ok, err := eng.store.GetMCPOAuthToken("gworkspace")
	if err != nil || !ok || stored.RefreshToken != refreshTok || stored.AccessToken != accessTok2 {
		t.Fatalf("expected preserved refresh_token and updated access_token, got %+v (ok=%v err=%v)", stored, ok, err)
	}

	// Test near-expiry automatic refresh using mocked clock.
	if err := eng.store.UpsertMCPOAuthToken(MCPOAuthToken{
		ServerName:   "gworkspace",
		AccessToken:  accessTok1,
		RefreshToken: refreshTok,
		ExpiresAt:    time.Now().UTC().Add(20 * time.Second).Format(time.RFC3339),
		TokenURL:     oauthSrv.URL + "/token",
	}); err != nil {
		t.Fatal(err)
	}
	tok, _, err := eng.mcp.OAuth().GetAccessToken(t.Context(), eng.Config().MCPServers[0], eng.opReader, false)
	if err != nil || tok != accessTok2 {
		t.Fatalf("expected near-expiry refresh to yield accessTok2, got %q (err=%v)", tok, err)
	}

	// Test mcp_oauth_connect + render_card + Discord embed formatting.
	var connectWidgets []WidgetPayload
	tctxConnect := &toolExecContext{ChannelID: "general", Source: "chat", Config: eng.Config(), Widgets: &connectWidgets}
	connOut, err := eng.executeTool(t.Context(), ToolCall{ID: "mc", Type: "function", Function: ToolCallFunction{Name: "mcp_oauth_connect", Arguments: `{"name":"gworkspace"}`}}, tctxConnect)
	if err != nil || !strings.Contains(connOut, `"attached":true`) || len(connectWidgets) != 1 || connectWidgets[0].Kind != "mcp_oauth" {
		t.Fatalf("mcp_oauth_connect failed: out=%s widgets=%+v err=%v", connOut, connectWidgets, err)
	}
	emb, needLink := widgetToDiscordEmbed(connectWidgets[0])
	if !needLink || emb == nil || !strings.Contains(fmt.Sprint(emb["description"]), "fleet assistant mcp-auth gworkspace") {
		t.Fatalf("unexpected Discord embed for mcp_oauth: %+v (needLink=%v)", emb, needLink)
	}
	if _, err := eng.executeTool(t.Context(), ToolCall{ID: "rc", Type: "function", Function: ToolCallFunction{Name: "render_card", Arguments: `{"kind":"mcp_oauth","data":{"name":"gworkspace"}}`}}, tctxConnect); err != nil {
		t.Fatalf("render_card mcp_oauth failed: %v", err)
	}
	if len(connectWidgets) != 1 {
		t.Fatalf("expected deduplicated mcp_oauth widget slice len 1, got %d (%+v)", len(connectWidgets), connectWidgets)
	}
	rcData, _ := connectWidgets[0].Data.(map[string]any)
	if rcData["server"] != "gworkspace" || rcData["provider"] != "google" || rcData["url"] != mcpSrv.URL || rcData["cli_command"] != "fleet assistant mcp-auth gworkspace" {
		t.Fatalf("expected render_card mcp_oauth enriched from config, got %+v", rcData)
	}
	if _, err := eng.executeTool(t.Context(), ToolCall{ID: "rc_bad", Type: "function", Function: ToolCallFunction{Name: "render_card", Arguments: `{"kind":"mcp_oauth","data":{"server":"nonexistent"}}`}}, tctxConnect); err == nil {
		t.Fatalf("expected render_card mcp_oauth for nonexistent server to fail")
	}

	// mcp_remove cleans up stored OAuth tokens.
	if _, err := eng.executeTool(t.Context(), ToolCall{ID: "mr", Type: "function", Function: ToolCallFunction{Name: "mcp_remove", Arguments: `{"name":"gworkspace"}`}}, tctxApproved); err != nil {
		t.Fatalf("mcp_remove failed: %v", err)
	}
	if _, hasTok, _ := eng.store.GetMCPOAuthToken("gworkspace"); hasTok {
		t.Fatalf("expected stored OAuth token deleted after mcp_remove")
	}
}

func TestMCPOAuthDiscoveryRefreshRefAndSecretScrubbing(t *testing.T) {
	const (
		clientID     = "generic-client"
		clientSecret = "generic-secret-xyz"
		preRefresh   = "op-seeded-refresh-token-abc"
		issuedAccess = "discovered-access-token-777"
	)

	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource":
			_ = json.NewEncoder(w).Encode(map[string]any{"authorization_servers": []string{srvURL}})
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"authorization_endpoint": srvURL + "/authorize",
				"token_endpoint":         srvURL + "/token",
			})
		case "/token":
			_ = r.ParseForm()
			if r.Form.Get("refresh_token") == "bad-refresh" {
				http.Error(w, fmt.Sprintf("rejected secret=%s refresh=%s", clientSecret, "bad-refresh"), http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": issuedAccess,
				"expires_in":   1800,
			})
		case "/mcp":
			if r.Header.Get("Authorization") != "Bearer "+issuedAccess {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			var req struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			switch req.Method {
			case "initialize":
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-03-26"}})
			case "notifications/initialized":
				w.WriteHeader(http.StatusAccepted)
			case "tools/list":
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": []map[string]any{{"name": "ping"}}}})
			}
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	mgr := NewMCPManager([]MCPServerConfig{{
		Name:      "generic",
		Transport: "http",
		URL:       srv.URL + "/mcp",
		Enabled:   true,
		OAuth: &MCPOAuthConfig{
			Enabled:           true,
			ClientID:          clientID,
			ClientSecretOPRef: "op://Vault/Item/secret",
			RefreshTokenOPRef: "op://Vault/Item/refresh",
		},
	}}, func(_ context.Context, ref string) (string, error) {
		switch ref {
		case "op://Vault/Item/secret":
			return clientSecret, nil
		case "op://Vault/Item/refresh":
			return preRefresh, nil
		default:
			return "", errors.New("unknown ref")
		}
	})
	defer mgr.Close()

	tools := mgr.ListTools(t.Context())
	if len(tools) != 1 || tools[0].RemoteName != "ping" {
		t.Fatalf("expected RFC 9728/8414 discovery + refresh_token_op_ref to discover ping tool, got %+v", tools)
	}

	// Verify OAuth errors scrub client_secret and refresh_token.
	mgr.reader = func(_ context.Context, ref string) (string, error) {
		if ref == "op://Vault/Item/secret" {
			return clientSecret, nil
		}
		return "bad-refresh", nil
	}
	mgr.ResetServer("generic")
	_, err := mgr.TestServer(t.Context(), MCPServerConfig{
		Name:      "generic",
		Transport: "http",
		URL:       srv.URL + "/mcp",
		Enabled:   true,
		OAuth: &MCPOAuthConfig{
			Enabled:           true,
			ClientID:          clientID,
			ClientSecretOPRef: "op://Vault/Item/secret",
			RefreshTokenOPRef: "op://Vault/Item/refresh",
		},
	})
	if err == nil || strings.Contains(err.Error(), clientSecret) || strings.Contains(err.Error(), "bad-refresh") {
		t.Fatalf("expected secrets scrubbed from OAuth error, got %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMCPOAuthDiscoverySSRFAndPendingCap(t *testing.T) {
	makeMgr := func(handler func(*http.Request) (*http.Response, error)) *MCPOAuthManager {
		cli := &http.Client{Transport: roundTripFunc(handler)}
		return NewMCPOAuthManager(nil, cli)
	}

	jsonResp := func(status int, body string, hdrs map[string]string) *http.Response {
		h := http.Header{"Content-Type": []string{"application/json"}}
		for k, v := range hdrs {
			h.Set(k, v)
		}
		return &http.Response{
			StatusCode: status,
			Header:     h,
			Body:       io.NopCloser(strings.NewReader(body)),
		}
	}

	for _, tc := range []struct {
		name    string
		handler func(*http.Request) (*http.Response, error)
	}{
		{
			name: "authorization_servers_metadata_ip",
			handler: func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/.well-known/oauth-protected-resource" {
					return jsonResp(200, `{"authorization_servers":["http://169.254.169.254"]}`, nil), nil
				}
				return jsonResp(200, `{"authorization_endpoint":"https://auth.example.com/a","token_endpoint":"https://auth.example.com/t"}`, nil), nil
			},
		},
		{
			name: "authorization_servers_non_https",
			handler: func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/.well-known/oauth-protected-resource" {
					return jsonResp(200, `{"authorization_servers":["http://auth.example.com"]}`, nil), nil
				}
				return jsonResp(404, `{}`, nil), nil
			},
		},
		{
			name: "authorization_endpoint_loopback",
			handler: func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/.well-known/oauth-protected-resource" {
					return jsonResp(404, `{}`, nil), nil
				}
				return jsonResp(200, `{"authorization_endpoint":"https://127.0.0.1/auth","token_endpoint":"https://auth.example.com/token"}`, nil), nil
			},
		},
		{
			name: "token_endpoint_mesh_ip",
			handler: func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/.well-known/oauth-protected-resource" {
					return jsonResp(404, `{}`, nil), nil
				}
				return jsonResp(200, `{"authorization_endpoint":"https://auth.example.com/auth","token_endpoint":"https://10.213.0.1/token"}`, nil), nil
			},
		},
		{
			name: "discovery_redirect_to_private_ip",
			handler: func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/.well-known/oauth-protected-resource" {
					return jsonResp(404, `{}`, nil), nil
				}
				return jsonResp(302, ``, map[string]string{"Location": "http://169.254.169.254/latest/meta-data"}), nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := makeMgr(tc.handler)
			if _, _, err := mgr.discoverEndpoints(t.Context(), "https://mcp.example.com/mcp"); err == nil {
				t.Fatalf("expected SSRF discovery case %s to fail", tc.name)
			}
		})
	}

	// Verify pending map cap eviction and Revoke cleanup of serverLocks.
	mgr := NewMCPOAuthManager(nil, nil)
	for i := range maxPendingMCPOAuth + 10 {
		srv := MCPServerConfig{
			Name:      "srv",
			Transport: "http",
			URL:       "https://mcp.example.com/mcp",
			Enabled:   true,
			OAuth: &MCPOAuthConfig{
				Enabled:     true,
				ClientID:    "cid",
				AuthURL:     "https://auth.example.com/auth",
				TokenURL:    "https://auth.example.com/token",
				RedirectURI: "http://127.0.0.1:9999/cb",
			},
		}
		if _, err := mgr.StartAuth(t.Context(), srv, nil, ""); err != nil {
			t.Fatalf("StartAuth %d failed: %v", i, err)
		}
	}
	mgr.mu.Lock()
	pendingLen := len(mgr.pending)
	mgr.mu.Unlock()
	if pendingLen > maxPendingMCPOAuth {
		t.Fatalf("expected pending capped at %d, got %d", maxPendingMCPOAuth, pendingLen)
	}
	_ = mgr.lockServer("srv")
	if err := mgr.Revoke("srv"); err != nil {
		t.Fatalf("Revoke failed: %v", err)
	}
	mgr.mu.Lock()
	_, hasLock := mgr.serverLocks["srv"]
	pendingAfterRevoke := len(mgr.pending)
	mgr.mu.Unlock()
	if hasLock || pendingAfterRevoke != 0 {
		t.Fatalf("expected Revoke to clear serverLocks and pending, hasLock=%v pending=%d", hasLock, pendingAfterRevoke)
	}
}

func TestMCPElicitationManualApprovalAndAutoApprove(t *testing.T) {
	var seenProtocolHeader atomic.Value
	var seenInitCaps atomic.Bool
	var seenMetaCaps atomic.Bool
	var replayChoices []string
	var replayMu sync.Mutex

	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenProtocolHeader.Store(r.Header.Get("MCP-Protocol-Version"))
		var req struct {
			ID     int64          `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			if caps, _ := req.Params["capabilities"].(map[string]any); caps != nil {
				if elic, _ := caps["elicitation"].(map[string]any); elic != nil {
					if _, hasForm := elic["form"]; hasForm {
						seenInitCaps.Store(true)
					}
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]any{"protocolVersion": "2026-07-28"},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"tools": []map[string]any{{
						"name":        "search_threads",
						"description": "Search Gmail threads",
						"annotations": map[string]any{"readOnlyHint": true},
					}},
				},
			})
		case "tools/call":
			if meta, _ := req.Params["_meta"].(map[string]any); meta != nil {
				if cc, _ := meta["io.modelcontextprotocol/clientCapabilities"].(map[string]any); cc != nil {
					if elic, _ := cc["elicitation"].(map[string]any); elic != nil {
						if _, hasForm := elic["form"]; hasForm {
							seenMetaCaps.Store(true)
						}
					}
				}
			}
			reqState, _ := req.Params["requestState"].(string)
			inResp, _ := req.Params["inputResponses"].(map[string]any)
			if reqState == "" || len(inResp) == 0 {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]any{
						"resultType":   "input_required",
						"requestState": `{"elicitationId":"el-1","requestBinding":"bind-1"}`,
						"inputRequests": map[string]any{
							"approval_choice": map[string]any{
								"method": "elicitation/create",
								"params": map[string]any{
									"mode":    "form",
									"message": "**Gmail Search**\nRead recent inbox threads?",
									"requestedSchema": map[string]any{
										"type": "object",
										"properties": map[string]any{
											"approval_choice": map[string]any{
												"type":      "string",
												"title":     "Allow Fleet Assistant to use search_threads?",
												"enum":      []string{"ALLOW_ONCE", "ALLOW_OWNER_ALWAYS", "ALLOW_TOOL_ALWAYS", "DENY_ONCE"},
												"enumNames": []string{"Allow once", "Always allow for these senders", "Always allow this tool", "Don't allow"},
											},
										},
									},
								},
							},
						},
					},
				})
				return
			}
			if reqState != `{"elicitationId":"el-1","requestBinding":"bind-1"}` {
				http.Error(w, "mismatched requestState: "+reqState, http.StatusBadRequest)
				return
			}
			ac, _ := inResp["approval_choice"].(map[string]any)
			content, _ := ac["content"].(map[string]any)
			choice, _ := content["approval_choice"].(string)
			replayMu.Lock()
			replayChoices = append(replayChoices, choice)
			replayMu.Unlock()
			if choice == "DENY_ONCE" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]any{
						"resultType": "complete",
						"isError":    true,
						"content":    []map[string]any{{"type": "text", "text": "Action canceled."}},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"resultType": "complete",
					"threads":    []map[string]any{{"id": "th-1", "subject": "Flight Confirmation"}},
				},
			})
		}
	}))
	defer mcpSrv.Close()

	var llmRound atomic.Int32
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat/completions":
			n := llmRound.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			if n == 1 {
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tc1\",\"type\":\"function\",\"function\":{\"name\":\"mcp__gmail__search_threads\",\"arguments\":\"{\\\"query\\\":\\\"in:inbox\\\"}\"}}]}}]}\n\n")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
				return
			}
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Found thread Flight Confirmation.\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":8}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case "/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		}
	}))
	defer mockLLM.Close()

	tmp := t.TempDir()
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()
	eng.mcp.SetHTTPClient(mcpSrv.Client())

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.BaseURL = mockLLM.URL
	cfg.APIKey = "sk-test"
	cfg.Routing.Mode = "off"
	cfg.MCPServers = []MCPServerConfig{{
		Name:           "gmail",
		Transport:      "http",
		URL:            mcpSrv.URL,
		Enabled:        true,
		ApprovalPolicy: MCPApprovalPolicyWritesOnly,
	}}
	if _, err := eng.UpdateConfig(cfg, false); err != nil {
		t.Fatal(err)
	}

	sub := eng.Subscribe()
	defer eng.Unsubscribe(sub)

	eng.Start(t.Context())

	// 1. Auto-Approve off: read-only search_threads pauses via mcp_elicitation approval card.
	if _, _, err := eng.EnqueueUserMessage("general", "check my recent emails", ""); err != nil {
		t.Fatal(err)
	}
	var apr Approval
	deadline := time.After(5 * time.Second)
	for apr.ID == "" {
		select {
		case ev := <-sub:
			if ev.Type == "approval" {
				apr, _ = ev.Data.(Approval)
			}
		case <-deadline:
			t.Fatalf("timed out waiting for mcp_elicitation approval")
		}
	}
	if apr.ToolName != MCPElicitationApprovalTool || apr.RiskLevel != "mcp_elicitation" {
		t.Fatalf("expected %s approval, got %+v", MCPElicitationApprovalTool, apr)
	}
	discordPayload := buildApprovalDiscordPayload(apr, "https://fleet.example/#chat/general")
	comps, _ := discordPayload["components"].([]map[string]any)
	btns, _ := comps[0]["components"].([]map[string]any)
	if len(btns) != 4 || btns[2]["custom_id"] != "fleet_apr:approve:"+apr.ID+":ALLOW_TOOL_ALWAYS" {
		t.Fatalf("unexpected Discord elicitation buttons: %+v", btns)
	}

	if _, err := eng.DecideApprovalWithScope(apr.ID, true, "ALLOW_TOOL_ALWAYS", ""); err != nil {
		t.Fatal(err)
	}
	var doneMsg Message
	deadline2 := time.After(5 * time.Second)
	for doneMsg.ID == 0 {
		select {
		case ev := <-sub:
			if ev.Type == "message_done" {
				doneMsg, _ = ev.Data.(Message)
			}
		case <-deadline2:
			t.Fatalf("timed out waiting for resumed assistant reply")
		}
	}
	if !strings.Contains(doneMsg.Content, "Flight Confirmation") {
		t.Fatalf("expected resumed reply mentioning Flight Confirmation, got %q", doneMsg.Content)
	}

	// 2. Manual Deny sends DENY_ONCE to MCP server.
	tctxManual := &toolExecContext{ChannelID: "general", Source: "chat", Config: eng.Config(), BypassHITL: false}
	_, err = eng.executeTool(t.Context(), ToolCall{
		ID:       "deny1",
		Type:     "function",
		Function: ToolCallFunction{Name: "mcp__gmail__search_threads", Arguments: `{"query":"is:unread"}`},
	}, tctxManual)
	aprDenyErr, ok := errors.AsType[*approvalRequiredError](err)
	if !ok || aprDenyErr.ToolName != MCPElicitationApprovalTool {
		t.Fatalf("expected approvalRequiredError for manual deny case, got %v", err)
	}
	denyApr, err := eng.store.CreateApproval(Approval{
		ChannelID:  "general",
		ToolName:   aprDenyErr.ToolName,
		ServerName: aprDenyErr.ServerName,
		ArgsJSON:   aprDenyErr.ArgsJSON,
		Summary:    aprDenyErr.Summary,
		RiskLevel:  aprDenyErr.RiskLevel,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.DecideApprovalWithScope(denyApr.ID, false, "DENY_ONCE", "user denied"); err != nil {
		t.Fatal(err)
	}

	// 3. Auto-Approve on (BypassHITL=true): server elicitation is answered with ALLOW_ONCE automatically.
	tctxAuto := &toolExecContext{ChannelID: "general", Source: "chat", Config: eng.Config(), BypassHITL: true}
	autoOut, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "auto1",
		Type:     "function",
		Function: ToolCallFunction{Name: "mcp__gmail__search_threads", Arguments: `{"query":"in:inbox"}`},
	}, tctxAuto)
	if err != nil || !strings.Contains(autoOut, "Flight Confirmation") {
		t.Fatalf("expected AutoApprove to replay ALLOW_ONCE and return threads, got out=%q err=%v", autoOut, err)
	}

	if gotProto, _ := seenProtocolHeader.Load().(string); gotProto != "2026-07-28" || !seenInitCaps.Load() || !seenMetaCaps.Load() {
		t.Fatalf("expected 2026-07-28 header (%q) + init caps (%v) + meta caps (%v)", gotProto, seenInitCaps.Load(), seenMetaCaps.Load())
	}
	replayMu.Lock()
	defer replayMu.Unlock()
	if len(replayChoices) < 2 || replayChoices[0] != "ALLOW_TOOL_ALWAYS" || replayChoices[len(replayChoices)-1] != "ALLOW_ONCE" {
		t.Fatalf("unexpected replay choices: %+v", replayChoices)
	}
}

func TestMCPStateless20260728ServerDiscoverAndLegacyFallback(t *testing.T) {
	t.Run("stateless_2026_07_28_server_discover_and_strict_headers", func(t *testing.T) {
		var sawDiscover atomic.Bool
		var sawInitialize atomic.Bool
		var sawInitializedNotify atomic.Bool
		var listProtoHeader atomic.Value
		var listMetaCaps atomic.Bool
		var callMetaCaps atomic.Bool
		var seenParamHeader atomic.Value

		sendHeaderMismatch := func(w http.ResponseWriter, id int64, msg string) {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"error":   map[string]any{"code": -32020, "message": "Header mismatch: " + msg},
			})
		}

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				ID     int64          `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.Header().Set("Content-Type", "application/json")

			if gotMethod := r.Header.Get("Mcp-Method"); gotMethod != req.Method {
				sendHeaderMismatch(w, req.ID, fmt.Sprintf("Mcp-Method=%q != %q", gotMethod, req.Method))
				return
			}
			meta, _ := req.Params["_meta"].(map[string]any)
			metaProto, _ := meta["io.modelcontextprotocol/protocolVersion"].(string)
			if gotProto := r.Header.Get("MCP-Protocol-Version"); gotProto == "" || gotProto != metaProto {
				sendHeaderMismatch(w, req.ID, fmt.Sprintf("MCP-Protocol-Version=%q != _meta=%q", gotProto, metaProto))
				return
			}

			switch req.Method {
			case "server/discover":
				sawDiscover.Store(true)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]any{
						"resultType":        "complete",
						"supportedVersions": []string{"2026-07-28"},
						"_meta": map[string]any{
							"io.modelcontextprotocol/serverInfo": map[string]any{
								"name":    "gmail",
								"version": "1.0.0",
							},
						},
					},
				})
			case "initialize":
				sawInitialize.Store(true)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"error":   map[string]any{"code": -32601, "message": "Method not supported"},
				})
			case "notifications/initialized":
				sawInitializedNotify.Store(true)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"error":   map[string]any{"code": -32601, "message": "Method not supported"},
				})
			case "tools/list":
				listProtoHeader.Store(r.Header.Get("MCP-Protocol-Version"))
				if cc, _ := meta["io.modelcontextprotocol/clientCapabilities"].(map[string]any); cc != nil {
					if elic, _ := cc["elicitation"].(map[string]any); elic != nil {
						if _, ok := elic["form"]; ok {
							listMetaCaps.Store(true)
						}
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result": map[string]any{
						"tools": []map[string]any{
							{
								"name":        "list_messages",
								"description": "List Gmail messages",
								"inputSchema": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"q":       map[string]any{"type": "string"},
										"user_id": map[string]any{"type": "string", "x-mcp-header": "UserId"},
									},
								},
							},
						},
					},
				})
			case "tools/call":
				wantName, _ := req.Params["name"].(string)
				if gotName := r.Header.Get("Mcp-Name"); gotName != wantName {
					sendHeaderMismatch(w, req.ID, fmt.Sprintf("Mcp-Name=%q != %q", gotName, wantName))
					return
				}
				args, _ := req.Params["arguments"].(map[string]any)
				if uid, _ := args["user_id"].(string); uid != "" {
					seenParamHeader.Store(r.Header.Get("Mcp-Param-UserId"))
					if r.Header.Get("Mcp-Param-UserId") == "" {
						sendHeaderMismatch(w, req.ID, "missing Mcp-Param-UserId")
						return
					}
				}
				if cc, _ := meta["io.modelcontextprotocol/clientCapabilities"].(map[string]any); cc != nil {
					if elic, _ := cc["elicitation"].(map[string]any); elic != nil {
						if _, ok := elic["form"]; ok {
							callMetaCaps.Store(true)
						}
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result":  map[string]any{"messages": []string{"msg-1"}},
				})
			}
		}))
		defer srv.Close()

		mgr := NewMCPManager([]MCPServerConfig{{
			Name:           "gmail",
			Transport:      "http",
			URL:            srv.URL,
			Enabled:        true,
			ApprovalPolicy: MCPApprovalPolicyNone,
		}}, nil)
		defer mgr.Close()

		tools := mgr.ListTools(t.Context())
		if len(tools) != 1 || tools[0].RemoteName != "list_messages" {
			t.Fatalf("expected list_messages tool, got %+v", tools)
		}
		res, err := mgr.CallTool(t.Context(), "mcp__gmail__list_messages", map[string]any{
			"q":       "in:inbox",
			"user_id": "José@example.com",
		})
		if err != nil {
			t.Fatalf("CallTool failed: %v", err)
		}
		if resMap, _ := res.(map[string]any); resMap == nil {
			t.Fatalf("expected map result, got %#v", res)
		}
		wantEncoded := "=?base64?" + base64.StdEncoding.EncodeToString([]byte("José@example.com")) + "?="
		if gotParam, _ := seenParamHeader.Load().(string); gotParam != wantEncoded {
			t.Fatalf("expected Base64-encoded Mcp-Param-UserId %q, got %q", wantEncoded, gotParam)
		}
		if !sawDiscover.Load() {
			t.Fatal("expected server/discover to be called")
		}
		if sawInitialize.Load() || sawInitializedNotify.Load() {
			t.Fatalf("expected initialize (%v) and notifications/initialized (%v) NOT to be called on stateless server", sawInitialize.Load(), sawInitializedNotify.Load())
		}
		if gotProto, _ := listProtoHeader.Load().(string); gotProto != "2026-07-28" || !listMetaCaps.Load() || !callMetaCaps.Load() {
			t.Fatalf("expected 2026-07-28 header (%q) + list/call meta caps (%v/%v)", gotProto, listMetaCaps.Load(), callMetaCaps.Load())
		}
	})

	t.Run("legacy_2024_11_05_fallback", func(t *testing.T) {
		var sawLegacyInit atomic.Bool
		var sawInitializedNotify atomic.Bool
		var listProtoHeader atomic.Value
		var callProtoHeader atomic.Value
		var sawMetaOnList atomic.Bool
		var sawMetaOnCall atomic.Bool
		var sawMethodHeaderOnLegacy atomic.Bool
		var sawNameHeaderOnLegacy atomic.Bool

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				ID     int64          `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.Header().Set("Content-Type", "application/json")
			switch req.Method {
			case "server/discover":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"error":   map[string]any{"code": -32601, "message": "Method not found"},
				})
			case "initialize":
				pv, _ := req.Params["protocolVersion"].(string)
				if pv == "2026-07-28" || r.Header.Get("MCP-Protocol-Version") == "2026-07-28" {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"jsonrpc": "2.0",
						"id":      req.ID,
						"error":   map[string]any{"code": -32022, "message": "Unsupported protocol version"},
					})
					return
				}
				if pv != "2024-11-05" {
					http.Error(w, "bad protocolVersion "+pv, http.StatusBadRequest)
					return
				}
				sawLegacyInit.Store(true)
				w.Header().Set("Mcp-Session-Id", "legacy-sess-1")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result":  map[string]any{"protocolVersion": "2024-11-05"},
				})
			case "notifications/initialized":
				sawInitializedNotify.Store(true)
				w.WriteHeader(http.StatusAccepted)
			case "tools/list":
				listProtoHeader.Store(r.Header.Get("MCP-Protocol-Version"))
				if r.Header.Get("Mcp-Method") != "" {
					sawMethodHeaderOnLegacy.Store(true)
				}
				if _, hasMeta := req.Params["_meta"]; hasMeta {
					sawMetaOnList.Store(true)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result":  map[string]any{"tools": []map[string]any{{"name": "legacy_ping"}}},
				})
			case "tools/call":
				callProtoHeader.Store(r.Header.Get("MCP-Protocol-Version"))
				if r.Header.Get("Mcp-Method") != "" {
					sawMethodHeaderOnLegacy.Store(true)
				}
				if r.Header.Get("Mcp-Name") != "" {
					sawNameHeaderOnLegacy.Store(true)
				}
				if _, hasMeta := req.Params["_meta"]; hasMeta {
					sawMetaOnCall.Store(true)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result":  map[string]any{"pong": true},
				})
			}
		}))
		defer srv.Close()

		mgr := NewMCPManager([]MCPServerConfig{{
			Name:           "legacy",
			Transport:      "http",
			URL:            srv.URL,
			Enabled:        true,
			ApprovalPolicy: MCPApprovalPolicyNone,
		}}, nil)
		defer mgr.Close()

		res, err := mgr.CallTool(t.Context(), "mcp__legacy__legacy_ping", nil)
		if err != nil {
			t.Fatalf("legacy CallTool failed: %v", err)
		}
		if resMap, _ := res.(map[string]any); resMap == nil || resMap["pong"] != true {
			t.Fatalf("unexpected legacy result: %#v", res)
		}
		if !sawLegacyInit.Load() || !sawInitializedNotify.Load() {
			t.Fatalf("expected legacy initialize (%v) + notifications/initialized (%v)", sawLegacyInit.Load(), sawInitializedNotify.Load())
		}
		if listProtoHeader.Load() != "2024-11-05" || callProtoHeader.Load() != "2024-11-05" {
			t.Fatalf("expected 2024-11-05 protocol headers, got list=%v call=%v", listProtoHeader.Load(), callProtoHeader.Load())
		}
		if sawMetaOnList.Load() || sawMetaOnCall.Load() || sawMethodHeaderOnLegacy.Load() || sawNameHeaderOnLegacy.Load() {
			t.Fatalf("expected _meta and Mcp-Method/Mcp-Name omitted after downgrade to 2024-11-05, got meta=(%v,%v) headers=(%v,%v)",
				sawMetaOnList.Load(), sawMetaOnCall.Load(), sawMethodHeaderOnLegacy.Load(), sawNameHeaderOnLegacy.Load())
		}
	})

	t.Run("http_400_header_mismatch_fails_fast_without_initialize", func(t *testing.T) {
		var sawInitialize atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Method == "initialize" {
				sawInitialize.Store(true)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error":   map[string]any{"code": -32020, "message": "Header mismatch (-32020)"},
			})
		}))
		defer srv.Close()

		mgr := NewMCPManager([]MCPServerConfig{{
			Name:      "mismatch",
			Transport: "http",
			URL:       srv.URL,
			Enabled:   true,
		}}, nil)
		defer mgr.Close()

		_, err := mgr.TestServer(t.Context(), MCPServerConfig{
			Name:      "mismatch",
			Transport: "http",
			URL:       srv.URL,
			Enabled:   true,
		})
		if err == nil || !strings.Contains(err.Error(), "-32020") {
			t.Fatalf("expected HTTP 400 -32020 error, got %v", err)
		}
		if sawInitialize.Load() {
			t.Fatal("expected HTTP 400 -32020 on server/discover NOT to fall back to initialize")
		}
	})

	t.Run("http_403_fails_fast_without_initialize", func(t *testing.T) {
		var sawInitialize atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Method string `json:"method"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Method == "initialize" {
				sawInitialize.Store(true)
			}
			http.Error(w, "forbidden", http.StatusForbidden)
		}))
		defer srv.Close()

		mgr := NewMCPManager([]MCPServerConfig{{
			Name:      "blocked",
			Transport: "http",
			URL:       srv.URL,
			Enabled:   true,
		}}, nil)
		defer mgr.Close()

		_, err := mgr.TestServer(t.Context(), MCPServerConfig{
			Name:      "blocked",
			Transport: "http",
			URL:       srv.URL,
			Enabled:   true,
		})
		if err == nil || !strings.Contains(err.Error(), "403") {
			t.Fatalf("expected HTTP 403 error, got %v", err)
		}
		if sawInitialize.Load() {
			t.Fatal("expected HTTP 403 on server/discover NOT to fall back to initialize")
		}
	})
}
