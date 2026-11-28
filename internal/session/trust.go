package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/bdatdo0601/fleet/internal/paths"
)

func resolveSessionHome() string {
	if h := strings.TrimSpace(os.Getenv("HOME")); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

// EnsureDirectoryTrusted pre-populates Claude Code (~/.claude.json), Codex (~/.codex/config.toml),
// or Antigravity CLI (~/.gemini/antigravity-cli/settings.json) workspace trust entries for directory
// so newly spawned sessions skip interactive trust prompts.
func EnsureDirectoryTrusted(harness, directory string) error {
	dir := strings.TrimSpace(directory)
	if dir == "" {
		return nil
	}
	norm, ok := NormalizeHarness(harness)
	if !ok {
		return nil
	}
	switch norm {
	case HarnessClaude:
		return ensureClaudeTrusted(dir)
	case HarnessCodex:
		return ensureCodexTrusted(dir)
	case HarnessAgy:
		return ensureAgyTrusted(dir)
	default:
		return nil
	}
}

func claudeConfigFilePath() string {
	if cfgDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); cfgDir != "" {
		return filepath.Join(cfgDir, ".claude.json")
	}
	return filepath.Join(resolveSessionHome(), ".claude.json")
}

func ensureClaudeTrusted(dir string) error {
	cfgPath := claudeConfigFilePath()
	var root map[string]any
	raw, err := os.ReadFile(cfgPath)
	switch {
	case err != nil:
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		root = map[string]any{}
	case len(bytes.TrimSpace(raw)) == 0:
		root = map[string]any{}
	default:
		if err := json.Unmarshal(raw, &root); err != nil {
			return fmt.Errorf("parse %s: %w", cfgPath, err)
		}
		if root == nil {
			root = map[string]any{}
		}
	}

	var projects map[string]any
	if rawProjects, exists := root["projects"]; !exists || rawProjects == nil {
		projects = map[string]any{}
	} else {
		var ok bool
		projects, ok = rawProjects.(map[string]any)
		if !ok {
			return fmt.Errorf("unexpected projects field type in %s", cfgPath)
		}
	}

	var proj map[string]any
	if rawProj, exists := projects[dir]; !exists || rawProj == nil {
		proj = map[string]any{}
	} else {
		var ok bool
		proj, ok = rawProj.(map[string]any)
		if !ok {
			return fmt.Errorf("unexpected project entry type for %q in %s", dir, cfgPath)
		}
	}

	if trusted, _ := proj["hasTrustDialogAccepted"].(bool); trusted {
		return nil
	}
	proj["hasTrustDialogAccepted"] = true
	projects[dir] = proj
	root["projects"] = projects

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return err
	}
	_, err = paths.WriteFileAtomic(cfgPath, out, 0o600, nil, nil)
	return err
}

func codexConfigFilePath() string {
	if codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME")); codexHome != "" {
		return filepath.Join(codexHome, "config.toml")
	}
	return filepath.Join(resolveSessionHome(), ".codex", "config.toml")
}

func ensureCodexTrusted(dir string) error {
	cfgPath := codexConfigFilePath()
	raw, err := os.ReadFile(cfgPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	content := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(content, "\n")
	header := fmt.Sprintf("[projects.%s]", strconv.Quote(dir))

	headerIdx := -1
	for i, ln := range lines {
		if strings.TrimSpace(ln) == header {
			headerIdx = i
			break
		}
	}

	if headerIdx >= 0 {
		endIdx := len(lines)
		for i := headerIdx + 1; i < len(lines); i++ {
			if strings.HasPrefix(strings.TrimSpace(lines[i]), "[") {
				endIdx = i
				break
			}
		}
		updated := false
		for i := headerIdx + 1; i < endIdx; i++ {
			trimmed := strings.TrimSpace(lines[i])
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if k, v, ok := strings.Cut(trimmed, "="); ok && strings.TrimSpace(k) == "trust_level" {
				if strings.TrimSpace(v) == `"trusted"` {
					return nil
				}
				lines[i] = `trust_level = "trusted"`
				updated = true
				break
			}
		}
		if !updated {
			lines = slices.Insert(lines, headerIdx+1, `trust_level = "trusted"`)
		}
	} else {
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, header, `trust_level = "trusted"`)
	}

	out := strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return err
	}
	_, err = paths.WriteFileAtomic(cfgPath, []byte(out), 0o600, nil, nil)
	return err
}

func agyCLISettingsFilePath() string {
	if cfgDir := strings.TrimSpace(os.Getenv("FLEET_AGY_CLI_DIR")); cfgDir != "" {
		return filepath.Join(cfgDir, "settings.json")
	}
	return filepath.Join(resolveSessionHome(), ".gemini", "antigravity-cli", "settings.json")
}

func ensureAgyTrusted(dir string) error {
	cfgPath := agyCLISettingsFilePath()
	var root map[string]any
	raw, err := os.ReadFile(cfgPath)
	switch {
	case err != nil:
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		root = map[string]any{}
	case len(bytes.TrimSpace(raw)) == 0:
		root = map[string]any{}
	default:
		if err := json.Unmarshal(raw, &root); err != nil {
			return fmt.Errorf("parse %s: %w", cfgPath, err)
		}
		if root == nil {
			root = map[string]any{}
		}
	}

	var workspaces []any
	if rawWS, exists := root["trustedWorkspaces"]; !exists || rawWS == nil {
		workspaces = []any{}
	} else {
		var ok bool
		workspaces, ok = rawWS.([]any)
		if !ok {
			return fmt.Errorf("unexpected trustedWorkspaces field type in %s", cfgPath)
		}
		for _, item := range workspaces {
			if s, ok := item.(string); ok && s == dir {
				return nil
			}
		}
	}

	workspaces = append(workspaces, dir)
	root["trustedWorkspaces"] = workspaces

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return err
	}
	_, err = paths.WriteFileAtomic(cfgPath, out, 0o600, nil, nil)
	return err
}
