package session

import (
	"cmp"
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
	UseRex    bool
	UseTmux   bool
	Attach    bool
	ResumeID  string
	Model     string
	Policy    Policy
	Prompt    string
	SelfBin   string
	Stdout    io.Writer
	Stderr    io.Writer
}

func ensureCodexRemoteControl(codexBin string, stderr io.Writer) {
	if os.Getenv("FLEET_CODEX_SKIP_REMOTE_CONTROL") == "1" {
		return
	}
	stderr = cmp.Or(stderr, io.Discard)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, codexBin, "remote-control", "start").CombinedOutput()
	if err == nil {
		return
	}
	detail := strings.TrimSpace(string(out))
	if detail == "" {
		detail = err.Error()
	}
	_, _ = fmt.Fprintf(stderr, "fleet session codex: remote-control start failed: %s\n", detail)
	lower := strings.ToLower(detail)
	if strings.Contains(lower, "errored") || strings.Contains(lower, "409") || strings.Contains(lower, "already online") {
		_, _ = fmt.Fprintln(stderr, "Disable Remote Control in Codex Desktop (Settings -> Connections / Remote Control) so the shared CLI daemon can connect.")
	}
}

func isGitRepo(dir string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--git-dir").Run() == nil
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

func validateStartOpts(opts StartOpts) (Spec, string, error) {
	spec, err := GetSpec(opts.Harness)
	if err != nil {
		return Spec{}, "", err
	}
	if err := ValidateName(opts.Name); err != nil {
		return Spec{}, "", err
	}
	if opts.UseRex && opts.UseTmux {
		return Spec{}, "", errors.New("--rex and --tmux cannot be combined")
	}
	directory, err := resolveDirectory(opts.Directory)
	if err != nil {
		return Spec{}, "", err
	}
	if opts.ResumeID != "" {
		if err := ValidateUUID(opts.ResumeID); err != nil {
			return Spec{}, "", err
		}
	}
	if exists, _ := SessionExists(spec.Name, opts.Name); exists {
		return Spec{}, "", fmt.Errorf("session '%s' already exists. Attach with: fleet session %s attach %s", opts.Name, spec.Name, opts.Name)
	}
	return spec, directory, nil
}

func prepareStartBackend(spec Spec, opts StartOpts, pathEnv string) (harnessBin string, useRex bool, err error) {
	if isUpgradeLockActive(upgradeLockPath(spec.Name)) {
		return "", false, errors.New("an upgrade is running; wait for it to finish before starting sessions")
	}
	harnessBin, err = lookPathWithEnv(spec.Binary, pathEnv)
	if err != nil {
		return "", false, fmt.Errorf("%s is required. Install %s and sign in first", spec.Binary, spec.DisplayName)
	}
	hasRex := rexInstalled(pathEnv)
	if opts.UseRex && !hasRex {
		return "", false, errors.New("rex is required when --rex is specified")
	}
	useRex = opts.UseRex || (!opts.UseTmux && spec.Name != HarnessAgy && hasRex)
	if !useRex {
		if _, err := lookPathWithEnv("tmux", pathEnv); err != nil {
			return "", false, ErrTmuxRequired
		}
	}
	return harnessBin, useRex, nil
}

func effectiveStartPolicy(opts StartOpts) Policy {
	if opts.Policy != "" {
		return opts.Policy
	}
	if opts.Prompt != "" {
		return PolicySafe
	}
	return PolicyAuto
}

// Start launches a new detached or attached session in the Antigravity app (agy default), Rex, or tmux.
func Start(opts StartOpts) (string, error) {
	opts.Stdout = cmp.Or(opts.Stdout, io.Discard)
	opts.Stderr = cmp.Or(opts.Stderr, io.Discard)
	spec, directory, err := validateStartOpts(opts)
	if err != nil {
		return "", err
	}
	if spec.Name == HarnessAgy && !opts.UseRex && !opts.UseTmux {
		if err := startAgyAppSession(opts, directory); err != nil {
			return "", err
		}
		return "app", nil
	}

	pathEnv := AugmentedPath(os.Getenv("PATH"), os.Getenv("HOME"))
	harnessBin, useRex, err := prepareStartBackend(spec, opts, pathEnv)
	if err != nil {
		return "", err
	}
	self := resolveSelfBin(opts.SelfBin)
	switch spec.Name {
	case HarnessAgy:
		if err := EnsureAgyHook(self, ""); err != nil {
			return "", err
		}
	case HarnessCodex:
		ensureCodexRemoteControl(harnessBin, opts.Stderr)
	}

	pol := effectiveStartPolicy(opts)
	if os.Getenv("FLEET_SKIP_AUTO_TRUST") != "1" {
		if tErr := EnsureDirectoryTrusted(spec.Name, directory); tErr != nil {
			_, _ = fmt.Fprintf(opts.Stderr, "fleet session %s: warning: could not pre-trust %s: %v\n", spec.Name, directory, tErr)
		}
	}

	if useRex {
		if err := startRexSession(spec, opts, directory, self, pathEnv, pol); err != nil {
			return "", err
		}
		return "rex", nil
	}
	session := spec.Prefix + opts.Name
	target := "=" + session
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
	}
	if shellEnv := os.Getenv("SHELL"); shellEnv != "" {
		rexArgs = append(rexArgs, "SHELL="+shellEnv)
	}
	rexArgs = append(rexArgs,
		self, "session", spec.Name, "__rex-run", opts.Name, "--policy", string(pol),
	)
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

	tmuxArgs := []string{"new-session", "-d", "-s", session, "-c", directory}
	tmuxArgs = append(tmuxArgs, tmuxSessionEnvArgs(spec, opts.Name, pathEnv, pol)...)
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

// Attach connects to an existing tmux session, selects a Rex session, or opens the Antigravity app for an app session.
func Attach(harness, name string, stderr io.Writer) error {
	stderr = cmp.Or(stderr, io.Discard)
	spec, err := GetSpec(harness)
	if err != nil {
		return err
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	pathEnv := AugmentedPath(os.Getenv("PATH"), os.Getenv("HOME"))
	hasTmux, err := checkTmuxForHarness(spec, pathEnv)
	if err != nil {
		return err
	}
	session := spec.Prefix + name
	target := "=" + session
	if hasTmux && tmuxHasSession(target) {
		return tmuxAttachExec(spec, target, pathEnv)
	}
	if rexID := rexSessionID(spec, session); rexID != "" {
		if !rexShow(spec, rexID, stderr) {
			return fmt.Errorf("could not show '%s' in Rex; open it from the Rex session picker (Shift-Cmd-K)", name)
		}
		return nil
	}
	if spec.Name == HarnessAgy {
		if st, ok := agyAppSessionActive(name); ok {
			if hub, hErr := discoverAgyHub(); hErr == nil {
				if conv, gErr := agyGetConversation(hub, st.ConversationID); gErr == nil && conv.Annotations.Archived {
					_ = agyUpdateConversationArchived(hub, st.ConversationID, false)
				}
			}
			return openAgyAppFn()
		}
	}
	return fmt.Errorf("no session named '%s'. Run fleet session %s list", name, spec.Name)
}

// List prints all active tmux, Rex, and (for agy) Antigravity app sessions for harness.
func List(harness string, stdout io.Writer) error {
	stdout = cmp.Or(stdout, io.Discard)
	spec, err := GetSpec(harness)
	if err != nil {
		return err
	}
	pathEnv := AugmentedPath(os.Getenv("PATH"), os.Getenv("HOME"))
	hasTmux, err := checkTmuxForHarness(spec, pathEnv)
	if err != nil {
		return err
	}
	found := false
	if hasTmux {
		for _, s := range tmuxListSessions() {
			if !strings.HasPrefix(s, spec.Prefix) {
				continue
			}
			if tmuxRunsHarness(spec, s) {
				_, _ = fmt.Fprintf(stdout, "%s\n", strings.TrimPrefix(s, spec.Prefix))
				found = true
			}
		}
	}
	for _, rs := range rexSessionsFor(spec) {
		rexName := strings.TrimPrefix(rs.Label, spec.Prefix)
		if _, ok := rexChildPID(spec, rexName); ok {
			_, _ = fmt.Fprintf(stdout, "%s (rex)\n", rexName)
			found = true
		}
	}
	if spec.Name == HarnessAgy {
		for _, st := range listAppSessionStates() {
			if _, ok := agyAppSessionActive(st.Name); ok {
				_, _ = fmt.Fprintf(stdout, "%s (app)\n", st.Name)
				found = true
			}
		}
	}
	if !found {
		_, _ = fmt.Fprintf(stdout, "No %s sessions. Start one: fleet session %s start NAME DIRECTORY\n", spec.DisplayName, spec.Name)
	}
	return nil
}

// Kill terminates one tmux or Rex session, or archives an Antigravity app session by exact name.
func Kill(harness, name string, stdout io.Writer) error {
	stdout = cmp.Or(stdout, io.Discard)
	spec, err := GetSpec(harness)
	if err != nil {
		return err
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	pathEnv := AugmentedPath(os.Getenv("PATH"), os.Getenv("HOME"))
	hasTmux, err := checkTmuxForHarness(spec, pathEnv)
	if err != nil {
		return err
	}
	session := spec.Prefix + name
	target := "=" + session
	if hasTmux && tmuxHasSession(target) {
		if _, err := tmuxCmd("kill-session", "-t", target); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "Killed %s\n", name)
		return nil
	}
	if rexID := rexSessionID(spec, session); rexID != "" {
		if _, err := runCmdCapture("rex", "kill", rexID); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "Killed %s\n", name)
		return nil
	}
	if spec.Name == HarnessAgy {
		if st, ok := agyAppSessionActive(name); ok {
			hub, hErr := discoverAgyHub()
			if hErr == nil {
				if uErr := agyUpdateConversationArchived(hub, st.ConversationID, true); uErr != nil && !isConnectNotFound(uErr) {
					return uErr
				}
				removeAppSessionState(name)
				_, _ = fmt.Fprintf(stdout, "Killed %s (app) (archived in Antigravity)\n", name)
				return nil
			}
			removeAppSessionState(name)
			_, _ = fmt.Fprintf(stdout, "Removed local session %s (app); Antigravity app is not running so it was not archived remotely\n", name)
			return nil
		}
	}
	return fmt.Errorf("no session named '%s'. Run fleet session %s list", name, spec.Name)
}

func killAllTmuxSessions(spec Spec, stdout io.Writer) (found bool, deferredSession string) {
	currentSession := ""
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		currentSession, _ = tmuxDisplay(pane, "#{session_name}")
	}
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
	return found, deferredSession
}

func killAllRexSessions(spec Spec, stdout, stderr io.Writer) (found bool, deferredName, deferredID string) {
	curRex := os.Getenv("REX_SESSION")
	for _, rs := range rexSessionsFor(spec) {
		rexName := strings.TrimPrefix(rs.Label, spec.Prefix)
		found = true
		if curRex != "" && rs.SessionID == curRex {
			deferredName = rexName
			deferredID = rs.SessionID
			continue
		}
		if _, err := runCmdCapture("rex", "kill", rs.SessionID); err == nil {
			_, _ = fmt.Fprintf(stdout, "Killed %s (rex)\n", rexName)
		} else {
			_, _ = fmt.Fprintf(stderr, "fleet session %s: could not kill Rex session %s\n", spec.Name, rexName)
		}
	}
	return found, deferredName, deferredID
}

func killAllAgyAppSessions(spec Spec, stdout, stderr io.Writer) bool {
	appStates := listAppSessionStates()
	if len(appStates) == 0 {
		return false
	}
	hub, hubErr := discoverAgyHub()
	for _, st := range appStates {
		if hubErr == nil {
			if uErr := agyUpdateConversationArchived(hub, st.ConversationID, true); uErr != nil && !isConnectNotFound(uErr) {
				_, _ = fmt.Fprintf(stderr, "fleet session %s: could not archive Antigravity session %s: %v\n", spec.Name, st.Name, uErr)
			}
		} else {
			_, _ = fmt.Fprintf(stderr, "fleet session %s: Antigravity app is not running; removed local session %s without archiving remotely\n", spec.Name, st.Name)
		}
		removeAppSessionState(st.Name)
		_, _ = fmt.Fprintf(stdout, "Killed %s (app)\n", st.Name)
	}
	return true
}

// KillAll terminates all tmux and Rex sessions (and archives Antigravity app sessions) belonging to harness, deferring the caller's own session to last.
func KillAll(harness string, stdout, stderr io.Writer) error {
	stdout = cmp.Or(stdout, io.Discard)
	stderr = cmp.Or(stderr, io.Discard)
	spec, err := GetSpec(harness)
	if err != nil {
		return err
	}
	pathEnv := AugmentedPath(os.Getenv("PATH"), os.Getenv("HOME"))
	hasTmux, err := checkTmuxForHarness(spec, pathEnv)
	if err != nil {
		return err
	}
	found := false
	deferredSession := ""
	if hasTmux {
		found, deferredSession = killAllTmuxSessions(spec, stdout)
	}
	rexFound, deferredRexName, deferredRexID := killAllRexSessions(spec, stdout, stderr)
	found = found || rexFound
	if spec.Name == HarnessAgy && killAllAgyAppSessions(spec, stdout, stderr) {
		found = true
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
