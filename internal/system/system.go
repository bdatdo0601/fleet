// Package system handles macOS side effects: root-owned files, launchd daemons, nebula, and atomic binary self-updates.
package system

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bdatdo0601/fleet/internal/paths"
)

const (
	LaunchDaemonsDir  = "/Library/LaunchDaemons"
	ALF               = "/usr/libexec/ApplicationFirewall/socketfilterfw"
	StandardPathEnv   = paths.StandardPathEnv
	defaultCmdTimeout = 30 * time.Second
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

// IsRoot reports whether the current effective UID is 0.
func IsRoot() bool {
	return os.Geteuid() == 0
}

func randomSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func runCmdTimeout(env []string, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = env
	}
	return cmd.CombinedOutput()
}

// Sudo runs args directly when root, or via `sudo` otherwise.
// When check is true, stdout/stderr are attached interactively; when false, output is captured with a bounded timeout.
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
	if check {
		cmd := exec.Command(name, cmdArgs...)
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
	ctx, cancel := context.WithTimeout(context.Background(), defaultCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, cmdArgs...)
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
	ctx, cancel := context.WithTimeout(context.Background(), defaultCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, ALF, "--getglobalstate").Output()
	if err != nil || !strings.Contains(string(out), "enabled") {
		return
	}
	for _, bin := range binaries {
		if bin == "" {
			continue
		}
		realPath := bin
		if resolved, err := filepath.EvalSymlinks(bin); err == nil {
			realPath = resolved
		} else if abs, err := filepath.Abs(bin); err == nil {
			realPath = abs
		}
		_, _, _, _ = Sudo(false, ALF, "--remove", realPath)
		_, _, _, _ = Sudo(false, ALF, "--add", realPath)
		_, _, _, _ = Sudo(false, ALF, "--unblockapp", realPath)
	}
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
