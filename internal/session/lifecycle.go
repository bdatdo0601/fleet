package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// StartOpts configures Start.
type StartOpts struct {
	Harness   string
	Name      string
	Directory string
	UseTmux   bool
	Attach    bool
	ResumeID  string
	Policy    Policy
	Prompt    string
	SelfBin   string
	Stdout    io.Writer
	Stderr    io.Writer
}

func ensureCodexRemoteControl(codexBin string) {
	if os.Getenv("FLEET_CODEX_SKIP_REMOTE_CONTROL") == "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, codexBin, "remote-control", "start").Run()
}

func isGitRepo(dir string) bool {
	return exec.Command("git", "-C", dir, "rev-parse", "--git-dir").Run() == nil
}

func writePromptTempFile(harness, name, prompt string) (string, error) {
	dir := filepath.Join(stateRootFor(harness), "prompts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, name+"-*.txt")
	if err != nil {
		return "", err
	}
	p := f.Name()
	if _, err := f.WriteString(prompt); err != nil {
		_ = f.Close()
		_ = os.Remove(p)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(p)
		return "", err
	}
	_ = os.Chmod(p, 0o600)
	return p, nil
}

func resolveDirectory(raw string) (string, error) {
	dir := strings.TrimSpace(raw)
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if dir == "~" {
				dir = home
			} else {
				dir = filepath.Join(home, dir[2:])
			}
		}
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return "", fmt.Errorf("directory does not exist: %s", raw)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

// Start launches a new detached or attached session in Rex (when installed) or tmux.
func Start(opts StartOpts) (string, error) {
	spec, err := GetSpec(opts.Harness)
	if err != nil {
		return "", err
	}
	if err := ValidateName(opts.Name); err != nil {
		return "", err
	}
	pathEnv := AugmentedPath(os.Getenv("PATH"), os.Getenv("HOME"))
	_ = os.Setenv("PATH", pathEnv)
	if _, err := lookPathWithEnv("tmux", pathEnv); err != nil {
		return "", errors.New("tmux is required (macOS: brew install tmux)")
	}
	if _, err := os.Stat(upgradeLockPath(spec.Name)); err == nil {
		return "", errors.New("an upgrade is running; wait for it to finish before starting sessions")
	}
	directory, err := resolveDirectory(opts.Directory)
	if err != nil {
		return "", err
	}
	harnessBin, err := lookPathWithEnv(spec.Binary, pathEnv)
	if err != nil {
		return "", fmt.Errorf("%s is required. Install %s and sign in first", spec.Binary, spec.DisplayName)
	}
	if opts.ResumeID != "" {
		if err := ValidateUUID(opts.ResumeID); err != nil {
			return "", err
		}
	}
	session := spec.Prefix + opts.Name
	target := "=" + session
	if exists, _ := SessionExists(spec.Name, opts.Name); exists {
		return "", fmt.Errorf("session '%s' already exists. Attach with: fleet session %s attach %s", opts.Name, spec.Name, opts.Name)
	}

	self := resolveSelfBin(opts.SelfBin)
	switch spec.Name {
	case HarnessAgy:
		if err := EnsureAgyHook(self, ""); err != nil {
			return "", err
		}
	case HarnessCodex:
		ensureCodexRemoteControl(harnessBin)
	}

	pol := opts.Policy
	if pol == "" {
		if opts.Prompt != "" {
			pol = PolicySafe
		} else {
			pol = PolicyBypass
		}
	}

	if !opts.UseTmux && rexInstalled(pathEnv) {
		if err := startRexSession(spec, opts, directory, self, pathEnv, pol); err != nil {
			return "", err
		}
		return "rex", nil
	}
	if err := startTmuxSession(spec, opts, directory, harnessBin, self, pathEnv, session, target, pol); err != nil {
		return "", err
	}
	return "tmux", nil
}

func startRexSession(spec Spec, opts StartOpts, directory, self, pathEnv string, pol Policy) error {
	session := spec.Prefix + opts.Name
	var promptFile string
	if opts.Prompt != "" {
		var err error
		promptFile, err = writePromptTempFile(spec.Name, opts.Name, opts.Prompt)
		if err != nil {
			return err
		}
	}

	rexArgs := []string{
		"new", session, "-c", directory, "--window", opts.Name, "--json", "--",
		"env", "PATH=" + pathEnv, "FLEET_SESSION_POLICY=" + string(pol),
		self, "session", spec.Name, "__rex-run", opts.Name, "--policy", string(pol),
	}
	if promptFile != "" {
		rexArgs = append(rexArgs, "--prompt-file", promptFile)
	}
	if opts.ResumeID != "" {
		rexArgs = append(rexArgs, opts.ResumeID)
	}

	out, err := runCmdCapture("rex", rexArgs...)
	if err != nil {
		if promptFile != "" {
			_ = os.Remove(promptFile)
		}
		return fmt.Errorf("rex new failed; is the Rex app running? (%w)", err)
	}
	var created struct {
		SessionID      string `json:"session_id"`
		InitialWindows []struct {
			BlockIDs []string `json:"block_ids"`
		} `json:"initial_windows"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil || created.SessionID == "" {
		return fmt.Errorf("unexpected rex new --json output: %s", strings.TrimSpace(out))
	}
	var blockID string
	if len(created.InitialWindows) > 0 && len(created.InitialWindows[0].BlockIDs) > 0 {
		blockID = created.InitialWindows[0].BlockIDs[0]
	}
	if blockID != "" {
		if _, err := lookPathWithEnv("lazygit", pathEnv); err == nil && isGitRepo(directory) {
			if _, sErr := runCmdCapture("rex", "split", "-s", created.SessionID, "-b", blockID,
				"--split=right", "--ratio", "35", "--focus=false", "--label", "files", "-c", directory, "--", "lazygit"); sErr != nil {
				_, _ = fmt.Fprintf(opts.Stderr, "fleet session %s: could not open the lazygit pane; %s is running.\n", spec.Name, spec.DisplayName)
			}
		}
	}
	_, _ = fmt.Fprintf(opts.Stdout, "Started Rex session %s in %s\n", opts.Name, directory)
	if opts.Attach && rexShow(spec, created.SessionID, opts.Stderr) {
		return nil
	}
	_, _ = fmt.Fprintf(opts.Stdout, "Show it: fleet session %s attach %s (or the Rex session picker, Shift-Cmd-K)\n", spec.Name, opts.Name)
	_, _ = fmt.Fprintf(opts.Stdout, "Open it once to complete any %s login, trust or Remote Control prompts.\n", spec.DisplayName)
	return nil
}

func startTmuxSession(spec Spec, opts StartOpts, directory, harnessBin, self, pathEnv, session, target string, pol Policy) error {
	var promptFile string
	if opts.Prompt != "" {
		var err error
		promptFile, err = writePromptTempFile(spec.Name, opts.Name, opts.Prompt)
		if err != nil {
			return err
		}
	}
	// When using __exec-prompt, BuildHarnessArgs gets empty prompt because __exec-prompt appends the prompt from promptFile.
	baseArgs, err := BuildHarnessArgs(spec, opts.Name, self, opts.ResumeID, "", pol)
	if err != nil {
		if promptFile != "" {
			_ = os.Remove(promptFile)
		}
		return err
	}

	tmuxArgs := []string{
		"new-session", "-d", "-s", session, "-c", directory,
		"-e", "PATH=" + pathEnv,
		"-e", "FLEET_SESSION_TARGET=tmux",
		"-e", "FLEET_SESSION_NAME=" + opts.Name,
		"-e", "FLEET_SESSION_POLICY=" + string(pol),
	}
	if spec.Name == HarnessAgy {
		tmuxArgs = append(tmuxArgs, "-e", "AGY_SESSION_TARGET=tmux", "-e", "AGY_SESSION_NAME="+opts.Name)
	}
	if promptFile != "" {
		tmuxArgs = append(tmuxArgs, self, "session", spec.Name, "__exec-prompt", harnessBin, promptFile, "--")
		tmuxArgs = append(tmuxArgs, baseArgs...)
	} else {
		tmuxArgs = append(tmuxArgs, harnessBin)
		tmuxArgs = append(tmuxArgs, baseArgs...)
	}

	if _, err := tmuxCmd(tmuxArgs...); err != nil {
		if promptFile != "" {
			_ = os.Remove(promptFile)
		}
		return err
	}
	_ = tmuxSetPaneOption(target+":", spec.ManagedOption, "1")
	if opts.ResumeID != "" && spec.Name == HarnessAgy && tmuxShowPaneOption(target+":", spec.PaneOption) == "" {
		if panePID, pErr := tmuxDisplay(target+":", "#{pane_pid}"); pErr == nil && panePID != "" {
			if seedBytes, mErr := json.Marshal(trackedState{
				ID:        opts.ResumeID,
				Directory: directory,
				PID:       panePID,
				Policy:    string(pol),
			}); mErr == nil {
				_ = tmuxSetPaneOption(target+":", spec.PaneOption, string(seedBytes))
			}
		}
	}
	_, _ = fmt.Fprintf(opts.Stdout, "Started tmux session %s in %s\n", opts.Name, directory)
	if opts.Attach {
		return tmuxAttachExec(spec, target, pathEnv)
	}
	_, _ = fmt.Fprintf(opts.Stdout, "Attach: fleet session %s attach %s\n", spec.Name, opts.Name)
	_, _ = fmt.Fprintf(opts.Stdout, "Attach once to complete any %s login, trust or Remote Control prompts.\n", spec.DisplayName)
	return nil
}

// Attach connects to an existing tmux session or selects an existing Rex session in the Rex app.
func Attach(harness, name string, stderr io.Writer) error {
	spec, err := GetSpec(harness)
	if err != nil {
		return err
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	pathEnv := AugmentedPath(os.Getenv("PATH"), os.Getenv("HOME"))
	_ = os.Setenv("PATH", pathEnv)
	if _, err := lookPathWithEnv("tmux", pathEnv); err != nil {
		return errors.New("tmux is required (macOS: brew install tmux)")
	}
	session := spec.Prefix + name
	target := "=" + session
	if !tmuxHasSession(target) {
		if rexID := rexSessionID(spec, session); rexID != "" {
			if !rexShow(spec, rexID, stderr) {
				return fmt.Errorf("could not show '%s' in Rex; open it from the Rex session picker (Shift-Cmd-K)", name)
			}
			return nil
		}
		return fmt.Errorf("no session named '%s'. Run fleet session %s list", name, spec.Name)
	}
	return tmuxAttachExec(spec, target, pathEnv)
}

// List prints all active tmux and Rex sessions for harness.
func List(harness string, stdout io.Writer) error {
	spec, err := GetSpec(harness)
	if err != nil {
		return err
	}
	pathEnv := AugmentedPath(os.Getenv("PATH"), os.Getenv("HOME"))
	_ = os.Setenv("PATH", pathEnv)
	if _, err := lookPathWithEnv("tmux", pathEnv); err != nil {
		return errors.New("tmux is required (macOS: brew install tmux)")
	}
	found := false
	for _, s := range tmuxListSessions() {
		if !strings.HasPrefix(s, spec.Prefix) {
			continue
		}
		if tmuxRunsHarness(spec, s) {
			_, _ = fmt.Fprintf(stdout, "%s\n", strings.TrimPrefix(s, spec.Prefix))
			found = true
		}
	}
	for _, rs := range rexSessionsFor(spec) {
		rexName := strings.TrimPrefix(rs.Label, spec.Prefix)
		if _, ok := rexChildPID(spec, rexName); ok {
			_, _ = fmt.Fprintf(stdout, "%s (rex)\n", rexName)
			found = true
		}
	}
	if !found {
		_, _ = fmt.Fprintf(stdout, "No %s sessions. Start one: fleet session %s start NAME DIRECTORY\n", spec.DisplayName, spec.Name)
	}
	return nil
}

// Kill terminates one tmux or Rex session by exact name.
func Kill(harness, name string, stdout io.Writer) error {
	spec, err := GetSpec(harness)
	if err != nil {
		return err
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	pathEnv := AugmentedPath(os.Getenv("PATH"), os.Getenv("HOME"))
	_ = os.Setenv("PATH", pathEnv)
	if _, err := lookPathWithEnv("tmux", pathEnv); err != nil {
		return errors.New("tmux is required (macOS: brew install tmux)")
	}
	session := spec.Prefix + name
	target := "=" + session
	if !tmuxHasSession(target) {
		if rexID := rexSessionID(spec, session); rexID != "" {
			if _, err := runCmdCapture("rex", "kill", rexID); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(stdout, "Killed %s\n", name)
			return nil
		}
		return fmt.Errorf("no session named '%s'. Run fleet session %s list", name, spec.Name)
	}
	if _, err := tmuxCmd("kill-session", "-t", target); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "Killed %s\n", name)
	return nil
}

// KillAll terminates all tmux and Rex sessions belonging to harness, deferring the caller's own session to last.
func KillAll(harness string, stdout, stderr io.Writer) error {
	spec, err := GetSpec(harness)
	if err != nil {
		return err
	}
	pathEnv := AugmentedPath(os.Getenv("PATH"), os.Getenv("HOME"))
	_ = os.Setenv("PATH", pathEnv)
	if _, err := lookPathWithEnv("tmux", pathEnv); err != nil {
		return errors.New("tmux is required (macOS: brew install tmux)")
	}
	currentSession := ""
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		currentSession, _ = tmuxDisplay(pane, "#{session_name}")
	}
	found := false
	deferredSession := ""
	for _, s := range tmuxListSessions() {
		if !strings.HasPrefix(s, spec.Prefix) {
			continue
		}
		found = true
		if s == currentSession {
			deferredSession = s
			continue
		}
		if _, err := tmuxCmd("kill-session", "-t", "="+s); err == nil {
			_, _ = fmt.Fprintf(stdout, "Killed %s\n", strings.TrimPrefix(s, spec.Prefix))
		}
	}

	var deferredRexName, deferredRexID string
	curRex := os.Getenv("REX_SESSION")
	for _, rs := range rexSessionsFor(spec) {
		rexName := strings.TrimPrefix(rs.Label, spec.Prefix)
		found = true
		if curRex != "" && rs.SessionID == curRex {
			deferredRexName = rexName
			deferredRexID = rs.SessionID
			continue
		}
		if _, err := runCmdCapture("rex", "kill", rs.SessionID); err == nil {
			_, _ = fmt.Fprintf(stdout, "Killed %s (rex)\n", rexName)
		} else {
			_, _ = fmt.Fprintf(stderr, "fleet session %s: could not kill Rex session %s\n", spec.Name, rexName)
		}
	}

	if deferredSession != "" {
		_, _ = fmt.Fprintf(stdout, "Killing %s\n", strings.TrimPrefix(deferredSession, spec.Prefix))
		_, _ = tmuxCmd("kill-session", "-t", "="+deferredSession)
	}
	if deferredRexID != "" {
		_, _ = fmt.Fprintf(stdout, "Killing %s (rex)\n", deferredRexName)
		_, _ = runCmdCapture("rex", "kill", deferredRexID)
	}
	if !found {
		_, _ = fmt.Fprintf(stdout, "No %s sessions.\n", spec.DisplayName)
	}
	return nil
}
