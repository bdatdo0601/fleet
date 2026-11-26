package paths

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const (
	shellPathStartMarker = "__FLEET_SHELL_PATH_START__"
	shellPathEndMarker   = "__FLEET_SHELL_PATH_END__"
	shellProbeTimeout    = 5 * time.Second
	dsclTimeout          = 2 * time.Second
)

// MergePathEnv joins PATH lists in priority order, removing empty, malformed, and duplicate entries.
func MergePathEnv(pathLists ...string) string {
	seen := make(map[string]struct{})
	var out []string
	sep := string(os.PathListSeparator)
	for _, list := range pathLists {
		if list == "" {
			continue
		}
		for entry := range strings.SplitSeq(list, sep) {
			if entry == "" || strings.ContainsAny(entry, "\n\r\x00") {
				continue
			}
			key := strings.TrimRight(entry, "/")
			if key == "" {
				key = "/"
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, entry)
		}
	}
	return strings.Join(out, sep)
}

func isExecutableFile(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

func parseDSCLUserShell(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), "UserShell:"); ok {
			return strings.TrimSpace(after)
		}
	}
	return ""
}

// ResolveUserShell returns the user's configured login shell path.
func ResolveUserShell(ctx context.Context, username string) string {
	if username == "" {
		if sh := strings.TrimSpace(os.Getenv("SHELL")); isExecutableFile(sh) {
			return sh
		}
		if u, err := user.Current(); err == nil && u.Username != "" {
			username = u.Username
		} else {
			username = strings.TrimSpace(os.Getenv("USER"))
		}
	}
	if runtime.GOOS == "darwin" && username != "" {
		dsclCtx, cancel := context.WithTimeout(ctx, dsclTimeout)
		defer cancel()
		out, err := exec.CommandContext(dsclCtx, "dscl", ".", "-read", "/Users/"+username, "UserShell").Output()
		if err == nil {
			if sh := parseDSCLUserShell(string(out)); isExecutableFile(sh) {
				return sh
			}
		}
	}
	for _, cand := range []string{"/bin/zsh", "/bin/bash", "/bin/sh"} {
		if isExecutableFile(cand) {
			return cand
		}
	}
	return "/bin/sh"
}

func shellProbeArgs(shell string) (interactiveArgs, loginArgs []string) {
	switch filepath.Base(shell) {
	case "fish":
		cmd := fmt.Sprintf("printf '\\n%s%%s%s\\n' (string join : $PATH)", shellPathStartMarker, shellPathEndMarker)
		return []string{"-l", "-i", "-c", cmd}, []string{"-l", "-c", cmd}
	case "csh", "tcsh":
		cmd := fmt.Sprintf("printf '\\n%s%%s%s\\n' \"$PATH\"", shellPathStartMarker, shellPathEndMarker)
		return []string{"-i", "-c", cmd}, []string{"-c", cmd}
	default:
		cmd := fmt.Sprintf("printf '\\n%s%%s%s\\n' \"$PATH\"", shellPathStartMarker, shellPathEndMarker)
		return []string{"-l", "-i", "-c", cmd}, []string{"-l", "-c", cmd}
	}
}

func parseShellPathOutput(out string) (string, bool) {
	startIdx := strings.LastIndex(out, shellPathStartMarker)
	if startIdx < 0 {
		return "", false
	}
	rest := out[startIdx+len(shellPathStartMarker):]
	extracted, _, ok := strings.Cut(rest, shellPathEndMarker)
	if !ok {
		return "", false
	}
	if extracted == "" || strings.ContainsAny(extracted, "\n\r\x00") {
		return "", false
	}
	cleaned := MergePathEnv(extracted)
	return cleaned, cleaned != ""
}

func runShellProbe(ctx context.Context, shell string, args, env []string) (string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, shellProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, shell, args...)
	cmd.Stdin = nil
	cmd.Env = env
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 500 * time.Millisecond

	if err := cmd.Run(); err != nil {
		return "", err
	}
	parsed, ok := parseShellPathOutput(stdout.String())
	if !ok {
		return "", fmt.Errorf("shell %s did not emit PATH markers", shell)
	}
	return parsed, nil
}

// LoginShellPath resolves the PATH initialized by the user's login shell startup files.
func LoginShellPath(ctx context.Context, shell, home, username string) (string, error) {
	if home == "" {
		home, _ = os.UserHomeDir()
		if home == "" {
			home = os.Getenv("HOME")
		}
	}
	if username == "" {
		if u, err := user.Current(); err == nil && u.Username != "" {
			username = u.Username
		} else {
			username = os.Getenv("USER")
		}
	}
	if shell == "" {
		shell = ResolveUserShell(ctx, username)
	}

	env := []string{
		"HOME=" + home,
		"USER=" + username,
		"LOGNAME=" + username,
		"SHELL=" + shell,
		"TERM=xterm-256color",
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
	}
	if zdot := os.Getenv("ZDOTDIR"); zdot != "" {
		env = append(env, "ZDOTDIR="+zdot)
	}

	interactiveArgs, loginArgs := shellProbeArgs(shell)
	if p, err := runShellProbe(ctx, shell, interactiveArgs, env); err == nil {
		return p, nil
	}
	return runShellProbe(ctx, shell, loginArgs, env)
}

// RefreshProcessPathFromLoginShell updates the current process's PATH (and unset SHELL) from the user's login shell.
func RefreshProcessPathFromLoginShell(ctx context.Context) string {
	home, _ := os.UserHomeDir()
	if home == "" {
		home = os.Getenv("HOME")
	}
	shell := ResolveUserShell(ctx, "")
	if os.Getenv("SHELL") == "" && shell != "" {
		_ = os.Setenv("SHELL", shell)
	}
	shellPath, _ := LoginShellPath(ctx, shell, home, "")
	merged := MergePathEnv(shellPath, os.Getenv("PATH"), filepath.Join(home, ".local", "bin"), StandardPathEnv)
	if merged != "" {
		_ = os.Setenv("PATH", merged)
	}
	return merged
}
