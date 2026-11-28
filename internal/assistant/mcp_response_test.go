package assistant

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestApprovalResumeDeliversFullUnwrappedMCPAndKeepsCompactHistory(t *testing.T) {
	items := make([]map[string]any, 0, 15)
	for i := 1; i <= 15; i++ {
		items = append(items, map[string]any{
			"index":       i,
			"title":       fmt.Sprintf("AP Story #%02d — Full headline preserved", i),
			"url":         fmt.Sprintf("https://apnews.com/article/story-%02d", i),
			"description": strings.Repeat("Summary detail ", 20),
		})
	}
	innerBytes, _ := json.Marshal(map[string]any{"items": items})
	innerJSON := string(innerBytes) // ~6 KB wrapped inside MCP content[0].text

	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2024-11-05"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"tools": []map[string]any{{
						"name":        "brave_news_search",
						"description": "Search news",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}},
					}},
				},
			})
		case "tools/call":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": innerJSON},
					},
				},
			})
		}
	}))
	defer mcpSrv.Close()

	var mu sync.Mutex
	var resumedMessages []ChatMessage
	var step atomic.Int32

	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/alpha/decisions" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"answers":{"needs_frontier":{"type":"noul","noul":0.1}}}`))
			return
		}
		var req struct {
			Messages []ChatMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		n := step.Add(1)
		if n == 1 {
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"tc_news\",\"type\":\"function\",\"function\":{\"name\":\"mcp__brave__brave_news_search\",\"arguments\":\"{\\\"query\\\":\\\"AP News\\\"}\"}}]}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"choices\":[{\"finish_reason\":\"tool_calls\",\"delta\":{}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		mu.Lock()
		resumedMessages = append([]ChatMessage(nil), req.Messages...)
		mu.Unlock()
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Found all 15 AP stories.\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer mockLLM.Close()

	tmp := t.TempDir()
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.BaseURL = mockLLM.URL
	cfg.APIKey = "sk-or-test"
	cfg.MCPServers = []MCPServerConfig{{
		Name:                 "brave",
		Transport:            "http",
		URL:                  mcpSrv.URL,
		Enabled:              true,
		RequireApprovalTools: []string{"*"},
	}}
	if _, err := eng.UpdateConfig(cfg, false); err != nil {
		t.Fatal(err)
	}

	eng.Start(t.Context())
	sub := eng.Subscribe()
	defer eng.Unsubscribe(sub)

	if _, _, err := eng.EnqueueUserMessage("general", "What is on AP News?", ""); err != nil {
		t.Fatal(err)
	}

	var aprID string
	deadline := time.After(5 * time.Second)
	for aprID == "" {
		select {
		case ev := <-sub:
			if ev.Type == "approval" {
				apr, _ := ev.Data.(Approval)
				aprID = apr.ID
			}
		case <-deadline:
			t.Fatal("timed out waiting for approval")
		}
	}

	if _, err := eng.DecideApproval(aprID, true, ""); err != nil {
		t.Fatal(err)
	}
	deadline2 := time.After(5 * time.Second)
	for {
		select {
		case ev := <-sub:
			if ev.Type == "message_done" {
				m, _ := ev.Data.(Message)
				if strings.Contains(m.Content, "Found all 15 AP stories") {
					goto done
				}
			}
		case <-deadline2:
			t.Fatal("timed out waiting for resumed reply")
		}
	}
done:

	mu.Lock()
	gotMsgs := resumedMessages
	mu.Unlock()

	var toolMsgContent string
	for _, m := range gotMsgs {
		if m.Role == "tool" && m.ToolCallID == aprID {
			toolMsgContent, _ = m.Content.(string)
		}
	}
	if !strings.Contains(toolMsgContent, "AP Story #15 — Full headline preserved") {
		t.Fatalf("expected full unwrapped 15th story in ephemeral tool message, got len=%d content=%q", len(toolMsgContent), truncateText(toolMsgContent, 300))
	}
	if strings.Contains(toolMsgContent, `\"url\"`) {
		t.Fatalf("expected MCP text envelope unwrapped without escaped JSON wrapper, got %q", truncateText(toolMsgContent, 200))
	}

	msgs, err := eng.Store().ListMessages("general", 20)
	if err != nil {
		t.Fatal(err)
	}
	var foundCompact bool
	for _, m := range msgs {
		if strings.HasPrefix(m.Content, "[approval_result]") {
			foundCompact = true
			if len(m.Content) > 300 {
				t.Fatalf("expected compact [approval_result] in durable history, got %d bytes: %s", len(m.Content), m.Content)
			}
		}
	}
	if !foundCompact {
		t.Fatal("expected [approval_result] message in channel history")
	}

	apr, err := eng.Store().GetApproval(aprID)
	if err != nil {
		t.Fatal(err)
	}
	if len(apr.ResultJSON) <= len(innerJSON) || !strings.Contains(apr.ResultJSON, "AP Story #15") {
		t.Fatalf("expected full raw JSON preserved in approval record, got len=%d", len(apr.ResultJSON))
	}
}

func TestToolOutputSpillAndPaginatedReadUTF8Safe(t *testing.T) {
	tmp := t.TempDir()
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	// Place multi-byte UTF-8 runes directly across the 24 KiB truncation boundary.
	prefix := strings.Repeat("a", defaultToolResultMaxBytes-2)
	tail := strings.Repeat("🚀日本語", 2000) + "FINAL_TAIL_MARKER"
	fullText := prefix + tail
	wrapped, _ := json.Marshal(map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": fullText},
		},
	})

	shaped := eng.formatToolResultForLLM("general", "mcp__brave__brave_llm_context", string(wrapped))
	if !utf8.ValidString(shaped) {
		t.Fatalf("truncated output split a UTF-8 rune")
	}
	if !strings.Contains(shaped, "[truncated: showing") || !strings.Contains(shaped, "tool_output_read") {
		t.Fatalf("expected explicit truncation footer with tool_output_read hint, got tail=%q", shaped[max(0, len(shaped)-250):])
	}

	m := regexp.MustCompile(`output_id="(out-[0-9a-f]{12})"`).FindStringSubmatch(shaped)
	if len(m) != 2 {
		t.Fatalf("expected output_id in truncation footer, got %q", shaped[max(0, len(shaped)-250):])
	}
	outputID := m[1]

	// Reconstruct the full payload by paging through tool_output_read.
	var reassembled strings.Builder
	offset := 0
	for {
		outStr, err := eng.executeTool(t.Context(), ToolCall{
			ID:       "read-1",
			Type:     "function",
			Function: ToolCallFunction{Name: "tool_output_read", Arguments: fmt.Sprintf(`{"output_id":%q,"offset":%d,"max_bytes":16384}`, outputID, offset)},
		}, &toolExecContext{ChannelID: "general", Source: "chat", Config: eng.Config()})
		if err != nil {
			t.Fatalf("tool_output_read failed at offset %d: %v", offset, err)
		}
		var page struct {
			NextOffset int    `json:"next_offset"`
			HasMore    bool   `json:"has_more"`
			Content    string `json:"content"`
		}
		if err := json.Unmarshal([]byte(outStr), &page); err != nil {
			t.Fatal(err)
		}
		if !utf8.ValidString(page.Content) {
			t.Fatalf("tool_output_read returned invalid UTF-8 at offset %d", offset)
		}
		reassembled.WriteString(page.Content)
		if !page.HasMore {
			break
		}
		if page.NextOffset <= offset {
			t.Fatalf("next_offset did not advance: %d -> %d", offset, page.NextOffset)
		}
		offset = page.NextOffset
	}
	if reassembled.String() != fullText {
		t.Fatalf("reassembled output mismatch: got %d bytes, want %d bytes", reassembled.Len(), len(fullText))
	}

	// Reject invalid/path-traversal output_id.
	if _, err := eng.executeTool(t.Context(), ToolCall{
		ID:       "bad-read",
		Type:     "function",
		Function: ToolCallFunction{Name: "tool_output_read", Arguments: `{"output_id":"../secret"}`},
	}, &toolExecContext{ChannelID: "general", Source: "chat", Config: eng.Config()}); err == nil {
		t.Fatal("expected path-traversal output_id to fail")
	}
}

func TestMCPTransportLargeSSEOversizedHTTPAndStdioError(t *testing.T) {
	// 1. SSE line > 64 KiB succeeds.
	bigPayload := strings.Repeat("x", 90*1024)
	sseSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2024-11-05"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": []map[string]any{{"name": "big_sse"}}}})
		case "tools/call":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"blob\":%q}}\n\n", req.ID, bigPayload)
		}
	}))
	defer sseSrv.Close()

	mgr := NewMCPManager([]MCPServerConfig{{
		Name: "bigsse", Transport: "http", URL: sseSrv.URL, Enabled: true, ApprovalPolicy: MCPApprovalPolicyNone,
	}}, nil)
	defer mgr.Close()

	res, err := mgr.CallTool(t.Context(), "mcp__bigsse__big_sse", nil)
	if err != nil {
		t.Fatalf("large SSE CallTool failed: %v", err)
	}
	resMap, _ := res.(map[string]any)
	if resMap["blob"] != bigPayload {
		t.Fatalf("expected 90KB SSE payload intact, got len=%d", len(fmt.Sprint(resMap["blob"])))
	}

	// 2. HTTP response exceeding mcpMaxHTTPResponseBytes fails explicitly.
	hugeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2024-11-05"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": []map[string]any{{"name": "huge"}}}})
		case "tools/call":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + strings.Repeat("z", mcpMaxHTTPResponseBytes+128) + `"}`))
		}
	}))
	defer hugeSrv.Close()

	hugeMgr := NewMCPManager([]MCPServerConfig{{
		Name: "hugesrv", Transport: "http", URL: hugeSrv.URL, Enabled: true, ApprovalPolicy: MCPApprovalPolicyNone,
	}}, nil)
	defer hugeMgr.Close()

	if _, err := hugeMgr.CallTool(t.Context(), "mcp__hugesrv__huge", nil); err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("expected size limit error for >4MB HTTP MCP response, got %v", err)
	}

	// 3. Stdio rpcCallLocked returns matched JSON-RPC error instead of hanging.
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	conn := &mcpClientConn{
		cfg:     MCPServerConfig{Name: "stdio-err", Transport: "stdio", Enabled: true},
		stdin:   stdinW,
		reader:  bufio.NewReader(stdoutR),
		started: true,
	}
	go func() {
		defer func() { _ = stdoutW.Close() }()
		sc := bufio.NewScanner(stdinR)
		if sc.Scan() {
			var req struct {
				ID int64 `json:"id"`
			}
			_ = json.Unmarshal(sc.Bytes(), &req)
			_, _ = fmt.Fprintf(stdoutW, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"error\":{\"code\":-32000,\"message\":\"boom from stdio\"}}\n", req.ID)
		}
	}()
	_, rpcErr := conn.callTool(t.Context(), nil, "fail_tool", nil)
	if rpcErr == nil || !strings.Contains(rpcErr.Error(), "boom from stdio") {
		t.Fatalf("expected matched stdio JSON-RPC error, got %v", rpcErr)
	}
}

func TestMCPApprovalPolicyAndReadOnlyHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2024-11-05"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"tools": []map[string]any{
						{"name": "search_news", "annotations": map[string]any{"readOnlyHint": true}},
						{"name": "delete_item", "annotations": map[string]any{"readOnlyHint": false, "destructiveHint": true}},
						{"name": "mystery_op"},
					},
				},
			})
		case "tools/call":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"called": req.Params.Name}})
		}
	}))
	defer srv.Close()

	if err := ValidateMCPServerConfig(MCPServerConfig{
		Name: "badpol", Transport: "http", URL: srv.URL, ApprovalPolicy: "bogus",
	}, false); err == nil {
		t.Fatal("expected invalid approval_policy to fail validation")
	}

	tmp := t.TempDir()
	eng, err := Open(filepath.Join(tmp, "assistant.db"), filepath.Join(tmp, "assistant.json"), filepath.Join(tmp, "vault"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	setPolicy := func(policy string, overrides []string) {
		cfg := eng.Config()
		cfg.MCPServers = []MCPServerConfig{{
			Name:                 "svc",
			Transport:            "http",
			URL:                  srv.URL,
			Enabled:              true,
			ApprovalPolicy:       policy,
			RequireApprovalTools: overrides,
		}}
		if _, uErr := eng.UpdateConfig(cfg, false); uErr != nil {
			t.Fatal(uErr)
		}
	}

	tctx := &toolExecContext{ChannelID: "general", Source: "chat", Config: eng.Config()}

	// writes_only: readOnlyHint=true runs inline; false and missing require approval; per-tool override wins.
	setPolicy(MCPApprovalPolicyWritesOnly, nil)
	out, err := eng.executeTool(t.Context(), ToolCall{ID: "1", Type: "function", Function: ToolCallFunction{Name: "mcp__svc__search_news", Arguments: `{}`}}, tctx)
	if err != nil || !strings.Contains(out, `"called":"search_news"`) {
		t.Fatalf("expected search_news to auto-run under writes_only, got out=%q err=%v", out, err)
	}
	for _, gated := range []string{"mcp__svc__delete_item", "mcp__svc__mystery_op"} {
		if _, gErr := eng.executeTool(t.Context(), ToolCall{ID: "2", Type: "function", Function: ToolCallFunction{Name: gated, Arguments: `{}`}}, tctx); !isApprovalErr(gErr) {
			t.Fatalf("expected %s to require approval under writes_only, got %v", gated, gErr)
		}
	}
	setPolicy(MCPApprovalPolicyWritesOnly, []string{"search_news"})
	if _, gErr := eng.executeTool(t.Context(), ToolCall{ID: "3", Type: "function", Function: ToolCallFunction{Name: "mcp__svc__search_news", Arguments: `{}`}}, tctx); !isApprovalErr(gErr) {
		t.Fatalf("expected require_approval_tools override to gate search_news, got %v", gErr)
	}

	// none: all auto-allow unless overridden in require_approval_tools.
	setPolicy(MCPApprovalPolicyNone, []string{"delete_item"})
	if _, err := eng.executeTool(t.Context(), ToolCall{ID: "4", Type: "function", Function: ToolCallFunction{Name: "mcp__svc__mystery_op", Arguments: `{}`}}, tctx); err != nil {
		t.Fatalf("expected mystery_op to auto-run under none, got %v", err)
	}
	if _, gErr := eng.executeTool(t.Context(), ToolCall{ID: "5", Type: "function", Function: ToolCallFunction{Name: "mcp__svc__delete_item", Arguments: `{}`}}, tctx); !isApprovalErr(gErr) {
		t.Fatalf("expected overridden delete_item to require approval under none, got %v", gErr)
	}

	// all: every tool requires approval.
	setPolicy(MCPApprovalPolicyAll, nil)
	if _, gErr := eng.executeTool(t.Context(), ToolCall{ID: "6", Type: "function", Function: ToolCallFunction{Name: "mcp__svc__search_news", Arguments: `{}`}}, tctx); !isApprovalErr(gErr) {
		t.Fatalf("expected search_news to require approval under all, got %v", gErr)
	}
}

func isApprovalErr(err error) bool {
	_, ok := errors.AsType[*approvalRequiredError](err)
	return ok
}
