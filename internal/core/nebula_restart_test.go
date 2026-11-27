package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNebulaRestartRequestAndStatusLifecycle(t *testing.T) {
	dir := t.TempDir()

	// Empty dir has no pending request.
	pending, err := PendingNebulaRestart(dir)
	if err != nil || pending != nil {
		t.Fatalf("expected no pending request initially, got pending=%v err=%v", pending, err)
	}
	health, err := ReadNebulaHealth(dir)
	if err != nil || health.Pending || health.PendingRequest != nil || health.LastRestart != nil {
		t.Fatalf("unexpected initial health: %+v err=%v", health, err)
	}

	// Request creates pending state and preserves inode on subsequent requests.
	req1, err := RequestNebulaRestart(dir, "laptop", "")
	if err != nil {
		t.Fatal(err)
	}
	if req1.ID == "" || req1.Reason != "manual" || req1.By != "laptop" {
		t.Fatalf("unexpected req1: %+v", req1)
	}
	info1, err := os.Stat(filepath.Join(dir, NebulaRestartReqFile))
	if err != nil {
		t.Fatal(err)
	}

	pending, err = PendingNebulaRestart(dir)
	if err != nil || pending == nil || pending.ID != req1.ID {
		t.Fatalf("expected pending req1, got %+v err=%v", pending, err)
	}
	health, err = ReadNebulaHealth(dir)
	if err != nil || !health.Pending || health.PendingRequest == nil || health.PendingRequest.ID != req1.ID {
		t.Fatalf("expected health pending=true for req1, got %+v err=%v", health, err)
	}

	// Recording status clears pending without removing the watched request file.
	err = RecordNebulaRestartStatus(dir, NebulaRestartStatus{
		HandledID:   req1.ID,
		RestartedAt: "2026-11-27T12:00:00+00:00",
		Reason:      req1.Reason,
		By:          req1.By,
		OK:          true,
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err = PendingNebulaRestart(dir)
	if err != nil || pending != nil {
		t.Fatalf("expected no pending request after status recorded, got %+v err=%v", pending, err)
	}
	health, err = ReadNebulaHealth(dir)
	if err != nil || health.Pending || health.LastRestart == nil || !health.LastRestart.OK || health.LastRestart.HandledID != req1.ID {
		t.Fatalf("unexpected health after recording status: %+v err=%v", health, err)
	}

	// A second request updates the file in place and marks pending again.
	req2, err := RequestNebulaRestart(dir, "watchdog", "unable to find host with relay")
	if err != nil {
		t.Fatal(err)
	}
	if req2.ID == req1.ID {
		t.Fatalf("expected distinct request IDs")
	}
	info2, err := os.Stat(filepath.Join(dir, NebulaRestartReqFile))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(info1, info2) {
		t.Fatalf("expected RequestNebulaRestart to write in place without replacing inode")
	}
	pending, err = PendingNebulaRestart(dir)
	if err != nil || pending == nil || pending.ID != req2.ID {
		t.Fatalf("expected req2 to be pending, got %+v err=%v", pending, err)
	}
}
