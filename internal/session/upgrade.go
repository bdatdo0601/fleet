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
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type tmuxUpgradeTarget struct {
	pane       string
	name       string
	id         string
	directory  string
	policy     Policy
	exitOption string
	pid        int
	stopping   bool
}

type rexUpgradeTarget struct {
	name     string
	pid      int
	stopping bool
}

// Upgrade stops all tracked sessions for harness, upgrades the harness CLI, and resumes every session in place.
func Upgrade(harness, selfBin string, stdout, stderr io.Writer) error {
	spec, err := GetSpec(harness)
	if err != nil {
		return err
	}
	pathEnv := AugmentedPath(os.Getenv("PATH"), os.Getenv("HOME"))
	_ = os.Setenv("PATH", pathEnv)
	if _, err := lookPathWithEnv("tmux", pathEnv); err != nil {
		return errors.New("tmux is required (macOS: brew install tmux)")
	}
	if spec.Name == HarnessClaude {
		if _, err := lookPathWithEnv("brew", pathEnv); err != nil {
			return errors.New("homebrew is required for upgrade")
		}
	}
	harnessBin, err := lookPathWithEnv(spec.Binary, pathEnv)
	if err != nil {
		return fmt.Errorf("%s is required. Install %s and sign in first", spec.Binary, spec.DisplayName)
	}

	stateRoot := stateRootFor(spec.Name)
	if err := os.MkdirAll(stateRoot, 0o755); err != nil {
		return err
	}
	lockDir := upgradeLockPath(spec.Name)
	if err := os.Mkdir(lockDir, 0o755); err != nil {
		return fmt.Errorf("another upgrade is running. If it was interrupted without cleanup, remove %s", lockDir)
	}
	lockRemoved := false
	removeLock := func() {
		if !lockRemoved {
			_ = os.Remove(lockDir)
			lockRemoved = true
		}
	}
	defer removeLock()

	tmuxTargets, err := collectTmuxUpgradeTargets(spec)
	if err != nil {
		return err
	}
	rexTargets, err := collectRexUpgradeTargets(spec)
	if err != nil {
		return err
	}

	self := resolveSelfBin(selfBin)
	if spec.Name == HarnessAgy && (len(tmuxTargets) > 0 || len(rexTargets) > 0) {
		if err := EnsureAgyHook(self, ""); err != nil {
			return err
		}
	}

	restartStarted := float64(time.Now().Unix())
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var interruptedSig os.Signal
	go func() {
		select {
		case s := <-sigCh:
			interruptedSig = s
			cancel()
		case <-ctx.Done():
		}
	}()

	var upgradeErr error
	defer func() {
		restoreAllSessions(spec, self, pathEnv, restartStarted, tmuxTargets, rexTargets, removeLock, stdout, stderr, &upgradeErr)
	}()

	if err := stopTargetsForUpgrade(ctx, spec, tmuxTargets, rexTargets, stdout); err != nil {
		upgradeErr = err
		return upgradeErr
	}
	if ctx.Err() != nil {
		upgradeErr = signalErr(interruptedSig)
		return upgradeErr
	}
	if err := runHarnessUpgradeCmd(ctx, spec, harnessBin, pathEnv, stdout, stderr); err != nil {
		if interruptedSig != nil {
			upgradeErr = signalErr(interruptedSig)
		} else {
			upgradeErr = err
		}
		return upgradeErr
	}
	return nil
}

func signalErr(sig os.Signal) error {
	if sig == nil {
		return errors.New("upgrade interrupted")
	}
	return fmt.Errorf("upgrade interrupted by %s", sig)
}

func collectTmuxUpgradeTargets(spec Spec) ([]*tmuxUpgradeTarget, error) {
	var targets []*tmuxUpgradeTarget
	selfPane := os.Getenv("TMUX_PANE")
	for _, sess := range tmuxListSessions() {
		if !strings.HasPrefix(sess, spec.Prefix) {
			continue
		}
		panes, err := tmuxListPanes("=" + sess)
		if err != nil {
			continue
		}
		tracked := false
		for _, p := range panes {
			rawState := tmuxShowPaneOption(p.ID, spec.PaneOption)
			if rawState == "" {
				if tmuxShowPaneOption(p.ID, spec.ManagedOption) == "1" && !p.Dead {
					return nil, fmt.Errorf("no saved conversation for %s yet. Send a message in %s before upgrading", sess, spec.DisplayName)
				}
				continue
			}
			tracked = true
			if selfPane != "" && p.ID == selfPane {
				return nil, fmt.Errorf("run upgrade from a separate terminal, outside the %s panes being restarted", spec.DisplayName)
			}
			st, ok := parseTrackedState(rawState)
			if !ok || st.ID == "" {
				return nil, fmt.Errorf("missing conversation ID in %s", sess)
			}
			if st.Directory == "" {
				return nil, fmt.Errorf("missing directory in %s", sess)
			}
			if fi, err := os.Stat(st.Directory); err != nil || !fi.IsDir() {
				return nil, fmt.Errorf("directory no longer exists: %s", st.Directory)
			}
			if err := validateTranscriptForUpgrade(spec, sess, st.Transcript); err != nil {
				return nil, err
			}
			if !p.Dead && st.PID != p.PID {
				return nil, fmt.Errorf("conversation tracking is not ready in %s. Attach to complete %s's startup prompts first", sess, spec.DisplayName)
			}
			pidInt, _ := strconv.Atoi(p.PID)
			pol, _ := ParsePolicy(st.Policy, PolicyBypass)
			exitOpt := tmuxShowPaneOption(p.ID, spec.ExitOption)
			if exitOpt == "" {
				exitOpt, _ = tmuxDisplay(p.ID, "#{remain-on-exit}")
			}
			targets = append(targets, &tmuxUpgradeTarget{
				pane:       p.ID,
				name:       strings.TrimPrefix(sess, spec.Prefix),
				id:         st.ID,
				directory:  st.Directory,
				policy:     pol,
				exitOption: cmp.Or(exitOpt, "off"),
				pid:        pidInt,
			})
		}
		if !tracked && tmuxRunsHarness(spec, sess) {
			short := strings.TrimPrefix(sess, spec.Prefix)
			return nil, fmt.Errorf("session '%s' has no conversation tracking. Exit it and restart it with `fleet session %s` before upgrading", short, spec.Name)
		}
	}
	return targets, nil
}

func validateTranscriptForUpgrade(spec Spec, sessionLabel, transcript string) error {
	if spec.RequiresTranscript {
		if transcript == "" {
			return fmt.Errorf("missing transcript path in %s", sessionLabel)
		}
		if _, err := os.Stat(transcript); err != nil {
			return fmt.Errorf("no saved conversation for %s yet. Send a message in %s before upgrading", sessionLabel, spec.DisplayName)
		}
		return nil
	}
	if transcript != "" {
		if _, err := os.Stat(transcript); err != nil {
			return fmt.Errorf("no saved conversation for %s yet. Send a message in %s before upgrading", sessionLabel, spec.DisplayName)
		}
	}
	return nil
}

func collectRexUpgradeTargets(spec Spec) ([]*rexUpgradeTarget, error) {
	var targets []*rexUpgradeTarget
	rDir := rexStateDir(spec.Name)
	curBlock := os.Getenv("REX_BLOCK")
	for _, rs := range rexSessionsFor(spec) {
		rexName := strings.TrimPrefix(rs.Label, spec.Prefix)
		pid, ok := rexChildPID(spec, rexName)
		if !ok {
			continue
		}
		_, block, _ := rexWrapperPID(spec, rexName)
		if curBlock != "" && block == curBlock {
			return nil, fmt.Errorf("run upgrade from a separate terminal, outside the %s panes being restarted", spec.DisplayName)
		}
		st, ok := readTrackedStateFile(filepath.Join(rDir, rexName+".json"))
		if !ok {
			return nil, fmt.Errorf("conversation tracking is not ready in Rex session %s. Open it to complete %s's startup prompts first", rexName, spec.DisplayName)
		}
		if st.ID == "" {
			return nil, fmt.Errorf("missing conversation ID in Rex session %s", rexName)
		}
		if st.Directory != "" {
			if fi, err := os.Stat(st.Directory); err != nil || !fi.IsDir() {
				return nil, fmt.Errorf("directory no longer exists: %s", st.Directory)
			}
		}
		if err := validateTranscriptForUpgrade(spec, "Rex session "+rexName, st.Transcript); err != nil {
			return nil, err
		}
		targets = append(targets, &rexUpgradeTarget{
			name: rexName,
			pid:  pid,
		})
	}
	return targets, nil
}

func stopTargetsForUpgrade(ctx context.Context, spec Spec, tmuxTargets []*tmuxUpgradeTarget, rexTargets []*rexUpgradeTarget, stdout io.Writer) error {
	for _, t := range tmuxTargets {
		_ = tmuxSetPaneOption(t.pane, spec.ExitOption, t.exitOption)
		_ = tmuxSetPaneOption(t.pane, "remain-on-exit", "on")
	}
	for _, t := range tmuxTargets {
		dead, _ := tmuxDisplay(t.pane, "#{pane_dead}")
		if dead == "0" && t.pid > 0 {
			_, _ = fmt.Fprintf(stdout, "Stopping %s\n", t.name)
			t.stopping = true
			_ = syscall.Kill(t.pid, syscall.SIGTERM)
		}
	}
	rDir := rexStateDir(spec.Name)
	for _, rt := range rexTargets {
		_, _ = fmt.Fprintf(stdout, "Stopping %s (rex)\n", rt.name)
		_ = os.WriteFile(filepath.Join(rDir, rt.name+".restart"), nil, 0o644)
		rt.stopping = true
		_ = syscall.Kill(rt.pid, syscall.SIGTERM)
	}

	deadline := time.Now().Add(30 * time.Second)
	for _, t := range tmuxTargets {
		for {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if dead, _ := tmuxDisplay(t.pane, "#{pane_dead}"); dead == "1" {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s did not stop within 30 seconds; upgrade cancelled. Wait for it to exit and rerun fleet session %s upgrade", spec.DisplayName, spec.Name)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	for _, rt := range rexTargets {
		for syscall.Kill(rt.pid, 0) == nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s did not stop within 30 seconds; upgrade cancelled. Wait for it to exit and rerun fleet session %s upgrade", spec.DisplayName, spec.Name)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	return nil
}

func useBrewCask(binPath, caskName, pathEnv string) bool {
	if _, err := lookPathWithEnv("brew", pathEnv); err != nil {
		return false
	}
	out, err := runCmdCapture("brew", "--prefix")
	if err != nil {
		return false
	}
	prefix := strings.TrimSpace(out)
	if prefix == "" || !strings.HasPrefix(binPath, strings.TrimRight(prefix, "/")+"/") {
		return false
	}
	return exec.Command("brew", "list", "--cask", caskName).Run() == nil
}

func runHarnessUpgradeCmd(ctx context.Context, spec Spec, harnessBin, pathEnv string, stdout, stderr io.Writer) error {
	var cmd *exec.Cmd
	switch spec.Name {
	case HarnessClaude:
		_, _ = fmt.Fprintln(stdout, "Running: brew upgrade claude-code@latest")
		cmd = exec.CommandContext(ctx, "brew", "upgrade", "claude-code@latest")
	case HarnessAgy:
		if useBrewCask(harnessBin, "antigravity-cli", pathEnv) {
			_, _ = fmt.Fprintln(stdout, "Running: brew upgrade --cask antigravity-cli")
			cmd = exec.CommandContext(ctx, "brew", "upgrade", "--cask", "antigravity-cli")
		} else {
			_, _ = fmt.Fprintf(stdout, "Running: %s update\n", harnessBin)
			cmd = exec.CommandContext(ctx, harnessBin, "update")
		}
	case HarnessCodex:
		if useBrewCask(harnessBin, "codex", pathEnv) {
			_, _ = fmt.Fprintln(stdout, "Running: brew upgrade --cask codex")
			cmd = exec.CommandContext(ctx, "brew", "upgrade", "--cask", "codex")
		} else {
			_, _ = fmt.Fprintf(stdout, "Running: %s update\n", harnessBin)
			cmd = exec.CommandContext(ctx, harnessBin, "update")
		}
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func restoreAllSessions(
	spec Spec,
	self string,
	pathEnv string,
	restartStarted float64,
	tmuxTargets []*tmuxUpgradeTarget,
	rexTargets []*rexUpgradeTarget,
	removeLock func(),
	stdout, stderr io.Writer,
	outErr *error,
) {
	var restoreErrs []error
	for _, t := range tmuxTargets {
		if err := restoreOneTmuxTarget(spec, self, pathEnv, t, stdout, stderr); err != nil {
			restoreErrs = append(restoreErrs, err)
		}
	}
	removeLock()
	for _, rt := range rexTargets {
		if !rt.stopping {
			continue
		}
		if err := waitRexResume(spec, rt, restartStarted, stdout, stderr); err != nil {
			restoreErrs = append(restoreErrs, err)
		}
	}
	if *outErr == nil && len(restoreErrs) > 0 {
		*outErr = errors.Join(restoreErrs...)
	}
}

func restoreOneTmuxTarget(spec Spec, self, pathEnv string, t *tmuxUpgradeTarget, stdout, stderr io.Writer) error {
	dead, err := tmuxDisplay(t.pane, "#{pane_dead}")
	if err != nil {
		return err
	}
	if t.stopping && dead != "1" {
		deadline := time.Now().Add(30 * time.Second)
		for dead == "0" && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
			dead, err = tmuxDisplay(t.pane, "#{pane_dead}")
			if err != nil {
				break
			}
		}
		if dead != "1" {
			_, _ = fmt.Fprintf(stderr, "fleet session %s: %s is still stopping; its pane is preserved. Rerun upgrade after it exits.\n", spec.Name, t.name)
			return fmt.Errorf("%s is still stopping", t.name)
		}
	}
	if dead == "1" {
		bin, bErr := lookPathWithEnv(spec.Binary, pathEnv)
		if bErr != nil {
			_, _ = fmt.Fprintf(stderr, "fleet session %s: could not resume %s; fix the installation and rerun upgrade.\n", spec.Name, t.name)
			return bErr
		}
		args, aErr := BuildHarnessArgs(spec, t.name, self, t.id, "", t.policy)
		if aErr != nil {
			return aErr
		}
		respawnArgs := []string{
			"respawn-pane", "-t", t.pane, "-c", t.directory,
			"-e", "PATH=" + pathEnv,
			"-e", "FLEET_SESSION_TARGET=tmux",
			"-e", "FLEET_SESSION_NAME=" + t.name,
			"-e", "FLEET_SESSION_POLICY=" + string(t.policy),
		}
		if spec.Name == HarnessAgy {
			respawnArgs = append(respawnArgs, "-e", "AGY_SESSION_TARGET=tmux", "-e", "AGY_SESSION_NAME="+t.name)
		}
		respawnArgs = append(respawnArgs, bin)
		respawnArgs = append(respawnArgs, args...)
		if _, rErr := tmuxCmd(respawnArgs...); rErr != nil {
			_, _ = fmt.Fprintf(stderr, "fleet session %s: could not resume %s; fix the installation and rerun upgrade.\n", spec.Name, t.name)
			return rErr
		}
		if !waitTmuxResumed(spec, t.pane) {
			_, _ = fmt.Fprintf(stderr, "fleet session %s: %s did not finish resuming; its pane is preserved. Attach to inspect startup prompts or errors, then rerun upgrade.\n", spec.Name, t.name)
			return fmt.Errorf("%s did not finish resuming", t.name)
		}
		_, _ = fmt.Fprintf(stdout, "Resumed %s\n", t.name)
	}
	_ = tmuxSetPaneOption(t.pane, "remain-on-exit", t.exitOption)
	_ = tmuxUnsetPaneOption(t.pane, spec.ExitOption)
	return nil
}

func waitTmuxResumed(spec Spec, pane string) bool {
	pid, err := tmuxDisplay(pane, "#{pane_pid}")
	if err != nil || pid == "" {
		return false
	}
	if spec.Name == HarnessClaude {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if st, ok := parseTrackedState(tmuxShowPaneOption(pane, spec.PaneOption)); ok && st.PID == pid {
				return true
			}
			if dead, _ := tmuxDisplay(pane, "#{pane_dead}"); dead != "0" {
				return false
			}
			time.Sleep(100 * time.Millisecond)
		}
		return false
	}
	ticks := resumeGraceTenths()
	hookFired := false
	for range ticks {
		if st, ok := parseTrackedState(tmuxShowPaneOption(pane, spec.PaneOption)); ok && st.PID == pid {
			hookFired = true
			break
		}
		if dead, _ := tmuxDisplay(pane, "#{pane_dead}"); dead != "0" {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !hookFired {
		dead, _ := tmuxDisplay(pane, "#{pane_dead}")
		if dead != "0" {
			return false
		}
		if st, ok := parseTrackedState(tmuxShowPaneOption(pane, spec.PaneOption)); ok {
			st.PID = pid
			if b, err := json.Marshal(st); err == nil {
				_ = tmuxSetPaneOption(pane, spec.PaneOption, string(b))
				return true
			}
		}
		return false
	}
	return true
}

func waitRexResume(spec Spec, rt *rexUpgradeTarget, restartStarted float64, stdout, stderr io.Writer) error {
	rDir := rexStateDir(spec.Name)
	statePath := filepath.Join(rDir, rt.name+".json")
	deadline := time.Now().Add(60 * time.Second)

	if spec.Name == HarnessClaude {
		for time.Now().Before(deadline) {
			if _, ok := rexChildPID(spec, rt.name); ok {
				if st, sOK := readTrackedStateFile(statePath); sOK && st.Started >= restartStarted {
					_, _ = fmt.Fprintf(stdout, "Resumed %s (rex)\n", rt.name)
					return nil
				}
			}
			if _, _, wOK := rexWrapperPID(spec, rt.name); !wOK {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		_, _ = fmt.Fprintf(stderr, "fleet session %s: Rex session %s has not resumed yet; open it in Rex to check for startup prompts or errors.\n", spec.Name, rt.name)
		return fmt.Errorf("rex session %s did not resume", rt.name)
	}

	newPID := 0
	for time.Now().Before(deadline) {
		if cand, ok := rexChildPID(spec, rt.name); ok && cand != rt.pid {
			newPID = cand
			break
		}
		if _, _, wOK := rexWrapperPID(spec, rt.name); !wOK {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	ready := false
	if newPID > 0 {
		ticks := resumeGraceTenths()
		for range ticks {
			if st, sOK := readTrackedStateFile(statePath); sOK && st.Started >= restartStarted {
				ready = true
				break
			}
			if syscall.Kill(newPID, 0) != nil {
				break
			}
			if _, _, wOK := rexWrapperPID(spec, rt.name); !wOK {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !ready && syscall.Kill(newPID, 0) == nil {
			if _, _, wOK := rexWrapperPID(spec, rt.name); wOK {
				ready = true
			}
		}
	}
	if ready {
		_, _ = fmt.Fprintf(stdout, "Resumed %s (rex)\n", rt.name)
		return nil
	}
	_, _ = fmt.Fprintf(stderr, "fleet session %s: Rex session %s has not resumed yet; open it in Rex to check for startup prompts or errors.\n", spec.Name, rt.name)
	return fmt.Errorf("rex session %s did not resume", rt.name)
}
