package session

import (
	"cmp"
	"errors"
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

func killAgyAppByRawID(rawID string, stdout io.Writer) bool {
	for _, st := range listAppSessionStates(HarnessAgy) {
		if st.ConversationID == rawID {
			removeAppSessionState(st.Name)
		}
	}
	hub, err := discoverAgyHub()
	if err != nil {
		return false
	}
	if uErr := agyUpdateConversationArchived(hub, rawID, true); uErr == nil {
		_, _ = fmt.Fprintf(stdout, "Archived Antigravity conversation %s\n", rawID)
		return true
	}
	return false
}

func killRexByRawID(spec Spec, rawID string, stdout io.Writer) bool {
	rDir := rexStateDir(spec.Name)
	entries, err := os.ReadDir(rDir)
	if err != nil {
		return false
	}
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
	return false
}

func killTmuxByRawID(spec Spec, rawID string, stdout io.Writer) bool {
	pathEnv := AugmentedPath(os.Getenv("PATH"), os.Getenv("HOME"))
	if _, err := lookPathWithEnv("tmux", pathEnv); err != nil {
		return false
	}
	for _, sess := range tmuxListSessions() {
		if !strings.HasPrefix(sess, spec.Prefix) {
			continue
		}
		panes, pErr := tmuxListPanes("=" + sess)
		if pErr != nil {
			continue
		}
		for _, p := range panes {
			st, ok := parseTrackedState(tmuxShowPaneOption(p.ID, spec.PaneOption))
			if !ok || st.ID != rawID {
				continue
			}
			if _, kErr := tmuxCmd("kill-session", "-t", "="+sess); kErr == nil {
				_, _ = fmt.Fprintf(stdout, "Killed %s\n", strings.TrimPrefix(sess, spec.Prefix))
				return true
			}
		}
	}
	return false
}

func killManagedByRawID(spec Spec, rawID string, stdout io.Writer) bool {
	if rawID == "" {
		return false
	}
	stdout = cmp.Or(stdout, io.Discard)
	if spec.Name == HarnessAgy && killAgyAppByRawID(rawID, stdout) {
		return true
	}
	if killRexByRawID(spec, rawID, stdout) {
		return true
	}
	return killTmuxByRawID(spec, rawID, stdout)
}

// KillAgent terminates a single agent session by managed name, raw conversation/session ID, or verified PID.
func KillAgent(t KillTarget, stdout, _ io.Writer) error {
	stdout = cmp.Or(stdout, io.Discard)
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
			if kErr := syscall.Kill(*t.PID, syscall.SIGTERM); kErr == nil {
				_, _ = fmt.Fprintf(stdout, "Killed %s pid %d\n", spec.Name, *t.PID)
			}
		}
	}
	return nil
}

// KillAllAgents terminates all managed sessions and any explicit agent targets for harness (or all harnesses when empty/"all").
func KillAllAgents(harness string, targets []KillTarget, stdout, stderr io.Writer) error {
	stdout = cmp.Or(stdout, io.Discard)
	stderr = cmp.Or(stderr, io.Discard)
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
		if err := KillAll(h, stdout, stderr); err != nil && !errors.Is(err, ErrTmuxRequired) {
			_, _ = fmt.Fprintf(stderr, "kill-all %s: %v\n", h, err)
		}
	}
	for _, t := range targets {
		if err := KillAgent(t, stdout, stderr); err != nil {
			_, _ = fmt.Fprintf(stderr, "kill %s: %v\n", cmp.Or(t.Name, t.SessionID), err)
		}
	}
	return nil
}
