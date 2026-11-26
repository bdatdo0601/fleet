package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/bdatdo0601/fleet/internal/session"
)

func sessionUsageText() string {
	return `usage: fleet session <setup|claude|agy|codex> [command] [options]

Setup:
  fleet session setup [--check] [--install] [--tmux-only] [--yes]
  fleet session <claude|agy|codex> setup [--check] [--install] [--tmux-only] [--yes]

Session commands (for <claude|agy|codex>, alias antigravity -> agy):
  start [--rex|--tmux] [--model MODEL] [--policy bypass|auto|safe] NAME DIRECTORY
  start-attach [--rex|--tmux] [--model MODEL] [--policy bypass|auto|safe] NAME DIRECTORY
  start-task [--rex|--tmux] [--model MODEL] [--policy safe|auto|bypass] NAME DIRECTORY   (reads multiline prompt from stdin)
  resume [--rex|--tmux] [--model MODEL] [--policy bypass|auto|safe] NAME DIRECTORY SESSION_ID
  attach NAME
  list
  kill NAME
  kill-all
  upgrade

Defaults: agy submits to the Antigravity desktop app (use --rex or --tmux for CLI sessions);
claude and codex default to Rex (with --tmux optional). --model applies to agy app sessions.
`
}

func runSessionSetup(harness string, args []string, cio *IO) error {
	fs := flag.NewFlagSet("session setup", flag.ContinueOnError)
	checkOnly := fs.Bool("check", false, "check prerequisites without installing")
	install := fs.Bool("install", false, "install missing prerequisites without prompting")
	yes := fs.Bool("yes", false, "accept confirmation prompts")
	tmuxOnly := fs.Bool("tmux-only", false, "skip optional Rex/lazygit checks")
	pos, err := parseFlagsInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(pos, " "))
	}
	return session.Setup(session.SetupOpts{
		Harness:   harness,
		CheckOnly: *checkOnly,
		Install:   *install,
		Yes:       *yes,
		TmuxOnly:  *tmuxOnly,
		Stdin:     cio.Stdin,
		Stdout:    cio.Stdout,
		Stderr:    cio.Stderr,
	})
}

type parsedSessionArgs struct {
	useRex     bool
	useTmux    bool
	model      string
	policyRaw  string
	promptFile string
	positional []string
}

func parseSessionActionArgs(args []string) (parsedSessionArgs, error) {
	var out parsedSessionArgs
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			out.positional = append(out.positional, args[i+1:]...)
			return out, nil
		case a == "--rex":
			out.useRex = true
		case a == "--tmux":
			out.useTmux = true
		case a == "--model":
			if i+1 >= len(args) {
				return out, fmt.Errorf("--model requires an argument")
			}
			i++
			out.model = args[i]
		case strings.HasPrefix(a, "--model="):
			out.model = strings.TrimPrefix(a, "--model=")
		case a == "--policy":
			if i+1 >= len(args) {
				return out, fmt.Errorf("--policy requires an argument (bypass|auto|safe)")
			}
			i++
			out.policyRaw = args[i]
		case strings.HasPrefix(a, "--policy="):
			out.policyRaw = strings.TrimPrefix(a, "--policy=")
		case a == "--prompt-file":
			if i+1 >= len(args) {
				return out, fmt.Errorf("--prompt-file requires a path")
			}
			i++
			out.promptFile = args[i]
		case strings.HasPrefix(a, "--prompt-file="):
			out.promptFile = strings.TrimPrefix(a, "--prompt-file=")
		case a == "-h" || a == "--help" || a == "help":
			return out, flag.ErrHelp
		default:
			out.positional = append(out.positional, a)
		}
	}
	if out.useRex && out.useTmux {
		return out, fmt.Errorf("--rex and --tmux cannot be combined")
	}
	return out, nil
}

func runSessionRexRun(harness string, pos []string, policyRaw, promptFile string, cio *IO) error {
	if len(pos) != 1 && len(pos) != 2 {
		return fmt.Errorf("usage: fleet session %s __rex-run NAME [RESUME_ID]", harness)
	}
	pol, err := session.ParsePolicy(policyRaw, session.PolicyBypass)
	if err != nil {
		return err
	}
	resumeID := ""
	if len(pos) == 2 {
		resumeID = pos[1]
	}
	return session.RunRexWrapper(harness, pos[0], resumeID, pol, promptFile, "", cio.Stdout, cio.Stderr)
}

func runSessionExecPrompt(harness string, args []string) error {
	sep := slices.Index(args, "--")
	if sep < 2 {
		return fmt.Errorf("usage: fleet session %s __exec-prompt BIN PROMPT_FILE -- ARGS", harness)
	}
	return session.ExecWithPrompt(harness, args[0], args[1], args[sep+1:])
}

func runSessionStartAction(harness, action string, pos []string, parsed parsedSessionArgs, cio *IO) error {
	switch action {
	case "start", "start-attach":
		if len(pos) != 2 {
			return fmt.Errorf("usage: fleet session %s %s [--rex|--tmux] [--model MODEL] [--policy bypass|auto|safe] NAME DIRECTORY", harness, action)
		}
		pol, pErr := session.ParsePolicy(parsed.policyRaw, session.PolicyBypass)
		if pErr != nil {
			return pErr
		}
		_, err := session.Start(session.StartOpts{
			Harness:   harness,
			Name:      pos[0],
			Directory: pos[1],
			UseRex:    parsed.useRex,
			UseTmux:   parsed.useTmux,
			Attach:    action == "start-attach",
			Model:     parsed.model,
			Policy:    pol,
			Stdout:    cio.Stdout,
			Stderr:    cio.Stderr,
		})
		return err
	case "start-task":
		if len(pos) != 2 {
			return fmt.Errorf("usage: fleet session %s start-task [--rex|--tmux] [--model MODEL] [--policy safe|auto|bypass] NAME DIRECTORY", harness)
		}
		pol, pErr := session.ParsePolicy(parsed.policyRaw, session.PolicySafe)
		if pErr != nil {
			return pErr
		}
		promptBytes, rErr := io.ReadAll(cio.Stdin)
		if rErr != nil {
			return rErr
		}
		prompt := strings.TrimSpace(string(promptBytes))
		if prompt == "" {
			return fmt.Errorf("start-task requires a non-empty prompt on stdin")
		}
		_, err := session.Start(session.StartOpts{
			Harness:   harness,
			Name:      pos[0],
			Directory: pos[1],
			UseRex:    parsed.useRex,
			UseTmux:   parsed.useTmux,
			Model:     parsed.model,
			Policy:    pol,
			Prompt:    prompt,
			Stdout:    cio.Stdout,
			Stderr:    cio.Stderr,
		})
		return err
	case "resume":
		if len(pos) != 3 {
			return fmt.Errorf("usage: fleet session %s resume [--rex|--tmux] [--model MODEL] [--policy bypass|auto|safe] NAME DIRECTORY SESSION_ID", harness)
		}
		pol, pErr := session.ParsePolicy(parsed.policyRaw, session.PolicyBypass)
		if pErr != nil {
			return pErr
		}
		_, err := session.Start(session.StartOpts{
			Harness:   harness,
			Name:      pos[0],
			Directory: pos[1],
			UseRex:    parsed.useRex,
			UseTmux:   parsed.useTmux,
			ResumeID:  pos[2],
			Model:     parsed.model,
			Policy:    pol,
			Stdout:    cio.Stdout,
			Stderr:    cio.Stderr,
		})
		return err
	default:
		return fmt.Errorf("unknown session action %q", action)
	}
}

func dispatchSessionAction(harness, action string, pos []string, parsed parsedSessionArgs, cio *IO) error {
	switch action {
	case "start", "start-attach", "start-task", "resume":
		return runSessionStartAction(harness, action, pos, parsed, cio)
	case "attach":
		if len(pos) != 1 {
			return fmt.Errorf("usage: fleet session %s attach NAME", harness)
		}
		return session.Attach(harness, pos[0], cio.Stderr)
	case "list":
		if len(pos) != 0 {
			return fmt.Errorf("usage: fleet session %s list", harness)
		}
		return session.List(harness, cio.Stdout)
	case "kill":
		if len(pos) != 1 {
			return fmt.Errorf("usage: fleet session %s kill NAME", harness)
		}
		return session.Kill(harness, pos[0], cio.Stdout)
	case "kill-all":
		if len(pos) != 0 {
			return fmt.Errorf("usage: fleet session %s kill-all", harness)
		}
		return session.KillAll(harness, cio.Stdout, cio.Stderr)
	case "upgrade":
		if len(pos) != 0 {
			return fmt.Errorf("usage: fleet session %s upgrade", harness)
		}
		return session.Upgrade(harness, "", cio.Stdout, cio.Stderr)
	case "__rex-run":
		return runSessionRexRun(harness, pos, parsed.policyRaw, parsed.promptFile, cio)
	case "__hook":
		if len(pos) != 0 {
			return fmt.Errorf("usage: fleet session %s __hook", harness)
		}
		return session.HandleHook(harness, cio.Stdin, cio.Stdout)
	default:
		return fmt.Errorf("unknown session action %q\n\n%s", action, sessionUsageText())
	}
}

func cmdSession(args []string, cio *IO) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		_, _ = io.WriteString(cio.Stdout, sessionUsageText())
		return nil
	}
	if args[0] == "setup" {
		return runSessionSetup("", args[1:], cio)
	}
	harness, ok := session.NormalizeHarness(args[0])
	if !ok {
		return fmt.Errorf("unknown session target %q\n\n%s", args[0], sessionUsageText())
	}

	rest := args[1:]
	if len(rest) > 0 && rest[0] == "setup" {
		return runSessionSetup(harness, rest[1:], cio)
	}
	if len(rest) > 0 && rest[0] == "__exec-prompt" {
		return runSessionExecPrompt(harness, rest[1:])
	}

	parsed, err := parseSessionActionArgs(rest)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.WriteString(cio.Stdout, sessionUsageText())
			return nil
		}
		return err
	}
	action := "list"
	pos := parsed.positional
	if len(pos) > 0 {
		action = pos[0]
		pos = pos[1:]
	}

	if (parsed.useRex || parsed.useTmux || parsed.model != "") && action != "start" && action != "start-attach" && action != "start-task" && action != "resume" {
		return fmt.Errorf("--rex, --tmux, and --model are only valid with start, start-attach, start-task, or resume\n\n%s", sessionUsageText())
	}
	return dispatchSessionAction(harness, action, pos, parsed, cio)
}
