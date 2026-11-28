package session

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	t.Setenv("FLEET_AGY_APP_DATA_DIR", filepath.Join(base, ".gemini", "antigravity"))
	t.Setenv("FLEET_SESSION_NO_OPEN", "1")
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
	if !slices.Contains(rec.Args, "--permission-mode") || !slices.Contains(rec.Args, "auto") || slices.Contains(rec.Args, "--dangerously-skip-permissions") || !slices.Contains(rec.Args, "--remote-control") {
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

	// Agy start (explicit --tmux) + Codex start
	if _, err := Start(StartOpts{Harness: "agy", Name: "beta", Directory: specialDir, UseTmux: true, Stdout: &out, Stderr: &errBuf}); err != nil {
		t.Fatal(err)
	}
	se.waitTmuxReady("agy", "beta")
	agyRec := se.readRecord("agy", "beta")
	if !slices.Equal(agyRec.Args, []string{"--mode", "auto", "--remote-control"}) {
		t.Fatalf("unexpected agy args: %v", agyRec.Args)
	}

	if _, err := Start(StartOpts{Harness: "codex", Name: "gamma", Directory: specialDir, Stdout: &out, Stderr: &errBuf}); err != nil {
		t.Fatal(err)
	}
	se.waitTmuxReady("codex", "gamma")
	cdxRec := se.readRecord("codex", "gamma")
	if !slices.Equal(cdxRec.Args, []string{"--approve-for-me"}) {
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

	// 2. Agy start-task in Rex (--rex) -> --sandbox --mode plan --prompt-interactive <prompt>, lazygit split
	backend, err = Start(StartOpts{
		Harness: "agy", Name: "slack-c1-2", Directory: repo, UseRex: true,
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

func TestAgyDefaultAppBackendLifecycle(t *testing.T) {
	se := newSessionEnv(t)
	_ = se.installFakeRex() // Ensure agy defaults to app even when rex is installed.

	repo := filepath.Join(se.base, "repo-with-project")
	unmapped := filepath.Join(se.base, "repo-unmapped")
	for _, d := range []string{repo, unmapped} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	appDir := filepath.Join(se.base, ".gemini", "antigravity")
	daemonDir := filepath.Join(appDir, "daemon")
	if err := os.MkdirAll(daemonDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Seed conversation_summaries.db mapping repo -> proj-test-123.
	dbPath := filepath.Join(appDir, "conversation_summaries.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE conversation_summaries (
			conversation_id TEXT PRIMARY KEY,
			last_modified_time TEXT NOT NULL,
			workspace_uris TEXT NOT NULL,
			project_id TEXT NOT NULL
		);
		INSERT INTO conversation_summaries (conversation_id, last_modified_time, workspace_uris, project_id)
		VALUES ('conv-old', '2026-11-25T10:00:00Z', ?, 'proj-test-123');
	`, fmt.Sprintf(`[%q]`, "file://"+repo)); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	var out, errBuf bytes.Buffer
	// 8. Before any live hub exists, Start fails with clear error mentioning --rex / --tmux.
	if _, err := Start(StartOpts{Harness: "agy", Name: "nohub", Directory: repo, Stdout: &out, Stderr: &errBuf}); err == nil || !strings.Contains(err.Error(), "--rex / --tmux") {
		t.Fatalf("expected missing hub error mentioning --rex / --tmux, got %v", err)
	}
	// 9. --rex and --tmux together are rejected.
	if _, err := Start(StartOpts{Harness: "agy", Name: "badflags", Directory: repo, UseRex: true, UseTmux: true, Stdout: &out, Stderr: &errBuf}); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("expected --rex/--tmux conflict error, got %v", err)
	}

	// Start mock Antigravity hub.
	const testCSRF = "csrf-secret-123"
	var (
		mu             sync.Mutex
		createRequests []map[string]any
		archivedCalls  []map[string]any
		convs          = map[string]agyConversation{}
		nextSeq        = 0
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-antigravity-csrf-token") != testCSRF || r.Header.Get("Connect-Protocol-Version") != "1" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"permission_denied","message":"bad headers"}`))
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()

		switch r.URL.Path {
		case "/product.v1.ConversationService/CreateConversation":
			createRequests = append(createRequests, body)
			nextSeq++
			id := fmt.Sprintf("11111111-1111-4111-8111-%012d", nextSeq)
			var c agyConversation
			c.ID = id
			if pid, ok := body["projectId"].(string); ok {
				c.Metadata.ProjectID = pid
			}
			if title, ok := body["title"].(string); ok {
				c.Annotations.Title = title
			}
			convs[id] = c
			_ = json.NewEncoder(w).Encode(map[string]any{"conversation": c})
		case "/product.v1.ConversationService/GetConversation":
			id, _ := body["conversationId"].(string)
			c, ok := convs[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"code": "not_found", "message": "not found"})
				return
			}
			_ = json.NewEncoder(w).Encode(c)
		case "/product.v1.ConversationService/UpdateConversation":
			id, _ := body["conversationId"].(string)
			c, ok := convs[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"code": "not_found", "message": "not found"})
				return
			}
			if ann, ok := body["annotations"].(map[string]any); ok {
				if arch, ok := ann["archived"].(bool); ok {
					c.Annotations.Archived = arch
				}
			}
			convs[id] = c
			archivedCalls = append(archivedCalls, body)
			_ = json.NewEncoder(w).Encode(c)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	hubBytes, _ := json.Marshal(agyHubInfo{
		PID:       os.Getpid(),
		HTTPPort:  port,
		HTTPSPort: port,
		LSVersion: "2.35.2",
		CSRFToken: testCSRF,
	})
	if err := os.WriteFile(filepath.Join(daemonDir, fmt.Sprintf("hub_%d.json", port)), hubBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	origOpen := openAgyAppFn
	openCount := 0
	openAgyAppFn = func() error {
		openCount++
		return nil
	}
	t.Cleanup(func() { openAgyAppFn = origOpen })

	// 1a. Start with mapped project (empty prompt).
	out.Reset()
	errBuf.Reset()
	backend, err := Start(StartOpts{Harness: "agy", Name: "app-idle", Directory: repo, Model: "pro", Stdout: &out, Stderr: &errBuf})
	if err != nil || backend != "app" {
		t.Fatalf("agy Start default failed: backend=%s err=%v stderr=%s", backend, err, errBuf.String())
	}

	// 1b. Start-task with mapped project + multiline prompt.
	out.Reset()
	errBuf.Reset()
	multiline := "Line 1\nLine 2: fix tests"
	backend, err = Start(StartOpts{Harness: "agy", Name: "app-task", Directory: repo, Prompt: multiline, Stdout: &out, Stderr: &errBuf})
	if err != nil || backend != "app" {
		t.Fatalf("agy start-task default failed: backend=%s err=%v stderr=%s", backend, err, errBuf.String())
	}

	// 2. Start-task in unmapped directory -> omits projectId, prepends working dir, warns on stderr.
	out.Reset()
	errBuf.Reset()
	backend, err = Start(StartOpts{Harness: "agy", Name: "app-unmapped", Directory: unmapped, Prompt: "Investigate", Stdout: &out, Stderr: &errBuf})
	if err != nil || backend != "app" {
		t.Fatalf("agy unmapped Start failed: backend=%s err=%v", backend, err)
	}
	if !strings.Contains(errBuf.String(), "no existing Antigravity project found") {
		t.Fatalf("expected stderr warning for unmapped dir, got %q", errBuf.String())
	}

	mu.Lock()
	if len(createRequests) != 3 {
		mu.Unlock()
		t.Fatalf("expected 3 CreateConversation requests, got %d", len(createRequests))
	}
	r0, r1, r2 := createRequests[0], createRequests[1], createRequests[2]
	mu.Unlock()

	if r0["projectId"] != "proj-test-123" || r0["title"] != "app-idle" || r0["model"] != "pro" || r0["prompt"] != nil {
		t.Fatalf("unexpected r0 payload: %+v", r0)
	}
	if tags, ok := r0["tags"].([]any); !ok || len(tags) != 2 || tags[0] != "fleet" || tags[1] != "fleet-session:app-idle" {
		t.Fatalf("unexpected r0 tags: %+v", r0["tags"])
	}
	if p1, ok := r1["prompt"].([]any); !ok || len(p1) != 1 || p1[0].(map[string]any)["text"] != multiline {
		t.Fatalf("unexpected r1 prompt: %+v", r1["prompt"])
	}
	if _, hasProj := r2["projectId"]; hasProj {
		t.Fatalf("expected unmapped request to omit projectId, got %+v", r2)
	}
	if p2, ok := r2["prompt"].([]any); !ok || !strings.HasPrefix(p2[0].(map[string]any)["text"].(string), "Working directory: "+unmapped+"\n\nInvestigate") {
		t.Fatalf("expected working directory prepended in r2 prompt, got %+v", r2["prompt"])
	}

	// 3. Duplicate rejection.
	if _, err := Start(StartOpts{Harness: "agy", Name: "app-task", Directory: repo, Stdout: &out, Stderr: &errBuf}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected duplicate rejection, got %v", err)
	}

	// 4. SessionExists -> true, "app".
	if exists, gotBackend := SessionExists("agy", "app-task"); !exists || gotBackend != "app" {
		t.Fatalf("SessionExists = (%v, %q), want (true, %q)", exists, gotBackend, "app")
	}

	// 5. List prints "<name> (app)".
	out.Reset()
	if err := List("agy", &out); err != nil {
		t.Fatalf("List agy failed: %v", err)
	}
	if !strings.Contains(out.String(), "app-task (app)") || !strings.Contains(out.String(), "app-idle (app)") {
		t.Fatalf("expected app sessions in List output, got:\n%s", out.String())
	}

	// 6. Attach invokes openAgyAppFn.
	errBuf.Reset()
	if err := Attach("agy", "app-task", &errBuf); err != nil {
		t.Fatalf("Attach agy app failed: %v", err)
	}
	if openCount != 1 {
		t.Fatalf("expected openAgyAppFn called once, got %d", openCount)
	}

	// 7. Kill archives conversation and removes local state.
	out.Reset()
	if err := Kill("agy", "app-task", &out); err != nil {
		t.Fatalf("Kill agy app failed: %v", err)
	}
	if exists, _ := SessionExists("agy", "app-task"); exists {
		t.Fatal("expected app-task to be inactive after Kill")
	}
	mu.Lock()
	if len(archivedCalls) != 1 || archivedCalls[0]["annotations"].(map[string]any)["archived"] != true {
		mu.Unlock()
		t.Fatalf("expected UpdateConversation archived=true, got %+v", archivedCalls)
	}
	mu.Unlock()
}

func TestEnsureCodexRemoteControlWarnsOnConflict(t *testing.T) {
	se := newSessionEnv(t)
	codexBin := se.writeExec("codex", `#!/bin/sh
echo "Error: Remote control is enabled on host but the connection is errored." >&2
exit 1
`)
	var errBuf bytes.Buffer
	ensureCodexRemoteControl(codexBin, &errBuf)
	got := errBuf.String()
	if !strings.Contains(got, "remote-control start failed") || !strings.Contains(got, "Codex Desktop") {
		t.Fatalf("expected conflict warning mentioning Codex Desktop, got %q", got)
	}

	t.Setenv("FLEET_CODEX_SKIP_REMOTE_CONTROL", "1")
	errBuf.Reset()
	ensureCodexRemoteControl(codexBin, &errBuf)
	if errBuf.Len() != 0 {
		t.Fatalf("expected skip env var to suppress invocation, got %q", errBuf.String())
	}
}

func TestAutoAndBypassPoliciesAcrossHarnesses(t *testing.T) {
	if pol, err := ParsePolicy("", PolicyAuto); err != nil || pol != PolicyAuto {
		t.Fatalf("ParsePolicy('') = (%q, %v)", pol, err)
	}
	if pol, err := ParsePolicy("bypass", PolicyAuto); err != nil || pol != PolicyBypass {
		t.Fatalf("ParsePolicy(bypass) = (%q, %v)", pol, err)
	}
	if _, err := ParsePolicy("bogus", PolicyAuto); err == nil {
		t.Fatal("expected ParsePolicy(bogus) to fail")
	}

	cSpec, _ := GetSpec(HarnessClaude)
	cAuto, err := BuildHarnessArgs(cSpec, "s1", "/bin/fleet", "", "fix", "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cAuto, "--permission-mode") || !slices.Contains(cAuto, "auto") || slices.Contains(cAuto, "--dangerously-skip-permissions") {
		t.Fatalf("unexpected claude default auto args: %v", cAuto)
	}
	cBypass, err := BuildHarnessArgs(cSpec, "s1", "/bin/fleet", "", "fix", PolicyBypass)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cBypass, "--dangerously-skip-permissions") || slices.Contains(cBypass, "--permission-mode") {
		t.Fatalf("unexpected claude bypass args: %v", cBypass)
	}

	aSpec, _ := GetSpec(HarnessAgy)
	aAuto, err := BuildHarnessArgs(aSpec, "s2", "/bin/fleet", "", "fix", "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(aAuto, []string{"--mode", "auto", "--remote-control", "--prompt-interactive", "fix"}) {
		t.Fatalf("unexpected agy default auto args: %v", aAuto)
	}
	aBypass, err := BuildHarnessArgs(aSpec, "s2", "/bin/fleet", "", "fix", PolicyBypass)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(aBypass, []string{"--dangerously-skip-permissions", "--remote-control", "--prompt-interactive", "fix"}) {
		t.Fatalf("unexpected agy bypass args: %v", aBypass)
	}

	xSpec, _ := GetSpec(HarnessCodex)
	xAuto, err := BuildHarnessArgs(xSpec, "s3", "/bin/fleet", "", "fix", "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(xAuto, []string{"--approve-for-me", "--", "fix"}) {
		t.Fatalf("unexpected codex default auto args: %v", xAuto)
	}
	xBypass, err := BuildHarnessArgs(xSpec, "s3", "/bin/fleet", "", "fix", PolicyBypass)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(xBypass, []string{"--dangerously-bypass-approvals-and-sandbox", "--", "fix"}) {
		t.Fatalf("unexpected codex bypass args: %v", xBypass)
	}
}

func TestKillAgentArchivesAgyAppByRawID(t *testing.T) {
	se := newSessionEnv(t)
	daemonDir := filepath.Join(se.base, ".gemini", "antigravity", "daemon")
	if err := os.MkdirAll(daemonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const convID = "33333333-3333-4333-8333-000000000001"
	archived := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/product.v1.ConversationService/UpdateConversation" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["conversationId"] == convID && body["annotations"].(map[string]any)["archived"] == true {
				archived = true
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": convID})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	hubBytes, _ := json.Marshal(agyHubInfo{PID: os.Getpid(), HTTPPort: port, CSRFToken: "kill-csrf"})
	_ = os.WriteFile(filepath.Join(daemonDir, fmt.Sprintf("hub_%d.json", port)), hubBytes, 0o600)

	var out, errBuf bytes.Buffer
	if err := KillAgent(KillTarget{Harness: "antigravity", FullRawID: convID}, &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	if !archived || !strings.Contains(out.String(), convID) {
		t.Fatalf("expected KillAgent to archive %s, archived=%v out=%q", convID, archived, out.String())
	}
}

func TestEnsureDirectoryTrusted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")

	targetDir := filepath.Join(home, "workspace", "demo")

	// 1. Claude: creates ~/.claude.json, preserves existing keys, idempotent, rejects malformed JSON.
	claudePath := filepath.Join(home, ".claude.json")
	seedClaude := map[string]any{
		"oauthAccount": map[string]any{"email": "dev@example.com"},
		"projects": map[string]any{
			"/existing/repo": map[string]any{"hasTrustDialogAccepted": true, "allowedTools": []string{"Read"}},
		},
	}
	seedBytes, _ := json.MarshalIndent(seedClaude, "", "  ")
	if err := os.WriteFile(claudePath, seedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDirectoryTrusted("claude", targetDir); err != nil {
		t.Fatalf("EnsureDirectoryTrusted(claude) failed: %v", err)
	}
	// Repeat call is a clean no-op.
	if err := EnsureDirectoryTrusted("claude", targetDir); err != nil {
		t.Fatalf("repeat EnsureDirectoryTrusted(claude) failed: %v", err)
	}
	rawClaude, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	var gotClaude map[string]any
	if err := json.Unmarshal(rawClaude, &gotClaude); err != nil {
		t.Fatal(err)
	}
	if gotClaude["oauthAccount"].(map[string]any)["email"] != "dev@example.com" {
		t.Fatalf("expected oauthAccount preserved, got %+v", gotClaude)
	}
	projects := gotClaude["projects"].(map[string]any)
	if projects[targetDir].(map[string]any)["hasTrustDialogAccepted"] != true {
		t.Fatalf("expected %s trusted in .claude.json, got %+v", targetDir, projects)
	}
	if len(projects["/existing/repo"].(map[string]any)["allowedTools"].([]any)) != 1 {
		t.Fatalf("expected existing project keys preserved, got %+v", projects["/existing/repo"])
	}

	// Custom CLAUDE_CONFIG_DIR creates file when missing, and malformed JSON returns error without overwriting.
	customClaudeDir := filepath.Join(home, "custom-claude")
	t.Setenv("CLAUDE_CONFIG_DIR", customClaudeDir)
	if err := EnsureDirectoryTrusted("claude", targetDir); err != nil {
		t.Fatalf("EnsureDirectoryTrusted(claude custom dir) failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(customClaudeDir, ".claude.json")); err != nil {
		t.Fatalf("expected custom .claude.json created: %v", err)
	}
	if err := os.WriteFile(filepath.Join(customClaudeDir, ".claude.json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDirectoryTrusted("claude", targetDir); err == nil {
		t.Fatalf("expected error on malformed .claude.json")
	}
	if corrupt, _ := os.ReadFile(filepath.Join(customClaudeDir, ".claude.json")); string(corrupt) != "{not-json" {
		t.Fatalf("malformed .claude.json should not be overwritten, got %q", string(corrupt))
	}

	// 2. Codex: appends project trust section, preserves hooks/other sections, upgrades untrusted -> trusted.
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	codexPath := filepath.Join(codexDir, "config.toml")
	untrustedDir := filepath.Join(home, "workspace", "untrusted")
	initialTOML := fmt.Sprintf(`[hooks.state."/tmp/hooks.json:session_start:0:0"]
enabled = true
trusted_hash = "sha256:abc"

[projects.%q]
trust_level = "untrusted"

[projects."/other/repo"]
trust_level = "trusted"
`, untrustedDir)
	if err := os.WriteFile(codexPath, []byte(initialTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDirectoryTrusted("codex", targetDir); err != nil {
		t.Fatalf("EnsureDirectoryTrusted(codex append) failed: %v", err)
	}
	if err := EnsureDirectoryTrusted("codex", untrustedDir); err != nil {
		t.Fatalf("EnsureDirectoryTrusted(codex upgrade) failed: %v", err)
	}
	// Idempotent repeat call.
	if err := EnsureDirectoryTrusted("codex", targetDir); err != nil {
		t.Fatalf("EnsureDirectoryTrusted(codex repeat) failed: %v", err)
	}
	gotTOMLBytes, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	gotTOML := string(gotTOMLBytes)
	if !strings.Contains(gotTOML, `trusted_hash = "sha256:abc"`) ||
		!strings.Contains(gotTOML, fmt.Sprintf("[projects.%q]\ntrust_level = \"trusted\"", targetDir)) ||
		!strings.Contains(gotTOML, fmt.Sprintf("[projects.%q]\ntrust_level = \"trusted\"", untrustedDir)) ||
		strings.Contains(gotTOML, `trust_level = "untrusted"`) {
		t.Fatalf("unexpected codex config.toml:\n%s", gotTOML)
	}

	// Custom CODEX_HOME creates config.toml from scratch.
	customCodexHome := filepath.Join(home, "custom-codex")
	t.Setenv("CODEX_HOME", customCodexHome)
	if err := EnsureDirectoryTrusted("codex", targetDir); err != nil {
		t.Fatalf("EnsureDirectoryTrusted(codex custom home) failed: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(customCodexHome, "config.toml")); err != nil || !strings.Contains(string(raw), `trust_level = "trusted"`) {
		t.Fatalf("expected custom codex config.toml created, got %q (%v)", string(raw), err)
	}

	// 3. Agy is a no-op.
	if err := EnsureDirectoryTrusted("agy", targetDir); err != nil {
		t.Fatalf("expected agy no-op, got %v", err)
	}
}
