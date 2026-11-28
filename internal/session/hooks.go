package session

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bdatdo0601/fleet/internal/paths"
)

const maxHookPayloadBytes = 1 << 20

func agyHooksPath(override string) string {
	if override != "" {
		return override
	}
	if v := strings.TrimSpace(os.Getenv("AGY_HOOKS_FILE")); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gemini", "config", "hooks.json")
}

// EnsureAgyHook registers/updates the `agy-session` hook entry in ~/.gemini/config/hooks.json without disturbing existing keys.
func EnsureAgyHook(selfBin, hooksFile string) error {
	path := agyHooksPath(hooksFile)
	hookCmd := shellQuote(resolveSelfBin(selfBin)) + " session agy __hook"
	entry := map[string]any{
		"enabled": true,
		"SessionStart": []any{
			map[string]any{"type": "command", "command": hookCmd, "timeout": 5},
		},
		"PreInvocation": []any{
			map[string]any{"type": "command", "command": hookCmd, "timeout": 5},
		},
	}

	root := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &root); err != nil || root == nil {
			return fmt.Errorf("invalid JSON object in %s; fix it before launching agy", path)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}

	entryBytes, _ := json.Marshal(entry)
	if existing, ok := root["agy-session"]; ok {
		if exBytes, mErr := json.Marshal(existing); mErr == nil && bytes.Equal(exBytes, entryBytes) {
			return nil
		}
	}
	root["agy-session"] = entry
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return err
	}
	_, err := paths.WriteFileAtomic(path, buf.Bytes(), 0o644, nil, nil)
	return err
}

func readTrackedStateFile(path string) (trackedState, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return trackedState{}, false
	}
	return parseTrackedState(string(raw))
}

func writeTrackedStateFile(path string, st trackedState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = paths.WriteFileAtomic(path, append(b, '\n'), 0o644, nil, nil)
	return err
}

type rawHookPayload struct {
	AgentID              string   `json:"agent_id"`
	AgentIDCamel         string   `json:"agentId"`
	ParentConversationID string   `json:"parentConversationId"`
	ParentConvSnake      string   `json:"parent_conversation_id"`
	IsSubagent           bool     `json:"isSubagent"`
	SessionID            string   `json:"session_id"`
	ConversationID       string   `json:"conversationId"`
	CWD                  string   `json:"cwd"`
	WorkspacePaths       []string `json:"workspacePaths"`
	TranscriptPath       string   `json:"transcript_path"`
	TranscriptCamel      string   `json:"transcriptPath"`
}

func (p rawHookPayload) isSubagent() bool {
	return p.AgentID != "" || p.AgentIDCamel != "" || p.ParentConversationID != "" || p.ParentConvSnake != "" || p.IsSubagent
}

// HandleHook implements `fleet session <harness> __hook`, reading hook JSON from stdin and always writing `{}` on stdout.
func HandleHook(harness string, stdin io.Reader, stdout io.Writer) error {
	stdout = cmp.Or(stdout, io.Discard)
	defer func() {
		_, _ = io.WriteString(stdout, "{}\n")
	}()
	spec, err := GetSpec(harness)
	if err != nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, maxHookPayloadBytes))
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var payload rawHookPayload
	if json.Unmarshal(raw, &payload) != nil || payload.isSubagent() {
		return nil
	}
	id := cmp.Or(payload.ConversationID, payload.SessionID)
	var ws0 string
	if len(payload.WorkspacePaths) > 0 {
		ws0 = payload.WorkspacePaths[0]
	}
	dir := cmp.Or(payload.CWD, ws0)
	transcript := cmp.Or(payload.TranscriptCamel, payload.TranscriptPath)
	if id == "" || dir == "" {
		return nil
	}
	RecordHookState(spec.Name, id, dir, transcript)
	return nil
}

// RecordHookState updates tmux pane or Rex state tracking when a session was started via `fleet session`.
func RecordHookState(harness, rawID, cwd, transcript string) {
	if rawID == "" || cwd == "" {
		return
	}
	spec, err := GetSpec(harness)
	if err != nil {
		return
	}
	target := cmp.Or(os.Getenv("FLEET_SESSION_TARGET"), os.Getenv("AGY_SESSION_TARGET"))
	if target == "" {
		// Claude's --settings hook only runs inside fleet-managed tmux panes.
		if spec.Name == HarnessClaude && os.Getenv("TMUX_PANE") != "" {
			target = "tmux"
		} else {
			return
		}
	}
	policy := cmp.Or(os.Getenv("FLEET_SESSION_POLICY"), string(PolicyAuto))

	switch target {
	case "tmux":
		pane := os.Getenv("TMUX_PANE")
		if pane == "" {
			return
		}
		if expectedName := cmp.Or(os.Getenv("FLEET_SESSION_NAME"), os.Getenv("AGY_SESSION_NAME")); expectedName != "" {
			sessName, err := tmuxDisplay(pane, "#{session_name}")
			if err != nil || sessName != spec.Prefix+expectedName {
				return
			}
		}
		panePID, err := tmuxDisplay(pane, "#{pane_pid}")
		if err != nil || panePID == "" {
			return
		}
		st := trackedState{
			ID:         rawID,
			Directory:  cwd,
			Transcript: transcript,
			PID:        panePID,
			Policy:     policy,
		}
		b, err := json.Marshal(st)
		if err != nil {
			return
		}
		_ = tmuxSetPaneOption(pane, spec.PaneOption, string(b))

	case "rex":
		stateFile := cmp.Or(os.Getenv("FLEET_SESSION_STATE_FILE"), os.Getenv("AGY_SESSION_STATE_FILE"))
		if stateFile == "" {
			return
		}
		_ = writeTrackedStateFile(stateFile, trackedState{
			ID:         rawID,
			Directory:  cwd,
			Transcript: transcript,
			Policy:     policy,
			Started:    float64(time.Now().UnixNano()) / 1e9,
		})
	}
}

// ExecWithPrompt reads a multiline prompt from promptFile, removes the file, and execs harnessBin with the prompt appended.
func ExecWithPrompt(harness, harnessBin, promptFile string, baseArgs []string) error {
	spec, err := GetSpec(harness)
	if err != nil {
		return err
	}
	promptBytes, err := os.ReadFile(promptFile)
	_ = os.Remove(promptFile)
	if err != nil {
		return fmt.Errorf("read prompt file: %w", err)
	}
	prompt := string(promptBytes)
	argv := make([]string, 0, len(baseArgs)+4)
	argv = append(argv, filepath.Base(harnessBin))
	argv = append(argv, baseArgs...)
	switch {
	case spec.Name == HarnessAgy:
		argv = append(argv, "--prompt-interactive", prompt)
	case spec.Name == HarnessCodex && len(baseArgs) > 0 && baseArgs[0] == "resume":
		argv = append(argv, prompt)
	default:
		argv = append(argv, "--", prompt)
	}
	pathEnv := AugmentedPath(os.Getenv("PATH"), os.Getenv("HOME"))
	return syscall.Exec(harnessBin, argv, withEnvPath(os.Environ(), pathEnv))
}
