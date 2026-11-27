package assistant

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// ExternalTool describes one discovered tool from a configured stdio MCP server.
type ExternalTool struct {
	FullName        string
	ServerName      string
	RemoteName      string
	Description     string
	InputSchema     map[string]any
	RequireApproval bool
}

type mcpClientConn struct {
	cfg     MCPServerConfig
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	reader  *bufio.Reader
	mu      sync.Mutex
	nextID  atomic.Int64
	tools   []ExternalTool
	started bool
}

// MCPManager lazily spawns and calls configured stdio MCP servers.
type MCPManager struct {
	mu      sync.Mutex
	conns   map[string]*mcpClientConn
	configs map[string]MCPServerConfig
	reader  OPReaderFunc
}

// NewMCPManager creates an MCPManager configured from cfg.MCPServers.
func NewMCPManager(servers []MCPServerConfig, reader OPReaderFunc) *MCPManager {
	m := &MCPManager{
		conns:   make(map[string]*mcpClientConn),
		configs: make(map[string]MCPServerConfig),
		reader:  reader,
	}
	m.Reload(servers)
	return m
}

// Reload updates configured servers and closes any disabled or changed connections.
func (m *MCPManager) Reload(servers []MCPServerConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := make(map[string]MCPServerConfig, len(servers))
	for _, srv := range servers {
		if srv.Name == "" || !srv.Enabled {
			continue
		}
		next[srv.Name] = srv
	}
	for name, conn := range m.conns {
		if _, keep := next[name]; !keep {
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
		conn = &mcpClientConn{cfg: cfg}
		m.conns[name] = conn
	}
	reader := m.reader
	m.mu.Unlock()

	if err := conn.ensureStarted(ctx, reader); err != nil {
		return nil, err
	}
	return conn, nil
}

// ListTools ensures enabled MCP servers are initialized and returns their namespaced tools.
func (m *MCPManager) ListTools(ctx context.Context) []ExternalTool {
	m.mu.Lock()
	names := make([]string, 0, len(m.configs))
	for name := range m.configs {
		names = append(names, name)
	}
	m.mu.Unlock()

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
	return conn.callTool(ctx, tool.RemoteName, args)
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
	c.started = false
}

func (c *mcpClientConn) ensureStarted(ctx context.Context, opReader OPReaderFunc) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return nil
	}
	if strings.TrimSpace(c.cfg.Command) == "" {
		return errors.New("MCP command is empty")
	}
	if opReader == nil {
		opReader = defaultOPReader
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

	if err := c.handshakeLocked(ctx); err != nil {
		c.started = false
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		c.cmd = nil
		return err
	}
	return nil
}

func (c *mcpClientConn) handshakeLocked(ctx context.Context) error {
	if _, err := c.rpcCallLocked(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "fleet-assistant", "version": "1.0.0"},
	}); err != nil {
		return err
	}
	if err := c.writeFrameLocked(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}); err != nil {
		return err
	}
	rawTools, err := c.rpcCallLocked(ctx, "tools/list", map[string]any{})
	if err != nil {
		return err
	}
	var res struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
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
		c.tools = append(c.tools, ExternalTool{
			FullName:        "mcp__" + c.cfg.Name + "__" + t.Name,
			ServerName:      c.cfg.Name,
			RemoteName:      t.Name,
			Description:     cmp.Or(t.Description, "External MCP tool "+t.Name),
			InputSchema:     schema,
			RequireApproval: slices.Contains(c.cfg.RequireApprovalTools, t.Name) || slices.Contains(c.cfg.RequireApprovalTools, "*"),
		})
	}
	return nil
}

func (c *mcpClientConn) callTool(ctx context.Context, remoteName string, args map[string]any) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started {
		return nil, errors.New("MCP server not running")
	}
	raw, err := c.rpcCallLocked(ctx, "tools/call", map[string]any{
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

func (c *mcpClientConn) rpcCallLocked(_ context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	if err := c.writeFrameLocked(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}); err != nil {
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
		var resp struct {
			ID     *int64          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(trimmed), &resp); err != nil || resp.ID == nil || *resp.ID != id {
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("MCP %s: %s", method, resp.Error.Message)
		}
		return resp.Result, nil
	}
}
