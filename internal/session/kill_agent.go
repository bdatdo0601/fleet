package session

import (
	"cmp"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// KillTarget identifies a running agent session to terminate locally.
type KillTarget struct {
	SessionID string `json:"session_id,omitempty"`
	Harness   string `json:"harness"`
	Name      string `json:"name,omitempty"`
	FullRawID string `json:"full_raw_id,omitempty"`
	PID       *int   `json:"pid,omitempty"`
}

func killManagedByRawID(spec Spec, rawID string, stdout io.Writer) bool {
	if rawID == "" {
		return false
	}
	if spec.Name == HarnessAgy {
		for _, st := range listAppSessionStates(HarnessAgy) {
			if st.ConversationID == rawID {
				removeAppSessionState(HarnessAgy, st.Name)
			}
		}
		if hub, err := discoverAgyHub(); err == nil {
			if uErr := agyUpdateConversationArchived(hub, rawID, true); uErr == nil {
				if stdout != nil {
					_, _ = fmt.Fprintf(stdout, "Archived Antigravity conversation %s\n", rawID)
				}
				return true
			}
		}
	}

	rDir := rexStateDir(spec.Name)
	if entries, err := os.ReadDir(rDir); err == nil {
		for _, ent := range entries {
			if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
				continue
			}
			if st, ok := readTrackedStateFile(filepath.Join(rDir, ent.Name())); ok && st.ID == rawID {
				name := strings.TrimSuffix(ent.Name(), ".json")
				if err := Kill(spec.Name, name, stdout); err == nil {
					return true
				}
			}
		}
	}

	pathEnv := AugmentedPath(os.Getenv("PATH"), os.Getenv("HOME"))
	if _, err := lookPathWithEnv("tmux", pathEnv); err == nil {
		for _, sess := range tmuxListSessions() {
			if !strings.HasPrefix(sess, spec.Prefix) {
				continue
			}
			panes, pErr := tmuxListPanes("=" + sess)
			if pErr != nil {
				continue
			}
			for _, p := range panes {
				if st, ok := parseTrackedState(tmuxShowPaneOption(p.ID, spec.PaneOption)); ok && st.ID == rawID {
					if _, kErr := tmuxCmd("kill-session", "-t", "="+sess); kErr == nil {
						if stdout != nil {
							_, _ = fmt.Fprintf(stdout, "Killed %s\n", strings.TrimPrefix(sess, spec.Prefix))
						}
						return true
					}
				}
			}
		}
	}
	return false
}

// KillAgent terminates a single agent session by managed name, raw conversation/session ID, or verified PID.
func KillAgent(t KillTarget, stdout, _ io.Writer) error {
	spec, err := GetSpec(t.Harness)
	if err != nil {
		return err
	}
	if t.Name != "" && ValidateName(t.Name) == nil {
		if exists, _ := SessionExists(spec.Name, t.Name); exists {
			return Kill(spec.Name, t.Name, stdout)
		}
	}
	if killManagedByRawID(spec, t.FullRawID, stdout) {
		return nil
	}
	if t.PID != nil && *t.PID > 1 && *t.PID != os.Getpid() {
		cmdLine, err := runCmdCapture("ps", "-o", "command=", "-p", strconv.Itoa(*t.PID))
		if err == nil && spec.MatchesProcess(cmdLine) {
			if kErr := syscall.Kill(*t.PID, syscall.SIGTERM); kErr == nil && stdout != nil {
				_, _ = fmt.Fprintf(stdout, "Killed %s pid %d\n", spec.Name, *t.PID)
			}
		}
	}
	return nil
}

// KillAllAgents terminates all managed sessions and any explicit agent targets for harness (or all harnesses when empty/"all").
func KillAllAgents(harness string, targets []KillTarget, stdout, stderr io.Writer) error {
	var harnesses []string
	trimmed := strings.TrimSpace(harness)
	if trimmed == "" || strings.EqualFold(trimmed, "all") {
		harnesses = []string{HarnessClaude, HarnessAgy, HarnessCodex}
	} else {
		h, ok := NormalizeHarness(trimmed)
		if !ok {
			return fmt.Errorf("unknown harness %q (expected claude, agy, codex, or all)", harness)
		}
		harnesses = []string{h}
	}

	for _, h := range harnesses {
		if err := KillAll(h, stdout, stderr); err != nil && !strings.Contains(err.Error(), "tmux is required") {
			if stderr != nil {
				_, _ = fmt.Fprintf(stderr, "kill-all %s: %v\n", h, err)
			}
		}
	}
	for _, t := range targets {
		if err := KillAgent(t, stdout, stderr); err != nil && stderr != nil {
			_, _ = fmt.Fprintf(stderr, "kill %s: %v\n", cmp.Or(t.Name, t.SessionID), err)
		}
	}
	return nil
}
