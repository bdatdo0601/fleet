package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var testFleetBin string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "fleet-session-bin-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkdtemp: %v\n", err)
		os.Exit(1)
	}
	bin := filepath.Join(tmp, "fleet")
	buildCmd := exec.Command("go", "build", "-o", bin, "../../cmd/fleet")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "go build ../../cmd/fleet failed: %v\n%s\n", err, string(out))
		_ = os.RemoveAll(tmp)
		os.Exit(1)
	}
	testFleetBin = bin
	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

type sessionEnv struct {
	t        *testing.T
	base     string
	binDir   string
	socket   string
	realTmux string
}

func newSessionEnv(t *testing.T) *sessionEnv {
	t.Helper()
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not installed")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(base, "bin")
	_ = os.MkdirAll(binDir, 0o755)

	socket := fmt.Sprintf("fleet-sess-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		_ = exec.Command(realTmux, "-L", socket, "kill-server").Run()
	})

	// Strip any host PATH directory containing real `rex` so tmux tests never touch host Rex.
	var filteredPath []string
	for p := range strings.SplitSeq(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if p == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(p, "rex")); err == nil {
			continue
		}
		filteredPath = append(filteredPath, p)
	}
	t.Setenv("HOME", base)
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+strings.Join(filteredPath, string(os.PathListSeparator)))
	t.Setenv("FLEET_SESSION_SELF_BIN", testFleetBin)
	t.Setenv("AGY_SESSION_RESUME_GRACE_TENTHS", "3")
	t.Setenv("AGY_HOOKS_FILE", filepath.Join(base, ".gemini", "config", "hooks.json"))
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("REX_SESSION", "")
	t.Setenv("REX_BLOCK", "")
	t.Setenv("FLEET_SESSION_TARGET", "")
	t.Setenv("FLEET_SESSION_NAME", "")
	t.Setenv("FLEET_SESSION_POLICY", "")
	t.Setenv("FLEET_SESSION_STATE_FILE", "")
	t.Setenv("AGY_SESSION_TARGET", "")
	t.Setenv("AGY_SESSION_NAME", "")
	t.Setenv("AGY_SESSION_STATE_FILE", "")

	se := &sessionEnv{
		t:        t,
		base:     base,
		binDir:   binDir,
		socket:   socket,
		realTmux: realTmux,
	}
	se.writeExec("tmux", fmt.Sprintf(`#!/bin/bash
case "$1" in
  attach-session|switch-client)
    printf '%%s\n' "$@" > %s
    exit 0 ;;
esac
exec %s -L %s -f /dev/null "$@"
`, shellQuote(filepath.Join(base, "attach")), shellQuote(realTmux), shellQuote(socket)))

	se.installFakeClaude()
	se.installFakeAgy()
	se.installFakeCodex()
	se.installFakeBrew()
	return se
}

func (se *sessionEnv) writeExec(name, body string) string {
	p := filepath.Join(se.binDir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		se.t.Fatal(err)
	}
	return p
}

func (se *sessionEnv) installFakeClaude() {
	se.writeExec("claude", fmt.Sprintf(`#!/usr/bin/env python3
import json, os, signal, subprocess, sys, uuid
from pathlib import Path
args = sys.argv[1:]
base = Path(%q)
name = args[args.index("--remote-control") + 1]
session_id = args[args.index("--resume") + 1] if "--resume" in args else str(uuid.uuid4())
transcript = base / (session_id + ".jsonl")
if "--resume" in args and (not transcript.exists() or os.environ.get("FAKE_RESUME_FAIL")):
    sys.exit(1)
transcript.touch()
settings = json.loads(args[args.index("--settings") + 1])
hook = settings["hooks"]["SessionStart"][0]["hooks"][0]["command"]
subprocess.run(hook, shell=True, check=True, input=json.dumps({
    "session_id": session_id, "cwd": os.getcwd(), "transcript_path": str(transcript)
}), text=True)
record = {"cwd": os.getcwd(), "args": args, "tty": os.isatty(0), "id": session_id, "pid": os.getpid()}
(base / f"claude-{name}.json").write_text(json.dumps(record))
def stop(*_):
    sys.exit(0)
signal.signal(signal.SIGTERM, stop)
if os.environ.get("FLEET_SESSION_TARGET", "tmux") == "tmux" and not os.environ.get("REX_SESSION"):
    subprocess.run(["tmux", "wait-for", "-S", "ready-claude-" + name], check=True)
else:
    (base / ("rex-ready-claude-" + name)).write_text(str(os.getpid()))
while True:
    signal.pause()
`, se.base))
}

func (se *sessionEnv) installFakeAgy() {
	se.writeExec("agy", fmt.Sprintf(`#!/usr/bin/env python3
import json, os, signal, subprocess, sys, uuid
from pathlib import Path
args = sys.argv[1:]
base = Path(%q)
if args == ["update"]:
    (base / "agy-update.json").write_text(json.dumps({"args": args}))
    sys.exit(0)
is_resume = "--conversation" in args
session_id = args[args.index("--conversation") + 1] if is_resume else str(uuid.uuid4())
transcript = base / (session_id + ".jsonl")
if is_resume and (not transcript.exists() or os.environ.get("FAKE_RESUME_FAIL")):
    sys.exit(1)
hooks_path = Path(os.environ.get("AGY_HOOKS_FILE", str(Path.home() / ".gemini/config/hooks.json")))
hooks_data = json.loads(hooks_path.read_text()) if hooks_path.exists() else {}
entry = hooks_data.get("agy-session", {})
if not is_resume and not os.environ.get("FAKE_SKIP_SESSION_START"):
    transcript.touch()
    for handler in entry.get("SessionStart", []):
        res = subprocess.run(handler["command"], shell=True, check=True, text=True, capture_output=True, input=json.dumps({
            "conversationId": session_id, "workspacePaths": [os.getcwd()], "cwd": os.getcwd(), "transcriptPath": str(transcript)
        }))
        json.loads(res.stdout)
name = os.environ.get("FLEET_SESSION_NAME") or os.environ.get("AGY_SESSION_NAME") or "unnamed"
record = {"cwd": os.getcwd(), "args": args, "tty": os.isatty(0), "id": session_id, "pid": os.getpid()}
(base / f"agy-{name}.json").write_text(json.dumps(record))
signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
if os.environ.get("FLEET_SESSION_TARGET") == "tmux":
    subprocess.run(["tmux", "wait-for", "-S", "ready-agy-" + name], check=True)
else:
    (base / ("rex-ready-agy-" + name)).write_text(str(os.getpid()))
while True:
    signal.pause()
`, se.base))
}

func (se *sessionEnv) installFakeCodex() {
	se.writeExec("codex", fmt.Sprintf(`#!/usr/bin/env python3
import json, os, signal, subprocess, sys, uuid
from pathlib import Path
args = sys.argv[1:]
base = Path(%q)
if args[:2] == ["remote-control", "start"]:
    (base / "codex-rc.json").write_text(json.dumps({"args": args}))
    sys.exit(0)
if args == ["update"]:
    (base / "codex-update.json").write_text(json.dumps({"args": args}))
    sys.exit(0)
is_resume = len(args) > 0 and args[0] == "resume"
session_id = str(uuid.uuid4())
if is_resume:
    for a in args[1:]:
        if not a.startswith("-"):
            session_id = a
            break
transcript = base / (session_id + ".jsonl")
transcript.touch()
name = os.environ.get("FLEET_SESSION_NAME") or "unnamed"
subprocess.run([%q, "session", "codex", "__hook"], check=True, text=True, input=json.dumps({
    "session_id": session_id, "cwd": os.getcwd(), "transcript_path": str(transcript)
}))
record = {"cwd": os.getcwd(), "args": args, "tty": os.isatty(0), "id": session_id, "pid": os.getpid()}
(base / f"codex-{name}.json").write_text(json.dumps(record))
signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
if os.environ.get("FLEET_SESSION_TARGET") == "tmux":
    subprocess.run(["tmux", "wait-for", "-S", "ready-codex-" + name], check=True)
else:
    (base / ("rex-ready-codex-" + name)).write_text(str(os.getpid()))
while True:
    signal.pause()
`, se.base, testFleetBin))
}

func (se *sessionEnv) installFakeBrew() {
	se.writeExec("brew", fmt.Sprintf(`#!/usr/bin/env python3
import json, sys
from pathlib import Path
args = sys.argv[1:]
if args == ["--prefix"]:
    print(%q)
    sys.exit(0)
if args[:2] == ["list", "--cask"]:
    sys.exit(0)
Path(%q).write_text(json.dumps({"args": args}))
sys.exit(0)
`, se.base, filepath.Join(se.base, "brew.json")))
}

func (se *sessionEnv) installFakeRex() string {
	rexDir := filepath.Join(se.base, "rex-daemon")
	_ = os.MkdirAll(rexDir, 0o755)
	_ = os.WriteFile(filepath.Join(rexDir, "sessions.json"), []byte("[]"), 0o644)
	_ = os.WriteFile(filepath.Join(rexDir, "clients.json"), []byte(`[{"client_id":"app-1","info":{"kind":"app"}}]`), 0o644)
	_ = os.WriteFile(filepath.Join(rexDir, "actions.json"), []byte("[]"), 0o644)

	se.t.Cleanup(func() {
		raw, err := os.ReadFile(filepath.Join(rexDir, "sessions.json"))
		if err != nil {
			return
		}
		var list []struct {
			PID int `json:"pid"`
		}
		if json.Unmarshal(raw, &list) == nil {
			for _, s := range list {
				if s.PID > 0 {
					_ = exec.Command("kill", "-TERM", fmt.Sprintf("-%d", s.PID)).Run()
				}
			}
		}
	})

	se.writeExec("rex", fmt.Sprintf(`#!/usr/bin/env python3
import json, os, signal, subprocess, sys, uuid
from pathlib import Path
rex_dir = Path(%q)
sessions_file = rex_dir / "sessions.json"
clients_file = rex_dir / "clients.json"
actions_file = rex_dir / "actions.json"
args = sys.argv[1:]
if args == ["ls", "--json"]:
    alive = []
    for s in json.loads(sessions_file.read_text()):
        try:
            os.kill(s["pid"], 0)
            alive.append(s)
        except OSError:
            pass
    sessions_file.write_text(json.dumps(alive))
    print(json.dumps({"sessions": [{"label": s["label"], "session_id": s["session_id"]} for s in alive]}))
    sys.exit(0)
if args == ["client", "ls", "--json"]:
    print(json.dumps({"clients": json.loads(clients_file.read_text())}))
    sys.exit(0)
if args and args[0] == "-C":
    acts = json.loads(actions_file.read_text())
    acts.append({"client": args[1], "args": args[2:]})
    actions_file.write_text(json.dumps(acts))
    print("ok")
    sys.exit(0)
if args and args[0] == "new":
    label = args[1]
    cwd = args[args.index("-c") + 1]
    cmd = args[args.index("--") + 1:]
    sid = "rex-" + uuid.uuid4().hex[:8]
    bid = "blk-" + uuid.uuid4().hex[:8]
    env = dict(os.environ, REX_SESSION=sid, REX_BLOCK=bid)
    log = open(rex_dir / (sid + ".log"), "a")
    proc = subprocess.Popen(cmd, cwd=cwd, env=env, start_new_session=True, stdin=subprocess.DEVNULL, stdout=log, stderr=log)
    sessions = json.loads(sessions_file.read_text())
    sessions.append({"label": label, "session_id": sid, "block_id": bid, "pid": proc.pid, "cwd": cwd})
    sessions_file.write_text(json.dumps(sessions))
    print(json.dumps({"session_id": sid, "initial_windows": [{"block_ids": [bid]}]}))
    sys.exit(0)
if args and args[0] == "split":
    acts = json.loads(actions_file.read_text())
    acts.append({"split": args})
    actions_file.write_text(json.dumps(acts))
    sys.exit(0)
if args and args[0] == "kill":
    sid = args[1]
    kept = []
    for s in json.loads(sessions_file.read_text()):
        if s["session_id"] == sid:
            try:
                os.killpg(s["pid"], signal.SIGTERM)
            except OSError:
                pass
        else:
            kept.append(s)
    sessions_file.write_text(json.dumps(kept))
    sys.exit(0)
sys.exit(1)
`, rexDir))
	return rexDir
}

func (se *sessionEnv) waitTmuxReady(harness, name string) {
	se.t.Helper()
	cmd := exec.Command(filepath.Join(se.binDir, "tmux"), "wait-for", fmt.Sprintf("ready-%s-%s", harness, name))
	if out, err := cmd.CombinedOutput(); err != nil {
		se.t.Fatalf("waitTmuxReady(%s, %s): %v (%s)", harness, name, err, string(out))
	}
}

func (se *sessionEnv) waitRexReady(harness, name string, prevPID int) int {
	se.t.Helper()
	marker := filepath.Join(se.base, fmt.Sprintf("rex-ready-%s-%s", harness, name))
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(marker); err == nil {
			var pid int
			_, _ = fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &pid)
			if pid > 0 && pid != prevPID {
				return pid
			}
		}
		time.Sleep(40 * time.Millisecond)
	}
	se.t.Fatalf("timed out waiting for Rex %s/%s ready", harness, name)
	return 0
}

type fakeRecord struct {
	CWD  string   `json:"cwd"`
	Args []string `json:"args"`
	TTY  bool     `json:"tty"`
	ID   string   `json:"id"`
	PID  int      `json:"pid"`
}

func (se *sessionEnv) readRecord(harness, name string) fakeRecord {
	se.t.Helper()
	raw, err := os.ReadFile(filepath.Join(se.base, fmt.Sprintf("%s-%s.json", harness, name)))
	if err != nil {
		se.t.Fatal(err)
	}
	var rec fakeRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		se.t.Fatal(err)
	}
	return rec
}

func TestClaudeAndAgyAndCodexTmuxLifecycleAndUpgrade(t *testing.T) {
	se := newSessionEnv(t)
	specialDir := filepath.Join(se.base, "space ' quote; $(touch INJECTED)")
	if err := os.MkdirAll(specialDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Claude start + list + duplicate rejection + upgrade + kill + kill-all
	var out, errBuf bytes.Buffer
	if _, err := Start(StartOpts{Harness: "claude", Name: "alpha", Directory: specialDir, Stdout: &out, Stderr: &errBuf}); err != nil {
		t.Fatal(err)
	}
	se.waitTmuxReady("claude", "alpha")
	rec := se.readRecord("claude", "alpha")
	if rec.CWD != specialDir || !rec.TTY {
		t.Fatalf("unexpected claude record: %+v", rec)
	}
	if !slices.Contains(rec.Args, "--dangerously-skip-permissions") || !slices.Contains(rec.Args, "--remote-control") {
		t.Fatalf("unexpected claude args: %v", rec.Args)
	}
	if _, err := Start(StartOpts{Harness: "claude", Name: "alpha", Directory: specialDir, Stdout: &out, Stderr: &errBuf}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected duplicate rejection, got %v", err)
	}

	out.Reset()
	if err := List("claude", &out); err != nil || strings.TrimSpace(out.String()) != "alpha" {
		t.Fatalf("list claude failed: out=%q err=%v", out.String(), err)
	}

	out.Reset()
	if err := Upgrade("claude", "", &out, &errBuf); err != nil {
		t.Fatalf("claude upgrade failed: %v (%s)", err, errBuf.String())
	}
	se.waitTmuxReady("claude", "alpha")
	after := se.readRecord("claude", "alpha")
	if after.ID != rec.ID || after.PID == rec.PID {
		t.Fatalf("claude upgrade did not preserve ID with new PID: before=%+v after=%+v", rec, after)
	}

	// Agy start + Codex start
	if _, err := Start(StartOpts{Harness: "agy", Name: "beta", Directory: specialDir, Stdout: &out, Stderr: &errBuf}); err != nil {
		t.Fatal(err)
	}
	se.waitTmuxReady("agy", "beta")
	agyRec := se.readRecord("agy", "beta")
	if !slices.Equal(agyRec.Args, []string{"--dangerously-skip-permissions", "--remote-control"}) {
		t.Fatalf("unexpected agy args: %v", agyRec.Args)
	}

	if _, err := Start(StartOpts{Harness: "codex", Name: "gamma", Directory: specialDir, Stdout: &out, Stderr: &errBuf}); err != nil {
		t.Fatal(err)
	}
	se.waitTmuxReady("codex", "gamma")
	cdxRec := se.readRecord("codex", "gamma")
	if !slices.Equal(cdxRec.Args, []string{"--dangerously-bypass-approvals-and-sandbox"}) {
		t.Fatalf("unexpected codex args: %v", cdxRec.Args)
	}
	if _, err := os.Stat(filepath.Join(se.base, "codex-rc.json")); err != nil {
		t.Fatalf("expected codex remote-control start invocation: %v", err)
	}
	if err := Upgrade("codex", "", &out, &errBuf); err != nil {
		t.Fatalf("codex upgrade failed: %v", err)
	}
	se.waitTmuxReady("codex", "gamma")
	cdxAfter := se.readRecord("codex", "gamma")
	if cdxAfter.ID != cdxRec.ID || !slices.Contains(cdxAfter.Args, "resume") || !slices.Contains(cdxAfter.Args, cdxRec.ID) {
		t.Fatalf("codex upgrade did not resume exact ID: %+v", cdxAfter)
	}

	// Kill all claude sessions leaves agy & codex untouched
	out.Reset()
	if err := KillAll("claude", &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	if exists, _ := SessionExists("claude", "alpha"); exists {
		t.Fatal("expected claude alpha killed")
	}
	if exists, _ := SessionExists("agy", "beta"); !exists {
		t.Fatal("expected agy beta still alive")
	}
	if exists, _ := SessionExists("codex", "gamma"); !exists {
		t.Fatal("expected codex gamma still alive")
	}
	if _, err := os.Stat("INJECTED"); err == nil {
		t.Fatal("shell injection created INJECTED file")
	}
}

func TestStartTaskSafePoliciesAndMultilinePromptsAcrossTmuxAndRex(t *testing.T) {
	se := newSessionEnv(t)
	rexDir := se.installFakeRex()
	se.writeExec("lazygit", "#!/bin/sh\nexit 0\n")

	repo := filepath.Join(se.base, "repo")
	_ = os.MkdirAll(repo, 0o755)
	_ = exec.Command("git", "init", "-q", repo).Run()

	prompt := "Line 1 with 'quotes' and \"double\"\nLine 2 $(touch INJECTED) && `backtick`\nLine 3: fix tests"
	var out, errBuf bytes.Buffer

	// 1. Claude start-task in tmux -> manual approvals, NO --dangerously-skip-permissions, upgrade preserves safe policy
	backend, err := Start(StartOpts{
		Harness: "claude", Name: "slack-c1-1", Directory: repo, UseTmux: true,
		Policy: PolicySafe, Prompt: prompt, Stdout: &out, Stderr: &errBuf,
	})
	if err != nil || backend != "tmux" {
		t.Fatalf("claude start-task failed: backend=%s err=%v", backend, err)
	}
	se.waitTmuxReady("claude", "slack-c1-1")
	cRec := se.readRecord("claude", "slack-c1-1")
	if slices.Contains(cRec.Args, "--dangerously-skip-permissions") {
		t.Fatalf("claude start-task must NOT use --dangerously-skip-permissions: %v", cRec.Args)
	}
	if !slices.Contains(cRec.Args, "--permission-mode") || !slices.Contains(cRec.Args, "manual") {
		t.Fatalf("claude start-task must use --permission-mode manual: %v", cRec.Args)
	}
	if cRec.Args[len(cRec.Args)-1] != prompt {
		t.Fatalf("claude multiline prompt mismatch:\ngot:  %q\nwant: %q", cRec.Args[len(cRec.Args)-1], prompt)
	}

	if err := Upgrade("claude", "", &out, &errBuf); err != nil {
		t.Fatalf("claude safe upgrade failed: %v", err)
	}
	se.waitTmuxReady("claude", "slack-c1-1")
	cAfter := se.readRecord("claude", "slack-c1-1")
	if slices.Contains(cAfter.Args, "--dangerously-skip-permissions") || !slices.Contains(cAfter.Args, "manual") {
		t.Fatalf("claude upgrade escalated policy from safe: %v", cAfter.Args)
	}

	// 2. Agy start-task in Rex -> --sandbox --mode plan --prompt-interactive <prompt>, lazygit split
	backend, err = Start(StartOpts{
		Harness: "agy", Name: "slack-c1-2", Directory: repo,
		Policy: PolicySafe, Prompt: prompt, Stdout: &out, Stderr: &errBuf,
	})
	if err != nil || backend != "rex" {
		t.Fatalf("agy start-task Rex failed: backend=%s err=%v", backend, err)
	}
	firstAgyPID := se.waitRexReady("agy", "slack-c1-2", 0)
	aRec := se.readRecord("agy", "slack-c1-2")
	if slices.Contains(aRec.Args, "--dangerously-skip-permissions") {
		t.Fatalf("agy start-task must NOT use --dangerously-skip-permissions: %v", aRec.Args)
	}
	if !slices.Equal(aRec.Args, []string{"--sandbox", "--mode", "plan", "--remote-control", "--prompt-interactive", prompt}) {
		t.Fatalf("unexpected agy safe args: %v", aRec.Args)
	}
	actBytes, _ := os.ReadFile(filepath.Join(rexDir, "actions.json"))
	if !strings.Contains(string(actBytes), "lazygit") {
		t.Fatalf("expected lazygit split in Rex actions: %s", string(actBytes))
	}

	if err := Upgrade("agy", "", &out, &errBuf); err != nil {
		t.Fatalf("agy safe Rex upgrade failed: %v", err)
	}
	_ = se.waitRexReady("agy", "slack-c1-2", firstAgyPID)
	aAfter := se.readRecord("agy", "slack-c1-2")
	if slices.Contains(aAfter.Args, "--dangerously-skip-permissions") || !slices.Contains(aAfter.Args, "--sandbox") || !slices.Contains(aAfter.Args, "plan") {
		t.Fatalf("agy Rex upgrade escalated policy from safe: %v", aAfter.Args)
	}

	// 3. Codex start-task in tmux -> -s workspace-write -a on-request
	if _, err := Start(StartOpts{
		Harness: "codex", Name: "slack-c1-3", Directory: repo, UseTmux: true,
		Policy: PolicySafe, Prompt: prompt, Stdout: &out, Stderr: &errBuf,
	}); err != nil {
		t.Fatal(err)
	}
	se.waitTmuxReady("codex", "slack-c1-3")
	cdxRec := se.readRecord("codex", "slack-c1-3")
	if slices.Contains(cdxRec.Args, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("codex start-task must NOT bypass approvals: %v", cdxRec.Args)
	}
	if !slices.Equal(cdxRec.Args, []string{"-s", "workspace-write", "-a", "on-request", "--", prompt}) {
		t.Fatalf("unexpected codex safe args: %v", cdxRec.Args)
	}

	if _, err := os.Stat("INJECTED"); err == nil {
		t.Fatal("shell injection created INJECTED file")
	}
}

func TestSessionSetupCheckAndHookPreservation(t *testing.T) {
	se := newSessionEnv(t)
	hooksFile := filepath.Join(se.base, ".gemini", "config", "hooks.json")
	_ = os.MkdirAll(filepath.Dir(hooksFile), 0o755)
	_ = os.WriteFile(hooksFile, []byte(`{"fleet":{"SessionStart":[{"type":"command","command":"fleet-hook"}]}}`), 0o644)

	var out, errBuf bytes.Buffer
	if err := Setup(SetupOpts{CheckOnly: true, Stdout: &out, Stderr: &errBuf}); err != nil {
		t.Fatalf("setup --check failed: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "tmux") || !strings.Contains(out.String(), "claude") {
		t.Fatalf("unexpected setup check output:\n%s", out.String())
	}

	out.Reset()
	if err := Setup(SetupOpts{Yes: true, Stdout: &out, Stderr: &errBuf}); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	raw, err := os.ReadFile(hooksFile)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["fleet"] == nil || parsed["agy-session"] == nil {
		t.Fatalf("expected fleet and agy-session hooks preserved: %s", string(raw))
	}
}

func TestStartResolvesRelativeDotDirectoryForRex(t *testing.T) {
	se := newSessionEnv(t)
	rexDir := se.installFakeRex()
	repo := filepath.Join(se.base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	gotDir, err := resolveDirectory(".")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(gotDir) || gotDir != repo {
		t.Fatalf("resolveDirectory(.) = %q, want canonical %q", gotDir, repo)
	}

	var out, errBuf bytes.Buffer
	backend, err := Start(StartOpts{
		Harness: "claude", Name: "ethan", Directory: ".",
		Stdout: &out, Stderr: &errBuf,
	})
	if err != nil || backend != "rex" {
		t.Fatalf("Start in '.' failed: backend=%s err=%v stderr=%s", backend, err, errBuf.String())
	}
	if !strings.Contains(out.String(), "Started Rex session ethan in "+repo) {
		t.Fatalf("expected absolute repo dir in output, got: %q", out.String())
	}

	_ = se.waitRexReady("claude", "ethan", 0)
	rec := se.readRecord("claude", "ethan")
	if !filepath.IsAbs(rec.CWD) || rec.CWD != repo {
		t.Fatalf("launched claude CWD = %q, want %q", rec.CWD, repo)
	}

	sessRaw, err := os.ReadFile(filepath.Join(rexDir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sessions []struct {
		Label string `json:"label"`
		CWD   string `json:"cwd"`
	}
	if err := json.Unmarshal(sessRaw, &sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Label != "claude-ethan" || sessions[0].CWD != repo {
		t.Fatalf("expected rex -c to receive absolute %q, got %+v", repo, sessions)
	}
}
