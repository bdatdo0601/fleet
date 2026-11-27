package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdatdo0601/fleet/internal/core"
)

const sampleRelayErr = `time=2026-11-27T06:51:40.399-05:00 level=INFO msg="Failed to find target host info by ip" vpnAddrs=[10.213.0.4] certName=ipadpro relayTo=10.213.0.2 relayFrom=10.213.0.4 error="unable to find host with relay"` + "\n"

func appendLogLines(t *testing.T, path, line string, count int) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for range count {
		if _, err := f.WriteString(line); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNebulaWatchdogLifecycle(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "nebula.log")

	// Pre-existing lines present before Init() must be ignored.
	appendLogLines(t, logPath, sampleRelayErr, 10)

	wd := &NebulaWatchdog{
		StateDir:       dir,
		LogPath:        logPath,
		BurstWindow:    60 * time.Second,
		BurstThreshold: 5,
		Cooldown:       10 * time.Minute,
	}
	wd.Init()

	t0 := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	if wd.CheckOnce(t0) {
		t.Fatal("expected pre-existing log lines not to trigger restart")
	}

	// Below threshold (< 5 errors) does not trigger.
	appendLogLines(t, logPath, sampleRelayErr, 4)
	if wd.CheckOnce(t0.Add(5 * time.Second)) {
		t.Fatal("expected 4 errors not to trigger restart")
	}
	if st := wd.State(); st.RecentErrors != 4 || !st.Enabled {
		t.Fatalf("unexpected watchdog state: %+v", st)
	}

	// 5th error within window triggers a restart request with peer context.
	appendLogLines(t, logPath, sampleRelayErr, 1)
	if !wd.CheckOnce(t0.Add(10 * time.Second)) {
		t.Fatal("expected 5th error to trigger restart")
	}
	req1, err := core.PendingNebulaRestart(dir)
	if err != nil || req1 == nil || req1.By != "watchdog" || !strings.Contains(req1.Reason, "10.213.0.4 -> 10.213.0.2") {
		t.Fatalf("unexpected pending request: %+v err=%v", req1, err)
	}

	// Simulate completion of req1; another burst within cooldown must not re-trigger.
	if err := core.RecordNebulaRestartStatus(dir, core.NebulaRestartStatus{
		HandledID:   req1.ID,
		RestartedAt: "2026-11-27T12:00:15+00:00",
		Reason:      req1.Reason,
		By:          req1.By,
		OK:          true,
	}, nil, nil); err != nil {
		t.Fatal(err)
	}
	appendLogLines(t, logPath, sampleRelayErr, 6)
	if wd.CheckOnce(t0.Add(2 * time.Minute)) {
		t.Fatal("expected cooldown to suppress trigger")
	}

	// Simulate log truncation/rotation and burst after cooldown expires.
	if err := os.WriteFile(logPath, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	appendLogLines(t, logPath, sampleRelayErr, 5)
	if !wd.CheckOnce(t0.Add(12 * time.Minute)) {
		t.Fatal("expected trigger after log truncation and cooldown expiry")
	}
	req2, err := core.PendingNebulaRestart(dir)
	if err != nil || req2 == nil || req2.ID == req1.ID {
		t.Fatalf("expected new pending request after rotation/cooldown, got %+v err=%v", req2, err)
	}
}
