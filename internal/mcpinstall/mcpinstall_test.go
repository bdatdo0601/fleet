package mcpinstall

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

	// Empty 0-byte Antigravity mcp_config.json + existing config.json with unrelated sidecar/settings
	_ = os.MkdirAll(filepath.Join(home, ".gemini", "config"), 0o755)
	_ = os.WriteFile(filepath.Join(home, ".gemini", "config", "mcp_config.json"), nil, 0o644)
	_ = os.WriteFile(filepath.Join(home, ".gemini", "config", "config.json"), []byte(
		`{"plugins":{"swarm":{"enabled":true}},"sidecars":{"swarm/swarm_board":{"projectId":"p1","enabled":true}},"userSettings":{"fastMode":true}}`,
	), 0o644)

	changed, err := Install(InstallOpts{Home: home, BinPath: "/opt/homebrew/bin/fleet"})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 8 {
		t.Fatalf("expected 8 changed files, got %d (%v)", len(changed), changed)
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

	// Verify Antigravity sidecar.json and preserved config.json
	sidecarCfg, _ := loadJSON(filepath.Join(home, ".gemini", "config", "sidecars", "fleet", "sidecar.json"))
	if sidecarCfg["command"] != "/opt/homebrew/bin/fleet" || sidecarCfg["restart_policy"] != "always" {
		t.Fatalf("unexpected sidecar.json: %+v", sidecarCfg)
	}
	agyCfg, _ := loadJSON(filepath.Join(home, ".gemini", "config", "config.json"))
	agySidecars := subMap(agyCfg, "sidecars", false)
	if agySidecars["swarm/swarm_board"] == nil || subMap(agySidecars, "fleet", false)["enabled"] != true || subMap(agyCfg, "userSettings", false)["fastMode"] != true {
		t.Fatalf("config.json lost existing keys or missing fleet sidecar: %+v", agyCfg)
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
	if _, err := os.Stat(filepath.Join(home, ".gemini", "config", "sidecars", "fleet", "sidecar.json")); !os.IsNotExist(err) {
		t.Fatalf("expected sidecar.json removed on uninstall, err=%v", err)
	}
	agyAfterCfg, _ := loadJSON(filepath.Join(home, ".gemini", "config", "config.json"))
	agyAfterSidecars := subMap(agyAfterCfg, "sidecars", false)
	if agyAfterSidecars["fleet"] != nil || agyAfterSidecars["swarm/swarm_board"] == nil {
		t.Fatalf("unexpected antigravity config.json after uninstall: %+v", agyAfterCfg)
	}
}

func TestParsePSMCPServers(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	binMod := time.Date(2026, 11, 24, 15, 50, 0, 0, loc)
	psOutput := strings.Join([]string{
		"58623 Mon Nov 23 15:13:46 2026 /opt/homebrew/Python -m fleetctl mcp serve --harness antigravity",
		"60001 Tue Nov 24 14:00:00 2026 /opt/homebrew/bin/fleet mcp serve --harness claude",
		"60002 Tue Nov 24 16:00:00 2026 /opt/homebrew/bin/fleet mcp serve --harness codex",
		"60003 Tue Nov  3 09:05:00 2026 /usr/local/libexec/fleet/fleet.prev mcp serve --harness=antigravity",
		"60004 Tue Nov 24 10:00:00 2026 /usr/local/bin/other-tool mcp serve --harness antigravity",
		"60005 Tue Nov 24 10:00:00 2026 /opt/homebrew/bin/fleet mcp serve --harness claude",
	}, "\n")

	got := parsePSMCPServers(psOutput, binMod, 60005, loc)
	if len(got) != 3 {
		t.Fatalf("expected 3 stale servers, got %d: %+v", len(got), got)
	}
	if got[0].Harness != "antigravity" || got[0].PID != 58623 || !got[0].Legacy {
		t.Fatalf("unexpected entry 0: %+v", got[0])
	}
	if got[1].Harness != "antigravity" || got[1].PID != 60003 || got[1].Legacy {
		t.Fatalf("unexpected entry 1: %+v", got[1])
	}
	if got[2].Harness != "claude" || got[2].PID != 60001 || got[2].Legacy {
		t.Fatalf("unexpected entry 2: %+v", got[2])
	}
	if w := FormatStaleWarning(got[0]); !strings.Contains(w, "pid 58623") || !strings.Contains(w, "legacy Python fleetctl") || !strings.Contains(w, "antigravity") {
		t.Fatalf("unexpected warning string: %q", w)
	}
}
