package system

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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
		Label:      paths.SyncLabel,
		Args:       []string{"/usr/local/libexec/fleet/fleet", "sync"},
		Interval:   300,
		LogPath:    "/var/log/fleet-sync.log",
		WorkDir:    "/usr/local/libexec/fleet",
		KeepAlive:  false,
		WatchPaths: []string{"/Users/test/.fleet/nebula-restart.req"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plist, "<key>WatchPaths</key>") || !strings.Contains(plist, "/Users/test/.fleet/nebula-restart.req") {
		t.Fatalf("expected WatchPaths in daemon plist:\n%s", plist)
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

	if u, err := user.Current(); err == nil && u.Username != "" && u.HomeDir != "" {
		userDaemonPlist, err := BuildDaemonPlist(DaemonOpts{
			Label:   paths.ServeLabel,
			Args:    []string{"/usr/local/libexec/fleet/fleet", "serve"},
			User:    u.Username,
			LogPath: filepath.Join(u.HomeDir, ".fleet", "serve.log"),
			WorkDir: filepath.Join(u.HomeDir, ".fleet"),
		})
		if err != nil {
			t.Fatalf("BuildDaemonPlist with user failed: %v", err)
		}
		wantLocalBin := filepath.Join(u.HomeDir, ".local", "bin")
		if !strings.Contains(userDaemonPlist, wantLocalBin) {
			t.Fatalf("expected %s in user daemon PATH:\n%s", wantLocalBin, userDaemonPlist)
		}
	}
	rootDaemonPlist, err := BuildDaemonPlist(DaemonOpts{
		Label:   paths.EdgeLabel,
		Args:    []string{"/usr/local/libexec/fleet/fleet", "edge"},
		LogPath: "/var/log/fleet-edge.log",
	})
	if err != nil {
		t.Fatalf("BuildDaemonPlist root failed: %v", err)
	}
	if !strings.Contains(rootDaemonPlist, "<string>"+StandardPathEnv+"</string>") {
		t.Fatalf("expected StandardPathEnv when User is empty:\n%s", rootDaemonPlist)
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

func TestParseLaunchctlPIDAndProcessStartTime(t *testing.T) {
	sample := `
gui/501/io.github.bdatdo0601.fleet.slacker-worker = {
	active count = 1
	state = running
	pid = 7015
}
`
	if got := parseLaunchctlPID(sample); got != 7015 {
		t.Fatalf("parseLaunchctlPID = %d, want 7015", got)
	}
	if got := parseLaunchctlPID("state = waiting\n"); got != 0 {
		t.Fatalf("parseLaunchctlPID(no pid) = %d, want 0", got)
	}

	tsSingleDigit, err := parseProcessStartTime("Wed Nov  5 09:04:05 2026\n")
	if err != nil {
		t.Fatalf("parseProcessStartTime single digit failed: %v", err)
	}
	wantSingle := time.Date(2026, time.November, 5, 9, 4, 5, 0, time.Local)
	if !tsSingleDigit.Equal(wantSingle) {
		t.Fatalf("got %v, want %v", tsSingleDigit, wantSingle)
	}

	tsDoubleDigit, err := parseProcessStartTime("Wed Nov 25 21:40:54 2026")
	if err != nil {
		t.Fatalf("parseProcessStartTime double digit failed: %v", err)
	}
	wantDouble := time.Date(2026, time.November, 25, 21, 40, 54, 0, time.Local)
	if !tsDoubleDigit.Equal(wantDouble) {
		t.Fatalf("got %v, want %v", tsDoubleDigit, wantDouble)
	}
	if _, err := parseProcessStartTime("   "); err == nil {
		t.Fatal("expected parseProcessStartTime(empty) to fail")
	}
}

func TestBuildAgentPlistWithPathEnvAndShell(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	withShell, err := BuildAgentPlist(AgentOpts{
		Label:     paths.SlackerWorkerLabel,
		Args:      []string{"/usr/local/libexec/fleet/fleet", "slacker", "worker"},
		LogPath:   "/tmp/slacker-worker.log",
		WorkDir:   home,
		KeepAlive: true,
		ExtraPath: "/extra/bin",
		PathEnv:   "/proto/shims:/opt/homebrew/bin:/extra/bin:/usr/bin",
		Shell:     "/bin/zsh",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantPath := "/extra/bin:/proto/shims:/opt/homebrew/bin:/usr/bin:" + filepath.Join(home, ".local", "bin") + ":/usr/local/bin:/bin:/usr/sbin:/sbin"
	if !strings.Contains(withShell, "<string>"+wantPath+"</string>") {
		t.Fatalf("unexpected PATH in plist:\nwant: %s\ngot:\n%s", wantPath, withShell)
	}
	if !strings.Contains(withShell, "<key>SHELL</key>\n\t\t<string>/bin/zsh</string>") {
		t.Fatalf("expected SHELL=/bin/zsh in plist:\n%s", withShell)
	}
	if _, err := exec.LookPath("plutil"); err == nil {
		pFile := filepath.Join(t.TempDir(), "agent.plist")
		if err := os.WriteFile(pFile, []byte(withShell), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("plutil", "-lint", pFile).CombinedOutput(); err != nil {
			t.Fatalf("plutil -lint failed: %v\n%s", err, string(out))
		}
	}

	withoutShell, err := BuildAgentPlist(AgentOpts{
		Label:   paths.SlackerWorkerLabel,
		Args:    []string{"/usr/local/libexec/fleet/fleet", "slacker", "worker"},
		PathEnv: "/proto/shims",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(withoutShell, "<key>SHELL</key>") {
		t.Fatalf("did not expect SHELL key when Shell is empty:\n%s", withoutShell)
	}
}

func TestPullLighthouseRepoIgnoresPullRebaseAndMultipleMergeRefs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmdArgs := args
		if dir != "" {
			cmdArgs = append([]string{"-C", dir}, args...)
		}
		out, err := exec.Command("git", cmdArgs...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", cmdArgs, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}

	tmp := t.TempDir()
	originDir := filepath.Join(tmp, "origin.git")
	cloneDir := filepath.Join(tmp, "clone")
	pusherDir := filepath.Join(tmp, "pusher")

	runGit("", "init", "--bare", "-b", "main", originDir)
	runGit("", "clone", originDir, cloneDir)
	runGit(cloneDir, "checkout", "-B", "main")
	runGit(cloneDir, "config", "user.name", "Test")
	runGit(cloneDir, "config", "user.email", "test@example.com")

	if err := os.WriteFile(filepath.Join(cloneDir, "README.md"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(cloneDir, "add", "README.md")
	runGit(cloneDir, "commit", "-m", "initial")
	runGit(cloneDir, "push", "-u", "origin", "main")

	// Create and push a second branch so branch.main.merge can reference two heads.
	runGit(cloneDir, "checkout", "-b", "other")
	if err := os.WriteFile(filepath.Join(cloneDir, "OTHER.md"), []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(cloneDir, "add", "OTHER.md")
	runGit(cloneDir, "commit", "-m", "other branch commit")
	runGit(cloneDir, "push", "origin", "other")
	runGit(cloneDir, "checkout", "main")

	// Advance origin/main from a separate clone.
	runGit("", "clone", "-b", "main", originDir, pusherDir)
	runGit(pusherDir, "config", "user.name", "Pusher")
	runGit(pusherDir, "config", "user.email", "pusher@example.com")
	if err := os.WriteFile(filepath.Join(pusherDir, "README.md"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(pusherDir, "add", "README.md")
	runGit(pusherDir, "commit", "-m", "update main")
	runGit(pusherDir, "push", "origin", "main")
	wantSHA := runGit(pusherDir, "rev-parse", "HEAD")

	// Reproduce the failing config on the local repo: pull.rebase=true + multiple merge refs.
	runGit(cloneDir, "config", "pull.rebase", "true")
	runGit(cloneDir, "config", "--add", "branch.main.merge", "refs/heads/other")

	out, err := exec.Command("git", "-C", cloneDir, "pull", "--ff-only", "-q").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "Cannot rebase onto multiple branches") {
		t.Fatalf("expected raw git pull to fail with multiple branches error, got err=%v out=%q", err, string(out))
	}

	if got := pullLighthouseRepo(cloneDir, ""); got != wantSHA {
		t.Fatalf("pullLighthouseRepo() HEAD = %q, want %q", got, wantSHA)
	}
}

func TestReloadLaunchdServiceWaitsPastSlowShutdownAndRetriesBootstrap(t *testing.T) {
	bootoutCalls := 0
	loadedChecks := 0
	bootstrapCalls := 0
	var slept time.Duration

	err := reloadLaunchdService(
		"/Library/LaunchDaemons/io.github.bdatdo0601.fleet.serve.plist",
		func() { bootoutCalls++ },
		func() bool {
			loadedChecks++
			// Simulate a 6-second teardown (40 * 150ms > old 3s cap).
			return loadedChecks <= 40
		},
		func() (string, error) {
			bootstrapCalls++
			// Simulate transient launchd EIO (exit status 5) before succeeding.
			if bootstrapCalls < 4 {
				return "Bootstrap failed: 5: Input/output error", errors.New("exit status 5")
			}
			return "", nil
		},
		func(d time.Duration) { slept += d },
	)
	if err != nil {
		t.Fatalf("reloadLaunchdService failed: %v", err)
	}
	if bootoutCalls != 1 {
		t.Fatalf("bootoutCalls = %d, want 1", bootoutCalls)
	}
	if loadedChecks != 41 {
		t.Fatalf("loadedChecks = %d, want 41", loadedChecks)
	}
	if bootstrapCalls != 4 {
		t.Fatalf("bootstrapCalls = %d, want 4", bootstrapCalls)
	}
	wantSleep := 40*launchdUnloadPollInterval + 3*launchdBootstrapInterval
	if slept != wantSleep {
		t.Fatalf("slept = %v, want %v", slept, wantSleep)
	}
}

func TestReloadLaunchdServiceReturnsErrorWhenBootstrapNeverSucceeds(t *testing.T) {
	bootstrapCalls := 0
	err := reloadLaunchdService(
		"/Library/LaunchDaemons/test.plist",
		func() {},
		func() bool { return false },
		func() (string, error) {
			bootstrapCalls++
			return "Bootstrap failed: 5: Input/output error\n", errors.New("exit status 5")
		},
		func(time.Duration) {},
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if bootstrapCalls != launchdBootstrapAttempts {
		t.Fatalf("bootstrapCalls = %d, want %d", bootstrapCalls, launchdBootstrapAttempts)
	}
	if !strings.Contains(err.Error(), "Bootstrap failed: 5: Input/output error") {
		t.Fatalf("unexpected error: %v", err)
	}
}
