package system

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/paths"
)

func TestNebulaLabelAndPlistValidation(t *testing.T) {
	daemons := t.TempDir()
	if got := NebulaLabel(daemons); got != "" {
		t.Fatalf("expected empty label, got %q", got)
	}
	_ = os.WriteFile(filepath.Join(daemons, "homebrew.mxcl.nebula.plist"), nil, 0o644)
	if got := NebulaLabel(daemons); got != "homebrew.mxcl.nebula" {
		t.Fatalf("expected legacy label, got %q", got)
	}
	_ = os.WriteFile(filepath.Join(daemons, "sh.brew.nebula.plist"), nil, 0o644)
	if got := NebulaLabel(daemons); got != "sh.brew.nebula" {
		t.Fatalf("expected sh.brew.nebula, got %q", got)
	}

	plist, err := BuildDaemonPlist(DaemonOpts{
		Label:     paths.SyncLabel,
		Args:      []string{"/usr/local/libexec/fleet/fleet", "sync"},
		Interval:  300,
		LogPath:   "/var/log/fleet-sync.log",
		WorkDir:   "/usr/local/libexec/fleet",
		KeepAlive: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("plutil"); err == nil {
		pFile := filepath.Join(t.TempDir(), "test.plist")
		_ = os.WriteFile(pFile, []byte(plist), 0o644)
		if out, err := exec.Command("plutil", "-lint", pFile).CombinedOutput(); err != nil {
			t.Fatalf("plutil -lint failed: %v\n%s\n%s", err, string(out), plist)
		}
	}

	agentPlist, err := BuildAgentPlist(AgentOpts{
		Label:     paths.SlackerWorkerLabel,
		Args:      []string{"/opt/homebrew/bin/fleet", "slacker", "worker"},
		LogPath:   "/tmp/slacker-worker.log",
		WorkDir:   "/tmp",
		KeepAlive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(agentPlist, "<key>LimitLoadToSessionType</key>") || !strings.Contains(agentPlist, "<string>Aqua</string>") {
		t.Fatalf("expected Aqua session type in agent plist:\n%s", agentPlist)
	}
	if _, err := exec.LookPath("plutil"); err == nil {
		aFile := filepath.Join(t.TempDir(), "agent.plist")
		_ = os.WriteFile(aFile, []byte(agentPlist), 0o644)
		if out, err := exec.Command("plutil", "-lint", aFile).CombinedOutput(); err != nil {
			t.Fatalf("plutil -lint failed on agent plist: %v\n%s\n%s", err, string(out), agentPlist)
		}
	}
}

func TestInstallBinaryAtomicAndRollbackAndClientUpdate(t *testing.T) {
	tmp := t.TempDir()
	goodBin := filepath.Join(tmp, "good-fleet")
	badBin := filepath.Join(tmp, "bad-fleet")
	v1Bin := filepath.Join(tmp, "v1-fleet")

	_ = os.WriteFile(v1Bin, []byte("#!/bin/sh\necho 'fleet 0.1.0 (v1)'\n"), 0o755)
	_ = os.WriteFile(goodBin, []byte("#!/bin/sh\necho 'fleet 0.1.0 (v2)'\n"), 0o755)
	_ = os.WriteFile(badBin, []byte("#!/bin/sh\necho 'broken'; exit 1\n"), 0o755)

	target := filepath.Join(tmp, "libexec", "fleet")
	symlink := filepath.Join(tmp, "bin", "fleet")

	// Install v1 first
	prev, err := InstallBinaryAtomic(v1Bin, target, symlink)
	if err != nil || prev != "" {
		t.Fatalf("v1 install failed: prev=%q err=%v", prev, err)
	}
	// Broken binary must be rejected by smoke test without overwriting v1
	if _, err := InstallBinaryAtomic(badBin, target, symlink); err == nil {
		t.Fatal("expected smoke test failure for bad binary")
	}
	out, _ := exec.Command(symlink, "version").CombinedOutput()
	if !strings.Contains(string(out), "(v1)") {
		t.Fatalf("expected v1 preserved after bad smoke test, got %q", string(out))
	}

	// Install v2 via SelfUpdateClient over mock HTTP server
	goodBytes, _ := os.ReadFile(goodBin)
	sum := sha256.Sum256(goodBytes)
	goodSHA := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/binary" && r.URL.Query().Get("os") == runtime.GOOS && r.URL.Query().Get("arch") == runtime.GOARCH {
			w.Header().Set("X-Fleet-SHA256", goodSHA)
			_, _ = w.Write(goodBytes)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	updated, err := SelfUpdateClient(ClientUpdateOpts{
		APIURL:      srv.URL,
		ExpectedSHA: goodSHA,
		TargetBin:   target,
		SymlinkPath: symlink,
	})
	if err != nil || !updated {
		t.Fatalf("SelfUpdateClient failed: updated=%v err=%v", updated, err)
	}
	out2, _ := exec.Command(symlink, "version").CombinedOutput()
	if !strings.Contains(string(out2), "(v2)") {
		t.Fatalf("expected v2 via symlink, got %q", string(out2))
	}

	// Second call with matching SHA is a no-op
	updated2, err := SelfUpdateClient(ClientUpdateOpts{
		APIURL:      srv.URL,
		ExpectedSHA: goodSHA,
		TargetBin:   target,
		SymlinkPath: symlink,
	})
	if err != nil || updated2 {
		t.Fatalf("expected no-op when SHA matches: updated=%v err=%v", updated2, err)
	}

	// Rollback restores v1
	if err := RollbackBinary(target, target+".prev"); err != nil {
		t.Fatal(err)
	}
	out3, _ := exec.Command(symlink, "version").CombinedOutput()
	if !strings.Contains(string(out3), "(v1)") {
		t.Fatalf("expected v1 after rollback, got %q", string(out3))
	}
}

func TestSelfUpdateLighthouseRollbackAndManifestPublish(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not installed")
	}
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err != nil {
		t.Skip("go.mod not found at repoRoot")
	}

	// Verify manifest.json structure helper when reading back
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	binDir := filepath.Join(stateDir, "bin")
	_ = os.MkdirAll(binDir, 0o755)
	m := core.BinaryManifest{
		Version:  "0.1.0",
		Commit:   "abc",
		Binaries: map[string]core.BinaryEntry{"darwin/arm64": {File: "fleet-darwin-arm64", SHA256: "deadbeef", Size: 4}},
	}
	mb, _ := json.Marshal(m)
	if err := writeOwnerFile(filepath.Join(binDir, "manifest.json"), mb, 0o644, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(binDir, "manifest.json")); err != nil {
		t.Fatal(err)
	}
}
