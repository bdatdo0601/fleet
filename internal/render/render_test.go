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
	}
	out, err := Render("mac", settings, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "home.example.com:443") {
		t.Fatalf("expected host:port in mac config")
	}
}
