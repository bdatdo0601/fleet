package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIMemoryWorkflowCuratorSetupAndImport(t *testing.T) {
	f, _, tmp := setupCLIServer(t)
	home := filepath.Join(tmp, "home")
	cacheDir := filepath.Join(tmp, "cache")
	t.Setenv("HOME", home)
	t.Setenv("FLEET_CACHE_DIR", cacheDir)

	runCLI := func(opRef string, args ...string) (int, string, string) {
		var out, errBuf bytes.Buffer
		rc := Run(args, &IO{
			Stdin:  strings.NewReader(""),
			Stdout: &out,
			Stderr: &errBuf,
			ReadOPRef: func(ref string) (string, error) {
				if ref != "op://Private/OpenRouter/api_key" {
					t.Fatalf("unexpected op ref %q", ref)
				}
				return opRef, nil
			},
		})
		return rc, out.String(), errBuf.String()
	}

	// Mock OpenRouter endpoint.
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		content := `{"ok":true}`
		userMsg := req["messages"].([]any)[1].(map[string]any)["content"].(string)
		if strings.Contains(userMsg, "CANDIDATE:") {
			content = `{"action":"promote_global","target_key":"prefer-few-prs","scope":"global","kind":"feedback","tier":"always","summary":"Prefer minimal PRs","body":"Plan fewest PRs possible.","reason":"Cross-fleet workflow preference"}`
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": content}}},
		})
	}))
	defer llmSrv.Close()

	// 1. Setup curator via 1Password op-ref + openrouter.
	rc, out, errOut := runCLI("sk-or-v1-testsecret4321",
		"memory", "curator", "setup",
		"--provider", "openrouter",
		"--url", llmSrv.URL,
		"--model", "openai/gpt-6-luna",
		"--op-ref", "op://Private/OpenRouter/api_key",
		"--zdr",
	)
	if rc != 0 || !strings.Contains(out, "sk-or-v1...4321") || strings.Contains(out, "testsecret") {
		t.Fatalf("curator setup failed: rc=%d out=%q err=%q", rc, out, errOut)
	}

	// 2. Import local Claude memory file.
	projDir := filepath.Join(home, ".claude", "projects", "-Users-datdo-workspace-demo-app")
	memDir := filepath.Join(projDir, "memory")
	_ = os.MkdirAll(memDir, 0o755)
	_ = os.WriteFile(filepath.Join(memDir, "prefer-few-prs.md"), []byte(`---
name: prefer-few-prs
description: "Minimal PRs"
metadata:
  type: feedback
---
Plan fewest PRs possible.
`), 0o644)

	rc, out, errOut = runCLI("", "memory", "import")
	if rc != 0 || !strings.Contains(out, "+1 created") {
		t.Fatalf("memory import failed: rc=%d out=%q err=%q", rc, out, errOut)
	}

	// 3. Run curator -> queues global promotion -> review + approve.
	rc, out, errOut = runCLI("", "memory", "curator", "run")
	if rc != 0 || !strings.Contains(out, "promote_global") {
		t.Fatalf("curator run failed: rc=%d out=%q err=%q", rc, out, errOut)
	}
	rc, out, errOut = runCLI("", "memory", "review")
	if rc != 0 || !strings.Contains(out, "prefer-few-prs") {
		t.Fatalf("memory review failed: rc=%d out=%q err=%q", rc, out, errOut)
	}
	rc, out, errOut = runCLI("", "memory", "approve", "1")
	if rc != 0 || !strings.Contains(out, "global/prefer-few-prs") {
		t.Fatalf("memory approve failed: rc=%d out=%q err=%q", rc, out, errOut)
	}

	// 4. Set, get, update, history, revert, promote, search, context, rm.
	rc, out, errOut = runCLI("", "memory", "set", "global/small-changes", "Keep edits small and focused.", "--kind", "preference")
	if rc != 0 || !strings.Contains(out, "created global/small-changes") {
		t.Fatalf("memory set failed: rc=%d out=%q err=%q", rc, out, errOut)
	}
	rc, out, errOut = runCLI("", "memory", "set", "global/small-changes", "Updated body v2.", "--kind", "preference")
	if rc != 0 || !strings.Contains(out, "updated global/small-changes") {
		t.Fatalf("memory set v2 failed: rc=%d out=%q err=%q", rc, out, errOut)
	}
	rc, out, errOut = runCLI("", "memory", "revert", "global/small-changes", "--rev", "1")
	if rc != 0 || !strings.Contains(out, "rev 3") {
		t.Fatalf("memory revert failed: rc=%d out=%q err=%q", rc, out, errOut)
	}
	rc, out, errOut = runCLI("", "memory", "search", "focused")
	if rc != 0 || !strings.Contains(out, "small-changes") {
		t.Fatalf("memory search failed: rc=%d out=%q err=%q", rc, out, errOut)
	}
	rc, out, errOut = runCLI("", "memory", "context", "--repo", "demo-app")
	if rc != 0 || !strings.Contains(out, "[global/small-changes]") || !strings.Contains(out, "[global/prefer-few-prs]") {
		t.Fatalf("memory context failed: rc=%d out=%q err=%q", rc, out, errOut)
	}
	rc, out, errOut = runCLI("", "memory", "promote", "repo:demo-app/prefer-few-prs", "--to", "global/manual-copy", "--tier", "always")
	if rc != 0 || !strings.Contains(out, "global/manual-copy") {
		t.Fatalf("memory promote failed: rc=%d out=%q err=%q", rc, out, errOut)
	}
	rc, out, errOut = runCLI("", "memory", "rm", "global/manual-copy")
	if rc != 0 || !strings.Contains(out, "removed global/manual-copy") {
		t.Fatalf("memory rm failed: rc=%d out=%q err=%q", rc, out, errOut)
	}
	if props, pErr := f.Memory.ListProposals("pending", 10); pErr != nil || len(props) != 0 {
		t.Fatalf("expected 0 pending proposals after approve, got %+v err=%v", props, pErr)
	}
}
