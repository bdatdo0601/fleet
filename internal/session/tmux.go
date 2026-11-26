package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

type trackedState struct {
	ID         string  `json:"id"`
	Directory  string  `json:"directory"`
	Transcript string  `json:"transcript"`
	PID        string  `json:"pid,omitempty"`
	Policy     string  `json:"policy,omitempty"`
	Started    float64 `json:"started,omitempty"`
}

func runCmdCapture(name string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

func tmuxCmd(args ...string) (string, error) {
	return runCmdCapture("tmux", args...)
}

func tmuxHasSession(target string) bool {
	return exec.Command("tmux", "has-session", "-t", target).Run() == nil
}

func tmuxListSessions() []string {
	out, err := tmuxCmd("list-sessions", "-F", "#{session_name}")
	if err != nil {
		return nil
	}
	var res []string
	for ln := range strings.SplitSeq(out, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			res = append(res, t)
		}
	}
	return res
}

type tmuxPaneInfo struct {
	ID   string
	Dead bool
	PID  string
}

func tmuxListPanes(target string) ([]tmuxPaneInfo, error) {
	out, err := tmuxCmd("list-panes", "-s", "-t", target, "-F", "#{pane_id}\t#{pane_dead}\t#{pane_pid}")
	if err != nil {
		return nil, err
	}
	var panes []tmuxPaneInfo
	for ln := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		parts := strings.Split(ln, "\t")
		if len(parts) < 3 {
			continue
		}
		panes = append(panes, tmuxPaneInfo{
			ID:   parts[0],
			Dead: parts[1] == "1",
			PID:  parts[2],
		})
	}
	return panes, nil
}

func tmuxRunsHarness(spec Spec, session string) bool {
	panes, err := tmuxListPanes("=" + session)
	if err != nil {
		return false
	}
	for _, p := range panes {
		if p.Dead || p.PID == "" {
			continue
		}
		cmdLine, err := runCmdCapture("ps", "-o", "command=", "-p", p.PID)
		if err != nil {
			continue
		}
		if spec.MatchesProcess(cmdLine) {
			return true
		}
	}
	return false
}

func tmuxDisplay(target, format string) (string, error) {
	out, err := tmuxCmd("display-message", "-p", "-t", target, format)
	return strings.TrimSpace(out), err
}

func tmuxShowPaneOption(pane, opt string) string {
	out, err := tmuxCmd("show-options", "-pqv", "-t", pane, opt)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func tmuxSetPaneOption(pane, opt, val string) error {
	_, err := tmuxCmd("set-option", "-p", "-t", pane, opt, val)
	return err
}

func tmuxUnsetPaneOption(pane, opt string) error {
	_, err := tmuxCmd("set-option", "-pu", "-t", pane, opt)
	return err
}

func tmuxAttachExec(spec Spec, target, pathEnv string) error {
	tmuxBin, err := lookPathWithEnv("tmux", pathEnv)
	if err != nil {
		return fmt.Errorf("tmux not found: %w", err)
	}
	title := strings.TrimPrefix(strings.TrimPrefix(target, "="), spec.Prefix)
	_, _ = tmuxCmd("set-option", "-t", target+":", "set-titles-string", title)
	_, _ = tmuxCmd("set-option", "-t", target+":", "set-titles", "on")

	if os.Getenv("TMUX") != "" {
		return syscall.Exec(tmuxBin, []string{"tmux", "switch-client", "-t", target}, os.Environ())
	}
	return syscall.Exec(tmuxBin, []string{"tmux", "attach-session", "-t", target}, os.Environ())
}

func parseTrackedState(raw string) (trackedState, bool) {
	if strings.TrimSpace(raw) == "" {
		return trackedState{}, false
	}
	var st trackedState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return trackedState{}, false
	}
	return st, true
}

// SessionExists reports whether a session for harness/name exists in tmux or Rex.
func SessionExists(harness, name string) (bool, string) {
	spec, err := GetSpec(harness)
	if err != nil {
		return false, ""
	}
	session := spec.Prefix + name
	if tmuxHasSession("=" + session) {
		return true, "tmux"
	}
	if id := rexSessionID(spec, session); id != "" {
		return true, "rex"
	}
	return false, ""
}
