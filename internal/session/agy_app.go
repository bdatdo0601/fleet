package session

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/bdatdo0601/fleet/internal/paths"
	// Register the pure-Go sqlite driver for database/sql.
	_ "modernc.org/sqlite"
)

const maxAgyResponseBytes = 4 << 20

var agyHTTPClient = &http.Client{Timeout: 15 * time.Second}

type agyHubInfo struct {
	PID       int    `json:"pid"`
	HTTPPort  int    `json:"httpPort"`
	HTTPSPort int    `json:"httpsPort"`
	LSVersion string `json:"lsVersion"`
	CSRFToken string `json:"csrfToken"`
}

type agyConversation struct {
	ID       string `json:"id"`
	Metadata struct {
		ProjectID string   `json:"projectId"`
		Tags      []string `json:"tags"`
	} `json:"metadata"`
	Annotations struct {
		Title    string `json:"title"`
		Archived bool   `json:"archived"`
	} `json:"annotations"`
	State struct {
		Summary  string `json:"summary"`
		RunState string `json:"runState"`
	} `json:"state"`
}

type connectError struct {
	StatusCode int
	Code       string `json:"code"`
	Message    string `json:"message"`
}

func (e *connectError) Error() string {
	if e.Message != "" {
		if e.Code != "" {
			return fmt.Sprintf("%s: %s", e.Code, e.Message)
		}
		return e.Message
	}
	return fmt.Sprintf("HTTP %d", e.StatusCode)
}

func isConnectNotFound(err error) bool {
	var ce *connectError
	if !errors.As(err, &ce) {
		return false
	}
	return ce.StatusCode == http.StatusNotFound || ce.Code == "not_found"
}

type appSessionState struct {
	Name           string  `json:"name"`
	ConversationID string  `json:"conversation_id"`
	Directory      string  `json:"directory"`
	ProjectID      string  `json:"project_id,omitempty"`
	CreatedAt      float64 `json:"created_at,omitempty"`
}

var openAgyAppFn = defaultOpenAgyApp

func defaultOpenAgyApp() error {
	if runtime.GOOS != "darwin" || os.Getenv("FLEET_SESSION_NO_OPEN") == "1" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "open", "-a", "Antigravity").Run()
}

func agyAppDataDir() string {
	if v := strings.TrimSpace(os.Getenv("FLEET_AGY_APP_DATA_DIR")); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gemini", "antigravity")
}

func discoverAgyHub() (agyHubInfo, error) {
	daemonDir := filepath.Join(agyAppDataDir(), "daemon")
	matches, err := filepath.Glob(filepath.Join(daemonDir, "hub_*.json"))
	if err == nil {
		var best agyHubInfo
		var bestMod time.Time
		found := false
		for _, p := range matches {
			fi, sErr := os.Stat(p)
			if sErr != nil {
				continue
			}
			raw, rErr := os.ReadFile(p)
			if rErr != nil {
				continue
			}
			var hub agyHubInfo
			if json.Unmarshal(raw, &hub) != nil {
				continue
			}
			if hub.PID <= 0 || hub.HTTPPort <= 0 || hub.CSRFToken == "" {
				continue
			}
			if syscall.Kill(hub.PID, 0) != nil {
				continue
			}
			if !found || fi.ModTime().After(bestMod) {
				best = hub
				bestMod = fi.ModTime()
				found = true
			}
		}
		if found {
			return best, nil
		}
	}
	return agyHubInfo{}, fmt.Errorf("antigravity app is not running (no active hub in %s); open Antigravity or pass --rex / --tmux", daemonDir)
}

func canonicalPath(p string) string {
	cleaned := filepath.Clean(p)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return resolved
	}
	return cleaned
}

func resolveAgyProjectID(directory string) string {
	dbPath := filepath.Join(agyAppDataDir(), "conversation_summaries.db")
	if _, err := os.Stat(dbPath); err != nil {
		return ""
	}
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(dbPath),
		RawQuery: "mode=ro&_pragma=busy_timeout(2000)",
	}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return ""
	}
	defer func() { _ = db.Close() }()

	rows, err := db.Query(`
		SELECT project_id, workspace_uris
		FROM conversation_summaries
		WHERE project_id != '' AND project_id != 'default-cli-project' AND workspace_uris != ''
		ORDER BY last_modified_time DESC
	`)
	if err != nil {
		return ""
	}
	defer func() { _ = rows.Close() }()

	wantDir := canonicalPath(directory)
	for rows.Next() {
		var projectID, urisRaw string
		if rows.Scan(&projectID, &urisRaw) != nil {
			continue
		}
		var uris []string
		if json.Unmarshal([]byte(urisRaw), &uris) != nil {
			continue
		}
		for _, rawURI := range uris {
			u, uErr := url.Parse(rawURI)
			if uErr != nil || u.Scheme != "file" || u.Path == "" {
				continue
			}
			if canonicalPath(u.Path) == wantDir {
				return projectID
			}
		}
	}
	if rows.Err() != nil {
		return ""
	}
	return ""
}

func callConversationService(hub agyHubInfo, method string, reqBody any, respOut any) error {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/product.v1.ConversationService/%s", hub.HTTPPort, method)
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("x-antigravity-csrf-token", hub.CSRFToken)

	resp, err := agyHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("antigravity ConversationService/%s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAgyResponseBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ce := &connectError{StatusCode: resp.StatusCode}
		_ = json.Unmarshal(body, ce)
		if ce.Message == "" {
			ce.Message = strings.TrimSpace(string(body))
		}
		return ce
	}
	if respOut != nil && len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, respOut); err != nil {
			return fmt.Errorf("decode ConversationService/%s response: %w", method, err)
		}
	}
	return nil
}

func agyCreateConversation(hub agyHubInfo, projectID, title, model, prompt string, tags []string) (agyConversation, error) {
	req := map[string]any{}
	if projectID != "" {
		req["projectId"] = projectID
	}
	if title != "" {
		req["title"] = title
	}
	if model != "" {
		req["model"] = model
	}
	if len(tags) > 0 {
		req["tags"] = tags
	}
	if prompt != "" {
		req["prompt"] = []map[string]string{{"text": prompt}}
	}
	var resp struct {
		Conversation agyConversation `json:"conversation"`
	}
	if err := callConversationService(hub, "CreateConversation", req, &resp); err != nil {
		return agyConversation{}, err
	}
	if resp.Conversation.ID == "" {
		return agyConversation{}, errors.New("antigravity CreateConversation returned empty conversation ID")
	}
	return resp.Conversation, nil
}

func agyGetConversation(hub agyHubInfo, id string) (agyConversation, error) {
	var conv agyConversation
	if err := callConversationService(hub, "GetConversation", map[string]string{"conversationId": id}, &conv); err != nil {
		return agyConversation{}, err
	}
	return conv, nil
}

func agyUpdateConversationArchived(hub agyHubInfo, id string, archived bool) error {
	req := map[string]any{
		"conversationId": id,
		"annotations": map[string]any{
			"archived": archived,
		},
	}
	return callConversationService(hub, "UpdateConversation", req, nil)
}

func appStateDir(harness string) string {
	return filepath.Join(stateRootFor(harness), "app")
}

func appStatePath(harness, name string) string {
	return filepath.Join(appStateDir(harness), name+".json")
}

func readAppSessionState(harness, name string) (appSessionState, bool) {
	raw, err := os.ReadFile(appStatePath(harness, name))
	if err != nil {
		return appSessionState{}, false
	}
	var st appSessionState
	if json.Unmarshal(raw, &st) != nil || st.ConversationID == "" {
		return appSessionState{}, false
	}
	st.Name = cmp.Or(st.Name, name)
	return st, true
}

func writeAppSessionState(harness, name string, st appSessionState) error {
	st.Name = cmp.Or(st.Name, name)
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	_, err = paths.WriteFileAtomic(appStatePath(harness, name), append(b, '\n'), 0o644, nil, nil)
	return err
}

func removeAppSessionState(name string) {
	_ = os.Remove(appStatePath(HarnessAgy, name))
}

func listAppSessionStates(harness string) []appSessionState {
	entries, err := os.ReadDir(appStateDir(harness))
	if err != nil {
		return nil
	}
	var out []appSessionState
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(ent.Name(), ".json")
		if st, ok := readAppSessionState(harness, name); ok {
			out = append(out, st)
		}
	}
	return out
}

func agyAppSessionActive(name string) (appSessionState, bool) {
	st, ok := readAppSessionState(HarnessAgy, name)
	if !ok {
		return appSessionState{}, false
	}
	hub, err := discoverAgyHub()
	if err != nil {
		// Keep local tracking when Antigravity app is not currently running.
		return st, true
	}
	conv, err := agyGetConversation(hub, st.ConversationID)
	if err != nil {
		if isConnectNotFound(err) {
			removeAppSessionState(name)
			return appSessionState{}, false
		}
		return st, true
	}
	if conv.Annotations.Archived {
		removeAppSessionState(name)
		return appSessionState{}, false
	}
	return st, true
}

func startAgyAppSession(opts StartOpts, directory string) error {
	stdout := cmp.Or(opts.Stdout, io.Discard)
	stderr := cmp.Or(opts.Stderr, io.Discard)
	hub, err := discoverAgyHub()
	if err != nil {
		return err
	}
	if opts.ResumeID != "" {
		conv, gErr := agyGetConversation(hub, opts.ResumeID)
		if gErr != nil {
			return fmt.Errorf("resume Antigravity conversation %s: %w", opts.ResumeID, gErr)
		}
		if conv.Annotations.Archived {
			if uErr := agyUpdateConversationArchived(hub, opts.ResumeID, false); uErr != nil {
				return fmt.Errorf("unarchive Antigravity conversation %s: %w", opts.ResumeID, uErr)
			}
		}
		if wErr := writeAppSessionState(HarnessAgy, opts.Name, appSessionState{
			Name:           opts.Name,
			ConversationID: opts.ResumeID,
			Directory:      directory,
			ProjectID:      conv.Metadata.ProjectID,
			CreatedAt:      float64(time.Now().UnixNano()) / 1e9,
		}); wErr != nil {
			return wErr
		}
		_, _ = fmt.Fprintf(stdout, "Attached Antigravity app session %s (%s)\n", opts.Name, opts.ResumeID)
		if opts.Attach {
			if oErr := openAgyAppFn(); oErr != nil {
				_, _ = fmt.Fprintf(stderr, "fleet session agy: could not open Antigravity app: %v\n", oErr)
			}
		}
		return nil
	}

	projectID := resolveAgyProjectID(directory)
	promptText := opts.Prompt
	if projectID == "" {
		_, _ = fmt.Fprintf(stderr, "fleet session agy: no existing Antigravity project found for %s; creating conversation without project binding.\n", directory)
		if promptText != "" {
			promptText = "Working directory: " + directory + "\n\n" + promptText
		}
	}

	tags := []string{"fleet", "fleet-session:" + opts.Name}
	conv, err := agyCreateConversation(hub, projectID, opts.Name, opts.Model, promptText, tags)
	if err != nil {
		return err
	}
	if err := writeAppSessionState(HarnessAgy, opts.Name, appSessionState{
		Name:           opts.Name,
		ConversationID: conv.ID,
		Directory:      directory,
		ProjectID:      cmp.Or(conv.Metadata.ProjectID, projectID),
		CreatedAt:      float64(time.Now().UnixNano()) / 1e9,
	}); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(stdout, "Started Antigravity app session %s (%s) in %s\n", opts.Name, conv.ID, directory)
	if opts.Attach {
		if oErr := openAgyAppFn(); oErr != nil {
			_, _ = fmt.Fprintf(stderr, "fleet session agy: could not open Antigravity app: %v\n", oErr)
		}
	}
	_, _ = fmt.Fprintln(stdout, "Open it in the Antigravity app or at https://antigravity.google.com")
	return nil
}
