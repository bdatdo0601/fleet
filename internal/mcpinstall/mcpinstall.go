// Package mcpinstall manages idempotent MCP server and lifecycle hook configs for claude, codex, and antigravity.
package mcpinstall

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/bdatdo0601/fleet/internal/paths"
)

const hookMarker = "agent-hook --harness "

var (
	tomlHeaderRE = regexp.MustCompile(`^\s*\[[^\]]+\]\s*$`)
	tomlHooksRE  = regexp.MustCompile(`^\s*hooks\s*=`)
)

type hookEventSpec struct {
	event   string
	matcher string // empty means omit matcher
	timeout int
}

var claudeHookEvents = []hookEventSpec{
	{"SessionStart", "", 5},
	{"UserPromptSubmit", "", 5},
	{"PreToolUse", "AskUserQuestion|ExitPlanMode", 5},
	{"PermissionRequest", "*", 5},
	{"PostToolUse", "*", 5},
	{"PostToolUseFailure", "*", 5},
	{"PermissionDenied", "*", 5},
	{"Notification", "permission_prompt|elicitation_dialog|elicitation_url_dialog|agent_needs_input", 5},
	{"Stop", "", 5},
}

var codexHookEvents = []hookEventSpec{
	{"SessionStart", "", 5},
	{"UserPromptSubmit", "", 5},
	{"PreToolUse", "request_user_input", 5},
	{"PermissionRequest", "*", 5},
	{"PostToolUse", "*", 5},
	{"Stop", "", 5},
	{"Interrupt", "", 3},
	{"SessionEnd", "", 3},
}

// InstallOpts configures Install.
type InstallOpts struct {
	Home     string
	UID      *int
	GID      *int
	BinPath  string
	ForceAll bool
}

func loadJSON(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, fmt.Errorf("%s must contain a JSON object", path)
	}
	return out, nil
}

func marshalPrettyJSON(data any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func ensureDir(dir string, uid, gid *int) error {
	var missing []string
	cur := dir
	for {
		if _, err := os.Stat(cur); err == nil {
			break
		}
		missing = append(missing, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if uid != nil && gid != nil && os.Geteuid() == 0 {
		for i := len(missing) - 1; i >= 0; i-- {
			_ = os.Chown(missing[i], *uid, *gid)
		}
	}
	return nil
}

func writeUserFile(path, content string, uid, gid *int) (bool, error) {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == content {
		return false, nil
	}
	if err := ensureDir(filepath.Dir(path), uid, gid); err != nil {
		return false, err
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.fleet-tmp", filepath.Base(path)))
	if err := os.WriteFile(tmp, []byte(content), mode); err != nil {
		return false, err
	}
	_ = os.Chmod(tmp, mode)
	if uid != nil && gid != nil && os.Geteuid() == 0 {
		_ = os.Chown(tmp, *uid, *gid)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return true, nil
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func hasBinary(name, home string) bool {
	if _, err := exec.LookPath(name); err == nil {
		return true
	}
	for _, dir := range []string{filepath.Join(home, ".local", "bin"), "/opt/homebrew/bin", "/usr/local/bin"} {
		if pathExists(filepath.Join(dir, name)) {
			return true
		}
	}
	return false
}

// DetectHarnesses returns installed agent harnesses under home (or all if forceAll is true).
func DetectHarnesses(home string, forceAll bool) []string {
	if forceAll {
		out := make([]string, len(paths.Harnesses))
		copy(out, paths.Harnesses)
		return out
	}
	var found []string
	if pathExists(filepath.Join(home, ".claude")) || pathExists(filepath.Join(home, ".claude.json")) || hasBinary("claude", home) {
		found = append(found, "claude")
	}
	if pathExists(filepath.Join(home, ".codex")) || hasBinary("codex", home) {
		found = append(found, "codex")
	}
	if pathExists(filepath.Join(home, ".gemini")) || hasBinary("agy", home) {
		found = append(found, "antigravity")
	}
	return found
}

func isFleetHookGroup(group any, harness string) bool {
	m, ok := group.(map[string]any)
	if !ok {
		return false
	}
	handlers, ok := m["hooks"].([]any)
	if !ok {
		return false
	}
	needle := hookMarker + harness
	for _, rawH := range handlers {
		if h, ok := rawH.(map[string]any); ok {
			if cmd, _ := h["command"].(string); strings.Contains(cmd, needle) {
				return true
			}
		}
	}
	return false
}

func upsertClaudeStyleHooks(hooksRoot map[string]any, harness, cmdBin string) {
	hookCmd := fmt.Sprintf("%s agent-hook --harness %s", cmdBin, harness)
	events := claudeHookEvents
	if harness == "codex" {
		events = codexHookEvents
	}
	for _, spec := range events {
		var entries []any
		if raw, ok := hooksRoot[spec.event].([]any); ok {
			entries = raw
		}
		handler := map[string]any{"type": "command", "command": hookCmd, "timeout": spec.timeout}
		desired := map[string]any{"hooks": []any{handler}}
		if spec.matcher != "" {
			desired["matcher"] = spec.matcher
		}
		replaced := false
		for i, g := range entries {
			if isFleetHookGroup(g, harness) {
				entries[i] = desired
				replaced = true
				break
			}
		}
		if !replaced {
			// Append only, never reorder earlier items (preserves Codex trust-hash indices).
			entries = append(entries, desired)
		}
		hooksRoot[spec.event] = entries
	}
}

func removeClaudeStyleHooks(hooksRoot map[string]any, harness string) bool {
	changed := false
	for event, raw := range hooksRoot {
		entries, ok := raw.([]any)
		if !ok {
			continue
		}
		var filtered []any
		for _, g := range entries {
			if !isFleetHookGroup(g, harness) {
				filtered = append(filtered, g)
			}
		}
		if len(filtered) != len(entries) {
			changed = true
			if len(filtered) > 0 {
				hooksRoot[event] = filtered
			} else {
				delete(hooksRoot, event)
			}
		}
	}
	return changed
}

func updateCodexTOML(raw, cmdBin string, remove bool) string {
	var lines []string
	if raw != "" {
		rawClean := strings.ReplaceAll(raw, "\r\n", "\n")
		rawClean = strings.TrimSuffix(rawClean, "\n")
		if rawClean != "" || strings.Contains(raw, "\n") {
			lines = strings.Split(rawClean, "\n")
		}
	}
	var cleaned []string
	skipping := false
	for _, line := range lines {
		stripped := strings.TrimSpace(line)
		if stripped == "[mcp_servers.fleet]" {
			skipping = true
			continue
		}
		if skipping && tomlHeaderRE.MatchString(stripped) {
			skipping = false
		}
		if !skipping {
			cleaned = append(cleaned, line)
		}
	}
	if remove {
		for len(cleaned) > 0 && strings.TrimSpace(cleaned[len(cleaned)-1]) == "" {
			cleaned = cleaned[:len(cleaned)-1]
		}
		if len(cleaned) == 0 {
			return ""
		}
		return strings.Join(cleaned, "\n") + "\n"
	}

	featIdx := -1
	for i, ln := range cleaned {
		if strings.TrimSpace(ln) == "[features]" {
			featIdx = i
			break
		}
	}
	if featIdx < 0 {
		if len(cleaned) > 0 && strings.TrimSpace(cleaned[len(cleaned)-1]) != "" {
			cleaned = append(cleaned, "")
		}
		cleaned = append(cleaned, "[features]", "hooks = true")
	} else {
		endIdx := len(cleaned)
		for i := featIdx + 1; i < len(cleaned); i++ {
			if tomlHeaderRE.MatchString(strings.TrimSpace(cleaned[i])) {
				endIdx = i
				break
			}
		}
		hooksIdx := -1
		for i := featIdx + 1; i < endIdx; i++ {
			if tomlHooksRE.MatchString(cleaned[i]) {
				hooksIdx = i
				break
			}
		}
		if hooksIdx >= 0 {
			cleaned[hooksIdx] = "hooks = true"
		} else {
			cleaned = append(cleaned[:featIdx+1], append([]string{"hooks = true"}, cleaned[featIdx+1:]...)...)
		}
	}
	if len(cleaned) > 0 && strings.TrimSpace(cleaned[len(cleaned)-1]) != "" {
		cleaned = append(cleaned, "")
	}
	cmdQuoted, _ := json.Marshal(cmdBin)
	cleaned = append(cleaned,
		"[mcp_servers.fleet]",
		fmt.Sprintf("command = %s", string(cmdQuoted)),
		`args = ["mcp", "serve", "--harness", "codex"]`,
	)
	return strings.Join(cleaned, "\n") + "\n"
}

func containsHarness(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}

// Install idempotently writes MCP and lifecycle hook configs for detected harnesses.
func Install(opts InstallOpts) ([]string, error) {
	targetHome := opts.Home
	if targetHome == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		targetHome = h
	}
	cmdBin := opts.BinPath
	if cmdBin == "" {
		cmdBin = paths.LauncherPath()
	}
	harnesses := DetectHarnesses(targetHome, opts.ForceAll)
	var changed []string

	if containsHarness(harnesses, "claude") {
		claudeJSON := filepath.Join(targetHome, ".claude.json")
		data, err := loadJSON(claudeJSON)
		if err != nil {
			return nil, err
		}
		servers, ok := data["mcpServers"].(map[string]any)
		if !ok {
			if _, exists := data["mcpServers"]; !exists {
				servers = map[string]any{}
				data["mcpServers"] = servers
			}
		}
		if servers != nil {
			servers["fleet"] = map[string]any{
				"type":    "stdio",
				"command": cmdBin,
				"args":    []string{"mcp", "serve", "--harness", "claude"},
			}
			txt, err := marshalPrettyJSON(data)
			if err != nil {
				return nil, err
			}
			wrote, err := writeUserFile(claudeJSON, txt, opts.UID, opts.GID)
			if err != nil {
				return nil, err
			}
			if wrote {
				changed = append(changed, claudeJSON)
			}
		}

		settingsJSON := filepath.Join(targetHome, ".claude", "settings.json")
		settings, err := loadJSON(settingsJSON)
		if err != nil {
			return nil, err
		}
		hooksRoot, ok := settings["hooks"].(map[string]any)
		if !ok {
			if _, exists := settings["hooks"]; !exists {
				hooksRoot = map[string]any{}
				settings["hooks"] = hooksRoot
			}
		}
		if hooksRoot != nil {
			upsertClaudeStyleHooks(hooksRoot, "claude", cmdBin)
			txt, err := marshalPrettyJSON(settings)
			if err != nil {
				return nil, err
			}
			wrote, err := writeUserFile(settingsJSON, txt, opts.UID, opts.GID)
			if err != nil {
				return nil, err
			}
			if wrote {
				changed = append(changed, settingsJSON)
			}
		}
	}

	if containsHarness(harnesses, "codex") {
		codexTOML := filepath.Join(targetHome, ".codex", "config.toml")
		rawTOML := ""
		if b, err := os.ReadFile(codexTOML); err == nil {
			rawTOML = string(b)
		}
		updatedTOML := updateCodexTOML(rawTOML, cmdBin, false)
		wrote, err := writeUserFile(codexTOML, updatedTOML, opts.UID, opts.GID)
		if err != nil {
			return nil, err
		}
		if wrote {
			changed = append(changed, codexTOML)
		}

		codexHooks := filepath.Join(targetHome, ".codex", "hooks.json")
		hooksData, err := loadJSON(codexHooks)
		if err != nil {
			return nil, err
		}
		hooksRoot, ok := hooksData["hooks"].(map[string]any)
		if !ok {
			if _, exists := hooksData["hooks"]; !exists {
				hooksRoot = map[string]any{}
				hooksData["hooks"] = hooksRoot
			}
		}
		if hooksRoot != nil {
			upsertClaudeStyleHooks(hooksRoot, "codex", cmdBin)
			txt, err := marshalPrettyJSON(hooksData)
			if err != nil {
				return nil, err
			}
			wrote, err := writeUserFile(codexHooks, txt, opts.UID, opts.GID)
			if err != nil {
				return nil, err
			}
			if wrote {
				changed = append(changed, codexHooks)
			}
		}
	}

	if containsHarness(harnesses, "antigravity") {
		agyMCP := filepath.Join(targetHome, ".gemini", "config", "mcp_config.json")
		mcpData, err := loadJSON(agyMCP)
		if err != nil {
			return nil, err
		}
		servers, ok := mcpData["mcpServers"].(map[string]any)
		if !ok {
			if _, exists := mcpData["mcpServers"]; !exists {
				servers = map[string]any{}
				mcpData["mcpServers"] = servers
			}
		}
		if servers != nil {
			servers["fleet"] = map[string]any{
				"command": cmdBin,
				"args":    []string{"mcp", "serve", "--harness", "antigravity"},
			}
			txt, err := marshalPrettyJSON(mcpData)
			if err != nil {
				return nil, err
			}
			wrote, err := writeUserFile(agyMCP, txt, opts.UID, opts.GID)
			if err != nil {
				return nil, err
			}
			if wrote {
				changed = append(changed, agyMCP)
			}
		}

		agyHooks := filepath.Join(targetHome, ".gemini", "config", "hooks.json")
		agyHooksData, err := loadJSON(agyHooks)
		if err != nil {
			return nil, err
		}
		hookCmd := fmt.Sprintf("%s agent-hook --harness antigravity", cmdBin)
		handler := map[string]any{"type": "command", "command": hookCmd, "timeout": 5}
		agyHooksData["fleet"] = map[string]any{
			"SessionStart":  []any{handler},
			"PreInvocation": []any{handler},
			"PreToolUse":    []any{map[string]any{"matcher": "ask_question|ask_permission", "hooks": []any{handler}}},
			"PostToolUse":   []any{map[string]any{"matcher": "*", "hooks": []any{handler}}},
			"Stop":          []any{handler},
		}
		txt, err := marshalPrettyJSON(agyHooksData)
		if err != nil {
			return nil, err
		}
		wrote, err := writeUserFile(agyHooks, txt, opts.UID, opts.GID)
		if err != nil {
			return nil, err
		}
		if wrote {
			changed = append(changed, agyHooks)
		}
	}

	return changed, nil
}

// Uninstall removes fleet MCP and hook entries from all harness configs under home.
func Uninstall(home string) ([]string, error) {
	targetHome := home
	if targetHome == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		targetHome = h
	}
	var changed []string

	claudeJSON := filepath.Join(targetHome, ".claude.json")
	if pathExists(claudeJSON) {
		data, err := loadJSON(claudeJSON)
		if err != nil {
			return nil, err
		}
		if servers, ok := data["mcpServers"].(map[string]any); ok {
			if _, exists := servers["fleet"]; exists {
				delete(servers, "fleet")
				txt, err := marshalPrettyJSON(data)
				if err != nil {
					return nil, err
				}
				if wrote, err := writeUserFile(claudeJSON, txt, nil, nil); err != nil {
					return nil, err
				} else if wrote {
					changed = append(changed, claudeJSON)
				}
			}
		}
	}

	settingsJSON := filepath.Join(targetHome, ".claude", "settings.json")
	if pathExists(settingsJSON) {
		settings, err := loadJSON(settingsJSON)
		if err != nil {
			return nil, err
		}
		if hooksRoot, ok := settings["hooks"].(map[string]any); ok && removeClaudeStyleHooks(hooksRoot, "claude") {
			txt, err := marshalPrettyJSON(settings)
			if err != nil {
				return nil, err
			}
			if wrote, err := writeUserFile(settingsJSON, txt, nil, nil); err != nil {
				return nil, err
			} else if wrote {
				changed = append(changed, settingsJSON)
			}
		}
	}

	codexTOML := filepath.Join(targetHome, ".codex", "config.toml")
	if raw, err := os.ReadFile(codexTOML); err == nil {
		updated := updateCodexTOML(string(raw), "", true)
		if wrote, err := writeUserFile(codexTOML, updated, nil, nil); err != nil {
			return nil, err
		} else if wrote {
			changed = append(changed, codexTOML)
		}
	}

	codexHooks := filepath.Join(targetHome, ".codex", "hooks.json")
	if pathExists(codexHooks) {
		hooksData, err := loadJSON(codexHooks)
		if err != nil {
			return nil, err
		}
		if hooksRoot, ok := hooksData["hooks"].(map[string]any); ok && removeClaudeStyleHooks(hooksRoot, "codex") {
			txt, err := marshalPrettyJSON(hooksData)
			if err != nil {
				return nil, err
			}
			if wrote, err := writeUserFile(codexHooks, txt, nil, nil); err != nil {
				return nil, err
			} else if wrote {
				changed = append(changed, codexHooks)
			}
		}
	}

	agyMCP := filepath.Join(targetHome, ".gemini", "config", "mcp_config.json")
	if pathExists(agyMCP) {
		mcpData, err := loadJSON(agyMCP)
		if err != nil {
			return nil, err
		}
		if servers, ok := mcpData["mcpServers"].(map[string]any); ok {
			if _, exists := servers["fleet"]; exists {
				delete(servers, "fleet")
				txt, err := marshalPrettyJSON(mcpData)
				if err != nil {
					return nil, err
				}
				if wrote, err := writeUserFile(agyMCP, txt, nil, nil); err != nil {
					return nil, err
				} else if wrote {
					changed = append(changed, agyMCP)
				}
			}
		}
	}

	agyHooks := filepath.Join(targetHome, ".gemini", "config", "hooks.json")
	if pathExists(agyHooks) {
		agyHooksData, err := loadJSON(agyHooks)
		if err != nil {
			return nil, err
		}
		if _, exists := agyHooksData["fleet"]; exists {
			delete(agyHooksData, "fleet")
			txt, err := marshalPrettyJSON(agyHooksData)
			if err != nil {
				return nil, err
			}
			if wrote, err := writeUserFile(agyHooks, txt, nil, nil); err != nil {
				return nil, err
			} else if wrote {
				changed = append(changed, agyHooks)
			}
		}
	}

	return changed, nil
}

// Status reports {harness: configured} for all supported harnesses.
func Status(home string) map[string]bool {
	targetHome := home
	if targetHome == "" {
		targetHome, _ = os.UserHomeDir()
	}
	hasFleetHook := func(hooksMap map[string]any, harness string) bool {
		if hooksMap == nil {
			return false
		}
		entries, _ := hooksMap["SessionStart"].([]any)
		for _, g := range entries {
			if isFleetHookGroup(g, harness) {
				return true
			}
		}
		return false
	}

	clData, _ := loadJSON(filepath.Join(targetHome, ".claude.json"))
	clMCP, _ := clData["mcpServers"].(map[string]any)
	clSet, _ := loadJSON(filepath.Join(targetHome, ".claude", "settings.json"))
	clHooks, _ := clSet["hooks"].(map[string]any)
	_, hasClMCP := clMCP["fleet"]
	claudeOK := hasClMCP && hasFleetHook(clHooks, "claude")

	codexTOMLRaw, _ := os.ReadFile(filepath.Join(targetHome, ".codex", "config.toml"))
	cxHooksData, _ := loadJSON(filepath.Join(targetHome, ".codex", "hooks.json"))
	cxHooks, _ := cxHooksData["hooks"].(map[string]any)
	codexOK := strings.Contains(string(codexTOMLRaw), "[mcp_servers.fleet]") && hasFleetHook(cxHooks, "codex")

	agyMCPData, _ := loadJSON(filepath.Join(targetHome, ".gemini", "config", "mcp_config.json"))
	agyMCP, _ := agyMCPData["mcpServers"].(map[string]any)
	agyHooksData, _ := loadJSON(filepath.Join(targetHome, ".gemini", "config", "hooks.json"))
	_, hasAgyMCP := agyMCP["fleet"]
	_, hasAgyHooks := agyHooksData["fleet"]
	agyOK := hasAgyMCP && hasAgyHooks

	return map[string]bool{
		"claude":      claudeOK,
		"codex":       codexOK,
		"antigravity": agyOK,
	}
}

// SyncOwnerConfigs refreshes harness configs for owner when `fleet sync` runs as root.
func SyncOwnerConfigs(owner string) ([]string, error) {
	if owner == "" || owner == "root" {
		return nil, nil
	}
	u, err := user.Lookup(owner)
	if err != nil {
		return nil, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return nil, err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return nil, err
	}
	return Install(InstallOpts{
		Home: u.HomeDir,
		UID:  &uid,
		GID:  &gid,
	})
}
