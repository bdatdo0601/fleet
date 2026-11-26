// Package session provides native Go session management for claude, agy, and codex across Rex and tmux.
package session

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/bdatdo0601/fleet/internal/paths"
)

// Harness identifiers for managed terminal sessions.
const (
	HarnessClaude = "claude"
	HarnessAgy    = "agy"
	HarnessCodex  = "codex"
)

// Policy controls approval/sandbox flags when launching or resuming a session.
type Policy string

const (
	PolicyBypass Policy = "bypass"
	PolicySafe   Policy = "safe"
)

// Spec defines per-harness constants and launch rules.
type Spec struct {
	Name               string
	DisplayName        string
	Prefix             string
	Binary             string
	PaneOption         string
	ExitOption         string
	ManagedOption      string
	RequiresTranscript bool
	UsesExternalHook   bool
}

var (
	nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)
	uuidRE = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)
	agyRE  = regexp.MustCompile(`(^|[\s/])(agy|antigravity)([\s]|$)`)
	cdxRE  = regexp.MustCompile(`(^|[\s/])codex([\s]|$)`)
)

// NormalizeHarness maps CLI harness names/aliases to canonical session harness names.
func NormalizeHarness(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "claude":
		return HarnessClaude, true
	case "agy", "antigravity":
		return HarnessAgy, true
	case "codex":
		return HarnessCodex, true
	default:
		return "", false
	}
}

// GetSpec returns the Spec for a canonical or aliased harness name.
func GetSpec(raw string) (Spec, error) {
	h, ok := NormalizeHarness(raw)
	if !ok {
		return Spec{}, fmt.Errorf("unknown harness %q (expected claude, agy, or codex)", raw)
	}
	switch h {
	case HarnessClaude:
		return Spec{
			Name:               HarnessClaude,
			DisplayName:        "Claude",
			Prefix:             "claude-",
			Binary:             "claude",
			PaneOption:         "@claude_session",
			ExitOption:         "@claude_exit_option",
			ManagedOption:      "@claude_managed",
			RequiresTranscript: true,
		}, nil
	case HarnessAgy:
		return Spec{
			Name:             HarnessAgy,
			DisplayName:      "agy",
			Prefix:           "agy-",
			Binary:           "agy",
			PaneOption:       "@agy_session",
			ExitOption:       "@agy_exit_option",
			ManagedOption:    "@agy_managed",
			UsesExternalHook: true,
		}, nil
	default:
		return Spec{
			Name:             HarnessCodex,
			DisplayName:      "Codex",
			Prefix:           "codex-",
			Binary:           "codex",
			PaneOption:       "@codex_session",
			ExitOption:       "@codex_exit_option",
			ManagedOption:    "@codex_managed",
			UsesExternalHook: true,
		}, nil
	}
}

// ParsePolicy validates and normalizes a session policy string.
func ParsePolicy(raw string, def Policy) (Policy, error) {
	p := strings.ToLower(strings.TrimSpace(raw))
	if p == "" {
		return def, nil
	}
	switch Policy(p) {
	case PolicyBypass, PolicySafe:
		return Policy(p), nil
	default:
		return "", fmt.Errorf("invalid policy %q (expected safe or bypass)", raw)
	}
}

// ValidateName checks that name is safe for tmux/Rex session naming.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return errors.New("invalid name; use letters, digits, underscores and hyphens, starting with a letter or digit")
	}
	return nil
}

// ValidateUUID checks that id is a valid UUID conversation/session identifier.
func ValidateUUID(id string) error {
	if !uuidRE.MatchString(id) {
		return errors.New("invalid conversation ID; expected a UUID")
	}
	return nil
}

// MatchesProcess reports whether a process command line belongs to the harness binary itself.
func (s Spec) MatchesProcess(cmdLine string) bool {
	trimmed := strings.TrimSpace(cmdLine)
	if trimmed == "" || strings.Contains(trimmed, "__rex-run") || strings.Contains(trimmed, "__exec-prompt") {
		return false
	}
	switch s.Name {
	case HarnessClaude:
		return strings.Contains(trimmed, "claude") && !strings.Contains(trimmed, "claude-session")
	case HarnessAgy:
		return agyRE.MatchString(trimmed) && !strings.Contains(trimmed, "agy-session")
	case HarnessCodex:
		return cdxRE.MatchString(trimmed) && !strings.Contains(trimmed, "codex-session")
	default:
		return false
	}
}

// AugmentedPath returns basePath with standard user/Homebrew bin directories appended if present.
func AugmentedPath(basePath, home string) string {
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	parts := []string{}
	if basePath != "" {
		parts = append(parts, strings.Split(basePath, string(os.PathListSeparator))...)
	}
	cands := []string{
		filepath.Join(home, ".local", "bin"),
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/home/linuxbrew/.linuxbrew/bin",
	}
	for _, dir := range cands {
		if dir == "" || slices.Contains(parts, dir) {
			continue
		}
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			parts = append(parts, dir)
		}
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

func lookPathWithEnv(name, pathEnv string) (string, error) {
	if strings.Contains(name, string(os.PathSeparator)) {
		return filepath.Abs(name)
	}
	for dir := range strings.SplitSeq(pathEnv, string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		cand := filepath.Join(dir, name)
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return filepath.Abs(cand)
		}
	}
	return exec.LookPath(name)
}

func resolveSelfBin(override string) string {
	if override != "" {
		if abs, err := filepath.Abs(override); err == nil {
			return abs
		}
		return override
	}
	if v := strings.TrimSpace(os.Getenv("FLEET_SESSION_SELF_BIN")); v != "" {
		return v
	}
	if exe, err := os.Executable(); err == nil && exe != "" {
		if abs, aErr := filepath.Abs(exe); aErr == nil {
			return abs
		}
		return exe
	}
	return paths.LauncherPath()
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func stateRootFor(harness string) string {
	// Respect legacy XDG_STATE_HOME/<harness>-session lock/rex layout if FLEET_SESSION_LEGACY_STATE=1,
	// otherwise use paths.SessionStateDir(harness).
	if os.Getenv("FLEET_SESSION_LEGACY_STATE") == "1" {
		return filepath.Join(paths.LocalStateRoot(), harness+"-session")
	}
	return paths.SessionStateDir(harness)
}

func upgradeLockPath(harness string) string {
	return filepath.Join(stateRootFor(harness), "upgrade.lock")
}

func rexStateDir(harness string) string {
	return filepath.Join(stateRootFor(harness), "rex")
}

func resumeGraceTenths() int {
	raw := strings.TrimSpace(os.Getenv("AGY_SESSION_RESUME_GRACE_TENTHS"))
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("FLEET_SESSION_RESUME_GRACE_TENTHS"))
	}
	if raw == "" {
		return 10
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 10
	}
	return n
}

func buildClaudeSettingsJSON(selfBin string) (string, error) {
	hookCmd := shellQuote(selfBin) + " session claude __hook"
	payload := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": hookCmd,
						},
					},
				},
			},
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// BuildHarnessArgs constructs exact CLI arguments for launching or resuming a harness process.
func BuildHarnessArgs(spec Spec, name, selfBin, resumeID, prompt string, policy Policy) ([]string, error) {
	pol := cmp.Or(policy, PolicyBypass)
	switch spec.Name {
	case HarnessClaude:
		settingsJSON, err := buildClaudeSettingsJSON(selfBin)
		if err != nil {
			return nil, err
		}
		var args []string
		if pol == PolicySafe {
			args = append(args, "--permission-mode", "manual")
		} else {
			args = append(args, "--dangerously-skip-permissions")
		}
		args = append(args, "--remote-control", name, "--settings", settingsJSON)
		if resumeID != "" {
			args = append(args, "--resume", resumeID)
		}
		if prompt != "" {
			args = append(args, "--", prompt)
		}
		return args, nil

	case HarnessAgy:
		var args []string
		if pol == PolicySafe {
			args = append(args, "--sandbox", "--mode", "plan")
		} else {
			args = append(args, "--dangerously-skip-permissions")
		}
		args = append(args, "--remote-control")
		if resumeID != "" {
			args = append(args, "--conversation", resumeID)
		}
		if prompt != "" {
			args = append(args, "--prompt-interactive", prompt)
		}
		return args, nil

	case HarnessCodex:
		var args []string
		if resumeID != "" {
			args = append(args, "resume")
		}
		if pol == PolicySafe {
			args = append(args, "-s", "workspace-write", "-a", "on-request")
		} else {
			args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		}
		if resumeID != "" {
			args = append(args, resumeID)
		}
		if prompt != "" {
			if resumeID != "" {
				args = append(args, prompt)
			} else {
				args = append(args, "--", prompt)
			}
		}
		return args, nil

	default:
		return nil, fmt.Errorf("unsupported harness %q", spec.Name)
	}
}
