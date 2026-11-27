package render

import (
	"regexp"
	"strings"
	"testing"
)

func TestBlocklistEntriesAreQuoted(t *testing.T) {
	got := BlocklistYAML([]string{"0123"})
	if !strings.Contains(got, `- "0123"`) {
		t.Fatalf("expected quoted fingerprint, got %q", got)
	}
	if strings.TrimSpace(BlocklistYAML(nil)) != "blocklist: []" {
		t.Fatalf("unexpected empty blocklist: %q", BlocklistYAML(nil))
	}
}

func TestAllPlaceholdersRendered(t *testing.T) {
	settings := map[string]string{
		"lighthouse_host": "home.example.com",
		"lighthouse_port": "443",
	}
	re := regexp.MustCompile(`@[A-Z_]+@`)
	for _, kind := range []string{"lighthouse", "mac"} {
		out, err := Render(kind, settings, nil, "")
		if err != nil {
			t.Fatalf("render %s: %v", kind, err)
		}
		if re.MatchString(out) {
			t.Fatalf("leftover placeholders in %s:\n%s", kind, out)
		}
		if !strings.Contains(out, "drop_inactive: true") || !strings.Contains(out, "inactivity_timeout: 12h") {
			t.Fatalf("expected 12h inactive tunnel cleanup in %s:\n%s", kind, out)
		}
	}
	out, err := Render("mac", settings, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "home.example.com:443") {
		t.Fatalf("expected host:port in mac config")
	}
}

func TestConfigChangeRequiresRestart(t *testing.T) {
	settings := map[string]string{
		"lighthouse_host": "home.example.com",
		"lighthouse_port": "443",
	}
	base, err := Render("mac", settings, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// Blocklist-only (pki) change can be SIGHUP-reloaded.
	withBlocklist, err := Render("mac", settings, []string{"deadbeef"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if ConfigChangeRequiresRestart(base, withBlocklist) {
		t.Fatal("expected blocklist change not to require restart")
	}

	// Firewall change can be SIGHUP-reloaded.
	withFirewall := strings.Replace(base, "port: 22 # SSH", "port: 2222 # SSH", 1)
	if ConfigChangeRequiresRestart(base, withFirewall) {
		t.Fatal("expected firewall change not to require restart")
	}

	// Logging level change can be SIGHUP-reloaded.
	withLogging := strings.Replace(base, "level: info", "level: debug", 1)
	if ConfigChangeRequiresRestart(base, withLogging) {
		t.Fatal("expected logging change not to require restart")
	}

	// Empty old config requires restart.
	if !ConfigChangeRequiresRestart("", base) {
		t.Fatal("expected empty oldYAML to require restart")
	}

	// Adding tunnels section requires restart.
	withoutTunnels := strings.Replace(base, "tunnels:\n  drop_inactive: true\n  inactivity_timeout: 12h\n\n", "", 1)
	if !ConfigChangeRequiresRestart(withoutTunnels, base) {
		t.Fatal("expected adding tunnels section to require restart")
	}

	// Non-reloadable section changes require restart.
	for _, tc := range []struct {
		name string
		old  string
		new  string
	}{
		{"relay", "use_relays: true", "use_relays: false"},
		{"punchy", "respond: true", "respond: false"},
		{"lighthouse", "interval: 60", "interval: 30"},
		{"listen", "port: 0", "port: 4242"},
		{"tun", "mtu: 1300", "mtu: 1400"},
	} {
		modified := strings.Replace(base, tc.old, tc.new, 1)
		if modified == base {
			t.Fatalf("test setup %s did not modify base config", tc.name)
		}
		if !ConfigChangeRequiresRestart(base, modified) {
			t.Fatalf("expected %s change to require restart", tc.name)
		}
	}
}
