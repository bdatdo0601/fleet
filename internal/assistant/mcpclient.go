package assistant

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const mcpMaxHTTPResponseBytes = 4 * 1024 * 1024

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
	Name      string   `json:"name"`
	Transport string   `json:"transport"`
	Enabled   bool     `json:"enabled"`
	ToolCount int      `json:"tool_count"`
	Tools     []string `json:"tools,omitempty"`
	Error     string   `json:"error,omitempty"`
}

type mcpClientConn struct {
	cfg        MCPServerConfig
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	reader     *bufio.Reader
	httpClient *http.Client
	sessionID  string
	mu         sync.Mutex
	nextID     atomic.Int64
	tools      []ExternalTool
	started    bool
}

// MCPManager lazily spawns/connects and calls configured stdio or HTTP/SSE MCP servers.
type MCPManager struct {
	mu         sync.Mutex
	conns      map[string]*mcpClientConn
	configs    map[string]MCPServerConfig
	reader     OPReaderFunc
	httpClient *http.Client
}

// NewMCPManager creates an MCPManager configured from cfg.MCPServers.
func NewMCPManager(servers []MCPServerConfig, reader OPReaderFunc) *MCPManager {
	m := &MCPManager{
		conns:      make(map[string]*mcpClientConn),
		configs:    make(map[string]MCPServerConfig),
		reader:     reader,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	m.Reload(servers)
	return m
}

func mcpConfigEqual(a, b MCPServerConfig) bool {
	ab, _ := json.Marshal(NormalizeMCPServerConfig(a))
	bb, _ := json.Marshal(NormalizeMCPServerConfig(b))
	return bytes.Equal(ab, bb)
}

// Reload updates configured servers and closes any disabled, removed, or modified connections.
func (m *MCPManager) Reload(servers []MCPServerConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := make(map[string]MCPServerConfig, len(servers))
	for _, raw := range servers {
		srv := NormalizeMCPServerConfig(raw)
		if srv.Name == "" || !srv.Enabled {
			continue
		}
		next[srv.Name] = srv
	}
	for name, conn := range m.conns {
		cfg, keep := next[name]
		if !keep || !mcpConfigEqual(conn.cfg, cfg) {
			conn.close()
			delete(m.conns, name)
		}
	}
	m.configs = next
}

// Close terminates all running stdio MCP server subprocesses.
func (m *MCPManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, conn := range m.conns {
		conn.close()
		delete(m.conns, name)
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
		conn = &mcpClientConn{cfg: cfg, httpClient: m.httpClient}
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
	m.mu.Unlock()
	conn := &mcpClientConn{cfg: norm, httpClient: httpCli}
	defer conn.close()
	if err := conn.ensureStarted(ctx, reader); err != nil {
		return nil, err
	}
	return append([]ExternalTool(nil), conn.tools...), nil
}

// ListServerStatuses inspects each configured server and returns status + discovered tool names.
func (m *MCPManager) ListServerStatuses(ctx context.Context, servers []MCPServerConfig) []MCPServerStatus {
	out := make([]MCPServerStatus, 0, len(servers))
	for _, raw := range servers {
		srv := NormalizeMCPServerConfig(raw)
		st := MCPServerStatus{Name: srv.Name, Transport: srv.Transport, Enabled: srv.Enabled}
		if !srv.Enabled {
			out = append(out, st)
			continue
		}
		conn, err := m.ensureConn(ctx, srv.Name)
		if err != nil {
			st.Error = err.Error()
			out = append(out, st)
			continue
		}
		st.ToolCount = len(conn.tools)
		for _, t := range conn.tools {
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
		out = append(out, conn.tools...)
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
	return conn.callTool(ctx, reader, tool.RemoteName, args)
}

func (c *mcpClientConn) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stdin != nil {
		_ = c.stdin.Close()
		c.stdin = nil
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
		c.cmd = nil
	}
	c.sessionID = ""
	c.started = false
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
			return err
		}
		return nil
	}
	if err := c.startStdioLocked(ctx, opReader); err != nil {
		return err
	}
	if err := c.handshakeLocked(ctx, opReader); err != nil {
		c.started = false
		_ = c.stdin.Close()
		c.stdin = nil
		if c.cmd != nil && c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
			_ = c.cmd.Wait()
		}
		c.cmd = nil
		return err
	}
	return nil
}

func (c *mcpClientConn) startStdioLocked(ctx context.Context, opReader OPReaderFunc) error {
	if strings.TrimSpace(c.cfg.Command) == "" {
		return errors.New("MCP command is empty")
	}
	cmd := exec.Command(c.cfg.Command, c.cfg.Args...)
	env := os.Environ()
	for k, v := range c.cfg.Env {
		env = append(env, k+"="+v)
	}
	for k, ref := range c.cfg.EnvRefs {
		val, err := opReader(ctx, ref)
		if err != nil {
			return fmt.Errorf("resolve %s for MCP %s: %w", k, c.cfg.Name, err)
		}
		env = append(env, k+"="+val)
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return err
	}
	c.cmd = cmd
	c.stdin = stdin
	c.reader = bufio.NewReader(stdout)
	c.started = true
	return nil
}

func (c *mcpClientConn) handshakeLocked(ctx context.Context, opReader OPReaderFunc) error {
	if _, err := c.rpcCallLocked(ctx, opReader, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "fleet-assistant", "version": "1.0.0"},
	}); err != nil {
		return err
	}
	if err := c.notifyLocked(ctx, opReader, "notifications/initialized"); err != nil {
		return err
	}
	rawTools, err := c.rpcCallLocked(ctx, opReader, "tools/list", map[string]any{})
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
	}
	if err := json.Unmarshal(rawTools, &res); err != nil {
		return err
	}
	c.tools = make([]ExternalTool, 0, len(res.Tools))
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
	return nil
}

func (c *mcpClientConn) callTool(ctx context.Context, opReader OPReaderFunc, remoteName string, args map[string]any) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started {
		return nil, errors.New("MCP server not running")
	}
	if opReader == nil {
		opReader = defaultOPReader
	}
	raw, err := c.rpcCallLocked(ctx, opReader, "tools/call", map[string]any{
		"name":      remoteName,
		"arguments": cmp.Or[any](args, map[string]any{}),
	})
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return string(raw), nil
	}
	return out, nil
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
		_, err := c.httpPostLocked(ctx, opReader, msg, 0)
		return err
	}
	return c.writeFrameLocked(msg)
}

func (c *mcpClientConn) rpcCallLocked(ctx context.Context, opReader OPReaderFunc, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	reqBody := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	if c.cfg.Transport == "http" {
		return c.httpCallLocked(ctx, opReader, method, reqBody, id)
	}
	if err := c.writeFrameLocked(reqBody); err != nil {
		return nil, err
	}
	for {
		line, err := c.reader.ReadBytes('\n')
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

func (c *mcpClientConn) resolveHTTPHeadersLocked(ctx context.Context, opReader OPReaderFunc) (http.Header, []string, error) {
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

func (c *mcpClientConn) httpPostLocked(ctx context.Context, opReader OPReaderFunc, payload any, wantID int64) (json.RawMessage, error) {
	hdrs, secrets, err := c.resolveHTTPHeadersLocked(ctx, opReader)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New(scrubAllSecrets(err.Error(), secrets))
	}
	for k, vals := range hdrs {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, errors.New(scrubAllSecrets(err.Error(), secrets))
	}
	defer func() { _ = resp.Body.Close() }()
	if sid := strings.TrimSpace(resp.Header.Get("Mcp-Session-Id")); sid != "" {
		c.sessionID = sid
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, mcpMaxHTTPResponseBytes+1))
	if err != nil {
		return nil, errors.New(scrubAllSecrets(err.Error(), secrets))
	}
	if len(body) > mcpMaxHTTPResponseBytes {
		return nil, fmt.Errorf("MCP HTTP response exceeded %d byte limit", mcpMaxHTTPResponseBytes)
	}
	if resp.StatusCode >= 400 {
		return nil, errors.New(scrubAllSecrets(fmt.Sprintf("MCP HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body))), secrets))
	}
	if wantID == 0 {
		return nil, nil
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "text/event-stream") {
		res, sErr := extractSSEJSONRPC(body, wantID)
		if sErr != nil {
			return nil, errors.New(scrubAllSecrets(sErr.Error(), secrets))
		}
		return res, nil
	}
	return body, nil
}

func (c *mcpClientConn) httpCallLocked(ctx context.Context, opReader OPReaderFunc, method string, reqBody any, id int64) (json.RawMessage, error) {
	body, err := c.httpPostLocked(ctx, opReader, reqBody, id)
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
	sc.Buffer(make([]byte, 0, 64*1024), mcpMaxHTTPResponseBytes)
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
		return nil, true, fmt.Errorf("MCP %s: %s", method, resp.Error.Message)
	}
	return resp.Result, true, nil
}
