package usage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bdatdo0601/fleet/internal/agents"
	_ "modernc.org/sqlite"
)

func TestCollectCodexParsesRateLimitsAndUsage(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("skipped when running as root")
	}
	dir := t.TempDir()
	fakeCodex := filepath.Join(dir, "codex")
	script := `#!/usr/bin/env python3
import json, sys
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    mid = msg.get("id")
    m = msg.get("method")
    if m == "initialize":
        print(json.dumps({"id": mid, "result": {"userAgent": "codex-fake"}}), flush=True)
    elif m == "account/rateLimits/read":
        print(json.dumps({"id": mid, "result": {
            "ordinaryUsageAllowed": True,
            "rateLimits": {
                "planType": "pro",
                "primary": {"usedPercent": 12, "windowDurationMins": 10080, "resetsAt": 1796249386},
                "secondary": {"usedPercent": 4, "windowDurationMins": 300, "resetsAt": 1796000000},
                "credits": {"hasCredits": True, "unlimited": False, "balance": "15.50"}
            }
        }}), flush=True)
    elif m == "account/usage/read":
        print(json.dumps({"id": mid, "result": {
            "summary": {"lifetimeTokens": 987654, "peakDailyTokens": 123456, "currentStreakDays": 7}
        }}), flush=True)
`
	if err := os.WriteFile(fakeCodex, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	got := CollectCodex(context.Background(), fakeCodex)
	if got.Status != "ok" || got.Harness != "codex" || got.Plan != "pro" || got.Source != "codex-app-server" {
		t.Fatalf("unexpected codex usage: %+v", got)
	}
	if len(got.Windows) != 2 || got.Windows[0].Label != "5h" || got.Windows[0].UsedPercent != 4 || got.Windows[1].Label != "7d" || got.Windows[1].UsedPercent != 12 {
		t.Fatalf("unexpected codex windows: %+v", got.Windows)
	}
	if got.Metrics["lifetime_tokens"] != int64(987654) || got.Metrics["credits_balance"] != "15.50" || got.Metrics["current_streak_days"] != int64(7) {
		t.Fatalf("unexpected codex metrics: %+v", got.Metrics)
	}
}

func TestHandleClaudeStatusLineWrapCacheAndPush(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var pushed []agents.HarnessUsage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/usage" && r.Method == http.MethodPost {
			var u agents.HarnessUsage
			if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			pushed = append(pushed, u)
			_, _ = w.Write([]byte(`{"usage":[]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	wrapScript := filepath.Join(home, "status.sh")
	if err := os.WriteFile(wrapScript, []byte("#!/bin/sh\ncat >/dev/null\necho \"wrapped-line-ok\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	inputJSON := `{
		"model": {"id": "claude-opus", "display_name": "Claude Opus"},
		"cost": {"total_cost_usd": 1.25},
		"context_window": {"used_percentage": 42.5},
		"rate_limits": {
			"five_hour": {"used_percentage": 23.5, "resets_at": 1796000000},
			"seven_day": {"used_percentage": 61.0, "resets_at": "2026-12-01T00:00:00Z"}
		}
	}`

	var out, errBuf bytes.Buffer
	if err := HandleClaudeStatusLine(strings.NewReader(inputJSON), &out, &errBuf, wrapScript, home, srv.URL); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "wrapped-line-ok" {
		t.Fatalf("expected wrapped stdout preserved, got %q", out.String())
	}
	if len(pushed) != 1 || len(pushed[0].Windows) != 2 || pushed[0].Windows[0].Label != "5h" || pushed[0].Windows[0].UsedPercent != 23.5 {
		t.Fatalf("unexpected pushed usage: %+v", pushed)
	}

	// Second call with identical percentages within 30s should debounce POST, while still running wrapped output.
	out.Reset()
	if err := HandleClaudeStatusLine(strings.NewReader(inputJSON), &out, &errBuf, wrapScript, home, srv.URL); err != nil {
		t.Fatal(err)
	}
	if len(pushed) != 1 {
		t.Fatalf("expected second identical statusline call to debounce push, got %d pushes", len(pushed))
	}

	collected := CollectClaude(home)
	if collected.Status != "ok" || len(collected.Windows) != 2 || collected.Metrics["model"] != "Claude Opus" {
		t.Fatalf("unexpected CollectClaude result: %+v", collected)
	}
}

func TestCollectAntigravityLocalDBAndOptionalCLI(t *testing.T) {
	home := t.TempDir()
	dbPath := filepath.Join(home, ".gemini", "antigravity", "conversation_summaries.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE conversation_summaries (
			conversation_id TEXT PRIMARY KEY,
			last_modified_time TEXT NOT NULL,
			step_count INTEGER NOT NULL DEFAULT 0,
			not_fully_idle INTEGER NOT NULL DEFAULT 0
		);
		INSERT INTO conversation_summaries VALUES ('c1', datetime('now'), 12, 1);
		INSERT INTO conversation_summaries VALUES ('c2', datetime('now', '-3 days'), 8, 0);
	`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	t.Setenv("FLEET_AGY_USAGE_BIN", filepath.Join(home, "missing-agy-usage"))
	localRes := CollectAntigravity(context.Background(), home)
	if localRes.Status != "ok" || localRes.Source != "antigravity-local" || localRes.Metrics["conversations_total"] != 2 || localRes.Metrics["conversations_24h"] != 1 || localRes.Metrics["steps_24h"] != 12 {
		t.Fatalf("unexpected antigravity local usage: %+v", localRes)
	}

	fakeCLI := filepath.Join(home, "antigravity-usage")
	if err := os.WriteFile(fakeCLI, []byte(`#!/bin/sh
echo '{"plan":"pro","quotas":[{"model":"gemini-4-neon","remainingPercent":80,"resetTime":"2026-11-26T00:00:00Z"}]}'
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_AGY_USAGE_BIN", fakeCLI)
	cliRes := CollectAntigravity(context.Background(), home)
	if cliRes.Status != "ok" || cliRes.Source != "antigravity-usage-cli" || cliRes.Plan != "pro" || len(cliRes.Windows) != 1 || cliRes.Windows[0].UsedPercent != 20 {
		t.Fatalf("unexpected antigravity CLI usage: %+v", cliRes)
	}
}
