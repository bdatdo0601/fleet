package mcpinstall

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallPreservesExistingHandlesEmptyJSONAndUninstalls(t *testing.T) {
	home := t.TempDir()

	// Existing Claude configs
	_ = os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"other":{"command":"x"}}}`), 0o644)
	_ = os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	_ = os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(
		`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"existing.sh"}]}]}}`,
	), 0o644)

	// Existing Codex configs
	_ = os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	_ = os.WriteFile(filepath.Join(home, ".codex", "hooks.json"), []byte(
		`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"first.sh"}]}]}}`,
	), 0o644)
	_ = os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(
		"[features]\njs_repl = false\n\n"+
			"[hooks.state.\"/Users/x/.codex/hooks.json:session_start:0:0\"]\n"+
			"enabled = true\ntrusted_hash = \"sha256:abc\"\n\n"+
			"[projects.\"/Users/x/repo\"]\ntrust_level = \"trusted\"\n",
	), 0o644)

	// Empty 0-byte Antigravity mcp_config.json
	_ = os.MkdirAll(filepath.Join(home, ".gemini", "config"), 0o755)
	_ = os.WriteFile(filepath.Join(home, ".gemini", "config", "mcp_config.json"), nil, 0o644)

	changed, err := Install(InstallOpts{Home: home, BinPath: "/opt/homebrew/bin/fleet"})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 6 {
		t.Fatalf("expected 6 changed files, got %d (%v)", len(changed), changed)
	}
	// Idempotent second run
	changed2, err := Install(InstallOpts{Home: home, BinPath: "/opt/homebrew/bin/fleet"})
	if err != nil || len(changed2) != 0 {
		t.Fatalf("expected idempotent second install, got %v, err=%v", changed2, err)
	}
	st := Status(home)
	if !st["claude"] || !st["codex"] || !st["antigravity"] {
		t.Fatalf("unexpected status: %+v", st)
	}

	// Verify order preservation
	clSet, _ := loadJSON(filepath.Join(home, ".claude", "settings.json"))
	clSS := clSet["hooks"].(map[string]any)["SessionStart"].([]any)
	if clSS[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"] != "existing.sh" {
		t.Fatalf("existing claude hook not preserved at index 0")
	}

	cxTOMLBytes, _ := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	cxTOML := string(cxTOMLBytes)
	if !strings.Contains(cxTOML, "hooks = true") || !strings.Contains(cxTOML, "js_repl = false") ||
		!strings.Contains(cxTOML, `trusted_hash = "sha256:abc"`) || !strings.Contains(cxTOML, "[mcp_servers.fleet]") {
		t.Fatalf("unexpected codex config.toml:\n%s", cxTOML)
	}

	// Uninstall cleans fleet entries cleanly
	if _, err := Uninstall(home); err != nil {
		t.Fatal(err)
	}
	stAfter := Status(home)
	if stAfter["claude"] || stAfter["codex"] || stAfter["antigravity"] {
		t.Fatalf("expected all uninstalled, got %+v", stAfter)
	}
	clData, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	var clParsed map[string]any
	_ = json.Unmarshal(clData, &clParsed)
	if _, ok := clParsed["mcpServers"].(map[string]any)["other"]; !ok {
		t.Fatalf("other MCP server was deleted")
	}
	cxAfterTOML, _ := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if strings.Contains(string(cxAfterTOML), "[mcp_servers.fleet]") || !strings.Contains(string(cxAfterTOML), `trusted_hash = "sha256:abc"`) {
		t.Fatalf("unexpected codex config.toml after uninstall:\n%s", string(cxAfterTOML))
	}
}
