package assistant

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
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
			if pv, _ := req.Params["protocolVersion"].(string); pv != "2025-03-26" {
				http.Error(w, "unexpected protocolVersion "+pv, http.StatusBadRequest)
				return
			}
			w.Header().Set("Mcp-Session-Id", "sess-oauth-1")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-03-26"}})
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
	if _, err := eng.executeTool(t.Context(), ToolCall{ID: "rc", Type: "function", Function: ToolCallFunction{Name: "render_card", Arguments: `{"kind":"mcp_oauth","data":{"server":"gworkspace"}}`}}, tctxConnect); err != nil {
		t.Fatalf("render_card mcp_oauth failed: %v", err)
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
