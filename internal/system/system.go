// Package system handles macOS side effects: root-owned files, launchd daemons, nebula, and atomic binary self-updates.
package system

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bdatdo0601/fleet/internal/client"
	"github.com/bdatdo0601/fleet/internal/core"
	"github.com/bdatdo0601/fleet/internal/paths"
	"github.com/bdatdo0601/fleet/internal/version"
)

const (
	LaunchDaemonsDir = "/Library/LaunchDaemons"
	ALF              = "/usr/libexec/ApplicationFirewall/socketfilterfw"
	StandardPathEnv  = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
)

// IsRoot reports whether the current effective UID is 0.
func IsRoot() bool {
	return os.Geteuid() == 0
}

// Sudo runs args directly when root, or via `sudo` otherwise.
// When check is true, stdout/stderr are attached interactively; when false, output is captured.
func Sudo(check bool, args ...string) (string, string, int, error) {
	if len(args) == 0 {
		return "", "", 0, nil
	}
	var name string
	var cmdArgs []string
	if IsRoot() {
		name = args[0]
		cmdArgs = args[1:]
	} else {
		name = "sudo"
		cmdArgs = args
	}
	cmd := exec.Command(name, cmdArgs...)
	if check {
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			code := 1
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			}
			return "", "", code, err
		}
		return "", "", 0, nil
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		code = 1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	return stdout.String(), stderr.String(), code, err
}

// WriteRootFile writes content to path owned by root:wheel with mode; returns true if content changed.
func WriteRootFile(path, content string, mode os.FileMode) (bool, error) {
	if mode == 0 {
		mode = 0o644
	}
	if existing, err := os.ReadFile(path); err == nil && string(existing) == content {
		return false, nil
	}
	if IsRoot() {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return false, err
		}
		tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.%d.tmp", filepath.Base(path), os.Getpid()))
		if err := os.WriteFile(tmp, []byte(content), mode); err != nil {
			return false, err
		}
		_ = os.Chmod(tmp, mode)
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return false, err
		}
		return true, nil
	}
	tmpFile, err := os.CreateTemp("", "fleet-root-*")
	if err != nil {
		return false, err
	}
	tmpName := tmpFile.Name()
	_, writeErr := tmpFile.WriteString(content)
	closeErr := tmpFile.Close()
	defer func() { _ = os.Remove(tmpName) }()
	if writeErr != nil {
		return false, writeErr
	}
	if closeErr != nil {
		return false, closeErr
	}
	if _, _, _, err := Sudo(true, "mkdir", "-p", filepath.Dir(path)); err != nil {
		return false, err
	}
	if _, _, _, err := Sudo(true, "install", "-m", fmt.Sprintf("%o", mode), "-o", "root", "-g", "wheel", tmpName, path); err != nil {
		return false, err
	}
	return true, nil
}

// EnsureNebula ensures nebula and nebula-cert are installed via Homebrew.
func EnsureNebula() error {
	if _, err := exec.LookPath("nebula"); err == nil {
		if _, err2 := exec.LookPath("nebula-cert"); err2 == nil {
			return nil
		}
	}
	fmt.Println("==> installing nebula (brew)")
	cmd := exec.Command("brew", "install", "nebula")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// FirewallAllow permits binaries through the macOS Application Firewall when enabled.
func FirewallAllow(binaries ...string) {
	if _, err := os.Stat(ALF); err != nil {
		return
	}
	out, err := exec.Command(ALF, "--getglobalstate").Output()
	if err != nil || !strings.Contains(string(out), "enabled") {
		return
	}
	for _, bin := range binaries {
		if bin == "" {
			continue
		}
		real := bin
		if resolved, err := filepath.EvalSymlinks(bin); err == nil {
			real = resolved
		} else if abs, err := filepath.Abs(bin); err == nil {
			real = abs
		}
		_, _, _, _ = Sudo(false, ALF, "--add", real)
		_, _, _, _ = Sudo(false, ALF, "--unblockapp", real)
	}
}

// NebulaLabel returns the launchd label Homebrew gave the nebula service, or empty if absent.
func NebulaLabel(daemonsDir string) string {
	if daemonsDir == "" {
		daemonsDir = LaunchDaemonsDir
	}
	for _, label := range paths.NebulaLabels {
		if _, err := os.Stat(filepath.Join(daemonsDir, label+".plist")); err == nil {
			return label
		}
	}
	return ""
}

// LaunchdLoaded reports whether system/<label> is currently loaded in launchd.
func LaunchdLoaded(label string) bool {
	_, _, rc, _ := Sudo(false, "launchctl", "print", "system/"+label)
	return rc == 0
}

// Restart kickstarts a launchd system service.
func Restart(label string) {
	_, _, _, _ = Sudo(false, "launchctl", "kickstart", "-k", "system/"+label)
}

// StartNebula loads or restarts the Homebrew nebula service.
func StartNebula() error {
	if neb, err := exec.LookPath("nebula"); err == nil {
		FirewallAllow(neb)
	}
	if label := NebulaLabel(""); label != "" && LaunchdLoaded(label) {
		Restart(label)
		return nil
	}
	_, _, _, err := Sudo(true, "brew", "services", "start", "nebula")
	return err
}

// DaemonOpts configures a launchd LaunchDaemon plist.
type DaemonOpts struct {
	Label          string
	Args           []string
	User           string
	Interval       int
	KeepAlive      bool
	LogPath        string
	WorkDir        string
	ReloadDetached bool
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// BuildDaemonPlist renders a valid Apple XML plist for a LaunchDaemon.
func BuildDaemonPlist(opts DaemonOpts) (string, error) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")

	env := map[string]string{
		"PATH": StandardPathEnv,
	}
	if opts.User != "" {
		if h := paths.HomeForUser(opts.User); h != "" {
			env["HOME"] = h
		} else {
			return "", fmt.Errorf("lookup home for user %q failed", opts.User)
		}
	}
	envKeys := make([]string, 0, len(env))
	for k := range env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	for _, k := range envKeys {
		b.WriteString("\t\t<key>" + xmlEscape(k) + "</key>\n")
		b.WriteString("\t\t<string>" + xmlEscape(env[k]) + "</string>\n")
	}
	b.WriteString("\t</dict>\n")

	if opts.KeepAlive {
		b.WriteString("\t<key>KeepAlive</key>\n\t<true/>\n")
	}
	b.WriteString("\t<key>Label</key>\n\t<string>" + xmlEscape(opts.Label) + "</string>\n")
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range opts.Args {
		b.WriteString("\t\t<string>" + xmlEscape(a) + "</string>\n")
	}
	b.WriteString("\t</array>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>StandardErrorPath</key>\n\t<string>" + xmlEscape(opts.LogPath) + "</string>\n")
	b.WriteString("\t<key>StandardOutPath</key>\n\t<string>" + xmlEscape(opts.LogPath) + "</string>\n")
	if opts.Interval > 0 {
		b.WriteString(fmt.Sprintf("\t<key>StartInterval</key>\n\t<integer>%d</integer>\n", opts.Interval))
	}
	if opts.User != "" {
		b.WriteString("\t<key>UserName</key>\n\t<string>" + xmlEscape(opts.User) + "</string>\n")
	}
	if opts.WorkDir != "" {
		b.WriteString("\t<key>WorkingDirectory</key>\n\t<string>" + xmlEscape(opts.WorkDir) + "</string>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.String(), nil
}

// InstallDaemon writes the LaunchDaemon plist and loads/restarts it in launchd.
func InstallDaemon(opts DaemonOpts) error {
	plist, err := BuildDaemonPlist(opts)
	if err != nil {
		return err
	}
	plistPath := filepath.Join(LaunchDaemonsDir, opts.Label+".plist")
	if _, err := WriteRootFile(plistPath, plist, 0o644); err != nil {
		return err
	}
	if opts.ReloadDetached {
		return ReloadDaemonDetached(opts.Label, plistPath)
	}
	_, _, _, _ = Sudo(false, "launchctl", "bootout", "system/"+opts.Label)
	_, _, _, err = Sudo(true, "launchctl", "bootstrap", "system", plistPath)
	return err
}

// ReloadDaemonDetached reloads a LaunchDaemon in a detached session after a 1s delay
// so a running sync daemon does not SIGTERM itself mid-execution.
func ReloadDaemonDetached(label, plistPath string) error {
	script := `sleep 1; launchctl bootout "system/$1" >/dev/null 2>&1 || true; launchctl bootstrap system "$2" >/dev/null 2>&1 || true`
	var name string
	var args []string
	if IsRoot() {
		name = "sh"
		args = []string{"-c", script, "_", label, plistPath}
	} else {
		name = "sudo"
		args = []string{"sh", "-c", script, "_", label, plistPath}
	}
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

// InstallSyncDaemon installs/updates the root 5-minute sync LaunchDaemon.
func InstallSyncDaemon(binPath, workDir string, reloadDetached bool) error {
	if binPath == "" {
		binPath = paths.InstalledBinaryPath()
	}
	if workDir == "" {
		workDir = filepath.Dir(binPath)
	}
	return InstallDaemon(DaemonOpts{
		Label:          paths.SyncLabel,
		Args:           []string{binPath, "sync"},
		Interval:       paths.SyncIntervalSeconds,
		LogPath:        "/var/log/fleet-sync.log",
		WorkDir:        workDir,
		ReloadDetached: reloadDetached,
	})
}

// InstallServeDaemon installs/updates the lighthouse `fleet serve` LaunchDaemon.
// stateDir is passed explicitly so serve.log stays in the admin user's ~/.fleet even when run as root.
func InstallServeDaemon(binPath, username, stateDir, workDir string) error {
	if binPath == "" {
		binPath = paths.InstalledBinaryPath()
	}
	if stateDir == "" {
		stateDir = paths.StateDirForOwner(username)
	}
	if workDir == "" {
		workDir = stateDir
	}
	return InstallDaemon(DaemonOpts{
		Label:     paths.ServeLabel,
		Args:      []string{binPath, "serve"},
		User:      username,
		KeepAlive: true,
		LogPath:   filepath.Join(stateDir, "serve.log"),
		WorkDir:   workDir,
	})
}

// InstallDdnsDaemon installs/updates the lighthouse `fleet ddns run` LaunchDaemon.
// stateDir is passed explicitly so ddns.log stays in the admin user's ~/.fleet even when run as root.
func InstallDdnsDaemon(binPath, username, stateDir, workDir string) error {
	if binPath == "" {
		binPath = paths.InstalledBinaryPath()
	}
	if stateDir == "" {
		stateDir = paths.StateDirForOwner(username)
	}
	if workDir == "" {
		workDir = stateDir
	}
	return InstallDaemon(DaemonOpts{
		Label:    paths.DdnsLabel,
		Args:     []string{binPath, "ddns", "run"},
		User:     username,
		Interval: paths.SyncIntervalSeconds,
		LogPath:  filepath.Join(stateDir, "ddns.log"),
		WorkDir:  workDir,
	})
}

// PathOwner returns the username that owns path on disk.
func PathOwner(p string) (string, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("cannot inspect owner of %s", p)
	}
	u, err := user.LookupId(strconv.Itoa(int(st.Uid)))
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

// LookupUIDGID returns numeric uid/gid for username.
func LookupUIDGID(username string) (int, int, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return 0, 0, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

// FileSHA256 computes lowercase SHA-256 hex digest and size of path.
func FileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// SmokeTestBinary verifies binPath executes `<binPath> version` and exits 0.
func SmokeTestBinary(binPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binPath, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("smoke test `%s version` failed: %v (%s)", binPath, err, strings.TrimSpace(string(out)))
	}
	if !strings.Contains(string(out), "fleet ") {
		return fmt.Errorf("smoke test `%s version` returned unexpected output: %q", binPath, strings.TrimSpace(string(out)))
	}
	return nil
}

// EnsureSymlink atomically ensures linkPath points to target.
func EnsureSymlink(target, linkPath string) error {
	if linkPath == "" {
		return nil
	}
	if cur, err := os.Readlink(linkPath); err == nil && cur == target {
		return nil
	}
	dir := filepath.Dir(linkPath)
	if err := os.MkdirAll(dir, 0o755); err == nil {
		tmp := filepath.Join(dir, fmt.Sprintf(".%s.%d.sym", filepath.Base(linkPath), os.Getpid()))
		_ = os.Remove(tmp)
		if err := os.Symlink(target, tmp); err == nil {
			if err := os.Rename(tmp, linkPath); err == nil {
				return nil
			}
			_ = os.Remove(tmp)
		}
	}
	if _, _, _, err := Sudo(true, "mkdir", "-p", dir); err != nil {
		return err
	}
	_, _, _, err := Sudo(true, "ln", "-sfn", target, linkPath)
	return err
}

func copyFileAtomic(src, dst string, mode os.FileMode) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.%d.tmp", filepath.Base(dst), os.Getpid()))
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	_ = os.Chmod(tmp, mode)
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// InstallBinaryAtomic smoke-tests srcPath, saves a .prev backup if targetPath exists,
// atomically replaces targetPath, updates symlinkPath, and allowlists the binary in macOS ALF.
func InstallBinaryAtomic(srcPath, targetPath, symlinkPath string) (string, error) {
	if targetPath == "" {
		targetPath = paths.InstalledBinaryPath()
	}
	if err := SmokeTestBinary(srcPath); err != nil {
		return "", err
	}
	prevPath := ""
	if _, err := os.Stat(targetPath); err == nil {
		prevCandidate := targetPath + ".prev"
		if copyFileAtomic(targetPath, prevCandidate, 0o755) == nil {
			prevPath = prevCandidate
		} else if !IsRoot() {
			_, _, rc, _ := Sudo(false, "install", "-m", "755", "-o", "root", "-g", "wheel", targetPath, prevCandidate)
			if rc == 0 {
				prevPath = prevCandidate
			}
		}
	}
	if err := copyFileAtomic(srcPath, targetPath, 0o755); err != nil {
		if IsRoot() {
			return prevPath, err
		}
		if _, _, _, sErr := Sudo(true, "mkdir", "-p", filepath.Dir(targetPath)); sErr != nil {
			return prevPath, sErr
		}
		tmpDst := targetPath + fmt.Sprintf(".new.%d", os.Getpid())
		if _, _, _, sErr := Sudo(true, "install", "-m", "755", "-o", "root", "-g", "wheel", srcPath, tmpDst); sErr != nil {
			return prevPath, sErr
		}
		if _, _, _, sErr := Sudo(true, "mv", "-f", tmpDst, targetPath); sErr != nil {
			_, _, _, _ = Sudo(false, "rm", "-f", tmpDst)
			return prevPath, sErr
		}
	}
	if err := EnsureSymlink(targetPath, symlinkPath); err != nil {
		return prevPath, err
	}
	FirewallAllow(targetPath)
	return prevPath, nil
}

// RollbackBinary restores prevPath over targetPath and re-applies FirewallAllow.
func RollbackBinary(targetPath, prevPath string) error {
	if prevPath == "" {
		return nil
	}
	if _, err := os.Stat(prevPath); err != nil {
		return err
	}
	if err := os.Rename(prevPath, targetPath); err != nil {
		if !IsRoot() {
			if _, _, _, sErr := Sudo(true, "mv", "-f", prevPath, targetPath); sErr != nil {
				return sErr
			}
		} else {
			return err
		}
	}
	FirewallAllow(targetPath)
	return nil
}

// RunAsOwner runs name args... as owner (via `sudo -u owner -H` when root), preserving StandardPathEnv.
func RunAsOwner(owner, workDir string, envOverrides map[string]string, timeout time.Duration, name string, args ...string) (string, string, error) {
	ctx := context.Background()
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	envMap := map[string]string{
		"PATH":        StandardPathEnv,
		"GOTOOLCHAIN": "auto",
	}
	if owner != "" && owner != "root" {
		if h := paths.HomeForUser(owner); h != "" {
			envMap["HOME"] = h
		}
	}
	for k, v := range envOverrides {
		envMap[k] = v
	}

	var cmd *exec.Cmd
	if IsRoot() && owner != "" && owner != "root" {
		sudoArgs := []string{"-u", owner, "-H", "env"}
		keys := make([]string, 0, len(envMap))
		for k := range envMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sudoArgs = append(sudoArgs, k+"="+envMap[k])
		}
		sudoArgs = append(sudoArgs, name)
		sudoArgs = append(sudoArgs, args...)
		cmd = exec.CommandContext(ctx, "sudo", sudoArgs...)
	} else {
		cmd = exec.CommandContext(ctx, name, args...)
		envSlice := os.Environ()
		for k, v := range envMap {
			envSlice = append(envSlice, k+"="+v)
		}
		cmd.Env = envSlice
	}
	if workDir != "" {
		cmd.Dir = workDir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// FindGoBinary locates go across StandardPathEnv.
func FindGoBinary() string {
	for _, cand := range []string{"/opt/homebrew/bin/go", "/usr/local/bin/go"} {
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand
		}
	}
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	return ""
}

// LighthouseUpdateOpts configures SelfUpdateLighthouse.
type LighthouseUpdateOpts struct {
	RepoDir            string
	StateDir           string
	Owner              string
	TargetBin          string
	SymlinkPath        string
	LocalAPI           string
	Rebuild            bool
	HealthcheckTimeout time.Duration
	SkipDaemonControl  bool // for unit tests without launchd
	HealthcheckFn      func(apiURL string) bool
}

func waitHealthy(apiURL string, timeout time.Duration, checkFn func(string) bool) bool {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if checkFn == nil {
		hc := &http.Client{Timeout: 2 * time.Second}
		checkFn = func(u string) bool {
			resp, err := hc.Get(strings.TrimRight(u, "/") + "/v1/sync")
			if err != nil {
				return false
			}
			_ = resp.Body.Close()
			return resp.StatusCode == http.StatusOK
		}
	}
	deadline := time.Now().Add(timeout)
	for {
		if checkFn(apiURL) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func writeOwnerFile(path string, data []byte, mode os.FileMode, owner string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.%d.tmp", filepath.Base(path), os.Getpid()))
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	_ = os.Chmod(tmp, mode)
	if IsRoot() && owner != "" && owner != "root" {
		if uid, gid, err := LookupUIDGID(owner); err == nil {
			_ = os.Chown(dir, uid, gid)
			_ = os.Chown(tmp, uid, gid)
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// SelfUpdateLighthouse pulls git, builds darwin/arm64 & darwin/amd64 when needed,
// installs and healthchecks `fleet serve` (rolling back on failure), and publishes manifest.json only when healthy.
func SelfUpdateLighthouse(opts LighthouseUpdateOpts) (bool, error) {
	if opts.RepoDir == "" {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(opts.RepoDir, "go.mod")); err != nil {
		return false, nil
	}
	owner := opts.Owner
	if owner == "" {
		if o, err := PathOwner(opts.RepoDir); err == nil {
			owner = o
		}
	}
	stateDir := opts.StateDir
	if stateDir == "" {
		stateDir = paths.StateDirForOwner(owner)
	}
	targetBin := opts.TargetBin
	if targetBin == "" {
		targetBin = paths.InstalledBinaryPath()
	}
	symlinkPath := opts.SymlinkPath
	if symlinkPath == "" {
		symlinkPath = paths.SymlinkPath()
	}

	gitEnv := map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_SSH_COMMAND":     "ssh -o BatchMode=yes",
	}
	headSHA := ""
	if _, err := os.Stat(filepath.Join(opts.RepoDir, ".git")); err == nil {
		if _, pullErrOut, err := RunAsOwner(owner, opts.RepoDir, gitEnv, 60*time.Second, "git", "-C", opts.RepoDir, "pull", "--ff-only", "-q"); err != nil {
			detail := strings.TrimSpace(pullErrOut)
			if detail == "" {
				detail = "git pull failed"
			}
			_, _ = fmt.Fprintf(os.Stderr, "self-update skipped: %s\n", detail)
		}
		if out, _, err := RunAsOwner(owner, opts.RepoDir, gitEnv, 15*time.Second, "git", "-C", opts.RepoDir, "rev-parse", "HEAD"); err == nil {
			headSHA = strings.TrimSpace(out)
		}
	}

	binDir := filepath.Join(stateDir, "bin")
	manifestPath := filepath.Join(binDir, "manifest.json")
	var existingManifest core.BinaryManifest
	manifestOK := false
	if raw, err := os.ReadFile(manifestPath); err == nil {
		if json.Unmarshal(raw, &existingManifest) == nil && len(existingManifest.Binaries) > 0 {
			manifestOK = true
		}
	}
	_, binStatErr := os.Stat(targetBin)
	targetExists := binStatErr == nil

	needBuild := opts.Rebuild || !targetExists || !manifestOK
	if !needBuild && headSHA != "" && existingManifest.Commit != headSHA {
		needBuild = true
	}
	if !needBuild {
		return false, nil
	}

	goBin := FindGoBinary()
	if goBin == "" {
		return false, fmt.Errorf("go not found on lighthouse (install with: brew install go)")
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return false, err
	}
	if IsRoot() && owner != "" && owner != "root" {
		if uid, gid, err := LookupUIDGID(owner); err == nil {
			_ = os.Chown(stateDir, uid, gid)
			_ = os.Chown(binDir, uid, gid)
		}
	}

	builtAt := time.Now().UTC().Format(time.RFC3339)
	commitVal := headSHA
	if commitVal == "" {
		commitVal = "dev"
	}
	ldflags := fmt.Sprintf(
		"-s -w -X github.com/bdatdo0601/fleet/internal/version.Commit=%s -X github.com/bdatdo0601/fleet/internal/version.BuiltAt=%s",
		commitVal, builtAt,
	)

	arches := []string{"arm64", "amd64"}
	tmpBins := map[string]string{}
	cleanupTmp := func() {
		for _, p := range tmpBins {
			_ = os.Remove(p)
		}
	}
	for _, arch := range arches {
		tmpOut := filepath.Join(binDir, fmt.Sprintf(".fleet-darwin-%s.tmp", arch))
		tmpBins[arch] = tmpOut
		buildEnv := map[string]string{
			"CGO_ENABLED": "0",
			"GOOS":        "darwin",
			"GOARCH":      arch,
			"GOTOOLCHAIN": "auto",
		}
		_, stderrStr, err := RunAsOwner(
			owner, opts.RepoDir, buildEnv, 240*time.Second,
			goBin, "build", "-ldflags", ldflags, "-o", tmpOut, "./cmd/fleet",
		)
		if err != nil {
			cleanupTmp()
			return false, fmt.Errorf("go build darwin/%s failed: %v (%s)", arch, err, strings.TrimSpace(stderrStr))
		}
	}

	hostArch := runtime.GOARCH
	hostTmp, ok := tmpBins[hostArch]
	if !ok {
		cleanupTmp()
		return false, fmt.Errorf("unsupported host arch %s", hostArch)
	}
	prevBin, err := InstallBinaryAtomic(hostTmp, targetBin, symlinkPath)
	if err != nil {
		cleanupTmp()
		return false, err
	}

	if !opts.SkipDaemonControl {
		if err := InstallServeDaemon(targetBin, owner, stateDir, opts.RepoDir); err != nil {
			_ = RollbackBinary(targetBin, prevBin)
			Restart(paths.ServeLabel)
			cleanupTmp()
			return false, err
		}
		if LaunchdLoaded(paths.DdnsLabel) {
			_ = InstallDdnsDaemon(targetBin, owner, stateDir, opts.RepoDir)
		}
		localAPI := opts.LocalAPI
		if localAPI == "" {
			localAPI = fmt.Sprintf("http://127.0.0.1:%d", paths.APIPort())
		}
		if !waitHealthy(localAPI, opts.HealthcheckTimeout, opts.HealthcheckFn) {
			_ = RollbackBinary(targetBin, prevBin)
			Restart(paths.ServeLabel)
			cleanupTmp()
			return false, fmt.Errorf("new fleet serve failed health check; rolled back")
		}
	}

	manifest := core.BinaryManifest{
		Version:  version.Version,
		Commit:   headSHA,
		BuiltAt:  builtAt,
		Binaries: map[string]core.BinaryEntry{},
	}
	for _, arch := range arches {
		finalName := fmt.Sprintf("fleet-darwin-%s", arch)
		finalPath := filepath.Join(binDir, finalName)
		if err := os.Rename(tmpBins[arch], finalPath); err != nil {
			cleanupTmp()
			return false, err
		}
		_ = os.Chmod(finalPath, 0o755)
		if IsRoot() && owner != "" && owner != "root" {
			if uid, gid, err := LookupUIDGID(owner); err == nil {
				_ = os.Chown(finalPath, uid, gid)
			}
		}
		sha, sz, err := FileSHA256(finalPath)
		if err != nil {
			return false, err
		}
		manifest.Binaries["darwin/"+arch] = core.BinaryEntry{
			File:   finalName,
			SHA256: sha,
			Size:   sz,
		}
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return false, err
	}
	if err := writeOwnerFile(manifestPath, append(manifestBytes, '\n'), 0o644, owner); err != nil {
		return false, err
	}
	return true, nil
}

// ClientUpdateOpts configures SelfUpdateClient.
type ClientUpdateOpts struct {
	APIURL      string
	ExpectedSHA string
	TargetBin   string
	SymlinkPath string
}

// SelfUpdateClient downloads and atomically installs a new binary from the lighthouse when ExpectedSHA differs.
func SelfUpdateClient(opts ClientUpdateOpts) (bool, error) {
	if opts.ExpectedSHA == "" {
		return false, nil
	}
	targetBin := opts.TargetBin
	if targetBin == "" {
		targetBin = paths.InstalledBinaryPath()
	}
	symlinkPath := opts.SymlinkPath
	if symlinkPath == "" {
		symlinkPath = paths.SymlinkPath()
	}
	if curSHA, _, err := FileSHA256(targetBin); err == nil && strings.EqualFold(curSHA, opts.ExpectedSHA) {
		_ = EnsureSymlink(targetBin, symlinkPath)
		return false, nil
	}

	tmpDir, err := os.MkdirTemp("", "fleet-dl-*")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	dlPath := filepath.Join(tmpDir, "fleet.new")
	dlQuery := fmt.Sprintf("/v1/binary?os=%s&arch=%s", runtime.GOOS, runtime.GOARCH)
	gotSHA, _, err := client.Download(opts.APIURL, dlQuery, dlPath, 60*time.Second)
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(gotSHA, opts.ExpectedSHA) {
		return false, fmt.Errorf("downloaded binary SHA mismatch: expected %s, got %s", opts.ExpectedSHA, gotSHA)
	}
	if err := os.Chmod(dlPath, 0o755); err != nil {
		return false, err
	}
	if _, err := InstallBinaryAtomic(dlPath, targetBin, symlinkPath); err != nil {
		return false, err
	}
	return true, nil
}
