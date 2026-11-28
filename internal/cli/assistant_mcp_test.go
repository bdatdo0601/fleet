package cli

import (
	"fmt"
	"net"
	"strings"
	"testing"
)

func TestResolveLoopbackBindAndFinalizeRedirectURI(t *testing.T) {
	// Port-less --redirect-uri binds an ephemeral port and rewrites u.Host with the bound port.
	bindAddr, cbPath, err := resolveLoopbackBind(0, "http://127.0.0.1/oauth2callback")
	if err != nil {
		t.Fatalf("resolveLoopbackBind port-less failed: %v", err)
	}
	if bindAddr != "127.0.0.1:0" || cbPath != "/oauth2callback" {
		t.Fatalf("unexpected bindAddr=%q cbPath=%q", bindAddr, cbPath)
	}
	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		t.Fatal(err)
	}
	tcpAddr := ln.Addr().(*net.TCPAddr)
	_ = ln.Close()

	got, err := finalizeLoopbackRedirectURL("http://127.0.0.1/oauth2callback", tcpAddr, cbPath)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("http://127.0.0.1:%d/oauth2callback", tcpAddr.Port)
	if got != want {
		t.Fatalf("finalizeLoopbackRedirectURL port-less = %q, want %q", got, want)
	}

	// Port 0 in --redirect-uri is also rewritten to the actual bound port.
	gotZero, err := finalizeLoopbackRedirectURL("http://localhost:0/cb", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 43210}, "/cb")
	if err != nil {
		t.Fatal(err)
	}
	if gotZero != "http://localhost:43210/cb" {
		t.Fatalf("finalizeLoopbackRedirectURL port 0 = %q, want http://localhost:43210/cb", gotZero)
	}

	// Explicit non-zero port is preserved.
	gotExplicit, err := finalizeLoopbackRedirectURL("http://127.0.0.1:9876/cb", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9876}, "/cb")
	if err != nil {
		t.Fatal(err)
	}
	if gotExplicit != "http://127.0.0.1:9876/cb" {
		t.Fatalf("finalizeLoopbackRedirectURL explicit = %q", gotExplicit)
	}

	// Reject non-http or non-loopback redirect URIs.
	for _, bad := range []string{
		"https://127.0.0.1:8080/cb",
		"http://example.com:8080/cb",
		"javascript:alert(1)",
	} {
		if _, _, err := resolveLoopbackBind(0, bad); err == nil {
			t.Fatalf("expected error for invalid redirect URI %q", bad)
		}
	}
}

func TestValidateBrowserURL(t *testing.T) {
	for _, good := range []string{
		"http://127.0.0.1:8080/auth",
		"https://accounts.google.com/o/oauth2/v2/auth?client_id=abc",
	} {
		if _, err := validateBrowserURL(good); err != nil {
			t.Fatalf("expected %q to be valid, got err=%v", good, err)
		}
	}
	for _, bad := range []string{
		"",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"ftp://example.com/resource",
		"/relative/path",
		"https:///missing-host",
	} {
		if _, err := validateBrowserURL(bad); err == nil || !strings.Contains(err.Error(), "http or https") {
			t.Fatalf("expected http/https scheme error for %q, got %v", bad, err)
		}
	}
}
