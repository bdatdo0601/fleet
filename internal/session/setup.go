package session

import (
	"bufio"
	"cmp"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SetupOpts configures Setup.
type SetupOpts struct {
	Harness   string
	CheckOnly bool
	Install   bool
	Yes       bool
	TmuxOnly  bool
	SelfBin   string
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
}

type depCheck struct {
	Name      string
	Required  bool
	Installed bool
	Path      string
	Hint      string
}

func checkDep(name string, required bool, pathEnv, hint string) depCheck {
	p, err := lookPathWithEnv(name, pathEnv)
	return depCheck{
		Name:      name,
		Required:  required,
		Installed: err == nil && p != "",
		Path:      p,
		Hint:      hint,
	}
}

func tmuxHasExtendedKeys(home string) bool {
	for _, cand := range []string{
		filepath.Join(home, ".config", "tmux", "tmux.conf"),
		filepath.Join(home, ".tmux.conf"),
	} {
		if b, err := os.ReadFile(cand); err == nil && strings.Contains(string(b), "extended-keys") {
			return true
		}
	}
	return false
}

func hasCodexHooks(home string) bool {
	b, err := os.ReadFile(filepath.Join(home, ".codex", "hooks.json"))
	return err == nil && strings.Contains(string(b), "agent-hook --harness codex")
}

func legacyShellPresent(home string) []string {
	var found []string
	for _, name := range []string{"claude-session", "agy-session"} {
		if _, err := os.Stat(filepath.Join(home, ".customscripts", name)); err == nil {
			found = append(found, name)
		}
	}
	return found
}

func askConfirm(in io.Reader, out io.Writer, question string, autoYes bool) bool {
	if autoYes {
		return true
	}
	_, _ = fmt.Fprintf(out, "%s [y/N] ", question)
	br := bufio.NewReader(in)
	line, _ := br.ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes"
}

func installBrewFormula(formula string, stdout, stderr io.Writer) error {
	cmd := exec.Command("brew", "install", formula)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func buildDepChecks(targetHarness, pathEnv string) []depCheck {
	checks := []depCheck{
		checkDep("tmux", true, pathEnv, "brew install tmux"),
		checkDep("rex", false, pathEnv, "Install Rex app / CLI (optional; tmux is used when absent or --tmux)"),
		checkDep("lazygit", false, pathEnv, "brew install lazygit (optional diff split in Rex git sessions)"),
	}
	if targetHarness == "" || targetHarness == HarnessClaude {
		checks = append(checks, checkDep("claude", false, pathEnv, "brew install --cask claude-code@latest"))
	}
	if targetHarness == "" || targetHarness == HarnessAgy {
		checks = append(checks, checkDep("agy", false, pathEnv, "brew install --cask antigravity-cli"))
	}
	if targetHarness == "" || targetHarness == HarnessCodex {
		checks = append(checks, checkDep("codex", false, pathEnv, "brew install --cask codex"))
	}
	return checks
}

func printDepChecks(checks []depCheck, targetHarness, home string, stdout io.Writer) []depCheck {
	_, _ = fmt.Fprintln(stdout, "Fleet Session Prerequisites:")
	var missingRequired []depCheck
	for _, c := range checks {
		status := "missing"
		if c.Installed {
			status = c.Path
		} else if c.Required {
			missingRequired = append(missingRequired, c)
		}
		reqTag := "optional"
		if c.Required {
			reqTag = "required"
		}
		_, _ = fmt.Fprintf(stdout, "  %-10s %-10s %s", c.Name, "("+reqTag+")", status)
		if !c.Installed && c.Hint != "" {
			_, _ = fmt.Fprintf(stdout, "  -> %s", c.Hint)
		}
		_, _ = fmt.Fprintln(stdout)
	}

	extKeys := tmuxHasExtendedKeys(home)
	_, _ = fmt.Fprintf(stdout, "  %-21s %v\n", "tmux extended-keys:", extKeys)
	if !extKeys {
		_, _ = fmt.Fprintln(stdout, "    tip: add `set -s extended-keys on` and `set -s extended-keys-format csi-u` to ~/.config/tmux/tmux.conf for Shift-Enter.")
	}
	if targetHarness == "" || targetHarness == HarnessCodex {
		_, _ = fmt.Fprintf(stdout, "  %-21s %v\n", "codex fleet hooks:", hasCodexHooks(home))
	}
	if legacy := legacyShellPresent(home); len(legacy) > 0 {
		_, _ = fmt.Fprintf(stdout, "  %-21s %s (see README cutover guide)\n", "legacy shell scripts:", strings.Join(legacy, ", "))
	}
	return missingRequired
}

func applySetupInstalls(checks []depCheck, targetHarness, pathEnv, home, self string, opts SetupOpts, stdin io.Reader, stdout, stderr io.Writer) error {
	hasBrew := checkDep("brew", false, pathEnv, "").Installed
	for _, c := range checks {
		if c.Installed || !hasBrew {
			continue
		}
		if c.Name == "tmux" || (c.Name == "lazygit" && !opts.TmuxOnly) {
			if opts.Install || askConfirm(stdin, stdout, fmt.Sprintf("Install %s via Homebrew (`brew install %s`)?", c.Name, c.Name), opts.Yes) {
				if err := installBrewFormula(c.Name, stdout, stderr); err != nil {
					return fmt.Errorf("brew install %s: %w", c.Name, err)
				}
			}
		}
	}

	if (targetHarness == "" || targetHarness == HarnessAgy) && checkDep("agy", false, pathEnv, "").Installed {
		if err := EnsureAgyHook(self, ""); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "Configured agy-session hook in %s\n", agyHooksPath(""))
	}
	if (targetHarness == "" || targetHarness == HarnessCodex) && checkDep("codex", false, pathEnv, "").Installed && !hasCodexHooks(home) {
		_, _ = fmt.Fprintln(stdout, "tip: run `fleet mcp install` so Codex reports conversation IDs to `fleet session codex upgrade`.")
	}

	if !checkDep("tmux", true, AugmentedPath(os.Getenv("PATH"), home), "").Installed {
		return fmt.Errorf("tmux is required (`brew install tmux`)")
	}
	if !opts.TmuxOnly && !checkDep("rex", false, pathEnv, "").Installed {
		_, _ = fmt.Fprintln(stdout, "Note: `rex` was not found; sessions will run in tmux automatically.")
	}
	return nil
}

// Setup checks (and optionally installs/configures) tmux, rex, lazygit, and harness prerequisites for `fleet session`.
func Setup(opts SetupOpts) error {
	stdout := cmp.Or(opts.Stdout, io.Writer(os.Stdout))
	stderr := cmp.Or(opts.Stderr, io.Writer(os.Stderr))
	stdin := cmp.Or(opts.Stdin, io.Reader(os.Stdin))
	home, _ := os.UserHomeDir()
	pathEnv := AugmentedPath(os.Getenv("PATH"), home)

	var targetHarness string
	if opts.Harness != "" {
		h, ok := NormalizeHarness(opts.Harness)
		if !ok {
			return fmt.Errorf("unknown harness %q (expected claude, agy, or codex)", opts.Harness)
		}
		targetHarness = h
	}

	checks := buildDepChecks(targetHarness, pathEnv)
	missingRequired := printDepChecks(checks, targetHarness, home, stdout)
	if opts.CheckOnly {
		if len(missingRequired) > 0 {
			return fmt.Errorf("missing required prerequisite: %s", missingRequired[0].Name)
		}
		return nil
	}
	return applySetupInstalls(checks, targetHarness, pathEnv, home, resolveSelfBin(opts.SelfBin), opts, stdin, stdout, stderr)
}
