// Package mcpinstall manages idempotent skill, lifecycle hook, and sidecar configs for claude, codex, and antigravity.
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
	"github.com/bdatdo0601/fleet/internal/skills"
)

const (
	hookMarker       = "agent-hook --harness "
	statusLineMarker = " statusline claude"
)

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

func updateCodexTOML(raw string, remove bool) string {
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
	for len(cleaned) > 0 && strings.TrimSpace(cleaned[len(cleaned)-1]) == "" {
		cleaned = cleaned[:len(cleaned)-1]
	}
	if len(cleaned) == 0 {
		return ""
	}
	return strings.Join(cleaned, "\n") + "\n"
}

func skillDirsForHarnesses(home string, harnesses []string) []string {
	var dirs []string
	if slices.Contains(harnesses, "claude") {
		dirs = append(dirs, filepath.Join(home, ".claude", "skills"))
	}
	if slices.Contains(harnesses, "codex") {
		dirs = append(dirs, filepath.Join(home, ".codex", "skills"))
	}
	if slices.Contains(harnesses, "antigravity") {
		dirs = append(dirs, filepath.Join(home, ".gemini", "config", "skills"))
	}
	return dirs
}

func installSkills(dirs []string, uid, gid *int) ([]string, error) {
	all := skills.All()
	var changed []string
	for _, base := range dirs {
		for _, sk := range all {
			p := filepath.Join(base, sk.Name, "SKILL.md")
			wrote, err := paths.WriteFileAtomic(p, sk.Content, 0o644, uid, gid)
			if err != nil {
				return nil, err
			}
			if wrote {
				changed = append(changed, p)
			}
		}
	}
	return changed, nil
}

func removeSkills(dirs []string) []string {
	var removed []string
	for _, base := range dirs {
		for _, name := range skills.Names {
			dir := filepath.Join(base, name)
			if pathExists(dir) && os.RemoveAll(dir) == nil {
				removed = append(removed, dir)
			}
		}
	}
	return removed
}

func hasAllSkills(skillsDir string) bool {
	for _, name := range skills.Names {
		if !pathExists(filepath.Join(skillsDir, name, "SKILL.md")) {
			return false
		}
	}
	return true
}

func codexHooksEnabled(raw string) bool {
	return tomlHooksRE.MatchString(raw) || strings.Contains(raw, "hooks = true")
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

// Install idempotently writes Fleet skill, lifecycle hook, and sidecar configs for detected harnesses,
// and removes legacy global `fleet` MCP server entries to avoid context bloat.
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
	var createSpecs, cleanupSpecs []jsonEditSpec

	if slices.Contains(harnesses, "claude") {
		cleanupSpecs = append(cleanupSpecs, jsonEditSpec{".claude.json", func(d map[string]any) bool {
			return deleteFleetKey(d, "mcpServers")
		}})
		createSpecs = append(createSpecs, jsonEditSpec{filepath.Join(".claude", "settings.json"), func(d map[string]any) bool {
			h := subMap(d, "hooks", true)
			if h == nil {
				return false
			}
			upsertClaudeStyleHooks(h, "claude", cmdBin)
			wrapClaudeStatusLine(d, cmdBin)
			if _, exists := d["crossSessionInbound"]; !exists {
				d["crossSessionInbound"] = "accept"
			}
			return true
		}})
	}

	var changed []string
	if slices.Contains(harnesses, "codex") {
		codexTOML := filepath.Join(targetHome, ".codex", "config.toml")
		rawTOML, _ := os.ReadFile(codexTOML)
		wrote, err := writeUserFile(codexTOML, updateCodexTOML(string(rawTOML), false), opts.UID, opts.GID)
		if err != nil {
			return nil, err
		}
		if wrote {
			changed = append(changed, codexTOML)
		}
		createSpecs = append(createSpecs, jsonEditSpec{filepath.Join(".codex", "hooks.json"), func(d map[string]any) bool {
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
		cleanupSpecs = append(cleanupSpecs, jsonEditSpec{filepath.Join(".gemini", "config", "mcp_config.json"), func(d map[string]any) bool {
			return deleteFleetKey(d, "mcpServers")
		}})
		createSpecs = append(createSpecs,
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

	cleaned, err := applyEdits(targetHome, opts.UID, opts.GID, false, cleanupSpecs)
	if err != nil {
		return nil, err
	}
	edited, err := applyEdits(targetHome, opts.UID, opts.GID, true, createSpecs)
	if err != nil {
		return nil, err
	}
	skillFiles, err := installSkills(skillDirsForHarnesses(targetHome, harnesses), opts.UID, opts.GID)
	if err != nil {
		return nil, err
	}
	changed = append(changed, cleaned...)
	changed = append(changed, edited...)
	return append(changed, skillFiles...), nil
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func shellUnquote(s string) string {
	t := strings.TrimSpace(s)
	if len(t) >= 2 && t[0] == '\'' && t[len(t)-1] == '\'' {
		return strings.ReplaceAll(t[1:len(t)-1], `'\''`, "'")
	}
	if len(t) >= 2 && t[0] == '"' && t[len(t)-1] == '"' {
		var out string
		if json.Unmarshal([]byte(t), &out) == nil {
			return out
		}
	}
	return t
}

func wrapClaudeStatusLine(d map[string]any, cmdBin string) {
	sl, hasSL := d["statusLine"].(map[string]any)
	if !hasSL {
		if _, exists := d["statusLine"]; exists {
			return
		}
		d["statusLine"] = map[string]any{
			"type":    "command",
			"command": cmdBin + statusLineMarker,
		}
		return
	}
	if _, hasType := sl["type"]; !hasType {
		sl["type"] = "command"
	}
	origCmd, _ := sl["command"].(string)
	if strings.Contains(origCmd, statusLineMarker) {
		if _, tail, ok := strings.Cut(origCmd, statusLineMarker); ok {
			sl["command"] = cmdBin + statusLineMarker + tail
		}
		return
	}
	if strings.TrimSpace(origCmd) != "" {
		sl["command"] = cmdBin + statusLineMarker + " --wrap " + shellQuote(origCmd)
	} else {
		sl["command"] = cmdBin + statusLineMarker
	}
}

func unwrapClaudeStatusLine(d map[string]any) bool {
	sl, ok := d["statusLine"].(map[string]any)
	if !ok {
		return false
	}
	cmd, _ := sl["command"].(string)
	if !strings.Contains(cmd, statusLineMarker) {
		return false
	}
	_, tail, _ := strings.Cut(cmd, statusLineMarker)
	tail = strings.TrimSpace(tail)
	if rest, hasWrap := strings.CutPrefix(tail, "--wrap "); hasWrap {
		sl["command"] = shellUnquote(rest)
		return true
	}
	delete(d, "statusLine")
	return true
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

// Uninstall removes fleet skills, MCP, and hook entries from all harness configs under home.
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
		if wrote, err := writeUserFile(codexTOML, updateCodexTOML(string(raw), true), nil, nil); err != nil {
			return nil, err
		} else if wrote {
			changed = append(changed, codexTOML)
		}
	}
	edited, err := applyEdits(targetHome, nil, nil, false, []jsonEditSpec{
		{".claude.json", func(d map[string]any) bool { return deleteFleetKey(d, "mcpServers") }},
		{filepath.Join(".claude", "settings.json"), func(d map[string]any) bool {
			changed := false
			if h := subMap(d, "hooks", false); h != nil && removeClaudeStyleHooks(h, "claude") {
				changed = true
			}
			if unwrapClaudeStatusLine(d) {
				changed = true
			}
			return changed
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
	changed = append(changed, edited...)
	return append(changed, removeSkills(skillDirsForHarnesses(targetHome, paths.Harnesses))...), nil
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
		"claude":      hasFleetHook(filepath.Join(".claude", "settings.json"), "claude") && hasAllSkills(filepath.Join(targetHome, ".claude", "skills")),
		"codex":       codexHooksEnabled(string(codexTOMLRaw)) && hasFleetHook(filepath.Join(".codex", "hooks.json"), "codex") && hasAllSkills(filepath.Join(targetHome, ".codex", "skills")),
		"antigravity": hasKey(filepath.Join(".gemini", "config", "hooks.json"), "", "fleet") && hasAntigravitySidecar() && hasAllSkills(filepath.Join(targetHome, ".gemini", "config", "skills")),
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
