// Package slacker implements the lighthouse-hosted Slack Socket Mode dispatcher and pull-based Mac workers.
package slacker

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/bdatdo0601/fleet/internal/session"
)

// DefaultAccountID is the implicit Slack account identifier used when none is specified.
const DefaultAccountID = "default"

// Routing strategy constants for multi-device Slacker channels.
const (
	StrategyLeastAgents = "least_agents"
	StrategyRoundRobin  = "round_robin"
)

// SlackerError represents a caller-level error carrying an HTTP status code.
type SlackerError struct {
	Msg    string
	Status int
}

func (e *SlackerError) Error() string { return e.Msg }

// HTTPStatus returns the HTTP status code associated with this error.
func (e *SlackerError) HTTPStatus() int { return e.Status }

func newSlackerErr(msg string, status int) *SlackerError {
	if status == 0 {
		status = 400
	}
	return &SlackerError{Msg: msg, Status: status}
}

// AccountConfig stores credentials and policy for one Slack workspace/user account.
type AccountConfig struct {
	ID                string `json:"id"`
	Enabled           bool   `json:"enabled"`
	AppToken          string `json:"app_token,omitempty"`
	UserToken         string `json:"user_token,omitempty"`
	BotToken          string `json:"bot_token,omitempty"`
	AppTokenOPRef     string `json:"app_token_op_ref,omitempty"`
	UserTokenOPRef    string `json:"user_token_op_ref,omitempty"`
	BotTokenOPRef     string `json:"bot_token_op_ref,omitempty"`
	AuthorizedUserID  string `json:"authorized_user_id"`
	AuthorizedUser    string `json:"authorized_user,omitempty"`
	TeamID            string `json:"team_id,omitempty"`
	TeamName          string `json:"team_name,omitempty"`
	AllowUnmappedSend bool   `json:"allow_unmapped_send,omitempty"`
}

// AccountView is the redacted view of AccountConfig returned over HTTP APIs.
type AccountView struct {
	ID                string `json:"id"`
	Enabled           bool   `json:"enabled"`
	Configured        bool   `json:"configured"`
	HasAppToken       bool   `json:"has_app_token"`
	AppTokenHint      string `json:"app_token_hint,omitempty"`
	HasUserToken      bool   `json:"has_user_token"`
	UserTokenHint     string `json:"user_token_hint,omitempty"`
	HasBotToken       bool   `json:"has_bot_token"`
	BotTokenHint      string `json:"bot_token_hint,omitempty"`
	AppTokenOPRef     string `json:"app_token_op_ref,omitempty"`
	UserTokenOPRef    string `json:"user_token_op_ref,omitempty"`
	BotTokenOPRef     string `json:"bot_token_op_ref,omitempty"`
	AuthorizedUserID  string `json:"authorized_user_id"`
	AuthorizedUser    string `json:"authorized_user,omitempty"`
	TeamID            string `json:"team_id,omitempty"`
	TeamName          string `json:"team_name,omitempty"`
	AllowUnmappedSend bool   `json:"allow_unmapped_send"`
}

// AccountUpdate describes partial updates for a single Slack account.
type AccountUpdate struct {
	Enabled           *bool   `json:"enabled"`
	AppToken          *string `json:"app_token"`
	UserToken         *string `json:"user_token"`
	BotToken          *string `json:"bot_token"`
	AppTokenOPRef     *string `json:"app_token_op_ref"`
	UserTokenOPRef    *string `json:"user_token_op_ref"`
	BotTokenOPRef     *string `json:"bot_token_op_ref"`
	AuthorizedUserID  *string `json:"authorized_user_id"`
	AuthorizedUser    *string `json:"authorized_user"`
	TeamID            *string `json:"team_id"`
	TeamName          *string `json:"team_name"`
	AllowUnmappedSend *bool   `json:"allow_unmapped_send"`
}

// DeviceConfig stores per-machine Slacker defaults such as its workspace root directory.
type DeviceConfig struct {
	WorkspaceDir string `json:"workspace_dir,omitempty"`
}

// ChannelMapping maps a Slack (account, channel_id) pair to Fleet device(s), strategy, policy, and local repo path.
type ChannelMapping struct {
	Account     string   `json:"account,omitempty"`
	ChannelID   string   `json:"channel_id"`
	ChannelName string   `json:"channel_name,omitempty"`
	Device      string   `json:"device,omitempty"`
	Devices     []string `json:"devices,omitempty"`
	Strategy    string   `json:"strategy,omitempty"`
	Policy      string   `json:"policy,omitempty"`
	RepoPath    string   `json:"repo_path,omitempty"`
	UseTmux     bool     `json:"use_tmux,omitempty"`
}

// Config persists lighthouse Slacker settings in ~/.fleet/slacker.json.
// Top-level token fields are kept only for backward-compatible unmarshaling of single-account configs.
type Config struct {
	Accounts         map[string]AccountConfig  `json:"accounts,omitempty"`
	Devices          map[string]DeviceConfig   `json:"devices,omitempty"`
	DefaultDevices   []string                  `json:"default_devices,omitempty"`
	DefaultStrategy  string                    `json:"default_strategy,omitempty"`
	DefaultPolicy    string                    `json:"default_policy,omitempty"`
	Channels         map[string]ChannelMapping `json:"channels"`
	Emojis           map[string]string         `json:"emojis"`
	UpdatedAt        string                    `json:"updated_at,omitempty"`
	Enabled          bool                      `json:"enabled,omitempty"`
	AppToken         string                    `json:"app_token,omitempty"`
	UserToken        string                    `json:"user_token,omitempty"`
	BotToken         string                    `json:"bot_token,omitempty"`
	AppTokenOPRef    string                    `json:"app_token_op_ref,omitempty"`
	UserTokenOPRef   string                    `json:"user_token_op_ref,omitempty"`
	BotTokenOPRef    string                    `json:"bot_token_op_ref,omitempty"`
	AuthorizedUserID string                    `json:"authorized_user_id,omitempty"`
	AuthorizedUser   string                    `json:"authorized_user,omitempty"`
	TeamID           string                    `json:"team_id,omitempty"`
	TeamName         string                    `json:"team_name,omitempty"`
}

// ConfigView is the redacted view of Config returned over HTTP APIs.
type ConfigView struct {
	Accounts          map[string]AccountView    `json:"accounts"`
	Devices           map[string]DeviceConfig   `json:"devices"`
	DefaultDevices    []string                  `json:"default_devices"`
	DefaultStrategy   string                    `json:"default_strategy"`
	DefaultPolicy     string                    `json:"default_policy"`
	Enabled           bool                      `json:"enabled"`
	Configured        bool                      `json:"configured"`
	HasAppToken       bool                      `json:"has_app_token"`
	AppTokenHint      string                    `json:"app_token_hint,omitempty"`
	HasUserToken      bool                      `json:"has_user_token"`
	UserTokenHint     string                    `json:"user_token_hint,omitempty"`
	HasBotToken       bool                      `json:"has_bot_token"`
	BotTokenHint      string                    `json:"bot_token_hint,omitempty"`
	AppTokenOPRef     string                    `json:"app_token_op_ref,omitempty"`
	UserTokenOPRef    string                    `json:"user_token_op_ref,omitempty"`
	BotTokenOPRef     string                    `json:"bot_token_op_ref,omitempty"`
	AuthorizedUserID  string                    `json:"authorized_user_id"`
	AuthorizedUser    string                    `json:"authorized_user,omitempty"`
	TeamID            string                    `json:"team_id,omitempty"`
	TeamName          string                    `json:"team_name,omitempty"`
	AllowUnmappedSend bool                      `json:"allow_unmapped_send"`
	Channels          map[string]ChannelMapping `json:"channels"`
	Emojis            map[string]string         `json:"emojis"`
	UpdatedAt         string                    `json:"updated_at,omitempty"`
}

// ConfigUpdate is accepted by POST /v1/slacker/config; blank tokens preserve existing stored tokens.
type ConfigUpdate struct {
	Account           string                    `json:"account"`
	DeleteAccount     string                    `json:"delete_account"`
	Accounts          map[string]AccountUpdate  `json:"accounts"`
	Devices           map[string]DeviceConfig   `json:"devices"`
	DefaultDevices    *[]string                 `json:"default_devices"`
	DefaultStrategy   *string                   `json:"default_strategy"`
	DefaultPolicy     *string                   `json:"default_policy"`
	Enabled           *bool                     `json:"enabled"`
	AppToken          *string                   `json:"app_token"`
	UserToken         *string                   `json:"user_token"`
	BotToken          *string                   `json:"bot_token"`
	AppTokenOPRef     *string                   `json:"app_token_op_ref"`
	UserTokenOPRef    *string                   `json:"user_token_op_ref"`
	BotTokenOPRef     *string                   `json:"bot_token_op_ref"`
	AuthorizedUserID  *string                   `json:"authorized_user_id"`
	AuthorizedUser    *string                   `json:"authorized_user"`
	TeamID            *string                   `json:"team_id"`
	TeamName          *string                   `json:"team_name"`
	AllowUnmappedSend *bool                     `json:"allow_unmapped_send"`
	Channels          map[string]ChannelMapping `json:"channels"`
	Emojis            map[string]string         `json:"emojis"`
	Verify            bool                      `json:"verify"`
}

// Job represents a persisted Slack-reaction launch job in slacker.db.
type Job struct {
	ID             string `json:"id"`
	Account        string `json:"account"`
	EventID        string `json:"event_id"`
	ChannelID      string `json:"channel_id"`
	ChannelName    string `json:"channel_name,omitempty"`
	MessageTS      string `json:"message_ts"`
	ThreadTS       string `json:"thread_ts,omitempty"`
	Reaction       string `json:"reaction"`
	UserID         string `json:"user_id"`
	TargetDevice   string `json:"target_device"`
	RepoPath       string `json:"repo_path"`
	UseTmux        bool   `json:"use_tmux"`
	Policy         string `json:"policy,omitempty"`
	Model          string `json:"model,omitempty"`
	Harness        string `json:"harness"`
	SessionName    string `json:"session_name"`
	Prompt         string `json:"prompt"`
	Permalink      string `json:"permalink,omitempty"`
	Status         string `json:"status"`
	AttemptCount   int    `json:"attempt_count"`
	ClaimedBy      string `json:"claimed_by,omitempty"`
	ClaimedAt      string `json:"claimed_at,omitempty"`
	LeaseExpiresAt string `json:"lease_expires_at,omitempty"`
	ReceiptID      string `json:"receipt_id,omitempty"`
	Backend        string `json:"backend,omitempty"`
	Error          string `json:"error,omitempty"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

// Receipt is reported by a target Mac worker back to the lighthouse after launching or reconciling a job.
type Receipt struct {
	JobID       string `json:"job_id"`
	ReceiptID   string `json:"receipt_id"`
	SessionName string `json:"session_name"`
	Backend     string `json:"backend"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
}

// RuntimeStatus tracks listener and per-device worker liveness in slacker.db.
type RuntimeStatus struct {
	Role        string `json:"role"`
	PID         int    `json:"pid"`
	Connected   bool   `json:"connected"`
	LastPollAt  string `json:"last_poll_at,omitempty"`
	LastEventAt string `json:"last_event_at,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	UpdatedAt   string `json:"updated_at"`
}

var (
	unsafeNameCharsRE = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)
	accountIDRE       = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
)

// MaskSecret redacts a token/secret for display without leaking secret material.
func MaskSecret(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) >= 16 {
		return s[:8] + "..." + s[len(s)-4:]
	}
	return "***"
}

// NormalizeEmoji strips surrounding colons and lowercases the emoji name.
func NormalizeEmoji(raw string) string {
	s := strings.TrimSpace(strings.ToLower(raw))
	return strings.Trim(s, ":")
}

// NormalizeStrategy validates and normalizes a multi-device routing strategy.
func NormalizeStrategy(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return "", nil
	case "least_agents", "least-agents", "least":
		return StrategyLeastAgents, nil
	case "round_robin", "round-robin", "rr":
		return StrategyRoundRobin, nil
	default:
		return "", newSlackerErr(fmt.Sprintf("invalid routing strategy %q (expected least_agents or round_robin)", raw), 400)
	}
}

// NormalizePolicy validates and normalizes a Slacker session permission policy.
func NormalizePolicy(raw string) (string, error) {
	p := strings.ToLower(strings.TrimSpace(raw))
	if p == "" {
		return "", nil
	}
	pol, err := session.ParsePolicy(p, "")
	if err != nil {
		return "", newSlackerErr(err.Error(), 400)
	}
	return string(pol), nil
}

func normalizeDeviceList(raw []string) []string {
	if len(raw) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		d := strings.TrimSpace(item)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeWorkspaceDir(raw string) (string, error) {
	ws := strings.TrimSpace(raw)
	if ws == "" {
		return "", nil
	}
	if !strings.HasPrefix(ws, "/") && ws != "~" && !strings.HasPrefix(ws, "~/") {
		return "", newSlackerErr(fmt.Sprintf("workspace_dir %q must be an absolute or ~/ path", ws), 400)
	}
	if len(ws) > 1 {
		trimmed := strings.TrimRight(ws, "/")
		if trimmed == "" {
			ws = "/"
		} else {
			ws = trimmed
		}
	}
	return ws, nil
}

// NormalizeAccountID validates and normalizes a Slack account identifier, defaulting empty to "default".
func NormalizeAccountID(raw string) (string, error) {
	id := strings.ToLower(strings.TrimSpace(raw))
	if id == "" {
		return DefaultAccountID, nil
	}
	if !accountIDRE.MatchString(id) {
		return "", newSlackerErr("account id must be 1-32 lowercase letters, digits, dashes, or underscores", 400)
	}
	return id, nil
}

// ChannelKey builds the canonical map key for a (account, channelID) mapping.
func ChannelKey(account, channelID string) string {
	acct := strings.ToLower(strings.TrimSpace(account))
	cid := strings.TrimSpace(channelID)
	if acct == "" || acct == DefaultAccountID {
		return cid
	}
	return acct + ":" + cid
}

func sanitizeNameSegment(raw, fallback string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = unsafeNameCharsRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-_")
	if s == "" {
		return fallback
	}
	return s
}

// DeterministicSessionName derives a stable, valid session name from channelID and messageTS.
func DeterministicSessionName(channelID, messageTS string) string {
	ch := sanitizeNameSegment(channelID, "chan")
	ts := sanitizeNameSegment(strings.ReplaceAll(messageTS, ".", "-"), "0")
	return "slack-" + ch + "-" + ts
}

// DeterministicSessionNameForAccount derives a stable, valid session name scoped by account when non-default.
func DeterministicSessionNameForAccount(account, channelID, messageTS string) string {
	acct := strings.ToLower(strings.TrimSpace(account))
	if acct == "" || acct == DefaultAccountID {
		return DeterministicSessionName(channelID, messageTS)
	}
	ch := sanitizeNameSegment(channelID, "chan")
	ts := sanitizeNameSegment(strings.ReplaceAll(messageTS, ".", "-"), "0")
	return "slack-" + sanitizeNameSegment(acct, "acct") + "-" + ch + "-" + ts
}

// DefaultEmojis returns starter emoji-to-harness mappings.
func DefaultEmojis() map[string]string {
	return map[string]string{
		"claude":      session.HarnessClaude,
		"agy":         session.HarnessAgy,
		"antigravity": session.HarnessAgy,
		"codex":       session.HarnessCodex,
	}
}

func (a AccountConfig) configured() bool {
	return a.AppToken != "" && a.UserToken != "" && a.AuthorizedUserID != ""
}

// View returns the redacted AccountView for a.
func (a AccountConfig) View() AccountView {
	return AccountView{
		ID:                a.ID,
		Enabled:           a.Enabled,
		Configured:        a.configured(),
		HasAppToken:       a.AppToken != "",
		AppTokenHint:      MaskSecret(a.AppToken),
		HasUserToken:      a.UserToken != "",
		UserTokenHint:     MaskSecret(a.UserToken),
		HasBotToken:       a.BotToken != "",
		BotTokenHint:      MaskSecret(a.BotToken),
		AppTokenOPRef:     a.AppTokenOPRef,
		UserTokenOPRef:    a.UserTokenOPRef,
		BotTokenOPRef:     a.BotTokenOPRef,
		AuthorizedUserID:  a.AuthorizedUserID,
		AuthorizedUser:    a.AuthorizedUser,
		TeamID:            a.TeamID,
		TeamName:          a.TeamName,
		AllowUnmappedSend: a.AllowUnmappedSend,
	}
}
