package assistant

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func openObserverTestEngine(t *testing.T, handler http.HandlerFunc) (*Engine, *Vault, *syncBuffer) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	tmp := t.TempDir()
	eng, err := Open(
		filepath.Join(tmp, "assistant.db"),
		filepath.Join(tmp, "assistant.json"),
		filepath.Join(tmp, "vault"),
		nil,
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })

	logBuf := &syncBuffer{}
	eng.observerMu.Lock()
	eng.observerLogW = logBuf
	eng.observerMu.Unlock()

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.BaseURL = srv.URL
	cfg.APIKey = "test-key"
	cfg.GitAutoCommit = false
	if _, err := eng.UpdateConfig(cfg, false); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	v, err := eng.Vault()
	if err != nil {
		t.Fatalf("Vault failed: %v", err)
	}
	return eng, v, logBuf
}

func waitForVaultContent(t *testing.T, v *Vault, rel, substr string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if txt, err := v.ReadFile(rel); err == nil && strings.Contains(txt, substr) {
			return txt
		}
		time.Sleep(15 * time.Millisecond)
	}
	txt, _ := v.ReadFile(rel)
	t.Fatalf("timed out waiting for %q in %s; got:\n%s", substr, rel, txt)
	return ""
}

func TestObserverFlushOnResetClearHistory(t *testing.T) {
	var calls atomic.Int32
	eng, v, _ := openObserverTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "- 🔴 User prefers oat milk in morning coffee."}}},
		})
	})

	if _, err := eng.Store().AppendMessage(Message{
		ChannelID:    "general",
		ContextEpoch: 1,
		Role:         "user",
		Content:      "Remember that I prefer oat milk in my morning coffee.",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Store().AppendMessage(Message{
		ChannelID:    "general",
		ContextEpoch: 1,
		Role:         "assistant",
		Content:      "Noted — I will remember your oat milk preference.",
	}); err != nil {
		t.Fatal(err)
	}

	ch, err := eng.ResetChannel("general", true)
	if err != nil {
		t.Fatalf("ResetChannel(clear=true) failed: %v", err)
	}
	if ch.ContextEpoch != 2 {
		t.Fatalf("expected epoch=2, got %d", ch.ContextEpoch)
	}
	if msgs, err := eng.Store().ListMessages("general", 50); err != nil || len(msgs) != 0 {
		t.Fatalf("expected channel messages cleared immediately, got %d (%v)", len(msgs), err)
	}

	obsText := waitForVaultContent(t, v, "_agent/memory/OBSERVATIONS.md", "oat milk", 5*time.Second)
	if !strings.Contains(obsText, "🔴 User prefers oat milk") {
		t.Fatalf("unexpected OBSERVATIONS.md:\n%s", obsText)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 observer LLM call, got %d", got)
	}
}

func TestObserverFlushOnResetKeepHistoryAndDedupe(t *testing.T) {
	var calls atomic.Int32
	eng, v, _ := openObserverTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "- 🟡 Planning Kyoto trip for October."}}},
		})
	})

	m1, err := eng.Store().AppendMessage(Message{
		ChannelID:    "general",
		ContextEpoch: 1,
		Role:         "user",
		Content:      "We are planning a trip to Kyoto in October.",
	})
	if err != nil {
		t.Fatal(err)
	}
	m2, err := eng.Store().AppendMessage(Message{
		ChannelID:    "general",
		ContextEpoch: 1,
		Role:         "assistant",
		Content:      "Sounds great! I will keep October Kyoto planning in mind.",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := eng.ResetChannel("general", false); err != nil {
		t.Fatalf("ResetChannel(clear=false) failed: %v", err)
	}
	waitForVaultContent(t, v, "_agent/memory/OBSERVATIONS.md", "Kyoto trip", 5*time.Second)

	// Wait until MarkMessagesObserved completes as well.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		unobs, err := eng.Store().ListUnobservedMessages("general", 50)
		if err == nil && len(unobs) == 0 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if unobs, err := eng.Store().ListUnobservedMessages("general", 50); err != nil || len(unobs) != 0 {
		t.Fatalf("expected messages marked observed after keep-history reset, got %d (%v)", len(unobs), err)
	}

	// Re-running observeSnapshot with the same message IDs must deduplicate and make no LLM call.
	if err := eng.observeSnapshot(t.Context(), "general", []Message{m1, m2}); err != nil {
		t.Fatalf("observeSnapshot failed: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 total observer call after dedupe, got %d", got)
	}
}

func TestObserverIdlePass(t *testing.T) {
	var calls atomic.Int32
	eng, v, _ := openObserverTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "- 🟡 Switched primary build machine to Mac mini."}}},
		})
	})

	now := time.Date(2026, 11, 28, 17, 0, 0, 0, time.UTC)
	idleTS := now.Add(-45 * time.Minute).Format(time.RFC3339)
	recentTS := now.Add(-5 * time.Minute).Format(time.RFC3339)

	// #general has an idle conversation (45m old) -> should be observed.
	if _, err := eng.Store().AppendMessage(Message{
		ChannelID: "general", ContextEpoch: 1, Role: "user",
		Content: "Moved our main build pipeline to the Mac mini.", CreatedAt: idleTS,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Store().AppendMessage(Message{
		ChannelID: "general", ContextEpoch: 1, Role: "assistant",
		Content: "Got it, Mac mini is now the primary build host.", CreatedAt: idleTS,
	}); err != nil {
		t.Fatal(err)
	}

	// #fleet-ops has an active conversation (5m old) -> should be skipped.
	if _, err := eng.Store().AppendMessage(Message{
		ChannelID: "fleet-ops", ContextEpoch: 1, Role: "user",
		Content: "Still actively chatting in fleet-ops right now.", CreatedAt: recentTS,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Store().AppendMessage(Message{
		ChannelID: "fleet-ops", ContextEpoch: 1, Role: "assistant",
		Content: "Sure, standing by for the next step.", CreatedAt: recentTS,
	}); err != nil {
		t.Fatal(err)
	}

	// #briefing has only 1 message (even if old) -> should be skipped.
	if _, err := eng.Store().AppendMessage(Message{
		ChannelID: "briefing", ContextEpoch: 1, Role: "user",
		Content: "single ping", CreatedAt: idleTS,
	}); err != nil {
		t.Fatal(err)
	}

	cfg := eng.Config()
	if cfg.ObserverIdleMinutes != DefaultObserverIdleMins {
		t.Fatalf("expected default ObserverIdleMinutes=%d, got %d", DefaultObserverIdleMins, cfg.ObserverIdleMinutes)
	}
	if cfg.ObserverThresholdTokens != DefaultObserveTokens {
		t.Fatalf("expected default ObserverThresholdTokens=%d, got %d", DefaultObserveTokens, cfg.ObserverThresholdTokens)
	}

	// Disabled idle pass does nothing.
	disabledCfg := cfg
	disabledCfg.ObserverIdleMinutes = 0
	eng.runIdleObservePass(t.Context(), disabledCfg, now)
	if got := calls.Load(); got != 0 {
		t.Fatalf("expected 0 observer calls when idle disabled, got %d", got)
	}

	// Enabled idle pass observes #general only.
	eng.runIdleObservePass(t.Context(), cfg, now)
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 observer LLM call, got %d", got)
	}
	obsText, err := v.ReadFile("_agent/memory/OBSERVATIONS.md")
	if err != nil || !strings.Contains(obsText, "Mac mini") {
		t.Fatalf("expected idle observation written to OBSERVATIONS.md, got %q (%v)", obsText, err)
	}
	if unobs, err := eng.Store().ListUnobservedMessages("general", 50); err != nil || len(unobs) != 0 {
		t.Fatalf("expected #general unobserved=0, got %d (%v)", len(unobs), err)
	}
	if unobs, err := eng.Store().ListUnobservedMessages("fleet-ops", 50); err != nil || len(unobs) != 2 {
		t.Fatalf("expected #fleet-ops unobserved=2, got %d (%v)", len(unobs), err)
	}
}

func TestObserverLogsErrorsAndAppliesBackoff(t *testing.T) {
	var calls atomic.Int32
	eng, _, logBuf := openObserverTestEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":"upstream timeout"}`, http.StatusBadGateway)
	})

	cfg := eng.Config()
	cfg.ObserverThresholdTokens = 50
	if _, err := eng.UpdateConfig(cfg, false); err != nil {
		t.Fatal(err)
	}
	cfg = eng.Config()

	oldTS := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	for i := range 6 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		if _, err := eng.Store().AppendMessage(Message{
			ChannelID:    "general",
			ContextEpoch: 1,
			Role:         role,
			Content:      strings.Repeat("observer failure diagnostics ", 6),
			CreatedAt:    oldTS,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Threshold auto-observe should log the error instead of swallowing silently.
	eng.maybeAutoObserve(t.Context(), cfg, "general")
	if !strings.Contains(logBuf.String(), "auto-observe #general failed") || !strings.Contains(logBuf.String(), "502") {
		t.Fatalf("expected auto-observe failure logged, got:\n%s", logBuf.String())
	}

	// Idle pass failure records backoff so immediate reschedule is skipped.
	now := time.Now().UTC()
	eng.runIdleObservePass(t.Context(), cfg, now)
	if !strings.Contains(logBuf.String(), "idle-observe #general failed") {
		t.Fatalf("expected idle-observe failure logged, got:\n%s", logBuf.String())
	}
	callsBefore := calls.Load()
	eng.maybeScheduleIdleObserve(t.Context(), cfg, now.Add(5*time.Minute))
	time.Sleep(30 * time.Millisecond)
	if calls.Load() != callsBefore {
		t.Fatalf("expected backoff to prevent immediate idle-observe retry; calls %d -> %d", callsBefore, calls.Load())
	}
}
