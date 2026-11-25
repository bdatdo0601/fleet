package waiter

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"strings"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/hooks"
)

// SendClaudeSocketFn delivers a formatted peer message over Claude Code's local messaging socket.
type SendClaudeSocketFn func(socketPath, token, text string) error

// ClaudeListenerOpts configures NewClaudeSocketListener.
type ClaudeListenerOpts struct {
	APIURL      string
	PPID        int
	CWD         string
	CacheDir    string
	SocketPath  string
	Token       string
	PollTimeout float64
	SendFn      SendClaudeSocketFn
}

// ClaudeSocketListener runs inside `fleet mcp serve --harness claude` and pushes inbox messages over Claude's UDS socket.
type ClaudeSocketListener struct {
	*sessionPushLoop
	cacheDir   string
	socketPath string
	token      string
	sendFn     SendClaudeSocketFn
}

// NewClaudeSocketListener creates a push listener for Claude Code MCP sessions.
func NewClaudeSocketListener(opts ClaudeListenerOpts) *ClaudeSocketListener {
	l := &ClaudeSocketListener{
		cacheDir:   opts.CacheDir,
		socketPath: strings.TrimSpace(opts.SocketPath),
		token:      opts.Token,
		sendFn:     opts.SendFn,
	}
	l.sessionPushLoop = newSessionPushLoop("claude", opts.APIURL, opts.PPID, opts.CWD, opts.PollTimeout, false, l)
	return l
}

// SendClaudeSocketMessage writes an optional auth frame and a user-message frame to Claude Code's UDS socket.
func SendClaudeSocketMessage(socketPath, token, text string) error {
	conn, err := net.DialTimeout("unix", socketPath, 3*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))

	var buf bytes.Buffer
	if token != "" {
		authLine, err := json.Marshal(struct {
			Type  string `json:"type"`
			Token string `json:"token"`
		}{Type: "auth", Token: token})
		if err != nil {
			return err
		}
		buf.Write(authLine)
		buf.WriteByte('\n')
	}
	type userMsgPayload struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	msgLine, err := json.Marshal(struct {
		Type    string         `json:"type"`
		Message userMsgPayload `json:"message"`
	}{
		Type:    "user",
		Message: userMsgPayload{Role: "user", Content: text},
	})
	if err != nil {
		return err
	}
	buf.Write(msgLine)
	buf.WriteByte('\n')
	if _, err := conn.Write(buf.Bytes()); err != nil {
		return err
	}
	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	return nil
}

func (l *ClaudeSocketListener) mode() string { return "mcp_claude_socket" }

func (l *ClaudeSocketListener) preStart() (string, bool) { return "", true }

func (l *ClaudeSocketListener) resolveSocket(knownSID string) (string, string, bool) {
	if l.socketPath != "" {
		if l.sendFn == nil {
			if _, err := os.Stat(l.socketPath); err != nil {
				return "", "", false
			}
		}
		return l.socketPath, l.token, true
	}
	if envSock := strings.TrimSpace(os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET")); envSock != "" {
		if l.sendFn == nil {
			if _, err := os.Stat(envSock); err != nil {
				return "", "", false
			}
		}
		return envSock, os.Getenv("CLAUDE_CODE_MESSAGING_TOKEN"), true
	}
	info, ok := hooks.LoadClaudeSocket(l.cacheDir, l.ppid, knownSID)
	if !ok {
		return "", "", false
	}
	if l.sendFn == nil {
		if _, err := os.Stat(info.Socket); err != nil {
			return "", "", false
		}
	}
	return info.Socket, info.Token, true
}

func (l *ClaudeSocketListener) prePoll(knownSID string) bool {
	sock, _, ok := l.resolveSocket(knownSID)
	if !ok {
		l.setSocket("")
		return false
	}
	l.setSocket(sock)
	return true
}

func (l *ClaudeSocketListener) canDeliver(resp agents.WaitResult) bool { return resp.SessionID != "" }

func (l *ClaudeSocketListener) deliver(resp agents.WaitResult) (string, bool) {
	sock, token, ok := l.resolveSocket(resp.SessionID)
	if !ok {
		return "claude messaging socket unavailable", true
	}
	text := FormatWaitOutput(resp.SessionID, resp.Name, resp.Messages, "")
	send := l.sendFn
	if send == nil {
		send = SendClaudeSocketMessage
	}
	err := send(sock, token, text)
	if err != nil {
		// Re-resolve once in case the session rotated or updated its handoff file.
		if sock2, token2, ok2 := l.resolveSocket(resp.SessionID); ok2 && (sock2 != sock || token2 != token) {
			if retryErr := send(sock2, token2, text); retryErr == nil {
				l.setSocket(sock2)
				return "", false
			}
		}
		return err.Error(), true
	}
	l.setSocket(sock)
	return "", false
}

func (l *ClaudeSocketListener) onStop(sessionID string) {
	hooks.RemoveClaudeSocket(l.cacheDir, l.ppid, sessionID)
}
