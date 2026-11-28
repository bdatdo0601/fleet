// Package system handles macOS side effects: root-owned files, launchd daemons, nebula, and atomic binary self-updates.
package system

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
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

func rootIDs() (*int, *int) {
	if !IsRoot() {
		return nil, nil
	}
	return new(0), new(0)
}

// OwnerIDs returns the numeric UID and GID for owner when running as root, or (nil, nil) otherwise.
func OwnerIDs(owner string) (*int, *int) {
	if !IsRoot() || owner == "" || owner == "root" {
		return nil, nil
	}
	uid, gid, err := paths.LookupUIDGID(owner)
	if err != nil {
		return nil, nil
	}
	return &uid, &gid
}

const (
	LaunchDaemonsDir = "/Library/LaunchDaemons"
	ALF              = "/usr/libexec/ApplicationFirewall/socketfilterfw"
	StandardPathEnv  = paths.StandardPathEnv
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
			if ee, ok := errors.AsType[*exec.ExitError](err); ok {
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
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
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
	if IsRoot() {
		uid, gid := rootIDs()
		return paths.WriteFileAtomic(path, []byte(content), mode, uid, gid)
	}
	if existing, err := os.ReadFile(path); err == nil && string(existing) == content {
		return false, nil
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
// Removes any stale rule first so updated ad-hoc code signatures are refreshed.
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
		_, _, _, _ = Sudo(false, ALF, "--remove", real)
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
	_ = RestartService(label)
}

// RestartService kickstarts a launchd system service and returns any error.
func RestartService(label string) error {
	_, stderrStr, rc, err := Sudo(false, "launchctl", "kickstart", "-k", "system/"+label)
	if err != nil || rc != 0 {
		return fmt.Errorf("launchctl kickstart system/%s failed: %w (%s)", label, err, strings.TrimSpace(stderrStr))
	}
	return nil
}

// Reload sends SIGHUP to a launchd system service for live configuration reloads.
func Reload(label string) error {
	_, stderrStr, rc, err := Sudo(false, "launchctl", "kill", "HUP", "system/"+label)
	if err != nil || rc != 0 {
		return fmt.Errorf("launchctl kill HUP system/%s failed: %w (%s)", label, err, strings.TrimSpace(stderrStr))
	}
	return nil
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
	WatchPaths     []string
}

// AgentOpts configures a per-user launchd LaunchAgent plist.
type AgentOpts struct {
	Label     string
	Args      []string
	LogPath   string
	WorkDir   string
	KeepAlive bool
	ExtraPath string
	PathEnv   string
	Shell     string
}

type plistSpec struct {
	Label         string
	Args          []string
	Env           map[string]string
	Interval      int
	KeepAlive     bool
	LogPath       string
	User          string
	WorkDir       string
	LimitLoadType string
	WatchPaths    []string
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func buildPlistXML(spec plistSpec) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")

	if len(spec.Env) > 0 {
		envKeys := make([]string, 0, len(spec.Env))
		for k := range spec.Env {
			envKeys = append(envKeys, k)
		}
		sort.Strings(envKeys)
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, k := range envKeys {
			b.WriteString("\t\t<key>" + xmlEscape(k) + "</key>\n")
			b.WriteString("\t\t<string>" + xmlEscape(spec.Env[k]) + "</string>\n")
		}
		b.WriteString("\t</dict>\n")
	}

	if spec.KeepAlive {
		b.WriteString("\t<key>KeepAlive</key>\n\t<true/>\n")
	}
	b.WriteString("\t<key>Label</key>\n\t<string>" + xmlEscape(spec.Label) + "</string>\n")
	if spec.LimitLoadType != "" {
		b.WriteString("\t<key>LimitLoadToSessionType</key>\n\t<string>" + xmlEscape(spec.LimitLoadType) + "</string>\n")
	}
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range spec.Args {
		b.WriteString("\t\t<string>" + xmlEscape(a) + "</string>\n")
	}
	b.WriteString("\t</array>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	if spec.LogPath != "" {
		b.WriteString("\t<key>StandardErrorPath</key>\n\t<string>" + xmlEscape(spec.LogPath) + "</string>\n")
		b.WriteString("\t<key>StandardOutPath</key>\n\t<string>" + xmlEscape(spec.LogPath) + "</string>\n")
	}
	if spec.Interval > 0 {
		fmt.Fprintf(&b, "\t<key>StartInterval</key>\n\t<integer>%d</integer>\n", spec.Interval)
	}
	if spec.User != "" {
		b.WriteString("\t<key>UserName</key>\n\t<string>" + xmlEscape(spec.User) + "</string>\n")
	}
	if len(spec.WatchPaths) > 0 {
		b.WriteString("\t<key>WatchPaths</key>\n\t<array>\n")
		for _, wp := range spec.WatchPaths {
			b.WriteString("\t\t<string>" + xmlEscape(wp) + "</string>\n")
		}
		b.WriteString("\t</array>\n")
	}
	if spec.WorkDir != "" {
		b.WriteString("\t<key>WorkingDirectory</key>\n\t<string>" + xmlEscape(spec.WorkDir) + "</string>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// BuildDaemonPlist renders a valid Apple XML plist for a LaunchDaemon.
func BuildDaemonPlist(opts DaemonOpts) (string, error) {
	env := map[string]string{
		"PATH": StandardPathEnv,
	}
	if opts.User != "" {
		if h := paths.HomeForUser(opts.User); h != "" {
			env["HOME"] = h
			env["PATH"] = paths.MergePathEnv(filepath.Join(h, ".local", "bin"), StandardPathEnv)
		} else {
			return "", fmt.Errorf("lookup home for user %q failed", opts.User)
		}
	}
	return buildPlistXML(plistSpec{
		Label:      opts.Label,
		Args:       opts.Args,
		Env:        env,
		Interval:   opts.Interval,
		KeepAlive:  opts.KeepAlive,
		LogPath:    opts.LogPath,
		User:       opts.User,
		WorkDir:    opts.WorkDir,
		WatchPaths: opts.WatchPaths,
	}), nil
}

// BuildAgentPlist renders a valid Apple XML plist for a per-user LaunchAgent.
func BuildAgentPlist(opts AgentOpts) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	if home == "" {
		return "", fmt.Errorf("cannot determine user home directory")
	}
	pathEnv := paths.MergePathEnv(opts.ExtraPath, opts.PathEnv, filepath.Join(home, ".local", "bin"), StandardPathEnv)
	env := map[string]string{
		"HOME": home,
		"PATH": pathEnv,
	}
	if opts.Shell != "" {
		env["SHELL"] = opts.Shell
	}
	return buildPlistXML(plistSpec{
		Label:         opts.Label,
		Args:          opts.Args,
		Env:           env,
		KeepAlive:     opts.KeepAlive,
		LogPath:       opts.LogPath,
		WorkDir:       opts.WorkDir,
		LimitLoadType: "Aqua",
	}), nil
}

func reloadDaemon(label, plistPath string) error {
	_, _, _, _ = Sudo(false, "launchctl", "bootout", "system/"+label)
	for range 20 {
		if !LaunchdLoaded(label) {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	var lastStderr string
	var lastErr error
	for range 5 {
		_, stderrStr, rc, err := Sudo(false, "launchctl", "bootstrap", "system", plistPath)
		if err == nil && rc == 0 {
			return nil
		}
		lastStderr = stderrStr
		lastErr = err
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("launchctl bootstrap %s failed: %w (%s)", plistPath, lastErr, strings.TrimSpace(lastStderr))
}

// InstallDaemon writes the LaunchDaemon plist and loads/restarts it in launchd.
func InstallDaemon(opts DaemonOpts) error {
	plist, err := BuildDaemonPlist(opts)
	if err != nil {
		return err
	}
	plistPath := filepath.Join(LaunchDaemonsDir, opts.Label+".plist")
	changed, err := WriteRootFile(plistPath, plist, 0o644)
	if err != nil {
		return err
	}
	if opts.ReloadDetached {
		return ReloadDaemonDetached(opts.Label, plistPath)
	}
	if !changed && LaunchdLoaded(opts.Label) {
		Restart(opts.Label)
		return nil
	}
	return reloadDaemon(opts.Label, plistPath)
}

// ReloadDaemonDetached reloads a LaunchDaemon in a detached session after a 1s delay
// so a running sync daemon does not SIGTERM itself mid-execution.
func ReloadDaemonDetached(label, plistPath string) error {
	script := `sleep 1
launchctl bootout "system/$1" >/dev/null 2>&1 || true
i=0
while [ "$i" -lt 20 ] && launchctl print "system/$1" >/dev/null 2>&1; do
  sleep 0.15
  i=$((i + 1))
done
i=0
while [ "$i" -lt 5 ]; do
  launchctl bootstrap system "$2" >/dev/null 2>&1 && exit 0
  sleep 0.3
  i=$((i + 1))
done
exit 1`
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
func InstallSyncDaemon(binPath, workDir string, reloadDetached bool, watchPaths ...string) error {
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
		WatchPaths:     watchPaths,
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

// InstallDDNSDaemon installs/updates the lighthouse `fleet ddns run` LaunchDaemon.
// stateDir is passed explicitly so ddns.log stays in the admin user's ~/.fleet even when run as root.
func InstallDDNSDaemon(binPath, username, stateDir, workDir string) error {
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
		Label:    paths.DDNSLabel,
		Args:     []string{binPath, "ddns", "run"},
		User:     username,
		Interval: paths.SyncIntervalSeconds,
		LogPath:  filepath.Join(stateDir, "ddns.log"),
		WorkDir:  workDir,
	})
}

// InstallSlackerDaemon installs/updates the lighthouse `fleet slacker listen` LaunchDaemon.
func InstallSlackerDaemon(binPath, username, stateDir, workDir string) error {
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
		Label:     paths.SlackerLabel,
		Args:      []string{binPath, "slacker", "listen"},
		User:      username,
		KeepAlive: true,
		LogPath:   filepath.Join(stateDir, "slacker.log"),
		WorkDir:   workDir,
	})
}

// InstallEdgeDaemon installs/updates the root-owned `fleet edge` LaunchDaemon (HTTPS :443 + mesh DNS :53).
func InstallEdgeDaemon(binPath, stateDir, workDir string) error {
	if binPath == "" {
		binPath = paths.InstalledBinaryPath()
	}
	if stateDir == "" {
		stateDir = paths.StateDir()
	}
	if workDir == "" {
		workDir = stateDir
	}
	return InstallDaemon(DaemonOpts{
		Label:     paths.EdgeLabel,
		Args:      []string{binPath, "edge", "--state-dir", stateDir},
		KeepAlive: true,
		LogPath:   "/var/log/fleet-edge.log",
		WorkDir:   workDir,
	})
}

// StartDaemon kickstarts or bootstraps a system LaunchDaemon.
func StartDaemon(label string) error {
	plistPath := filepath.Join(LaunchDaemonsDir, label+".plist")
	if LaunchdLoaded(label) {
		_, stderrStr, rc, err := Sudo(false, "launchctl", "kickstart", "-k", "system/"+label)
		if err != nil || rc != 0 {
			return fmt.Errorf("launchctl kickstart system/%s failed: %w (%s)", label, err, strings.TrimSpace(stderrStr))
		}
		return nil
	}
	if _, err := os.Stat(plistPath); err != nil {
		return fmt.Errorf("LaunchDaemon %s is not installed (%s)", label, plistPath)
	}
	return reloadDaemon(label, plistPath)
}

// StopDaemon boots out a system LaunchDaemon without removing its plist.
func StopDaemon(label string) error {
	if !LaunchdLoaded(label) {
		return nil
	}
	_, stderrStr, rc, err := Sudo(false, "launchctl", "bootout", "system/"+label)
	if err != nil || rc != 0 {
		if !LaunchdLoaded(label) {
			return nil
		}
		return fmt.Errorf("launchctl bootout system/%s failed: %w (%s)", label, err, strings.TrimSpace(stderrStr))
	}
	return nil
}

// UninstallDaemon stops and removes a system LaunchDaemon plist.
func UninstallDaemon(label string) error {
	if err := StopDaemon(label); err != nil {
		return err
	}
	plistPath := filepath.Join(LaunchDaemonsDir, label+".plist")
	if _, err := os.Stat(plistPath); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if IsRoot() {
		return os.Remove(plistPath)
	}
	_, stderrStr, rc, err := Sudo(false, "rm", "-f", plistPath)
	if err != nil || rc != 0 {
		return fmt.Errorf("remove %s failed: %w (%s)", plistPath, err, strings.TrimSpace(stderrStr))
	}
	return nil
}

func launchAgentDomain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

// LaunchAgentPath returns the per-user LaunchAgent plist path for label.
func LaunchAgentPath(label string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist")
}

// UserAgentLoaded reports whether gui/<uid>/<label> is currently loaded in launchd.
func UserAgentLoaded(label string) bool {
	err := exec.Command("launchctl", "print", launchAgentDomain()+"/"+label).Run()
	return err == nil
}

// InstallUserAgent writes the per-user LaunchAgent plist and bootstraps it in gui/<uid>.
func InstallUserAgent(opts AgentOpts) error {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	if opts.Shell == "" {
		opts.Shell = paths.ResolveUserShell(context.Background(), "")
	}
	if opts.PathEnv == "" {
		if probed, pErr := paths.LoginShellPath(context.Background(), opts.Shell, home, ""); pErr == nil && probed != "" {
			opts.PathEnv = probed
		} else {
			opts.PathEnv = os.Getenv("PATH")
		}
	}
	plist, err := BuildAgentPlist(opts)
	if err != nil {
		return err
	}
	plistPath := LaunchAgentPath(opts.Label)
	changed, err := paths.WriteFileAtomic(plistPath, []byte(plist), 0o644, nil, nil)
	if err != nil {
		return err
	}
	if opts.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(opts.LogPath), 0o755); err != nil {
			return err
		}
	}
	if !changed && UserAgentLoaded(opts.Label) {
		return StartUserAgent(opts.Label)
	}
	return reloadUserAgent(opts.Label, plistPath)
}

func reloadUserAgent(label, plistPath string) error {
	domain := launchAgentDomain()
	_ = exec.Command("launchctl", "bootout", domain+"/"+label).Run()
	for range 20 {
		if !UserAgentLoaded(label) {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	var lastOut string
	var lastErr error
	for range 5 {
		out, err := exec.Command("launchctl", "bootstrap", domain, plistPath).CombinedOutput()
		if err == nil {
			return nil
		}
		lastOut = string(out)
		lastErr = err
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("launchctl bootstrap %s failed: %w (%s)", plistPath, lastErr, strings.TrimSpace(lastOut))
}

// StartUserAgent kickstarts or bootstraps a per-user LaunchAgent.
func StartUserAgent(label string) error {
	domain := launchAgentDomain()
	if UserAgentLoaded(label) {
		if out, err := exec.Command("launchctl", "kickstart", "-k", domain+"/"+label).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl kickstart %s/%s failed: %w (%s)", domain, label, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	plistPath := LaunchAgentPath(label)
	if _, err := os.Stat(plistPath); err != nil {
		return fmt.Errorf("LaunchAgent %s is not installed (%s)", label, plistPath)
	}
	return reloadUserAgent(label, plistPath)
}

// StopUserAgent boots out a per-user LaunchAgent without removing its plist.
func StopUserAgent(label string) error {
	if !UserAgentLoaded(label) {
		return nil
	}
	domain := launchAgentDomain()
	out, err := exec.Command("launchctl", "bootout", domain+"/"+label).CombinedOutput()
	if err != nil && UserAgentLoaded(label) {
		return fmt.Errorf("launchctl bootout %s/%s failed: %w (%s)", domain, label, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// UninstallUserAgent stops and removes a per-user LaunchAgent plist.
func UninstallUserAgent(label string) error {
	if err := StopUserAgent(label); err != nil {
		return err
	}
	plistPath := LaunchAgentPath(label)
	if err := os.Remove(plistPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func parseLaunchctlPID(out string) int {
	for line := range strings.SplitSeq(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "pid = ") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(trimmed, "pid = ")))
		if err == nil && pid > 0 {
			return pid
		}
	}
	return 0
}

func parseProcessStartTime(raw string) (time.Time, error) {
	s := strings.Join(strings.Fields(strings.TrimSpace(raw)), " ")
	if s == "" {
		return time.Time{}, fmt.Errorf("empty process start time")
	}
	return time.ParseInLocation("Mon Jan 2 15:04:05 2006", s, time.Local)
}

// RestartStaleUserAgent kickstarts gui/<uid>/<label> if it is running a process started before binPath's modtime.
func RestartStaleUserAgent(owner, label, binPath string) (bool, error) {
	if owner == "" || owner == "root" || label == "" || binPath == "" {
		return false, nil
	}
	uid, _, err := paths.LookupUIDGID(owner)
	if err != nil {
		return false, nil
	}
	fi, err := os.Stat(binPath)
	if err != nil {
		return false, nil
	}
	target := fmt.Sprintf("gui/%d/%s", uid, label)
	out, err := exec.Command("launchctl", "print", target).CombinedOutput()
	if err != nil {
		return false, nil
	}
	pid := parseLaunchctlPID(string(out))
	if pid <= 0 {
		return false, nil
	}
	psCmd := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	psCmd.Env = append(os.Environ(), "LC_ALL=C")
	psOut, err := psCmd.CombinedOutput()
	if err != nil {
		return false, nil
	}
	procStart, err := parseProcessStartTime(string(psOut))
	if err != nil {
		return false, nil
	}
	if !procStart.Before(fi.ModTime()) {
		return false, nil
	}
	if kOut, kErr := exec.Command("launchctl", "kickstart", "-k", target).CombinedOutput(); kErr != nil {
		return false, fmt.Errorf("launchctl kickstart %s failed: %w (%s)", target, kErr, strings.TrimSpace(string(kOut)))
	}
	return true, nil
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
		return fmt.Errorf("smoke test `%s version` failed: %w (%s)", binPath, err, strings.TrimSpace(string(out)))
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
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	uid, gid := rootIDs()
	written, err := paths.WriteFileAtomic(dst, data, mode, uid, gid)
	if err != nil {
		return err
	}
	if !written {
		_ = os.Chmod(dst, mode)
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
	maps.Copy(envMap, envOverrides)

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
	uid, gid := OwnerIDs(owner)
	_, err := paths.WriteFileAtomic(path, data, mode, uid, gid)
	return err
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func resolveLighthousePullTarget(repoDir, owner string, gitEnv map[string]string) (string, string) {
	branchOut, _, err := RunAsOwner(owner, repoDir, gitEnv, 15*time.Second, "git", "-C", repoDir, "symbolic-ref", "--short", "HEAD")
	branch := strings.TrimSpace(branchOut)
	if err != nil || branch == "" {
		return "", ""
	}
	remote := "origin"
	if remoteOut, _, err := RunAsOwner(owner, repoDir, gitEnv, 15*time.Second, "git", "-C", repoDir, "config", "--get", "branch."+branch+".remote"); err == nil {
		if r := strings.TrimSpace(remoteOut); r != "" {
			remote = r
		}
	}
	mergeRef := branch
	if mergeOut, _, err := RunAsOwner(owner, repoDir, gitEnv, 15*time.Second, "git", "-C", repoDir, "config", "--get-all", "branch."+branch+".merge"); err == nil {
		var unique []string
		seen := make(map[string]struct{})
		for line := range strings.SplitSeq(mergeOut, "\n") {
			ref := strings.TrimSpace(line)
			if ref == "" {
				continue
			}
			if _, ok := seen[ref]; !ok {
				seen[ref] = struct{}{}
				unique = append(unique, ref)
			}
		}
		if len(unique) == 1 {
			mergeRef = unique[0]
		}
	}
	return remote, mergeRef
}

func pullLighthouseRepo(repoDir, owner string) string {
	gitEnv := map[string]string{"GIT_TERMINAL_PROMPT": "0", "GIT_SSH_COMMAND": "ssh -o BatchMode=yes"}
	if !fileExists(filepath.Join(repoDir, ".git")) {
		return ""
	}
	args := []string{"-C", repoDir, "-c", "pull.rebase=false", "pull", "--no-rebase", "--ff-only", "-q"}
	if remote, mergeRef := resolveLighthousePullTarget(repoDir, owner, gitEnv); remote != "" && mergeRef != "" {
		args = append(args, remote, mergeRef)
	}
	if _, pullErrOut, err := RunAsOwner(owner, repoDir, gitEnv, 60*time.Second, "git", args...); err != nil {
		detail := cmp.Or(strings.TrimSpace(pullErrOut), "git pull failed")
		_, _ = fmt.Fprintf(os.Stderr, "self-update skipped: %s\n", detail)
	}
	if out, _, err := RunAsOwner(owner, repoDir, gitEnv, 15*time.Second, "git", "-C", repoDir, "rev-parse", "HEAD"); err == nil {
		return strings.TrimSpace(out)
	}
	return ""
}

func buildDarwinBinaries(repoDir, binDir, owner, headSHA, builtAt string) (map[string]string, error) {
	goBin := FindGoBinary()
	if goBin == "" {
		return nil, fmt.Errorf("go not found on lighthouse (install with: brew install go)")
	}
	uid, gid := OwnerIDs(owner)
	if err := paths.EnsureDir(binDir, 0o755, uid, gid); err != nil {
		return nil, err
	}
	ldflags := fmt.Sprintf(
		"-s -w -X github.com/bdatdo0601/fleet/internal/version.Commit=%s -X github.com/bdatdo0601/fleet/internal/version.BuiltAt=%s",
		cmp.Or(headSHA, "dev"), builtAt,
	)
	tmpBins := map[string]string{}
	for _, arch := range []string{"arm64", "amd64"} {
		tmpOut := filepath.Join(binDir, fmt.Sprintf(".fleet-darwin-%s.tmp", arch))
		tmpBins[arch] = tmpOut
		buildEnv := map[string]string{"CGO_ENABLED": "0", "GOOS": "darwin", "GOARCH": arch, "GOTOOLCHAIN": "auto"}
		if _, stderrStr, err := RunAsOwner(owner, repoDir, buildEnv, 240*time.Second, goBin, "build", "-ldflags", ldflags, "-o", tmpOut, "./cmd/fleet"); err != nil {
			for _, p := range tmpBins {
				_ = os.Remove(p)
			}
			return nil, fmt.Errorf("go build darwin/%s failed: %w (%s)", arch, err, strings.TrimSpace(stderrStr))
		}
	}
	return tmpBins, nil
}

func restartAndVerifyLighthouse(opts LighthouseUpdateOpts, targetBin, prevBin, owner, stateDir string) error {
	if opts.SkipDaemonControl {
		return nil
	}
	ensureServe := func() {
		if LaunchdLoaded(paths.ServeLabel) {
			Restart(paths.ServeLabel)
		} else {
			_ = InstallServeDaemon(targetBin, owner, stateDir, stateDir)
		}
	}
	if err := InstallServeDaemon(targetBin, owner, stateDir, stateDir); err != nil {
		_ = RollbackBinary(targetBin, prevBin)
		ensureServe()
		return err
	}
	if LaunchdLoaded(paths.DDNSLabel) {
		_ = InstallDDNSDaemon(targetBin, owner, stateDir, stateDir)
	}
	if LaunchdLoaded(paths.SlackerLabel) {
		_ = InstallSlackerDaemon(targetBin, owner, stateDir, stateDir)
	}
	if LaunchdLoaded(paths.EdgeLabel) {
		_ = InstallEdgeDaemon(targetBin, stateDir, stateDir)
	}
	localAPI := cmp.Or(opts.LocalAPI, fmt.Sprintf("http://127.0.0.1:%d", paths.APIPort()))
	if !waitHealthy(localAPI, opts.HealthcheckTimeout, opts.HealthcheckFn) {
		_ = RollbackBinary(targetBin, prevBin)
		ensureServe()
		return fmt.Errorf("new fleet serve failed health check; rolled back")
	}
	return nil
}

func publishManifest(binDir, owner, headSHA, builtAt string, tmpBins map[string]string) error {
	uid, gid := OwnerIDs(owner)
	manifest := core.BinaryManifest{Version: version.Version, Commit: headSHA, BuiltAt: builtAt, Binaries: map[string]core.BinaryEntry{}}
	for _, arch := range []string{"arm64", "amd64"} {
		finalName := "fleet-darwin-" + arch
		finalPath := filepath.Join(binDir, finalName)
		if err := os.Rename(tmpBins[arch], finalPath); err != nil {
			return err
		}
		_ = os.Chmod(finalPath, 0o755)
		if uid != nil && gid != nil {
			_ = os.Chown(finalPath, *uid, *gid)
		}
		sha, sz, err := FileSHA256(finalPath)
		if err != nil {
			return err
		}
		manifest.Binaries["darwin/"+arch] = core.BinaryEntry{File: finalName, SHA256: sha, Size: sz}
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return writeOwnerFile(filepath.Join(binDir, "manifest.json"), append(manifestBytes, '\n'), 0o644, owner)
}

// SelfUpdateLighthouse pulls git, builds darwin/arm64 & darwin/amd64 when needed,
// installs and healthchecks `fleet serve` (rolling back on failure), and publishes manifest.json only when healthy.
func SelfUpdateLighthouse(opts LighthouseUpdateOpts) (bool, error) {
	if opts.RepoDir == "" || !fileExists(filepath.Join(opts.RepoDir, "go.mod")) {
		return false, nil
	}
	owner := opts.Owner
	if owner == "" {
		owner, _ = PathOwner(opts.RepoDir)
	}
	stateDir := cmp.Or(opts.StateDir, paths.StateDirForOwner(owner))
	targetBin := cmp.Or(opts.TargetBin, paths.InstalledBinaryPath())
	symlinkPath := cmp.Or(opts.SymlinkPath, paths.SymlinkPath())

	headSHA := pullLighthouseRepo(opts.RepoDir, owner)
	binDir := filepath.Join(stateDir, "bin")
	var existing core.BinaryManifest
	manifestOK := false
	if raw, err := os.ReadFile(filepath.Join(binDir, "manifest.json")); err == nil {
		manifestOK = json.Unmarshal(raw, &existing) == nil && len(existing.Binaries) > 0
	}
	if !opts.Rebuild && fileExists(targetBin) && manifestOK && (headSHA == "" || existing.Commit == headSHA) {
		return false, nil
	}

	builtAt := time.Now().UTC().Format(time.RFC3339)
	tmpBins, err := buildDarwinBinaries(opts.RepoDir, binDir, owner, headSHA, builtAt)
	if err != nil {
		return false, err
	}
	cleanupTmp := func() {
		for _, p := range tmpBins {
			_ = os.Remove(p)
		}
	}
	hostTmp, ok := tmpBins[runtime.GOARCH]
	if !ok {
		cleanupTmp()
		return false, fmt.Errorf("unsupported host arch %s", runtime.GOARCH)
	}
	prevBin, err := InstallBinaryAtomic(hostTmp, targetBin, symlinkPath)
	if err != nil {
		cleanupTmp()
		return false, err
	}
	if err := restartAndVerifyLighthouse(opts, targetBin, prevBin, owner, stateDir); err != nil {
		cleanupTmp()
		return false, err
	}
	if err := publishManifest(binDir, owner, headSHA, builtAt, tmpBins); err != nil {
		cleanupTmp()
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
