package assistant

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// GoogleOAuthAuthURL is Google's standard OAuth 2.0 authorization endpoint.
	GoogleOAuthAuthURL = "https://accounts.google.com/o/oauth2/v2/auth"
	// GoogleOAuthTokenURL is Google's standard OAuth 2.0 token endpoint.
	GoogleOAuthTokenURL = "https://oauth2.googleapis.com/token"

	mcpOAuthStateTTL    = 10 * time.Minute
	mcpOAuthRefreshSkew = 60 * time.Second
	maxPendingMCPOAuth  = 256
)

// MCPOAuthTokenStore persists OAuth access and refresh tokens per MCP server.
type MCPOAuthTokenStore interface {
	GetMCPOAuthToken(serverName string) (MCPOAuthToken, bool, error)
	UpsertMCPOAuthToken(tok MCPOAuthToken) error
	DeleteMCPOAuthToken(serverName string) error
}

// MCPOAuthStartResult is returned when initiating an OAuth 2.0/2.1 PKCE consent flow.
type MCPOAuthStartResult struct {
	ServerName   string   `json:"server_name"`
	AuthorizeURL string   `json:"authorize_url"`
	State        string   `json:"state"`
	RedirectURI  string   `json:"redirect_uri"`
	Scopes       []string `json:"scopes,omitempty"`
	ExpiresAt    string   `json:"expires_at"`
}

// MCPOAuthCompleteResult summarizes the outcome of exchanging an OAuth authorization code.
type MCPOAuthCompleteResult struct {
	ServerName string   `json:"server_name"`
	Authorized bool     `json:"authorized"`
	Scope      string   `json:"scope,omitempty"`
	ExpiresAt  string   `json:"expires_at,omitempty"`
	ToolCount  int      `json:"tool_count"`
	Tools      []string `json:"tools,omitempty"`
	TestError  string   `json:"test_error,omitempty"`
}

// MCPOAuthStatus reports non-secret OAuth authorization status for an MCP server.
type MCPOAuthStatus struct {
	Authorized      bool   `json:"authorized"`
	HasRefreshToken bool   `json:"has_refresh_token"`
	ExpiresAt       string `json:"expires_at,omitempty"`
	Scope           string `json:"scope,omitempty"`
}

type pendingMCPOAuth struct {
	ServerName   string
	RedirectURI  string
	CodeVerifier string
	TokenURL     string
	ExpiresAt    time.Time
}

// MCPOAuthManager handles OAuth 2.0/2.1 discovery, PKCE authorization, token exchange, and refresh.
type MCPOAuthManager struct {
	mu            sync.Mutex
	store         MCPOAuthTokenStore
	httpClient    *http.Client
	now           func() time.Time
	lookupIP      func(context.Context, string) ([]netip.Addr, error)
	allowLoopback bool
	pending       map[string]pendingMCPOAuth
	serverLocks   map[string]*sync.Mutex
}

// NewMCPOAuthManager constructs an MCPOAuthManager backed by store and httpClient.
func NewMCPOAuthManager(store MCPOAuthTokenStore, httpClient *http.Client) *MCPOAuthManager {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &MCPOAuthManager{
		store:       store,
		httpClient:  httpClient,
		now:         time.Now,
		pending:     make(map[string]pendingMCPOAuth),
		serverLocks: make(map[string]*sync.Mutex),
	}
}

// SetStore replaces the token persistence store.
func (m *MCPOAuthManager) SetStore(store MCPOAuthTokenStore) {
	m.mu.Lock()
	m.store = store
	m.mu.Unlock()
}

// SetHTTPClient replaces the HTTP client used for discovery and token requests.
func (m *MCPOAuthManager) SetHTTPClient(cli *http.Client) {
	if cli == nil {
		return
	}
	m.mu.Lock()
	m.httpClient = cli
	m.mu.Unlock()
}

func (m *MCPOAuthManager) client() *http.Client {
	m.mu.Lock()
	cli := m.httpClient
	m.mu.Unlock()
	if cli == nil {
		return &http.Client{Timeout: 30 * time.Second}
	}
	return cli
}

func (m *MCPOAuthManager) currentStore() MCPOAuthTokenStore {
	m.mu.Lock()
	st := m.store
	m.mu.Unlock()
	return st
}

func (m *MCPOAuthManager) lockServer(name string) *sync.Mutex {
	clean := strings.ToLower(strings.TrimSpace(name))
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.serverLocks[clean]
	if !ok {
		l = &sync.Mutex{}
		m.serverLocks[clean] = l
	}
	return l
}

func randomOAuthToken(byteLen int) (string, error) {
	buf := make([]byte, byteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func pkceS256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func isGoogleMCPOAuth(srv MCPServerConfig) bool {
	if srv.OAuth != nil && srv.OAuth.Provider == "google" {
		return true
	}
	return isGoogleAPIHost(srv.URL)
}

func isLoopbackHost(host string) bool {
	clean := strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	if clean == "localhost" || strings.HasSuffix(clean, ".localhost") {
		return true
	}
	if ip, err := netip.ParseAddr(clean); err == nil {
		return ip.Unmap().IsLoopback()
	}
	return false
}

func isLoopbackURL(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	return isLoopbackHost(u.Hostname())
}

func (m *MCPOAuthManager) resolveOAuthHostIPs(ctx context.Context, host string) ([]netip.Addr, error) {
	if m.lookupIP != nil {
		return m.lookupIP(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

func (m *MCPOAuthManager) validateOAuthURL(ctx context.Context, rawURL string, allowLoopback bool) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("invalid OAuth URL %q", rawURL)
	}
	scheme := strings.ToLower(u.Scheme)
	cleanHost := strings.Trim(strings.TrimSpace(u.Hostname()), "[]")
	switch scheme {
	case "https":
	case "http":
		if !allowLoopback || !isLoopbackHost(cleanHost) {
			return fmt.Errorf("OAuth URL %q must use https://", rawURL)
		}
	default:
		return fmt.Errorf("OAuth URL %q must use https://", rawURL)
	}
	if ip, parseErr := netip.ParseAddr(cleanHost); parseErr == nil {
		if allowLoopback && ip.Unmap().IsLoopback() {
			return nil
		}
		if IsBlockedEgressAddr(ip) {
			return fmt.Errorf("OAuth URL %q (%s) blocked: private/internal/mesh IP", rawURL, ip)
		}
		return nil
	}
	if isLoopbackHost(cleanHost) {
		if !allowLoopback {
			return fmt.Errorf("OAuth URL %q blocked: loopback host not allowed", rawURL)
		}
		return nil
	}
	ips, lookupErr := m.resolveOAuthHostIPs(ctx, cleanHost)
	if lookupErr != nil {
		if m.lookupIP != nil {
			return fmt.Errorf("OAuth DNS lookup failed for %q: %w", cleanHost, lookupErr)
		}
		return nil
	}
	for _, ip := range ips {
		if allowLoopback && ip.Unmap().IsLoopback() {
			continue
		}
		if IsBlockedEgressAddr(ip) {
			return fmt.Errorf("OAuth URL %q resolved to blocked IP %s", rawURL, ip)
		}
	}
	return nil
}

func (m *MCPOAuthManager) dialOAuthTCP(allowLoopback bool) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid OAuth address %q", addr)
		}
		cleanHost := strings.Trim(strings.TrimSpace(host), "[]")
		var ips []netip.Addr
		if literal, parseErr := netip.ParseAddr(cleanHost); parseErr == nil {
			ips = []netip.Addr{literal}
		} else {
			ips, err = m.resolveOAuthHostIPs(ctx, cleanHost)
			if err != nil {
				return nil, fmt.Errorf("OAuth DNS lookup failed for %q: %w", cleanHost, err)
			}
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no IP addresses resolved for %q", cleanHost)
		}
		for _, ip := range ips {
			if allowLoopback && ip.Unmap().IsLoopback() {
				continue
			}
			if IsBlockedEgressAddr(ip) {
				return nil, fmt.Errorf("OAuth egress to %q (%s) blocked: private/internal/mesh IP", cleanHost, ip)
			}
		}
		dialer := &net.Dialer{Timeout: 15 * time.Second}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].Unmap().String(), port))
	}
}

func (m *MCPOAuthManager) oauthHTTPClient(allowLoopback bool) *http.Client {
	base := m.client()
	cli := *base
	if cli.Transport == nil {
		cli.Transport = &http.Transport{
			DialContext:           m.dialOAuthTCP(allowLoopback),
			ForceAttemptHTTP2:     true,
			ResponseHeaderTimeout: 15 * time.Second,
			IdleConnTimeout:       15 * time.Second,
		}
	}
	prevRedirect := cli.CheckRedirect
	cli.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if err := m.validateOAuthURL(req.Context(), req.URL.String(), allowLoopback); err != nil {
			return err
		}
		if prevRedirect != nil {
			return prevRedirect(req, via)
		}
		return nil
	}
	return &cli
}

func (m *MCPOAuthManager) fetchJSON(ctx context.Context, targetURL string, allowLoopback bool, dst any) error {
	if err := m.validateOAuthURL(ctx, targetURL, allowLoopback); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := m.oauthHTTPClient(allowLoopback).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, targetURL)
	}
	return json.Unmarshal(body, dst)
}

func (m *MCPOAuthManager) discoverEndpoints(ctx context.Context, serverURL string) (authURL, tokenURL string, err error) {
	u, err := url.Parse(strings.TrimSpace(serverURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", "", fmt.Errorf("invalid MCP server URL %q for OAuth discovery", serverURL)
	}
	allowLoopback := m.allowLoopback || isLoopbackURL(serverURL)
	origin := u.Scheme + "://" + u.Host
	authServerBase := origin

	var prm struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if m.fetchJSON(ctx, origin+"/.well-known/oauth-protected-resource", allowLoopback, &prm) == nil && len(prm.AuthorizationServers) > 0 {
		if cand := strings.TrimRight(strings.TrimSpace(prm.AuthorizationServers[0]), "/"); cand != "" {
			if vErr := m.validateOAuthURL(ctx, cand, allowLoopback); vErr != nil {
				return "", "", fmt.Errorf("OAuth discovery rejected authorization_server %q: %w", cand, vErr)
			}
			authServerBase = cand
		}
	}

	var meta struct {
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
	}
	metaURL := authServerBase + "/.well-known/oauth-authorization-server"
	if dErr := m.fetchJSON(ctx, metaURL, allowLoopback, &meta); dErr != nil {
		return "", "", fmt.Errorf("OAuth discovery failed at %s: %w", metaURL, dErr)
	}
	authURL = strings.TrimSpace(meta.AuthorizationEndpoint)
	tokenURL = strings.TrimSpace(meta.TokenEndpoint)
	if authURL == "" || tokenURL == "" {
		return "", "", fmt.Errorf("OAuth metadata at %s missing authorization_endpoint or token_endpoint", metaURL)
	}
	if err := m.validateOAuthURL(ctx, authURL, allowLoopback); err != nil {
		return "", "", fmt.Errorf("OAuth discovery rejected authorization_endpoint %q: %w", authURL, err)
	}
	if err := m.validateOAuthURL(ctx, tokenURL, allowLoopback); err != nil {
		return "", "", fmt.Errorf("OAuth discovery rejected token_endpoint %q: %w", tokenURL, err)
	}
	return authURL, tokenURL, nil
}

func (m *MCPOAuthManager) resolveEndpoints(ctx context.Context, srv MCPServerConfig) (authURL, tokenURL string, err error) {
	if srv.OAuth == nil {
		return "", "", errors.New("oauth config is nil")
	}
	allowLoopback := m.allowLoopback || isLoopbackURL(srv.URL)
	authURL = strings.TrimSpace(srv.OAuth.AuthURL)
	tokenURL = strings.TrimSpace(srv.OAuth.TokenURL)
	if authURL != "" && tokenURL != "" {
		if err := m.validateOAuthURL(ctx, authURL, allowLoopback); err != nil {
			return "", "", err
		}
		if err := m.validateOAuthURL(ctx, tokenURL, allowLoopback); err != nil {
			return "", "", err
		}
		return authURL, tokenURL, nil
	}
	if isGoogleMCPOAuth(srv) {
		resolvedAuth := cmp.Or(authURL, GoogleOAuthAuthURL)
		resolvedTok := cmp.Or(tokenURL, GoogleOAuthTokenURL)
		if err := m.validateOAuthURL(ctx, resolvedAuth, allowLoopback); err != nil {
			return "", "", err
		}
		if err := m.validateOAuthURL(ctx, resolvedTok, allowLoopback); err != nil {
			return "", "", err
		}
		return resolvedAuth, resolvedTok, nil
	}
	dAuth, dTok, dErr := m.discoverEndpoints(ctx, srv.URL)
	if dErr != nil {
		return "", "", dErr
	}
	resolvedAuth := cmp.Or(authURL, dAuth)
	resolvedTok := cmp.Or(tokenURL, dTok)
	if err := m.validateOAuthURL(ctx, resolvedAuth, allowLoopback); err != nil {
		return "", "", err
	}
	if err := m.validateOAuthURL(ctx, resolvedTok, allowLoopback); err != nil {
		return "", "", err
	}
	return resolvedAuth, resolvedTok, nil
}

func (m *MCPOAuthManager) resolveClientCreds(ctx context.Context, srv MCPServerConfig, opReader OPReaderFunc) (clientID, clientSecret string, secrets []string, err error) {
	if srv.OAuth == nil {
		return "", "", nil, errors.New("oauth config is nil")
	}
	if opReader == nil {
		opReader = defaultOPReader
	}
	clientID = strings.TrimSpace(srv.OAuth.ClientID)
	if ref := strings.TrimSpace(srv.OAuth.ClientIDOPRef); ref != "" {
		val, rErr := opReader(ctx, ref)
		if rErr != nil {
			return "", "", nil, fmt.Errorf("resolve oauth.client_id_op_ref for MCP %s: %w", srv.Name, rErr)
		}
		clientID = strings.TrimSpace(val)
	}
	if clientID == "" {
		return "", "", nil, fmt.Errorf("MCP %q oauth client_id is empty", srv.Name)
	}
	clientSecret = strings.TrimSpace(srv.OAuth.ClientSecret)
	if ref := strings.TrimSpace(srv.OAuth.ClientSecretOPRef); ref != "" {
		val, rErr := opReader(ctx, ref)
		if rErr != nil {
			return "", "", nil, fmt.Errorf("resolve oauth.client_secret_op_ref for MCP %s: %w", srv.Name, rErr)
		}
		clientSecret = strings.TrimSpace(val)
	}
	if clientSecret != "" {
		secrets = append(secrets, clientSecret)
	}
	return clientID, clientSecret, secrets, nil
}

func (m *MCPOAuthManager) pruneExpiredLocked(now time.Time) {
	for st, p := range m.pending {
		if now.After(p.ExpiresAt) {
			delete(m.pending, st)
		}
	}
}

func (m *MCPOAuthManager) evictPendingCapLocked(now time.Time) {
	m.pruneExpiredLocked(now)
	for len(m.pending) >= maxPendingMCPOAuth {
		var oldestKey string
		var oldestExp time.Time
		for st, p := range m.pending {
			if oldestKey == "" || p.ExpiresAt.Before(oldestExp) {
				oldestKey = st
				oldestExp = p.ExpiresAt
			}
		}
		if oldestKey == "" {
			break
		}
		delete(m.pending, oldestKey)
	}
}

// StartAuth builds an OAuth 2.0/2.1 PKCE S256 authorization URL and tracks the pending state.
func (m *MCPOAuthManager) StartAuth(ctx context.Context, srv MCPServerConfig, opReader OPReaderFunc, redirectURI string) (MCPOAuthStartResult, error) {
	srv = NormalizeMCPServerConfig(srv)
	if srv.OAuth == nil || !srv.OAuth.Enabled {
		return MCPOAuthStartResult{}, newErr(fmt.Sprintf("MCP server %q does not have OAuth enabled", srv.Name), http.StatusBadRequest)
	}
	if err := ValidateMCPServerConfig(srv, false); err != nil {
		return MCPOAuthStartResult{}, err
	}
	redir := cmp.Or(strings.TrimSpace(redirectURI), srv.OAuth.RedirectURI)
	if redir == "" {
		return MCPOAuthStartResult{}, newErr("redirect_uri is required to start OAuth flow", http.StatusBadRequest)
	}
	if err := validateHTTPURLField(redir, "redirect_uri"); err != nil {
		return MCPOAuthStartResult{}, err
	}
	authURL, tokenURL, err := m.resolveEndpoints(ctx, srv)
	if err != nil {
		return MCPOAuthStartResult{}, newErr(err.Error(), http.StatusBadRequest)
	}
	clientID, _, secrets, err := m.resolveClientCreds(ctx, srv, opReader)
	if err != nil {
		return MCPOAuthStartResult{}, newErr(scrubAllSecrets(err.Error(), secrets), http.StatusBadRequest)
	}
	verifier, err := randomOAuthToken(32)
	if err != nil {
		return MCPOAuthStartResult{}, err
	}
	state, err := randomOAuthToken(24)
	if err != nil {
		return MCPOAuthStartResult{}, err
	}
	u, err := url.Parse(authURL)
	if err != nil {
		return MCPOAuthStartResult{}, newErr("invalid authorize URL", http.StatusBadRequest)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redir)
	q.Set("state", state)
	q.Set("code_challenge", pkceS256Challenge(verifier))
	q.Set("code_challenge_method", "S256")
	if len(srv.OAuth.Scopes) > 0 {
		q.Set("scope", strings.Join(srv.OAuth.Scopes, " "))
	}
	if isGoogleMCPOAuth(srv) {
		q.Set("access_type", "offline")
		q.Set("prompt", "consent")
	}
	keys := make([]string, 0, len(srv.OAuth.ExtraAuthParams))
	for k := range srv.OAuth.ExtraAuthParams {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		q.Set(k, srv.OAuth.ExtraAuthParams[k])
	}
	u.RawQuery = q.Encode()

	now := m.now().UTC()
	exp := now.Add(mcpOAuthStateTTL)
	m.mu.Lock()
	m.evictPendingCapLocked(now)
	m.pending[state] = pendingMCPOAuth{
		ServerName:   srv.Name,
		RedirectURI:  redir,
		CodeVerifier: verifier,
		TokenURL:     tokenURL,
		ExpiresAt:    exp,
	}
	m.mu.Unlock()

	return MCPOAuthStartResult{
		ServerName:   srv.Name,
		AuthorizeURL: u.String(),
		State:        state,
		RedirectURI:  redir,
		Scopes:       append([]string(nil), srv.OAuth.Scopes...),
		ExpiresAt:    exp.Format(time.RFC3339),
	}, nil
}

// LookupPendingServer returns the server name associated with an active pending OAuth state.
func (m *MCPOAuthManager) LookupPendingServer(state string) (string, error) {
	clean := strings.TrimSpace(state)
	if clean == "" {
		return "", newErr("missing OAuth state", http.StatusBadRequest)
	}
	now := m.now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneExpiredLocked(now)
	p, ok := m.pending[clean]
	if !ok {
		return "", newErr("invalid or expired OAuth state", http.StatusBadRequest)
	}
	return p.ServerName, nil
}

func (m *MCPOAuthManager) consumePending(serverName, state string) (pendingMCPOAuth, error) {
	cleanState := strings.TrimSpace(state)
	if cleanState == "" {
		return pendingMCPOAuth{}, newErr("OAuth state is required", http.StatusBadRequest)
	}
	now := m.now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneExpiredLocked(now)
	p, ok := m.pending[cleanState]
	if !ok || !strings.EqualFold(p.ServerName, strings.TrimSpace(serverName)) {
		return pendingMCPOAuth{}, newErr("invalid or expired OAuth state", http.StatusBadRequest)
	}
	delete(m.pending, cleanState)
	return p, nil
}

type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func (m *MCPOAuthManager) postTokenForm(ctx context.Context, tokenURL string, allowLoopback bool, form url.Values, secrets []string) (oauthTokenResponse, error) {
	if err := m.validateOAuthURL(ctx, tokenURL, allowLoopback); err != nil {
		return oauthTokenResponse{}, errors.New(scrubAllSecrets(err.Error(), secrets))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return oauthTokenResponse{}, errors.New(scrubAllSecrets(err.Error(), secrets))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := m.oauthHTTPClient(allowLoopback).Do(req)
	if err != nil {
		return oauthTokenResponse{}, errors.New(scrubAllSecrets(err.Error(), secrets))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return oauthTokenResponse{}, errors.New(scrubAllSecrets(err.Error(), secrets))
	}
	var tr oauthTokenResponse
	_ = json.Unmarshal(body, &tr)
	secrets = append(secrets, tr.AccessToken, tr.RefreshToken)
	if resp.StatusCode >= 400 || tr.Error != "" {
		msg := cmp.Or(strings.TrimSpace(tr.ErrorDesc), strings.TrimSpace(tr.Error), strings.TrimSpace(string(body)))
		return oauthTokenResponse{}, newErr(scrubAllSecrets(fmt.Sprintf("OAuth token endpoint error (HTTP %d): %s", resp.StatusCode, msg), secrets), http.StatusBadGateway)
	}
	if strings.TrimSpace(tr.AccessToken) == "" {
		return oauthTokenResponse{}, newErr("OAuth token endpoint returned empty access_token", http.StatusBadGateway)
	}
	return tr, nil
}

// ExchangeCode exchanges an authorization code + PKCE verifier for tokens and persists them.
func (m *MCPOAuthManager) ExchangeCode(ctx context.Context, srv MCPServerConfig, opReader OPReaderFunc, state, code string) (MCPOAuthToken, error) {
	srv = NormalizeMCPServerConfig(srv)
	cleanCode := strings.TrimSpace(code)
	if cleanCode == "" {
		return MCPOAuthToken{}, newErr("OAuth authorization code is required", http.StatusBadRequest)
	}
	pending, err := m.consumePending(srv.Name, state)
	if err != nil {
		return MCPOAuthToken{}, err
	}
	clientID, clientSecret, secrets, err := m.resolveClientCreds(ctx, srv, opReader)
	if err != nil {
		return MCPOAuthToken{}, newErr(scrubAllSecrets(err.Error(), secrets), http.StatusBadRequest)
	}
	secrets = append(secrets, cleanCode)
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {cleanCode},
		"redirect_uri":  {pending.RedirectURI},
		"client_id":     {clientID},
		"code_verifier": {pending.CodeVerifier},
	}
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	allowLoopback := m.allowLoopback || isLoopbackURL(srv.URL)
	tr, err := m.postTokenForm(ctx, pending.TokenURL, allowLoopback, form, secrets)
	if err != nil {
		return MCPOAuthToken{}, err
	}
	st := m.currentStore()
	if st == nil {
		return MCPOAuthToken{}, errors.New("OAuth token store unavailable")
	}
	refreshTok := strings.TrimSpace(tr.RefreshToken)
	if refreshTok == "" {
		if prev, ok, _ := st.GetMCPOAuthToken(srv.Name); ok && prev.RefreshToken != "" {
			refreshTok = prev.RefreshToken
		}
	}
	tok := MCPOAuthToken{
		ServerName:   srv.Name,
		AccessToken:  strings.TrimSpace(tr.AccessToken),
		RefreshToken: refreshTok,
		TokenType:    cmp.Or(strings.TrimSpace(tr.TokenType), "Bearer"),
		Scope:        cmp.Or(strings.TrimSpace(tr.Scope), strings.Join(srv.OAuth.Scopes, " ")),
		ExpiresAt:    formatTokenExpiry(m.now().UTC(), tr.ExpiresIn),
		TokenURL:     pending.TokenURL,
	}
	if err := st.UpsertMCPOAuthToken(tok); err != nil {
		return MCPOAuthToken{}, err
	}
	return tok, nil
}

func formatTokenExpiry(now time.Time, expiresIn int64) string {
	if expiresIn <= 0 {
		return ""
	}
	return now.Add(time.Duration(expiresIn) * time.Second).Truncate(time.Second).Format(time.RFC3339)
}

func tokenNeedsRefresh(expiresAt string, now time.Time) bool {
	if strings.TrimSpace(expiresAt) == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(expiresAt))
	if err != nil {
		return true
	}
	return !t.After(now.Add(mcpOAuthRefreshSkew))
}

// GetAccessToken returns a valid OAuth access token for srv, refreshing automatically when needed.
func (m *MCPOAuthManager) GetAccessToken(ctx context.Context, srv MCPServerConfig, opReader OPReaderFunc, forceRefresh bool) (string, []string, error) {
	srv = NormalizeMCPServerConfig(srv)
	if srv.OAuth == nil || !srv.OAuth.Enabled {
		return "", nil, nil
	}
	if opReader == nil {
		opReader = defaultOPReader
	}
	lock := m.lockServer(srv.Name)
	lock.Lock()
	defer lock.Unlock()

	st := m.currentStore()
	var stored MCPOAuthToken
	var hasStored bool
	if st != nil {
		var err error
		stored, hasStored, err = st.GetMCPOAuthToken(srv.Name)
		if err != nil {
			return "", nil, err
		}
	}
	now := m.now().UTC()
	if !forceRefresh && hasStored && stored.AccessToken != "" && !tokenNeedsRefresh(stored.ExpiresAt, now) {
		return stored.AccessToken, []string{stored.AccessToken}, nil
	}
	refreshTok := strings.TrimSpace(stored.RefreshToken)
	if refreshTok == "" && srv.OAuth.RefreshTokenOPRef != "" {
		val, rErr := opReader(ctx, srv.OAuth.RefreshTokenOPRef)
		if rErr != nil {
			return "", nil, fmt.Errorf("resolve oauth.refresh_token_op_ref for MCP %s: %w", srv.Name, rErr)
		}
		refreshTok = strings.TrimSpace(val)
	}
	if refreshTok == "" {
		return "", nil, fmt.Errorf("MCP server %q requires OAuth authorization (run 'fleet assistant mcp-auth %s' or connect in Settings)", srv.Name, srv.Name)
	}
	tokenURL := strings.TrimSpace(stored.TokenURL)
	if tokenURL == "" {
		_, discovered, rErr := m.resolveEndpoints(ctx, srv)
		if rErr != nil {
			return "", nil, rErr
		}
		tokenURL = discovered
	}
	clientID, clientSecret, secrets, err := m.resolveClientCreds(ctx, srv, opReader)
	if err != nil {
		return "", secrets, errors.New(scrubAllSecrets(err.Error(), secrets))
	}
	secrets = append(secrets, refreshTok)
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshTok},
		"client_id":     {clientID},
	}
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	allowLoopback := m.allowLoopback || isLoopbackURL(srv.URL)
	tr, err := m.postTokenForm(ctx, tokenURL, allowLoopback, form, secrets)
	if err != nil {
		return "", secrets, err
	}
	nextRefresh := cmp.Or(strings.TrimSpace(tr.RefreshToken), refreshTok)
	nextTok := MCPOAuthToken{
		ServerName:   srv.Name,
		AccessToken:  strings.TrimSpace(tr.AccessToken),
		RefreshToken: nextRefresh,
		TokenType:    cmp.Or(strings.TrimSpace(tr.TokenType), stored.TokenType, "Bearer"),
		Scope:        cmp.Or(strings.TrimSpace(tr.Scope), stored.Scope, strings.Join(srv.OAuth.Scopes, " ")),
		ExpiresAt:    formatTokenExpiry(m.now().UTC(), tr.ExpiresIn),
		TokenURL:     tokenURL,
	}
	if st != nil {
		if uErr := st.UpsertMCPOAuthToken(nextTok); uErr != nil {
			return "", secrets, uErr
		}
	}
	secrets = append(secrets, nextTok.AccessToken)
	return nextTok.AccessToken, secrets, nil
}

// Revoke removes stored OAuth tokens and any pending authorization states for serverName.
func (m *MCPOAuthManager) Revoke(serverName string) error {
	clean := strings.ToLower(strings.TrimSpace(serverName))
	m.mu.Lock()
	for st, p := range m.pending {
		if p.ServerName == clean {
			delete(m.pending, st)
		}
	}
	delete(m.serverLocks, clean)
	st := m.store
	m.mu.Unlock()
	if st == nil {
		return nil
	}
	return st.DeleteMCPOAuthToken(clean)
}

// TokenStatus returns non-secret authorization metadata for serverName.
func (m *MCPOAuthManager) TokenStatus(serverName string) MCPOAuthStatus {
	st := m.currentStore()
	if st == nil {
		return MCPOAuthStatus{}
	}
	tok, ok, err := st.GetMCPOAuthToken(serverName)
	if err != nil || !ok {
		return MCPOAuthStatus{}
	}
	hasTok := tok.AccessToken != "" || tok.RefreshToken != ""
	return MCPOAuthStatus{
		Authorized:      hasTok,
		HasRefreshToken: tok.RefreshToken != "",
		ExpiresAt:       tok.ExpiresAt,
		Scope:           tok.Scope,
	}
}
