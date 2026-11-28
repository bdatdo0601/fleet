package system

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bdatdo0601/fleet/internal/paths"
)

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
		return RestartService(label)
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

const (
	launchdUnloadPollInterval = 150 * time.Millisecond
	launchdUnloadWaitAttempts = 140 // ~21s, covering macOS launchd's default 20s ExitTimeOut
	launchdBootstrapAttempts  = 20
	launchdBootstrapInterval  = 500 * time.Millisecond
)

func reloadLaunchdService(
	plistPath string,
	bootout func(),
	isLoaded func() bool,
	bootstrap func() (string, error),
	sleep func(time.Duration),
) error {
	bootout()
	for range launchdUnloadWaitAttempts {
		if !isLoaded() {
			break
		}
		sleep(launchdUnloadPollInterval)
	}
	var lastStderr string
	var lastErr error
	for range launchdBootstrapAttempts {
		stderrStr, err := bootstrap()
		if err == nil {
			return nil
		}
		lastStderr = stderrStr
		lastErr = err
		sleep(launchdBootstrapInterval)
	}
	return fmt.Errorf("launchctl bootstrap %s failed: %w (%s)", plistPath, lastErr, strings.TrimSpace(lastStderr))
}

func reloadDaemon(label, plistPath string) error {
	return reloadLaunchdService(
		plistPath,
		func() { _, _, _, _ = Sudo(false, "launchctl", "bootout", "system/"+label) },
		func() bool { return LaunchdLoaded(label) },
		func() (string, error) {
			_, stderrStr, rc, err := Sudo(false, "launchctl", "bootstrap", "system", plistPath)
			if err == nil && rc == 0 {
				return "", nil
			}
			if err == nil {
				err = fmt.Errorf("exit status %d", rc)
			}
			return stderrStr, err
		},
		time.Sleep,
	)
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
		if RestartService(opts.Label) == nil {
			return nil
		}
	}
	return reloadDaemon(opts.Label, plistPath)
}

// ReloadDaemonDetached reloads a LaunchDaemon in a detached session after a 1s delay
// so a running sync daemon does not SIGTERM itself mid-execution.
func ReloadDaemonDetached(label, plistPath string) error {
	script := `sleep 1
launchctl bootout "system/$1" >/dev/null 2>&1 || true
i=0
while [ "$i" -lt 140 ] && launchctl print "system/$1" >/dev/null 2>&1; do
  sleep 0.15
  i=$((i + 1))
done
i=0
while [ "$i" -lt 20 ]; do
  launchctl bootstrap system "$2" >/dev/null 2>&1 && exit 0
  sleep 0.5
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
		return RestartService(label)
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
	_, err := runCmdTimeout(nil, "launchctl", "print", launchAgentDomain()+"/"+label)
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
	return reloadLaunchdService(
		plistPath,
		func() { _, _ = runCmdTimeout(nil, "launchctl", "bootout", domain+"/"+label) },
		func() bool { return UserAgentLoaded(label) },
		func() (string, error) {
			out, err := runCmdTimeout(nil, "launchctl", "bootstrap", domain, plistPath)
			return string(out), err
		},
		time.Sleep,
	)
}

// StartUserAgent kickstarts or bootstraps a per-user LaunchAgent.
func StartUserAgent(label string) error {
	domain := launchAgentDomain()
	if UserAgentLoaded(label) {
		if out, err := runCmdTimeout(nil, "launchctl", "kickstart", "-k", domain+"/"+label); err != nil {
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
	out, err := runCmdTimeout(nil, "launchctl", "bootout", domain+"/"+label)
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

func userAgentNeedsRestart(owner, label, binPath string) (string, bool) {
	if owner == "" || owner == "root" || label == "" || binPath == "" {
		return "", false
	}
	uid, _, err := paths.LookupUIDGID(owner)
	if err != nil {
		return "", false
	}
	fi, err := os.Stat(binPath)
	if err != nil {
		return "", false
	}
	target := fmt.Sprintf("gui/%d/%s", uid, label)
	out, err := runCmdTimeout(nil, "launchctl", "print", target)
	if err != nil {
		return "", false
	}
	pid := parseLaunchctlPID(string(out))
	if pid <= 0 {
		return "", false
	}
	psOut, err := runCmdTimeout(append(os.Environ(), "LC_ALL=C"), "ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	if err != nil {
		return "", false
	}
	procStart, err := parseProcessStartTime(string(psOut))
	if err != nil || !procStart.Before(fi.ModTime()) {
		return "", false
	}
	return target, true
}

// RestartStaleUserAgent kickstarts gui/<uid>/<label> if it is running a process started before binPath's modtime.
func RestartStaleUserAgent(owner, label, binPath string) (bool, error) {
	target, stale := userAgentNeedsRestart(owner, label, binPath)
	if !stale {
		return false, nil
	}
	if kOut, kErr := runCmdTimeout(nil, "launchctl", "kickstart", "-k", target); kErr != nil {
		return false, fmt.Errorf("launchctl kickstart %s failed: %w (%s)", target, kErr, strings.TrimSpace(string(kOut)))
	}
	return true, nil
}
