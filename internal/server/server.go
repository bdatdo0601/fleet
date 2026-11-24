// Package server implements `fleet serve`: the lighthouse's mesh-only HTTP API.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bdatdo0601/fleet/internal/agents"
	"github.com/bdatdo0601/fleet/internal/core"
)

const MaxBody = 256 * 1024

func writeJSON(w http.ResponseWriter, status int, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, `{"error":"internal encode error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func readJSONBody(r *http.Request, dst any) error {
	if r.ContentLength > MaxBody {
		return &core.FleetError{Msg: "request too large", Status: http.StatusRequestEntityTooLarge}
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil {
		return &core.FleetError{Msg: "failed to read body", Status: http.StatusBadRequest}
	}
	if len(data) > MaxBody {
		return &core.FleetError{Msg: "request too large", Status: http.StatusRequestEntityTooLarge}
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		data = []byte("{}")
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return &core.FleetError{Msg: "invalid JSON", Status: http.StatusBadRequest}
	}
	return nil
}

func clientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil && host != "" {
		return host
	}
	return remoteAddr
}

func handleError(w http.ResponseWriter, r *http.Request, err error, quiet bool) {
	var fe *core.FleetError
	if errors.As(err, &fe) {
		writeJSON(w, fe.Status, map[string]string{"error": fe.Msg})
		return
	}
	var ae *agents.AgentError
	if errors.As(err, &ae) {
		writeJSON(w, ae.Status, map[string]string{"error": ae.Msg})
		return
	}
	if !quiet {
		_, _ = fmt.Fprintf(os.Stderr, "%s internal error: %v\n", clientIP(r.RemoteAddr), err)
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error (see serve log)"})
}

// NewHandler returns the HTTP handler for the lighthouse API.
func NewHandler(fleet *core.Fleet, quiet bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerIP := clientIP(r.RemoteAddr)
		if !quiet {
			_, _ = fmt.Fprintf(os.Stderr, "%s %s %s\n", peerIP, r.Method, r.URL.RequestURI())
		}
		caller, err := fleet.Identify(peerIP)
		if err != nil {
			handleError(w, r, err, quiet)
			return
		}
		if caller == nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "not a member of this fleet"})
			return
		}

		pathOnly := r.URL.Path
		if r.Method == http.MethodGet && pathOnly == "/v1/sync" {
			q := r.URL.Query()
			res, err := fleet.Sync(caller, r.Header.Get("X-Fleet-Client"), q.Get("os"), q.Get("arch"))
			if err != nil {
				handleError(w, r, err, quiet)
				return
			}
			writeJSON(w, http.StatusOK, res)
			return
		}

		if r.Method == http.MethodGet && pathOnly == "/v1/binary" {
			q := r.URL.Query()
			binPath, shaHex, ver, err := fleet.BinaryForPlatform(q.Get("os"), q.Get("arch"))
			if err != nil {
				handleError(w, r, err, quiet)
				return
			}
			f, err := os.Open(binPath)
			if err != nil {
				handleError(w, r, err, quiet)
				return
			}
			defer func() { _ = f.Close() }()
			info, err := f.Stat()
			if err != nil {
				handleError(w, r, err, quiet)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("X-Fleet-SHA256", shaHex)
			if ver != "" {
				w.Header().Set("X-Fleet-Version", ver)
			}
			http.ServeContent(w, r, filepath.Base(binPath), info.ModTime(), f)
			return
		}

		if pathOnly == "/v1/agents" || strings.HasPrefix(pathOnly, "/v1/agents/") {
			handled, err := dispatchAgents(w, r, fleet, caller.Name)
			if err != nil {
				handleError(w, r, err, quiet)
				return
			}
			if !handled {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			}
			return
		}

		if !core.IsAdmin(caller) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": fmt.Sprintf("%s is not an admin device", caller.Name)})
			return
		}

		if r.Method == http.MethodGet && pathOnly == "/v1/devices" {
			devs, err := fleet.List()
			if err != nil {
				handleError(w, r, err, quiet)
				return
			}
			caID, err := fleet.CAID()
			if err != nil {
				handleError(w, r, err, quiet)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"devices": devs, "ca_id": caID})
			return
		}

		if r.Method == http.MethodPost {
			switch pathOnly {
			case "/v1/approve":
				var req struct {
					Name       string `json:"name"`
					Pubkey     string `json:"pubkey"`
					Kind       string `json:"kind"`
					Group      string `json:"group"`
					Passphrase string `json:"passphrase"`
					Replace    bool   `json:"replace"`
				}
				if err := readJSONBody(r, &req); err != nil {
					handleError(w, r, err, quiet)
					return
				}
				res, err := fleet.Approve(core.ApproveOpts{
					Name:       req.Name,
					Pubkey:     req.Pubkey,
					Kind:       req.Kind,
					Group:      req.Group,
					Passphrase: req.Passphrase,
					Replace:    req.Replace,
				})
				if err != nil {
					handleError(w, r, err, quiet)
					return
				}
				writeJSON(w, http.StatusOK, res)
				return
			case "/v1/revoke":
				var req struct {
					Name string `json:"name"`
				}
				if err := readJSONBody(r, &req); err != nil {
					handleError(w, r, err, quiet)
					return
				}
				if err := fleet.Revoke(req.Name); err != nil {
					handleError(w, r, err, quiet)
					return
				}
				writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
				return
			case "/v1/set-group":
				var req struct {
					Name       string `json:"name"`
					Group      string `json:"group"`
					Passphrase string `json:"passphrase"`
				}
				if err := readJSONBody(r, &req); err != nil {
					handleError(w, r, err, quiet)
					return
				}
				res, err := fleet.SetGroup(req.Name, req.Group, req.Passphrase)
				if err != nil {
					handleError(w, r, err, quiet)
					return
				}
				writeJSON(w, http.StatusOK, res)
				return
			}
		}

		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})
}

func dispatchAgents(w http.ResponseWriter, r *http.Request, fleet *core.Fleet, callerName string) (bool, error) {
	pathOnly := r.URL.Path
	if r.Method == http.MethodGet && pathOnly == "/v1/agents" {
		q := r.URL.Query()
		inc := strings.ToLower(q.Get("include_stale"))
		list, err := fleet.Agents.ListSessions(
			q.Get("device"),
			q.Get("repo"),
			q.Get("harness"),
			q.Get("state"),
			inc == "1" || inc == "true",
		)
		if err != nil {
			return true, err
		}
		writeJSON(w, http.StatusOK, map[string]any{"agents": list})
		return true, nil
	}
	if r.Method == http.MethodGet && strings.HasPrefix(pathOnly, "/v1/agents/") {
		escaped := r.URL.EscapedPath()
		rawTarget := strings.TrimPrefix(escaped, "/v1/agents/")
		target, err := url.PathUnescape(rawTarget)
		if err != nil {
			target = strings.TrimPrefix(pathOnly, "/v1/agents/")
		}
		detail, err := fleet.Agents.GetSession(target)
		if err != nil {
			return true, err
		}
		writeJSON(w, http.StatusOK, map[string]any{"agent": detail})
		return true, nil
	}
	if r.Method != http.MethodPost {
		return false, nil
	}

	switch pathOnly {
	case "/v1/agents/heartbeat":
		var req struct {
			Harness       string            `json:"harness"`
			RawID         string            `json:"raw_id"`
			PID           *int              `json:"pid"`
			CWD           string            `json:"cwd"`
			Repo          string            `json:"repo"`
			Branch        string            `json:"branch"`
			State         string            `json:"state"`
			PendingAction map[string]string `json:"pending_action"`
			Title         *string           `json:"title"`
			Name          *string           `json:"name"`
			ExplicitName  bool              `json:"explicit_name"`
			Summary       *string           `json:"summary"`
			GitStatus     *string           `json:"git_status"`
			Tail          []agents.TailItem `json:"tail"`
			DrainInbox    bool              `json:"drain_inbox"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return true, err
		}
		if req.Harness == "" {
			req.Harness = "claude"
		}
		if req.State == "" {
			req.State = "working"
		}
		res, err := fleet.Agents.Heartbeat(agents.HeartbeatParams{
			Device:        callerName,
			Harness:       req.Harness,
			RawID:         req.RawID,
			PID:           req.PID,
			CWD:           req.CWD,
			Repo:          req.Repo,
			Branch:        req.Branch,
			State:         req.State,
			PendingAction: req.PendingAction,
			Title:         req.Title,
			Name:          req.Name,
			ExplicitName:  req.ExplicitName,
			Summary:       req.Summary,
			GitStatus:     req.GitStatus,
			Tail:          req.Tail,
			DrainInbox:    req.DrainInbox,
		})
		if err != nil {
			return true, err
		}
		writeJSON(w, http.StatusOK, res)
		return true, nil

	case "/v1/agents/deregister":
		var req struct {
			SessionID *string `json:"session_id"`
			Harness   *string `json:"harness"`
			PID       *int    `json:"pid"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return true, err
		}
		if err := fleet.Agents.Deregister(callerName, req.SessionID, req.Harness, req.PID); err != nil {
			return true, err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return true, nil

	case "/v1/agents/send":
		var req struct {
			Harness   string  `json:"harness"`
			CWD       string  `json:"cwd"`
			PID       *int    `json:"pid"`
			SessionID *string `json:"session_id"`
			To        string  `json:"to"`
			Body      string  `json:"body"`
			ThreadID  *string `json:"thread_id"`
			ReplyTo   *int64  `json:"reply_to"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return true, err
		}
		fromSID, err := fleet.Agents.ResolveCallerSession(callerName, req.Harness, req.SessionID, req.CWD, req.PID)
		if err != nil {
			return true, err
		}
		known, err := fleet.ActiveDeviceNames()
		if err != nil {
			return true, err
		}
		res, err := fleet.Agents.SendMessageWithReport(callerName, fromSID, req.To, req.Body, req.ThreadID, req.ReplyTo, known)
		if err != nil {
			return true, err
		}
		writeJSON(w, http.StatusOK, res)
		return true, nil

	case "/v1/agents/inbox":
		var req struct {
			Harness    string  `json:"harness"`
			CWD        string  `json:"cwd"`
			PID        *int    `json:"pid"`
			SessionID  *string `json:"session_id"`
			UnreadOnly *bool   `json:"unread_only"`
			MarkRead   *bool   `json:"mark_read"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return true, err
		}
		sid, err := fleet.Agents.ResolveCallerSession(callerName, req.Harness, req.SessionID, req.CWD, req.PID)
		if err != nil {
			return true, err
		}
		unreadOnly := true
		if req.UnreadOnly != nil {
			unreadOnly = *req.UnreadOnly
		}
		markRead := true
		if req.MarkRead != nil {
			markRead = *req.MarkRead
		}
		msgs, err := fleet.Agents.FetchInbox(callerName, sid, unreadOnly, markRead, 50)
		if err != nil {
			return true, err
		}
		writeJSON(w, http.StatusOK, map[string]any{"session_id": sid, "messages": msgs})
		return true, nil

	case "/v1/agents/ask":
		var req struct {
			Harness    string   `json:"harness"`
			CWD        string   `json:"cwd"`
			PID        *int     `json:"pid"`
			SessionID  *string  `json:"session_id"`
			To         string   `json:"to"`
			Body       string   `json:"body"`
			Timeout    *float64 `json:"timeout"`
			WaitIfIdle bool     `json:"wait_if_idle"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return true, err
		}
		fromSID, err := fleet.Agents.ResolveCallerSession(callerName, req.Harness, req.SessionID, req.CWD, req.PID)
		if err != nil {
			return true, err
		}
		known, err := fleet.ActiveDeviceNames()
		if err != nil {
			return true, err
		}
		timeout := 30.0
		if req.Timeout != nil {
			timeout = *req.Timeout
		}
		res, err := fleet.Agents.Ask(r.Context(), agents.AskParams{
			Device:       callerName,
			FromSession:  fromSID,
			To:           req.To,
			Body:         req.Body,
			Timeout:      timeout,
			WaitIfIdle:   req.WaitIfIdle,
			KnownDevices: known,
		})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return true, nil
			}
			return true, err
		}
		writeJSON(w, http.StatusOK, res)
		return true, nil

	case "/v1/agents/wait":
		var req struct {
			Harness       string   `json:"harness"`
			CWD           string   `json:"cwd"`
			PID           *int     `json:"pid"`
			SessionID     *string  `json:"session_id"`
			Timeout       *float64 `json:"timeout"`
			MarkRead      *bool    `json:"mark_read"`
			Limit         *int     `json:"limit"`
			RequireFullID bool     `json:"require_full_id"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return true, err
		}
		sid, err := fleet.Agents.ResolveCallerSession(callerName, req.Harness, req.SessionID, req.CWD, req.PID)
		if err != nil {
			return true, err
		}
		timeout := 55.0
		if req.Timeout != nil {
			timeout = *req.Timeout
		}
		markRead := true
		if req.MarkRead != nil {
			markRead = *req.MarkRead
		}
		limit := 50
		if req.Limit != nil {
			limit = *req.Limit
		}
		res, err := fleet.Agents.WaitForMessages(r.Context(), agents.WaitParams{
			Device:        callerName,
			SessionID:     sid,
			Timeout:       timeout,
			MarkRead:      markRead,
			Limit:         limit,
			RequireFullID: req.RequireFullID,
		})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return true, nil
			}
			return true, err
		}
		writeJSON(w, http.StatusOK, res)
		return true, nil

	case "/v1/agents/ack":
		var req struct {
			Harness    string   `json:"harness"`
			CWD        string   `json:"cwd"`
			PID        *int     `json:"pid"`
			SessionID  *string  `json:"session_id"`
			MessageIDs *[]int64 `json:"message_ids"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return true, err
		}
		sid, err := fleet.Agents.ResolveCallerSession(callerName, req.Harness, req.SessionID, req.CWD, req.PID)
		if err != nil {
			return true, err
		}
		if req.MessageIDs == nil {
			return true, &agents.AgentError{Msg: "message_ids list is required", Status: 400}
		}
		acked, err := fleet.Agents.AckMessages(callerName, sid, *req.MessageIDs)
		if err != nil {
			return true, err
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "acked": acked})
		return true, nil

	case "/v1/agents/rename":
		var req struct {
			Harness   string  `json:"harness"`
			CWD       string  `json:"cwd"`
			PID       *int    `json:"pid"`
			SessionID *string `json:"session_id"`
			Name      string  `json:"name"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return true, err
		}
		sid, err := fleet.Agents.ResolveCallerSession(callerName, req.Harness, req.SessionID, req.CWD, req.PID)
		if err != nil {
			return true, err
		}
		slug, err := fleet.Agents.RenameSession(callerName, sid, req.Name)
		if err != nil {
			return true, err
		}
		writeJSON(w, http.StatusOK, map[string]string{"session_id": sid, "name": slug})
		return true, nil
	}

	return false, nil
}

// NewServer constructs an *http.Server configured with safe mesh timeouts.
// WriteTimeout is intentionally omitted so long-poll /v1/agents/wait and /v1/agents/ask
// requests can stay open up to 120 seconds.
func NewServer(fleet *core.Fleet, addr string, quiet bool) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           NewHandler(fleet, quiet),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    64 * 1024,
	}
}

// Serve runs the lighthouse HTTP API server until SIGINT or SIGTERM.
func Serve(port int, host string) error {
	if host == "" {
		host = "0.0.0.0"
	}
	fleet, err := core.Open("")
	if err != nil {
		return err
	}
	defer func() { _ = fleet.Close() }()

	addr := fmt.Sprintf("%s:%d", host, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := NewServer(fleet, addr, false)
	_, _ = fmt.Fprintf(os.Stderr, "fleet serve listening on %s\n", addr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return <-errCh
	case err := <-errCh:
		return err
	}
}
