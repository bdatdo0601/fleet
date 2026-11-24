package agents

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openTestHub(t *testing.T) *Hub {
	t.Helper()
	h, err := Open(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func TestHeartbeatPlaceholderPromotionAndDeregister(t *testing.T) {
	hub := openTestHub(t)

	hb1, err := hub.Heartbeat(HeartbeatParams{
		Device: "laptop", Harness: "claude", RawID: "pid-4242", PID: new(4242),
		CWD: "/Users/datdo/proj", Repo: "proj", Branch: "main",
	})
	if err != nil || hb1.SessionID != "laptop:claude:pid-4242" {
		t.Fatalf("hb1 unexpected: %+v, %v", hb1, err)
	}

	if _, err := hub.Heartbeat(HeartbeatParams{
		Device: "office", Harness: "codex", RawID: "codex-1111", CWD: "/tmp/o", Repo: "other",
	}); err != nil {
		t.Fatal(err)
	}
	sendRes, err := hub.SendMessageWithReport("office", "office:codex:codex-1111", "laptop:claude:pid-4242", "check api.py", nil, nil, nil)
	if err != nil || sendRes.Message.ToSession == nil || *sendRes.Message.ToSession != "laptop:claude:pid-4242" {
		t.Fatalf("send failed: %+v, %v", sendRes, err)
	}

	hb2, err := hub.Heartbeat(HeartbeatParams{
		Device: "laptop", Harness: "claude", RawID: "c779894d-fe1c-41c0",
		CWD: "/Users/datdo/proj", Repo: "proj", Branch: "main",
		Summary:    new("fix bug"),
		Tail:       []TailItem{{Role: "user", Text: "fix bug"}},
		DrainInbox: true,
	})
	if err != nil || hb2.SessionID != "laptop:claude:c779894d-fe1" {
		t.Fatalf("hb2 unexpected: %+v, %v", hb2, err)
	}
	if len(hb2.Unread) != 1 || hb2.Unread[0].Body != "check api.py" {
		t.Fatalf("expected migrated unread message, got %+v", hb2.Unread)
	}

	live, err := hub.ListSessions("laptop", "", "", "", false)
	if err != nil || len(live) != 1 || live[0].PID == nil || *live[0].PID != 4242 {
		t.Fatalf("unexpected live sessions: %+v, %v", live, err)
	}
	detail, err := hub.GetSession("c779894d-fe1")
	if err != nil || len(detail.Tail) != 1 || detail.Tail[0].Text != "fix bug" {
		t.Fatalf("unexpected detail: %+v, %v", detail, err)
	}

	if err := hub.Deregister("laptop", nil, new("claude"), new(4242)); err != nil {
		t.Fatal(err)
	}
	if after, _ := hub.ListSessions("laptop", "", "", "", false); len(after) != 0 {
		t.Fatalf("expected no live sessions after deregister, got %+v", after)
	}
	if all, _ := hub.ListSessions("laptop", "", "", "", true); len(all) != 1 {
		t.Fatalf("expected 1 stale session, got %+v", all)
	}
}

func TestBroadcastReadsAndAskWakeup(t *testing.T) {
	hub := openTestHub(t)
	_, _ = hub.Heartbeat(HeartbeatParams{Device: "mini", Harness: "claude", RawID: "s1", Repo: "fleet", CWD: "/f"})
	_, _ = hub.Heartbeat(HeartbeatParams{Device: "laptop", Harness: "codex", RawID: "s2", Repo: "fleet", CWD: "/f"})
	_, _ = hub.Heartbeat(HeartbeatParams{Device: "office", Harness: "antigravity", RawID: "s3", Repo: "other", CWD: "/o"})
	known := map[string]bool{"mini": true, "laptop": true, "office": true}

	if _, err := hub.SendMessageWithReport("mini", "mini:claude:s1", "repo:fleet", "rebasing main", nil, nil, known); err != nil {
		t.Fatal(err)
	}
	if m, _ := hub.FetchInbox("laptop", "laptop:codex:s2", true, true, 50); len(m) != 1 {
		t.Fatalf("expected 1 repo msg, got %d", len(m))
	}
	if m, _ := hub.FetchInbox("laptop", "laptop:codex:s2", true, true, 50); len(m) != 0 {
		t.Fatalf("expected 0 after mark_read, got %d", len(m))
	}
	if m, _ := hub.FetchInbox("office", "office:antigravity:s3", true, true, 50); len(m) != 0 {
		t.Fatalf("expected 0 for other repo, got %d", len(m))
	}

	if _, err := hub.SendMessageWithReport("mini", "mini:claude:s1", "*", "deploying", nil, nil, known); err != nil {
		t.Fatal(err)
	}
	if m, _ := hub.FetchInbox("laptop", "laptop:codex:s2", true, true, 50); len(m) != 1 {
		t.Fatalf("expected 1 bcast msg for laptop, got %d", len(m))
	}
	if m, _ := hub.FetchInbox("office", "office:antigravity:s3", true, true, 50); len(m) != 1 {
		t.Fatalf("expected 1 bcast msg for office, got %d", len(m))
	}

	// Concurrent ask wakeup
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 60 {
			msgs, _ := hub.FetchInbox("laptop", "laptop:codex:s2", true, true, 50)
			if len(msgs) > 0 {
				_, _ = hub.SendMessageWithReport("laptop", "laptop:codex:s2", msgs[0].FromSession, "tests pass", nil, &msgs[0].ID, known)
				return
			}
			time.Sleep(15 * time.Millisecond)
		}
	})
	res, err := hub.Ask(context.Background(), AskParams{
		Device: "mini", FromSession: "mini:claude:s1", To: "laptop:codex:s2", Body: "status?", Timeout: 3.0,
	})
	wg.Wait()
	if err != nil || res.TimedOut || res.Reply == nil || res.Reply.Body != "tests pass" {
		t.Fatalf("unexpected ask res: %+v, err=%v", res, err)
	}

	// Timeout fallback
	toRes, err := hub.Ask(context.Background(), AskParams{
		Device: "mini", FromSession: "mini:claude:s1", To: "office:antigravity:s3", Body: "anyone there?", Timeout: 0.12,
	})
	if err != nil || !toRes.TimedOut || toRes.Reply != nil {
		t.Fatalf("expected timeout, got %+v, err=%v", toRes, err)
	}
}

func TestLegacyAgentsDBMigrationAndNamingAndWaiting(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "agents.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	nowISO := time.Now().UTC().Truncate(time.Second).Format("2006-01-02T15:04:05+00:00")
	_, err = db.Exec(`
		CREATE TABLE agent_sessions (
			session_id TEXT PRIMARY KEY, device TEXT NOT NULL, harness TEXT NOT NULL,
			raw_id TEXT NOT NULL, pid INTEGER, cwd TEXT NOT NULL DEFAULT '',
			repo TEXT NOT NULL DEFAULT '', branch TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL DEFAULT 'working', summary TEXT NOT NULL DEFAULT '',
			git_status TEXT NOT NULL DEFAULT '', tail_json TEXT NOT NULL DEFAULT '[]',
			started_at TEXT NOT NULL, updated_at TEXT NOT NULL
		);
		CREATE TABLE agent_messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT, thread_id TEXT NOT NULL, reply_to INTEGER,
			from_session TEXT NOT NULL, from_device TEXT NOT NULL,
			to_device TEXT, to_session TEXT, to_repo TEXT, body TEXT NOT NULL, created_at TEXT NOT NULL
		);
		CREATE TABLE agent_reads (
			session_id TEXT NOT NULL, message_id INTEGER NOT NULL, read_at TEXT NOT NULL,
			PRIMARY KEY (session_id, message_id)
		);
		INSERT INTO agent_sessions(session_id, device, harness, raw_id, cwd, repo, started_at, updated_at)
		VALUES ('mini:claude:leg1', 'mini', 'claude', 'leg1', '/proj', 'legacy-repo', ?, ?);
	`, nowISO, nowISO)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}

	hub, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hub.Close() }()

	listed, err := hub.ListSessions("", "", "", "", false)
	if err != nil || len(listed) != 1 || listed[0].SessionID != "mini:claude:leg1" || listed[0].Delivery != "next_hook" {
		t.Fatalf("unexpected migrated session: %+v, err=%v", listed, err)
	}
	hb, err := hub.Heartbeat(HeartbeatParams{
		Device: "mini", Harness: "claude", RawID: "leg1-full", Title: new("Migrated Session"), CWD: "/proj",
	})
	if err != nil || hb.Name != "migrated-session" {
		t.Fatalf("unexpected migrated name: %+v, err=%v", hb, err)
	}

	// Rich pending action not clobbered by generic notification
	_, _ = hub.Heartbeat(HeartbeatParams{
		Device: "mini", Harness: "claude", RawID: "cl-wait-1", PID: new(5151), CWD: "/wait",
		State:         "waiting_permission",
		PendingAction: map[string]string{"kind": "permission", "tool": "Bash", "detail": "rm -rf build", "event": "PermissionRequest"},
	})
	s1, _ := hub.GetSession("cl-wait-1")
	origChanged := s1.StateChangedAt
	_, _ = hub.Heartbeat(HeartbeatParams{
		Device: "mini", Harness: "claude", RawID: "cl-wait-1", PID: new(5151), CWD: "/wait",
		State:         "waiting_permission",
		PendingAction: map[string]string{"kind": "permission", "detail": "Claude needs permission", "event": "Notification:permission_prompt"},
	})
	s2, _ := hub.GetSession("cl-wait-1")
	if s2.PendingAction["tool"] != "Bash" || s2.PendingAction["detail"] != "rm -rf build" || s2.StateChangedAt != origChanged {
		t.Fatalf("generic notification clobbered rich pending action: %+v", s2)
	}
}

func TestWaiterContextCancellationCleansUpWaiterCount(t *testing.T) {
	hub := openTestHub(t)
	_, _ = hub.Heartbeat(HeartbeatParams{Device: "mini", Harness: "claude", RawID: "w1", CWD: "/w", State: "idle"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := hub.WaitForMessages(ctx, WaitParams{
			Device: "mini", SessionID: "mini:claude:w1", Timeout: 10.0, MarkRead: true,
		})
		done <- err
	}()

	var listening bool
	for range 40 {
		s, _ := hub.GetSession("w1")
		if s.Listening {
			listening = true
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !listening {
		t.Fatal("expected session to become listening=true")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForMessages did not unblock on context cancel")
	}

	hub.mu.Lock()
	count := hub.waiters["mini:claude:w1"]
	subs := len(hub.sessionSubs["mini:claude:w1"])
	hub.mu.Unlock()
	if count != 0 || subs != 0 {
		t.Fatalf("waiter leak after cancel: count=%d subs=%d", count, subs)
	}
}

func TestSlugifyAndNormalize(t *testing.T) {
	if got := SlugifyName("Auth Bug Fix: Token Refresh!", 5, 32); got != "auth-bug-fix-token-refresh" {
		t.Fatalf("unexpected slug: %q", got)
	}
	if _, _, err := MakeSessionID("mini", "unknown", "abc"); err == nil || !strings.Contains(err.Error(), "unknown harness") {
		t.Fatalf("expected unknown harness error, got %v", err)
	}
}
