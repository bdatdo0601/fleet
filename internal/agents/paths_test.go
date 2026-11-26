package agents

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestPathHelpers(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"", ""},
		{"/", ""},
		{"relative/dir", ""},
		{"~/../escape", ""},
		{"~", "~"},
		{"~/workspace/fleet/", "~/workspace/fleet"},
		{"/Users/datdo/workspace/fleet/", "/Users/datdo/workspace/fleet"},
	} {
		if got := NormalizeTrackedPath(tc.in); got != tc.want {
			t.Errorf("NormalizeTrackedPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	if got := ToTildePath("/Users/datdo/workspace/fleet"); got != "~/workspace/fleet" {
		t.Errorf("ToTildePath(/Users/...) = %q", got)
	}
	if got := ToTildePath("/home/dat/src/app"); got != "~/src/app" {
		t.Errorf("ToTildePath(/home/...) = %q", got)
	}
	if got := ToTildePath("/opt/app"); got != "" {
		t.Errorf("ToTildePath(/opt/app) = %q, want empty", got)
	}

	if got := WorkspaceSubpath("~/workspace", "/Users/datdo/workspace/fleet"); got != "fleet" {
		t.Errorf("WorkspaceSubpath tilde->abs = %q", got)
	}
	if got := WorkspaceSubpath("/Users/dat/src", "~/src/api"); got != "api" {
		t.Errorf("WorkspaceSubpath abs->tilde = %q", got)
	}
	if got := WorkspaceSubpath("~/workspace", "/Users/datdo/other/fleet"); got != "" {
		t.Errorf("WorkspaceSubpath outside = %q, want empty", got)
	}
	if got := RepoNameFromPath("~/workspace/my-app/"); got != "my-app" {
		t.Errorf("RepoNameFromPath = %q", got)
	}
}

func TestTrackedPathsHeartbeatAndTildeDedup(t *testing.T) {
	hub := openTestHub(t)

	// 1. Spawn/config records ~/workspace/fleet on laptop.
	if err := hub.RecordPath("laptop", "~/workspace/fleet", "fleet", "spawn", true); err != nil {
		t.Fatal(err)
	}

	// 2. Heartbeat from laptop arrives with resolved /Users/datdo/workspace/fleet -> upgrades row without duplicating.
	if _, err := hub.Heartbeat(HeartbeatParams{
		Device:  "laptop",
		Harness: "claude",
		RawID:   "sess-1",
		CWD:     "/Users/datdo/workspace/fleet/",
		Repo:    "fleet",
		State:   "working",
	}); err != nil {
		t.Fatal(err)
	}
	// Repeated heartbeat for same session+cwd does NOT inflate use_count.
	if _, err := hub.Heartbeat(HeartbeatParams{
		Device:  "laptop",
		Harness: "claude",
		RawID:   "sess-1",
		CWD:     "/Users/datdo/workspace/fleet",
		Repo:    "fleet",
		State:   "idle",
	}); err != nil {
		t.Fatal(err)
	}

	// 3. Another spawn with ~/workspace/fleet matches the upgraded absolute row.
	if err := hub.RecordPath("laptop", "~/workspace/fleet", "fleet", "spawn", true); err != nil {
		t.Fatal(err)
	}

	paths, err := hub.ListPaths("laptop", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("expected 1 unified path row, got %+v", paths)
	}
	if paths[0].Path != "/Users/datdo/workspace/fleet" || paths[0].TildePath != "~/workspace/fleet" || paths[0].Repo != "fleet" || paths[0].UseCount != 3 {
		t.Fatalf("unexpected tracked path row: %+v", paths[0])
	}

	// Delete via tilde alias removes the absolute row.
	deleted, err := hub.DeletePath("laptop", "~/workspace/fleet")
	if err != nil || !deleted {
		t.Fatalf("expected DeletePath via tilde to succeed, deleted=%v err=%v", deleted, err)
	}
	if after, _ := hub.ListPaths("laptop", ""); len(after) != 0 {
		t.Fatalf("expected empty paths after delete, got %+v", after)
	}
}

func TestBackfillPathsFromExistingSessions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "agents.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(schema)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(
		`INSERT INTO agent_sessions(session_id, device, harness, raw_id, cwd, repo, state, started_at, updated_at)
		 VALUES ('mini:claude:s1', 'mini', 'claude', 's1', '/Users/dat/code/app', 'app', 'idle', '2026-11-26T09:00:00+00:00', '2026-11-26T10:00:00+00:00')`,
	)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	hub, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hub.Close() }()

	paths, err := hub.ListPaths("mini", "")
	if err != nil || len(paths) != 1 || paths[0].Path != "/Users/dat/code/app" || paths[0].Repo != "app" {
		t.Fatalf("expected backfilled path from agent_sessions, got %+v err=%v", paths, err)
	}
}
