package assistant

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bdatdo0601/fleet/internal/paths"
)

const (
	mcpMaxHTTPResponseBytes    = 4 * 1024 * 1024
	mcpProtocolVersion         = "2026-07-28"
	mcpLegacyProtocolVersion   = "2024-11-05"
	MCPElicitationApprovalTool = "mcp_elicitation"
	maxMCPElicitationAutoRound = 3
)

type mcpJSONRPCError struct {
	Method  string
	Code    int
	Message string
}

func (e *mcpJSONRPCError) Error() string {
	return fmt.Sprintf("MCP %s: %s", e.Method, e.Message)
}

type mcpHTTPStatusError struct {
	Status  int
	RPCCode int
	Message string
}

func (e *mcpHTTPStatusError) Error() string {
	return e.Message
}

func isMCPMethodUnsupportedError(err error) bool {
	if rpcErr, ok := errors.AsType[*mcpJSONRPCError](err); ok {
		if rpcErr.Code == -32601 {
			return true
		}
		msg := strings.ToLower(rpcErr.Message)
		return strings.Contains(msg, "method not supported") ||
			strings.Contains(msg, "method not found") ||
			strings.Contains(msg, "unknown method") ||
			strings.Contains(msg, "unsupported method") ||
			strings.Contains(msg, "not implemented")
	}
	return false
}

func isMCPProtocolVersionError(err error) bool {
	if rpcErr, ok := errors.AsType[*mcpJSONRPCError](err); ok && rpcErr.Code == -32022 {
		return true
	}
	if httpErr, ok := errors.AsType[*mcpHTTPStatusError](err); ok && httpErr.RPCCode == -32022 {
		return true
	}
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "protocol version") ||
		strings.Contains(msg, "protocolversion") ||
		strings.Contains(msg, "mcp-protocol-version") ||
		strings.Contains(msg, "unsupported version")
}

func isIgnorableInitializedNotifyError(err error) bool {
	if isMCPMethodUnsupportedError(err) {
		return true
	}
	if httpErr, ok := errors.AsType[*mcpHTTPStatusError](err); ok {
		switch httpErr.Status {
		case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
			return true
		}
	}
	return false
}

func shouldFallbackFromDiscover(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	if httpErr, ok := errors.AsType[*mcpHTTPStatusError](err); ok {
		switch httpErr.Status {
		case http.StatusBadRequest:
			return httpErr.RPCCode != -32020
		case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUnprocessableEntity, http.StatusNotImplemented:
			return true
		default:
			return false
		}
	}
	if _, ok := errors.AsType[*mcpJSONRPCError](err); ok {
		return true
	}
	if _, ok := errors.AsType[*json.SyntaxError](err); ok {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "missing matching JSON-RPC response") ||
		strings.Contains(msg, "no matching JSON-RPC response in SSE")
}

func encodeMCPHeaderValue(val string) string {
	needsBase64 := strings.HasPrefix(val, "=?base64?") && strings.HasSuffix(val, "?=")
	if !needsBase64 && val != "" && (val[0] == ' ' || val[0] == '\t' || val[len(val)-1] == ' ' || val[len(val)-1] == '\t') {
		needsBase64 = true
	}
	if !needsBase64 {
		for i := 0; i < len(val); i++ {
			b := val[i]
			if b != '\t' && (b < 0x20 || b > 0x7E) {
				needsBase64 = true
				break
			}
		}
	}
	if needsBase64 {
		return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(val)) + "?="
	}
	return val
}

func isValidHTTPToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isAlphaNum := (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		if isAlphaNum {
			continue
		}
		if !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			return false
		}
	}
	return true
}

func formatMCPHeaderParamValue(propType string, v any) (string, bool) {
	if v == nil {
		return "", false
	}
	if propType == "" || propType == "string" {
		if s, ok := v.(string); ok {
			return s, true
		}
		if propType == "string" {
			return "", false
		}
	}
	if propType == "" || propType == "boolean" {
		if b, ok := v.(bool); ok {
			return strconv.FormatBool(b), true
		}
		if propType == "boolean" {
			return "", false
		}
	}
	if propType == "" || propType == "integer" {
		return formatMCPIntegerParam(v)
	}
	return "", false
}

func formatMCPIntegerParam(v any) (string, bool) {
	const maxSafeInt = (1 << 53) - 1
	switch n := v.(type) {
	case int:
		return strconv.Itoa(n), true
	case int64:
		return strconv.FormatInt(n, 10), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return strconv.FormatInt(i, 10), true
		}
	case float64:
		if !math.IsNaN(n) && !math.IsInf(n, 0) && math.Trunc(n) == n && math.Abs(n) <= maxSafeInt {
			return strconv.FormatInt(int64(n), 10), true
		}
	}
	return "", false
}

func walkMCPHeaderParams(props, curArgs map[string]any, headers map[string]string) {
	for name, rawProp := range props {
		propMap, ok := rawProp.(map[string]any)
		if !ok {
			continue
		}
		val, exists := curArgs[name]
		if !exists || val == nil {
			continue
		}
		if nestedProps, ok := propMap["properties"].(map[string]any); ok {
			if nestedArgs, ok := val.(map[string]any); ok {
				walkMCPHeaderParams(nestedProps, nestedArgs, headers)
			}
			continue
		}
		hdrName, _ := propMap["x-mcp-header"].(string)
		if !isValidHTTPToken(hdrName) {
			continue
		}
		propType, _ := propMap["type"].(string)
		if strVal, ok := formatMCPHeaderParamValue(propType, val); ok {
			headers["Mcp-Param-"+hdrName] = encodeMCPHeaderValue(strVal)
		}
	}
}

func collectMCPHeaderParams(schema, args map[string]any) map[string]string {
	if len(schema) == 0 || len(args) == 0 {
		return nil
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		return nil
	}
	out := make(map[string]string)
	walkMCPHeaderParams(props, args, out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// MCPElicitationOption represents a single server-provided approval choice.
type MCPElicitationOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// MCPElicitationChallenge holds a stateless 2026-07-28 form elicitation prompt returned by an MCP server.
type MCPElicitationChallenge struct {
	ServerName   string                 `json:"server_name"`
	FullToolName string                 `json:"full_tool_name"`
	RemoteTool   string                 `json:"remote_tool"`
	RequestKey   string                 `json:"request_key"`
	FieldName    string                 `json:"field_name"`
	Title        string                 `json:"title,omitempty"`
	Message      string                 `json:"message,omitempty"`
	RequestState string                 `json:"request_state"`
	Options      []MCPElicitationOption `json:"options,omitempty"`
	Arguments    map[string]any         `json:"arguments,omitempty"`
}

// MCPElicitationRequiredError is returned when an MCP server responds with resultType="input_required".
type MCPElicitationRequiredError struct {
	Challenge MCPElicitationChallenge
}

func (e *MCPElicitationRequiredError) Error() string {
	return fmt.Sprintf("MCP server %s requested confirmation for %s", e.Challenge.ServerName, e.Challenge.RemoteTool)
}

func mcpClientInfo() map[string]string {
	return map[string]string{"name": "Fleet Assistant", "version": "1.0.0"}
}

func mcpClientCapabilities() map[string]any {
	return map[string]any{"elicitation": map[string]any{"form": map[string]any{}}}
}

func mcpStatelessMeta() map[string]any {
	return map[string]any{
		"io.modelcontextprotocol/protocolVersion":    mcpProtocolVersion,
		"io.modelcontextprotocol/clientInfo":         mcpClientInfo(),
		"io.modelcontextprotocol/clientCapabilities": mcpClientCapabilities(),
	}
}

// BuildMCPElicitationResponses formats params.inputResponses for a 2026-07-28 stateless retry call.
func BuildMCPElicitationResponses(requestKey, fieldName, action, choice string) map[string]any {
	rKey := cmp.Or(strings.TrimSpace(requestKey), "approval_choice")
	fName := cmp.Or(strings.TrimSpace(fieldName), rKey)
	act := strings.ToLower(strings.TrimSpace(action))
	if act == "accept" && strings.TrimSpace(choice) != "" {
		return map[string]any{
			rKey: map[string]any{
				"action":  "accept",
				"content": map[string]any{fName: strings.TrimSpace(choice)},
			},
		}
	}
	return map[string]any{
		rKey: map[string]any{
			"action": cmp.Or(act, "cancel"),
		},
	}
}

// PickAutoAllowElicitationChoice chooses the safest allow option when auto-approve is active.
func PickAutoAllowElicitationChoice(options []MCPElicitationOption) string {
	for _, opt := range options {
		if strings.EqualFold(strings.TrimSpace(opt.Value), "ALLOW_ONCE") {
			return opt.Value
		}
	}
	for _, opt := range options {
		up := strings.ToUpper(strings.TrimSpace(opt.Value))
		if up != "" && !strings.HasPrefix(up, "DENY_") {
			return opt.Value
		}
	}
	return ""
}

// ExternalTool describes one discovered tool from a configured stdio or HTTP MCP server.
type ExternalTool struct {
	FullName        string
	ServerName      string
	RemoteName      string
	Description     string
	InputSchema     map[string]any
	RequireApproval bool
	ReadOnly        bool
	Destructive     bool
}

func mcpToolRequiresApproval(cfg MCPServerConfig, remoteName string, readOnlyHint *bool) bool {
	if slices.Contains(cfg.RequireApprovalTools, remoteName) {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(cfg.ApprovalPolicy)) {
	case MCPApprovalPolicyAll:
		return true
	case MCPApprovalPolicyWritesOnly:
		return readOnlyHint == nil || !*readOnlyHint
	case MCPApprovalPolicyNone:
		return false
	default:
		return slices.Contains(cfg.RequireApprovalTools, "*")
	}
}

// MCPServerStatus summarizes one configured MCP server and its discovered tools.
type MCPServerStatus struct {
	Name            string   `json:"name"`
	Transport       string   `json:"transport"`
	Enabled         bool     `json:"enabled"`
	ToolCount       int      `json:"tool_count"`
	Tools           []string `json:"tools,omitempty"`
	Error           string   `json:"error,omitempty"`
	OAuthEnabled    bool     `json:"oauth_enabled,omitempty"`
	OAuthAuthorized bool     `json:"oauth_authorized,omitempty"`
	OAuthExpiresAt  string   `json:"oauth_expires_at,omitempty"`
	OAuthScope      string   `json:"oauth_scope,omitempty"`
}

type mcpClientConn struct {
	cfg             MCPServerConfig
	cmd             *exec.Cmd
	stdin           io.WriteCloser
	stdout          io.ReadCloser
	reader          *bufio.Reader
	httpClient      *http.Client
	oauth           *MCPOAuthManager
	sessionID       string
	protocolVersion string
	mu              sync.Mutex
	nextID          atomic.Int64
	tools           []ExternalTool
	started         bool
}

func (c *mcpClientConn) effectiveProtocolVersion() string {
	return cmp.Or(c.protocolVersion, mcpProtocolVersion)
}

func (c *mcpClientConn) useStatelessMeta() bool {
	return c.cfg.Transport == "http" && c.effectiveProtocolVersion() == mcpProtocolVersion
}

// MCPManager lazily spawns/connects and calls configured stdio or HTTP/SSE MCP servers.
type MCPManager struct {
	mu         sync.Mutex
	conns      map[string]*mcpClientConn
	configs    map[string]MCPServerConfig
	reader     OPReaderFunc
	httpClient *http.Client
	oauth      *MCPOAuthManager
}

// NewMCPManager creates an MCPManager configured from cfg.MCPServers.
func NewMCPManager(servers []MCPServerConfig, reader OPReaderFunc) *MCPManager {
	httpCli := &http.Client{Timeout: 30 * time.Second}
	m := &MCPManager{
		conns:      make(map[string]*mcpClientConn),
		configs:    make(map[string]MCPServerConfig),
		reader:     reader,
		httpClient: httpCli,
		oauth:      NewMCPOAuthManager(nil, httpCli),
	}
	m.Reload(servers)
	return m
}

// SetOAuthStore attaches persistent token storage for OAuth-enabled HTTP MCP servers.
func (m *MCPManager) SetOAuthStore(store MCPOAuthTokenStore) {
	if m.oauth != nil {
		m.oauth.SetStore(store)
	}
}

// SetOPReader updates the 1Password secret reader used when resolving MCP env_refs and header_refs.
func (m *MCPManager) SetOPReader(fn OPReaderFunc) {
	m.mu.Lock()
	m.reader = fn
	m.mu.Unlock()
}

// SetHTTPClient sets the HTTP client used for HTTP MCP calls and OAuth exchanges.
func (m *MCPManager) SetHTTPClient(cli *http.Client) {
	if cli == nil {
		return
	}
	m.mu.Lock()
	m.httpClient = cli
	for _, conn := range m.conns {
		conn.mu.Lock()
		conn.httpClient = cli
		conn.mu.Unlock()
	}
	m.mu.Unlock()
	if m.oauth != nil {
		m.oauth.SetHTTPClient(cli)
	}
}

// OAuth returns the underlying MCPOAuthManager.
func (m *MCPManager) OAuth() *MCPOAuthManager {
	return m.oauth
}

// ResetServer closes and evicts any cached connection for name so the next call re-initializes cleanly.
func (m *MCPManager) ResetServer(name string) {
	clean := strings.ToLower(strings.TrimSpace(name))
	m.mu.Lock()
	conn, ok := m.conns[clean]
	if ok {
		delete(m.conns, clean)
	}
	m.mu.Unlock()
	if ok && conn != nil {
		conn.close()
	}
}

func mcpConfigEqual(a, b MCPServerConfig) bool {
	ab, _ := json.Marshal(NormalizeMCPServerConfig(a))
	bb, _ := json.Marshal(NormalizeMCPServerConfig(b))
	return bytes.Equal(ab, bb)
}

// Reload updates configured servers and closes any disabled, removed, or modified connections.
func (m *MCPManager) Reload(servers []MCPServerConfig) {
	next := make(map[string]MCPServerConfig, len(servers))
	for _, raw := range servers {
		srv := NormalizeMCPServerConfig(raw)
		if srv.Name == "" || !srv.Enabled {
			continue
		}
		next[srv.Name] = srv
	}
	var toClose []*mcpClientConn
	m.mu.Lock()
	for name, conn := range m.conns {
		cfg, keep := next[name]
		if !keep || !mcpConfigEqual(conn.cfg, cfg) {
			toClose = append(toClose, conn)
			delete(m.conns, name)
		}
	}
	m.configs = next
	m.mu.Unlock()
	for _, conn := range toClose {
		conn.close()
	}
}

// Close terminates all running stdio MCP server subprocesses.
func (m *MCPManager) Close() {
	m.mu.Lock()
	toClose := make([]*mcpClientConn, 0, len(m.conns))
	for name, conn := range m.conns {
		toClose = append(toClose, conn)
		delete(m.conns, name)
	}
	m.mu.Unlock()
	for _, conn := range toClose {
		conn.close()
	}
}

func (m *MCPManager) ensureConn(ctx context.Context, name string) (*mcpClientConn, error) {
	m.mu.Lock()
	cfg, ok := m.configs[name]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("MCP server %q not enabled", name)
	}
	conn, exists := m.conns[name]
	if !exists {
		conn = &mcpClientConn{cfg: cfg, httpClient: m.httpClient, oauth: m.oauth}
		m.conns[name] = conn
	}
	reader := m.reader
	m.mu.Unlock()

	if err := conn.ensureStarted(ctx, reader); err != nil {
		return nil, err
	}
	return conn, nil
}

// TestServer performs an ephemeral initialize + tools/list handshake against srv without modifying active state.
func (m *MCPManager) TestServer(ctx context.Context, srv MCPServerConfig) ([]ExternalTool, error) {
	norm := NormalizeMCPServerConfig(srv)
	if err := ValidateMCPServerConfig(norm, false); err != nil {
		return nil, err
	}
	m.mu.Lock()
	reader := m.reader
	httpCli := m.httpClient
	oauthMgr := m.oauth
	m.mu.Unlock()
	conn := &mcpClientConn{cfg: norm, httpClient: httpCli, oauth: oauthMgr}
	defer conn.close()
	if err := conn.ensureStarted(ctx, reader); err != nil {
		return nil, err
	}
	return conn.snapshotTools(), nil
}

// ListServerStatuses inspects each configured server and returns status + discovered tool names.
func (m *MCPManager) ListServerStatuses(ctx context.Context, servers []MCPServerConfig) []MCPServerStatus {
	out := make([]MCPServerStatus, 0, len(servers))
	for _, raw := range servers {
		srv := NormalizeMCPServerConfig(raw)
		st := MCPServerStatus{Name: srv.Name, Transport: srv.Transport, Enabled: srv.Enabled}
		if srv.OAuth != nil && srv.OAuth.Enabled {
			st.OAuthEnabled = true
			if m.oauth != nil {
				ost := m.oauth.TokenStatus(srv.Name)
				st.OAuthAuthorized = ost.Authorized
				st.OAuthExpiresAt = ost.ExpiresAt
				st.OAuthScope = ost.Scope
			}
		}
		if !srv.Enabled {
			out = append(out, st)
			continue
		}
		if st.OAuthEnabled && !st.OAuthAuthorized && srv.OAuth.RefreshTokenOPRef == "" {
			st.Error = fmt.Sprintf("OAuth authorization required: run 'fleet assistant mcp-auth %s' or connect in Settings", srv.Name)
			out = append(out, st)
			continue
		}
		conn, err := m.ensureConn(ctx, srv.Name)
		if err != nil {
			st.Error = err.Error()
			out = append(out, st)
			continue
		}
		tools := conn.snapshotTools()
		st.ToolCount = len(tools)
		for _, t := range tools {
			st.Tools = append(st.Tools, t.RemoteName)
		}
		out = append(out, st)
	}
	return out
}

// ListTools ensures enabled MCP servers are initialized and returns their namespaced tools.
func (m *MCPManager) ListTools(ctx context.Context) []ExternalTool {
	m.mu.Lock()
	names := make([]string, 0, len(m.configs))
	for name := range m.configs {
		names = append(names, name)
	}
	m.mu.Unlock()
	slices.Sort(names)

	var out []ExternalTool
	for _, name := range names {
		conn, err := m.ensureConn(ctx, name)
		if err != nil {
			continue
		}
		out = append(out, conn.snapshotTools()...)
	}
	return out
}

// FindTool looks up a namespaced external tool by FullName.
func (m *MCPManager) FindTool(ctx context.Context, fullName string) (ExternalTool, bool) {
	for _, t := range m.ListTools(ctx) {
		if t.FullName == fullName {
			return t, true
		}
	}
	return ExternalTool{}, false
}

// CallTool invokes a namespaced external MCP tool (mcp__<server>__<tool>).
func (m *MCPManager) CallTool(ctx context.Context, fullName string, args map[string]any) (any, error) {
	return m.CallToolWithElicitation(ctx, fullName, args, "", nil)
}

// CallToolWithElicitation invokes an external MCP tool, optionally supplying 2026-07-28 elicitation retry fields.
func (m *MCPManager) CallToolWithElicitation(ctx context.Context, fullName string, args map[string]any, requestState string, inputResponses map[string]any) (any, error) {
	tool, ok := m.FindTool(ctx, fullName)
	if !ok {
		return nil, fmt.Errorf("external MCP tool %q not found", fullName)
	}
	conn, err := m.ensureConn(ctx, tool.ServerName)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	reader := m.reader
	m.mu.Unlock()
	return conn.callToolWithElicitation(ctx, reader, tool.RemoteName, args, requestState, inputResponses)
}

func (c *mcpClientConn) snapshotTools() []ExternalTool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ExternalTool(nil), c.tools...)
}

func (c *mcpClientConn) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetStdioLocked()
}

func (c *mcpClientConn) terminateStdioHandlesLocked() {
	if c.stdin != nil {
		_ = c.stdin.Close()
		c.stdin = nil
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	if c.stdout != nil {
		_ = c.stdout.Close()
		c.stdout = nil
	}
}

func (c *mcpClientConn) finishStdioWaitLocked() {
	if c.cmd != nil {
		if c.cmd.Process != nil {
			_ = c.cmd.Wait()
		}
		c.cmd = nil
	}
	c.reader = nil
	c.sessionID = ""
	c.protocolVersion = ""
	c.started = false
}

func (c *mcpClientConn) resetStdioLocked() {
	c.terminateStdioHandlesLocked()
	c.finishStdioWaitLocked()
}

func (c *mcpClientConn) ensureStarted(ctx context.Context, opReader OPReaderFunc) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return nil
	}
	if opReader == nil {
		opReader = defaultOPReader
	}
	if c.cfg.Transport == "http" {
		if strings.TrimSpace(c.cfg.URL) == "" {
			return errors.New("MCP http url is empty")
		}
		if c.httpClient == nil {
			c.httpClient = &http.Client{Timeout: 30 * time.Second}
		}
		c.started = true
		if err := c.handshakeLocked(ctx, opReader); err != nil {
			c.started = false
			c.sessionID = ""
			c.protocolVersion = ""
			return err
		}
		return nil
	}
	if err := c.startStdioLocked(ctx, opReader); err != nil {
		return err
	}
	if err := c.handshakeLocked(ctx, opReader); err != nil {
		c.resetStdioLocked()
		return err
	}
	return nil
}

func mcpHostPathEnv(basePath, home string) string {
	var extra []string
	if home != "" {
		extra = append(extra,
			filepath.Join(home, ".local", "bin"),
			"/opt/homebrew/bin",
			"/usr/local/bin",
			filepath.Join(home, ".volta", "bin"),
			filepath.Join(home, ".local", "share", "mise", "shims"),
			filepath.Join(home, ".asdf", "shims"),
			filepath.Join(home, "Library", "pnpm"),
			filepath.Join(home, ".npm-global", "bin"),
		)
		for _, glob := range []string{
			filepath.Join(home, ".nvm", "versions", "node", "*", "bin"),
			filepath.Join(home, ".local", "share", "fnm", "node-versions", "*", "installation", "bin"),
		} {
			if matches, err := filepath.Glob(glob); err == nil && len(matches) > 0 {
				sort.Strings(matches)
				extra = append(extra, matches[len(matches)-1])
			}
		}
	}
	return paths.MergePathEnv(basePath, strings.Join(extra, string(os.PathListSeparator)), paths.StandardPathEnv)
}

func resolveMCPCommand(command, pathEnv string) string {
	if strings.ContainsRune(command, os.PathSeparator) {
		return command
	}
	for dir := range strings.SplitSeq(pathEnv, string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		cand := filepath.Join(dir, command)
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return cand
		}
	}
	return command
}

func setEnvVar(env []string, key, value string) []string {
	prefix := key + "="
	for i, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func (c *mcpClientConn) startStdioLocked(ctx context.Context, opReader OPReaderFunc) error {
	if strings.TrimSpace(c.cfg.Command) == "" {
		return errors.New("MCP command is empty")
	}
	home, _ := os.UserHomeDir()
	effectivePath := mcpHostPathEnv(os.Getenv("PATH"), home)
	if customPath := strings.TrimSpace(c.cfg.Env["PATH"]); customPath != "" {
		effectivePath = paths.MergePathEnv(customPath, effectivePath)
	}
	resolvedCmd := resolveMCPCommand(c.cfg.Command, effectivePath)
	cmd := exec.Command(resolvedCmd, c.cfg.Args...)
	env := setEnvVar(os.Environ(), "PATH", effectivePath)
	for k, v := range c.cfg.Env {
		if k == "PATH" {
			continue
		}
		env = setEnvVar(env, k, v)
	}
	for k, ref := range c.cfg.EnvRefs {
		val, err := opReader(ctx, ref)
		if err != nil {
			return fmt.Errorf("resolve %s for MCP %s: %w", k, c.cfg.Name, err)
		}
		env = setEnvVar(env, k, val)
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	rPipe, wPipe, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return err
	}
	cmd.Stdout = wPipe
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = rPipe.Close()
		_ = wPipe.Close()
		return err
	}
	_ = wPipe.Close()
	c.cmd = cmd
	c.stdin = stdin
	c.stdout = rPipe
	c.reader = bufio.NewReader(rPipe)
	c.started = true
	return nil
}

func (c *mcpClientConn) sendInitializedNotifyLocked(ctx context.Context, opReader OPReaderFunc) error {
	if nErr := c.notifyLocked(ctx, opReader, "notifications/initialized"); nErr != nil && !isIgnorableInitializedNotifyError(nErr) {
		return nErr
	}
	return nil
}

func (c *mcpClientConn) handshakeHTTPLocked(ctx context.Context, opReader OPReaderFunc) error {
	c.protocolVersion = mcpProtocolVersion
	_, discoverErr := c.rpcCallLocked(ctx, opReader, "server/discover", map[string]any{
		"_meta": mcpStatelessMeta(),
	})
	if discoverErr == nil {
		return nil
	}
	if !shouldFallbackFromDiscover(ctx, discoverErr) {
		return discoverErr
	}
	if !isMCPProtocolVersionError(discoverErr) {
		_, initErr := c.rpcCallLocked(ctx, opReader, "initialize", map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    mcpClientCapabilities(),
			"clientInfo":      mcpClientInfo(),
		})
		if initErr == nil {
			return c.sendInitializedNotifyLocked(ctx, opReader)
		}
		if isMCPMethodUnsupportedError(initErr) {
			return nil
		}
		if !isMCPProtocolVersionError(initErr) {
			return initErr
		}
	}
	c.protocolVersion = mcpLegacyProtocolVersion
	if _, legErr := c.rpcCallLocked(ctx, opReader, "initialize", map[string]any{
		"protocolVersion": mcpLegacyProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      mcpClientInfo(),
	}); legErr != nil {
		return legErr
	}
	return c.sendInitializedNotifyLocked(ctx, opReader)
}

func (c *mcpClientConn) handshakeLocked(ctx context.Context, opReader OPReaderFunc) error {
	if c.cfg.Transport == "http" {
		if err := c.handshakeHTTPLocked(ctx, opReader); err != nil {
			return err
		}
	} else {
		if _, err := c.rpcCallLocked(ctx, opReader, "initialize", map[string]any{
			"protocolVersion": mcpLegacyProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      mcpClientInfo(),
		}); err != nil {
			return err
		}
		if err := c.notifyLocked(ctx, opReader, "notifications/initialized"); err != nil {
			return err
		}
	}
	c.tools = nil
	cursor := ""
	for range 20 {
		params := map[string]any{}
		if c.useStatelessMeta() {
			params["_meta"] = mcpStatelessMeta()
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		rawTools, err := c.rpcCallLocked(ctx, opReader, "tools/list", params)
		if err != nil {
			return err
		}
		var res struct {
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"inputSchema"`
				Annotations *struct {
					ReadOnlyHint    *bool `json:"readOnlyHint"`
					DestructiveHint *bool `json:"destructiveHint"`
				} `json:"annotations"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(rawTools, &res); err != nil {
			return err
		}
		for _, t := range res.Tools {
			schema := t.InputSchema
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			var readOnlyHint *bool
			var readOnly, destructive bool
			if t.Annotations != nil {
				readOnlyHint = t.Annotations.ReadOnlyHint
				if t.Annotations.ReadOnlyHint != nil {
					readOnly = *t.Annotations.ReadOnlyHint
				}
				if t.Annotations.DestructiveHint != nil {
					destructive = *t.Annotations.DestructiveHint
				}
			}
			c.tools = append(c.tools, ExternalTool{
				FullName:        "mcp__" + c.cfg.Name + "__" + t.Name,
				ServerName:      c.cfg.Name,
				RemoteName:      t.Name,
				Description:     cmp.Or(t.Description, "External MCP tool "+t.Name),
				InputSchema:     schema,
				RequireApproval: mcpToolRequiresApproval(c.cfg, t.Name, readOnlyHint),
				ReadOnly:        readOnly,
				Destructive:     destructive,
			})
		}
		if strings.TrimSpace(res.NextCursor) == "" {
			break
		}
		cursor = strings.TrimSpace(res.NextCursor)
	}
	return nil
}

func (c *mcpClientConn) callTool(ctx context.Context, opReader OPReaderFunc, remoteName string, args map[string]any) (any, error) {
	return c.callToolWithElicitation(ctx, opReader, remoteName, args, "", nil)
}

func (c *mcpClientConn) findToolSchemaLocked(remoteName string) map[string]any {
	for _, t := range c.tools {
		if t.RemoteName == remoteName {
			return t.InputSchema
		}
	}
	return nil
}

func (c *mcpClientConn) callToolWithElicitation(ctx context.Context, opReader OPReaderFunc, remoteName string, args map[string]any, requestState string, inputResponses map[string]any) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started {
		return nil, errors.New("MCP server not running")
	}
	if opReader == nil {
		opReader = defaultOPReader
	}
	params := map[string]any{
		"name":      remoteName,
		"arguments": cmp.Or[any](args, map[string]any{}),
	}
	var extraHeaders map[string]string
	if c.useStatelessMeta() {
		params["_meta"] = mcpStatelessMeta()
		extraHeaders = map[string]string{"Mcp-Name": encodeMCPHeaderValue(remoteName)}
		maps.Copy(extraHeaders, collectMCPHeaderParams(c.findToolSchemaLocked(remoteName), args))
	}
	if strings.TrimSpace(requestState) != "" {
		params["requestState"] = requestState
	}
	if len(inputResponses) > 0 {
		params["inputResponses"] = inputResponses
	}
	raw, err := c.rpcCallWithHeadersLocked(ctx, opReader, "tools/call", params, extraHeaders)
	if err != nil {
		return nil, err
	}
	if ch, ok := parseMCPElicitationChallenge(raw, c.cfg.Name, "mcp__"+c.cfg.Name+"__"+remoteName, remoteName, args); ok {
		return nil, &MCPElicitationRequiredError{Challenge: ch}
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return string(raw), nil
	}
	return out, nil
}

func parseMCPElicitationChallenge(raw json.RawMessage, serverName, fullTool, remoteTool string, args map[string]any) (MCPElicitationChallenge, bool) {
	var env struct {
		ResultType    string                     `json:"resultType"`
		RequestState  string                     `json:"requestState"`
		InputRequests map[string]json.RawMessage `json:"inputRequests"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.ResultType != "input_required" || strings.TrimSpace(env.RequestState) == "" || len(env.InputRequests) == 0 {
		return MCPElicitationChallenge{}, false
	}
	reqKey := selectInputRequestKey(env.InputRequests)
	if reqKey == "" {
		return MCPElicitationChallenge{}, false
	}
	var reqObj struct {
		Method string `json:"method"`
		Params struct {
			Mode            string `json:"mode"`
			Message         string `json:"message"`
			RequestedSchema struct {
				Properties map[string]struct {
					Title     string   `json:"title"`
					Enum      []string `json:"enum"`
					EnumNames []string `json:"enumNames"`
					OneOf     []struct {
						Const string `json:"const"`
						Title string `json:"title"`
					} `json:"oneOf"`
				} `json:"properties"`
			} `json:"requestedSchema"`
		} `json:"params"`
	}
	if err := json.Unmarshal(env.InputRequests[reqKey], &reqObj); err != nil {
		return MCPElicitationChallenge{}, false
	}
	if reqObj.Method != "elicitation/create" || (reqObj.Params.Mode != "" && reqObj.Params.Mode != "form") {
		return MCPElicitationChallenge{}, false
	}
	fieldName := selectSchemaFieldName(reqKey, reqObj.Params.RequestedSchema.Properties)
	prop := reqObj.Params.RequestedSchema.Properties[fieldName]
	options := make([]MCPElicitationOption, 0, len(prop.Enum)+len(prop.OneOf))
	if len(prop.Enum) > 0 {
		for i, val := range prop.Enum {
			lbl := val
			if i < len(prop.EnumNames) && strings.TrimSpace(prop.EnumNames[i]) != "" {
				lbl = prop.EnumNames[i]
			}
			options = append(options, MCPElicitationOption{Value: val, Label: lbl})
		}
	} else {
		for _, item := range prop.OneOf {
			options = append(options, MCPElicitationOption{Value: item.Const, Label: cmp.Or(item.Title, item.Const)})
		}
	}
	return MCPElicitationChallenge{
		ServerName:   serverName,
		FullToolName: fullTool,
		RemoteTool:   remoteTool,
		RequestKey:   reqKey,
		FieldName:    cmp.Or(fieldName, reqKey),
		Title:        cmp.Or(strings.TrimSpace(prop.Title), "Confirm "+fullTool),
		Message:      strings.TrimSpace(reqObj.Params.Message),
		RequestState: env.RequestState,
		Options:      options,
		Arguments:    maps.Clone(args),
	}, true
}

func selectInputRequestKey(reqs map[string]json.RawMessage) string {
	if _, ok := reqs["approval_choice"]; ok {
		return "approval_choice"
	}
	keys := make([]string, 0, len(reqs))
	for k := range reqs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

func selectSchemaFieldName[V any](preferred string, props map[string]V) string {
	if _, ok := props[preferred]; ok {
		return preferred
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

func (c *mcpClientConn) writeFrameLocked(payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = c.stdin.Write(b)
	return err
}

func (c *mcpClientConn) notifyLocked(ctx context.Context, opReader OPReaderFunc, method string) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if c.cfg.Transport == "http" {
		_, err := c.httpPostLocked(ctx, opReader, msg, 0, nil)
		return err
	}
	return c.writeFrameLocked(msg)
}

type stdioReadResult struct {
	line []byte
	err  error
}

func (c *mcpClientConn) readStdioLineLocked(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		c.resetStdioLocked()
		return nil, err
	}
	reader := c.reader
	if reader == nil {
		c.resetStdioLocked()
		return nil, errors.New("MCP stdio reader closed")
	}
	ch := make(chan stdioReadResult, 1)
	go func() {
		line, err := reader.ReadBytes('\n')
		ch <- stdioReadResult{line: line, err: err}
	}()
	select {
	case <-ctx.Done():
		c.terminateStdioHandlesLocked()
		<-ch
		c.finishStdioWaitLocked()
		return nil, ctx.Err()
	case res := <-ch:
		if res.err != nil {
			c.resetStdioLocked()
			return nil, res.err
		}
		return res.line, nil
	}
}

func (c *mcpClientConn) rpcCallLocked(ctx context.Context, opReader OPReaderFunc, method string, params any) (json.RawMessage, error) {
	return c.rpcCallWithHeadersLocked(ctx, opReader, method, params, nil)
}

func (c *mcpClientConn) rpcCallWithHeadersLocked(ctx context.Context, opReader OPReaderFunc, method string, params any, extraHeaders map[string]string) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	reqBody := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	if c.cfg.Transport == "http" {
		return c.httpCallLocked(ctx, opReader, method, reqBody, id, extraHeaders)
	}
	if err := c.writeFrameLocked(reqBody); err != nil {
		c.resetStdioLocked()
		return nil, err
	}
	for {
		line, err := c.readStdioLineLocked(ctx)
		if err != nil {
			return nil, err
		}
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" {
			continue
		}
		res, matched, rErr := parseJSONRPCResponse([]byte(trimmed), id, method)
		if matched {
			if rErr != nil {
				return nil, rErr
			}
			return res, nil
		}
	}
}

func (c *mcpClientConn) resolveHTTPHeadersLocked(ctx context.Context, opReader OPReaderFunc, forceRefresh bool) (http.Header, []string, error) {
	hdr := make(http.Header)
	var secrets []string
	for k, v := range c.cfg.Headers {
		hdr.Set(k, v)
	}
	for k, ref := range c.cfg.HeaderRefs {
		val, err := opReader(ctx, ref)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve header %s for MCP %s: %w", k, c.cfg.Name, err)
		}
		hdr.Set(k, val)
		secrets = append(secrets, val)
	}
	if c.cfg.OAuth != nil && c.cfg.OAuth.Enabled && c.oauth != nil {
		tok, oauthSecrets, err := c.oauth.GetAccessToken(ctx, c.cfg, opReader, forceRefresh)
		secrets = append(secrets, oauthSecrets...)
		if err != nil {
			return nil, secrets, errors.New(scrubAllSecrets(err.Error(), secrets))
		}
		if tok != "" {
			hdr.Set("Authorization", "Bearer "+tok)
			secrets = append(secrets, tok)
		}
	}
	return hdr, secrets, nil
}

func scrubAllSecrets(text string, secrets []string) string {
	out := text
	for _, s := range secrets {
		out = scrubSecret(out, s)
		if _, tok, ok := strings.Cut(strings.TrimSpace(s), " "); ok && len(strings.TrimSpace(tok)) >= 8 {
			out = scrubSecret(out, strings.TrimSpace(tok))
		}
	}
	return out
}

func (c *mcpClientConn) doHTTPPostOnceLocked(ctx context.Context, opReader OPReaderFunc, raw []byte, extraHeaders map[string]string, forceRefresh bool) (int, http.Header, []byte, []string, error) {
	hdrs, secrets, err := c.resolveHTTPHeadersLocked(ctx, opReader, forceRefresh)
	if err != nil {
		return 0, nil, nil, secrets, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, nil, secrets, errors.New(scrubAllSecrets(err.Error(), secrets))
	}
	for k, vals := range hdrs {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", c.effectiveProtocolVersion())
	if c.useStatelessMeta() {
		for k, v := range extraHeaders {
			req.Header.Set(k, v)
		}
	}
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, nil, secrets, errors.New(scrubAllSecrets(err.Error(), secrets))
	}
	defer func() { _ = resp.Body.Close() }()
	if sid := strings.TrimSpace(resp.Header.Get("Mcp-Session-Id")); sid != "" {
		c.sessionID = sid
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, mcpMaxHTTPResponseBytes+1))
	if err != nil {
		return 0, nil, nil, secrets, errors.New(scrubAllSecrets(err.Error(), secrets))
	}
	return resp.StatusCode, resp.Header.Clone(), body, secrets, nil
}

func (c *mcpClientConn) httpPostLocked(ctx context.Context, opReader OPReaderFunc, payload any, wantID int64, extraHeaders map[string]string) (json.RawMessage, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	status, respHdr, body, secrets, err := c.doHTTPPostOnceLocked(ctx, opReader, raw, extraHeaders, false)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized && c.cfg.OAuth != nil && c.cfg.OAuth.Enabled && c.oauth != nil {
		c.sessionID = ""
		status, respHdr, body, secrets, err = c.doHTTPPostOnceLocked(ctx, opReader, raw, extraHeaders, true)
		if err != nil {
			return nil, err
		}
	}
	if len(body) > mcpMaxHTTPResponseBytes {
		return nil, fmt.Errorf("MCP HTTP response exceeded %d byte limit", mcpMaxHTTPResponseBytes)
	}
	if status >= 400 {
		var rpcErrEnv struct {
			Error *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &rpcErrEnv)
		rpcCode := 0
		if rpcErrEnv.Error != nil {
			rpcCode = rpcErrEnv.Error.Code
		}
		return nil, &mcpHTTPStatusError{
			Status:  status,
			RPCCode: rpcCode,
			Message: scrubAllSecrets(fmt.Sprintf("MCP HTTP %d: %s", status, strings.TrimSpace(string(body))), secrets),
		}
	}
	if wantID == 0 {
		return nil, nil
	}
	ct := strings.ToLower(respHdr.Get("Content-Type"))
	if strings.Contains(ct, "text/event-stream") {
		res, sErr := extractSSEJSONRPC(body, wantID)
		if sErr != nil {
			return nil, errors.New(scrubAllSecrets(sErr.Error(), secrets))
		}
		return res, nil
	}
	return body, nil
}

func (c *mcpClientConn) httpCallLocked(ctx context.Context, opReader OPReaderFunc, method string, reqBody any, id int64, extraHeaders map[string]string) (json.RawMessage, error) {
	var hdrs map[string]string
	if c.useStatelessMeta() {
		hdrs = make(map[string]string, len(extraHeaders)+1)
		maps.Copy(hdrs, extraHeaders)
		hdrs["Mcp-Method"] = method
	}
	body, err := c.httpPostLocked(ctx, opReader, reqBody, id, hdrs)
	if err != nil {
		return nil, err
	}
	res, matched, rErr := parseJSONRPCResponse(body, id, method)
	if rErr != nil {
		return nil, rErr
	}
	if !matched {
		return nil, fmt.Errorf("MCP %s: missing matching JSON-RPC response id %d", method, id)
	}
	return res, nil
}

func extractSSEJSONRPC(body []byte, wantID int64) ([]byte, error) {
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), max(2*1024*1024, mcpMaxHTTPResponseBytes))
	var dataLines []string
	checkBlock := func() ([]byte, bool) {
		if len(dataLines) == 0 {
			return nil, false
		}
		joined := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = nil
		if joined == "" || joined == "[DONE]" {
			return nil, false
		}
		var probe struct {
			ID *int64 `json:"id"`
		}
		if json.Unmarshal([]byte(joined), &probe) == nil && probe.ID != nil && *probe.ID == wantID {
			return []byte(joined), true
		}
		return nil, false
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if matched, ok := checkBlock(); ok {
				return matched, nil
			}
			continue
		}
		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			dataLines = append(dataLines, strings.TrimSpace(rest))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read MCP SSE stream: %w", err)
	}
	if matched, ok := checkBlock(); ok {
		return matched, nil
	}
	return nil, fmt.Errorf("no matching JSON-RPC response in SSE stream")
}

func parseJSONRPCResponse(raw []byte, id int64, method string) (json.RawMessage, bool, error) {
	var resp struct {
		ID     *int64          `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, false, err
	}
	if resp.ID == nil || *resp.ID != id {
		return nil, false, nil
	}
	if resp.Error != nil {
		return nil, true, &mcpJSONRPCError{
			Method:  method,
			Code:    resp.Error.Code,
			Message: resp.Error.Message,
		}
	}
	return resp.Result, true, nil
}
