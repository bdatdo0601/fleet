package agents

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWatchLifecycleAndPushWakeup(t *testing.T) {
	hub, err := Open(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })

	var transitions []StateTransition
	var trMu sync.Mutex
	hub.SetOnTransition(func(ev StateTransition) {
		trMu.Lock()
		transitions = append(transitions, ev)
		trMu.Unlock()
	})

	if _, err := hub.Heartbeat(HeartbeatParams{Device: "mini", Harness: "claude", RawID: "watcher-1", CWD: "/a", Name: new("coord"), ExplicitName: true}); err != nil {
		t.Fatal(err)
	}

	// Self-watch rejected.
	if _, err := hub.CreateWatch(CreateWatchParams{
		WatcherDevice: "mini", WatcherSession: "mini:claude:watcher-1", Target: "mini:claude:watcher-1",
	}); err == nil {
		t.Fatal("expected self-watch to fail")
	}

	// Pending watch on device/name binds automatically when the target first reports in.
	w, err := hub.CreateWatch(CreateWatchParams{
		WatcherDevice: "mini", WatcherSession: "mini:claude:watcher-1",
		TargetDevice: "laptop", TargetName: "worker-x", AllowPending: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if w.TargetSession != "" || !slices.Equal(w.Events, []string{"idle", "waiting_permission", "waiting_question", "exited"}) {
		t.Fatalf("unexpected initial pending watch: %+v", w)
	}

	// Worker starts working -> binds watch, does not wake on working.
	if _, err := hub.Heartbeat(HeartbeatParams{
		Device: "laptop", Harness: "codex", RawID: "wrk-1", CWD: "/b", Name: new("worker-x"), ExplicitName: true, State: "working",
	}); err != nil {
		t.Fatal(err)
	}
	watches, err := hub.ListWatches("mini", "mini:claude:watcher-1", "")
	if err != nil || len(watches) != 1 || watches[0].TargetSession != "laptop:codex:wrk-1" {
		t.Fatalf("expected pending watch bound to laptop:codex:wrk-1, got %+v err=%v", watches, err)
	}

	// Transition working -> waiting_permission wakes watcher via inbox + WaitForMessages.
	var waitRes WaitResult
	var waitErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		waitRes, waitErr = hub.WaitForMessages(context.Background(), WaitParams{
			Device: "mini", SessionID: "mini:claude:watcher-1", Timeout: 2.0, MarkRead: true,
		})
	})
	time.Sleep(25 * time.Millisecond)

	if _, err := hub.Heartbeat(HeartbeatParams{
		Device: "laptop", Harness: "codex", RawID: "wrk-1", CWD: "/b", State: "waiting_permission",
		PendingAction: map[string]string{"kind": "permission", "tool": "Bash", "detail": "rm -rf build"},
	}); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if waitErr != nil || len(waitRes.Messages) != 1 || !strings.Contains(waitRes.Messages[0].Body, "waiting_permission") || !strings.Contains(waitRes.Messages[0].Body, "rm -rf build") {
		t.Fatalf("unexpected watcher wakeup: err=%v res=%+v", waitErr, waitRes)
	}

	// Repeated liveness heartbeat in the same waiting_permission state is deduped.
	if _, err := hub.Heartbeat(HeartbeatParams{
		Device: "laptop", Harness: "codex", RawID: "wrk-1", CWD: "/b", State: "waiting_permission",
		PendingAction: map[string]string{"kind": "permission", "tool": "Bash", "detail": "rm -rf build"},
	}); err != nil {
		t.Fatal(err)
	}
	msgs, _ := hub.FetchInbox("mini", "mini:claude:watcher-1", true, true, 10)
	if len(msgs) != 0 {
		t.Fatalf("expected duplicate waiting_permission heartbeat to be deduped, got %+v", msgs)
	}

	// Resuming working clears dedupe, then finishing (idle) wakes again with LastAssistant.
	if _, err := hub.Heartbeat(HeartbeatParams{Device: "laptop", Harness: "codex", RawID: "wrk-1", CWD: "/b", State: "working"}); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Heartbeat(HeartbeatParams{
		Device: "laptop", Harness: "codex", RawID: "wrk-1", CWD: "/b", State: "idle",
		Tail: []TailItem{{Role: "assistant", Text: "Fixed the auth bug and all tests pass."}},
	}); err != nil {
		t.Fatal(err)
	}
	msgs, _ = hub.FetchInbox("mini", "mini:claude:watcher-1", true, true, 10)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Body, "working -> idle") || !strings.Contains(msgs[0].Body, "Fixed the auth bug") {
		t.Fatalf("expected idle wakeup with LastAssistant, got %+v", msgs)
	}

	// Deregister fires exited wakeup and auto-removes watch row.
	if err := hub.DeregisterAny("laptop:codex:wrk-1"); err != nil {
		t.Fatal(err)
	}
	msgs, _ = hub.FetchInbox("mini", "mini:claude:watcher-1", true, true, 10)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Body, "idle -> exited") {
		t.Fatalf("expected exited wakeup on DeregisterAny, got %+v", msgs)
	}
	watches, _ = hub.ListWatches("mini", "mini:claude:watcher-1", "")
	if len(watches) != 0 {
		t.Fatalf("expected watch auto-deleted after exited, got %+v", watches)
	}

	trMu.Lock()
	defer trMu.Unlock()
	if len(transitions) < 5 || transitions[len(transitions)-1].NewState != "exited" {
		t.Fatalf("unexpected onTransition callbacks: %+v", transitions)
	}
}

func TestWaitForSessionEventAndOnceWatch(t *testing.T) {
	hub, err := Open(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })

	_, _ = hub.Heartbeat(HeartbeatParams{Device: "mini", Harness: "claude", RawID: "w1", CWD: "/a"})
	_, _ = hub.Heartbeat(HeartbeatParams{Device: "mini", Harness: "codex", RawID: "t1", CWD: "/b", Name: new("runner"), ExplicitName: true, State: "working"})

	// Once watch removes itself after the first notification.
	if _, err := hub.CreateWatch(CreateWatchParams{
		WatcherDevice: "mini", WatcherSession: "mini:claude:w1", Target: "runner", Events: []string{"idle"}, Once: true,
	}); err != nil {
		t.Fatal(err)
	}

	var res WatchWaitResult
	var waitErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		res, waitErr = hub.WaitForSessionEvent(context.Background(), WatchWaitParams{
			Target: "runner", Events: []string{"idle", "waiting"}, Timeout: 2.0,
		})
	})
	time.Sleep(25 * time.Millisecond)

	_, _ = hub.Heartbeat(HeartbeatParams{
		Device: "mini", Harness: "codex", RawID: "t1", CWD: "/b", State: "idle",
		Tail: []TailItem{{Role: "assistant", Text: "Done!"}},
	})
	wg.Wait()
	if waitErr != nil || !res.Matched || res.Event == nil || res.Event.NewState != "idle" || res.Event.LastAssistant != "Done!" {
		t.Fatalf("unexpected WaitForSessionEvent result: err=%v res=%+v", waitErr, res)
	}
	if left, _ := hub.ListWatches("mini", "mini:claude:w1", ""); len(left) != 0 {
		t.Fatalf("expected once watch to be removed, got %+v", left)
	}

	// IncludeCurrent returns immediately when already in matching state.
	curRes, err := hub.WaitForSessionEvent(context.Background(), WatchWaitParams{
		Target: "runner", Events: []string{"idle"}, Timeout: 0.2, IncludeCurrent: true,
	})
	if err != nil || !curRes.Matched || curRes.Event == nil || curRes.Event.LastAssistant != "Done!" {
		t.Fatalf("expected IncludeCurrent immediate match, got err=%v res=%+v", err, curRes)
	}
}

func TestAssistantPeerMessagingAndAsk(t *testing.T) {
	hub, err := Open(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hub.Close() }()

	if _, err := hub.Heartbeat(HeartbeatParams{
		Device:  "work-mac",
		Harness: "claude",
		RawID:   "sess-work-1",
		State:   "working",
	}); err != nil {
		t.Fatal(err)
	}

	gotCh := make(chan Message, 2)
	hub.SetOnAssistantMessage(func(m Message) {
		gotCh <- m
		if m.ToSession != nil && *m.ToSession == "assistant:fleet-ops" {
			// Simulate assistant replying immediately on the same thread.
			_, _ = hub.SendMessageWithReport(AssistantDevice, AssistantSessionID("fleet-ops"), m.FromSession, "ack from assistant", &m.ThreadID, &m.ID, map[string]bool{"work-mac": true})
		}
	})

	askRes, err := hub.Ask(context.Background(), AskParams{
		Device:       "work-mac",
		FromSession:  "work-mac:claude:sess-work-1",
		To:           "assistant:fleet-ops",
		Body:         "PR #42 passed checks",
		Timeout:      2,
		KnownDevices: map[string]bool{"work-mac": true},
	})
	if err != nil {
		t.Fatalf("Ask assistant failed: %v", err)
	}
	if !askRes.Delivered || askRes.Reply == nil || askRes.Reply.Body != "ack from assistant" {
		t.Fatalf("unexpected Ask reply: %+v", askRes)
	}
}

func TestAssistantWatchAndSweepStaleSessions(t *testing.T) {
	hub, err := Open(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hub.Close() }()

	if _, err := hub.Heartbeat(HeartbeatParams{
		Device:  "laptop",
		Harness: "codex",
		RawID:   "sess-watch-1",
		Name:    new("fix-auth"),
		State:   "working",
	}); err != nil {
		t.Fatal(err)
	}

	var triggered []string
	hub.SetOnWatchTriggered(func(w Watch, ev StateTransition) {
		triggered = append(triggered, w.WatcherSession+"->"+ev.SessionID+":"+ev.NewState)
	})

	w, err := hub.CreateWatch(CreateWatchParams{
		WatcherDevice:  AssistantDevice,
		WatcherSession: AssistantSessionID("fleet-ops"),
		Target:         "laptop/fix-auth",
		Events:         []string{"idle", "waiting", "exited"},
	})
	if err != nil || w.ID == "" {
		t.Fatalf("CreateWatch failed: %+v err=%v", w, err)
	}

	if _, err := hub.Heartbeat(HeartbeatParams{
		Device:  "laptop",
		Harness: "codex",
		RawID:   "sess-watch-1",
		State:   "idle",
		Tail:    []TailItem{{Role: "assistant", Text: "Auth fixed and tested."}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(triggered) != 1 || triggered[0] != "assistant:fleet-ops->laptop:codex:sess-watch-1:idle" {
		t.Fatalf("unexpected watch trigger: %v", triggered)
	}

	// Simulate time advancing past AgentActiveSeconds without a deregister.
	trs := hub.SweepStaleSessions(time.Now().Add(15 * time.Minute))
	if len(trs) != 1 || trs[0].NewState != "exited" {
		t.Fatalf("expected stale sweep exited transition, got %+v", trs)
	}
	if len(triggered) != 2 || triggered[1] != "assistant:fleet-ops->laptop:codex:sess-watch-1:exited" {
		t.Fatalf("expected exited watch trigger after sweep, got %v", triggered)
	}
	// Exited watches are automatically cleaned up.
	rem, err := hub.ListWatches(AssistantDevice, "", "")
	if err != nil || len(rem) != 0 {
		t.Fatalf("expected exited watch deleted, got %+v err=%v", rem, err)
	}
}
