package mcpinstall

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdatdo0601/fleet/internal/skills"
)

func TestInstallPreservesExistingHandlesEmptyJSONAndUninstalls(t *testing.T) {
	home := t.TempDir()

	// Existing Claude configs (including legacy fleet MCP + custom statusLine)
	_ = os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"fleet":{"command":"old"},"other":{"command":"x"}}}`), 0o644)
	_ = os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	_ = os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(
		`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"existing.sh"}]}]},"statusLine":{"type":"command","command":"node \"/Users/x/.claude/hooks/gsd-statusline.js\""}}`,
	), 0o644)

	// Existing Codex configs (including legacy [mcp_servers.fleet])
	_ = os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	_ = os.WriteFile(filepath.Join(home, ".codex", "hooks.json"), []byte(
		`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"first.sh"}]}]}}`,
	), 0o644)
	_ = os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(
		"[features]\njs_repl = false\n\n"+
			"[hooks.state.\"/Users/x/.codex/hooks.json:session_start:0:0\"]\n"+
			"enabled = true\ntrusted_hash = \"sha256:abc\"\n\n"+
			"[projects.\"/Users/x/repo\"]\ntrust_level = \"trusted\"\n\n"+
			"[mcp_servers.fleet]\ncommand = \"/opt/homebrew/bin/fleet\"\nargs = [\"mcp\", \"serve\", \"--harness\", \"codex\"]\n",
	), 0o644)

	// Antigravity mcp_config.json with legacy fleet MCP + other server, plus existing config.json
	_ = os.MkdirAll(filepath.Join(home, ".gemini", "config"), 0o755)
	_ = os.WriteFile(filepath.Join(home, ".gemini", "config", "mcp_config.json"), []byte(`{"mcpServers":{"fleet":{"command":"old"},"other":{"command":"y"}}}`), 0o644)
	_ = os.WriteFile(filepath.Join(home, ".gemini", "config", "config.json"), []byte(
		`{"plugins":{"swarm":{"enabled":true}},"sidecars":{"swarm/swarm_board":{"projectId":"p1","enabled":true}},"userSettings":{"fastMode":true}}`,
	), 0o644)

	changed, err := Install(InstallOpts{Home: home, BinPath: "/opt/homebrew/bin/fleet"})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) == 0 {
		t.Fatalf("expected changed files, got none")
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
	for _, skillDir := range []string{
		filepath.Join(home, ".claude", "skills"),
		filepath.Join(home, ".codex", "skills"),
		filepath.Join(home, ".gemini", "config", "skills"),
	} {
		for _, skillName := range []string{"fleet-agents", "fleet-memory", "fleet-slack"} {
			if _, err := os.Stat(filepath.Join(skillDir, skillName, "SKILL.md")); err != nil {
				t.Fatalf("missing installed skill %s/%s: %v", skillDir, skillName, err)
			}
		}
	}

	// Verify legacy fleet MCP entries were removed while preserving other servers.
	clInstallData, _ := loadJSON(filepath.Join(home, ".claude.json"))
	clMCP := subMap(clInstallData, "mcpServers", false)
	if clMCP["fleet"] != nil || clMCP["other"] == nil {
		t.Fatalf("unexpected claude mcpServers after install: %+v", clMCP)
	}
	agyMCPData, _ := loadJSON(filepath.Join(home, ".gemini", "config", "mcp_config.json"))
	agyMCP := subMap(agyMCPData, "mcpServers", false)
	if agyMCP["fleet"] != nil || agyMCP["other"] == nil {
		t.Fatalf("unexpected antigravity mcp_config.json after install: %+v", agyMCP)
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

	// Verify order preservation and crossSessionInbound default
	clSet, _ := loadJSON(filepath.Join(home, ".claude", "settings.json"))
	clSS := clSet["hooks"].(map[string]any)["SessionStart"].([]any)
	if clSS[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"] != "existing.sh" {
		t.Fatalf("existing claude hook not preserved at index 0")
	}
	if clSet["crossSessionInbound"] != "accept" {
		t.Fatalf("expected crossSessionInbound=accept default, got %v", clSet["crossSessionInbound"])
	}
	clSL, _ := clSet["statusLine"].(map[string]any)
	if cmd, _ := clSL["command"].(string); !strings.Contains(cmd, "/opt/homebrew/bin/fleet statusline claude --wrap ") || !strings.Contains(cmd, "gsd-statusline.js") {
		t.Fatalf("expected wrapped statusLine command, got %+v", clSL)
	}

	cxTOMLBytes, _ := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	cxTOML := string(cxTOMLBytes)
	if !strings.Contains(cxTOML, "hooks = true") || !strings.Contains(cxTOML, "js_repl = false") ||
		!strings.Contains(cxTOML, `trusted_hash = "sha256:abc"`) || strings.Contains(cxTOML, "[mcp_servers.fleet]") {
		t.Fatalf("unexpected codex config.toml:\n%s", cxTOML)
	}

	// Uninstall cleans fleet entries and skills cleanly
	if _, err := Uninstall(home); err != nil {
		t.Fatal(err)
	}
	stAfter := Status(home)
	if stAfter["claude"] || stAfter["codex"] || stAfter["antigravity"] {
		t.Fatalf("expected all uninstalled, got %+v", stAfter)
	}
	for _, skillDir := range []string{
		filepath.Join(home, ".claude", "skills", "fleet-agents"),
		filepath.Join(home, ".codex", "skills", "fleet-memory"),
		filepath.Join(home, ".gemini", "config", "skills", "fleet-slack"),
	} {
		if _, err := os.Stat(skillDir); !os.IsNotExist(err) {
			t.Fatalf("expected skill dir %s removed on uninstall, err=%v", skillDir, err)
		}
	}
	clAfterSet, _ := loadJSON(filepath.Join(home, ".claude", "settings.json"))
	clAfterSL, _ := clAfterSet["statusLine"].(map[string]any)
	if clAfterSL["command"] != `node "/Users/x/.claude/hooks/gsd-statusline.js"` {
		t.Fatalf("expected original statusLine command restored on uninstall, got %+v", clAfterSL)
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

	// Explicit user crossSessionInbound setting is preserved across Install.
	_ = os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"crossSessionInbound":"refuse"}`), 0o644)
	if _, err := Install(InstallOpts{Home: home, BinPath: "/opt/homebrew/bin/fleet"}); err != nil {
		t.Fatal(err)
	}
	clSetRefuse, _ := loadJSON(filepath.Join(home, ".claude", "settings.json"))
	if clSetRefuse["crossSessionInbound"] != "refuse" {
		t.Fatalf("expected explicit crossSessionInbound=refuse preserved, got %v", clSetRefuse["crossSessionInbound"])
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

func TestSyncDynamicSkillsWritesAndPrunes(t *testing.T) {
	home := t.TempDir()
	// Install baseline skills first.
	if _, err := Install(InstallOpts{Home: home, BinPath: "/opt/homebrew/bin/fleet"}); err != nil {
		t.Fatal(err)
	}
	changed, err := syncDynamicSkillsInHome(home, nil, nil, []skills.Skill{
		{Name: "daily-brief", Content: []byte("---\nname: daily-brief\n---\nHello\n")},
		{Name: "fleet-agents", Content: []byte("should not overwrite builtin")},
	})
	if err != nil || len(changed) == 0 {
		t.Fatalf("syncDynamicSkillsInHome failed: changed=%v err=%v", changed, err)
	}
	for _, p := range []string{
		filepath.Join(home, ".claude", "skills", "daily-brief", "SKILL.md"),
		filepath.Join(home, ".codex", "skills", "daily-brief", "SKILL.md"),
		filepath.Join(home, ".gemini", "config", "skills", "daily-brief", "SKILL.md"),
	} {
		if b, rErr := os.ReadFile(p); rErr != nil || !strings.Contains(string(b), "Hello") {
			t.Fatalf("expected synced skill at %s, got %q (%v)", p, string(b), rErr)
		}
	}
	// Built-in fleet-agents skill must not be overwritten.
	if b, _ := os.ReadFile(filepath.Join(home, ".claude", "skills", "fleet-agents", "SKILL.md")); strings.Contains(string(b), "should not overwrite") {
		t.Fatalf("builtin fleet-agents skill was overwritten")
	}

	// Removing daily-brief from dynamic list prunes it cleanly.
	if _, err := syncDynamicSkillsInHome(home, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "daily-brief")); !os.IsNotExist(err) {
		t.Fatalf("expected pruned daily-brief skill dir, err=%v", err)
	}
}
