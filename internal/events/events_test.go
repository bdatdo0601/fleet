package events

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestPublishDedupeListAndSubscribe(t *testing.T) {
	bus, err := Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bus.Close() }()

	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)

	ev1, ins, err := bus.Publish(Event{
		Type:      "agent.transition",
		Severity:  "warning",
		Source:    "agents",
		Actor:     "laptop",
		Subject:   "laptop:claude:1234",
		DedupeKey: "tr-1",
		Title:     "Agent waiting",
		Summary:   "Needs Bash permission",
		Payload:   map[string]any{"state": "waiting_permission"},
	})
	if err != nil || !ins || ev1.ID == 0 || ev1.Severity != "warn" {
		t.Fatalf("unexpected publish result: ev=%+v ins=%v err=%v", ev1, ins, err)
	}

	// Duplicate dedupe_key must be ignored without notifying subscribers.
	_, insDup, err := bus.Publish(Event{
		Type:      "agent.transition",
		Source:    "agents",
		DedupeKey: "tr-1",
	})
	if err != nil || insDup {
		t.Fatalf("expected dedupe ignore, got ins=%v err=%v", insDup, err)
	}

	ev2, ins2, err := bus.Publish(Event{
		Type:        "slacker.job",
		Severity:    "error",
		Source:      "slacker",
		Actor:       "office",
		Subject:     "job-1",
		ChannelHint: "fleet-ops",
		DedupeKey:   "slk-1",
		Title:       "Spawn failed",
	})
	if err != nil || !ins2 {
		t.Fatalf("publish ev2 failed: ins=%v err=%v", ins2, err)
	}

	select {
	case got := <-sub:
		if got.ID != ev1.ID {
			t.Fatalf("expected ev1 first on sub, got %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ev1 on subscriber")
	}
	select {
	case got := <-sub:
		if got.ID != ev2.ID {
			t.Fatalf("expected ev2 second on sub, got %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ev2 on subscriber")
	}
	select {
	case extra := <-sub:
		t.Fatalf("unexpected duplicate event on subscriber: %+v", extra)
	default:
	}

	// Filter by type prefix & severity.
	list, err := bus.List(Filter{TypePrefix: "agent.", Severity: "warn"})
	if err != nil || len(list) != 1 || list[0].ID != ev1.ID || list[0].Payload["state"] != "waiting_permission" {
		t.Fatalf("unexpected filtered list: %+v err=%v", list, err)
	}

	// SinceID returns newer events in ascending order.
	since, err := bus.List(Filter{SinceID: ev1.ID})
	if err != nil || len(since) != 1 || since[0].ID != ev2.ID {
		t.Fatalf("unexpected since list: %+v err=%v", since, err)
	}
}

func TestPruneAndCloseSafety(t *testing.T) {
	bus, err := Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}

	oldTS := time.Now().UTC().Add(-(RetentionDays + 2) * 24 * time.Hour).Format("2006-01-02T15:04:05+00:00")
	if _, ins, err := bus.Publish(Event{
		Type:      "infra.old",
		Source:    "infra",
		DedupeKey: "old-1",
		CreatedAt: oldTS,
	}); err != nil || !ins {
		t.Fatalf("insert old event failed: ins=%v err=%v", ins, err)
	}
	bus.mu.Lock()
	bus.lastPrune = time.Time{}
	bus.mu.Unlock()
	if _, _, err := bus.Publish(Event{Type: "infra.new", Source: "infra", DedupeKey: "new-1"}); err != nil {
		t.Fatal(err)
	}
	all, err := bus.List(Filter{})
	if err != nil || len(all) != 1 || all[0].DedupeKey != "new-1" {
		t.Fatalf("expected old event pruned, got %+v err=%v", all, err)
	}

	if err := bus.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := bus.Publish(Event{Type: "x"}); !errors.Is(err, errBusClosed) {
		t.Fatalf("expected errBusClosed on Publish, got %v", err)
	}
	if _, err := bus.List(Filter{}); !errors.Is(err, errBusClosed) {
		t.Fatalf("expected errBusClosed on List, got %v", err)
	}
}
