package session

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type rexSession struct {
	Label     string `json:"label"`
	SessionID string `json:"session_id"`
}

func rexInstalled(pathEnv string) bool {
	_, err := lookPathWithEnv("rex", pathEnv)
	return err == nil
}

func rexSessionsFor(spec Spec) []rexSession {
	out, err := runCmdCapture("rex", "ls", "--json")
	if err != nil {
		return nil
	}
	var resp struct {
		Sessions []rexSession `json:"sessions"`
	}
	if json.Unmarshal([]byte(out), &resp) != nil {
		return nil
	}
	var matched []rexSession
	for _, s := range resp.Sessions {
		if strings.HasPrefix(s.Label, spec.Prefix) && s.SessionID != "" {
			matched = append(matched, s)
		}
	}
	return matched
}

func rexSessionID(spec Spec, label string) string {
	for _, s := range rexSessionsFor(spec) {
		if s.Label == label {
			return s.SessionID
		}
	}
	return ""
}

func rexWrapperPID(spec Spec, name string) (int, string, bool) {
	pidPath := filepath.Join(rexStateDir(spec.Name), name+".pid")
	raw, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, "", false
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0, "", false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return 0, "", false
	}
	block := ""
	if len(fields) > 1 {
		block = fields[1]
	}
	cmdLine, err := runCmdCapture("ps", "-o", "command=", "-p", strconv.Itoa(pid))
	if err != nil {
		return 0, "", false
	}
	needle := "__rex-run " + name
	if !strings.Contains(cmdLine, needle+" ") && !strings.HasSuffix(strings.TrimSpace(cmdLine), needle) {
		return 0, "", false
	}
	return pid, block, true
}

func rexChildPID(spec Spec, name string) (int, bool) {
	wrapperPID, _, ok := rexWrapperPID(spec, name)
	if !ok {
		return 0, false
	}
	out, err := runCmdCapture("pgrep", "-P", strconv.Itoa(wrapperPID))
	if err != nil {
		return 0, false
	}
	for ln := range strings.SplitSeq(out, "\n") {
		pidStr := strings.TrimSpace(ln)
		if pidStr == "" {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil || pid <= 0 {
			continue
		}
		cmdLine, err := runCmdCapture("ps", "-o", "command=", "-p", pidStr)
		if err != nil {
			continue
		}
		if spec.MatchesProcess(cmdLine) {
			return pid, true
		}
	}
	return 0, false
}

func rexShow(spec Spec, sessionID string, stderr io.Writer) bool {
	out, err := runCmdCapture("rex", "client", "ls", "--json")
	if err != nil {
		return false
	}
	var resp struct {
		Clients []struct {
			ClientID string `json:"client_id"`
			Info     struct {
				Kind string `json:"kind"`
			} `json:"info"`
		} `json:"clients"`
	}
	if json.Unmarshal([]byte(out), &resp) != nil {
		return false
	}
	shown := false
	for _, cl := range resp.Clients {
		if cl.Info.Kind != "app" || cl.ClientID == "" {
			continue
		}
		cmd := exec.Command("rex", "-C", cl.ClientID, "do", "session.select", "session_id="+sessionID)
		resOut, rErr := cmd.CombinedOutput()
		text := string(resOut)
		if rErr == nil && !strings.Contains(text, "Error:") {
			shown = true
		} else if strings.Contains(text, "not accepting actions") {
			_, _ = fmt.Fprintf(stderr, "fleet session %s: to let it switch sessions, turn on Remote Control in Rex Settings, under Rex Server.\n", spec.Name)
		}
	}
	return shown
}

func readPromptFileIfSet(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	_ = os.Remove(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func prepareRexEnvAndHook(spec Spec, self string, stderr io.Writer) (string, error) {
	pathEnv := AugmentedPath(os.Getenv("PATH"), os.Getenv("HOME"))
	_ = os.Setenv("PATH", pathEnv)
	bin, err := lookPathWithEnv(spec.Binary, pathEnv)
	if err != nil {
		return "", fmt.Errorf("%s is required. Install %s and sign in first", spec.Binary, spec.DisplayName)
	}
	switch spec.Name {
	case HarnessAgy:
		if err := EnsureAgyHook(self, ""); err != nil {
			return "", err
		}
	case HarnessCodex:
		ensureCodexRemoteControl(bin, stderr)
	}
	return bin, nil
}

func runRexChild(bin string, args []string, name string, pol Policy, stateFile string, sigCh <-chan os.Signal, stdout, stderr io.Writer) (bool, error) {
	cmd := exec.Command(bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = append(os.Environ(),
		"FLEET_SESSION_TARGET=rex",
		"FLEET_SESSION_NAME="+name,
		"FLEET_SESSION_POLICY="+string(pol),
		"FLEET_SESSION_STATE_FILE="+stateFile,
		"AGY_SESSION_TARGET=rex",
		"AGY_SESSION_NAME="+name,
		"AGY_SESSION_STATE_FILE="+stateFile,
	)
	if err := cmd.Start(); err != nil {
		return false, err
	}
	doneCh := make(chan error, 1)
	go func() { doneCh <- cmd.Wait() }()

	select {
	case sig := <-sigCh:
		if cmd.Process != nil {
			_ = cmd.Process.Signal(sig)
		}
		waitErr := <-doneCh
		_ = waitErr
		return true, fmt.Errorf("interrupted by %s", sig)
	case runErr := <-doneCh:
		return false, runErr
	}
}

// RunRexWrapper executes the internal `__rex-run` supervision loop inside a Rex block.
func RunRexWrapper(harness, name, resumeID string, policy Policy, promptFile, selfBin string, stdout, stderr io.Writer) error {
	spec, err := GetSpec(harness)
	if err != nil {
		return err
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	rDir := rexStateDir(spec.Name)
	if err := os.MkdirAll(rDir, 0o755); err != nil {
		return err
	}
	pidFile := filepath.Join(rDir, name+".pid")
	stateFile := filepath.Join(rDir, name+".json")
	marker := filepath.Join(rDir, name+".restart")
	if err := os.WriteFile(pidFile, fmt.Appendf(nil, "%d %s\n", os.Getpid(), os.Getenv("REX_BLOCK")), 0o644); err != nil {
		return err
	}
	defer func() {
		for _, p := range []string{pidFile, stateFile, stateFile + ".tmp", marker} {
			_ = os.Remove(p)
		}
	}()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	pol := cmp.Or(policy, PolicyBypass)
	prompt, err := readPromptFileIfSet(promptFile)
	if err != nil {
		return err
	}
	curResume := resumeID
	if curResume != "" && spec.Name == HarnessAgy {
		cwd, _ := os.Getwd()
		_ = writeTrackedStateFile(stateFile, trackedState{
			ID:        curResume,
			Directory: cwd,
			Policy:    string(pol),
			Started:   float64(time.Now().UnixNano()) / 1e9,
		})
	}

	self := resolveSelfBin(selfBin)
	lockDir := upgradeLockPath(spec.Name)

	for {
		bin, err := prepareRexEnvAndHook(spec, self, stderr)
		if err != nil {
			return err
		}
		args, err := BuildHarnessArgs(spec, name, self, curResume, prompt, pol)
		if err != nil {
			return err
		}
		prompt = ""

		interrupted, runErr := runRexChild(bin, args, name, pol, stateFile, sigCh, stdout, stderr)
		if interrupted {
			return runErr
		}
		if _, mErr := os.Stat(marker); mErr != nil {
			return runErr
		}
		_ = os.Remove(marker)
		waitForLockRelease(lockDir, stdout, spec.Name)

		curResume = ""
		if st, ok := readTrackedStateFile(stateFile); ok && st.ID != "" {
			curResume = st.ID
			if st.Policy != "" {
				if parsed, pErr := ParsePolicy(st.Policy, pol); pErr == nil {
					pol = parsed
				}
			}
			if st.Directory != "" {
				_ = os.Chdir(st.Directory)
			}
		} else {
			_, _ = fmt.Fprintf(stderr, "fleet session %s: no saved conversation to resume; starting a new one.\n", spec.Name)
		}
	}
}

func waitForLockRelease(lockDir string, stdout io.Writer, harness string) {
	if _, err := os.Stat(lockDir); err != nil {
		return
	}
	_, _ = fmt.Fprintf(stdout, "Waiting for fleet session %s upgrade to finish...\n", harness)
	for {
		if _, err := os.Stat(lockDir); err != nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}
