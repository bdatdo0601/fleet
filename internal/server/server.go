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
	"github.com/bdatdo0601/fleet/internal/registry"
)

const MaxBody = 256 * 1024

type statusError interface {
	error
	HTTPStatus() int
}

type callerReq struct {
	Harness   string  `json:"harness"`
	CWD       string  `json:"cwd"`
	PID       *int    `json:"pid"`
	SessionID *string `json:"session_id"`
}

func (c callerReq) callerInfo() callerReq { return c }

func decodeCaller[T interface{ callerInfo() callerReq }](r *http.Request, fleet *core.Fleet, dev string) (T, string, error) {
	var req T
	if err := readJSONBody(r, &req); err != nil {
		return req, "", err
	}
	c := req.callerInfo()
	sid, err := fleet.Agents.ResolveCallerSession(dev, c.Harness, c.SessionID, c.CWD, c.PID)
	return req, sid, err
}

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
	if se, ok := errors.AsType[statusError](err); ok {
		writeJSON(w, se.HTTPStatus(), map[string]string{"error": se.Error()})
		return
	}
	if !quiet {
		_, _ = fmt.Fprintf(os.Stderr, "%s internal error: %v\n", clientIP(r.RemoteAddr), err)
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error (see serve log)"})
}

func rawAgentTarget(r *http.Request) string {
	rawTarget := strings.TrimPrefix(r.URL.EscapedPath(), "/v1/agents/")
	if target, err := url.PathUnescape(rawTarget); err == nil {
		return target
	}
	return strings.TrimPrefix(r.URL.Path, "/v1/agents/")
}

func optBool(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func optFloat(p *float64, def float64) float64 {
	if p == nil {
		return def
	}
	return *p
}

func optInt(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// NewHandler returns the HTTP handler for the lighthouse API.
func NewHandler(fleet *core.Fleet, quiet bool) http.Handler {
	mux := http.NewServeMux()
	route := func(adminOnly bool, fn func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
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
			if adminOnly && !core.IsAdmin(caller) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": fmt.Sprintf("%s is not an admin device", caller.Name)})
				return
			}
			if err := fn(w, r, caller); err != nil && !errors.Is(err, context.Canceled) {
				handleError(w, r, err, quiet)
			}
		}
	}

	mux.HandleFunc("GET /v1/sync", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		q := r.URL.Query()
		res, err := fleet.Sync(c, r.Header.Get("X-Fleet-Client"), q.Get("os"), q.Get("arch"))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}))
	mux.HandleFunc("GET /v1/binary", route(false, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		q := r.URL.Query()
		binPath, shaHex, ver, err := fleet.BinaryForPlatform(q.Get("os"), q.Get("arch"))
		if err != nil {
			return err
		}
		f, err := os.Open(binPath)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Fleet-SHA256", shaHex)
		if ver != "" {
			w.Header().Set("X-Fleet-Version", ver)
		}
		http.ServeContent(w, r, filepath.Base(binPath), info.ModTime(), f)
		return nil
	}))

	registerAgentRoutes(mux, fleet, route)

	mux.HandleFunc("GET /v1/devices", route(true, func(w http.ResponseWriter, _ *http.Request, _ *registry.Device) error {
		devs, err := fleet.List()
		if err != nil {
			return err
		}
		caID, err := fleet.CAID()
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"devices": devs, "ca_id": caID})
		return nil
	}))
	mux.HandleFunc("POST /v1/approve", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req core.ApproveOpts
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		res, err := fleet.Approve(req)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}))
	mux.HandleFunc("POST /v1/revoke", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			Name string `json:"name"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		if err := fleet.Revoke(req.Name); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))
	mux.HandleFunc("POST /v1/set-group", route(true, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		var req struct {
			Name       string `json:"name"`
			Group      string `json:"group"`
			Passphrase string `json:"passphrase"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		res, err := fleet.SetGroup(req.Name, req.Group, req.Passphrase)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}))
	mux.HandleFunc("/", route(false, func(w http.ResponseWriter, _ *http.Request, _ *registry.Device) error {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return nil
	}))
	return mux
}

func registerAgentRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("GET /v1/agents", route(false, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		q := r.URL.Query()
		inc := strings.ToLower(q.Get("include_stale"))
		list, err := fleet.Agents.ListSessions(q.Get("device"), q.Get("repo"), q.Get("harness"), q.Get("state"), inc == "1" || inc == "true")
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"agents": list})
		return nil
	}))
	mux.HandleFunc("GET /v1/agents/{target...}", route(false, func(w http.ResponseWriter, r *http.Request, _ *registry.Device) error {
		detail, err := fleet.Agents.GetSession(rawAgentTarget(r))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"agent": detail})
		return nil
	}))
	mux.HandleFunc("POST /v1/agents/heartbeat", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		var req agents.HeartbeatParams
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		req.Device = c.Name
		if req.Harness == "" {
			req.Harness = "claude"
		}
		if req.State == "" {
			req.State = "working"
		}
		res, err := fleet.Agents.Heartbeat(req)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}))
	mux.HandleFunc("POST /v1/agents/deregister", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		var req struct {
			SessionID *string `json:"session_id"`
			Harness   *string `json:"harness"`
			PID       *int    `json:"pid"`
		}
		if err := readJSONBody(r, &req); err != nil {
			return err
		}
		if err := fleet.Agents.Deregister(c.Name, req.SessionID, req.Harness, req.PID); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}))
	registerAgentMessageRoutes(mux, fleet, route)
}

func registerAgentMessageRoutes(
	mux *http.ServeMux,
	fleet *core.Fleet,
	route func(bool, func(http.ResponseWriter, *http.Request, *registry.Device) error) http.HandlerFunc,
) {
	mux.HandleFunc("POST /v1/agents/send", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		req, fromSID, err := decodeCaller[struct {
			callerReq
			To       string  `json:"to"`
			Body     string  `json:"body"`
			ThreadID *string `json:"thread_id"`
			ReplyTo  *int64  `json:"reply_to"`
		}](r, fleet, c.Name)
		if err != nil {
			return err
		}
		known, err := fleet.ActiveDeviceNames()
		if err != nil {
			return err
		}
		res, err := fleet.Agents.SendMessageWithReport(c.Name, fromSID, req.To, req.Body, req.ThreadID, req.ReplyTo, known)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}))
	mux.HandleFunc("POST /v1/agents/inbox", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		req, sid, err := decodeCaller[struct {
			callerReq
			UnreadOnly *bool `json:"unread_only"`
			MarkRead   *bool `json:"mark_read"`
		}](r, fleet, c.Name)
		if err != nil {
			return err
		}
		msgs, err := fleet.Agents.FetchInbox(c.Name, sid, optBool(req.UnreadOnly, true), optBool(req.MarkRead, true), 50)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"session_id": sid, "messages": msgs})
		return nil
	}))
	mux.HandleFunc("POST /v1/agents/ask", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		return handleAgentAsk(w, r, fleet, c.Name)
	}))
	mux.HandleFunc("POST /v1/agents/wait", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		return handleAgentWait(w, r, fleet, c.Name)
	}))
	mux.HandleFunc("POST /v1/agents/ack", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		req, sid, err := decodeCaller[struct {
			callerReq
			MessageIDs *[]int64 `json:"message_ids"`
		}](r, fleet, c.Name)
		if err != nil {
			return err
		}
		if req.MessageIDs == nil {
			return &agents.AgentError{Msg: "message_ids list is required", Status: 400}
		}
		acked, err := fleet.Agents.AckMessages(c.Name, sid, *req.MessageIDs)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "acked": acked})
		return nil
	}))
	mux.HandleFunc("POST /v1/agents/rename", route(false, func(w http.ResponseWriter, r *http.Request, c *registry.Device) error {
		req, sid, err := decodeCaller[struct {
			callerReq
			Name string `json:"name"`
		}](r, fleet, c.Name)
		if err != nil {
			return err
		}
		slug, err := fleet.Agents.RenameSession(c.Name, sid, req.Name)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]string{"session_id": sid, "name": slug})
		return nil
	}))
}

func handleAgentAsk(w http.ResponseWriter, r *http.Request, fleet *core.Fleet, dev string) error {
	req, fromSID, err := decodeCaller[struct {
		callerReq
		To         string   `json:"to"`
		Body       string   `json:"body"`
		Timeout    *float64 `json:"timeout"`
		WaitIfIdle bool     `json:"wait_if_idle"`
	}](r, fleet, dev)
	if err != nil {
		return err
	}
	known, err := fleet.ActiveDeviceNames()
	if err != nil {
		return err
	}
	res, err := fleet.Agents.Ask(r.Context(), agents.AskParams{
		Device: dev, FromSession: fromSID, To: req.To, Body: req.Body,
		Timeout: optFloat(req.Timeout, 30.0), WaitIfIdle: req.WaitIfIdle, KnownDevices: known,
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}

func handleAgentWait(w http.ResponseWriter, r *http.Request, fleet *core.Fleet, dev string) error {
	req, sid, err := decodeCaller[struct {
		callerReq
		Timeout       *float64 `json:"timeout"`
		MarkRead      *bool    `json:"mark_read"`
		Limit         *int     `json:"limit"`
		RequireFullID bool     `json:"require_full_id"`
	}](r, fleet, dev)
	if err != nil {
		return err
	}
	res, err := fleet.Agents.WaitForMessages(r.Context(), agents.WaitParams{
		Device: dev, SessionID: sid, Timeout: optFloat(req.Timeout, 55.0),
		MarkRead: optBool(req.MarkRead, true), Limit: optInt(req.Limit, 50), RequireFullID: req.RequireFullID,
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, res)
	return nil
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
