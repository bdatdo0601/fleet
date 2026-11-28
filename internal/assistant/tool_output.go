package assistant

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	defaultToolResultMaxBytes    = 24 * 1024
	toolOutputDefaultReadBytes   = 16 * 1024
	toolOutputMaxReadBytes       = 32 * 1024
	toolOutputMaxSpillBytes      = 4 * 1024 * 1024
	toolOutputMaxFilesPerChannel = 50
)

var toolOutputIDRE = regexp.MustCompile(`^out-[0-9a-f]{12}$`)

// unwrapMCPToolResult extracts text/structured content from standard MCP tools/call envelopes.
func unwrapMCPToolResult(raw string) string {
	var env struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			MimeType string `json:"mimeType"`
			URI      string `json:"uri"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return raw
	}
	structured := bytes.TrimSpace(env.StructuredContent)
	if len(env.Content) == 0 && len(structured) == 0 {
		return raw
	}

	parts := make([]string, 0, len(env.Content)+1)
	for _, item := range env.Content {
		if item.Type == "text" || (item.Type == "" && item.Text != "") {
			if item.Text != "" {
				parts = append(parts, item.Text)
			}
			continue
		}
		parts = append(parts, fmt.Sprintf("[%s content: %s]", cmp.Or(item.Type, "binary"), cmp.Or(item.MimeType, item.URI, "omitted")))
	}
	if len(parts) == 0 && len(structured) > 0 && !bytes.Equal(structured, []byte("null")) {
		parts = append(parts, string(structured))
	}
	if len(parts) == 0 {
		return raw
	}
	out := strings.Join(parts, "\n\n")
	if env.IsError {
		return "[MCP tool error]\n" + out
	}
	return out
}

func (e *Engine) toolOutputsDir(channelID string) string {
	return filepath.Join(e.filesDir, SanitizeFileName(cmp.Or(strings.TrimSpace(channelID), "general")), "tool-outputs")
}

func (e *Engine) spillToolOutput(channelID, content string) (string, string, error) {
	if strings.TrimSpace(e.filesDir) == "" {
		return "", "", fmt.Errorf("tool output directory unavailable")
	}
	dir := e.toolOutputsDir(channelID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	outputID := randomHexID("out")
	spillPath := filepath.Join(dir, outputID+".txt")
	toWrite := content
	if len(toWrite) > toolOutputMaxSpillBytes {
		toWrite = truncateUTF8Bytes(toWrite, toolOutputMaxSpillBytes)
	}
	if err := os.WriteFile(spillPath, []byte(toWrite), 0o600); err != nil {
		return "", "", err
	}
	pruneToolOutputs(dir, toolOutputMaxFilesPerChannel)

	var stagedSandboxPath string
	if e.Config().Sandbox.Enabled && e.sandbox != nil {
		if err := e.stageFileToSandboxIn(cmp.Or(strings.TrimSpace(channelID), "general"), spillPath, outputID+".txt"); err == nil {
			stagedSandboxPath = "/work/in/" + outputID + ".txt"
		}
	}
	return outputID, stagedSandboxPath, nil
}

func pruneToolOutputs(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) <= keep {
		return
	}
	type fileEntry struct {
		name    string
		modUnix int64
	}
	files := make([]fileEntry, 0, len(entries))
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		info, infoErr := ent.Info()
		if infoErr != nil {
			continue
		}
		files = append(files, fileEntry{name: ent.Name(), modUnix: info.ModTime().UnixNano()})
	}
	if len(files) <= keep {
		return
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].modUnix != files[j].modUnix {
			return files[i].modUnix < files[j].modUnix
		}
		return files[i].name < files[j].name
	})
	for i := range len(files) - keep {
		_ = os.Remove(filepath.Join(dir, files[i].name))
	}
}

func (e *Engine) formatToolResultForLLM(channelID, toolName, raw string) string {
	if toolName == "tool_output_read" {
		return raw
	}
	shaped := raw
	if strings.HasPrefix(toolName, "mcp__") {
		shaped = unwrapMCPToolResult(raw)
	}
	if len(shaped) <= defaultToolResultMaxBytes {
		return shaped
	}
	head := truncateUTF8Bytes(shaped, defaultToolResultMaxBytes)
	outID, stagedPath, err := e.spillToolOutput(channelID, shaped)
	if err != nil {
		return head + fmt.Sprintf("\n\n... [truncated: showing %d of %d bytes]", len(head), len(shaped))
	}
	sandboxHint := ""
	if stagedPath != "" {
		sandboxHint = fmt.Sprintf(" (also staged at %s in sandbox)", stagedPath)
	}
	footer := fmt.Sprintf(
		"\n\n... [truncated: showing %d of %d bytes; full output saved as output_id=%q — call `tool_output_read` with output_id=%q and offset=%d to read more%s]",
		len(head), len(shaped), outID, outID, len(head), sandboxHint,
	)
	return head + footer
}

func (e *Engine) execToolOutputRead(channelID string, args map[string]any) (any, error) {
	outputID := strArg(args, "output_id")
	if !toolOutputIDRE.MatchString(outputID) {
		return nil, newErr("invalid output_id", http.StatusBadRequest)
	}
	offset := max(intArg(args, "offset", 0), 0)
	maxBytes := intArg(args, "max_bytes", toolOutputDefaultReadBytes)
	if maxBytes <= 0 {
		maxBytes = toolOutputDefaultReadBytes
	}
	maxBytes = min(max(maxBytes, 256), toolOutputMaxReadBytes)

	data, err := os.ReadFile(filepath.Join(e.toolOutputsDir(channelID), outputID+".txt"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, newErr(fmt.Sprintf("tool output %q not found", outputID), http.StatusNotFound)
		}
		return nil, err
	}
	if offset >= len(data) {
		return map[string]any{
			"output_id":   outputID,
			"offset":      offset,
			"next_offset": len(data),
			"total_bytes": len(data),
			"has_more":    false,
			"content":     "",
		}, nil
	}
	start := offset
	for start < len(data) && !utf8.RuneStart(data[start]) {
		start++
	}
	chunk := truncateUTF8Bytes(string(data[start:]), maxBytes)
	nextOffset := start + len(chunk)
	return map[string]any{
		"output_id":   outputID,
		"offset":      start,
		"next_offset": nextOffset,
		"total_bytes": len(data),
		"has_more":    nextOffset < len(data),
		"content":     chunk,
	}, nil
}
