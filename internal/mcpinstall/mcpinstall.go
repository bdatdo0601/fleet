// Package mcpinstall manages idempotent MCP server and lifecycle hook configs for claude, codex, and antigravity.
package mcpinstall

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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
	{"SessionEnd", "", 3},
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

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func writeUserFile(path, content string, uid, gid *int) (bool, error) {
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	return paths.WriteFileAtomic(path, []byte(content), mode, uid, gid)
}

func subMap(root map[string]any, key string, create bool) map[string]any {
	if m, ok := root[key].(map[string]any); ok {
		return m
	}
	if _, exists := root[key]; exists || !create {
		return nil
	}
	m := map[string]any{}
	root[key] = m
	return m
}

func editJSONFile(path string, uid, gid *int, create bool, mutate func(map[string]any) bool) (bool, error) {
	if !create && !pathExists(path) {
		return false, nil
	}
	data, err := loadJSON(path)
	if err != nil {
		return false, err
	}
	if !mutate(data) {
		return false, nil
	}
	txt, err := marshalPrettyJSON(data)
	if err != nil {
		return false, err
	}
	return writeUserFile(path, txt, uid, gid)
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
		return slices.Clone(paths.Harnesses)
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
		entries, _ := hooksRoot[spec.event].([]any)
		handler := map[string]any{"type": "command", "command": hookCmd, "timeout": spec.timeout}
		desired := map[string]any{"hooks": []any{handler}}
		if spec.matcher != "" {
			desired["matcher"] = spec.matcher
		}
		if idx := slices.IndexFunc(entries, func(g any) bool { return isFleetHookGroup(g, harness) }); idx >= 0 {
			entries[idx] = desired
		} else {
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
		filtered := slices.DeleteFunc(slices.Clone(entries), func(g any) bool {
			return isFleetHookGroup(g, harness)
		})
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

func stripTOMLSection(lines []string, header string) []string {
	var cleaned []string
	skipping := false
	for _, line := range lines {
		stripped := strings.TrimSpace(line)
		if stripped == header {
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
	return cleaned
}

func ensureCodexFeaturesHook(lines []string) []string {
	featIdx := slices.IndexFunc(lines, func(ln string) bool {
		return strings.TrimSpace(ln) == "[features]"
	})
	if featIdx < 0 {
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		return append(lines, "[features]", "hooks = true")
	}
	endIdx := len(lines)
	for i := featIdx + 1; i < len(lines); i++ {
		if tomlHeaderRE.MatchString(strings.TrimSpace(lines[i])) {
			endIdx = i
			break
		}
	}
	for i := featIdx + 1; i < endIdx; i++ {
		if tomlHooksRE.MatchString(lines[i]) {
			lines[i] = "hooks = true"
			return lines
		}
	}
	return slices.Insert(lines, featIdx+1, "hooks = true")
}

func updateCodexTOML(raw, cmdBin string, remove bool) string {
	var lines []string
	if raw != "" {
		clean := strings.TrimSuffix(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
		if clean != "" || strings.Contains(raw, "\n") {
			lines = strings.Split(clean, "\n")
		}
	}
	cleaned := stripTOMLSection(lines, "[mcp_servers.fleet]")
	if remove {
		for len(cleaned) > 0 && strings.TrimSpace(cleaned[len(cleaned)-1]) == "" {
			cleaned = cleaned[:len(cleaned)-1]
		}
		if len(cleaned) == 0 {
			return ""
		}
		return strings.Join(cleaned, "\n") + "\n"
	}

	cleaned = ensureCodexFeaturesHook(cleaned)
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

type jsonEditSpec struct {
	relPath string
	mutate  func(map[string]any) bool
}

func applyEdits(home string, uid, gid *int, create bool, specs []jsonEditSpec) ([]string, error) {
	var changed []string
	for _, s := range specs {
		p := filepath.Join(home, s.relPath)
		wrote, err := editJSONFile(p, uid, gid, create, s.mutate)
		if err != nil {
			return nil, err
		}
		if wrote {
			changed = append(changed, p)
		}
	}
	return changed, nil
}

// Install idempotently writes MCP and lifecycle hook configs for detected harnesses.
func Install(opts InstallOpts) ([]string, error) {
	targetHome := opts.Home
	if targetHome == "" {
		var err error
		if targetHome, err = os.UserHomeDir(); err != nil {
			return nil, err
		}
	}
	cmdBin := opts.BinPath
	if cmdBin == "" {
		cmdBin = paths.LauncherPath()
	}
	harnesses := DetectHarnesses(targetHome, opts.ForceAll)
	var specs []jsonEditSpec

	if slices.Contains(harnesses, "claude") {
		specs = append(specs,
			jsonEditSpec{".claude.json", func(d map[string]any) bool {
				srv := subMap(d, "mcpServers", true)
				if srv == nil {
					return false
				}
				srv["fleet"] = map[string]any{"type": "stdio", "command": cmdBin, "args": []string{"mcp", "serve", "--harness", "claude"}}
				return true
			}},
			jsonEditSpec{filepath.Join(".claude", "settings.json"), func(d map[string]any) bool {
				h := subMap(d, "hooks", true)
				if h == nil {
					return false
				}
				upsertClaudeStyleHooks(h, "claude", cmdBin)
				return true
			}},
		)
	}

	var changed []string
	if slices.Contains(harnesses, "codex") {
		codexTOML := filepath.Join(targetHome, ".codex", "config.toml")
		rawTOML, _ := os.ReadFile(codexTOML)
		wrote, err := writeUserFile(codexTOML, updateCodexTOML(string(rawTOML), cmdBin, false), opts.UID, opts.GID)
		if err != nil {
			return nil, err
		}
		if wrote {
			changed = append(changed, codexTOML)
		}
		specs = append(specs, jsonEditSpec{filepath.Join(".codex", "hooks.json"), func(d map[string]any) bool {
			h := subMap(d, "hooks", true)
			if h == nil {
				return false
			}
			upsertClaudeStyleHooks(h, "codex", cmdBin)
			return true
		}})
	}

	if slices.Contains(harnesses, "antigravity") {
		handler := map[string]any{"type": "command", "command": cmdBin + " agent-hook --harness antigravity", "timeout": 5}
		specs = append(specs,
			jsonEditSpec{filepath.Join(".gemini", "config", "mcp_config.json"), func(d map[string]any) bool {
				srv := subMap(d, "mcpServers", true)
				if srv == nil {
					return false
				}
				srv["fleet"] = map[string]any{"command": cmdBin, "args": []string{"mcp", "serve", "--harness", "antigravity"}}
				return true
			}},
			jsonEditSpec{filepath.Join(".gemini", "config", "hooks.json"), func(d map[string]any) bool {
				d["fleet"] = map[string]any{
					"SessionStart":  []any{handler},
					"PreInvocation": []any{handler},
					"PreToolUse":    []any{map[string]any{"matcher": "ask_question|ask_permission", "hooks": []any{handler}}},
					"PostToolUse":   []any{map[string]any{"matcher": "*", "hooks": []any{handler}}},
					"Stop":          []any{handler},
				}
				return true
			}},
			jsonEditSpec{filepath.Join(".gemini", "config", "sidecars", "fleet", "sidecar.json"), func(d map[string]any) bool {
				d["display_name"] = "Fleet Message Relay"
				d["description"] = "Delivers incoming Fleet peer messages directly to Antigravity conversations via agentapi."
				d["command"] = cmdBin
				d["args"] = []string{"agents", "relay", "--harness", "antigravity"}
				d["restart_policy"] = "always"
				return true
			}},
			jsonEditSpec{filepath.Join(".gemini", "config", "config.json"), func(d map[string]any) bool {
				sidecars := subMap(d, "sidecars", true)
				if sidecars == nil {
					return false
				}
				fleetSidecar := subMap(sidecars, "fleet", true)
				if fleetSidecar == nil {
					return false
				}
				if enabled, ok := fleetSidecar["enabled"].(bool); ok && enabled {
					return false
				}
				fleetSidecar["enabled"] = true
				return true
			}},
		)
	}

	edited, err := applyEdits(targetHome, opts.UID, opts.GID, true, specs)
	if err != nil {
		return nil, err
	}
	return append(changed, edited...), nil
}

func deleteFleetKey(root map[string]any, sub string) bool {
	m := root
	if sub != "" {
		m = subMap(root, sub, false)
	}
	if _, ok := m["fleet"]; !ok {
		return false
	}
	delete(m, "fleet")
	return true
}

// Uninstall removes fleet MCP and hook entries from all harness configs under home.
func Uninstall(home string) ([]string, error) {
	targetHome := home
	if targetHome == "" {
		var err error
		if targetHome, err = os.UserHomeDir(); err != nil {
			return nil, err
		}
	}
	var changed []string
	codexTOML := filepath.Join(targetHome, ".codex", "config.toml")
	if raw, err := os.ReadFile(codexTOML); err == nil {
		if wrote, err := writeUserFile(codexTOML, updateCodexTOML(string(raw), "", true), nil, nil); err != nil {
			return nil, err
		} else if wrote {
			changed = append(changed, codexTOML)
		}
	}
	edited, err := applyEdits(targetHome, nil, nil, false, []jsonEditSpec{
		{".claude.json", func(d map[string]any) bool { return deleteFleetKey(d, "mcpServers") }},
		{filepath.Join(".claude", "settings.json"), func(d map[string]any) bool {
			h := subMap(d, "hooks", false)
			return h != nil && removeClaudeStyleHooks(h, "claude")
		}},
		{filepath.Join(".codex", "hooks.json"), func(d map[string]any) bool {
			h := subMap(d, "hooks", false)
			return h != nil && removeClaudeStyleHooks(h, "codex")
		}},
		{filepath.Join(".gemini", "config", "mcp_config.json"), func(d map[string]any) bool { return deleteFleetKey(d, "mcpServers") }},
		{filepath.Join(".gemini", "config", "hooks.json"), func(d map[string]any) bool { return deleteFleetKey(d, "") }},
		{filepath.Join(".gemini", "config", "config.json"), func(d map[string]any) bool { return deleteFleetKey(d, "sidecars") }},
	})
	if err != nil {
		return nil, err
	}
	sidecarFile := filepath.Join(targetHome, ".gemini", "config", "sidecars", "fleet", "sidecar.json")
	if err := os.Remove(sidecarFile); err == nil {
		changed = append(changed, sidecarFile)
		_ = os.Remove(filepath.Dir(sidecarFile))
	}
	return append(changed, edited...), nil
}

// Status reports {harness: configured} for all supported harnesses.
func Status(home string) map[string]bool {
	targetHome := home
	if targetHome == "" {
		targetHome, _ = os.UserHomeDir()
	}
	hasFleetHook := func(relPath, harness string) bool {
		d, _ := loadJSON(filepath.Join(targetHome, relPath))
		hooksMap := subMap(d, "hooks", false)
		entries, _ := hooksMap["SessionStart"].([]any)
		return slices.ContainsFunc(entries, func(g any) bool { return isFleetHookGroup(g, harness) })
	}
	hasKey := func(relPath, sub, key string) bool {
		d, _ := loadJSON(filepath.Join(targetHome, relPath))
		m := d
		if sub != "" {
			m = subMap(d, sub, false)
		}
		_, ok := m[key]
		return ok
	}

	hasAntigravitySidecar := func() bool {
		if !hasKey(filepath.Join(".gemini", "config", "sidecars", "fleet", "sidecar.json"), "", "command") {
			return false
		}
		d, _ := loadJSON(filepath.Join(targetHome, ".gemini", "config", "config.json"))
		sidecars := subMap(d, "sidecars", false)
		fleetCfg := subMap(sidecars, "fleet", false)
		enabled, _ := fleetCfg["enabled"].(bool)
		return enabled
	}

	codexTOMLRaw, _ := os.ReadFile(filepath.Join(targetHome, ".codex", "config.toml"))
	return map[string]bool{
		"claude":      hasKey(".claude.json", "mcpServers", "fleet") && hasFleetHook(filepath.Join(".claude", "settings.json"), "claude"),
		"codex":       strings.Contains(string(codexTOMLRaw), "[mcp_servers.fleet]") && hasFleetHook(filepath.Join(".codex", "hooks.json"), "codex"),
		"antigravity": hasKey(filepath.Join(".gemini", "config", "mcp_config.json"), "mcpServers", "fleet") && hasKey(filepath.Join(".gemini", "config", "hooks.json"), "", "fleet") && hasAntigravitySidecar(),
	}
}

// SyncOwnerConfigs refreshes harness configs for owner when `fleet sync` runs as root.
func SyncOwnerConfigs(owner string) ([]string, error) {
	if owner == "" || owner == "root" {
		return nil, nil
	}
	home := paths.HomeForUser(owner)
	if home == "" {
		return nil, fmt.Errorf("lookup user %q failed", owner)
	}
	uid, gid, err := paths.LookupUIDGID(owner)
	if err != nil {
		return nil, err
	}
	return Install(InstallOpts{Home: home, UID: &uid, GID: &gid})
}
