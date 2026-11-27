package system

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncMacResolver(t *testing.T) {
	dir := t.TempDir()

	// Create a non-fleet user resolver file; it must never be touched.
	userFile := filepath.Join(dir, "corp.internal")
	if err := os.WriteFile(userFile, []byte("nameserver 192.168.10.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Initial create.
	changed, action, err := SyncMacResolver(dir, "Fleet.Example.com.", "10.213.0.1")
	if err != nil || !changed || action != "resolver(fleet.example.com)" {
		t.Fatalf("unexpected initial sync: changed=%v action=%q err=%v", changed, action, err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "fleet.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "# managed by fleet\nnameserver 10.213.0.1\n" {
		t.Fatalf("unexpected resolver content: %q", string(got))
	}

	// Idempotent second call.
	changed, action, err = SyncMacResolver(dir, "fleet.example.com", "10.213.0.1")
	if err != nil || changed || action != "" {
		t.Fatalf("expected no-op second sync, got changed=%v action=%q err=%v", changed, action, err)
	}

	// Domain change removes old fleet-managed file and preserves user file.
	changed, action, err = SyncMacResolver(dir, "ui.example.org", "10.213.0.1")
	if err != nil || !changed || !strings.Contains(action, "ui.example.org") {
		t.Fatalf("unexpected domain change sync: changed=%v action=%q err=%v", changed, action, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "fleet.example.com")); !os.IsNotExist(err) {
		t.Fatalf("expected old fleet resolver file to be removed, got err=%v", err)
	}
	if raw, err := os.ReadFile(userFile); err != nil || string(raw) != "nameserver 192.168.10.1\n" {
		t.Fatalf("user resolver file was modified: raw=%q err=%v", string(raw), err)
	}

	// Disabling removes fleet-managed files while keeping user files.
	changed, action, err = SyncMacResolver(dir, "", "")
	if err != nil || !changed || action != "cleaned stale resolver" && action != "removed fleet resolver" {
		t.Fatalf("unexpected disable sync: changed=%v action=%q err=%v", changed, action, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ui.example.org")); !os.IsNotExist(err) {
		t.Fatalf("expected ui.example.org resolver file removed, got err=%v", err)
	}
	if _, err := os.Stat(userFile); err != nil {
		t.Fatalf("expected user resolver file preserved, got err=%v", err)
	}

	// Invalid domain rejected.
	if _, _, err := SyncMacResolver(dir, "bad/domain", "10.213.0.1"); err == nil {
		t.Fatal("expected error for invalid domain")
	}
}
